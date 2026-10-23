package orca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

const (
	// coordinatorTitle names the quiet terminal that holds the Run of the workers. The app used the same title.
	coordinatorTitle     = "oversire-coordinator"
	coordinatorObjective = "OverSire questions"
	// askLife: an older ask is not open any more.
	askLife = 24 * time.Hour
)

// The Orca error codes of a coordinator terminal that is gone or bound to another consumer.
var staleCoordinator = []string{"stable_pane_required", "consumer_fenced", "terminal_not_found"}

// The worker states of worker-show after which no worker_done can come.
var settled = []string{"failed", "stopped", "abandoned", "exited", "cancelled", "succeeded"}

// coordinator is the quiet terminal of this Orca and the Run it is bound to. Workers send to run:<run>.
type coordinator struct{ terminal, run string }

func (c coordinator) address() string { return "run:" + c.run }

// message is one row of orchestration inbox.
type message struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	From    string          `json:"from_handle"`
	To      string          `json:"to_handle"`
	Thread  *string         `json:"thread_id"`
	Body    string          `json:"body"`
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"created_at"`
}

// replyTo is the question that a reply names in its thread. A question of a worker names itself.
func (m message) replyTo() string {
	if m.Thread == nil || *m.Thread == m.ID {
		return ""
	}
	return *m.Thread
}

// payload is a JSON object, or a string that holds one.
func (m message) payload() map[string]any {
	var v any
	if json.Unmarshal(m.Payload, &v) != nil {
		return nil
	}
	if s, ok := v.(string); ok && json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	d, _ := v.(map[string]any)
	return d
}

func (c coordinator) gets(m message) bool { return m.To == c.address() || m.To == c.terminal }

// coordinatorFor finds or makes the coordinator once. A failed search tries again on the next call.
func (o *Orca) coordinatorFor(ctx context.Context) (coordinator, error) {
	o.coordMu.Lock()
	defer o.coordMu.Unlock()
	if o.coord != nil {
		return *o.coord, nil
	}
	c, err := o.findCoordinator(ctx)
	if err != nil {
		return c, err
	}
	o.coord = &c
	return c, nil
}

// findCoordinator takes the live terminal with the coordinator title, else a new one. Then its Run:
// the bound one, else the newest OverSire Run, else a new Run.
func (o *Orca) findCoordinator(ctx context.Context) (coordinator, error) {
	var c coordinator
	var list struct{ Terminals []terminal }
	if err := o.callInto(ctx, 20*time.Second, &list, "terminal", "list", "--limit", "1000"); err != nil {
		return c, err
	}
	for _, t := range list.Terminals {
		if t.Title == coordinatorTitle && !t.Orphaned {
			c.terminal = t.Handle
			break
		}
	}
	if c.terminal == "" {
		var made struct{ Terminal terminal }
		if err := o.callInto(ctx, 30*time.Second, &made, "terminal", "create", "--title", coordinatorTitle); err != nil {
			return c, err
		}
		if made.Terminal.Handle == "" {
			return c, errors.New("Orca made no coordinator terminal")
		}
		c.terminal = made.Terminal.Handle
	}
	var cur struct{ Run *struct{ ID string } }
	if err := o.callInto(ctx, 20*time.Second, &cur, "orchestration", "run-current", "--from", c.terminal); err != nil {
		return c, err
	}
	if cur.Run != nil && cur.Run.ID != "" {
		c.run = cur.Run.ID
		return c, nil
	}
	var runs struct {
		Runs []struct {
			ID        string
			Objective string
			CreatedAt string `json:"created_at"`
		}
	}
	if err := o.callInto(ctx, 20*time.Second, &runs, "orchestration", "run-list"); err != nil {
		return c, err
	}
	old := ""
	newest := ""
	for _, r := range runs.Runs {
		if r.Objective == coordinatorObjective && r.CreatedAt > newest {
			old, newest = r.ID, r.CreatedAt
		}
	}
	if old != "" {
		if _, err := o.call(ctx, 20*time.Second, "orchestration", "run-use", "--id", old, "--from", c.terminal); err == nil {
			c.run = old
			return c, nil
		}
	}
	var made struct{ Run struct{ ID string } }
	if err := o.callInto(ctx, 20*time.Second, &made, "orchestration", "run-create", "--objective", coordinatorObjective, "--from", c.terminal); err != nil {
		return c, err
	}
	if made.Run.ID == "" {
		return c, errors.New("Orca made no Run")
	}
	c.run = made.Run.ID
	return c, nil
}

// withCoordinator calls f with the coordinator. When Orca tells that the coordinator is stale, it finds it again and calls f once more.
func (o *Orca) withCoordinator(ctx context.Context, f func(coordinator) error) error {
	c, err := o.coordinatorFor(ctx)
	if err != nil {
		return err
	}
	err = f(c)
	var oe *Error
	if !errors.As(err, &oe) || !slices.Contains(staleCoordinator, oe.Code) {
		return err
	}
	o.coordMu.Lock()
	o.coord = nil
	o.coordMu.Unlock()
	if c, err = o.coordinatorFor(ctx); err != nil {
		return err
	}
	return f(c)
}

// StartWorker starts a worker in the folder w.Path with w.Spec as its task. Its worker_done goes to the coordinator Run.
func (o *Orca) StartWorker(ctx context.Context, w wire.WorkerStart) (runtime.WorkerHandle, error) {
	var h runtime.WorkerHandle
	if !agentName.MatchString(w.Agent) {
		return h, fmt.Errorf("bad agent name %q", w.Agent)
	}
	err := o.withCoordinator(ctx, func(c coordinator) error {
		raw, err := o.call(ctx, 180*time.Second, "orchestration", "worker-start", "--spec", w.Spec, "--task-title", w.Title,
			"--worktree", "path:"+w.Path, "--agent", w.Agent, "--run", c.run, "--from", c.terminal)
		if err != nil {
			return err
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		h = runtime.WorkerHandle{Dispatch: find("dispatchId", v), Task: find("taskId", v), Terminal: find("agentTerminalHandle", v)}
		if h.Terminal == "" {
			h.Terminal = find("terminalHandle", v)
		}
		if h.Dispatch == "" {
			return errors.New("Orca started no worker")
		}
		return nil
	})
	return h, err
}

// WorkerState reads the inbox again after worker-show, because the worker can settle between the two calls.
func (o *Orca) WorkerState(ctx context.Context, h runtime.WorkerHandle) (wire.WorkerState, error) {
	if st, ok, err := o.doneMessage(ctx, h); err != nil || ok {
		return st, err
	}
	raw, err := o.call(ctx, 20*time.Second, "orchestration", "worker-show", "--dispatch", h.Dispatch)
	if err != nil {
		return wire.WorkerState{}, err
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	state := find("workerState", v)
	if !slices.Contains(settled, state) {
		return wire.WorkerState{State: wire.WorkerRunning}, nil
	}
	if st, ok, err := o.doneMessage(ctx, h); err != nil || ok {
		return st, err
	}
	return wire.WorkerState{State: wire.WorkerEnded, Reason: state}, nil
}

func (o *Orca) doneMessage(ctx context.Context, h runtime.WorkerHandle) (wire.WorkerState, bool, error) {
	mail, err := o.inbox(ctx)
	if err != nil {
		return wire.WorkerState{}, false, err
	}
	for _, m := range mail {
		p := m.payload()
		if m.Type != "worker_done" || p["dispatchId"] != h.Dispatch {
			continue
		}
		outcome, _ := p["outcome"].(string)
		if outcome == "" {
			outcome = "succeeded"
		}
		return wire.WorkerState{State: wire.WorkerDone, Outcome: outcome, Body: m.Body}, true, nil
	}
	return wire.WorkerState{}, false, nil
}

func (o *Orca) StopWorker(ctx context.Context, h runtime.WorkerHandle) error {
	_, err := o.call(ctx, 20*time.Second, "orchestration", "worker-stop", "--dispatch", h.Dispatch)
	return err
}

func (o *Orca) ReleaseWorker(ctx context.Context, h runtime.WorkerHandle) error {
	_, err := o.call(ctx, 20*time.Second, "orchestration", "worker-release", "--dispatch", h.Dispatch)
	return err
}

// Mail reads the inbox once. It gives the newest ask of each terminal from the last day, with its reply.
func (o *Orca) Mail(ctx context.Context) (runtime.Mail, error) {
	out := runtime.Mail{Workers: map[string]bool{}}
	c, err := o.coordinatorFor(ctx)
	if err != nil {
		return out, err
	}
	mail, err := o.inbox(ctx)
	if err != nil {
		return out, err
	}
	replies := map[string]string{}
	newest := map[string]message{}
	for _, m := range mail {
		if to := m.replyTo(); to != "" {
			if _, ok := replies[to]; !ok {
				replies[to] = m.Body
			}
		}
		if !c.gets(m) {
			continue
		}
		out.Workers[m.From] = true
		if m.Type == "question" && o.now().Sub(m.At) < askLife {
			if cur, ok := newest[m.From]; !ok || m.At.After(cur.At) {
				newest[m.From] = m
			}
		}
	}
	for _, m := range newest {
		a := runtime.Ask{ID: m.ID, Terminal: m.From, Body: m.Body, At: m.At}
		if r, ok := replies[m.ID]; ok {
			a.Reply = &r
		}
		out.Asks = append(out.Asks, a)
	}
	sort.Slice(out.Asks, func(i, j int) bool { return out.Asks[i].At.Before(out.Asks[j].At) })
	return out, nil
}

// Reply answers the ask. Orca makes a new message for each reply, so the caller checks for a reply first.
func (o *Orca) Reply(ctx context.Context, ask, body string) error {
	return o.withCoordinator(ctx, func(c coordinator) error {
		_, err := o.call(ctx, 20*time.Second, "orchestration", "reply", "--id", ask, "--body", body, "--from", c.terminal)
		return err
	})
}

// inbox gives the last 200 messages of this Orca, newest first. It marks nothing read.
func (o *Orca) inbox(ctx context.Context) ([]message, error) {
	var r struct{ Messages []message }
	err := o.callInto(ctx, 20*time.Second, &r, "orchestration", "inbox", "--limit", "200")
	return r.Messages, err
}

// find gives the first string of the key at any depth. The receipts nest the ids in different places.
func find(key string, v any) string {
	switch v := v.(type) {
	case map[string]any:
		if s, ok := v[key].(string); ok && s != "" {
			return s
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s := find(key, v[k]); s != "" {
				return s
			}
		}
	case []any:
		for _, x := range v {
			if s := find(key, x); s != "" {
				return s
			}
		}
	}
	return ""
}
