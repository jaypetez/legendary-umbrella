package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
)

// sessionBroker matches one browser WebSocket to one online device, routes
// SDP/ICE envelopes between them, and cleans up when either side hangs up.
//
// Sessions are ephemeral and in-memory. A session ends when (a) the browser
// disconnects, (b) the device disconnects, or (c) either side sends
// session.end. We never retain session state across signaling restarts.
type sessionBroker struct {
	mu       sync.Mutex
	sessions map[string]*session
	presence *presence
}

type session struct {
	id       string
	deviceID string
	// browserOut pushes messages toward the browser WebSocket.
	browserOut chan proto.Envelope
	cancel     context.CancelFunc
	closed     bool
}

func newSessionBroker(p *presence) *sessionBroker {
	return &sessionBroker{
		sessions: make(map[string]*session),
		presence: p,
	}
}

// open creates a new session targeting the given device. It returns the
// session ID and pushes a session.request envelope onto the device.
func (b *sessionBroker) open(deviceID, kind string, browserOut chan proto.Envelope, cancel context.CancelFunc) (*session, error) {
	if !b.presence.isOnline(deviceID) {
		return nil, ErrDeviceOffline
	}
	s := &session{
		id:         randomToken(16),
		deviceID:   deviceID,
		browserOut: browserOut,
		cancel:     cancel,
	}
	b.mu.Lock()
	b.sessions[s.id] = s
	b.mu.Unlock()

	if err := b.presence.Send(deviceID, mustEnvelope(
		proto.TypeSessionRequest, s.id, proto.SessionRequestData{Kind: kind},
	)); err != nil {
		b.close(s.id, "device unreachable")
		return nil, err
	}
	return s, nil
}

func (b *sessionBroker) get(id string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

// fromBrowser forwards a browser-originated envelope to the paired device.
func (b *sessionBroker) fromBrowser(sessionID string, env proto.Envelope) error {
	s := b.get(sessionID)
	if s == nil {
		return errors.New("unknown session")
	}
	env.SessionID = sessionID
	return b.presence.Send(s.deviceID, env)
}

// fromDevice forwards a device-originated envelope to the paired browser.
func (b *sessionBroker) fromDevice(deviceID string, env proto.Envelope) {
	if env.SessionID == "" {
		return
	}
	s := b.get(env.SessionID)
	if s == nil || s.deviceID != deviceID {
		return
	}
	select {
	case s.browserOut <- env:
	default:
		slog.Warn("browser outbound full, dropping", "session", env.SessionID, "type", env.Type)
	}
	if env.Type == proto.TypeSessionEnd {
		b.close(s.id, "device ended session")
	}
}

// close tears down a session and notifies both sides best-effort.
func (b *sessionBroker) close(id, reason string) {
	b.mu.Lock()
	s, ok := b.sessions[id]
	if !ok || s.closed {
		b.mu.Unlock()
		return
	}
	s.closed = true
	delete(b.sessions, id)
	b.mu.Unlock()

	end := mustEnvelope(proto.TypeSessionEnd, id, proto.SessionEndData{Reason: reason})
	_ = b.presence.Send(s.deviceID, end)
	select {
	case s.browserOut <- end:
	default:
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// closeByDevice ends every session hanging off the given device.
func (b *sessionBroker) closeByDevice(deviceID, reason string) {
	b.mu.Lock()
	ids := make([]string, 0)
	for id, s := range b.sessions {
		if s.deviceID == deviceID {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.close(id, reason)
	}
}

// --- HTTP handler ----------------------------------------------------------

// handleSessionWS accepts a browser WebSocket for one session. The browser
// speaks first with a session.open envelope naming the target device.
func (s *Server) handleSessionWS(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	out := make(chan proto.Envelope, 32)
	// Writer goroutine.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-out:
				if err := wsWriteJSON(ctx, conn, env); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	defer func() {
		conn.Close(websocket.StatusNormalClosure, "")
		<-writerDone
	}()

	// The first message from the browser must be session.open.
	_, first, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var opener proto.Envelope
	if err := json.Unmarshal(first, &opener); err != nil || opener.Type != proto.TypeSessionOpen {
		out <- mustEnvelope(proto.TypeSessionEnd, "", proto.SessionEndData{Reason: "expected session.open first"})
		return
	}
	var openData proto.SessionOpenData
	if err := json.Unmarshal(opener.Data, &openData); err != nil || openData.DeviceID == "" {
		out <- mustEnvelope(proto.TypeSessionEnd, "", proto.SessionEndData{Reason: "invalid session.open data"})
		return
	}

	sess, err := s.sessions.open(openData.DeviceID, openData.Kind, out, cancel)
	if err != nil {
		out <- mustEnvelope(proto.TypeSessionEnd, "", proto.SessionEndData{Reason: err.Error()})
		return
	}
	defer s.sessions.close(sess.id, "browser closed")

	// Acknowledge the session creation to the browser so it knows the ID.
	out <- mustEnvelope("session.ready", sess.id, map[string]any{"session_id": sess.id})

	slog.Info("session opened", "session", sess.id, "device", sess.deviceID, "kind", openData.Kind)

	// Read loop: forward every envelope to the device.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch env.Type {
		case proto.TypeSessionAnswer,
			proto.TypeSessionCandidate,
			proto.TypeSessionEnd:
			if err := s.sessions.fromBrowser(sess.id, env); err != nil {
				slog.Warn("session forward failed", "session", sess.id, "err", err)
			}
			if env.Type == proto.TypeSessionEnd {
				return
			}
		}
	}
}
