package oversire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// The clip limits of a tool turn, in characters.
const (
	maxToolInput   = 200
	maxToolSummary = 2000
)

// turnLog appends the chat turns of one session to <id>.turns.jsonl and feeds the live viewers.
// A tool turn that changes state goes again with the same seq. The last line of a seq wins.
type turnLog struct {
	mu     sync.Mutex
	file   *os.File
	seq    int
	tools  map[string]wire.SessionTurn // the running tools by tool id
	feeds  map[*feed]struct{}
	closed bool
}

// openTurns opens the log for append. The seq goes on from the last line of the file.
func openTurns(path string) (*turnLog, error) {
	old, err := readTurns(path, 0)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := openLog(path)
	if err != nil {
		return nil, err
	}
	l := &turnLog{file: f, tools: map[string]wire.SessionTurn{}, feeds: map[*feed]struct{}{}}
	if len(old) > 0 {
		l.seq = old[len(old)-1].Seq
	}
	return l, nil
}

// add gives t the next seq, writes it and gives it to the viewers.
func (l *turnLog) add(t wire.SessionTurn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	t.Seq = l.seq
	l.write(t)
}

// tool adds a turn when a tool starts. The end of the tool changes that turn.
func (l *turnLog) tool(ev *agentwire.ToolEvent, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, running := l.tools[ev.ID]
	if !running {
		l.seq++
		t = wire.SessionTurn{Seq: l.seq, Role: wire.RoleTool, At: at, Tool: &wire.TurnTool{Name: ev.Name}}
	}
	tool := *t.Tool
	if ev.Input != "" {
		tool.Input = clip(ev.Input, maxToolInput)
	}
	switch ev.Status {
	case "completed":
		tool.Status = wire.ToolDone
	case "failed":
		tool.Status = wire.ToolFailed
	default:
		tool.Status = wire.ToolRunning
	}
	if ev.Output != "" {
		tool.Summary = clip(ev.Output, maxToolSummary)
	}
	t.Tool = &tool
	if tool.Status == wire.ToolRunning && ev.ID != "" {
		l.tools[ev.ID] = t
	} else {
		delete(l.tools, ev.ID)
	}
	l.write(t)
}

// write appends one line. Call it with l.mu held.
func (l *turnLog) write(t wire.SessionTurn) {
	if l.closed {
		return
	}
	if data, err := json.Marshal(t); err == nil {
		_, _ = l.file.Write(append(data, '\n'))
	}
	for f := range l.feeds {
		f.push(t)
	}
}

// follow gives the turns after the seq after, then the live turns until the session ends or ctx ends.
func (l *turnLog) follow(ctx context.Context, path string, after int) ([]wire.SessionTurn, <-chan wire.SessionTurn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	history, err := readTurns(path, after)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	f := newFeed()
	if l.closed {
		f.end()
	} else {
		l.feeds[f] = struct{}{}
	}
	go f.run(ctx, func() {
		l.mu.Lock()
		delete(l.feeds, f)
		l.mu.Unlock()
	})
	return history, f.out, nil
}

// close ends the live feeds after their last turns.
func (l *turnLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	_ = l.file.Close()
	for f := range l.feeds {
		f.end()
	}
}

// readTurns reads the turns after the seq after, in seq order.
func readTurns(path string, after int) ([]wire.SessionTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []wire.SessionTurn
	at := map[int]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var t wire.SessionTurn
		if json.Unmarshal(sc.Bytes(), &t) != nil || t.Seq <= after {
			continue
		}
		if i, ok := at[t.Seq]; ok {
			out[i] = t
			continue
		}
		at[t.Seq] = len(out)
		out = append(out, t)
	}
	return out, sc.Err()
}

// feed gives the turns to one viewer. Its queue has no limit, so the session never waits for a viewer.
type feed struct {
	mu    sync.Mutex
	queue []wire.SessionTurn
	done  bool
	wake  chan struct{}
	out   chan wire.SessionTurn
}

func newFeed() *feed {
	return &feed{wake: make(chan struct{}, 1), out: make(chan wire.SessionTurn)}
}

func (f *feed) push(t wire.SessionTurn) {
	f.mu.Lock()
	f.queue = append(f.queue, t)
	f.mu.Unlock()
	f.signal()
}

func (f *feed) end() {
	f.mu.Lock()
	f.done = true
	f.mu.Unlock()
	f.signal()
}

func (f *feed) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *feed) run(ctx context.Context, leave func()) {
	defer close(f.out)
	defer leave()
	for {
		f.mu.Lock()
		queue, done := f.queue, f.done
		f.queue = nil
		f.mu.Unlock()
		for _, t := range queue {
			select {
			case f.out <- t:
			case <-ctx.Done():
				return
			}
		}
		if done && len(queue) == 0 {
			return
		}
		if len(queue) > 0 {
			continue
		}
		select {
		case <-f.wake:
		case <-ctx.Done():
			return
		}
	}
}

// Terminal: a session of the oversire runtime has no terminal.
func (rt *Runtime) Terminal(context.Context, wire.DaemonSession) (term.Source, error) {
	return nil, term.ErrNoView
}

// Chat opens the chat view of a session. A session from before a restart gives its history only.
func (rt *Runtime) Chat(_ context.Context, s wire.DaemonSession) (term.TurnSource, error) {
	if !sessionID.MatchString(s.ID) {
		return nil, fmt.Errorf("The oversire runtime has no session %s.", s.ID)
	}
	return chatView{rt: rt, ref: wire.SessionRef{ID: s.ID}}, nil
}

// StreamState: agent-wire gives each turn live.
func (rt *Runtime) StreamState() wire.RuntimeStream {
	return wire.RuntimeStream{State: wire.StreamLive}
}

func (rt *Runtime) turnsPath(id string) string {
	return filepath.Join(rt.opts.DataDir, "sessions", id+".turns.jsonl")
}

type chatView struct {
	rt  *Runtime
	ref wire.SessionRef
}

func (c chatView) Turns(ctx context.Context, after int) ([]wire.SessionTurn, <-chan wire.SessionTurn, error) {
	path := c.rt.turnsPath(c.ref.ID)
	if s, err := c.rt.session(c.ref); err == nil {
		return s.turns.follow(ctx, path, after)
	}
	turns, err := readTurns(path, after)
	if errors.Is(err, fs.ErrNotExist) {
		turns, err = c.digestTurns(ctx, after)
	}
	if err != nil {
		return nil, nil, err
	}
	ended := make(chan wire.SessionTurn)
	close(ended)
	return turns, ended, nil
}

// digestTurns builds the turns of an old session with no turn log from the transcript of the harness.
func (c chatView) digestTurns(ctx context.Context, after int) ([]wire.SessionTurn, error) {
	d, err := c.rt.History(ctx, c.ref, wire.NewHistoryBudget(100, wire.DefaultFull, wire.DefaultMaxChars))
	if err != nil {
		return nil, err
	}
	var all []wire.SessionTurn
	add := func(role wire.TurnRole, text string) {
		if text != "" {
			all = append(all, wire.SessionTurn{Seq: len(all) + 1, Role: role, Text: text})
		}
	}
	for _, t := range d.Earlier {
		add(wire.RoleUser, t.Prompt)
		add(wire.RoleAssistant, t.Closing)
	}
	for _, t := range d.Last {
		add(wire.RoleUser, t.Prompt)
		add(wire.RoleAssistant, t.Text)
	}
	return all[min(after, len(all)):], nil
}

// Send goes the path of sessionSend: the text joins the turn or starts a new one.
func (c chatView) Send(ctx context.Context, text string) error {
	return c.rt.Send(ctx, wire.SendPayload{Session: c.ref, Text: text})
}
