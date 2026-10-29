package wire

import "testing"

func TestRunbookFixtures(t *testing.T) {
	roundTrip[Runbook](t, "runbook")
	roundTrip[RunbookStart](t, "runbook-start")
	roundTrip[RunbookRun](t, "runbook-run")
	roundTrip[StepDone](t, "step-done")
	roundTrip[StepPatch](t, "step-patch")
}

func TestRunbookLookup(t *testing.T) {
	var r RunbookRun
	if err := Decode(fixtures(t, "runbook-run")["paused-round-limit.json"], &r); err != nil {
		t.Fatal(err)
	}
	if !r.State.Open() || r.Runbook.Step("review-plan").Review.Fix != "plan-fixes" || r.Runbook.Index("implement") != 3 || r.Runbook.Step("x") != nil {
		t.Fatalf("%+v", r)
	}
}
