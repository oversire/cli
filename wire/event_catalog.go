package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"unicode/utf8"
)

// EventKindSpec is one entry of contract/schema/event-kinds.json.
type EventKindSpec struct {
	Kind       string      `json:"kind"`
	Domain     string      `json:"domain"`
	ActorTypes []ActorType `json:"actorTypes"`
	Private    bool        `json:"private"`
	Summary    string      `json:"summary"`
	Payload    struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"payload"`
}

// EventCatalog is contract/schema/event-kinds.json. Tests check rows against it.
type EventCatalog map[string]EventKindSpec

// LoadEventCatalog reads the catalog of this checkout.
func LoadEventCatalog() (EventCatalog, error) {
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "contract", "schema", "event-kinds.json"))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Kinds []EventKindSpec `json:"kinds"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	c := EventCatalog{}
	for _, k := range doc.Kinds {
		c[k.Kind] = k
	}
	return c, nil
}

// Problems tells why a row does not follow the catalog. Empty when it does.
func (c EventCatalog) Problems(d EventDraft) []string {
	k, ok := c[d.Kind]
	if !ok {
		return []string{fmt.Sprintf("kind %s is not in event-kinds.json", d.Kind)}
	}
	var out []string
	if !slices.Contains(k.ActorTypes, d.ActorType) {
		out = append(out, fmt.Sprintf("%s: actor type %s", d.Kind, d.ActorType))
	}
	if k.Private != d.Private {
		out = append(out, fmt.Sprintf("%s: private must be %v", d.Kind, k.Private))
	}
	var payload map[string]json.RawMessage
	if len(d.Payload) > 0 {
		if err := json.Unmarshal(d.Payload, &payload); err != nil {
			out = append(out, fmt.Sprintf("%s: payload is not an object", d.Kind))
		}
	}
	for f := range payload {
		if _, ok := k.Payload.Properties[f]; !ok {
			out = append(out, fmt.Sprintf("%s: payload field %s", d.Kind, f))
		}
	}
	for _, f := range k.Payload.Required {
		if _, ok := payload[f]; !ok {
			out = append(out, fmt.Sprintf("%s: payload needs %s", d.Kind, f))
		}
	}
	if utf8.RuneCountInString(d.Summary) > MaxEventSummary {
		out = append(out, fmt.Sprintf("%s: summary too long", d.Kind))
	}
	if len(d.Payload) > MaxEventPayload {
		out = append(out, fmt.Sprintf("%s: payload over %d bytes", d.Kind, MaxEventPayload))
	}
	return out
}
