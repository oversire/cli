package wire

import (
	"strings"
	"testing"
)

func TestEventFixtures(t *testing.T) {
	roundTrip[Event](t, "event")
	roundTrip[EventDraft](t, "event-draft")
	roundTrip[EventPage](t, "event-page")
	roundTrip[EventAppend](t, "event-append")
	roundTrip[EventAppendResult](t, "event-append-result")
	roundTrip[ItemLinks](t, "item-links")
	roundTrip[EventFrame](t, "event-frame")
}

func TestEventCatalogMatchesTheFixtures(t *testing.T) {
	cat, err := LoadEventCatalog()
	if err != nil {
		t.Fatal(err)
	}
	special := map[string]bool{"parent-unknown.json": true, "private-prompt.json": true, "provider-human.json": true, "system-reopen.json": true}
	seen := map[string]bool{}
	for name, data := range fixtures(t, "event") {
		var e Event
		if err := Decode(data, &e); err != nil {
			t.Fatal(name, err)
		}
		if p := cat.Problems(e.EventDraft); len(p) > 0 {
			t.Errorf("%s: %v", name, p)
		}
		if !special[name] {
			if name != e.Kind+".json" {
				t.Errorf("%s holds kind %s", name, e.Kind)
			}
			seen[e.Kind] = true
		}
	}
	for k := range cat {
		if !seen[k] {
			t.Errorf("kind %s has no fixture", k)
		}
	}
	for _, k := range []string{KindSessionStarted, KindSessionEnded, KindTurnStarted, KindTurnEnded, KindTurnInterrupted,
		KindWaitStarted, KindWaitEnded, KindContextCompacted, KindSubagentEnded, KindReviewReady, KindPromptSubmitted,
		KindQuestionAnswered, KindReviewSubmitted, KindReviewApproved, KindWorktreeAdded, KindWorktreeRemoved, KindGitCommit,
		KindGitRebase, KindGitReset, KindGitMerge, KindGitPush, KindQueuePicked, KindQueueHeld, KindRunbookStepStarted,
		KindRunbookStepEnded, KindAutomationRan} {
		if _, ok := cat[k]; !ok {
			t.Errorf("constant %s is not in the catalog", k)
		}
	}
	if p := cat.Problems(EventDraft{Kind: "made.up"}); len(p) == 0 || !strings.Contains(p[0], "not in") {
		t.Fatal(p)
	}
}
