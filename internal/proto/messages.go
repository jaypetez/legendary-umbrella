// Package proto defines the wire messages exchanged between the signaling
// service, the device agent, and the browser client. Keep this package
// dependency-free so it can be imported anywhere.
package proto

// --- Enrollment (RFC 8628 device code flow) --------------------------------

type EnrollStartRequest struct {
	Platform string `json:"platform,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

type EnrollStartResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type EnrollPollRequest struct {
	DeviceCode string `json:"device_code"`
}

type EnrollPollError struct {
	Error string `json:"error"`
}

type EnrollPollSuccess struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
	Name        string `json:"name"`
}

type EnrollApproveRequest struct {
	UserCode string `json:"user_code"`
	Name     string `json:"name"`
}

// --- Device presence WebSocket ---------------------------------------------
//
// The server upgrades /device requests bearing a valid device token. Messages
// are JSON-encoded envelopes. In M0 only hello/ping/pong are used; later
// milestones extend this with session offer/answer/candidate messages.

type Envelope struct {
	Type string `json:"type"`
	// Opaque payload; parsed based on Type.
	Data interface{} `json:"data,omitempty"`
}

const (
	TypeHello = "hello"
	TypePing  = "ping"
	TypePong  = "pong"
)

type HelloData struct {
	DeviceID string `json:"device_id"`
	ServerT  int64  `json:"server_t"`
}

// --- Browser API -----------------------------------------------------------

type DeviceSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	Online     bool   `json:"online"`
	LastSeenAt int64  `json:"last_seen_at,omitempty"`
}
