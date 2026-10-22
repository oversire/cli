package sessions

import "github.com/oversire/cli/wire"

// Report applies a session report of the oversire runtime as a wire patch. It does not block.
func (b *Book) Report(s wire.RunnerSession) { b.Apply(RunnerPatch(s)) }

// RunnerPatch converts a session of the oversire runtime into a wire patch.
func RunnerPatch(s wire.RunnerSession) Patch {
	str := func(v string) *string { return &v }
	state, reason := wire.PhaseWorking, wire.SessionReason("")
	switch s.State {
	case wire.SessionQueued, wire.SessionStarting:
		state = wire.PhaseStarting
	case wire.SessionWaitingInput:
		state, reason = wire.PhaseWaiting, wire.ReasonTurnEnd
	case wire.SessionWaitingPermission:
		state, reason = wire.PhaseWaiting, wire.ReasonPermission
	case wire.SessionDone:
		state = wire.PhaseDone
	case wire.SessionFailed:
		state, reason = wire.PhaseFailed, wire.ReasonError
	}
	question := ""
	if s.Question != nil && !s.State.Ended() {
		state, reason, question = wire.PhaseWaiting, wire.ReasonQuestion, s.Question.ID
	}
	p := Patch{
		ID: s.ID, Source: SourceWire, At: s.UpdatedAt,
		Runtime: str(wire.RuntimeOversire), Harness: str(s.Agent), Workspace: str(s.Session.Workspace.ID),
		Repo: str(s.Repo), Branch: str(s.Branch),
		State: &state, Reason: &reason, Question: &question,
		End: s.State.Ended(),
	}
	if s.Summary != "" {
		p.LastMessage = str(s.Summary)
	}
	if s.Permission != nil {
		p.Tool, p.ToolInput = str(s.Permission.Tool), str(s.Permission.Text)
	}
	return p
}
