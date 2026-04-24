// Package proto defines the wire messages exchanged between the signaling
// service, the device agent, and the browser client. Keep this package
// dependency-free so it can be imported anywhere.
package proto

import "encoding/json"

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

// Envelope is the outer JSON frame on the device WS and the browser session WS.
// Data is left as raw JSON so the dispatcher can decode into a concrete type
// based on Type. SessionID is empty for presence messages and mandatory for
// session.* messages.
type Envelope struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

const (
	TypeHello = "hello"
	TypePing  = "ping"
	TypePong  = "pong"

	// --- session signaling ---
	// Browser → signaling: open a session against a device.
	TypeSessionOpen = "session.open"
	// Signaling → device: a browser has requested a session.
	TypeSessionRequest = "session.request"
	// Device → browser: SDP offer.
	TypeSessionOffer = "session.offer"
	// Browser → device: SDP answer.
	TypeSessionAnswer = "session.answer"
	// Bidirectional: ICE candidate.
	TypeSessionCandidate = "session.candidate"
	// Either end: session finished / error.
	TypeSessionEnd = "session.end"
)

type HelloData struct {
	DeviceID string `json:"device_id"`
	ServerT  int64  `json:"server_t"`
}

// SessionOpenData is sent by the browser to start a session.
type SessionOpenData struct {
	DeviceID string `json:"device_id"`
	// Kind picks the feature to enable: "shell" for M1; later "desktop" will join.
	Kind string `json:"kind"`
}

// SessionRequestData is sent by signaling to the device to start a session.
type SessionRequestData struct {
	Kind string `json:"kind"`
}

// SDPData carries a raw SDP string (offer or answer).
type SDPData struct {
	SDP string `json:"sdp"`
}

// CandidateData carries a trickled ICE candidate as emitted by the browser/Pion.
type CandidateData struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid,omitempty"`
	SDPMLineIndex uint16 `json:"sdpMLineIndex,omitempty"`
}

// SessionEndData carries a reason string; empty means normal close.
type SessionEndData struct {
	Reason string `json:"reason,omitempty"`
}

// --- Browser API -----------------------------------------------------------

type DeviceSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	Online     bool   `json:"online"`
	LastSeenAt int64  `json:"last_seen_at,omitempty"`
}
