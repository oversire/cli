package questions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/hooks"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// Screen reads and types into the terminal of a session.
type Screen interface {
	Read(ctx context.Context, terminal string, lines int) ([]string, error)
	Send(ctx context.Context, terminal, text string, enter bool) error
}

// Transcripts reads the agent transcripts on this machine.
type Transcripts interface {
	Tail(path string, bytes int) ([]byte, error)
	// Find gives the files to try in order: the newest file of the folder, then the hits of orca search.
	Find(ctx context.Context, cwd, words string) ([]string, error)
}

// ErrBadAnswer: the answer does not fit the question.
var ErrBadAnswer = errors.New("the answer does not fit the question")

// Codes of an AnswerError result. CodeFormOpen and wire.CodeFormChanged have the same meaning as in the app.
const (
	CodeFormOpen      = "form_open"
	CodeNotAnswerable = "not_answerable"
)

const (
	// ScreenLines is the number of terminal rows that one read gives.
	ScreenLines = 40
	// PollInterval is the check of the sessions that wait: a form changes page and a prompt can close without a hook.
	PollInterval = 10 * time.Second
	keepClosed   = time.Hour
)

// Item ids of the questions that are not forms.
const (
	ItemPermission = "permission"
	ItemPlan       = "plan"
	ItemReply      = "reply"
	ItemWait       = "wait"
)

// WaitingText is the text of the fallback question of a session that waits with no readable pause.
const WaitingText = "The session waits for you. OverSire cannot read the prompt."

// record is a question and the session it was made from.
type record struct {
	q wire.DaemonQuestion
	s wire.DaemonSession
	// key keeps the id stable: the prompt key on the screen, or the text of a turn question.
	key    string
	closed time.Time
}

// keyError is an answer that stopped. It goes out as an AnswerError result.
type keyError struct{ code, message string }

func (e *keyError) Error() string { return e.message }

// Channel finds the pauses of the sessions in the book, from the hooks, the screen and the transcript, and answers them.
type Channel struct {
	book   *sessions.Book
	screen Screen
	tr     Transcripts
	log    sessions.Appender
	clock  func() time.Time

	// Tries and Wait: the reads of the screen for each key until the step holds.
	Tries int
	Wait  time.Duration
	Poll  time.Duration

	mu     sync.Mutex
	open   map[string]*record // by session id
	closed map[string]*record // by question id
	sent   map[string]time.Time
	given  map[string]wire.QuestionAnswer
	hooked map[string]string // the transcript_path of each session
	fitted map[string]string // the transcript that fit last, for each session
	locks  map[string]*sync.Mutex
	kicks  chan string
}

// NewChannel needs a book. screen and tr can be nil: then no question is answerable and no transcript is read.
func NewChannel(book *sessions.Book, screen Screen, tr Transcripts, log sessions.Appender, clock func() time.Time) *Channel {
	if clock == nil {
		clock = time.Now
	}
	return &Channel{book: book, screen: screen, tr: tr, log: log, clock: clock, Tries: 10, Wait: 200 * time.Millisecond, Poll: PollInterval,
		open: map[string]*record{}, closed: map[string]*record{}, sent: map[string]time.Time{}, given: map[string]wire.QuestionAnswer{},
		hooked: map[string]string{}, fitted: map[string]string{}, locks: map[string]*sync.Mutex{}, kicks: make(chan string, 64)}
}

// Open gives the open questions, oldest first.
func (c *Channel) Open() []wire.DaemonQuestion {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]wire.DaemonQuestion, 0, len(c.open))
	for _, r := range c.open {
		out = append(out, r.q)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].AskedAt.Equal(out[j].AskedAt) {
			return out[i].AskedAt.Before(out[j].AskedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// hookPayload holds the fields of a Claude hook payload that the channel reads.
type hookPayload struct {
	TranscriptPath       string          `json:"transcript_path"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// OnHook takes the exact pauses from the hooks. It does no terminal I/O: a check for the screen goes to Run.
func (c *Channel) OnHook(ev hooks.Event) {
	var p hookPayload
	_ = json.Unmarshal(ev.Payload, &p)
	at := ev.At
	if at.IsZero() {
		at = c.clock()
	}
	if p.TranscriptPath != "" && ev.Session != "" {
		c.mu.Lock()
		c.hooked[ev.Session] = p.TranscriptPath
		c.mu.Unlock()
	}
	s, ok := c.book.Get(ev.Session)
	if !ok {
		return
	}
	switch {
	case ev.Name == "PreToolUse" && p.ToolName == "AskUserQuestion":
		var in askInput
		_ = json.Unmarshal(p.ToolInput, &in)
		if items := in.items(); len(items) > 0 {
			c.set(s, c.question(s, c.formID(s, p.ToolUseID, items[0].Text), wire.ViaForm, wire.SourceHook, at, items...), "")
			return
		}
	case ev.Name == "PreToolUse" && p.ToolName == "ExitPlanMode":
		var in struct {
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(p.ToolInput, &in)
		text := strings.TrimSpace(in.Plan)
		if text == "" {
			text = "Approve the plan"
		}
		item := approve(ItemPlan, "Plan", text)
		item.AllowsOther = true
		c.set(s, c.question(s, c.formID(s, p.ToolUseID, text), wire.ViaPermission, wire.SourceHook, at, item), "")
	case ev.Name == "PostToolUse" && (p.ToolName == "AskUserQuestion" || p.ToolName == "ExitPlanMode"):
		c.closeHook(s, p)
		return
	case ev.Name == "PermissionRequest":
		input := toolInput(p.ToolInput)
		id := ""
		if p.ToolUseID != "" {
			id = "form:" + p.ToolUseID
		} else if p.TranscriptPath != "" && c.tr != nil {
			if data, err := c.tr.Tail(p.TranscriptPath, TranscriptTail); err == nil {
				if t := OpenTool(p.ToolName, input, data); t != "" {
					id = "form:" + t
				}
			}
		}
		if id == "" {
			id = "form:" + s.ID + ":" + fnv(p.ToolName+"\n"+input)
		}
		c.set(s, c.question(s, id, wire.ViaPermission, wire.SourceHook, at, approve(ItemPermission, p.ToolName, inputText(p.ToolName, input))), "")
	case ev.Name == "Stop":
		msg := p.LastAssistantMessage
		if msg == "" {
			msg = s.LastMessage
		}
		if text, ok := TurnQuestion(msg); ok {
			c.set(s, c.turn(s, text, msg), text)
			return
		}
	}
	c.kick(ev.Session)
}

// closeHook closes a form or a plan approval after its PostToolUse, and keeps the given answer.
func (c *Channel) closeHook(s wire.DaemonSession, p hookPayload) {
	c.mu.Lock()
	cur := c.open[s.ID]
	id := ""
	if p.ToolUseID != "" {
		id = "form:" + p.ToolUseID
	} else if cur != nil && cur.q.Source == wire.SourceHook {
		id = cur.q.ID
	}
	var given *wire.QuestionAnswer
	if p.ToolName == "ExitPlanMode" {
		given = &wire.QuestionAnswer{ID: id, Items: []wire.QuestionAnswerItem{{ID: ItemPlan, Choices: []string{wire.OptionAllow}}}}
	} else {
		var in askInput
		_ = json.Unmarshal(p.ToolInput, &in)
		var r askResult
		_ = json.Unmarshal(p.ToolResponse, &r)
		if r.Answers == nil {
			r.Answers = map[string]string{}
		}
		given = Ask{ID: id, Items: in.items(), Answers: r.Answers, Notes: r.notes()}.Answer(id)
	}
	if id != "" && given != nil {
		c.given[id] = *given
	}
	c.mu.Unlock()
	if cur != nil && cur.q.ID == id {
		c.set(s, nil, "")
	}
	c.kick(s.ID)
}

// Run checks a session on each change of the book and on each hook, and each Poll the sessions that wait.
func (c *Channel) Run(ctx context.Context) error {
	changes, stop := c.book.Watch()
	defer stop()
	t := time.NewTicker(c.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case id := <-changes:
			c.check(ctx, id)
		case id := <-c.kicks:
			c.check(ctx, id)
		case <-t.C:
			for _, id := range c.due() {
				c.check(ctx, id)
			}
		}
	}
}

// due gives the sessions that wait or have an open question, and drops old closed questions.
func (c *Channel) due() []string {
	ids := map[string]bool{}
	for _, s := range c.book.All() {
		if s.State == wire.PhaseWaiting {
			ids[s.ID] = true
		}
	}
	now := c.clock()
	c.mu.Lock()
	for id := range c.open {
		ids[id] = true
	}
	for id, r := range c.closed {
		if now.Sub(r.closed) > keepClosed {
			delete(c.closed, id)
			delete(c.given, id)
		}
	}
	for id, at := range c.sent {
		if now.Sub(at) > keepClosed {
			delete(c.sent, id)
		}
	}
	c.mu.Unlock()
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Check finds the pause of one session again. Run calls it.
func (c *Channel) Check(ctx context.Context, id string) { c.check(ctx, id) }

func (c *Channel) check(ctx context.Context, id string) {
	l := c.lock(id)
	// An answer holds the session. It checks the session again when it ends.
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	s, ok := c.book.Get(id)
	if !ok {
		c.set(wire.DaemonSession{ID: id}, nil, "")
		return
	}
	q, key, err := c.find(ctx, s)
	if err != nil {
		return
	}
	c.set(s, q, key)
}

// find gives the pause of the session now, or nil.
func (c *Channel) find(ctx context.Context, s wire.DaemonSession) (*wire.DaemonQuestion, string, error) {
	cur := c.current(s.ID)
	switch s.State {
	case wire.PhaseDone:
		text, ok := TurnQuestion(s.LastMessage)
		if !ok {
			return nil, "", nil
		}
		if cur != nil && cur.q.Via == wire.ViaTurn && cur.key == text {
			return &cur.q, cur.key, nil
		}
		return c.turn(s, text, s.LastMessage), text, nil
	case wire.PhaseWaiting:
	default:
		// Never a read for a session that works.
		return nil, "", nil
	}
	if cur != nil && cur.q.Source == wire.SourceHook && cur.q.Via == wire.ViaForm {
		return &cur.q, "", nil
	}
	if !c.canType(s) {
		if cur != nil && cur.q.Source == wire.SourceHook {
			return &cur.q, cur.key, nil
		}
		return c.waiting(s), "", nil
	}
	screen, err := c.screen.Read(ctx, s.Terminal, ScreenLines)
	if err != nil {
		return nil, "", err
	}
	tool, input := c.book.Tool(s.ID)
	if cur != nil && cur.q.Source == wire.SourceHook && cur.q.Via == wire.ViaPermission {
		if p, ok := ParsePrompt(screen); ok {
			hook := cur.q.Items[0]
			item := p.Item(hook.ID, tool, input)
			item.Header, item.Text = hook.Header, hook.Text
			q := cur.q
			q.Items = []wire.DaemonQuestionItem{item}
			return &q, p.Key(), nil
		}
		// The prompt did not show yet. A prompt that showed and left was answered in the terminal.
		if cur.key == "" {
			return &cur.q, "", nil
		}
	}
	if tool != "AskUserQuestion" {
		if p, ok := ParsePrompt(screen); ok {
			if cur != nil && cur.q.Via == wire.ViaPermission && cur.q.Source == wire.SourceScreen && cur.key == p.Key() {
				return &cur.q, cur.key, nil
			}
			itemID := ItemPermission
			if p.Kind == PromptPlan {
				itemID = ItemPlan
			}
			id := "form:" + s.ID + ":" + fnv(p.Key())
			if tool != "" {
				words := input
				if words == "" {
					words = p.Key()
				}
				if t := c.openTool(ctx, s, tool, input, words); t != "" {
					id = "form:" + t
				}
			}
			return c.question(s, id, wire.ViaPermission, wire.SourceScreen, s.StateSince, p.Item(itemID, tool, input)), p.Key(), nil
		}
	}
	if tool == "" || tool == "AskUserQuestion" {
		if f, ok := ParseForm(screen); ok {
			words := f.Key()
			if len(f.Items) > 0 {
				words = f.Items[0].Text
			}
			var ask Ask
			found := false
			c.transcript(ctx, s, words, func(d []byte) bool {
				a, ok := LastAsk(d)
				ask, found = a, ok && a.Open() && a.Matches(f)
				return found
			})
			if found {
				return c.question(s, "form:"+ask.ID, wire.ViaForm, wire.SourceTranscript, s.StateSince, ask.Items...), "", nil
			}
			if len(f.Items) > 0 {
				items := f.Items
				if f.Review {
					items = []wire.DaemonQuestionItem{f.ReviewItem()}
				}
				return c.question(s, "form:"+s.ID+":"+fnv(f.Page()), wire.ViaForm, wire.SourceScreen, s.StateSince, items...), "", nil
			}
		}
	}
	return c.waiting(s), "", nil
}

func (c *Channel) turn(s wire.DaemonSession, text, message string) *wire.DaemonQuestion {
	item := wire.DaemonQuestionItem{ID: ItemReply, Text: text, Kind: wire.QuestionFree, Options: []wire.QuestionOption{}}
	if message != text {
		item.Why = message
	}
	return c.question(s, fmt.Sprintf("turn:%s:%d", s.ID, s.StateSince.UnixMilli()), wire.ViaTurn, wire.SourceTranscript, s.StateSince, item)
}

func (c *Channel) waiting(s wire.DaemonSession) *wire.DaemonQuestion {
	item := wire.DaemonQuestionItem{ID: ItemWait, Text: WaitingText, Kind: wire.QuestionFree, Options: []wire.QuestionOption{}}
	return c.question(s, fmt.Sprintf("wait:%s:%d", s.ID, s.StateSince.UnixMilli()), wire.ViaWaiting, wire.SourceScreen, s.StateSince, item)
}

// question makes a question of the session. A session that the channel cannot type into tells where to answer.
func (c *Channel) question(s wire.DaemonSession, id string, via wire.QuestionVia, src wire.QuestionSource, at time.Time,
	items ...wire.DaemonQuestionItem) *wire.DaemonQuestion {
	q := &wire.DaemonQuestion{ID: id, Session: s.ID, Terminal: s.Terminal, Harness: s.Harness, Via: via,
		Items: append([]wire.DaemonQuestionItem{}, items...), Answerable: via != wire.ViaWaiting && c.canType(s), Source: src, AskedAt: at.UTC()}
	if !q.Answerable && via != wire.ViaWaiting {
		for i := range q.Items {
			if q.Items[i].Why == "" {
				q.Items[i].Why = "Answer in the terminal on " + s.Machine + "."
			}
		}
	}
	return q
}

// formID is the id of a hook form: the tool call, else a hash of the session and the text.
func (c *Channel) formID(s wire.DaemonSession, toolUseID, text string) string {
	if toolUseID != "" {
		return "form:" + toolUseID
	}
	return "form:" + s.ID + ":" + fnv(text)
}

// canType tells whether the channel can read and type into the terminal of the session: an Orca session on this machine.
func (c *Channel) canType(s wire.DaemonSession) bool {
	return c.screen != nil && s.Runtime == wire.RuntimeOrca && s.Terminal != ""
}

func (c *Channel) current(session string) *record {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.open[session]; r != nil {
		cp := *r
		return &cp
	}
	return nil
}

// set makes q the open question of the session, or closes it when q is nil. Each change appends one event.
func (c *Channel) set(s wire.DaemonSession, q *wire.DaemonQuestion, key string) {
	now := c.clock()
	c.mu.Lock()
	cur := c.open[s.ID]
	if q != nil {
		if _, done := c.sent[q.ID]; done {
			q = nil
		}
	}
	if q == nil {
		if cur == nil {
			c.mu.Unlock()
			return
		}
		delete(c.open, s.ID)
		cur.closed = now
		c.closed[cur.q.ID] = cur
		c.mu.Unlock()
		c.event(cur.s, "", now)
		return
	}
	if cur != nil && reflect.DeepEqual(cur.q, *q) {
		cur.key = key
		c.mu.Unlock()
		return
	}
	if cur != nil && cur.q.ID != q.ID {
		cur.closed = now
		c.closed[cur.q.ID] = cur
	}
	c.open[s.ID] = &record{q: *q, s: s, key: key}
	delete(c.closed, q.ID)
	c.mu.Unlock()
	c.event(s, q.ID, now)
}

// event tells the clients that the open set changed. They read GET /v1/questions.
func (c *Channel) event(s wire.DaemonSession, question string, now time.Time) {
	if c.log == nil {
		return
	}
	id := fmt.Sprintf("daemon-%s-%s-question-%d", s.Machine, s.ID, now.UnixNano())
	if question != "" {
		id = fmt.Sprintf("daemon-%s-%s-question-%s-%d", s.Machine, s.ID, question, now.UnixNano())
	}
	runtime := s.Runtime
	if runtime == "" {
		runtime = wire.RuntimeCLI
	}
	acct := wire.AccountID{Provider: runtime, ID: s.Machine}
	ws := s.Workspace
	if ws == "" {
		ws = s.Cwd
	}
	title := s.Repo
	if title == "" && s.Cwd != "" {
		title = filepath.Base(s.Cwd)
	}
	c.log.Append(wire.ProviderEvent{
		ID: id, Schema: 1, Provider: runtime, Account: acct, Kind: "session", Action: "question",
		Subject: wire.SessionSubject(wire.SessionRef{Workspace: wire.WorkspaceRef{Account: acct, ID: ws}, ID: s.ID}),
		At:      now,
		Summary: &wire.EventSummary{Title: title, Status: string(s.State), Actor: s.Harness},
	})
}

func (c *Channel) kick(session string) {
	select {
	case c.kicks <- session:
	default:
	}
}

func (c *Channel) lock(session string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.locks[session]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[session] = l
	}
	return l
}

// transcript gives the tail of the transcript of the session that fits. The order: the transcript_path of the hooks,
// the file of the last fit, then the files that Transcripts.Find gives.
func (c *Channel) transcript(ctx context.Context, s wire.DaemonSession, words string, fits func([]byte) bool) []byte {
	if c.tr == nil {
		return nil
	}
	c.mu.Lock()
	first := []string{c.hooked[s.ID], c.fitted[s.ID]}
	c.mu.Unlock()
	tried := map[string]bool{"": true}
	try := func(path string) []byte {
		if tried[path] {
			return nil
		}
		tried[path] = true
		data, err := c.tr.Tail(path, TranscriptTail)
		if err != nil || !fits(data) {
			return nil
		}
		c.mu.Lock()
		c.fitted[s.ID] = path
		c.mu.Unlock()
		return data
	}
	for _, p := range first {
		if d := try(p); d != nil {
			return d
		}
	}
	if s.Cwd == "" {
		return nil
	}
	found, _ := c.tr.Find(ctx, s.Cwd, words)
	for _, p := range found {
		if d := try(p); d != nil {
			return d
		}
	}
	return nil
}

// openTool gives the id of the open call of tool in the transcript of the session.
func (c *Channel) openTool(ctx context.Context, s wire.DaemonSession, tool, input, words string) string {
	id := ""
	c.transcript(ctx, s, words, func(d []byte) bool {
		id = OpenTool(tool, input, d)
		return id != ""
	})
	return id
}

// Given gives the answer that the user gave in the terminal, from PostToolUse or the transcript, or nil.
func (c *Channel) Given(ctx context.Context, id string) (*wire.QuestionAnswer, error) {
	c.mu.Lock()
	if g, ok := c.given[id]; ok {
		c.mu.Unlock()
		return &g, nil
	}
	var rec *record
	if r := c.closed[id]; r != nil {
		rec = r
	}
	for _, r := range c.open {
		if r.q.ID == id {
			rec = r
		}
	}
	var r record
	if rec != nil {
		r = *rec
	}
	c.mu.Unlock()
	tool := strings.TrimPrefix(id, "form:")
	if rec == nil || r.q.Via != wire.ViaForm || !strings.HasPrefix(id, "form:") || strings.Contains(tool, ":") {
		return nil, nil
	}
	words := ""
	if len(r.q.Items) > 0 {
		words = r.q.Items[0].Text
	}
	var ask Ask
	c.transcript(ctx, r.s, words, func(d []byte) bool {
		a, ok := LastAsk(d)
		if ok && a.ID == tool {
			ask = a
			return true
		}
		return false
	})
	return ask.Answer(id), nil
}

// Answer types the answer into the terminal. One answer at a time for each session.
// The result is sent, alreadyAnswered or gone. A stopped answer gives an AnswerError result with form_changed or form_open.
func (c *Channel) Answer(ctx context.Context, id string, a wire.QuestionAnswer) (wire.DaemonAnswerResult, error) {
	rec, res := c.lookup(id)
	if rec == nil {
		return res, nil
	}
	l := c.lock(rec.s.ID)
	l.Lock()
	defer func() {
		l.Unlock()
		c.kick(rec.s.ID)
	}()
	// The question can close while the lock waits.
	if rec, res = c.lookup(id); rec == nil {
		return res, nil
	}
	q := rec.q
	if !q.Answerable {
		return wire.DaemonAnswerResult{Result: wire.AnswerError, Code: CodeNotAnswerable, Message: "Answer this question in the terminal."}, nil
	}
	if err := fits(q, a); err != nil {
		return wire.DaemonAnswerResult{}, err
	}
	var err error
	switch q.Via {
	case wire.ViaForm:
		res, err = c.answerForm(ctx, *rec, a)
	case wire.ViaPermission:
		res, err = c.answerPrompt(ctx, *rec, a)
	case wire.ViaTurn:
		res, err = c.answerTurn(ctx, *rec, a)
	default:
		return wire.DaemonAnswerResult{Result: wire.AnswerError, Code: CodeNotAnswerable, Message: "Answer this question in the terminal."}, nil
	}
	var ke *keyError
	if errors.As(err, &ke) {
		return wire.DaemonAnswerResult{Result: wire.AnswerError, Code: ke.code, Message: ke.message}, nil
	}
	if err != nil {
		return wire.DaemonAnswerResult{}, err
	}
	if res.Result == wire.AnswerSent {
		c.mu.Lock()
		c.sent[id] = c.clock()
		a.ID = id
		c.given[id] = a
		c.mu.Unlock()
	}
	if res.Result == wire.AnswerSent || res.Result == wire.AnswerAlreadyAnswered {
		if cur := c.current(rec.s.ID); cur != nil && cur.q.ID == id {
			c.set(rec.s, nil, "")
		}
	}
	return res, nil
}

// lookup gives the open record of the question, or the result for a question that is not open.
func (c *Channel) lookup(id string) (*record, wire.DaemonAnswerResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.sent[id]; ok {
		return nil, wire.DaemonAnswerResult{Result: wire.AnswerAlreadyAnswered}
	}
	for _, r := range c.open {
		if r.q.ID == id {
			cp := *r
			return &cp, wire.DaemonAnswerResult{}
		}
	}
	if _, ok := c.closed[id]; ok {
		return nil, wire.DaemonAnswerResult{Result: wire.AnswerAlreadyAnswered}
	}
	return nil, wire.DaemonAnswerResult{Result: wire.AnswerGone}
}

// fits checks the answer: known items, known labels, and a text where a text is needed.
func fits(q wire.DaemonQuestion, a wire.QuestionAnswer) error {
	if len(a.Items) == 0 {
		return fmt.Errorf("%w: no items", ErrBadAnswer)
	}
	for _, g := range a.Items {
		var item *wire.DaemonQuestionItem
		for i := range q.Items {
			if q.Items[i].ID == g.ID {
				item = &q.Items[i]
			}
		}
		if item == nil {
			return fmt.Errorf("%w: no item %q", ErrBadAnswer, g.ID)
		}
		for _, c := range g.Choices {
			if labelIndex(*item, c) == 0 {
				return fmt.Errorf("%w: no option %q in %q", ErrBadAnswer, c, g.ID)
			}
		}
		if strings.TrimSpace(g.Other) != "" && !item.AllowsOther && item.Kind != wire.QuestionFree {
			return fmt.Errorf("%w: the item %q takes no text", ErrBadAnswer, g.ID)
		}
		if len(g.Choices) == 0 && strings.TrimSpace(g.Other) == "" {
			return fmt.Errorf("%w: the item %q has no answer", ErrBadAnswer, g.ID)
		}
	}
	return nil
}

// keyPress is one key and the check of the screen before it. holds: the screen is at the step. present: the pause is on the screen.
type keyPress struct {
	keys  string
	check func(screen []string) (holds, present bool)
}

// press sends each key when its check holds. It stops when the pause left the screen (alreadyAnswered)
// or when the screen is at another step (form_changed). After the last key the pause must leave the screen.
func (c *Channel) press(ctx context.Context, terminal string, steps []keyPress) (wire.DaemonAnswerResult, error) {
	if len(steps) == 0 {
		return wire.DaemonAnswerResult{Result: wire.AnswerGone}, nil
	}
	for _, st := range steps {
		for i := 0; ; i++ {
			screen, err := c.screen.Read(ctx, terminal, ScreenLines)
			if err != nil {
				return wire.DaemonAnswerResult{}, err
			}
			holds, present := st.check(screen)
			if !present {
				return wire.DaemonAnswerResult{Result: wire.AnswerAlreadyAnswered}, nil
			}
			if holds {
				break
			}
			if i >= c.Tries-1 {
				return wire.DaemonAnswerResult{}, &keyError{wire.CodeFormChanged, "The question in Orca changed. Answer it in Orca."}
			}
			if err := c.sleep(ctx); err != nil {
				return wire.DaemonAnswerResult{}, err
			}
		}
		if err := c.screen.Send(ctx, terminal, st.keys, false); err != nil {
			return wire.DaemonAnswerResult{}, err
		}
	}
	for i := 0; ; i++ {
		screen, err := c.screen.Read(ctx, terminal, ScreenLines)
		if err != nil {
			return wire.DaemonAnswerResult{}, err
		}
		if _, present := steps[0].check(screen); !present {
			return wire.DaemonAnswerResult{Result: wire.AnswerSent}, nil
		}
		if i >= c.Tries*5-1 {
			return wire.DaemonAnswerResult{}, &keyError{CodeFormOpen, "The question is still open in Orca."}
		}
		if err := c.sleep(ctx); err != nil {
			return wire.DaemonAnswerResult{}, err
		}
	}
}

func (c *Channel) sleep(ctx context.Context) error {
	if c.Wait <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(c.Wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// answerForm types the FormKeys steps. A form from the screen has the hash of its page at the end of the id:
// another page on the screen is an answered page.
func (c *Channel) answerForm(ctx context.Context, r record, a wire.QuestionAnswer) (wire.DaemonAnswerResult, error) {
	rest := strings.TrimPrefix(r.q.ID, "form:")
	page := strings.HasPrefix(rest, r.s.ID+":")
	hash := strings.TrimPrefix(rest, r.s.ID+":")
	var steps []keyPress
	for _, st := range FormKeys(r.q, a, page) {
		st := st
		steps = append(steps, keyPress{keys: st.Keys, check: func(screen []string) (bool, bool) {
			f, ok := ParseForm(screen)
			if !ok || page && fnv(f.Page()) != hash {
				return false, false
			}
			return st.Holds(f), true
		}})
	}
	return c.press(ctx, r.s.Terminal, steps)
}

// answerPrompt presses the choice number, or the text after Tab, as the app does.
// The prompt on the screen must have the choices of the question.
func (c *Channel) answerPrompt(ctx context.Context, r record, a wire.QuestionAnswer) (wire.DaemonAnswerResult, error) {
	item := r.q.Items[0]
	var given *wire.QuestionAnswerItem
	for i := range a.Items {
		if a.Items[i].ID == item.ID {
			given = &a.Items[i]
		}
	}
	if given == nil {
		return wire.DaemonAnswerResult{}, fmt.Errorf("%w: no answer for %q", ErrBadAnswer, item.ID)
	}
	screen, err := c.screen.Read(ctx, r.s.Terminal, ScreenLines)
	if err != nil {
		return wire.DaemonAnswerResult{}, err
	}
	p, ok := ParsePrompt(screen)
	if !ok {
		return wire.DaemonAnswerResult{Result: wire.AnswerAlreadyAnswered}, nil
	}
	var steps []PromptStep
	if item.Kind == wire.QuestionApprove {
		steps = approveKeys(p, *given)
	} else {
		labels := make([]string, len(item.Options))
		for i, o := range item.Options {
			labels[i] = o.Label
		}
		if p.Question != item.Why || !reflect.DeepEqual(p.Labels(), labels) {
			return wire.DaemonAnswerResult{Result: wire.AnswerAlreadyAnswered}, nil
		}
		steps = PromptKeys(p, *given)
	}
	key := p.Key()
	var presses []keyPress
	for _, st := range steps {
		st := st
		presses = append(presses, keyPress{keys: st.Keys, check: func(screen []string) (bool, bool) {
			now, ok := ParsePrompt(screen)
			if !ok || now.Key() != key {
				return false, false
			}
			return st.Holds(now), true
		}})
	}
	return c.press(ctx, r.s.Terminal, presses)
}

// approveKeys answers an Allow or Deny item from a hook with the choices on the screen.
// Allow: the first choice of a permission, the last "Yes" choice of a plan (it keeps the context and asks for each edit).
// Deny: the text choice with the text, or with no text the "No" choice of a permission and Esc for a plan.
func approveKeys(p Prompt, g wire.QuestionAnswerItem) []PromptStep {
	choice := ""
	if len(g.Choices) > 0 {
		choice = g.Choices[0]
	}
	if choice == wire.OptionAllow || choice == "" && strings.TrimSpace(g.Other) == "" {
		label := p.Options[0].Label
		if p.Kind == PromptPlan {
			for _, o := range p.Options {
				if strings.HasPrefix(o.Label, "Yes") {
					label = o.Label
				}
			}
		}
		return PromptKeys(p, wire.QuestionAnswerItem{Choices: []string{label}})
	}
	if strings.TrimSpace(g.Other) != "" && p.Typed != 0 {
		return PromptKeys(p, wire.QuestionAnswerItem{Other: g.Other})
	}
	if p.Kind == PromptPlan {
		return []PromptStep{{Keys: KeyEsc}}
	}
	last := p.Options[len(p.Options)-1]
	return PromptKeys(p, wire.QuestionAnswerItem{Choices: []string{last.Label}})
}

// answerTurn sends the text as a prompt. The session must still be done in the same turn.
func (c *Channel) answerTurn(ctx context.Context, r record, a wire.QuestionAnswer) (wire.DaemonAnswerResult, error) {
	s, ok := c.book.Get(r.s.ID)
	text, _ := TurnQuestion(s.LastMessage)
	if !ok || s.State != wire.PhaseDone || text != r.key || s.LastMessage != r.s.LastMessage {
		return wire.DaemonAnswerResult{Result: wire.AnswerAlreadyAnswered}, nil
	}
	g := a.Items[0]
	msg := strings.TrimSpace(g.Other)
	if msg == "" {
		msg = strings.Join(g.Choices, ", ")
	}
	if err := c.screen.Send(ctx, s.Terminal, msg, true); err != nil {
		return wire.DaemonAnswerResult{}, err
	}
	return wire.DaemonAnswerResult{Result: wire.AnswerSent}, nil
}

func approve(id, header, text string) wire.DaemonQuestionItem {
	return wire.DaemonQuestionItem{ID: id, Header: header, Text: text, Kind: wire.QuestionApprove,
		Options: []wire.QuestionOption{{Label: wire.OptionAllow}, {Label: wire.OptionDeny}}}
}

// toolInput gives the main string of a tool input: the command, the file or the URL. Else the compact JSON.
func toolInput(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "notebook_path", "url", "pattern", "path", "query", "prompt"} {
		if s, ok := in[k].(string); ok && s != "" {
			return s
		}
	}
	data, _ := json.Marshal(in)
	return string(data)
}

// fnv is a short stable hash (FNV-1a, 64 bit) for an id. The same as OrcaRuntime.fnv in the app.
func fnv(s string) string {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * 0x100000001b3
	}
	return strconv.FormatUint(h, 16)
}
