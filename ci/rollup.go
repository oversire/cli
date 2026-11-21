package ci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/wire"
)

const etaWindow = 3 * 24 * time.Hour

// runSet is the runs of one answer.
type runSet []wire.CIRunView

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func workflowOf(r wire.CIRun) string {
	if r.WorkflowName != "" {
		return r.WorkflowName
	}
	return r.Name
}

// runView converts a run of the runs doc. The caller adds the duration and the ETA.
func runView(r wire.CIRun) wire.CIRunView {
	return wire.CIRunView{ID: r.ID, Attempt: max(r.Attempt, 1), Name: r.Name, Workflow: workflowOf(r), Status: r.Status,
		Conclusion: r.Conclusion, Branch: r.HeadBranch, SHA: r.HeadSha, Event: r.Event, URL: r.URL, StartedAt: r.StartedAt,
		UpdatedAt: r.UpdatedAt}
}

// newer tells if run a is newer than run b: a higher id, else a higher attempt. Run ids grow with time.
func newer(a, b wire.CIRunView) bool {
	x, ex := strconv.ParseInt(a.ID, 10, 64)
	y, ey := strconv.ParseInt(b.ID, 10, 64)
	switch {
	case a.ID == b.ID:
		return a.Attempt > b.Attempt
	case ex == nil && ey == nil:
		return x > y
	}
	return a.ID > b.ID
}

// latest keeps the newest attempt of the newest run of each workflow. A rerun thus replaces the old result.
func (s runSet) latest() runSet {
	best := map[string]int{}
	var out runSet
	for _, v := range s {
		i, ok := best[v.Workflow]
		switch {
		case !ok:
			best[v.Workflow] = len(out)
			out = append(out, v)
		case newer(v, out[i]):
			out[i] = v
		}
	}
	return out
}

func failed(conclusion string) bool {
	return conclusion == "failure" || conclusion == "timed_out" || conclusion == "startup_failure"
}

// state gives the rollup state and done. Done is true when no run or check can change. No run and no check is not done:
// a run can still start.
func (s runSet) state(checks []wire.CICheck) (wire.CIRollupState, bool) {
	if len(s) == 0 && len(checks) == 0 {
		return wire.CINone, false
	}
	var fail, run, pend, cancel bool
	done := true
	for _, v := range s {
		switch {
		case v.Status == "in_progress":
			run, done = true, false
		case v.Status != "completed":
			pend, done = true, false
		case failed(v.Conclusion):
			fail = true
		case v.Conclusion == "cancelled":
			cancel = true
		}
	}
	for _, c := range checks {
		switch c.State {
		case wire.CICheckFailure:
			fail = true
		case wire.CICheckRunning:
			run, done = true, false
		case wire.CICheckPending:
			pend, done = true, false
		case wire.CICheckCancelled:
			cancel = true
		}
	}
	switch {
	case fail:
		return wire.CIFailure, done
	case run:
		return wire.CIRunning, false
	case pend:
		return wire.CIPending, false
	case cancel:
		return wire.CICancelled, true
	}
	return wire.CISuccess, true
}

// eta is the mean duration of the completed runs of the workflow in the last 3 days. It is the rule of
// Run.estimatedTotal(history:now:) in the app.
func eta(history []wire.CIRun, workflow string, now time.Time) (time.Duration, bool) {
	var sum time.Duration
	var n int
	for _, r := range history {
		start := parseTime(r.StartedAt)
		if start.IsZero() {
			start = parseTime(r.CreatedAt)
		}
		end := parseTime(r.UpdatedAt)
		if workflowOf(r) != workflow || r.Status != "completed" || start.IsZero() || start.Before(now.Add(-etaWindow)) || end.IsZero() {
			continue
		}
		if d := end.Sub(start); d > 0 {
			sum += d
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / time.Duration(n), true
}

// rev is the first 12 hex chars of the hash of the state, the runs and the checks. The duration and the ETA grow with the
// clock, and the freshness fields change without a change of the CI, so the hash leaves them out.
func rev(r wire.CIRollup) string {
	runs := make([]wire.CIRunView, len(r.Runs))
	for i, v := range r.Runs {
		v.DurationS, v.ETAS = 0, 0
		runs[i] = v
	}
	checks := r.Checks
	if checks == nil {
		checks = []wire.CICheck{}
	}
	b, _ := json.Marshal(struct {
		State  wire.CIRollupState `json:"state"`
		Runs   []wire.CIRunView   `json:"runs"`
		Checks []wire.CICheck     `json:"checks"`
	}{r.State, runs, checks})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// keepWorkflows keeps the runs of the named workflows. No names keeps all.
func (s runSet) keepWorkflows(names []string) runSet {
	if len(names) == 0 {
		return s
	}
	out := runSet{}
	for _, v := range s {
		for _, n := range names {
			if strings.EqualFold(v.Workflow, n) {
				out = append(out, v)
				break
			}
		}
	}
	return out
}
