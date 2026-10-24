// Package sessions merges the partial updates of each source (hooks, Orca, agent-wire) into one DaemonSession for each id.
// A change of state, reason or question sends one ProviderEvent to the log.
package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

type Source string

const (
	SourceHook Source = wire.SessionSourceHook
	SourceOrca Source = wire.SessionSourceOrca
	SourceWire Source = wire.SessionSourceWire
)

const (
	// Coalesce is the window in which more changes of one session give only one event.
	Coalesce = 300 * time.Millisecond
	// OrcaLag is how much newer an Orca patch must be than the last hook patch to set the state.
	OrcaLag     = 5 * time.Second
	DoneTTL     = 24 * time.Hour
	CLIEndTTL   = 10 * time.Minute
	persistGap  = 2 * time.Second
	runInterval = 100 * time.Millisecond
)

// Patch is a partial update from one source. A nil field does not change.
type Patch struct {
	ID                                                                       string
	Source                                                                   Source
	At                                                                       time.Time
	Runtime, Harness, Workspace, Terminal, HarnessSession, Cwd, Repo, Branch *string
	State                                                                    *wire.SessionPhase
	Reason                                                                   *wire.SessionReason // a pointer to "" clears it
	// Question is the open question id. A pointer to "" clears it.
	Question        *string
	Tool, ToolInput *string // from Orca toolName and toolInput, or the hook
	Prompt          *string
	LastMessage     *string
	End             bool // the source says the session is gone
	// Since is when the source saw the state start. Zero means At.
	Since time.Time
}

type Appender interface{ Append(e wire.ProviderEvent) }

type entry struct {
	s               wire.DaemonSession
	tool, toolInput string
	lastHook        time.Time
	lastOrca        time.Time
	ended           time.Time
	// sent is the session as the last event told it. A pending change waits until due.
	sent    wire.DaemonSession
	pending bool
	due     time.Time
}

type Book struct {
	machine string
	mu      sync.Mutex
	byID    map[string]*entry
	log     Appender
	clock   func() time.Time
	path    string
	dirty   bool
	saved   time.Time
	watch   watchers
}

// NewBook reads path when it exists. An empty path keeps the sessions in memory only.
func NewBook(machine, path string, log Appender, clock func() time.Time) *Book {
	if clock == nil {
		clock = time.Now
	}
	b := &Book{machine: machine, byID: map[string]*entry{}, log: log, clock: clock, path: path}
	b.load()
	return b
}

func (b *Book) Apply(p Patch) {
	if p.ID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.At.IsZero() {
		p.At = b.clock()
	}
	e, ok := b.byID[p.ID]
	if !ok {
		if p.End {
			return
		}
		e = &entry{s: wire.DaemonSession{ID: p.ID, Machine: b.machine, State: wire.PhaseStarting, StateSince: p.At, UpdatedAt: p.At, Sources: []string{}}}
		b.byID[p.ID] = e
	}
	before := copySession(e.s)
	e.merge(p)
	if p.End {
		if e.s.Runtime == wire.RuntimeOrca {
			b.remove(e, p.At)
			return
		}
		e.ended = p.At
	}
	if e.s.State != before.State || e.s.Reason != before.Reason {
		e.s.StateSince = p.At
		if !p.Since.IsZero() && p.Since.Before(p.At) {
			e.s.StateSince = p.Since
		}
		b.watch.notify(p.ID)
	}
	if !reflect.DeepEqual(e.s, before) {
		e.s.UpdatedAt = p.At
		b.dirty = true
	}
	// The first change opens the window. The state at its end goes out.
	if e.differs() && !e.pending {
		e.pending, e.due = true, p.At.Add(Coalesce)
	}
	b.flush(p.At)
}

// merge applies the fields of p by the precedence rules.
func (e *entry) merge(p Patch) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&e.s.Runtime, p.Runtime)
	set(&e.s.Harness, p.Harness)
	set(&e.s.Workspace, p.Workspace)
	set(&e.s.Terminal, p.Terminal)
	set(&e.s.HarnessSession, p.HarnessSession)
	set(&e.s.Cwd, p.Cwd)
	set(&e.s.Repo, p.Repo)
	set(&e.s.Branch, p.Branch)
	set(&e.s.Question, p.Question)
	set(&e.tool, p.Tool)
	set(&e.toolInput, p.ToolInput)
	if p.Prompt != nil {
		e.s.Prompt = clip(*p.Prompt, wire.MaxSessionPrompt)
	}
	if p.LastMessage != nil {
		e.s.LastMessage = clip(*p.LastMessage, wire.MaxSessionLastMessage)
	}
	if !containsStr(e.s.Sources, string(p.Source)) {
		e.s.Sources = append(e.s.Sources, string(p.Source))
		sort.Strings(e.s.Sources)
	}

	if e.ownsState(p) {
		if p.State != nil {
			e.s.State = *p.State
			if p.Reason == nil {
				e.s.Reason = ""
			}
		}
		if p.Reason != nil && (*p.Reason != wire.ReasonMonitoring || p.Source == SourceOrca) {
			e.s.Reason = *p.Reason
		}
	}
	switch p.Source {
	case SourceHook:
		e.lastHook = p.At
	case SourceOrca:
		e.lastOrca = p.At
	}
	// Needs input: a done turn that ends with a question.
	if e.s.State == wire.PhaseDone && e.s.Reason == "" && TurnQuestion(e.s.LastMessage) != "" {
		e.s.Reason = wire.ReasonQuestion
	}
}

// ownsState tells whether p may set the state and the reason.
// Wire is the truth for its own sessions. For Orca sessions the newer of hook and Orca wins, but Orca lags, so it needs OrcaLag.
func (e *entry) ownsState(p Patch) bool {
	if e.s.Runtime == wire.RuntimeOversire {
		return p.Source == SourceWire
	}
	switch p.Source {
	case SourceHook:
		return !p.At.Before(e.lastOrca)
	case SourceOrca:
		return e.lastHook.IsZero() || !p.At.Before(e.lastHook.Add(OrcaLag))
	}
	return true
}

func (e *entry) differs() bool {
	return e.s.State != e.sent.State || e.s.Reason != e.sent.Reason || e.s.Question != e.sent.Question
}

// Flush sends the changes whose coalesce window is over. Run calls it often.
func (b *Book) Flush(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flush(now)
}

func (b *Book) flush(now time.Time) {
	for _, e := range b.byID {
		if !e.differs() {
			e.pending = false
			continue
		}
		if !e.pending || now.Before(e.due) {
			continue
		}
		action := string(e.s.State)
		if e.s.State == e.sent.State && e.s.Reason == e.sent.Reason {
			action = "question"
		}
		b.send(e.s, action, now)
		e.sent, e.pending = e.s, false
	}
}

func (b *Book) remove(e *entry, now time.Time) {
	delete(b.byID, e.s.ID)
	b.dirty = true
	b.watch.notify(e.s.ID)
	b.send(e.s, "removed", now)
}

func (b *Book) send(s wire.DaemonSession, action string, now time.Time) {
	if b.log == nil {
		return
	}
	id := fmt.Sprintf("daemon-%s-%s-%s-%d", b.machine, s.ID, s.State, s.StateSince.UnixMilli())
	switch action {
	case "question":
		id += "-q-" + s.Question
	case "removed":
		id += "-removed"
	}
	acct := wire.AccountID{Provider: s.Runtime, ID: b.machine}
	ws := s.Workspace
	if ws == "" {
		ws = s.Cwd
	}
	title := s.Repo
	if title == "" && s.Cwd != "" {
		title = filepath.Base(s.Cwd)
	}
	b.log.Append(wire.ProviderEvent{
		ID: id, Schema: 1, Provider: s.Runtime, Account: acct, Kind: "session", Action: action,
		Subject: wire.SessionSubject(wire.SessionRef{Workspace: wire.WorkspaceRef{Account: acct, ID: ws}, ID: s.ID}),
		At:      now,
		Summary: &wire.EventSummary{Title: title, Status: string(s.State), Actor: s.Harness},
	})
}

// Prune removes expired sessions: done or failed for DoneTTL, and ended cli sessions after CLIEndTTL.
// A done session that Orca still reports stays: its DoneTTL starts at the last Orca poll.
func (b *Book) Prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.byID {
		done := e.s.State == wire.PhaseDone || e.s.State == wire.PhaseFailed
		since := e.s.StateSince
		if e.lastOrca.After(since) {
			since = e.lastOrca
		}
		if done && !now.Before(since.Add(DoneTTL)) || !e.ended.IsZero() && !now.Before(e.ended.Add(CLIEndTTL)) {
			b.remove(e, now)
		}
	}
}

// All is sorted by updatedAt, newest first.
func (b *Book) All() []wire.DaemonSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]wire.DaemonSession, 0, len(b.byID))
	for _, e := range b.byID {
		out = append(out, copySession(e.s))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (b *Book) Get(id string) (wire.DaemonSession, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.byID[id]
	if !ok {
		return wire.DaemonSession{}, false
	}
	return copySession(e.s), true
}

// Tool gives the last tool and its input of a session.
func (b *Book) Tool(id string) (tool, input string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.byID[id]; ok {
		return e.tool, e.toolInput
	}
	return "", ""
}

// Run flushes coalesced changes, prunes, and writes the sessions file at most each 2 s.
func (b *Book) Run(ctx context.Context) error {
	t := time.NewTicker(runInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			b.Save(true)
			return nil
		case <-t.C:
			now := b.clock()
			b.Flush(now)
			b.Prune(now)
			b.Save(false)
		}
	}
}

// Save writes the file when the sessions changed. Without force it waits 2 s after the last write.
func (b *Book) Save(force bool) error {
	b.mu.Lock()
	now := b.clock()
	if b.path == "" || !b.dirty || !force && now.Sub(b.saved) < persistGap {
		b.mu.Unlock()
		return nil
	}
	list := make([]wire.DaemonSession, 0, len(b.byID))
	for _, e := range b.byID {
		list = append(list, copySession(e.s))
	}
	b.dirty, b.saved = false, now
	b.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	data, err := json.Marshal(wire.DaemonSessionsPage{Sessions: list})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// load reads the sessions file. The next hook or Orca poll corrects a stale session.
func (b *Book) load() {
	if b.path == "" {
		return
	}
	data, err := os.ReadFile(b.path)
	if err != nil {
		return
	}
	var page wire.DaemonSessionsPage
	if err := json.Unmarshal(data, &page); err != nil {
		return
	}
	for _, s := range page.Sessions {
		if s.ID == "" {
			continue
		}
		s.Machine = b.machine
		e := &entry{s: s, sent: s}
		// Give the first Orca poll time to report the session again.
		if containsStr(s.Sources, string(SourceOrca)) {
			e.lastOrca = b.clock()
		}
		b.byID[s.ID] = e
	}
}

func copySession(s wire.DaemonSession) wire.DaemonSession {
	s.Sources = append([]string{}, s.Sources...)
	return s
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
