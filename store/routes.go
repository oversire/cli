package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Group gives the roster of this machine (group.Store).
type Group interface {
	Snapshot() (wire.Group, bool)
	Self() wire.GroupMember
}

// Routes serves the store API of the cloud hub on the home daemon: GET and POST /v1/store/ops.
type Routes struct {
	log    *OpLog
	group  Group
	notify func(any)
	client *http.Client
	logger *slog.Logger
}

// NewRoutes makes the store routes. notify sends a frame to the WebSocket clients (server.Server.Notify).
func NewRoutes(log *OpLog, group Group, notify func(any), logger *slog.Logger) *Routes {
	if notify == nil {
		notify = func(any) {}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Routes{log: log, group: group, notify: notify, client: &http.Client{Timeout: 30 * time.Second}, logger: logger}
}

// notHome is the 409 body when this daemon does not serve the store. Home is the machine that does; Cloud is set when the cloud hub does.
type notHome struct {
	Error string           `json:"error"`
	Home  string           `json:"home,omitempty"`
	Cloud *wire.GroupCloud `json:"cloud,omitempty"`
}

// errorBody is the body of a 4xx or 5xx answer.
type errorBody struct {
	Error string `json:"error"`
}

// Routes gives the store routes. A member that is not home gets 409.
func (r *Routes) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/store/ops", Access: server.Member, Handle: r.home(r.pull)},
		{Pattern: "POST /v1/store/ops", Access: server.Member, Handle: r.home(r.write)},
	}
}

// home answers 409 when the group has cloud, or when another member is home.
func (r *Routes) home(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		g, ok := r.group.Snapshot()
		switch {
		case !ok:
			writeJSON(w, http.StatusConflict, notHome{Error: "this machine has no group"})
		case g.Cloud != nil:
			writeJSON(w, http.StatusConflict, notHome{Error: "the cloud is home", Cloud: g.Cloud})
		case g.Home != r.group.Self().Machine:
			writeJSON(w, http.StatusConflict, notHome{Error: g.Home + " is home", Home: g.Home})
		default:
			next(w, req)
		}
	}
}

func (r *Routes) pull(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	after, _ := strconv.Atoi(q.Get("after"))
	limit := PageLimit
	if s := q.Get("limit"); s != "" {
		limit, _ = strconv.Atoi(s)
		limit = min(max(limit, 1), PageLimit)
	}
	p, err := r.log.Pull(max(after, 0), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (r *Routes) write(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "bad json"})
		return
	}
	ops, err := DecodeOps(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	res, err := r.apply(ops)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// apply writes ops and tells the clients to pull when one op won.
func (r *Routes) apply(ops []wire.StoreOp) (wire.StoreApplyResult, error) {
	res, err := r.log.Apply(ops)
	if err == nil && len(res.Accepted) > 0 {
		r.notify(map[string]any{"type": "store", "head": res.Head})
	}
	return res, err
}

// TakeHome copies all records of the old home at base (GET /v1/store/ops from 0 until more is false) with their HLC.
// It implements group.HomeTaker. When the old home does not answer, offline is true and the move goes on:
// the apps then push their full local store.
func (r *Routes) TakeHome(ctx context.Context, base, token string) (records int, offline bool, err error) {
	after := 0
	for {
		page, down, err := r.get(ctx, fmt.Sprintf("%s/v1/store/ops?after=%d&limit=%d", base, after, PageLimit), token)
		if down && after == 0 {
			r.logger.Warn("store: the old home does not answer; the apps push their stores", "home", base, "err", err)
			return 0, true, nil
		}
		if err != nil {
			return records, false, err
		}
		for i, op := range page.Ops {
			if err := check(op, fmt.Sprintf("ops[%d]", i)); err != nil {
				return records, false, fmt.Errorf("store: the old home sent a bad op: %w", err)
			}
		}
		if _, err := r.apply(page.Ops); err != nil {
			return records, false, err
		}
		records += len(page.Ops)
		if !page.More || page.Cursor <= after {
			return records, false, nil
		}
		after = page.Cursor
	}
}

// get reads one page. down is true when the old home does not answer.
func (r *Routes) get(ctx context.Context, url, token string) (p wire.StorePull, down bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return p, false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return p, true, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		_ = json.Unmarshal(data, &e)
		return p, false, fmt.Errorf("store: the old home answered %d %s", resp.StatusCode, e.Error)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, false, fmt.Errorf("store: the answer of the old home is not valid: %w", err)
	}
	return p, false, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
