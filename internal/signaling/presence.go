package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
)

// presence tracks which device IDs currently hold a live WebSocket and owns the
// serialized writer for each connection. Session messages are routed via the
// session broker; ping/pong and hello are handled inline here.
type presence struct {
	mu    sync.RWMutex
	conns map[string]*deviceConn // deviceID -> connection state
}

type deviceConn struct {
	out    chan proto.Envelope
	cancel context.CancelFunc
	lastHB time.Time
}

func newPresence() *presence {
	return &presence{conns: make(map[string]*deviceConn)}
}

func (p *presence) register(deviceID string, dc *deviceConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.conns[deviceID]; ok {
		prev.cancel() // kick the stale connection
	}
	p.conns[deviceID] = dc
}

func (p *presence) unregister(deviceID string, dc *deviceConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[deviceID] == dc {
		delete(p.conns, deviceID)
	}
}

func (p *presence) isOnline(deviceID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.conns[deviceID]
	return ok
}

func (p *presence) heartbeat(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[deviceID]; ok {
		c.lastHB = time.Now()
	}
}

// Send pushes an envelope to the named device's outbound queue. Returns an
// error if the device isn't connected or its queue is full.
func (p *presence) Send(deviceID string, env proto.Envelope) error {
	p.mu.RLock()
	c, ok := p.conns[deviceID]
	p.mu.RUnlock()
	if !ok {
		return ErrDeviceOffline
	}
	select {
	case c.out <- env:
		return nil
	default:
		return errors.New("device outbound queue full")
	}
}

var ErrDeviceOffline = errors.New("device offline")

// handleDeviceWS is the WebSocket endpoint the agent holds open.
func (s *Server) handleDeviceWS(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	dev, err := s.store.AuthenticateDevice(r.Context(), token)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // single-tenant: origin policy handled upstream
	})
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	dc := &deviceConn{
		out:    make(chan proto.Envelope, 32),
		cancel: cancel,
		lastHB: time.Now(),
	}
	s.presence.register(dev.ID, dc)
	defer s.presence.unregister(dev.ID, dc)
	_ = s.store.TouchDevice(ctx, dev.ID)

	slog.Info("device connected", "device_id", dev.ID, "name", dev.Name, "remote", r.RemoteAddr)
	defer slog.Info("device disconnected", "device_id", dev.ID)

	// Writer goroutine: serialize all conn.Write calls.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-dc.out:
				if err := wsWriteJSON(ctx, conn, env); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Send hello on the writer.
	dc.out <- mustEnvelope(proto.TypeHello, "", proto.HelloData{DeviceID: dev.ID, ServerT: time.Now().Unix()})

	// Read loop.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			<-writerDone
			// Mark any live sessions on this device as ended.
			s.sessions.closeByDevice(dev.ID, "device disconnected")
			return
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch env.Type {
		case proto.TypePing:
			s.presence.heartbeat(dev.ID)
			_ = s.store.TouchDevice(ctx, dev.ID)
			dc.out <- proto.Envelope{Type: proto.TypePong}
		case proto.TypeSessionOffer,
			proto.TypeSessionAnswer,
			proto.TypeSessionCandidate,
			proto.TypeSessionEnd:
			s.sessions.fromDevice(dev.ID, env)
		}
	}
}

func wsWriteJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return h[len(p):]
	}
	return r.URL.Query().Get("token")
}

// mustEnvelope wraps a typed data payload into an Envelope, panicking if the
// payload can't be marshalled (should be impossible for our own types).
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
