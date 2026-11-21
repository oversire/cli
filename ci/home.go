package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/oversire/cli/wire"
)

// HomeError is a refusal or a failure of the home daemon. Status is 0 when home gave no answer.
type HomeError struct {
	Status int
	Text   string
}

func (e *HomeError) Error() string { return e.Text }

// HomeClient calls the CI routes on the home daemon from the daemon of an agent machine.
type HomeClient struct {
	g      HomeFinder
	local  string
	client *http.Client
}

// NewHomeClient makes the client. local is the loopback base of this daemon, for the case that this machine is home.
func NewHomeClient(g HomeFinder, local string, client *http.Client) *HomeClient {
	return &HomeClient{g: g, local: local, client: client}
}

// Wait calls GET /v1/ci/wait.
func (h *HomeClient) Wait(ctx context.Context, q WaitQuery) (r wire.CIRollup, err error) {
	err = h.do(ctx, http.MethodGet, "/v1/ci/wait", q.values(), nil, &r)
	return r, err
}

// AddWatch calls POST /v1/ci/watches.
func (h *HomeClient) AddWatch(ctx context.Context, req wire.CIWatchRequest) (a wire.CIWatchAnswer, err error) {
	err = h.do(ctx, http.MethodPost, "/v1/ci/watches", nil, req, &a)
	return a, err
}

func (h *HomeClient) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	base, token, self := h.g.MirrorHome()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	if self {
		base = h.local
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.URL.RawQuery = q.Encode()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !self {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(ForwardedHeader, "1")
	}
	_, home := h.g.Info()
	resp, err := h.client.Do(req)
	if err != nil {
		var dns *net.DNSError
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.As(err, &dns):
			return &HomeError{Text: fmt.Sprintf("%s cannot reach %s (home).", h.g.Self().Machine, home)}
		}
		return &HomeError{Text: home + " (home) is offline."}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		text := fmt.Sprintf("%s (home) answered %s.", home, resp.Status)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			text = e.Error
		}
		return &HomeError{Status: resp.StatusCode, Text: text}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &HomeError{Text: home + " (home) sent an answer that is not valid."}
	}
	return nil
}

// WaitQuery holds the parameters of GET /v1/ci/wait.
type WaitQuery struct {
	Repo, Ref, Since string
	Timeout, Settle  time.Duration
	FailFast         bool
	Started          time.Time
	Workflows        []string
}

func (q WaitQuery) values() url.Values {
	v := url.Values{"repo": {q.Repo}, "ref": {q.Ref},
		"timeout": {strconv.Itoa(int(q.Timeout / time.Second))}, "settle": {strconv.Itoa(int(q.Settle / time.Second))}}
	if !q.Started.IsZero() {
		v.Set("started", strconv.FormatInt(q.Started.Unix(), 10))
	}
	if q.Since != "" {
		v.Set("since", q.Since)
	}
	if q.FailFast {
		v.Set("failFast", "1")
	}
	for _, w := range q.Workflows {
		v.Add("workflow", w)
	}
	return v
}
