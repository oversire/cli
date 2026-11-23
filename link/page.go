package link

import (
	"errors"
	"html/template"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

const (
	repeatWait = 3 * time.Second
	pushLimit  = 30 // pushes for each minute, from all addresses
)

// gate stops a double click or a reload from a second push, and limits the pushes.
type gate struct {
	mu     sync.Mutex
	last   map[string]pageData
	lastAt map[string]time.Time
	pushes []time.Time
}

func newGate() gate { return gate{last: map[string]pageData{}, lastAt: map[string]time.Time{}} }

// seen gives the last page of the same click in the repeat wait.
func (g *gate) seen(key string, now time.Time) (pageData, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, at := range g.lastAt {
		if now.Sub(at) >= repeatWait {
			delete(g.lastAt, k)
			delete(g.last, k)
		}
	}
	d, ok := g.last[key]
	return d, ok
}

// allow counts one push. It is false after pushLimit pushes in the last minute.
func (g *gate) allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := 0
	for i < len(g.pushes) && now.Sub(g.pushes[i]) >= time.Minute {
		i++
	}
	g.pushes = g.pushes[i:]
	if len(g.pushes) >= pushLimit {
		return false
	}
	g.pushes = append(g.pushes, now)
	return true
}

func (g *gate) keep(key string, d pageData, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last[key], g.lastAt[key] = d, now
}

type pageData struct {
	Status   int
	Title    string
	Lines    []string
	App      template.URL
	Redirect bool
}

var page = template.Must(template.New("go").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Redirect}}<meta http-equiv="refresh" content="0;url={{.App}}">{{end}}
<title>{{.Title}}</title>
<style>
body{font:15px -apple-system,BlinkMacSystemFont,sans-serif;margin:15vh auto;max-width:34em;padding:0 1em;color:#1d1d1f;background:#fff}
@media (prefers-color-scheme:dark){body{color:#f5f5f7;background:#1d1d1f}a{color:#56b4e9}}
h1{font-size:19px;font-weight:600}p{line-height:1.45}.app{font:13px ui-monospace,monospace;word-break:break-all}
</style></head><body>
<h1>{{.Title}}</h1>
{{range .Lines}}<p>{{.}}</p>{{end}}
{{if .App}}<p class="app">App link: <a href="{{.App}}">{{.App}}</a></p>{{end}}
</body></html>
`))

func (o *Opener) serveGo(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	p, err := ParsePath(r.PathValue("path"), r.URL.Query().Get("focus"))
	if err != nil {
		o.render(w, pageData{Status: http.StatusBadRequest, Title: ErrBadRoute.Error()})
		return
	}
	app := template.URL(p.App())
	what := p.Route
	if k := p.Item(); k != "" {
		what = k
	}
	if r.Method == http.MethodHead || r.Header.Get("Sec-Purpose") == "prefetch" || r.Header.Get("Purpose") == "prefetch" {
		o.render(w, pageData{Status: http.StatusOK, Title: "Open " + what + " in OverSire", App: app})
		return
	}
	click := netip.Addr{}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		click = ap.Addr().Unmap()
	}
	now := o.now()
	key := click.String() + " " + p.Route + "#" + p.Focus
	if d, ok := o.gate.seen(key, now); ok {
		o.render(w, d)
		return
	}
	if !o.gate.allow(now) {
		o.render(w, pageData{Status: http.StatusTooManyRequests, Title: "Too many links. Wait a minute.", App: app})
		return
	}
	d := pageData{Status: http.StatusOK, App: app}
	res, err := o.Open(r.Context(), p, click, "", wire.OpenWeb, "")
	var nr *NotRunningError
	switch {
	case err == nil && res.How == wire.OpenByClick:
		d.Title = "Opened " + what + " in OverSire on " + res.Machine + "."
		d.Lines = []string{"You can close this tab."}
	case err == nil:
		d.Title = "Opened " + what + " in OverSire on " + res.Machine + ", the Mac with the latest input."
		d.Lines = []string{"You can close this tab."}
		d.Redirect = true
	case errors.As(err, &nr):
		d.Status, d.Title, d.Redirect = http.StatusServiceUnavailable, nr.Error(), true
	default:
		d.Status, d.Title, d.Redirect = http.StatusServiceUnavailable, ErrNoApp.Error(), true
	}
	o.gate.keep(key, d, now)
	o.render(w, d)
}

func (o *Opener) render(w http.ResponseWriter, d pageData) {
	w.WriteHeader(d.Status)
	if err := page.Execute(w, d); err != nil {
		o.Log.Debug("link: page", "err", err)
	}
}
