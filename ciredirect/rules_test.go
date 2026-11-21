package ciredirect

import (
	"strings"
	"testing"
)

func replacement(t *testing.T, cmd string) string {
	t.Helper()
	r, ok := Rules{}.Match(cmd)
	if !ok {
		t.Fatalf("no match: %s", cmd)
	}
	return r.Replacement
}

func TestMatchGhRunWatch(t *testing.T) {
	r, ok := Rules{}.Match("gh run watch 18233412 --exit-status")
	if !ok || r.Replacement != "oversire ci wait 18233412" || r.Command != "gh run watch 18233412 --exit-status" {
		t.Fatalf("%+v %v", r, ok)
	}
	if got := replacement(t, "gh run watch -i 5 --compact -R o/r 7"); got != "oversire ci wait 7 --repo o/r" {
		t.Fatal(got)
	}
}

func TestMatchGhRunView(t *testing.T) {
	for cmd, want := range map[string]string{
		"gh run view 1 --log-failed":       "oversire ci why 1",
		"gh run view 1 --log":              "oversire ci logs 1",
		"gh run view --job 7 --log":        "oversire ci logs 7",
		"gh run view 1":                    "oversire ci show 1",
		"gh run view 1 --json status":      "oversire ci show 1 --json",
		"gh run view 1 --attempt 2":        "oversire ci show 1 --attempt 2",
		"gh run view https://x/runs/9":     "oversire ci show https://x/runs/9",
		"gh run view":                      "oversire ci status",
		"gh run view --log-failed":         "oversire ci status",
		"gh run view --job=7 --log-failed": "oversire ci logs 7 --failed",
	} {
		if got := replacement(t, cmd); got != want {
			t.Errorf("%s: got %s, want %s", cmd, got, want)
		}
	}
	if _, ok := (Rules{}).Match("gh run view 1 --web"); ok {
		t.Fatal("--web must pass")
	}
}

func TestMatchGhRunList(t *testing.T) {
	if got := replacement(t, "gh run list -b main -w test -L 5 -s failure -R o/r"); got != "oversire ci ls --branch main --workflow test --limit 5 --state failure --repo o/r" {
		t.Fatal(got)
	}
	for cmd, want := range map[string]string{
		"gh run list -s queued":        "oversire ci ls --state pending",
		"gh run list -s in_progress":   "oversire ci ls --state running",
		"gh run list -s completed":     "oversire ci ls",
		"gh run list -c abc1234":       "oversire ci ls --sha abc1234",
		"gh run list --limit=3 --json": "oversire ci ls --limit 3 --json",
	} {
		if got := replacement(t, cmd); got != want {
			t.Errorf("%s: got %s, want %s", cmd, got, want)
		}
	}
}

func TestMatchPrChecks(t *testing.T) {
	for cmd, want := range map[string]string{
		"gh pr checks 212 --watch --fail-fast":       "oversire ci wait '#212' --fail-fast",
		"gh pr checks":                               "oversire ci status",
		"gh pr checks 5":                             "oversire ci status '#5'",
		"gh pr checks https://github.com/o/r/pull/9": "oversire ci status '#9'",
		"gh pr checks my-branch -R o/r":              "oversire ci status my-branch --repo o/r",
		"gh pr checks 5 --fail-fast":                 "oversire ci status '#5'",
	} {
		if got := replacement(t, cmd); got != want {
			t.Errorf("%s: got %s, want %s", cmd, got, want)
		}
	}
	if _, ok := (Rules{}).Match("gh pr checks 5 --web"); ok {
		t.Fatal("--web must pass")
	}
}

func TestMatchGlab(t *testing.T) {
	for cmd, want := range map[string]string{
		"glab ci status --live -b main": "oversire ci wait main",
		"glab ci view":                  "oversire ci status",
		"glab ci view dev -R g/p":       "oversire ci status dev --repo g/p",
		"glab pipeline status":          "oversire ci status",
	} {
		if got := replacement(t, cmd); got != want {
			t.Errorf("%s: got %s, want %s", cmd, got, want)
		}
	}
}

func TestMatchCompound(t *testing.T) {
	for _, cmd := range []string{
		"cd x && FOO=1 /opt/homebrew/bin/gh run view 2 | tail -5",
		"git push; gh run watch 3",
		"echo $(gh run list)",
		"echo `gh run list`",
		"sleep 1 &\ngh run watch",
		"env FOO=1 gh run watch 1 2>&1",
		"timeout 5 true; time gh pr checks",
		"if gh run view 1; then echo ok; fi",
		"gh run watch 1 > out.txt",
	} {
		if _, ok := (Rules{}).Match(cmd); !ok {
			t.Errorf("no match: %q", cmd)
		}
	}
	for _, cmd := range []string{
		`echo "gh run watch"`,
		`echo 'gh run watch' # gh run watch`,
		"git commit -m \"$(cat <<'EOF'\ngh run watch is refused\nEOF\n)\"",
		"cat <<EOF\ngh run list\nEOF",
	} {
		if r, ok := (Rules{}).Match(cmd); ok {
			t.Errorf("match: %q -> %s", cmd, r.Replacement)
		}
	}
}

func TestMatchMore(t *testing.T) {
	r, ok := Rules{}.Match("gh run watch 1 && gh run view 2; glab ci status")
	if !ok || r.Replacement != "oversire ci wait 1" || !strings.Contains(r.Reason, "2 more CI polls") {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"Use: oversire ci wait 1", "OVERSIRE_CI_REDIRECT=0 in front", "exit 3"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, r.Reason)
		}
	}
}

func TestMatchEscape(t *testing.T) {
	for _, cmd := range []string{"OVERSIRE_CI_REDIRECT=0 gh run watch 3", "FOO=1 OVERSIRE_CI_REDIRECT=0 gh run watch 3", "env OVERSIRE_CI_REDIRECT=0 gh run watch 3"} {
		if _, ok := (Rules{}).Match(cmd); ok {
			t.Errorf("match: %s", cmd)
		}
	}
}

func TestMatchOther(t *testing.T) {
	for _, cmd := range []string{"gh pr view 3", "gh run rerun 3", "git push", "gh run watch '3", `gh run "watch`, "", "gh", "gh run", "glab mr list", "command -v gh"} {
		if r, ok := (Rules{}).Match(cmd); ok {
			t.Errorf("match: %q -> %s", cmd, r.Replacement)
		}
	}
}
