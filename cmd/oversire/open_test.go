package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/link"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

type oneGroup struct{}

func (oneGroup) Snapshot() (wire.Group, bool) {
	return wire.Group{Name: "My machines", Members: []wire.GroupMember{{Machine: "mbp"}, {Machine: "mini"}}}, true
}
func (oneGroup) Self() wire.GroupMember { return wire.GroupMember{Machine: "mbp"} }

type noEvents struct{}

func (noEvents) AddFor(wire.DaemonSession, wire.EventDraft) {}

// linkDaemon runs a daemon with the link routes. The app of mbp is on loopback and gives its presence.
// It gives the openRoute frames that the app gets.
func linkDaemon(t *testing.T, c *cli) <-chan wire.OpenRoute {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("OVERSIRE_SESSION", "")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	off := false
	cfg := config.ServeConfig{Port: port, Tailscale: &off}
	if err := c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &cfg}); err != nil {
		t.Fatal(err)
	}
	events := eventlog.New(10)
	info := func() wire.DaemonInfo {
		return wire.DaemonInfo{Machine: "mbp", Version: "test", Contract: wire.DaemonContract, Features: []string{wire.FeatureLinks}}
	}
	srv := server.NewServer(cfg, info, nil, events, sessions.NewBook("mbp", "", events, nil), nil)
	srv.Log = c.log
	srv.Web = func() string { return "http://mbp.tail8ed2.ts.net:7690" }
	o := link.NewOpener(srv, link.NewBook(time.Now), nil, oneGroup{}, noEvents{}, time.Now)
	o.Log = c.log
	srv.Handle(o.Routes()...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	var conn *websocket.Conn
	for i := 0; ; i++ {
		dctx, dcancel := context.WithTimeout(ctx, time.Second)
		conn, _, err = websocket.Dial(dctx, "ws://127.0.0.1:"+strconv.Itoa(port)+"/v1/connect", nil)
		dcancel()
		if err == nil {
			break
		}
		if i == 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { conn.CloseNow() })
	d, _ := c.daemonClient()
	if err := d.call(ctx, "POST", "/v1/presence", wire.AppPresence{Idle: 14}, nil); err != nil {
		t.Fatal(err)
	}
	frames := make(chan wire.OpenRoute, 4)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f wire.OpenRouteFrame
			if json.Unmarshal(data, &f) == nil && f.Type == wire.FrameOpenRoute {
				frames <- f.Open
			}
		}
	}()
	return frames
}

func TestOpenCommand(t *testing.T) {
	c, out, _ := testCLI(t, "")
	frames := linkDaemon(t, c)
	ctx := context.Background()
	if err := c.dispatch(ctx, []string{"open", "oversire-400", "--harness", "claude", "--session", "s-1"}); err != nil {
		t.Fatal(err)
	}
	want := "Opened on mbp (active 14 s ago, from the \"My machines\" group)\n" +
		"App link: oversire://item/OVERSIRE-400\nWeb link: http://mbp.tail8ed2.ts.net:7690/go/item/OVERSIRE-400 (⌘-click)\n"
	if out.String() != want {
		t.Fatalf("got %q", out.String())
	}
	select {
	case o := <-frames:
		if o.Path != "item/OVERSIRE-400" || o.Source != wire.OpenCLI || o.Daemon != "mbp" || o.Agent != "claude s-1" {
			t.Fatalf("frame %+v", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame")
	}

	out.Reset()
	c.tty = func(io.Writer) bool { return true }
	if err := c.dispatch(ctx, []string{"open", "item/OVERSIRE-400/changes#OVERSIRE-402", "--on", "mbp"}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.HasPrefix(s, "Opened on mbp\nOpen in OverSire: \x1b]8;;oversire://item/OVERSIRE-400/changes#OVERSIRE-402\x1b\\") ||
		!strings.Contains(s, "/go/item/OVERSIRE-400/changes?focus=OVERSIRE-402 (⌘-click)") {
		t.Fatalf("got %q", s)
	}
	if o := <-frames; o.Focus != "OVERSIRE-402" {
		t.Fatalf("frame %+v", o)
	}

	out.Reset()
	if err := c.dispatch(ctx, []string{"open", "OVERSIRE-1", "--json"}); err != nil {
		t.Fatal(err)
	}
	var res wire.OpenResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Machine != "mbp" || res.How != wire.OpenByInput {
		t.Fatalf("json %s", out)
	}

	for args, code := range map[string]int{"open": 2, "open a b": 2, "open nope/x": 2, "open OVERSIRE-1 --on zzz": 1, "open OVERSIRE-1 --on mini": 1} {
		err := c.dispatch(ctx, strings.Fields(args))
		if exitCode(err) != code {
			t.Errorf("%s: %v", args, err)
		}
	}
	if err := c.dispatch(ctx, []string{"open", "OVERSIRE-1", "--on", "mini"}); err == nil || err.Error() != "OverSire on mini is not running." {
		t.Fatalf("not running: %v", err)
	}
}

func TestOpenOnAnOldDaemon(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	err := c.dispatch(context.Background(), []string{"open", "OVERSIRE-1"})
	if exitCode(err) != 1 || err.Error() != "Update the daemon: oversire update" {
		t.Fatalf("got %v", err)
	}
}
