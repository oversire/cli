package questions

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/oversire/cli/wire"
)

// TranscriptTail is the number of transcript bytes that one read looks at.
const TranscriptTail = 512 * 1024

// Ask is an AskUserQuestion call in a Claude transcript or hook, and its answers when it has a result.
type Ask struct {
	ID      string
	Items   []wire.DaemonQuestionItem
	Answers map[string]string // question text to answer text; nil while the call is open
	Notes   []string
}

func (a Ask) Open() bool { return a.Answers == nil }

// askInput is the tool_input of AskUserQuestion.
type askInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

func (in askInput) items() []wire.DaemonQuestionItem {
	out := make([]wire.DaemonQuestionItem, 0, len(in.Questions))
	for i, q := range in.Questions {
		item := wire.DaemonQuestionItem{ID: "q" + strconv.Itoa(i+1), Header: q.Header, Text: q.Question, Kind: wire.QuestionFree,
			Options: []wire.QuestionOption{}}
		for _, o := range q.Options {
			if o.Label != "" {
				item.Options = append(item.Options, wire.QuestionOption{Label: o.Label, Detail: o.Description})
			}
		}
		if len(item.Options) > 0 {
			item.Kind, item.AllowsOther = wire.QuestionSingle, true
			if q.MultiSelect {
				item.Kind = wire.QuestionMulti
			}
		}
		out = append(out, item)
	}
	return out
}

// askResult is the toolUseResult of AskUserQuestion in a transcript, and the tool_response of its PostToolUse hook.
type askResult struct {
	Answers     map[string]string `json:"answers"`
	Annotations map[string]struct {
		Notes string `json:"notes"`
	} `json:"annotations"`
}

func (r askResult) notes() []string {
	var out []string
	for _, a := range r.Annotations {
		if a.Notes != "" {
			out = append(out, a.Notes)
		}
	}
	sort.Strings(out)
	return out
}

type transcriptLine struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// blocks gives the content blocks of each line. A line that is not JSON is skipped, so the data can start in a line.
func blocks(jsonl []byte, each func(line transcriptLine, b block)) {
	for _, raw := range bytes.Split(jsonl, []byte("\n")) {
		var l transcriptLine
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		var bs []block
		if json.Unmarshal(l.Message.Content, &bs) != nil {
			continue
		}
		for _, b := range bs {
			each(l, b)
		}
	}
}

// LastAsk gives the last AskUserQuestion call in a Claude transcript.
func LastAsk(jsonl []byte) (Ask, bool) {
	var ask Ask
	found := false
	blocks(jsonl, func(l transcriptLine, b block) {
		switch {
		case b.Type == "tool_use" && b.Name == "AskUserQuestion" && b.ID != "":
			var in askInput
			_ = json.Unmarshal(b.Input, &in)
			ask, found = Ask{ID: b.ID, Items: in.items()}, true
		case b.Type == "tool_result" && found && b.ToolUseID == ask.ID:
			var r askResult
			_ = json.Unmarshal(l.ToolUseResult, &r)
			if r.Answers == nil {
				r.Answers = map[string]string{}
			}
			ask.Answers, ask.Notes = r.Answers, r.notes()
		}
	})
	return ask, found
}

// Matches tells whether the screen shows this call: the same headers, or the same question text.
func (a Ask) Matches(f Form) bool {
	var headers []string
	texts := map[string]bool{}
	for _, it := range a.Items {
		if it.Header != "" {
			headers = append(headers, it.Header)
		}
		texts[flat(it.Text)] = true
	}
	if len(f.Headers) > 0 && strings.Join(f.Headers, "\x00") == strings.Join(headers, "\x00") {
		return true
	}
	if len(f.Items) == 0 {
		return false
	}
	for _, it := range f.Items {
		if !texts[flat(it.Text)] {
			return false
		}
	}
	return true
}

// Answer gives the answers of the result, or nil while the call is open.
// Claude joins the labels with ", ". A text that is no label is the free text.
func (a Ask) Answer(id string) *wire.QuestionAnswer {
	if a.Answers == nil {
		return nil
	}
	out := &wire.QuestionAnswer{ID: id, Items: []wire.QuestionAnswerItem{}, Note: strings.Join(a.Notes, "\n")}
	for _, it := range a.Items {
		given, ok := a.Answers[it.Text]
		if !ok {
			continue
		}
		labels := map[string]bool{}
		for _, o := range it.Options {
			labels[o.Label] = true
		}
		parts := strings.Split(given, ", ")
		unknown := false
		for _, p := range parts {
			unknown = unknown || !labels[p]
		}
		switch {
		case labels[given]:
			out.Items = append(out.Items, wire.QuestionAnswerItem{ID: it.ID, Choices: []string{given}})
		case unknown:
			out.Items = append(out.Items, wire.QuestionAnswerItem{ID: it.ID, Choices: []string{}, Other: given})
		default:
			out.Items = append(out.Items, wire.QuestionAnswerItem{ID: it.ID, Choices: parts})
		}
	}
	return out
}

// OpenTool gives the id of the last call of tool in a Claude transcript that has no result yet.
// input is the start of the input that the screen or the hook shows. A string of the call must start with it.
func OpenTool(tool, input string, jsonl []byte) string {
	start := []rune(input)
	if len(start) > 40 {
		start = start[:40]
	}
	open := ""
	blocks(jsonl, func(_ transcriptLine, b block) {
		if b.Type == "tool_use" && b.Name == tool {
			open = ""
			var in map[string]any
			_ = json.Unmarshal(b.Input, &in)
			for _, v := range in {
				if s, ok := v.(string); ok && strings.HasPrefix(s, string(start)) {
					open = b.ID
					break
				}
			}
			if len(start) == 0 {
				open = b.ID
			}
		}
		if b.Type == "tool_result" && open != "" && b.ToolUseID == open {
			open = ""
		}
	})
	return open
}
