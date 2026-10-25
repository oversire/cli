package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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
	roundTrip[SessionAsk](t, "session-ask")
	roundTrip[SessionAskReply](t, "session-ask-reply")
	roundTrip[DaemonRuntimesPage](t, "daemon-runtimes-page")
	roundTrip[DaemonReposPage](t, "daemon-repos-page")
	roundTrip[DaemonWorkspacesPage](t, "daemon-workspaces-page")
	roundTrip[WorkspaceRemove](t, "workspace-remove")
	roundTrip[RuntimePair](t, "runtime-pair")
	roundTrip[WorkspaceRemoval](t, "workspace-removal")
	roundTrip[WorkspaceFiles](t, "workspace-files")
	roundTrip[WorkspaceFilesResult](t, "workspace-files-result")
	roundTrip[LeaseRequest](t, "lease-request")
	roundTrip[Lease](t, "lease")
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

func TestSessionAsk(t *testing.T) {
	no := false
	cases := []struct {
		a       SessionAsk
		ok      bool
		timeout time.Duration
		prompt  string
	}{
		{SessionAsk{Text: "why?"}, true, 180 * time.Second, "why?\n" + AskReadOnlyLine},
		{SessionAsk{Text: "why?\n", TimeoutSec: 900, ReadOnly: &no}, true, 600 * time.Second, "why?\n"},
		{SessionAsk{Text: " \n"}, false, 0, ""},
		{SessionAsk{Text: strings.Repeat("é", MaxAskText+1)}, false, 0, ""},
		{SessionAsk{Text: "x", TimeoutSec: -1}, false, 0, ""},
	}
	for i, c := range cases {
		if err := c.a.Check(); (err == nil) != c.ok {
			t.Errorf("%d: check %v", i, err)
		}
		if c.ok && (c.a.Timeout() != c.timeout || c.a.Prompt() != c.prompt) {
			t.Errorf("%d: %v %q", i, c.a.Timeout(), c.a.Prompt())
		}
	}
}
