package wire

import "testing"

func TestCIFixtures(t *testing.T) {
	roundTrip[CIRunsDoc](t, "ci-runs-doc")
	roundTrip[CIJobsDoc](t, "ci-jobs-doc")
	roundTrip[CIChecksDoc](t, "ci-checks-doc")
	roundTrip[CIStateDoc](t, "ci-state-doc")
	roundTrip[CIRollup](t, "ci-rollup")
	roundTrip[CIRunsPage](t, "ci-runs-page")
	roundTrip[CIRunAnswer](t, "ci-run-answer")
	roundTrip[CIFailuresAnswer](t, "ci-failures-answer")
	roundTrip[CILogView](t, "ci-log-view")
	roundTrip[CIWorkflows](t, "ci-workflows")
	roundTrip[CIActionRequest](t, "ci-action-request")
	roundTrip[CIActionResult](t, "ci-action-result")
	roundTrip[CIWatch](t, "ci-watch")
}

func TestCIRepoStateHas(t *testing.T) {
	s := CIRepoState{Capabilities: []string{"logs", "rerun"}}
	if !s.Has("logs") || s.Has("cancel") {
		t.Error("Has")
	}
}
