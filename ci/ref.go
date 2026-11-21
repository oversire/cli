package ci

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type RefKind string

const (
	RefSHA    RefKind = "sha"
	RefBranch RefKind = "branch"
	RefPR     RefKind = "pr"
	RefRun    RefKind = "run"
)

// Ref is what the user typed to name a commit, a branch, a PR or a run.
type Ref struct {
	Kind  RefKind
	Value string
	PR    int
}

var (
	hexRef    = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	digitsRef = regexp.MustCompile(`^[0-9]+$`)
	runURL    = regexp.MustCompile(`/actions/runs/([0-9]+)`)
	pullURL   = regexp.MustCompile(`/pull/([0-9]+)`)
)

// ParseRef reads a ref: #123, a run or PR URL, a run id, a SHA (7 to 40 hex chars) or a branch.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("the ref is empty")
	}
	if n, ok := strings.CutPrefix(s, "#"); ok && digitsRef.MatchString(n) {
		pr, _ := strconv.Atoi(n)
		return Ref{Kind: RefPR, Value: n, PR: pr}, nil
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		if m := runURL.FindStringSubmatch(u.Path); m != nil {
			return Ref{Kind: RefRun, Value: m[1]}, nil
		}
		if m := pullURL.FindStringSubmatch(u.Path); m != nil {
			pr, _ := strconv.Atoi(m[1])
			return Ref{Kind: RefPR, Value: m[1], PR: pr}, nil
		}
	}
	switch {
	case digitsRef.MatchString(s):
		return Ref{Kind: RefRun, Value: s}, nil
	case hexRef.MatchString(s):
		return Ref{Kind: RefSHA, Value: strings.ToLower(s)}, nil
	}
	return Ref{Kind: RefBranch, Value: s}, nil
}
