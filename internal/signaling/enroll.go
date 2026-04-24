package signaling

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
)

const (
	enrollTTL          = 10 * time.Minute
	enrollPollInterval = 2
	deviceTokenBytes   = 32
)

// handleEnrollStart is called by the device at the start of the RFC 8628 flow.
func (s *Server) handleEnrollStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req proto.EnrollStartRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	userCode, deviceCode := randomUserCode(), randomToken(32)
	exp := time.Now().Add(enrollTTL)
	err := s.store.CreateEnrollment(r.Context(), Enrollment{
		DeviceCode: deviceCode,
		UserCode:   userCode,
		ExpiresAt:  exp,
		Platform:   req.Platform,
		Hostname:   req.Hostname,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	base := s.publicURL(r)
	resp := proto.EnrollStartResponse{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         base + "/enroll",
		VerificationURIComplete: base + "/enroll?user_code=" + userCode,
		ExpiresIn:               int(enrollTTL.Seconds()),
		Interval:                enrollPollInterval,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleEnrollPoll is called by the device until the user approves or timeout.
func (s *Server) handleEnrollPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req proto.EnrollPollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	e, err := s.store.GetEnrollmentByDeviceCode(r.Context(), req.DeviceCode)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusBadRequest, proto.EnrollPollError{Error: "invalid_device_code"})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if time.Now().After(e.ExpiresAt) {
		writeJSON(w, http.StatusBadRequest, proto.EnrollPollError{Error: "expired_token"})
		return
	}
	if !e.Approved {
		writeJSON(w, http.StatusBadRequest, proto.EnrollPollError{Error: "authorization_pending"})
		return
	}
	if e.DeviceID.Valid {
		// Already materialized; prevent token reissue.
		writeJSON(w, http.StatusBadRequest, proto.EnrollPollError{Error: "already_claimed"})
		return
	}

	token := randomToken(deviceTokenBytes)
	dev, err := s.store.MaterializeDevice(r.Context(), req.DeviceCode, token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, proto.EnrollPollSuccess{
		DeviceID:    dev.ID,
		DeviceToken: token,
		Name:        dev.Name,
	})
}

// handleEnrollApprove is called by the browser after the operator confirms.
// In M0 there is no user session: the signaling service is assumed to be on
// localhost or behind a reverse proxy with its own auth. Later milestones
// replace this with an OIDC-gated handler.
func (s *Server) handleEnrollApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req proto.EnrollApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.UserCode = strings.ToUpper(strings.TrimSpace(req.UserCode))
	req.Name = strings.TrimSpace(req.Name)

	err := s.store.ApproveEnrollment(r.Context(), req.UserCode, req.Name)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "invalid or expired user code", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEnrollLookup lets the browser pre-fill the approval form with context
// about a user code (platform, hostname) before the operator commits.
func (s *Server) handleEnrollLookup(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("user_code")))
	if code == "" {
		http.Error(w, "user_code required", http.StatusBadRequest)
		return
	}
	e, err := s.store.GetEnrollmentByUserCode(r.Context(), code)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if time.Now().After(e.ExpiresAt) {
		http.Error(w, "expired", http.StatusGone)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_code": e.UserCode,
		"platform":  e.Platform,
		"hostname":  e.Hostname,
		"approved":  e.Approved,
		"claimed":   e.DeviceID.Valid,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
