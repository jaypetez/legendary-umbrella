package signaling

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
)

// Web is the embedded browser bundle. It's populated by the cmd/signaling
// package via SetWebFS so the server package itself has no embed path.
var webFS fs.FS

// SetWebFS injects the embedded browser assets. Call once before Start.
func SetWebFS(f embed.FS, root string) {
	sub, err := fs.Sub(f, root)
	if err != nil {
		panic(err)
	}
	webFS = sub
}

type Config struct {
	Addr       string // e.g. ":8080"
	PublicURL  string // optional, e.g. "https://connect.example.com"
	AdminToken string // optional; if set, browser requests must bear X-Admin-Token
}

type Server struct {
	cfg      Config
	store    *Store
	presence *presence
	sessions *sessionBroker
}

func NewServer(cfg Config, store *Store) *Server {
	p := newPresence()
	return &Server{
		cfg:      cfg,
		store:    store,
		presence: p,
		sessions: newSessionBroker(p),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Device-facing endpoints.
	mux.HandleFunc("/enroll/start", s.handleEnrollStart)
	mux.HandleFunc("/enroll/poll", s.handleEnrollPoll)

	// Browser-facing REST.
	mux.HandleFunc("/api/enroll/lookup", s.handleEnrollLookup)
	mux.HandleFunc("/api/enroll/approve", s.handleEnrollApprove)
	mux.HandleFunc("/api/devices", s.handleListDevices)

	// WebSocket from agents.
	mux.HandleFunc("/device", s.handleDeviceWS)
	// WebSocket from the browser for an individual remote session.
	mux.HandleFunc("/ws/session", s.handleSessionWS)

	// Static browser bundle.
	if webFS != nil {
		// /enroll is advertised to devices in verification_uri; serve the
		// approval page there (preserving the user_code query string).
		mux.HandleFunc("/enroll", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, webFS, "enroll.html")
		})
		mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, webFS, "session.html")
		})
		mux.Handle("/", http.FileServer(http.FS(webFS)))
	}

	return logRequests(mux)
}

func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("signaling listening", "addr", s.cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) checkAdmin(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return true // dev mode
	}
	return r.Header.Get("X-Admin-Token") == s.cfg.AdminToken
}

// --- device list ------------------------------------------------------------

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	devs, err := s.store.ListDevices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]proto.DeviceSummary, 0, len(devs))
	for _, d := range devs {
		out = append(out, proto.DeviceSummary{
			ID:         d.ID,
			Name:       d.Name,
			Platform:   d.Platform,
			Online:     s.presence.isOnline(d.ID),
			LastSeenAt: d.LastSeenAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		if !strings.HasPrefix(r.URL.Path, "/device") { // WS logs itself
			slog.Debug("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"dur", time.Since(start).String(),
			)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack forwards to the underlying ResponseWriter so WebSocket upgrades work
// through the logging middleware. Without this, coder/websocket.Accept sees a
// non-hijackable writer and returns 501.
func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("response writer does not support hijacking")
}

// Flush forwards to the underlying ResponseWriter.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
