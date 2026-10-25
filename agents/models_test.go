package agents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/wire"
)

type fakeLister struct {
	models map[agentwire.Harness][]agentwire.ModelInfo
	err    error
	calls  int
}

func (f *fakeLister) Models(_ context.Context, h agentwire.Harness, _ agentwire.ModelQuery) ([]agentwire.ModelInfo, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.models[h]; ok {
		return m, nil
	}
	return nil, agentwire.ErrUnsupported
}

func get(t *testing.T, a *API, agent string) (int, wire.AgentModels) {
	t.Helper()
	mux := http.NewServeMux()
	for _, r := range a.Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/"+agent+"/models", nil))
	var out wire.AgentModels
	if rec.Code == http.StatusOK {
		if err := wire.Decode(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, out
}

func TestModels(t *testing.T) {
	l := &fakeLister{models: map[agentwire.Harness][]agentwire.ModelInfo{
		agentwire.Claude: {{ID: "default", Name: "Default", Default: true}, {ID: "claude-opus-5-5", Name: "Opus 5.5", Description: "1M"}},
	}}
	a := NewAPI(l)

	code, out := get(t, a, "claude")
	want := wire.AgentModels{Agent: "claude", Source: wire.ModelSourceWire, Models: []wire.AgentModel{
		{ID: "default", Name: "Default", IsDefault: true}, {ID: "claude-opus-5-5", Name: "Opus 5.5", Description: "1M"}}}
	if code != http.StatusOK || !same(out, want) {
		t.Fatalf("claude %d %#v", code, out)
	}

	code, out = get(t, a, "gemini")
	if code != http.StatusOK || out.Source != wire.ModelSourceNone || len(out.Models) != 0 || out.Reason == "" {
		t.Fatalf("gemini %d %#v", code, out)
	}

	for _, agent := range []string{"nope", "fake", "native"} {
		if code, _ := get(t, a, agent); code != http.StatusNotFound {
			t.Fatalf("%s: %d", agent, code)
		}
	}

	l.err = errors.New("claude is not logged in")
	code, out = get(t, a, "claude")
	if code != http.StatusOK || out.Source != wire.ModelSourceNone || out.Reason != "claude is not logged in" || out.Models == nil {
		t.Fatalf("error %d %#v", code, out)
	}
}

func same(a, b wire.AgentModels) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
