package questions

import (
	"strconv"
	"strings"

	"github.com/oversire/cli/wire"
)

// Keys of the terminal.
const (
	KeyEnter = "\r"
	KeyDown  = "\x1b[B"
	KeyTab   = "\t"
	KeyEsc   = "\x1b"
)

// Step is one key of a form answer, and the screen that the key needs.
type Step struct {
	Question *string      // the question of the tab; nil: the review page
	Cursor   string       // the start of the cursor row, for example "4." or "Next|Submit"; "" for any
	Boxes    map[int]bool // the boxes of a multi question before the key
	Keys     string
}

// Holds tells whether the form on the screen is at this step.
func (s Step) Holds(f Form) bool {
	if s.Question != nil {
		if f.Review || len(f.Items) != 1 || flat(f.Items[0].Text) != flat(*s.Question) {
			return false
		}
	} else if !f.Review {
		return false
	}
	if s.Cursor != "" {
		ok := false
		for _, c := range strings.Split(s.Cursor, "|") {
			ok = ok || strings.HasPrefix(f.Cursor, c)
		}
		if !ok {
			return false
		}
	}
	for n, on := range s.Boxes {
		if got, ok := f.Boxes[n]; !ok || got != on {
			return false
		}
	}
	return true
}

// FormKeys gives the keys that answer an AskUserQuestion form, one step for each key.
// Recorded on Claude Code 2.1.308: a number picks a single option, or toggles a multi option and leaves the cursor.
// The number of "Type something" moves the cursor there in a single question. Then the text goes into that row.
// A form with a tab bar ends on a review page. page: q is one page of a form from the screen; its review page is another question.
func FormKeys(q wire.DaemonQuestion, a wire.QuestionAnswer, page bool) []Step {
	if len(q.Items) > 0 && q.Items[0].ID == ReviewID {
		item := q.Items[0]
		for _, g := range a.Items {
			if g.ID != item.ID || len(g.Choices) == 0 {
				continue
			}
			if n := labelIndex(item, g.Choices[0]); n > 0 {
				return []Step{{Keys: strconv.Itoa(n)}}
			}
		}
		return nil
	}
	var steps []Step
	multi := false
	for i, item := range q.Items {
		given := wire.QuestionAnswerItem{ID: item.ID}
		for _, g := range a.Items {
			if g.ID == item.ID {
				given = g
				break
			}
		}
		other := oneLine(given.Other)
		text := item.Text
		at := func(cursor string, boxes map[int]bool, keys string) Step {
			return Step{Question: &text, Cursor: cursor, Boxes: boxes, Keys: keys}
		}
		n := len(item.Options)
		typed := n + 1
		var picked []int
		for _, c := range given.Choices {
			if k := labelIndex(item, c); k > 0 {
				picked = append(picked, k)
			}
		}
		switch item.Kind {
		case wire.QuestionFree:
			steps = append(steps, at("", nil, other), at("", nil, KeyEnter))
		case wire.QuestionMulti:
			multi = true
			on := picked
			if other != "" {
				on = append(append([]int{}, picked...), typed)
			}
			for _, k := range on {
				steps = append(steps, at("", map[int]bool{k: false}, strconv.Itoa(k)))
			}
			rows := n
			if item.AllowsOther {
				rows++
			}
			if other == "" {
				for k := 1; k <= rows; k++ {
					steps = append(steps, at(strconv.Itoa(k)+".", nil, KeyDown))
				}
			} else {
				for k := 1; k < typed; k++ {
					steps = append(steps, at(strconv.Itoa(k)+".", nil, KeyDown))
				}
				steps = append(steps, at(strconv.Itoa(typed)+".", nil, other), at(strconv.Itoa(typed)+".", nil, KeyDown))
			}
			boxes := map[int]bool{}
			for k := 1; k <= rows; k++ {
				boxes[k] = contains(on, k)
			}
			last := "Next"
			if page {
				last = "Next|Submit"
			} else if i == len(q.Items)-1 {
				last = "Submit"
			}
			steps = append(steps, at(last, boxes, KeyEnter))
		default: // single and approve
			if len(picked) > 0 {
				steps = append(steps, at("", nil, strconv.Itoa(picked[0])))
			} else {
				c := strconv.Itoa(typed) + "."
				steps = append(steps, at("", nil, strconv.Itoa(typed)), at(c, nil, other), at(c, nil, KeyEnter))
			}
		}
	}
	if !page && (len(q.Items) > 1 || multi) {
		steps = append(steps, Step{Cursor: "1. Submit answers", Keys: "1"})
	}
	return steps
}

// PromptStep is one key of a prompt answer.
type PromptStep struct {
	Cursor   string // the start of the cursor row, for example "4."; "" for any
	Amending *bool  // the state of the "No" text of a permission; nil for either
	Keys     string
}

func (s PromptStep) Holds(p Prompt) bool {
	if s.Cursor != "" && !strings.HasPrefix(p.Cursor, s.Cursor) {
		return false
	}
	return s.Amending == nil || *s.Amending == p.Amending
}

// PromptKeys gives the keys that answer a prompt, one step for each key.
// A text for a permission: Down to "No", Tab, the text, Enter. A text for a plan: the number moves the cursor, the text, Enter.
func PromptKeys(p Prompt, a wire.QuestionAnswerItem) []PromptStep {
	no, yes := false, true
	other := oneLine(a.Other)
	if len(a.Choices) > 0 {
		for _, o := range p.Options {
			if o.Label == a.Choices[0] {
				return []PromptStep{{Amending: &no, Keys: strconv.Itoa(o.N)}}
			}
		}
	}
	if p.Typed == 0 {
		return nil
	}
	t := strconv.Itoa(p.Typed)
	if p.Kind == PromptPlan {
		return []PromptStep{{Keys: t}, {Cursor: t + ".", Keys: other}, {Cursor: t + ".", Keys: KeyEnter}}
	}
	var steps []PromptStep
	for k := 1; k < p.Typed; k++ {
		steps = append(steps, PromptStep{Cursor: strconv.Itoa(k) + ".", Amending: &no, Keys: KeyDown})
	}
	return append(steps, PromptStep{Cursor: t + ".", Amending: &no, Keys: KeyTab}, PromptStep{Cursor: t + ".", Amending: &yes, Keys: other},
		PromptStep{Cursor: t + ".", Amending: &yes, Keys: KeyEnter})
}

// oneLine joins the lines of a text with one space. A new line in the text would submit early.
func oneLine(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029' || r == '\v' || r == '\f' || r == '\u0085'
	})
	return trim(strings.Join(parts, " "))
}

// labelIndex gives the 1-based number of the option with label, or 0.
func labelIndex(item wire.DaemonQuestionItem, label string) int {
	for i, o := range item.Options {
		if o.Label == label {
			return i + 1
		}
	}
	return 0
}

func contains(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}
