package ci

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	explainWindow = 10 * time.Minute
	explainBody   = 4096
	outputsKind   = "ai-outputs"
)

var ErrNotFailed = errors.New("the run did not fail")

// RunFinder reads the index (*Index).
type RunFinder interface {
	Repo(repo string) (wire.CIRepoState, error)
	RunAnswer(repo, id string, attempt int) (wire.CIRunAnswer, error)
	SourceMachine() string
}

// OutputReader reads the shared AI results of the home store (*store.OpLog).
type OutputReader interface {
	Record(collection, id string) (json.RawMessage, bool, error)
}

// explainFlight is one explain call that other requests for the same run attempt wait for.
type explainFlight struct {
	done chan struct{}
	res  wire.CIExplanation
	err  error
}

// Explainer gives the cause of a failed run. It reads the shared cache first. On a miss, it asks the source app to run the AI task.
type Explainer struct {
	app
	runs    RunFinder
	outputs OutputReader
	lim     *Limiter

	mu      sync.Mutex
	flights map[string]*explainFlight
}

// NewExplainer makes an Explainer. lim holds the window of `force`.
func NewExplainer(runs RunFinder, outputs OutputReader, calls Caller, lim *Limiter, self func() wire.GroupMember) *Explainer {
	return &Explainer{app: app{calls: calls, self: self}, runs: runs, outputs: outputs, lim: lim, flights: map[string]*explainFlight{}}
}

// Routes gives POST /v1/ci/runs/{id}/explain. A member daemon sends the request to home.
func (e *Explainer) Routes(fwd *Forward) []server.Route {
	return []server.Route{{Pattern: "POST /v1/ci/runs/{id}/explain", Access: server.Member, Handle: fwd.Wrap(e.handle)}}
}

func (e *Explainer) handle(w http.ResponseWriter, r *http.Request) {
	var req wire.CIExplainRequest
	if !server.Decode(w, r, explainBody, "", &req) {
		return
	}
	res, err := e.Explain(r.Context(), r.PathValue("id"), req)
	if err != nil {
		e.fail(w, err)
		return
	}
	server.WriteJSON(w, http.StatusOK, res)
}

func (e *Explainer) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		server.WriteError(w, http.StatusGatewayTimeout, "The AI did not answer in 60 s. Ask again in a minute. The answer is then in the cache.")
		return
	}
	if errors.Is(err, ErrNotFailed) {
		server.WriteError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	failWith(w, err, e.runs.SourceMachine())
}

// Explain gives the explanation of a run attempt. Without `force`, a saved explanation is enough.
func (e *Explainer) Explain(ctx context.Context, runID string, req wire.CIExplainRequest) (wire.CIExplanation, error) {
	repo, err := checkRepo(req.Repo)
	if err != nil {
		return wire.CIExplanation{}, err
	}
	st, err := e.runs.Repo(repo)
	if err != nil {
		return wire.CIExplanation{}, err
	}
	ans, err := e.runs.RunAnswer(st.Repo, runID, req.Attempt)
	if err != nil {
		return wire.CIExplanation{}, err
	}
	run := ans.Run
	if run.Status != "completed" || run.Conclusion != "failure" {
		return wire.CIExplanation{}, textError{ErrNotFailed, "Run " + runID + " did not fail."}
	}
	req.Repo, req.Attempt = st.Repo, run.Attempt
	return e.once(ctx, st.Repo+"#"+runID+"#"+strconv.Itoa(run.Attempt), run.URL, runID, req)
}

// once runs explain for the key. A request that comes while one runs waits for the same answer.
func (e *Explainer) once(ctx context.Context, key, runURL, runID string, req wire.CIExplainRequest) (wire.CIExplanation, error) {
	e.mu.Lock()
	if f, ok := e.flights[key]; ok {
		e.mu.Unlock()
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return wire.CIExplanation{}, ctx.Err()
		}
	}
	f := &explainFlight{done: make(chan struct{})}
	e.flights[key] = f
	e.mu.Unlock()
	f.res, f.err = e.explain(ctx, key, runURL, runID, req)
	e.mu.Lock()
	delete(e.flights, key)
	e.mu.Unlock()
	close(f.done)
	return f.res, f.err
}

func (e *Explainer) explain(ctx context.Context, key, runURL, runID string, req wire.CIExplainRequest) (wire.CIExplanation, error) {
	if !req.Force {
		if res, ok, err := e.cached(runURL, runID, req); err != nil || ok {
			return res, err
		}
	} else if retry, ok := e.lim.Allow("explain:" + key); !ok {
		return wire.CIExplanation{}, tooSoonError{retry: retry, window: e.lim.Window(), what: "explanation"}
	}
	args := map[string]any{"repo": req.Repo, "runId": runID, "attempt": req.Attempt}
	if req.Job != "" {
		args["job"] = req.Job
	}
	if req.Force {
		args["force"] = true
	}
	data, err := e.callAs(ctx, "ci.explain", args, req.Agent)
	if err != nil {
		e.lim.Forget("explain:" + key)
		return wire.CIExplanation{}, err
	}
	var res wire.CIExplanation
	if err := json.Unmarshal(data, &res); err != nil || res.Cause == "" {
		e.lim.Forget("explain:" + key)
		return wire.CIExplanation{}, &appError{status: wire.AppCallFailed, summary: "The OverSire app sent no explanation for run " + runID + "."}
	}
	if res.Source == "" {
		res.Source = "ai"
	}
	return res, nil
}

// cached reads the explanation that the app saved to the home store.
func (e *Explainer) cached(runURL, runID string, req wire.CIExplainRequest) (wire.CIExplanation, bool, error) {
	rec, ok, err := e.outputs.Record(outputsKind, "ci-explanation:"+runURL+"#"+strconv.Itoa(req.Attempt))
	if err != nil || !ok {
		return wire.CIExplanation{}, false, err
	}
	var out struct {
		Body struct {
			Cause      string    `json:"cause"`
			Fix        string    `json:"fix"`
			Confidence string    `json:"confidence"`
			At         time.Time `json:"at"`
		} `json:"body"`
	}
	if json.Unmarshal(rec, &out) != nil || out.Body.Cause == "" {
		return wire.CIExplanation{}, false, nil
	}
	b := out.Body
	return wire.CIExplanation{Repo: req.Repo, Run: runID, Attempt: req.Attempt, Cause: b.Cause, Fix: b.Fix, Confidence: b.Confidence, At: b.At, Source: "cache"}, true, nil
}
