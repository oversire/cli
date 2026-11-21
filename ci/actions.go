package ci

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/oversire/cli/appcall"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// The app actions of the CI writes (ci.json).
const (
	actionRerun    = "ci.rerun"
	actionRerunJob = "ci.rerunJob"
	actionCancel   = "ci.cancel"
	actionDispatch = "ci.dispatch"
	actionApprove  = "ci.approve"
)

// writes gives the POST routes. Each one sends a member call to the source app. A daemon credential is let in:
// a member daemon forwards the request of its CLI with it.
func (r *Routes) writes() []server.Route {
	run := func(in *wire.CIActionRequest, id string) error { in.Run = id; return nil }
	return []server.Route{
		{Pattern: "POST /v1/ci/runs/{id}/rerun", Access: server.Member, Handle: r.write(actionRerun, run)},
		{Pattern: "POST /v1/ci/jobs/{id}/rerun", Access: server.Member, Handle: r.write(actionRerunJob, func(in *wire.CIActionRequest, id string) error {
			if in.Run == "" {
				return invalid("Give the run of job %s.", id)
			}
			in.Job = id
			return nil
		})},
		{Pattern: "POST /v1/ci/runs/{id}/cancel", Access: server.Member, Handle: r.write(actionCancel, run)},
		{Pattern: "POST /v1/ci/dispatch", Access: server.Member, Handle: r.write(actionDispatch, func(in *wire.CIActionRequest, _ string) error {
			if in.Workflow == "" || in.Ref == "" {
				return invalid("Give the workflow and the ref.")
			}
			return nil
		})},
		{Pattern: "POST /v1/ci/runs/{id}/approve", Access: server.Member, Handle: r.write(actionApprove, run)},
	}
}

// write is the handler of one write route. fill sets the {id} of the path, which wins over the body, and checks the body.
func (r *Routes) write(action string, fill func(in *wire.CIActionRequest, id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in wire.CIActionRequest
		if !server.Decode(w, req, appcall.MaxCallBody, "", &in) {
			return
		}
		repo, err := checkRepo(in.Repo)
		if err == nil {
			err = fill(&in, req.PathValue("id"))
		}
		if err != nil {
			r.fail(w, err)
			return
		}
		st, err := r.x.Repo(repo)
		if err != nil {
			r.fail(w, err)
			return
		}
		in.Repo = st.Repo
		res, err := r.act(req.Context(), action, in, r.from(req))
		if err != nil {
			r.fail(w, err)
			return
		}
		if (action == actionRerun || action == actionRerunJob) && !res.Noop && res.Run != nil {
			r.x.ExpectAttempt(st.Repo, in.Run, res.Run.Attempt+1)
		}
		server.WriteJSON(w, http.StatusOK, res)
	}
}

// from is the sender of a write: this machine on loopback, else the device of the credential. A daemon credential
// gives the machine of that daemon.
func (r *Routes) from(req *http.Request) wire.MemberCaller {
	c, ok := server.CallerFrom(req.Context())
	if !ok || c.Loopback {
		return wire.MemberCaller{Machine: r.self().Machine}
	}
	return wire.MemberCaller{Machine: c.Machine, Name: c.Name, Device: c.DeviceID}
}

// act sends one ci.* write to the source app and gives its data. The agent goes in the cli field, not in the args.
func (r *Routes) act(ctx context.Context, action string, in wire.CIActionRequest, from wire.MemberCaller) (wire.CIActionResult, error) {
	agent := in.Agent
	in.Agent = ""
	args, err := json.Marshal(in)
	if err != nil {
		return wire.CIActionResult{}, err
	}
	data, err := r.send(ctx, wire.MemberCall{Action: action, Args: args, From: from, CLI: &wire.MemberCLI{Agent: agent}})
	if err != nil {
		return wire.CIActionResult{}, err
	}
	var res wire.CIActionResult
	if err := json.Unmarshal(data, &res); err != nil {
		return wire.CIActionResult{}, &appError{status: wire.AppCallFailed, summary: "The OverSire app sent no result for " + action + "."}
	}
	return res, nil
}
