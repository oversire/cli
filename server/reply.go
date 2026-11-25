package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/oversire/cli/wire"
)

// WriteJSON writes v as the JSON body of a reply.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the body {"error": msg}.
func WriteError(w http.ResponseWriter, code int, msg string) {
	WriteJSON(w, code, map[string]string{"error": msg})
}

// ErrTooLarge is the ReadJSON error for a body over the limit.
var ErrTooLarge = errors.New("the body is too large")

// ReadJSON reads a body of at most limit bytes and decodes it with wire.Decode.
func ReadJSON(r *http.Request, limit int64, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return ErrTooLarge
	}
	return wire.Decode(body, v)
}

// Decode is ReadJSON. On an error it writes a 400 reply with prefix before the error text and gives false.
func Decode(w http.ResponseWriter, r *http.Request, limit int64, prefix string, v any) bool {
	if err := ReadJSON(r, limit, v); err != nil {
		WriteError(w, http.StatusBadRequest, prefix+err.Error())
		return false
	}
	return true
}

// RefusalKind sets the HTTP status of a Refusal.
type RefusalKind int

const (
	NotFound RefusalKind = iota + 1
	Conflict
	Invalid
)

func (k RefusalKind) String() string {
	switch k {
	case NotFound:
		return "notFound"
	case Conflict:
		return "conflict"
	case Invalid:
		return "invalid"
	}
	return fmt.Sprintf("RefusalKind(%d)", int(k))
}

// Of tells if err is a Refusal of kind k.
func (k RefusalKind) Of(err error) bool {
	var r *Refusal
	return errors.As(err, &r) && r.Kind == k
}

// Refusal is an engine error that the caller can fix. Fail maps it to a status.
type Refusal struct {
	Kind RefusalKind
	Text string
}

func (e *Refusal) Error() string { return e.Text }

// Status gives the HTTP status of the kind.
func (e *Refusal) Status() int {
	switch e.Kind {
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case Invalid:
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// Refuse makes a Refusal with a formatted text.
func Refuse(k RefusalKind, format string, a ...any) *Refusal {
	return &Refusal{Kind: k, Text: fmt.Sprintf(format, a...)}
}

// Fail writes err as a reply: a Refusal gets its status, any other error gets 500.
func Fail(w http.ResponseWriter, err error) {
	var e *Refusal
	if errors.As(err, &e) {
		WriteError(w, e.Status(), e.Text)
		return
	}
	WriteError(w, http.StatusInternalServerError, err.Error())
}

// RefuseDaemon answers 403 with msg when the caller is a daemon. Else it calls next.
func RefuseDaemon(msg string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, _ := CallerFrom(r.Context()); c.IsDaemon() {
			WriteError(w, http.StatusForbidden, msg)
			return
		}
		next(w, r)
	}
}
