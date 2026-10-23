package questions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// Mail is the mail of the coordinator of the supervised workers (orca.Orca).
type Mail interface {
	Mail(ctx context.Context) (runtime.Mail, error)
	Reply(ctx context.Context, ask, body string) error
}

// askBody is the body of an ask: {"round":N,"questions":[...]}. A body that is not this JSON is one free question.
type askBody struct {
	Round     int `json:"round"`
	Questions []struct {
		ID          string            `json:"id"`
		Header      string            `json:"header"`
		Text        string            `json:"text"`
		Why         string            `json:"why"`
		Type        string            `json:"type"`
		Options     []json.RawMessage `json:"options"`
		AllowsOther *bool             `json:"allowsOther"`
	} `json:"questions"`
}

// parseAsk gives the round and the items of an ask body. plain is true for a body that is one free question.
func parseAsk(body string) (round int, items []wire.DaemonQuestionItem, plain bool) {
	free := []wire.DaemonQuestionItem{{ID: ItemReply, Text: body, Kind: wire.QuestionFree, Options: []wire.QuestionOption{}}}
	var b askBody
	if json.Unmarshal([]byte(body), &b) != nil || len(b.Questions) == 0 {
		return 1, free, true
	}
	for i, q := range b.Questions {
		if q.Text == "" {
			return 1, free, true
		}
		item := wire.DaemonQuestionItem{ID: q.ID, Header: q.Header, Text: q.Text, Why: q.Why, Options: []wire.QuestionOption{}}
		if item.ID == "" {
			item.ID = fmt.Sprintf("q%d", i+1)
		}
		for _, raw := range q.Options {
			var o wire.QuestionOption
			var label string
			if json.Unmarshal(raw, &label) == nil {
				o.Label = label
			} else if json.Unmarshal(raw, &o) != nil {
				continue
			}
			if o.Label != "" {
				item.Options = append(item.Options, o)
			}
		}
		switch k := wire.QuestionKind(q.Type); {
		case len(item.Options) == 0:
			item.Kind = wire.QuestionFree
		case k == wire.QuestionSingle || k == wire.QuestionMulti:
			item.Kind = k
		default:
			item.Kind = wire.QuestionSingle
		}
		item.AllowsOther = len(item.Options) > 0
		if q.AllowsOther != nil && len(item.Options) > 0 {
			item.AllowsOther = *q.AllowsOther
		}
		items = append(items, item)
	}
	if b.Round < 1 {
		b.Round = 1
	}
	return b.Round, items, false
}

// replyBody is the reply to an ask: {"round":N,"answers":{id: text or [text]},"note":...}, or plain text for a plain ask.
func replyBody(ask string, a wire.QuestionAnswer) string {
	round, items, plain := parseAsk(ask)
	if plain {
		var parts []string
		if len(a.Items) > 0 {
			g := a.Items[0]
			text := strings.Join(append(append([]string{}, g.Choices...), nonEmpty(g.Other)...), ", ")
			parts = append(parts, nonEmpty(text)...)
		}
		return strings.Join(append(parts, nonEmpty(a.Note)...), "\n\n")
	}
	answers := map[string]any{}
	for _, item := range items {
		for _, g := range a.Items {
			if g.ID != item.ID {
				continue
			}
			if item.Kind == wire.QuestionMulti {
				answers[item.ID] = append(append([]string{}, g.Choices...), nonEmpty(g.Other)...)
			} else if g.Other != "" {
				answers[item.ID] = g.Other
			} else if len(g.Choices) > 0 {
				answers[item.ID] = g.Choices[0]
			} else {
				answers[item.ID] = ""
			}
		}
	}
	d := map[string]any{"round": round, "answers": answers}
	if a.Note != "" {
		d["note"] = a.Note
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(d)
	return strings.TrimSpace(buf.String())
}

// replyAnswer reads a reply as the answer to q. A text that is an option label is a choice; other text is the free text.
func replyAnswer(reply string, q wire.DaemonQuestion) wire.QuestionAnswer {
	var d struct {
		Answers map[string]json.RawMessage `json:"answers"`
		Note    string                     `json:"note"`
	}
	if json.Unmarshal([]byte(reply), &d) != nil || d.Answers == nil {
		id := ItemReply
		if len(q.Items) > 0 {
			id = q.Items[0].ID
		}
		return wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: id, Choices: []string{}, Other: reply}}}
	}
	out := wire.QuestionAnswer{ID: q.ID, Note: d.Note, Items: []wire.QuestionAnswerItem{}}
	for _, item := range q.Items {
		raw, ok := d.Answers[item.ID]
		if !ok {
			continue
		}
		var given []string
		var one string
		if json.Unmarshal(raw, &one) == nil {
			given = []string{one}
		} else if json.Unmarshal(raw, &given) != nil {
			continue
		}
		g := wire.QuestionAnswerItem{ID: item.ID, Choices: []string{}}
		var others []string
		for _, text := range given {
			if labelIndex(item, text) > 0 {
				g.Choices = append(g.Choices, text)
			} else {
				others = append(others, text)
			}
		}
		g.Other = strings.Join(others, ", ")
		out.Items = append(out.Items, g)
	}
	return out
}

func nonEmpty(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return []string{s}
}
