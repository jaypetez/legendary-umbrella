package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
)

type RunOptions struct {
	Config            *Config
	HeartbeatInterval time.Duration
	ReconnectInitial  time.Duration
	ReconnectMax      time.Duration
}

func (o *RunOptions) defaults() {
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = 20 * time.Second
	}
	if o.ReconnectInitial == 0 {
		o.ReconnectInitial = 1 * time.Second
	}
	if o.ReconnectMax == 0 {
		o.ReconnectMax = 30 * time.Second
	}
}

// sendFunc enqueues an envelope on the signaling WS. Safe for concurrent use.
type sendFunc func(proto.Envelope) error

// Run holds a WebSocket to the signaling service open, reconnecting with
// exponential backoff. It returns only when ctx is cancelled.
func Run(ctx context.Context, opt RunOptions) error {
	opt.defaults()

	backoff := opt.ReconnectInitial
	for {
		err := runSession(ctx, opt)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("session ended, reconnecting", "err", err, "backoff", backoff.String())

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > opt.ReconnectMax {
			backoff = opt.ReconnectMax
		}
	}
}

func runSession(ctx context.Context, opt RunOptions) error {
	wsURL, err := toWSURL(opt.Config.ServerURL)
	if err != nil {
		return err
	}

	conn, _, err := websocket.Dial(ctx, wsURL+"/device", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + opt.Config.DeviceToken},
		},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	slog.Info("connected to signaling", "server", opt.Config.ServerURL, "device_id", opt.Config.DeviceID)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Serialize writes on the WS through a single goroutine.
	out := make(chan proto.Envelope, 32)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-out:
				if err := writeEnvelope(ctx, conn, env); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	send := func(env proto.Envelope) error {
		select {
		case out <- env:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Track active sessions so incoming answers/candidates can reach the
	// right PeerConnection.
	sess := newSessionRegistry()
	defer sess.closeAll("agent reconnecting")

	// Reader.
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
			case proto.TypeHello, proto.TypePong:
				// healthy
			case proto.TypeSessionRequest:
				go startAgentSession(ctx, opt.Config, env, send, sess)
			case proto.TypeSessionAnswer,
				proto.TypeSessionCandidate,
				proto.TypeSessionEnd:
				sess.route(env)
			default:
				slog.Debug("unhandled message", "type", env.Type)
			}
		}
	}()

	// Heartbeat.
	ticker := time.NewTicker(opt.HeartbeatInterval)
	defer ticker.Stop()
	if err := send(proto.Envelope{Type: proto.TypePing}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			<-writerDone
			return ctx.Err()
		case err := <-readErr:
			<-writerDone
			return err
		case <-ticker.C:
			if err := send(proto.Envelope{Type: proto.TypePing}); err != nil {
				return err
			}
		}
	}
}

func writeEnvelope(ctx context.Context, c *websocket.Conn, env proto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

func toWSURL(httpURL string) (string, error) {
	u, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already good
	default:
		return "", fmt.Errorf("unsupported server URL scheme %q", u.Scheme)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// --- session registry ------------------------------------------------------

type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*agentSession
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]*agentSession)}
}

func (r *sessionRegistry) add(s *agentSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[s.id] = s
}

func (r *sessionRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

func (r *sessionRegistry) route(env proto.Envelope) {
	r.mu.Lock()
	s, ok := r.sessions[env.SessionID]
	r.mu.Unlock()
	if !ok {
		return
	}
	s.dispatch(env)
}

func (r *sessionRegistry) closeAll(reason string) {
	r.mu.Lock()
	ss := make([]*agentSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		ss = append(ss, s)
	}
	r.sessions = make(map[string]*agentSession)
	r.mu.Unlock()
	for _, s := range ss {
		s.close(reason)
	}
}
