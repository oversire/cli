package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixtures(t *testing.T, kind string) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "contract", "fixtures", kind, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures for %s: %v", kind, err)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = b
	}
	return out
}

func sameJSON(t *testing.T, name string, want, got []byte) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(want, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("%s: round trip differs\nwant %s\ngot  %s", name, want, got)
	}
}

func TestWorkOrderFixtures(t *testing.T) {
	for name, data := range fixtures(t, "work-order") {
		var o WorkOrder
		if err := Decode(data, &o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, name, data, out)
	}
}

func TestLaunchRequestFixtures(t *testing.T) {
	for name, data := range fixtures(t, "launch-request") {
		var r LaunchRequest
		if err := Decode(data, &r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, _ := json.Marshal(r)
		sameJSON(t, name, data, out)
	}
}

func TestRunnerMessageFixtures(t *testing.T) {
	for name, data := range fixtures(t, "runner-message") {
		m, err := DecodeMessage(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := m.(Unknown); ok {
			t.Fatalf("%s: unknown type", name)
		}
		out, err := Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, name, data, out)
	}
}

func TestUnknownFieldsFail(t *testing.T) {
	cases := map[string]string{
		"order field":   `{"id":"o","runner":"r","runtime":"orca","op":"sessionStop","payload":{"session":{"workspace":{"account":{"provider":"orca","id":"local"},"id":"w"},"id":"s"}},"suggestion":"s","createdAt":"2026-10-01T09:10:00Z","expiresAt":"2026-10-01T21:10:00Z","state":"sent","shell":"rm -rf /"}`,
		"payload field": `{"id":"o","runner":"r","runtime":"orca","op":"sessionStop","payload":{"session":{"workspace":{"account":{"provider":"orca","id":"local"},"id":"w"},"id":"s"},"cmd":"x"},"suggestion":"s","createdAt":"2026-10-01T09:10:00Z","expiresAt":"2026-10-01T21:10:00Z","state":"sent"}`,
		"nested ref":    `{"id":"o","runner":"r","runtime":"orca","op":"sessionStop","payload":{"session":{"workspace":{"account":{"provider":"orca","id":"local","x":1},"id":"w"},"id":"s"}},"suggestion":"s","createdAt":"2026-10-01T09:10:00Z","expiresAt":"2026-10-01T21:10:00Z","state":"sent"}`,
		"unknown op":    `{"id":"o","runner":"r","runtime":"orca","op":"shell","payload":{},"suggestion":"s","createdAt":"2026-10-01T09:10:00Z","expiresAt":"2026-10-01T21:10:00Z","state":"sent"}`,
	}
	for name, data := range cases {
		var o WorkOrder
		if err := Decode([]byte(data), &o); err == nil {
			t.Errorf("%s: decode passed", name)
		}
	}
	if _, err := DecodeMessage([]byte(`{"type":"welcome","device":"d","contract":1,"extra":true}`)); err == nil {
		t.Error("welcome with an extra field passed")
	}
	var r LaunchRequest
	if err := Decode([]byte(`{"repo":"/w","agent":"claude","prompt":"go","shell":"x"}`), &r); err == nil {
		t.Error("launch request with an extra field passed")
	}
}

func TestUnknownTypeIsIgnored(t *testing.T) {
	m, err := DecodeMessage([]byte(`{"type":"store","head":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := m.(Unknown); !ok || u.Type() != "store" {
		t.Fatalf("got %#v", m)
	}
	if _, err := DecodeMessage([]byte(`{"head":12}`)); err == nil || !strings.Contains(err.Error(), "no type") {
		t.Fatalf("got %v", err)
	}
}

func TestWorkspaceNameDefaults(t *testing.T) {
	r := LaunchRequest{Item: &ItemRef{Key: "ENG-4", URL: "https://x/ENG-4"}}
	if r.WorkspaceName() != "ENG-4" || r.WorkspaceNote() != "https://x/ENG-4" {
		t.Fatal(r.WorkspaceName(), r.WorkspaceNote())
	}
	r.Name, r.Note = "n", "m"
	if r.WorkspaceName() != "n" || r.WorkspaceNote() != "m" {
		t.Fatal("explicit values lost")
	}
}
