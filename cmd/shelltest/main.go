// shelltest acts as a headless "browser" against the signaling service: it
// opens a session WebSocket, does the SDP/ICE dance with the device agent, and
// runs a small integration check on the PTY data channels.
//
// Used only by the M1 smoke tests. Not shipped.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
	"github.com/pion/webrtc/v4"
)

func main() {
	var (
		server   = flag.String("server", "http://127.0.0.1:8765", "Signaling base URL")
		deviceID = flag.String("device", "", "Target device ID")
		command  = flag.String("cmd", "echo hello-from-shelltest", "Command to send over the PTY")
		wait     = flag.Duration("wait", 5*time.Second, "How long to wait for output to include expected substring")
		expect   = flag.String("expect", "hello-from-shelltest", "Substring the command output must contain")
	)
	flag.Parse()
	if *deviceID == "" {
		log.Fatal("-device is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 15*time.Second)
	defer timeout()

	if err := run(ctx, *server, *deviceID, *command, *expect, *wait); err != nil {
		log.Fatalf("shelltest failed: %v", err)
	}
	fmt.Println("shelltest OK")
}

func run(ctx context.Context, serverURL, deviceID, command, expect string, wait time.Duration) error {
	wsURL, err := toWS(serverURL)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, wsURL+"/ws/session", nil)
	if err != nil {
		return fmt.Errorf("dial session WS: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("new PC: %w", err)
	}
	defer pc.Close()

	var sessionID string
	outputBuf := &safeBuf{}
	gotOutput := make(chan struct{})
	ctlReady := make(chan *webrtc.DataChannel, 1)
	shellReady := make(chan *webrtc.DataChannel, 1)

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "shell":
			dc.OnOpen(func() { shellReady <- dc })
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				outputBuf.Write(msg.Data)
				if bytes.Contains(outputBuf.Bytes(), []byte(expect)) {
					select {
					case <-gotOutput:
					default:
						close(gotOutput)
					}
				}
			})
		case "shell_ctl":
			dc.OnOpen(func() { ctlReady <- dc })
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || sessionID == "" {
			return
		}
		j := c.ToJSON()
		var mid string
		if j.SDPMid != nil {
			mid = *j.SDPMid
		}
		var idx uint16
		if j.SDPMLineIndex != nil {
			idx = *j.SDPMLineIndex
		}
		raw, _ := json.Marshal(proto.CandidateData{Candidate: j.Candidate, SDPMid: mid, SDPMLineIndex: idx})
		_ = conn.Write(ctx, websocket.MessageText, mustJSON(proto.Envelope{
			Type: proto.TypeSessionCandidate, SessionID: sessionID, Data: raw,
		}))
	})

	// Open the session.
	open := mustJSON(proto.Envelope{
		Type: proto.TypeSessionOpen,
		Data: mustJSON2(proto.SessionOpenData{DeviceID: deviceID, Kind: "shell"}),
	})
	if err := conn.Write(ctx, websocket.MessageText, open); err != nil {
		return fmt.Errorf("write open: %w", err)
	}

	// Read loop.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var env proto.Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			switch env.Type {
			case "session.ready":
				sessionID = env.SessionID
			case proto.TypeSessionOffer:
				sessionID = env.SessionID
				var sdp proto.SDPData
				_ = json.Unmarshal(env.Data, &sdp)
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{
					Type: webrtc.SDPTypeOffer, SDP: sdp.SDP,
				}); err != nil {
					readErr <- err
					return
				}
				ans, err := pc.CreateAnswer(nil)
				if err != nil {
					readErr <- err
					return
				}
				if err := pc.SetLocalDescription(ans); err != nil {
					readErr <- err
					return
				}
				ansRaw, _ := json.Marshal(proto.SDPData{SDP: ans.SDP})
				_ = conn.Write(ctx, websocket.MessageText, mustJSON(proto.Envelope{
					Type: proto.TypeSessionAnswer, SessionID: sessionID, Data: ansRaw,
				}))
			case proto.TypeSessionCandidate:
				var c proto.CandidateData
				_ = json.Unmarshal(env.Data, &c)
				mid := c.SDPMid
				idx := c.SDPMLineIndex
				_ = pc.AddICECandidate(webrtc.ICECandidateInit{
					Candidate: c.Candidate, SDPMid: &mid, SDPMLineIndex: &idx,
				})
			case proto.TypeSessionEnd:
				readErr <- fmt.Errorf("session ended unexpectedly")
				return
			}
		}
	}()

	// Wait for both data channels to open.
	var shellDC *webrtc.DataChannel
	select {
	case shellDC = <-shellReady:
	case err := <-readErr:
		return err
	case <-ctx.Done():
		return errors.New("timeout waiting for shell DC")
	}
	// ctl DC is nice-to-have for resize but not required for command output.
	select {
	case <-ctlReady:
	case <-time.After(500 * time.Millisecond):
	}

	// Send the command followed by CR. A real Enter keystroke sends \r, and
	// PTY line discipline (or cmd.exe on Windows) promotes it to a submit.
	// Give the shell a beat to finish painting its initial prompt first,
	// otherwise our bytes land before it's reading.
	time.Sleep(300 * time.Millisecond)
	if err := shellDC.Send([]byte(command + "\r")); err != nil {
		return fmt.Errorf("shell send: %w", err)
	}

	// Wait for expected output (or timeout).
	select {
	case <-gotOutput:
		fmt.Printf("saw expected substring %q\n", expect)
	case <-time.After(wait):
		return fmt.Errorf("timed out waiting for %q in PTY output; got: %q", expect, outputBuf.String())
	case err := <-readErr:
		return err
	}

	// Clean close.
	end := mustJSON(proto.Envelope{Type: proto.TypeSessionEnd, SessionID: sessionID})
	_ = conn.Write(ctx, websocket.MessageText, end)
	return nil
}

// --- helpers --------------------------------------------------------------

type safeBuf struct {
	bytes.Buffer
}

func (b *safeBuf) Bytes() []byte {
	// bytes.Buffer isn't safe for concurrent access; but this program only
	// reads after channel sync, so Buffer's own Bytes() is fine.
	return b.Buffer.Bytes()
}

func toWS(httpURL string) (string, error) {
	u, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func mustJSON2(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
