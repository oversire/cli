package orca

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

const screenB = `{"ok":true,"result":{"terminal":{"tail":["$ ls","a  b"]}}}`
const goneReply = `{"ok":false,"error":{"code":"terminal_handle_stale","message":"The terminal is gone."}}`

func TestPollAttachRepaintsOnChange(t *testing.T) {
	r := newReplay()
	r.on("terminal read", recorded(t, "terminal-read-screen.json"), recorded(t, "terminal-read-screen.json"), screenB, goneReply)
	src, err := New("orca", r).Terminal(context.Background(), wire.DaemonSession{ID: "tab:leaf", Terminal: "term_1"})
	if err != nil {
		t.Fatal(err)
	}
	p := src.(*Poll)
	p.every = 5 * time.Millisecond
	if c := p.Caps(); c.Source != wire.SourcePoll || !c.Writable || c.Resizable || c.Reason == "" {
		t.Fatalf("caps %+v", c)
	}
	snap, chunks, err := p.Attach(context.Background(), term.Viewport{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(snap.Data), "\x1b[H\x1b[2J╭") || !strings.Contains(string(snap.Data), "\r\n> fix the test") {
		t.Fatalf("snapshot %q", snap.Data)
	}
	if snap.Size != (term.Viewport{Cols: 29, Rows: 3}) {
		t.Fatalf("size %+v", snap.Size)
	}
	// The same screen gives nothing; a new screen gives a repaint; a gone terminal ends the view.
	c := <-chunks
	if c.Repaint == nil || string(c.Repaint.Data) != "\x1b[H\x1b[2J$ ls\r\na  b" {
		t.Fatalf("chunk %+v", c)
	}
	if c := <-chunks; c.Ended != "exited" {
		t.Fatalf("chunk %+v", c)
	}
	if _, ok := <-chunks; ok {
		t.Fatal("the channel is open")
	}
	args := r.args("terminal read")[0]
	if !slices.Equal(args, []string{"terminal", "read", "--terminal", "term_1", "--screen", "--json"}) {
		t.Fatalf("args %q", args)
	}
}

func TestPollEndsAfterFailures(t *testing.T) {
	r := newReplay()
	r.on("terminal read", screenB, `{"ok":false,"error":{"code":"runtime_unavailable","message":"down"}}`)
	src, _ := New("orca", r).Terminal(context.Background(), wire.DaemonSession{Terminal: "term_1"})
	p := src.(*Poll)
	p.every = time.Millisecond
	_, chunks, err := p.Attach(context.Background(), term.Viewport{})
	if err != nil {
		t.Fatal(err)
	}
	if c := <-chunks; c.Ended != "closed" {
		t.Fatalf("chunk %+v", c)
	}
	if n := len(r.args("terminal read")); n != 1+pollFailures {
		t.Fatalf("reads %d", n)
	}
}

func TestPollWriteSplitsKeys(t *testing.T) {
	r := newReplay()
	r.on("terminal send", `{"ok":true,"result":{}}`)
	src, _ := New("orca", r).Terminal(context.Background(), wire.DaemonSession{Terminal: "term_1"})
	if err := src.Write(context.Background(), []byte("--help\r\rab\x03\x1b[A\x00")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range r.args("terminal send") {
		got = append(got, strings.Join(a[4:len(a)-1], " "))
	}
	want := []string{"--text=--help --enter", "--enter", "--text=ab", "--interrupt", "--text=\x1b[A"}
	if !slices.Equal(got, want) {
		t.Fatalf("sends %q", got)
	}
	if err := src.Control(context.Background(), &term.Viewport{Cols: 1, Rows: 1}); !errors.Is(err, term.ErrNoView) {
		t.Fatalf("control %v", err)
	}
}

func TestTerminalByPaneKey(t *testing.T) {
	r := newReplay()
	r.on("terminal list", `{"ok":true,"result":{"terminals":[{"handle":"term_old","tabId":"t","leafId":"l","orphaned":true},{"handle":"term_new","tabId":"t","leafId":"l"}]}}`)
	src, err := New("orca", r).Terminal(context.Background(), wire.DaemonSession{ID: "t:l"})
	if err != nil || src.(*Poll).terminal != "term_new" {
		t.Fatalf("%v %v", src, err)
	}
	if _, err := New("orca", r).Terminal(context.Background(), wire.DaemonSession{ID: "x:y"}); !errors.Is(err, term.ErrNoView) {
		t.Fatalf("err %v", err)
	}
}
