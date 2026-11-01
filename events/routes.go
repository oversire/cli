package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	maxLinks  = 2000
	maxBody   = 4 << 20
)

// Routes serves the events table: POST /v1/events, POST /v1/events/links, GET /v1/items/{key}/events, GET /v1/events/rows.
// Each new row also goes to the WebSocket clients as the frame {type: event}.
type Routes struct {
	store  *Store
	client Doer
	log    *slog.Logger
}

// NewRoutes makes the routes. notify sends a frame to the WebSocket clients (server.Server.Notify).
func NewRoutes(store *Store, notify func(any), log *slog.Logger) *Routes {
	if log == nil {
		log = slog.Default()
	}
	if notify != nil {
		store.Subscribe(func(rows []wire.Event) {
			for _, e := range rows {
				notify(wire.NewEventFrame(e))
			}
		})
	}
	return &Routes{store: store, client: &http.Client{Timeout: 30 * time.Second}, log: log}
}

func (r *Routes) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/events", Access: server.Member, Handle: r.append},
		{Pattern: "POST /v1/events/links", Access: server.Member, Handle: r.links},
		{Pattern: "GET /v1/items/{key}/events", Access: server.Member, Handle: r.item},
		{Pattern: "GET /v1/events/rows", Access: server.Member, Handle: r.rows},
	}
}

type errorBody struct {
	Error string `json:"error"`
}

func (r *Routes) append(w http.ResponseWriter, req *http.Request) {
	var body wire.EventAppend
	if !decode(w, req, &body) {
		return
	}
	if len(body.Events) > MaxAppend {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{fmt.Sprintf("at most %d events", MaxAppend)})
		return
	}
	for i, d := range body.Events {
		if _, err := Normalize(d); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{fmt.Sprintf("events[%d]: %v", i, err)})
			return
		}
	}
	rows, err := r.store.Append(req.Context(), body.Events)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{err.Error()})
		return
	}
	head, _ := r.store.Head(req.Context())
	writeJSON(w, http.StatusOK, wire.EventAppendResult{Accepted: len(rows), Head: head})
}

func (r *Routes) links(w http.ResponseWriter, req *http.Request) {
	var body wire.ItemLinks
	if !decode(w, req, &body) {
		return
	}
	if len(body.Links) > maxLinks {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{fmt.Sprintf("at most %d links", maxLinks)})
		return
	}
	n, err := r.store.SetLinks(req.Context(), body.Links)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": n})
}

func (r *Routes) item(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	after, limit, ok := cursor(w, q.Get("after"), q.Get("limit"))
	if !ok {
		return
	}
	var kinds []string
	if k := q.Get("kinds"); k != "" {
		kinds = strings.Split(k, ",")
	}
	tree := q.Get("tree")
	page, err := r.store.Query(req.Context(), wire.EventQuery{Item: req.PathValue("key"), Tree: tree == "1" || tree == "true",
		After: after, Kinds: kinds, Limit: limit})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (r *Routes) rows(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	after, limit, ok := cursor(w, q.Get("after"), q.Get("limit"))
	if !ok {
		return
	}
	page, err := r.store.Tail(req.Context(), after, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// TakeHome copies all rows of the old home at base (GET /v1/events/rows until more is false). It implements group.HomeTaker.
// The copied rows do not go to the outbox. When the old home does not answer, offline is true and the move goes on.
func (r *Routes) TakeHome(ctx context.Context, base, token string) (records int, offline bool, err error) {
	var after int64
	for {
		page, down, err := r.get(ctx, fmt.Sprintf("%s/v1/events/rows?after=%d&limit=1000", base, after), token)
		if down && after == 0 {
			r.log.Warn("events: the old home does not answer; the rows of the members come from their outboxes", "home", base, "err", err)
			return 0, true, nil
		}
		if err != nil {
			return records, false, err
		}
		drafts := make([]wire.EventDraft, len(page.Events))
		for i, e := range page.Events {
			drafts[i] = e.EventDraft
		}
		rows, err := r.store.add(ctx, drafts, false)
		if err != nil {
			return records, false, err
		}
		records += len(rows)
		if !page.More || page.Next <= after {
			return records, false, nil
		}
		after = page.Next
	}
}

func (r *Routes) get(ctx context.Context, url, token string) (p wire.EventPage, down bool, err error) {
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
	if resp.StatusCode == http.StatusNotFound {
		return p, false, nil // an old daemon with no events table
	}
	if resp.StatusCode != http.StatusOK {
		return p, false, fmt.Errorf("events: the old home answered %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, false, fmt.Errorf("events: the answer of the old home is not valid: %w", err)
	}
	return p, false, nil
}

func cursor(w http.ResponseWriter, a, l string) (after int64, limit int, ok bool) {
	var err error
	if a != "" {
		if after, err = strconv.ParseInt(a, 10, 64); err != nil || after < 0 {
			writeJSON(w, http.StatusBadRequest, errorBody{"after must be a seq"})
			return 0, 0, false
		}
	}
	if l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 || limit > MaxLimit {
			writeJSON(w, http.StatusBadRequest, errorBody{fmt.Sprintf("limit must be 1 to %d", MaxLimit)})
			return 0, 0, false
		}
	}
	return after, limit, true
}

func decode(w http.ResponseWriter, req *http.Request, v any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err == nil {
		err = wire.Decode(data, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
