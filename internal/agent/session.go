package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"

	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
	"github.com/pion/webrtc/v4"
)

// Default STUN servers used for NAT discovery in dev. In production we swap in
// our own coturn with REST-API credentials.
var defaultICEServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
}

// agentSession holds a single WebRTC peer connection and the PTY it bridges.
type agentSession struct {
	id      string
	kind    string
	pc      *webrtc.PeerConnection
	shellDC *webrtc.DataChannel
	ctlDC   *webrtc.DataChannel
	pty     ptyHost
	send    sendFunc

	closeOnce sync.Once
	closed    chan struct{}
}

// ptyHost abstracts a pseudo-terminal so we can plug in creack/pty on Unix
// and Windows ConPTY (via UserExistsError/conpty) without the session code
// caring which one is active.
type ptyHost interface {
	io.Reader
	io.Writer
	io.Closer
	Resize(rows, cols uint16) error
}

// startAgentSession is kicked off in response to a session.request envelope.
// It creates the PeerConnection, offers, and wires data channels to a PTY.
func startAgentSession(parentCtx context.Context, cfg *Config, env proto.Envelope, send sendFunc, reg *sessionRegistry) {
	var req proto.SessionRequestData
	_ = json.Unmarshal(env.Data, &req)

	s := &agentSession{
		id:     env.SessionID,
		kind:   req.Kind,
		send:   send,
		closed: make(chan struct{}),
	}
	reg.add(s)

	if err := s.run(parentCtx, cfg); err != nil {
		slog.Warn("session failed", "session", s.id, "err", err)
		s.sendEnd(err.Error())
	}
	reg.remove(s.id)
}

func (s *agentSession) run(ctx context.Context, cfg *Config) error {
	if s.kind != "" && s.kind != "shell" {
		return fmt.Errorf("unsupported session kind %q (only 'shell' in M1)", s.kind)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: defaultICEServers})
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}
	s.pc = pc

	// Stream PTY bytes on "shell"; JSON control messages on "shell_ctl".
	shellDC, err := pc.CreateDataChannel("shell", &webrtc.DataChannelInit{
		Ordered: boolPtr(true),
	})
	if err != nil {
		return fmt.Errorf("create shell DC: %w", err)
	}
	ctlDC, err := pc.CreateDataChannel("shell_ctl", &webrtc.DataChannelInit{
		Ordered: boolPtr(true),
	})
	if err != nil {
		return fmt.Errorf("create shell_ctl DC: %w", err)
	}
	s.shellDC = shellDC
	s.ctlDC = ctlDC

	shellDC.OnOpen(func() {
		slog.Info("shell DC open", "session", s.id)
		if err := s.spawnPTY(); err != nil {
			slog.Error("spawn PTY", "session", s.id, "err", err)
			s.sendEnd("pty spawn failed: " + err.Error())
			s.close("pty spawn failed")
			return
		}
		go s.pumpPTYToDC()
	})
	shellDC.OnMessage(func(msg webrtc.DataChannelMessage) {
		if s.pty == nil {
			return
		}
		if _, err := s.pty.Write(msg.Data); err != nil {
			slog.Warn("pty write", "err", err)
		}
	})
	shellDC.OnClose(func() {
		slog.Info("shell DC closed", "session", s.id)
		s.close("shell DC closed")
	})

	ctlDC.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.handleCtl(msg.Data)
	})

	// ICE candidates -> relay to browser.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		_ = s.send(mustEnvelope(proto.TypeSessionCandidate, s.id, proto.CandidateData{
			Candidate:     j.Candidate,
			SDPMid:        derefString(j.SDPMid),
			SDPMLineIndex: derefUint16(j.SDPMLineIndex),
		}))
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		slog.Debug("pc state", "session", s.id, "state", st.String())
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			s.close(fmt.Sprintf("peer connection %s", st.String()))
		}
	})

	// Create offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	if err := s.send(mustEnvelope(proto.TypeSessionOffer, s.id, proto.SDPData{SDP: offer.SDP})); err != nil {
		return err
	}

	// Wait for close. dispatch() drives state from incoming envelopes.
	select {
	case <-s.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dispatch is called by the registry when a session-scoped envelope arrives
// from the browser via signaling.
func (s *agentSession) dispatch(env proto.Envelope) {
	switch env.Type {
	case proto.TypeSessionAnswer:
		var sdp proto.SDPData
		if err := json.Unmarshal(env.Data, &sdp); err != nil {
			return
		}
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sdp.SDP,
		}); err != nil {
			slog.Warn("set remote description", "err", err)
		}
	case proto.TypeSessionCandidate:
		var c proto.CandidateData
		if err := json.Unmarshal(env.Data, &c); err != nil {
			return
		}
		mid := c.SDPMid
		idx := c.SDPMLineIndex
		if err := s.pc.AddICECandidate(webrtc.ICECandidateInit{
			Candidate:     c.Candidate,
			SDPMid:        &mid,
			SDPMLineIndex: &idx,
		}); err != nil {
			slog.Debug("add ICE candidate", "err", err)
		}
	case proto.TypeSessionEnd:
		s.close("browser ended session")
	}
}

// --- PTY ------------------------------------------------------------------

func (s *agentSession) spawnPTY() error {
	shell, args := pickShell()
	h, err := openPTY(shell, args, 24, 80)
	if err != nil {
		return err
	}
	s.pty = h
	slog.Info("pty spawned", "session", s.id, "shell", shell)
	return nil
}

func (s *agentSession) pumpPTYToDC() {
	buf := make([]byte, 4096)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			// DataChannel.Send takes []byte; copy because Pion may retain.
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := s.shellDC.Send(chunk); err != nil {
				slog.Warn("dc send", "err", err)
				s.close("dc send failed")
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Debug("pty read", "err", err)
			}
			s.close("pty ended")
			return
		}
	}
}

type ctlMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

func (s *agentSession) handleCtl(data []byte) {
	var m ctlMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case "resize":
		if s.pty == nil || m.Rows == 0 || m.Cols == 0 {
			return
		}
		if err := s.pty.Resize(m.Rows, m.Cols); err != nil {
			slog.Debug("pty resize", "err", err)
		}
	}
}

// --- close ----------------------------------------------------------------

func (s *agentSession) sendEnd(reason string) {
	_ = s.send(mustEnvelope(proto.TypeSessionEnd, s.id, proto.SessionEndData{Reason: reason}))
}

func (s *agentSession) close(reason string) {
	s.closeOnce.Do(func() {
		slog.Info("session closing", "session", s.id, "reason", reason)
		if s.pty != nil {
			_ = s.pty.Close()
		}
		if s.pc != nil {
			_ = s.pc.Close()
		}
		close(s.closed)
	})
}

// --- helpers --------------------------------------------------------------

func pickShell() (string, []string) {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("COMSPEC"); v != "" {
			return v, nil
		}
		return "powershell.exe", []string{"-NoLogo"}
	case "darwin":
		if v := os.Getenv("SHELL"); v != "" {
			return v, nil
		}
		return "/bin/zsh", nil
	default: // linux and friends
		if v := os.Getenv("SHELL"); v != "" {
			return v, nil
		}
		return "/bin/bash", nil
	}
}

func mustEnvelope(t, sid string, data any) proto.Envelope {
	env := proto.Envelope{Type: t, SessionID: sid}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		env.Data = raw
	}
	return env
}

func boolPtr(b bool) *bool { return &b }
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func derefUint16(p *uint16) uint16 {
	if p == nil {
		return 0
	}
	return *p
}
