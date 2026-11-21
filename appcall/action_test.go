package appcall

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActionChanges(t *testing.T) {
	for name, want := range map[string]bool{
		"epic.list": false, "mirror.read": false, "runbook.status": false,
		"item.create": true, "settings.patch": true, "x.y": true,
	} {
		if got := (Action{Name: name}).Changes(); got != want {
			t.Errorf("%s: Changes() = %v, want %v", name, got, want)
		}
	}
}

func TestActionTimeoutMessageNamesWho(t *testing.T) {
	got := Action{Name: "item.setStatus", Args: json.RawMessage(`{"key":"ACME-1"}`)}.TimeoutMessage("mba")
	want := "mba did not answer in time. The change can be made already. Check it before you try again, for example with: oversire item show ACME-1."
	if got != want {
		t.Fatal(got)
	}
	got = Action{Name: "mirror.read"}.TimeoutMessage("mba")
	if got != "mba did not answer in time. Try again." {
		t.Fatal(got)
	}
}

func TestActionWithNoArgs(t *testing.T) {
	got := Action{Name: "item.create"}.TimeoutMessage("The app")
	if !strings.HasSuffix(got, "with: oversire item list.") {
		t.Fatal(got)
	}
}

func TestVendorListIsRead(t *testing.T) {
	if (Action{Name: "vendor.list"}).Changes() {
		t.Fatal("vendor.list changes data")
	}
	if (Action{Name: "vendor.skill"}).Changes() {
		t.Fatal("vendor.skill changes data")
	}
	if !(Action{Name: "vendor.run"}).Changes() {
		t.Fatal("vendor.run is a read")
	}
}

func TestVendorRunTimeoutMessage(t *testing.T) {
	got := Action{Name: "vendor.run", Args: json.RawMessage(`{"project":"OVERSIRE","args":["wi","list"]}`)}.TimeoutMessage("The app")
	if !strings.Contains(got, "a read command of the vendor CLI, with oversire vendor run.") || strings.Contains(got, "oversire item list") {
		t.Fatal(got)
	}
}
