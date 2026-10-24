package orcaws_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/runtime/orca/orcaws"
	"github.com/oversire/cli/runtime/orca/orcaws/orcawstest"
)

func TestParseLink(t *testing.T) {
	srv := orcawstest.New()
	defer srv.Close()
	want := srv.Offer()
	for _, link := range []string{
		srv.Link(),
		"  " + srv.Link() + "\n",
		"orca://pair#" + want.Code(),
		want.Code(),
		base64.StdEncoding.EncodeToString(mustJSON(want)), // padded standard base64
	} {
		got, err := orcaws.ParseLink(link)
		if err != nil || got != want {
			t.Fatalf("%q: %v %+v", link, err, got)
		}
	}
	bad := func(o map[string]any) string { return base64.RawURLEncoding.EncodeToString(mustJSON(o)) }
	for name, c := range map[string]struct{ link, err string }{
		"empty":   {"", "empty"},
		"host":    {"orca://pairing?code=" + want.Code(), "orca://pair"},
		"path":    {"orca://pair/x?code=" + want.Code(), "orca://pair"},
		"junk":    {"%%%", "not valid"},
		"version": {bad(map[string]any{"v": 3, "endpoint": "ws://x", "deviceToken": "t", "publicKeyB64": want.PublicKey}), "version 3"},
		"mobile":  {bad(map[string]any{"v": 2, "endpoint": "ws://x", "deviceToken": "t", "publicKeyB64": want.PublicKey, "scope": "mobile"}), "mobile"},
		"relay":   {bad(map[string]any{"v": 2, "endpoint": "ws://x", "deviceToken": "t", "publicKeyB64": want.PublicKey, "relay": map[string]any{"v": 1}}), "relay"},
		"key":     {bad(map[string]any{"v": 2, "endpoint": "ws://x", "deviceToken": "t", "publicKeyB64": "AAAA"}), "key"},
		"scheme":  {bad(map[string]any{"v": 2, "endpoint": "http://x", "deviceToken": "t", "publicKeyB64": want.PublicKey}), "endpoint"},
		"token":   {bad(map[string]any{"v": 2, "endpoint": "ws://x", "publicKeyB64": want.PublicKey}), "token"},
	} {
		if _, err := orcaws.ParseLink(c.link); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestCallAndStream(t *testing.T) {
	srv := orcawstest.New()
	defer srv.Close()
	srv.Handle = func(p *orcawstest.Peer, r orcawstest.Request) {
		switch r.Method {
		case "echo":
			p.Keepalive()
			p.Reply(r.ID, json.RawMessage(r.Params), false)
		case "count":
			for i := range 3 {
				p.Reply(r.ID, map[string]int{"n": i}, true)
			}
			p.Fail(r.ID, "done", "the end")
		default:
			p.Fail(r.ID, "method_not_found", "no "+r.Method)
		}
	}
	ctx := context.Background()
	c, err := orcaws.Dial(ctx, srv.Offer())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var st orcaws.Status
	if err := c.Call(ctx, "status.get", nil, &st); err != nil || st.Check() != nil {
		t.Fatal(err, st)
	}
	var echo map[string]string
	if err := c.Call(ctx, "echo", map[string]string{"a": "b"}, &echo); err != nil || echo["a"] != "b" {
		t.Fatal(err, echo)
	}
	if err := c.Call(ctx, "nope", nil, nil); err == nil || !strings.Contains(err.Error(), "method_not_found") {
		t.Fatal(err)
	}
	ch, err := c.Stream(ctx, "count", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for r := range ch {
		if r.OK {
			got = append(got, string(r.Result))
		} else {
			got = append(got, r.Error.Code)
		}
	}
	if strings.Join(got, " ") != `{"n":0} {"n":1} {"n":2} done` {
		t.Fatal(got)
	}
}

func TestDialRefused(t *testing.T) {
	srv := orcawstest.New()
	defer srv.Close()
	o := srv.Offer()
	o.DeviceToken = "wrong"
	if _, err := orcaws.Dial(context.Background(), o); err == nil || !strings.Contains(err.Error(), "Pair again") {
		t.Fatal(err)
	}
	o = srv.Offer()
	o.PublicKey = orcawstest.New().Offer().PublicKey // another host: the auth frame does not open
	if _, err := orcaws.Dial(context.Background(), o); err == nil {
		t.Fatal("a wrong host key must fail")
	}
	o = srv.Offer()
	o.Endpoint = "ws://127.0.0.1:1"
	if _, err := orcaws.Dial(context.Background(), o); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatal(err)
	}
}

func TestDropEndsStream(t *testing.T) {
	srv := orcawstest.New()
	defer srv.Close()
	srv.Handle = func(p *orcawstest.Peer, r orcawstest.Request) {
		p.Reply(r.ID, "x", true)
		p.Drop()
	}
	c, err := orcaws.Dial(context.Background(), srv.Offer())
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := c.Stream(context.Background(), "s", nil)
	n := 0
	for range ch {
		n++
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("the socket did not end")
	}
	if n != 1 || c.Err() == nil {
		t.Fatal(n, c.Err())
	}
}

func TestStatusCheck(t *testing.T) {
	for _, c := range []struct {
		st orcaws.Status
		ok bool
	}{
		{orcaws.Status{ProtocolVersion: 3, MinClientVersion: 2}, true},
		{orcaws.Status{ProtocolVersion: 2}, true},
		{orcaws.Status{ProtocolVersion: 4, MinClientVersion: 4}, false},
		{orcaws.Status{}, false},
	} {
		if err := c.st.Check(); (err == nil) != c.ok {
			t.Error(c.st, err)
		}
	}
}
