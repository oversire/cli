package wire

import (
	"encoding/json"
	"testing"
)

func roundTrip[T any](t *testing.T, kind string) {
	t.Helper()
	for name, data := range fixtures(t, kind) {
		var v T
		if err := Decode(data, &v); err != nil {
			t.Fatalf("%s/%s: %v", kind, name, err)
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, kind+"/"+name, data, out)
	}
}

func TestDaemonFixtures(t *testing.T) {
	roundTrip[DaemonSession](t, "daemon-session")
	roundTrip[DaemonSessionsPage](t, "daemon-sessions-page")
	roundTrip[DaemonQuestion](t, "daemon-question")
	roundTrip[DaemonQuestionsPage](t, "daemon-questions-page")
	roundTrip[DaemonAnswerResult](t, "daemon-answer-result")
	roundTrip[DaemonInfo](t, "daemon-info")
	roundTrip[Group](t, "group")
	roundTrip[GroupInvite](t, "group-invite")
	roundTrip[JoinRequest](t, "join-request")
	roundTrip[JoinPending](t, "join-pending")
	roundTrip[JoinResult](t, "join-result")
	roundTrip[ProviderEvent](t, "provider-event")
}

func TestSessionSubject(t *testing.T) {
	s := SessionSubject(SessionRef{Workspace: WorkspaceRef{Account: AccountID{Provider: "orca", ID: "mbp"}, ID: "wt-cloud-88"}, ID: "tab-2:leaf-1"})
	out, _ := json.Marshal(s)
	want := `{"kind":"session","ref":{"workspace":{"account":{"provider":"orca","id":"mbp"},"id":"wt-cloud-88"},"id":"tab-2:leaf-1"}}`
	if string(out) != want {
		t.Fatalf("got %s", out)
	}
}

func TestEventsPageDecodes(t *testing.T) {
	ev := fixtures(t, "provider-event")["daemon-session.json"]
	data := []byte(`{"events":[` + string(ev) + `],"head":12,"next":12,"more":false,"truncated":false}`)
	var p EventsPage
	if err := Decode(data, &p); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(p)
	sameJSON(t, "events page", data, out)
}

func TestHookEnvIsStrict(t *testing.T) {
	var e HookEnv
	if err := Decode([]byte(`{"paneKey":"tab-1:leaf-1","terminalHandle":"term_7f3a9c21","ppid":4211}`), &e); err != nil {
		t.Fatal(err)
	}
	if e.PaneKey != "tab-1:leaf-1" || e.PPID != 4211 {
		t.Fatalf("got %+v", e)
	}
	if err := Decode([]byte(`{"pane":"x"}`), &e); err == nil {
		t.Fatal("an unknown field must fail")
	}
}
