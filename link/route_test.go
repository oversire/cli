package link

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestParsePath(t *testing.T) {
	ok := []string{"item/OVERSIRE-400", "/item/oversire-400/changes", "home", "run/r-1.a_b", "item/A-1/b/c/d", "SETTINGS"}
	for _, r := range ok {
		if _, err := ParsePath(r, ""); err != nil {
			t.Errorf("%q: %v", r, err)
		}
	}
	bad := []string{"", "nope/x", "item//x", "item/../x", "item/a..b", "item/a/b/c/d/e", "item/-x", "javascript:alert(1)",
		"item/x?y", "item/" + string(bytes.Repeat([]byte("a"), 300))}
	for _, r := range bad {
		if _, err := ParsePath(r, ""); err != ErrBadRoute {
			t.Errorf("%q: got %v", r, err)
		}
	}
	if _, err := ParsePath("item/A-1", "a/b"); err != ErrBadRoute {
		t.Errorf("a bad focus: %v", err)
	}
	p, _ := ParsePath("/item/A-1/changes", "A-2")
	if p != (Path{"item/A-1/changes", "A-2"}) {
		t.Errorf("got %+v", p)
	}
}

func TestHeadsMatchContract(t *testing.T) {
	b, err := os.ReadFile("../contract/schema/link.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs struct {
			RouteHead struct {
				Enum []string `json:"enum"`
			} `json:"RouteHead"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Defs.RouteHead.Enum, heads) {
		t.Fatalf("contract %v, code %v", s.Defs.RouteHead.Enum, heads)
	}
}

func TestParseArg(t *testing.T) {
	cases := map[string]Path{
		"oversire-400":                            {Route: "item/OVERSIRE-400"},
		" OVERSIRE-400 ":                          {Route: "item/OVERSIRE-400"},
		"item/OVERSIRE-400/changes#A-2":           {Route: "item/OVERSIRE-400/changes", Focus: "A-2"},
		"oversire://item/OVERSIRE-400/plan":       {Route: "item/OVERSIRE-400/plan"},
		"oversire://item/A-1#A-2":                 {Route: "item/A-1", Focus: "A-2"},
		"http://mbp.ts.net:7690/go/item/A-1":      {Route: "item/A-1"},
		"https://x/go/item/A-1/changes?focus=A-2": {Route: "item/A-1/changes", Focus: "A-2"},
	}
	for arg, want := range cases {
		got, err := ParseArg(arg)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v", arg, got, err)
		}
	}
	for _, arg := range []string{"http://x/item/A-1", "ftp://x/go/item/A-1", "oversire://evil/x", "nothing here"} {
		if _, err := ParseArg(arg); err == nil {
			t.Errorf("%q: no error", arg)
		}
	}
}

func TestPathAppWebPrint(t *testing.T) {
	p := Path{Route: "item/A-1/changes", Focus: "A-2"}
	if p.App() != "oversire://item/A-1/changes#A-2" {
		t.Errorf("app %s", p.App())
	}
	if p.Web("http://mbp:7690/") != "http://mbp:7690/go/item/A-1/changes?focus=A-2" || p.Web("") != "" {
		t.Errorf("web %s", p.Web("http://mbp:7690/"))
	}
	if p.Item() != "A-1" || (Path{Route: "home"}).Item() != "" {
		t.Error("item")
	}
	var b bytes.Buffer
	_ = Path{Route: "item/A-1"}.Print(&b, "http://mbp:7690/go/item/A-1", false)
	if b.String() != "App link: oversire://item/A-1\nWeb link: http://mbp:7690/go/item/A-1 (⌘-click)\n" {
		t.Errorf("no tty: %q", b.String())
	}
	b.Reset()
	_ = Path{Route: "item/A-1"}.Print(&b, "", true)
	if b.String() != "Open in OverSire: \x1b]8;;oversire://item/A-1\x1b\\oversire://item/A-1\x1b]8;;\x1b\\\n" {
		t.Errorf("tty: %q", b.String())
	}
}
