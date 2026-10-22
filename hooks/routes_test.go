package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func TestRoutes(t *testing.T) {
	fs := memFS{"/h/.claude/settings.json": read(t, "claude/settings.json")}
	routes := map[string]server.Route{}
	for _, r := range NewInstaller("/h", testExe).WithFS(fs).Routes() {
		if r.Access != server.LoopbackOnly {
			t.Fatalf("%s is not loopback only", r.Pattern)
		}
		routes[strings.Fields(r.Pattern)[0]] = r
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
