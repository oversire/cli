package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// RequestID is the header that the app sends on each request. The app log and the daemon log both show it.
const RequestID = "X-Request-Id"

// accessLog writes one line for each request: the method, the path, the status, the time, the caller and the request id.
// A failure has the error text. A read that works and a routine write are at level debug. No headers, bodies or query.
func (s *Server) accessLog(via string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get(RequestID)
		if id != "" {
			w.Header().Set(RequestID, id)
		}
		n := &note{caller: via}
		attrs := func() []any {
			a := []any{"method", r.Method, "path", r.URL.Path, "via", via, "caller", n.caller, "ms", time.Since(start).Milliseconds()}
			if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && via != "loopback" {
				a = append(a, "remote", host)
			}
			if id != "" {
				a = append(a, "id", id)
			}
			return a
		}
		rec := &recorder{ResponseWriter: w, onHijack: func() { s.Log.Info("websocket open", attrs()...) }}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), noteKey{}, n)))

		if rec.hijacked {
			s.Log.Info("websocket close", attrs()...)
			return
		}
		a := append(attrs(), "status", rec.code(), "bytes", rec.bytes)
		switch code := rec.code(); {
		case code >= 500:
			s.Log.Error("request", append(a, "err", rec.errText())...)
		case code >= 400:
			s.Log.Warn("request", append(a, "err", rec.errText())...)
		case writes(r.Method) && !routine(r.URL.Path):
			s.Log.Info("request", a...)
		default:
			s.Log.Debug("request", a...)
		}
	})
}

// routine tells a write that comes many times a minute: an agent hook, an event ack or a lease renew.
func routine(path string) bool {
	return path == "/v1/ack" || strings.HasPrefix(path, "/v1/hooks/") || strings.HasPrefix(path, "/v1/leases/")
}

// note carries the caller from the access rule back to the access log.
type note struct{ caller string }

type noteKey struct{}

func setCaller(ctx context.Context, name string) {
	if n, ok := ctx.Value(noteKey{}).(*note); ok && name != "" {
		n.caller = name
	}
}

// recorder keeps the status, the size and the start of an error body. A WebSocket can still take the connection.
type recorder struct {
	http.ResponseWriter
	status   int
	bytes    int
	body     []byte
	hijacked bool
	onHijack func()
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.status >= 400 && len(r.body) < 1024 {
		r.body = append(r.body, b[:min(len(b), 1024-len(r.body))]...)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("server: the connection cannot be hijacked")
	}
	r.hijacked = true
	r.onHijack()
	return h.Hijack()
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// errText is the error of a JSON error body, else the start of the body.
func (r *recorder) errText() string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(r.body, &e) == nil && e.Error != "" {
		return e.Error
	}
	t := strings.Join(strings.Fields(string(r.body)), " ")
	if len(t) > 300 {
		t = t[:299] + "…"
	}
	return t
}
