package ticket

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var keyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-\d+$`)

// NormalKey gives the key in upper case, or an error when it is not a ticket key.
func NormalKey(key string) (string, error) {
	k := strings.ToUpper(strings.TrimSpace(key))
	if !keyPattern.MatchString(k) {
		return "", fmt.Errorf("%q is not a ticket key (for example OVERSIRE-12)", key)
	}
	return k, nil
}

// SessionRecord is one agent session that worked on a ticket.
type SessionRecord struct {
	Key     string    `json:"key"`
	Harness string    `json:"harness"`
	Session string    `json:"session"`
	Machine string    `json:"machine"`
	Cwd     string    `json:"cwd"`
	Role    string    `json:"role"` // new, plan or work
	At      time.Time `json:"at"`
	Done    bool      `json:"done,omitempty"`
}

type ledgerFile struct {
	Key      string          `json:"key"`
	Sessions []SessionRecord `json:"sessions"`
}

// Ledger keeps the sessions of each ticket on this machine, one file <KEY>.json for each ticket.
type Ledger struct{ dir string }

func NewLedger(dir string) *Ledger { return &Ledger{dir: dir} }

func (l *Ledger) path(key string) string { return filepath.Join(l.dir, key+".json") }

func (l *Ledger) read(key string) (ledgerFile, error) {
	f := ledgerFile{Key: key}
	b, err := os.ReadFile(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

func (l *Ledger) save(f ledgerFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(l.path(f.Key), append(b, '\n'))
}

// Add records a session. The same session and role again only moves At.
func (l *Ledger) Add(r SessionRecord) error {
	f, err := l.read(r.Key)
	if err != nil {
		return err
	}
	for i, s := range f.Sessions {
		if s.Session == r.Session && s.Harness == r.Harness && s.Role == r.Role {
			f.Sessions[i].At, f.Sessions[i].Cwd = r.At, r.Cwd
			return l.save(f)
		}
	}
	f.Sessions = append(f.Sessions, r)
	return l.save(f)
}

// Sessions gives the sessions of a ticket, oldest first.
func (l *Ledger) Sessions(key string) ([]SessionRecord, error) {
	f, err := l.read(key)
	sort.SliceStable(f.Sessions, func(i, j int) bool { return f.Sessions[i].At.Before(f.Sessions[j].At) })
	return f.Sessions, err
}

// Items gives the open tickets that a session is bound to, sorted.
func (l *Ledger) Items(session string) ([]string, error) {
	if session == "" {
		return nil, nil
	}
	names, err := filepath.Glob(filepath.Join(l.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		f, err := l.read(strings.TrimSuffix(filepath.Base(n), ".json"))
		if err != nil {
			continue
		}
		for _, s := range f.Sessions {
			if s.Session == session && !s.Done {
				out = append(out, f.Key)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Close marks the records of a session on a ticket as done.
func (l *Ledger) Close(key, session string) error {
	f, err := l.read(key)
	if err != nil {
		return err
	}
	for i := range f.Sessions {
		if f.Sessions[i].Session == session {
			f.Sessions[i].Done = true
		}
	}
	return l.save(f)
}
