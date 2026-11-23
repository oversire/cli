// Package link makes the links that open a page of the app on the Mac of the click (contract link.json).
// GET /go/<route> finds the Mac of the click from the remote address and pushes an openRoute frame to its app.
package link

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Path is a route of the app plus a row to show.
type Path struct {
	Route string
	Focus string
}

var ErrBadRoute = errors.New("This is not an OverSire link.")

// heads is the RouteHead enum of contract/schema/link.json.
var heads = []string{"home", "inbox", "intake", "activity", "work", "mine", "sessions", "agents", "queue", "plan", "runs", "run",
	"prs", "insights", "settings", "project", "item", "epic"}

var (
	segment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	keyArg  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)
)

const maxRoute = 256

// ParsePath checks a route and a focus. It refuses a scheme, a host, "..", an empty segment and an unknown head,
// so that a page cannot send the browser to another place.
func ParsePath(route, focus string) (Path, error) {
	route = strings.TrimPrefix(route, "/")
	if route == "" || len(route) > maxRoute {
		return Path{}, ErrBadRoute
	}
	parts := strings.Split(route, "/")
	if len(parts) > 5 || !slices.Contains(heads, strings.ToLower(parts[0])) {
		return Path{}, ErrBadRoute
	}
	for _, p := range parts[1:] {
		if !segment.MatchString(p) || strings.Contains(p, "..") {
			return Path{}, ErrBadRoute
		}
	}
	if focus != "" && !segment.MatchString(focus) {
		return Path{}, ErrBadRoute
	}
	return Path{Route: route, Focus: focus}, nil
}

// ParseArg reads the argument of `oversire open`: an item key, a route, an oversire:// link or a /go/ web link.
func ParseArg(arg string) (Path, error) {
	arg = strings.TrimSpace(arg)
	if keyArg.MatchString(arg) {
		return Path{Route: "item/" + strings.ToUpper(arg)}, nil
	}
	if !strings.Contains(arg, "://") {
		route, focus, _ := strings.Cut(arg, "#")
		return ParsePath(route, focus)
	}
	u, err := url.Parse(arg)
	if err != nil {
		return Path{}, ErrBadRoute
	}
	switch strings.ToLower(u.Scheme) {
	case "oversire":
		return ParsePath(strings.TrimPrefix(u.Host+u.Path, "/"), u.Fragment)
	case "http", "https":
		route, ok := strings.CutPrefix(u.Path, "/go/")
		if !ok {
			return Path{}, ErrBadRoute
		}
		return ParsePath(route, u.Query().Get("focus"))
	}
	return Path{}, ErrBadRoute
}

// App gives the oversire:// link, for example oversire://item/OVERSIRE-400/changes#OVERSIRE-402.
func (p Path) App() string {
	if p.Focus == "" {
		return "oversire://" + p.Route
	}
	return "oversire://" + p.Route + "#" + p.Focus
}

// Web gives the /go/ link on the daemon at base. Empty base: empty.
func (p Path) Web(base string) string {
	if base == "" {
		return ""
	}
	s := strings.TrimSuffix(base, "/") + "/go/" + p.Route
	if p.Focus != "" {
		s += "?focus=" + url.QueryEscape(p.Focus)
	}
	return s
}

// Item gives the key of an item route, else "".
func (p Path) Item() string {
	parts := strings.Split(p.Route, "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "item") {
		return ""
	}
	return strings.ToUpper(parts[1])
}

// Print writes the links. A terminal gets an OSC 8 link; web is the /go/ link, empty when the daemon has none.
func (p Path) Print(w io.Writer, web string, tty bool) error {
	app := p.App()
	var err error
	if tty {
		_, err = fmt.Fprintf(w, "Open in OverSire: \x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\\n", app, app)
	} else {
		_, err = fmt.Fprintf(w, "App link: %s\n", app)
	}
	if err == nil && web != "" {
		_, err = fmt.Fprintf(w, "Web link: %s (⌘-click)\n", web)
	}
	return err
}
