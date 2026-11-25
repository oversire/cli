package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	maxNotifyJobs  = 20
	notifyFile     = "notify.json"
	notifyPoll     = 300 * time.Second
	notifySettle   = time.Minute
	notifyIdleMax  = 10 * time.Minute
	notifyIdleStep = 2 * time.Second
	notifyRetry    = 15 * time.Second
	notifyFloor    = time.Second
	notifySendTry  = 3
	notifyLineMax  = 200
)

var (
	ErrNoSession  = errors.New("OverSire does not know this agent session.")
	ErrCannotSend = errors.New("OverSire cannot send text into this session. Use oversire ci wait without --notify.")
	ErrFull       = errors.New("OverSire has too many notify jobs. Wait for one to end.")
)

// SessionFinder finds a session of this daemon (sessions.Book).
type SessionFinder interface {
	Get(id string) (wire.DaemonSession, bool)
}

// Sender types text into a session (ask.Asker).
type Sender interface {
	CanSend(s wire.DaemonSession) bool
	Deliver(ctx context.Context, s wire.DaemonSession, text string) error
}

// RollupWaiter holds a long poll on the CI state (HomeClient).
type RollupWaiter interface {
	Wait(ctx context.Context, q WaitQuery) (wire.CIRollup, error)
}

// WatchAdder adds or extends a watch on home (HomeClient).
type WatchAdder interface {
	AddWatch(ctx context.Context, req wire.CIWatchRequest) (wire.CIWatchAnswer, error)
}

// NotifyJob is one wait that sends its result into an agent session.
type NotifyJob struct {
	Key       string              `json:"key"`
	Session   string              `json:"session"`
	Request   wire.CIWatchRequest `json:"request"`
	CreatedAt time.Time           `json:"createdAt"`
	ExpiresAt time.Time           `json:"expiresAt"`
}

// Notifier runs the notify jobs of this daemon. A job waits for the end of the CI of a commit and then types the result into the session.
type Notifier struct {
	Log *slog.Logger

	sessions SessionFinder
	sender   Sender
	waiter   RollupWaiter
	watches  WatchAdder
	path     string
	now      func() time.Time

	poll, settle, idleMax, idleStep, retry, floor time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	jobs   map[string]NotifyJob
}

func NewNotifier(sessions SessionFinder, sender Sender, waiter RollupWaiter, watches WatchAdder, dir string, clock func() time.Time) *Notifier {
	if clock == nil {
		clock = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Notifier{Log: slog.Default(), sessions: sessions, sender: sender, waiter: waiter, watches: watches, path: filepath.Join(dir, notifyFile), now: clock,
		poll: notifyPoll, settle: notifySettle, idleMax: notifyIdleMax, idleStep: notifyIdleStep, retry: notifyRetry, floor: notifyFloor,
		ctx: ctx, cancel: cancel, jobs: map[string]NotifyJob{}}
}

// Start adds the watch and queues the job. A rollup that has ended gives done and no job.
func (n *Notifier) Start(_ context.Context, req wire.CIWatchRequest) (wire.CIWatchAnswer, error) {
	s, err := n.session(req.Notify)
	if err != nil {
		return wire.CIWatchAnswer{}, err
	}
	key := strings.ToLower(s.ID + "|" + req.Repo + "|" + req.Ref)
	n.mu.Lock()
	if _, ok := n.jobs[key]; ok {
		n.mu.Unlock()
		return wire.CIWatchAnswer{Notify: "queued"}, nil
	}
	if len(n.jobs) >= maxNotifyJobs {
		n.mu.Unlock()
		return wire.CIWatchAnswer{}, ErrFull
	}
	n.mu.Unlock()

	ans, err := n.watches.AddWatch(n.ctx, req)
	if err != nil {
		return wire.CIWatchAnswer{}, err
	}
	if ans.Done {
		ans.Notify = "done"
		return ans, nil
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = defaultWatchTTL
	}
	now := n.now()
	job := NotifyJob{Key: key, Session: s.ID, Request: req, CreatedAt: now, ExpiresAt: now.Add(time.Duration(ttl) * time.Second)}
	n.mu.Lock()
	if _, ok := n.jobs[key]; ok {
		n.mu.Unlock()
		return wire.CIWatchAnswer{Watch: ans.Watch, Notify: "queued"}, nil
	}
	n.jobs[key] = job
	err = n.save()
	n.mu.Unlock()
	if err != nil {
		n.Log.Warn("ci notify: save the jobs", "err", err)
	}
	n.launch(job, false)
	return wire.CIWatchAnswer{Watch: ans.Watch, Notify: "queued"}, nil
}

// session finds the session of the target and checks that it takes text.
func (n *Notifier) session(t *wire.CINotifyTarget) (wire.DaemonSession, error) {
	if t == nil {
		return wire.DaemonSession{}, invalid("Give notify.session or notify.paneKey.")
	}
	for _, id := range []string{t.Session, t.PaneKey} {
		if id == "" {
			continue
		}
		if s, ok := n.sessions.Get(id); ok {
			if !n.sender.CanSend(s) {
				return wire.DaemonSession{}, ErrCannotSend
			}
			return s, nil
		}
	}
	if t.Session == "" && t.PaneKey == "" {
		return wire.DaemonSession{}, invalid("Give notify.session or notify.paneKey.")
	}
	return wire.DaemonSession{}, ErrNoSession
}

// Resume starts the saved jobs that did not expire. It blocks until ctx ends.
func (n *Notifier) Resume(ctx context.Context) error {
	n.load()
	<-ctx.Done()
	n.cancel()
	n.wg.Wait()
	return nil
}

func (n *Notifier) load() {
	b, err := os.ReadFile(n.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			n.Log.Warn("ci notify: read the jobs", "err", err)
		}
		return
	}
	var doc struct {
		Jobs []NotifyJob `json:"jobs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		n.Log.Warn("ci notify: the jobs file is not valid", "err", err)
		return
	}
	for _, j := range doc.Jobs {
		if !j.ExpiresAt.After(n.now()) {
			continue
		}
		n.mu.Lock()
		_, known := n.jobs[j.Key]
		if !known && len(n.jobs) < maxNotifyJobs {
			n.jobs[j.Key] = j
		}
		n.mu.Unlock()
		if !known {
			n.launch(j, true)
		}
	}
	n.mu.Lock()
	_ = n.save()
	n.mu.Unlock()
}

// save writes the jobs. The caller holds mu.
func (n *Notifier) save() error {
	list := make([]NotifyJob, 0, len(n.jobs))
	for _, j := range n.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	b, err := json.MarshalIndent(map[string]any{"jobs": list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return err
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, n.path)
}

// List gives the jobs, sorted by key.
func (n *Notifier) List() []NotifyJob {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]NotifyJob, 0, len(n.jobs))
	for _, j := range n.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (n *Notifier) launch(j NotifyJob, extend bool) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.run(j, extend)
	}()
}

// remove drops a job that ended. A job that the daemon stop interrupted stays in the file.
func (n *Notifier) remove(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.jobs, key)
	if err := n.save(); err != nil {
		n.Log.Warn("ci notify: save the jobs", "err", err)
	}
}

// run waits for the end of the CI and sends the result. extend is true when the watch must be added again first.
func (n *Notifier) run(j NotifyJob, extend bool) {
	var since string
	for n.ctx.Err() == nil {
		left := j.ExpiresAt.Sub(n.now())
		if left <= 0 {
			n.finish(j, timeoutText(j))
			return
		}
		began := time.Now()
		roll, done, err := n.step(j, extend, since, left)
		extend = true
		var he *HomeError
		switch {
		case n.ctx.Err() != nil:
			return
		case errors.As(err, &he) && he.Status == http.StatusNotFound:
			n.finish(j, notFoundText(j, he.Text))
			return
		case err != nil:
			n.Log.Warn("ci notify: wait", "repo", j.Request.Repo, "ref", j.Request.Ref, "err", err)
			if !n.sleep(min(n.retry, max(left, time.Millisecond))) {
				return
			}
		case done:
			n.finish(j, resultText(roll, j.Request.Ref))
			return
		default:
			since = roll.Rev
			// An answer that comes at once must not spin.
			if gap := n.floor - time.Since(began); gap > 0 && !n.sleep(gap) {
				return
			}
		}
	}
}

// step extends the watch and holds one long poll.
func (n *Notifier) step(j NotifyJob, extend bool, since string, left time.Duration) (roll wire.CIRollup, done bool, err error) {
	if extend {
		req := j.Request
		req.TTL = min(max(int(left/time.Second), minWatchTTL), maxWatchTTL)
		ans, err := n.watches.AddWatch(n.ctx, req)
		if err != nil {
			return roll, false, err
		}
		if ans.Done && ans.Rollup != nil {
			return *ans.Rollup, true, nil
		}
	}
	timeout := min(n.poll, left)
	roll, err = n.waiter.Wait(n.ctx, WaitQuery{Repo: j.Request.Repo, Ref: j.Request.Ref, Since: since, Timeout: max(timeout.Round(time.Second), time.Second),
		Settle: n.settle, Started: j.CreatedAt})
	return roll, err == nil && roll.Done, err
}

func (n *Notifier) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-n.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// finish sends the text and removes the job. A session that is gone or cannot take text ends the job.
func (n *Notifier) finish(j NotifyJob, text string) {
	if err := n.send(j, text); err != nil {
		if n.ctx.Err() != nil {
			return
		}
		n.Log.Warn("ci notify: send", "session", j.Session, "err", err)
	}
	n.remove(j.Key)
}

func (n *Notifier) send(j NotifyJob, text string) error {
	var err error
	for try := 0; try < notifySendTry; try++ {
		if try > 0 && !n.sleep(n.retry) {
			return n.ctx.Err()
		}
		s, ok := n.sessions.Get(j.Session)
		if !ok {
			return ErrNoSession
		}
		if !n.sender.CanSend(s) {
			return ErrCannotSend
		}
		if s.Runtime == wire.RuntimeOrca {
			s = n.waitIdle(s)
		}
		if err = n.sender.Deliver(n.ctx, s, text); err == nil {
			return nil
		}
	}
	return err
}

// waitIdle waits until an Orca session is not working. After idleMax it gives the session as it is.
func (n *Notifier) waitIdle(s wire.DaemonSession) wire.DaemonSession {
	end := n.now().Add(n.idleMax)
	for s.State == wire.PhaseWorking && n.now().Before(end) {
		if !n.sleep(n.idleStep) {
			return s
		}
		next, ok := n.sessions.Get(s.ID)
		if !ok {
			return s
		}
		s = next
	}
	return s
}

// Routes gives POST and GET /v1/ci/notify. Only an agent on this machine can ask.
func (n *Notifier) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/ci/notify", Access: server.LoopbackOnly, Handle: n.post},
		{Pattern: "GET /v1/ci/notify", Access: server.LoopbackOnly, Handle: n.get},
	}
}

func (n *Notifier) post(w http.ResponseWriter, r *http.Request) {
	var req wire.CIWatchRequest
	if !server.Decode(w, r, watchBodyLimit, "Give a JSON body: ", &req) {
		return
	}
	ans, err := n.Start(r.Context(), req)
	var he *HomeError
	switch {
	case err == nil:
		server.WriteJSON(w, http.StatusOK, ans)
	case errors.Is(err, ErrNoSession), errors.Is(err, ErrCannotSend):
		server.WriteError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrFull):
		server.WriteError(w, http.StatusTooManyRequests, err.Error())
	case errors.As(err, &he) && he.Status != 0:
		server.WriteError(w, he.Status, he.Text)
	case errors.As(err, &he):
		server.WriteError(w, http.StatusServiceUnavailable, he.Text)
	default:
		failCI(w, err)
	}
}

func (n *Notifier) get(w http.ResponseWriter, _ *http.Request) {
	server.WriteJSON(w, http.StatusOK, n.List())
}

// resultText is the message for the end of the CI.
func resultText(r wire.CIRollup, ref string) string {
	head := fmt.Sprintf("[OverSire] CI of %s (%s)", sha7(r, ref), r.Repo)
	switch r.State {
	case wire.CISuccess:
		return fmt.Sprintf("%s passed. %s in %s.", head, plural(len(r.Runs), "run"), spanText(longestRun(r)))
	case wire.CINone:
		return head + " started no run. Run: oversire ci status " + ref
	case wire.CICancelled:
		return head + " was cancelled. Run: oversire ci status " + ref
	}
	return head + " failed." + failureText(r, ref)
}

func timeoutText(j NotifyJob) string {
	ttl := j.ExpiresAt.Sub(j.CreatedAt)
	return fmt.Sprintf("[OverSire] CI of %s (%s) did not end in %s. Run: oversire ci status %s", sanitize(j.Request.Ref, notifyLineMax), j.Request.Repo, spanText(int(ttl.Seconds())), j.Request.Ref)
}

// notFoundText is the message when home does not know the ref after the settle time.
func notFoundText(j NotifyJob, why string) string {
	return fmt.Sprintf("[OverSire] %s Run: oversire ci status %s", sanitize(why, notifyLineMax), j.Request.Ref)
}

func sha7(r wire.CIRollup, ref string) string {
	if len(r.SHA) >= 7 {
		return r.SHA[:7]
	}
	return sanitize(ref, 12)
}

// failureText names the first failed step and the first message or log line. A failure of a check only points to the status.
func failureText(r wire.CIRollup, ref string) string {
	for _, run := range r.Runs {
		if run.Conclusion != "failure" && run.Conclusion != "timed_out" && run.Conclusion != "startup_failure" {
			continue
		}
		flow := run.Workflow
		if flow == "" {
			flow = run.Name
		}
		out := ""
		if len(run.Failures) > 0 {
			f := run.Failures[0]
			out = " " + sanitize(strings.TrimSpace(strings.Join(nonEmpty(flow, f.Job, f.Step), " › ")), notifyLineMax) + ":"
			if line := firstLine(f); line != "" {
				out += " " + sanitize(line, notifyLineMax)
			}
		}
		return out + " Run: oversire ci why " + run.ID
	}
	return " Run: oversire ci status " + ref
}

func firstLine(f wire.CIFailureView) string {
	for _, m := range f.Messages {
		if strings.TrimSpace(m) != "" {
			return m
		}
	}
	for i := len(f.LogTail) - 1; i >= 0; i-- {
		if strings.TrimSpace(f.LogTail[i]) != "" {
			return f.LogTail[i]
		}
	}
	return ""
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sanitize makes one plain line: no control byte, one space between words, at most limit runes.
func sanitize(s string, limit int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if rs := []rune(s); len(rs) > limit {
		s = string(rs[:limit-1]) + "…"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func longestRun(r wire.CIRollup) int {
	best := 0
	for _, run := range r.Runs {
		best = max(best, run.DurationS)
	}
	return best
}

// spanText writes a time span of seconds in short form, for example "4 min 12 s".
func spanText(s int) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s%60 == 0:
		return fmt.Sprintf("%d min", s/60)
	case s < 3600:
		return fmt.Sprintf("%d min %d s", s/60, s%60)
	}
	return fmt.Sprintf("%d h %d min", s/3600, s%3600/60)
}
