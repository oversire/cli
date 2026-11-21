package ci

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// ForwardedHeader marks a request that a member daemon sent to home.
const ForwardedHeader = "X-OverSire-Forwarded"

// HomeFinder is the group (group.Store). The mirror home is the home of the group also when the group has a cloud.
type HomeFinder interface {
	MirrorHome() (base, token string, self bool)
	Info() (id, home string)
	Self() wire.GroupMember
}

// Forward sends a /v1/ci request of a member daemon to home. Only home has the mirror.
type Forward struct {
	g      HomeFinder
	client *http.Client
}

func NewForward(g HomeFinder, client *http.Client) *Forward {
	return &Forward{g: g, client: client}
}

// Wrap runs next on home. On a member it sends the request to home and copies the answer.
func (f *Forward) Wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		base, token, self := f.g.MirrorHome()
		if self {
			next(w, req)
			return
		}
		_, home := f.g.Info()
		if req.Header.Get(ForwardedHeader) != "" {
			server.WriteJSON(w, http.StatusConflict, map[string]string{"error": home + " is home", "home": home})
			return
		}
		f.send(w, req, base, token, home)
	}
}

func (f *Forward) send(w http.ResponseWriter, req *http.Request, base, token, home string) {
	out, err := http.NewRequestWithContext(req.Context(), req.Method, base+req.URL.Path, req.Body)
	if err != nil {
		server.WriteError(w, http.StatusBadGateway, f.noRoute(home))
		return
	}
	out.URL.RawQuery = req.URL.RawQuery
	out.ContentLength = req.ContentLength
	if ct := req.Header.Get("Content-Type"); ct != "" {
		out.Header.Set("Content-Type", ct)
	}
	out.Header.Set("Authorization", "Bearer "+token)
	out.Header.Set(ForwardedHeader, "1")
	resp, err := f.client.Do(out)
	if err != nil {
		var dns *net.DNSError
		switch {
		case req.Context().Err() != nil:
		case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.As(err, &dns):
			server.WriteError(w, http.StatusBadGateway, f.noRoute(home))
		default:
			server.WriteError(w, http.StatusServiceUnavailable, home+" (home) is offline.")
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		server.WriteError(w, http.StatusBadGateway, f.noRoute(home))
		return
	}
	for _, h := range []string{"Content-Type", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (f *Forward) noRoute(home string) string {
	return fmt.Sprintf("%s cannot reach %s (home).", f.g.Self().Machine, home)
}
