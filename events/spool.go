package events

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oversire/cli/wire"
)

// MaxSpool is the size of the spool file at which a writer stops.
const MaxSpool = 5 << 20

// SpoolPath is <data>/events.spool.
func SpoolPath(data string) string { return filepath.Join(data, "events.spool") }

// Spool keeps drafts in a JSONL file when the daemon does not answer. The daemon reads it at the start.
type Spool struct{ path string }

func NewSpool(path string) *Spool { return &Spool{path: path} }

// Write appends the drafts. A full spool drops them.
func (s *Spool) Write(drafts []wire.EventDraft) error {
	if st, err := os.Stat(s.path); err == nil && st.Size() > MaxSpool {
		return errors.New("events: the spool is full")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, d := range drafts {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	return nil
}

// Drain appends the spooled drafts to the store and removes the file. A bad line is skipped.
// The file moves aside first, so that a writer that runs now makes a new file.
func (s *Spool) Drain(ctx context.Context, store *Store) (int, error) {
	work := s.path + ".draining"
	if err := os.Rename(s.path, work); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	f, err := os.Open(work)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var drafts []wire.EventDraft
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), MaxSpool)
	for sc.Scan() {
		var d wire.EventDraft
		if json.Unmarshal(sc.Bytes(), &d) != nil {
			continue
		}
		if _, err := Normalize(d); err == nil {
			drafts = append(drafts, d)
		}
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return 0, err
	}
	rows, err := store.Append(ctx, drafts)
	if err != nil {
		return 0, err
	}
	return len(rows), os.Remove(work)
}
