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

// presence tracks which device IDs currently hold a live WebSocket.
// M0 uses a single in-memory map; horizontal scaling is a later concern.
type presence struct {
	mu      sync.RWMutex
	online  map[string]time.Time // deviceID -> last heartbeat
	closers map[string]context.CancelFunc
}

func newPresence() *presence {
	return &presence{
		online:  make(map[string]time.Time),
		closers: make(map[string]context.CancelFunc),
	}
}

func (p *presence) connect(deviceID string, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.closers[deviceID]; ok {
		prev() // kick the stale connection
	}
	p.closers[deviceID] = cancel
	p.online[deviceID] = time.Now()
}

func (p *presence) disconnect(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.closers, deviceID)
	delete(p.online, deviceID)
}

func (p *presence) isOnline(deviceID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.online[deviceID]
	return ok
}

func (p *presence) heartbeat(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.online[deviceID]; ok {
		p.online[deviceID] = time.Now()
	}
}

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
		InsecureSkipVerify: true, // origin check is handled at the HTTP layer in single-tenant mode
	})
	if err != nil {
		return // websocket.Accept already wrote a response
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.presence.connect(dev.ID, cancel)
	defer s.presence.disconnect(dev.ID)
	_ = s.store.TouchDevice(ctx, dev.ID)

	slog.Info("device connected", "device_id", dev.ID, "name", dev.Name, "remote", r.RemoteAddr)
	defer slog.Info("device disconnected", "device_id", dev.ID)

	// Send hello.
	if err := wsWriteJSON(ctx, conn, proto.Envelope{
		Type: proto.TypeHello,
		Data: proto.HelloData{DeviceID: dev.ID, ServerT: time.Now().Unix()},
	}); err != nil {
		conn.Close(websocket.StatusInternalError, "hello failed")
		return
	}

	// Read loop. In M0 we only expect pings from the agent.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
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
			_ = wsWriteJSON(ctx, conn, proto.Envelope{Type: proto.TypePong})
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
	// Allow ?token= for WS clients that can't set headers easily (browsers).
	return r.URL.Query().Get("token")
}
