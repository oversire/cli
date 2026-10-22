package group

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/oversire/cli/config"
)

// Errors of the join flows. statusOf maps them to HTTP status codes.
var (
	ErrBadCode  = errors.New("group: the code is not valid or was used")
	ErrExpired  = errors.New("group: the request expired")
	ErrDenied   = errors.New("group: the request was denied")
	ErrRate     = errors.New("group: too many requests, try again in one minute")
	ErrBusy     = errors.New("group: too many open join requests")
	ErrInGroup  = errors.New("group: this machine is in a group with other machines; remove it there first")
	ErrDecided  = errors.New("group: the request has a decision")
	ErrBadInput = errors.New("group: the request body is not valid")
)

func statusOf(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrBadCode), errors.Is(err, ErrDenied):
		return http.StatusForbidden
	case errors.Is(err, ErrExpired):
		return http.StatusGone
	case errors.Is(err, ErrRate), errors.Is(err, ErrBusy):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrInGroup), errors.Is(err, ErrDecided), errors.Is(err, ErrConflict), errors.Is(err, ErrNoGroup),
		errors.Is(err, ErrOtherGroup):
		return http.StatusConflict
	case errors.Is(err, ErrBadInput), errors.Is(err, ErrSelf):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// errorBody is the body of an answer that is not 2xx.
type errorBody struct {
	Error  string `json:"error,omitempty"`
	Status string `json:"status,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, statusOf(err), errorBody{Error: err.Error()})
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	return nil
}

// RemoteError is a 4xx or 5xx answer of another daemon.
type RemoteError struct {
	Code    int
	Message string
	Status  string
}

func (e *RemoteError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the daemon answered %d %s", e.Code, http.StatusText(e.Code))
}

// peerClient calls the /v1 API of other daemons.
type peerClient struct{ http *http.Client }

// do sends body as JSON and decodes a 2xx answer into out. A non-2xx answer gives a *RemoteError.
func (c peerClient) do(ctx context.Context, method, url, token string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e errorBody
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, &RemoteError{Code: resp.StatusCode, Message: e.Error, Status: e.Status}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("the answer of %s is not valid: %w", url, err)
		}
	}
	return resp.StatusCode, nil
}

// Endpoint gives the base URL of a daemon: host, host:port or a URL. port is used when host has no port.
func Endpoint(host string, port int) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimRight(host, "/")
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return "http://" + host
	}
	if port == 0 {
		port = config.DefaultPort
	}
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}

// memberURL gives the base URL of a member: its first address, else its host.
func memberURL(addrs []string, host string, port int) string {
	if len(addrs) > 0 {
		return Endpoint(addrs[0], port)
	}
	return Endpoint(host, port)
}

// via gives host, plus the port when it is not the default port.
func via(host string, port int) string {
	if port == 0 || port == config.DefaultPort {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
