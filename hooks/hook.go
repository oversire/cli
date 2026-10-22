package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Limits of the hook command.
const (
	MaxPayload     = 1 << 20
	MaxSpool       = 5 << 20
	ConnectTimeout = 300 * time.Millisecond
	TotalTimeout   = time.Second
)

// spoolLine is one line of the hook spool: a server.HookCall that did not reach the daemon, with its harness and time.
type spoolLine struct {
	Harness string    `json:"harness"`
	At      time.Time `json:"at"`
	server.HookCall
}

// Replying is a harness that needs a reply on stdout from each hook.
type Replying interface{ Reply() []byte }

func (Claude) Reply() []byte { return []byte("{}\n") }

// Hook is `oversire hook <harness>`. It never fails and never blocks the agent for more than TotalTimeout.
type Hook struct {
	URL    string // the daemon base URL, for example http://127.0.0.1:7690
	Spool  string // <data>/spool/hooks.jsonl
	Getenv func(string) string
	PPID   int
	Client *http.Client
	Clock  func() time.Time
}

// NewHook uses the env of this process and the timeouts of the hook.
func NewHook(url, spool string) *Hook {
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: ConnectTimeout}).DialContext, DisableKeepAlives: true}
	return &Hook{URL: url, Spool: spool, Getenv: os.Getenv, PPID: os.Getppid(), Client: &http.Client{Timeout: TotalTimeout, Transport: tr}, Clock: time.Now}
}

// Env gives the env of the agent process that the daemon uses.
func (h *Hook) Env() wire.HookEnv {
	g := h.Getenv
	return wire.HookEnv{
		PaneKey: g("ORCA_PANE_KEY"), TabID: g("ORCA_TAB_ID"), WorktreeID: g("ORCA_WORKTREE_ID"),
		TerminalHandle: g("ORCA_TERMINAL_HANDLE"), WorktreePath: g("ORCA_WORKTREE_PATH"), Pwd: g("PWD"),
		PPID: h.PPID, OversireSession: g("OVERSIRE_SESSION"),
	}
}

// Run reads the payload, prints the reply of the harness and sends the call. It spools the call when the daemon does not take it.
func (h *Hook) Run(ctx context.Context, harness string, in io.Reader, out io.Writer) {
	for _, hs := range Harnesses() {
		if r, ok := hs.(Replying); ok && hs.Name() == harness {
			_, _ = out.Write(r.Reply())
		}
	}
	data, _ := io.ReadAll(io.LimitReader(in, MaxPayload+1))
	_, _ = io.Copy(io.Discard, in)
	if len(data) > MaxPayload || !json.Valid(data) {
		return
	}
	env := h.Env()
	// A session of the oversire runtime reports through agent-wire.
	if env.OversireSession != "" {
		return
	}
	if h.post(ctx, harness, env, data) {
		return
	}
	h.spool(harness, env, data)
}

func (h *Hook) post(ctx context.Context, harness string, env wire.HookEnv, payload []byte) bool {
	body, err := json.Marshal(server.HookCall{Env: env, Payload: payload})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+"/v1/hooks/"+harness, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	// A 4xx is a call that the daemon will never take. Only a daemon that is not ready gets the spool.
	return resp.StatusCode < 500
}

func (h *Hook) spool(harness string, env wire.HookEnv, payload []byte) {
	if h.Spool == "" {
		return
	}
	line, err := json.Marshal(spoolLine{Harness: harness, At: h.Clock(), HookCall: server.HookCall{Env: env, Payload: payload}})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.Spool), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(h.Spool, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size()+int64(len(line))+1 > MaxSpool {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// SpoolPath is the hook spool in the data folder of the daemon.
func SpoolPath(data string) string { return filepath.Join(data, "spool", "hooks.jsonl") }
