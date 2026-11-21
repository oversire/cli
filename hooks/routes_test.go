package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func TestRoutes(t *testing.T) {
	fs := memFS{"/h/.claude/settings.json": read(t, "claude/settings.json")}
	routes := map[string]server.Route{}
	for _, r := range NewInstaller("/h", testExe).WithFS(fs).Routes(NewSettings(config.Paths{Dir: t.TempDir()})) {
		if r.Access != server.LoopbackOnly {
			t.Fatalf("%s is not loopback only", r.Pattern)
		}
		if strings.HasSuffix(r.Pattern, "/install") {
			routes[strings.Fields(r.Pattern)[0]] = r
		}
	}
	call := func(method, body string) (int, []wire.HarnessHooks) {
		w := httptest.NewRecorder()
		routes[method].Handle(w, httptest.NewRequest(method, "/v1/hooks/install", strings.NewReader(body)))
		var st []wire.HarnessHooks
		_ = json.Unmarshal(w.Body.Bytes(), &st)
		return w.Code, st
	}
	if code, st := call("GET", ""); code != http.StatusOK || st[0].Hooks != wire.HooksMissing {
		t.Fatalf("get: %d %+v", code, st)
	}
	if code, st := call("POST", `{"harnesses":["claude"]}`); code != http.StatusOK || st[0].Hooks != wire.HooksInstalled || st[1].Hooks != wire.HooksMissing {
		t.Fatalf("post: %d %+v", code, st)
	}
	if code, _ := call("POST", `{"harnesses":["cursor"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown harness: %d", code)
	}
	if code, st := call("DELETE", `{"harnesses":["claude"]}`); code != http.StatusOK || st[0].Hooks != wire.HooksMissing {
		t.Fatalf("delete: %d %+v", code, st)
	}
}

func TestSettingsRoutes(t *testing.T) {
	dir := t.TempDir()
	routes := map[string]server.Route{}
	for _, r := range NewInstaller("/h", testExe).WithFS(memFS{}).Routes(NewSettings(config.Paths{Dir: dir})) {
		if strings.HasSuffix(r.Pattern, "/settings") {
			if r.Access != server.LoopbackOnly {
				t.Fatalf("%s is not loopback only", r.Pattern)
			}
			routes[strings.Fields(r.Pattern)[0]] = r
		}
	}
	if len(routes) != 2 {
		t.Fatalf("routes %v", routes)
	}
	call := func(method, body string) (int, wire.HookSettings) {
		w := httptest.NewRecorder()
		routes[method].Handle(w, httptest.NewRequest(method, "/v1/hooks/settings", strings.NewReader(body)))
		var v wire.HookSettings
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	if code, v := call("GET", ""); code != http.StatusOK || !v.CIRedirect {
		t.Fatalf("empty config: %d %+v", code, v)
	}
	if code, v := call("PUT", `{"ciRedirect":false}`); code != http.StatusOK || v.CIRedirect {
		t.Fatalf("put: %d %+v", code, v)
	}
	if code, v := call("GET", ""); code != http.StatusOK || v.CIRedirect {
		t.Fatalf("get after put: %d %+v", code, v)
	}
	cfg, _, _ := config.Paths{Dir: dir}.LoadConfig()
	if cfg.CIRedirect() || cfg.Hooks == nil {
		t.Fatalf("config %+v", cfg)
	}
	for _, body := range []string{`{`, `{}`, `{"ciRedirect":1}`, `{"ciRedirect":true,"x":1}`} {
		if code, _ := call("PUT", body); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, code)
		}
	}
	if _, v := call("GET", ""); v.CIRedirect {
		t.Fatal("a bad body changed the setting")
	}
}
