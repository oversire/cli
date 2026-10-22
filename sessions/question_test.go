package sessions

import "testing"

// The cases of TurnQuestionTests in the app.
func TestTurnQuestion(t *testing.T) {
	for in, want := range map[string]string{
		"notes.md has two lines. Do you want me to add a third line?":     "notes.md has two lines. Do you want me to add a third line?",
		"I fixed the test.\n\nShould I push the branch?\n":                "Should I push the branch?",
		"Done.\n\n**Do you want the PR now?**":                            "**Do you want the PR now?**",
		"Two ways:\n\n```swift\nlet a = 1\n```\n\nWhich one do you want?": "Which one do you want?",
		"":                                    "",
		"All tests pass.":                     "",
		"Is it fast? Yes. The run takes 2 s.": "",
		"The rule:\n\n```\nwhy?\n```":         "",
		"Run this:\n```\nls ?\n```\nThen it works.": "",
		"Should I go on?\n\n```sh\nmake\n```":       "",
	} {
		if got := TurnQuestion(in); got != want {
			t.Errorf("TurnQuestion(%q) = %q, want %q", in, got, want)
		}
	}
}
