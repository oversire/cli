package term

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const attachTimeout = 10 * time.Second

// Sessions finds a session of the session book.
type Sessions interface {
	Get(id string) (wire.DaemonSession, bool)
}

// The limits of GET /v1/sessions/{id}/turns.
const (
	defaultTurnsLimit = 200
	maxTurnsLimit     = 1000
)

// API serves GET /v1/sessions/{id}/stream and GET /v1/sessions/{id}/turns.
type API struct {
	Log *slog.Logger

	sessions Sessions
	open     Opener
	leases   *Leases
	now      func() time.Time
}

func NewAPI(sessions Sessions, open Opener, now func() time.Time) *API {
	return &API{Log: slog.Default(), sessions: sessions, open: open, leases: NewLeases(now), now: now}
}

func (a *API) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/sessions/{id}/stream", Access: server.Member, Handle: a.stream},
		{Pattern: "GET /v1/sessions/{id}/turns", Access: server.Member, Handle: a.turns},
	}
}

// turns gives the turns after a seq, as one page. It reads the history and no live turns.
func (a *API) turns(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "a web page cannot read the turns of a session")
		return
	}
	after, ok := intParam(w, r, "after", 0, 0, 1<<31-1)
	if !ok {
		return
	}
	limit, ok := intParam(w, r, "limit", defaultTurnsLimit, 1, maxTurnsLimit)
	if !ok {
		return
	}
	ses, found := a.sessions.Get(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, "no session")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	src, err := a.open.Chat(ctx, ses)
	if errors.Is(err, ErrNoView) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	history, _, err := src.Turns(ctx, after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	page := wire.SessionTurnsPage{Turns: []wire.SessionTurn{}, More: len(history) > limit}
	page.Turns = append(page.Turns, history[:min(limit, len(history))]...)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// intParam reads an integer query parameter in [lo, hi]. It writes 400 and gives false when the value is bad.
func intParam(w http.ResponseWriter, r *http.Request, name string, def, lo, hi int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		writeError(w, http.StatusBadRequest, "bad "+name+": use an integer from "+strconv.Itoa(lo)+" to "+strconv.Itoa(hi))
		return 0, false
	}
	return n, true
}

func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	// The app sends no Origin. A web page always does: it must not reach a terminal.
	if r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "a web page cannot open a session stream")
		return
	}
	ses, ok := a.sessions.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no session")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attach, err := readAttach(ctx, conn)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	v := newViewer(conn, ses.ID, a.leases, a.now)
	go v.readLoop(ctx)
	go v.pinger(ctx, cancel)
	log := a.Log.With("session", ses.ID, "view", attach.View)

	switch attach.View {
	case wire.ViewTerminal:
		src, err := a.open.Terminal(ctx, ses)
		if err != nil {
			a.refuse(ctx, v, err)
			return
		}
		err = v.terminal(ctx, src, Viewport{Cols: attach.Cols, Rows: attach.Rows})
		log.Debug("stream end", "err", err)
	case wire.ViewChat:
		src, err := a.open.Chat(ctx, ses)
		if err != nil {
			a.refuse(ctx, v, err)
			return
		}
		after := 0
		if attach.After != nil {
			after = *attach.After
		}
		err = v.chat(ctx, src, after)
		log.Debug("stream end", "err", err)
	default:
		_ = conn.Close(websocket.StatusPolicyViolation, "unknown view")
	}
}

func (a *API) refuse(ctx context.Context, v *viewer, err error) {
	code := "sourceFailed"
	if errors.Is(err, ErrNoView) {
		code = "unsupported"
	}
	v.fail(ctx, code, err.Error())
	_ = v.conn.Close(websocket.StatusNormalClosure, code)
}

func readAttach(ctx context.Context, conn *websocket.Conn) (wire.StreamAttach, error) {
	rctx, done := context.WithTimeout(ctx, attachTimeout)
	defer done()
	typ, data, err := conn.Read(rctx)
	if err != nil {
		return wire.StreamAttach{}, err
	}
	if typ != websocket.MessageText {
		return wire.StreamAttach{}, errors.New("the first frame must be attach")
	}
	m, err := wire.DecodeClientFrame(data)
	if err != nil {
		return wire.StreamAttach{}, err
	}
	at, ok := m.(wire.StreamAttach)
	if !ok {
		return wire.StreamAttach{}, errors.New("the first frame must be attach")
	}
	return at, nil
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
