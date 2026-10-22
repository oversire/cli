package store

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// shape checks one JSON value (decoded with UseNumber) against a contract rule.
// The rules are a port of cloud/src/contract/decode.ts, for the records that the store checks.
type shape func(v any, path string) error

func fail(path, why string) error {
	if path == "" {
		path = "value"
	}
	return fmt.Errorf("%s: %s", path, why)
}

func str(v any, path string) error {
	if _, ok := v.(string); !ok {
		return fail(path, "not a string")
	}
	return nil
}

func nonEmpty(v any, path string) error {
	if s, ok := v.(string); !ok {
		return fail(path, "not a string")
	} else if s == "" {
		return fail(path, "empty")
	}
	return nil
}

func boolean(v any, path string) error {
	if _, ok := v.(bool); !ok {
		return fail(path, "not a boolean")
	}
	return nil
}

func text(lo, hi int) shape {
	return func(v any, path string) error {
		s, ok := v.(string)
		if !ok {
			return fail(path, "not a string")
		}
		if n := utf8.RuneCountInString(s); n < lo || n > hi {
			return fail(path, fmt.Sprintf("not %d to %d characters", lo, hi))
		}
		return nil
	}
}

// integer checks a whole number in [lo, hi].
func integer(lo, hi float64) shape {
	return func(v any, path string) error {
		n, ok := v.(json.Number)
		f, err := n.Float64()
		if !ok || err != nil || math.Trunc(f) != f || f < lo || f > hi {
			return fail(path, fmt.Sprintf("not an integer >= %v", lo))
		}
		return nil
	}
}

func oneOf(values ...string) shape {
	return func(v any, path string) error {
		if s, ok := v.(string); !ok || !slices.Contains(values, s) {
			return fail(path, "not one of "+strings.Join(values, ", "))
		}
		return nil
	}
}

func pattern(re *regexp.Regexp, why string) shape {
	return func(v any, path string) error {
		if s, ok := v.(string); !ok {
			return fail(path, "not a string")
		} else if !re.MatchString(s) {
			return fail(path, why)
		}
		return nil
	}
}

var (
	date       = pattern(regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`), "not an ISO 8601 date-time")
	providerID = pattern(regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`), "not a provider id")
	area       = pattern(regexp.MustCompile(`^[a-z0-9-]{1,32}$`), "not an area")
	hlcPattern = regexp.MustCompile(`^[0-9]{15}-[0-9]{6}-.+$`)
)

func uri(v any, path string) error {
	s, ok := v.(string)
	if !ok {
		return fail(path, "not a string")
	}
	if u, err := url.Parse(s); err != nil || u.Scheme == "" {
		return fail(path, "not a URI")
	}
	return nil
}

func arrayOf(item shape) shape {
	return func(v any, path string) error {
		a, ok := v.([]any)
		if !ok {
			return fail(path, "not an array")
		}
		for i, x := range a {
			if err := item(x, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
}

func maxItems(n int, item shape) shape {
	all := arrayOf(item)
	return func(v any, path string) error {
		if err := all(v, path); err != nil {
			return err
		}
		if len(v.([]any)) > n {
			return fail(path, fmt.Sprintf("more than %d items", n))
		}
		return nil
	}
}

func mapOf(item shape) shape {
	return func(v any, path string) error {
		o, ok := v.(map[string]any)
		if !ok {
			return fail(path, "not an object")
		}
		for k, x := range o {
			if err := item(x, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	}
}

func orNull(s shape) shape {
	return func(v any, path string) error {
		if v == nil {
			return nil
		}
		return s(v, path)
	}
}

// fields are the fields of an object. A name that ends in "?" is optional.
type fields map[string]shape

// object checks an object with no unknown fields.
func object(f fields) shape {
	byName, optional := map[string]shape{}, map[string]bool{}
	for k, s := range f {
		name, opt := strings.CutSuffix(k, "?")
		byName[name], optional[name] = s, opt
	}
	names := slices.Sorted(maps.Keys(byName))
	return func(v any, path string) error {
		o, ok := v.(map[string]any)
		if !ok {
			return fail(path, "not an object")
		}
		for k := range o {
			if _, ok := byName[k]; !ok {
				return fail(path+"."+k, "unknown field")
			}
		}
		for _, k := range names {
			s := byName[k]
			x, ok := o[k]
			if !ok {
				if optional[k] {
					continue
				}
				return fail(path+"."+k, "missing")
			}
			if err := s(x, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	}
}

// tagged checks a union that a string field selects.
func tagged(tag string, cases map[string]shape) shape {
	return func(v any, path string) error {
		o, ok := v.(map[string]any)
		if !ok {
			return fail(path, "not an object")
		}
		t, ok := o[tag].(string)
		if !ok {
			return fail(path+"."+tag, "not a string")
		}
		c, ok := cases[t]
		if !ok {
			return fail(path+"."+tag, "unknown "+tag+" "+t)
		}
		return c(v, path)
	}
}

var (
	accountID    = object(fields{"provider": providerID, "id": nonEmpty})
	projectRef   = object(fields{"account": accountID, "id": str, "key?": str})
	itemRef      = object(fields{"project": projectRef, "id": str, "key": str, "url": uri})
	repoRef      = object(fields{"account": accountID, "path": str, "url": uri})
	changeRef    = object(fields{"repo": repoRef, "number": integer(math.Inf(-1), math.Inf(1)), "url": uri})
	runRef       = object(fields{"account": accountID, "repo?": repoRef, "id": str, "attempt": integer(1, math.Inf(1))})
	jobRef       = object(fields{"run": runRef, "id": str})
	workspaceRef = object(fields{"account": accountID, "id": str})
	sessionRef   = object(fields{"workspace": workspaceRef, "id": str})

	subjectRef = tagged("kind", map[string]shape{
		"project":   subject("project", projectRef),
		"item":      subject("item", itemRef),
		"repo":      subject("repo", repoRef),
		"change":    subject("change", changeRef),
		"run":       subject("run", runRef),
		"job":       subject("job", jobRef),
		"workspace": subject("workspace", workspaceRef),
		"session":   subject("session", sessionRef),
	})

	areas        = maxItems(6, area)
	itemRelation = object(fields{"kind": oneOf("blockedBy", "blocks", "relatesTo"), "item": itemRef})
	itemPatch    = object(fields{"title?": str, "status?": str, "priority?": str, "assignees?": arrayOf(str), "labels?": arrayOf(str),
		"parent?": str, "bodyMarkdown?": str})
	changeAction = tagged("type", map[string]shape{
		"approve":         object(fields{"type": oneOf("approve"), "body?": str}),
		"request-changes": object(fields{"type": oneOf("request-changes"), "body": str}),
		"comment":         object(fields{"type": oneOf("comment"), "body": str}),
		"merge":           object(fields{"type": oneOf("merge"), "method": oneOf("merge", "rebase", "squash")}),
		"close":           object(fields{"type": oneOf("close")}),
	})
	runAction = oneOf("rerun", "rerunFailed", "cancel")

	launchRequest = object(fields{"repo": str, "item?": itemRef, "name?": str, "note?": str, "branch?": str, "agent": str, "prompt": str,
		"environment?": str, "command?": str})
	historyBudget = object(fields{"earlier?": integer(0, math.Inf(1)), "full?": integer(0, math.Inf(1)), "maxChars?": integer(1, math.Inf(1))})
	runnerAnswer  = object(fields{"id": nonEmpty, "items": arrayOf(object(fields{"id": nonEmpty, "choices": arrayOf(str), "other?": str})), "note?": str})

	sessionPayloads = map[string]shape{
		"sessionStart": launchRequest,
		"sessionStop":  object(fields{"session": sessionRef}),
		"sessionRead":  object(fields{"session": sessionRef, "lines": integer(1, math.Inf(1))}),
		"sessionSend": object(fields{"session": sessionRef, "text": str, "permission?": object(fields{"id": nonEmpty, "allow": boolean}),
			"answer?": runnerAnswer}),
		"sessionHistory": object(fields{"session": sessionRef, "budget?": historyBudget}),
	}
	workOrderDraft = withSessionOrder(fields{"runtime": providerID, "runner?": str})

	memoryScope = oneOf("user", "project", "subject")
	memoryKind  = oneOf("fact", "vision")
	memoryDraft = object(fields{"scope": memoryScope, "scopeRef?": subjectRef, "text": text(1, 500), "kind?": memoryKind})

	reason    = text(0, 200)
	planDraft = object(fields{
		"order":     arrayOf(object(fields{"item": itemRef, "lane": nonEmpty, "after": orNull(itemRef), "reason": reason})),
		"relations": arrayOf(object(fields{"item": itemRef, "add": arrayOf(itemRelation), "remove": arrayOf(itemRelation), "reason": reason})),
		"areas":     arrayOf(object(fields{"item": itemRef, "add": areas, "remove": areas, "reason": reason})),
		"triggers":  arrayOf(object(fields{"kind": nonEmpty, "subject": subjectRef, "at": date})),
		"overlaps?": object(fields{"before": integer(0, math.Inf(1)), "after": integer(0, math.Inf(1))}),
		"kinds?":    mapOf(nonEmpty),
	})
	proposal = tagged("type", map[string]shape{
		"itemPatch":    object(fields{"type": oneOf("itemPatch"), "patch": itemPatch, "relations?": arrayOf(itemRelation)}),
		"changeAction": object(fields{"type": oneOf("changeAction"), "action": changeAction}),
		"runAction":    object(fields{"type": oneOf("runAction"), "action": runAction}),
		"workOrder":    object(fields{"type": oneOf("workOrder"), "order": workOrderDraft}),
		"memory":       object(fields{"type": oneOf("memory"), "memory": memoryDraft}),
		"plan":         object(fields{"type": oneOf("plan"), "plan": planDraft}),
	})

	eventFilter = object(fields{"kinds?": arrayOf(str), "actions?": arrayOf(str), "providers?": arrayOf(providerID)})
	jobTrigger  = tagged("type", map[string]shape{
		"schedule": object(fields{"type": oneOf("schedule"), "cron": nonEmpty}),
		"event":    object(fields{"type": oneOf("event"), "filter": eventFilter}),
		"manual":   object(fields{"type": oneOf("manual")}),
	})
)

// recordShapes are the record rules of the synced collections that have one. The other synced collections take any object.
var recordShapes = map[string]shape{
	"memory": object(fields{"id": nonEmpty, "scope": memoryScope, "scopeRef?": subjectRef, "text": text(1, 500), "kind?": memoryKind,
		"source": oneOf("user", "suggestion"), "suggestion?": str, "updatedAt": date}),
	"jobs": object(fields{"id": nonEmpty, "kind": str, "trigger": jobTrigger, "scope": arrayOf(subjectRef), "enabled": boolean,
		"autonomy": oneOf("suggest"), "lastRun?": str}),
	"suggestions": object(fields{"id": nonEmpty, "subject": subjectRef, "kind": str, "title": str, "body": str, "proposal?": proposal,
		"state": oneOf("open", "accepted", "dismissed", "expired", "done", "failed"), "dedupeKey": nonEmpty, "subjectVersion": str,
		"createdAt": date, "expiresAt": date, "decidedAt?": date, "decidedBy?": str, "jobRun?": str, "error?": str,
		"edit?": object(fields{"drop": arrayOf(integer(0, math.Inf(1)))})}),
	"item-areas": object(fields{"id": nonEmpty, "item": itemRef, "areas": areas, "source": oneOf("user", "suggestion"), "suggestion?": str,
		"updatedAt": date}),
	"planner-settings": object(fields{"project": nonEmpty, "mode": nonEmpty, "holdLimit": integer(60, math.Inf(1)), "triggers?": arrayOf(str),
		"sharedFeature": oneOf("serial", "parallel"), "slots": integer(1, 10), "updatedAt": date}),
	"area-catalog": object(fields{"project": nonEmpty, "updatedAt": date,
		"areas": arrayOf(object(fields{"name": area, "kind": nonEmpty, "proposed?": boolean,
			"rules": arrayOf(object(fields{"glob": text(1, 200), "source": oneOf("user", "learned", "graph")}))}))}),
	"area-candidates": object(fields{"project": nonEmpty, "at": date,
		"hubs":     maxItems(50, object(fields{"glob": nonEmpty, "importers": integer(0, math.Inf(1))})),
		"clusters": maxItems(50, object(fields{"name": area, "globs": maxItems(20, nonEmpty)}))}),
	"changed-paths": object(fields{"id": nonEmpty, "subject": subjectRef, "paths": maxItems(500, nonEmpty), "at": date}),
}

func subject(kind string, ref shape) shape {
	return object(fields{"kind": oneOf(kind), "ref": ref})
}

// withSessionOrder checks an object that holds a SessionOrder ("op" and "payload") next to its own fields.
func withSessionOrder(own fields) shape {
	ops := slices.Sorted(maps.Keys(sessionPayloads))
	return func(v any, path string) error {
		o, ok := v.(map[string]any)
		if !ok {
			return fail(path, "not an object")
		}
		op, _ := o["op"].(string)
		payload, ok := sessionPayloads[op]
		if !ok {
			return fail(path+".op", "not one of "+strings.Join(ops, ", "))
		}
		f := fields{"op": oneOf(op), "payload": payload}
		for k, s := range own {
			f[k] = s
		}
		return object(f)(v, path)
	}
}
