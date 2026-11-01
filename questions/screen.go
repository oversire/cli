// Package questions finds the pauses of the agent sessions on this machine and answers them.
// The screen parsers are in agent-wire/tui. They read the screens in contract/fixtures/claude-screens
// like ClaudeForm and ClaudePrompt of the app, and must agree with them.
package questions

import (
	"strings"
	"unicode"

	"github.com/trebi-ai/agent-wire/tui"

	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// ReviewID is the id of the item that submits or cancels the review page of a form.
const ReviewID = tui.ReviewID

// Form is the AskUserQuestion form of Claude Code, read from the screen of a terminal as a best effort.
type Form struct {
	tui.Form
	Items []wire.DaemonQuestionItem // the items of tui.Form
}

// ParseForm gives false when no form is open at the bottom of the screen.
func ParseForm(screen []string) (Form, bool) {
	f, ok := tui.ParseForm(screen)
	if !ok {
		return Form{}, false
	}
	items := make([]wire.DaemonQuestionItem, len(f.Items))
	for i, it := range f.Items {
		items[i] = wireItem(it)
	}
	return Form{Form: f, Items: items}, true
}

// ReviewItem is the review page as one choice, for a form from the screen only.
func (f Form) ReviewItem() wire.DaemonQuestionItem { return wireItem(f.Form.ReviewItem()) }

const PromptPlan = tui.PromptPlan

// Prompt is a tool permission prompt or the plan approval of Claude Code, read from the screen.
type Prompt struct{ tui.Prompt }

// ParsePrompt gives false when no prompt is open at the bottom of the screen.
func ParsePrompt(screen []string) (Prompt, bool) {
	p, ok := tui.ParsePrompt(screen)
	return Prompt{p}, ok
}

// Item is one choice item. The header names the tool, and the text is a short form of its input: "Bash" and "git push".
func (p Prompt) Item(id, tool, input string) wire.DaemonQuestionItem {
	return wireItem(p.Prompt.Item(id, tool, input))
}

// wireItem is a question item of the screen in the wire form.
func wireItem(it tui.Item) wire.DaemonQuestionItem {
	opts := make([]wire.QuestionOption, len(it.Options))
	for i, o := range it.Options {
		opts[i] = wire.QuestionOption{Label: o.Label, Detail: o.Detail}
	}
	return wire.DaemonQuestionItem{ID: it.ID, Header: it.Header, Text: it.Text, Why: it.Why,
		Kind: wire.QuestionKind(it.Kind), Options: opts, AllowsOther: it.AllowsOther}
}

// inputText is a short form of a tool input: its first line, and only the file name for a file tool.
func inputText(tool, input string) string {
	if input == "" {
		return ""
	}
	return tui.Prompt{}.Item("", tool, input).Text
}

// TurnQuestion gives the question at the end of a turn: the rule of AgentSession.question(in:).
func TurnQuestion(message string) (string, bool) {
	q := sessions.TurnQuestion(message)
	return q, q != ""
}

func trim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r != '\n' && unicode.IsSpace(r) })
}

// flat joins the words of s with one space.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }
