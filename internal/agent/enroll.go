package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/jaysonpetersen/legendary-umbrella/internal/proto"
)

type EnrollOptions struct {
	ServerURL  string // e.g. http://localhost:8080
	ConfigPath string
	Stdout     io.Writer
}

// Enroll runs the RFC 8628 device-code flow against the signaling service and
// saves the resulting device token to the config file.
func Enroll(ctx context.Context, opt EnrollOptions) error {
	if opt.Stdout == nil {
		opt.Stdout = os.Stdout
	}
	opt.ServerURL = strings.TrimRight(opt.ServerURL, "/")

	host, _ := os.Hostname()
	start, err := postJSON[proto.EnrollStartResponse](ctx, opt.ServerURL+"/enroll/start", proto.EnrollStartRequest{
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Hostname: host,
	})
	if err != nil {
		return fmt.Errorf("enroll/start: %w", err)
	}

	fmt.Fprintln(opt.Stdout, "")
	fmt.Fprintln(opt.Stdout, "  Visit:  ", start.VerificationURI)
	fmt.Fprintln(opt.Stdout, "  Code:   ", start.UserCode)
	if start.VerificationURIComplete != "" {
		fmt.Fprintln(opt.Stdout, "  Direct: ", start.VerificationURIComplete)
	}
	fmt.Fprintln(opt.Stdout, "")
	fmt.Fprintln(opt.Stdout, "Waiting for approval…")

	interval := time.Duration(start.Interval) * time.Second
	if interval < time.Second {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return errors.New("enrollment timed out; rerun `agent enroll`")
		}
		res, err := pollOnce(ctx, opt.ServerURL, start.DeviceCode)
		if err == nil {
			fmt.Fprintf(opt.Stdout, "\nApproved as %q (id %s).\n", res.Name, res.DeviceID)
			cfg := &Config{
				ServerURL:   opt.ServerURL,
				DeviceID:    res.DeviceID,
				DeviceToken: res.DeviceToken,
				DeviceName:  res.Name,
			}
			if err := SaveConfig(opt.ConfigPath, cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Fprintf(opt.Stdout, "Saved to %s\n", opt.ConfigPath)
			return nil
		}
		if errors.Is(err, errAuthPending) {
			continue
		}
		if errors.Is(err, errSlowDown) {
			interval += time.Second
			continue
		}
		return err
	}
}

var (
	errAuthPending = errors.New("authorization_pending")
	errSlowDown    = errors.New("slow_down")
)

func pollOnce(ctx context.Context, baseURL, deviceCode string) (*proto.EnrollPollSuccess, error) {
	reqBody, _ := json.Marshal(proto.EnrollPollRequest{DeviceCode: deviceCode})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/enroll/poll", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode == http.StatusOK {
		var ok proto.EnrollPollSuccess
		if err := json.Unmarshal(body, &ok); err != nil {
			return nil, fmt.Errorf("parse poll success: %w", err)
		}
		return &ok, nil
	}
	var perr proto.EnrollPollError
	_ = json.Unmarshal(body, &perr)
	switch perr.Error {
	case "authorization_pending":
		return nil, errAuthPending
	case "slow_down":
		return nil, errSlowDown
	case "expired_token":
		return nil, errors.New("enrollment expired before approval")
	case "already_claimed":
		return nil, errors.New("enrollment was already claimed by another agent")
	case "":
		return nil, fmt.Errorf("poll failed (HTTP %d): %s", res.StatusCode, strings.TrimSpace(string(body)))
	default:
		return nil, fmt.Errorf("poll error: %s", perr.Error)
	}
}

func postJSON[T any](ctx context.Context, url string, body any) (*T, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		data, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(data)))
	}
	var out T
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
