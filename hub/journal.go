package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/oversire/cli/wire"
)

// JournalSize is the number of finished orders that the journal keeps.
const JournalSize = 500

// readSize is the number of read-only results that the journal keeps in memory.
const readSize = 50

type entry struct {
	Result wire.Result `json:"result"`
	Sent   bool        `json:"sent"`
}

// Journal is the local file of finished orders. A re-sent order that is in it does not run again.
// A read-only order (sessionRead, sessionHistory) stays only in memory: it can run again with no effect (OVERSIRE-427).
type Journal struct {
	path string

	mu      sync.Mutex
	entries []entry
	reads   []entry
	version int

	wmu     sync.Mutex // holds the file writes in version order
	written int
}

// OpenJournal reads the file. A missing or bad file gives an empty journal.
// It drops the sent read results that an older daemon wrote: they have lines or a digest.
func OpenJournal(path string) *Journal {
	j := &Journal{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &j.entries)
	}
	j.entries = slices.DeleteFunc(j.entries, func(e entry) bool {
		r := e.Result.Result
		return e.Sent && r != nil && (r.Lines != nil || r.Digest != nil)
	})
	return j
}

func (j *Journal) Get(order string) (wire.Result, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, list := range [][]entry{j.entries, j.reads} {
		for _, e := range list {
			if e.Result.Order == order {
				return e.Result, true
			}
		}
	}
	return wire.Result{}, false
}

// Put keeps the result of an order of op. An empty op is an order that the runner could not read.
func (j *Journal) Put(r wire.Result, op wire.Op) {
	j.mu.Lock()
	if readOnly(op) {
		j.reads = append(j.reads, entry{Result: r})
		if n := len(j.reads) - readSize; n > 0 {
			j.reads = j.reads[n:]
		}
		j.mu.Unlock()
		return
	}
	j.entries = append(j.entries, entry{Result: r})
	if n := len(j.entries) - JournalSize; n > 0 {
		j.entries = j.entries[n:]
	}
	j.save()
}

func (j *Journal) MarkSent(order string) {
	j.mu.Lock()
	for i := range j.reads {
		if j.reads[i].Result.Order == order {
			j.reads[i].Sent = true
		}
	}
	i := slices.IndexFunc(j.entries, func(e entry) bool { return e.Result.Order == order })
	if i < 0 || j.entries[i].Sent {
		j.mu.Unlock()
		return
	}
	j.entries[i].Sent = true
	j.save()
}

// Unsent are the results that no hub has had yet.
func (j *Journal) Unsent() []wire.Result {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []wire.Result
	for _, list := range [][]entry{j.entries, j.reads} {
		for _, e := range list {
			if !e.Sent {
				out = append(out, e.Result)
			}
		}
	}
	return out
}

// save writes the file. Call it with j.mu: it unlocks j.mu before the write.
func (j *Journal) save() {
	b, err := json.Marshal(j.entries)
	j.version++
	v := j.version
	j.mu.Unlock()
	if err != nil {
		return
	}
	j.wmu.Lock()
	defer j.wmu.Unlock()
	if v <= j.written {
		return // a newer state is in the file
	}
	j.written = v
	_ = writeFile(j.path, b)
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readOnly tells an op that changes nothing.
func readOnly(op wire.Op) bool {
	return op == wire.OpSessionRead || op == wire.OpSessionHistory
}
