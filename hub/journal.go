package hub

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

// JournalSize is the number of finished orders that the journal keeps.
const JournalSize = 500

type entry struct {
	Result wire.Result `json:"result"`
	Sent   bool        `json:"sent"`
}

// Journal is the local file of finished orders. A re-sent order that is in it does not run again.
type Journal struct {
	path    string
	mu      sync.Mutex
	entries []entry
}

// OpenJournal reads the file. A missing or bad file gives an empty journal.
func OpenJournal(path string) *Journal {
	j := &Journal{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &j.entries)
	}
	return j
}

func (j *Journal) Get(order string) (wire.Result, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range j.entries {
		if e.Result.Order == order {
			return e.Result, true
		}
	}
	return wire.Result{}, false
}

func (j *Journal) Put(r wire.Result) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, entry{Result: r})
	if n := len(j.entries) - JournalSize; n > 0 {
		j.entries = j.entries[n:]
	}
	j.save()
}

func (j *Journal) MarkSent(order string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := range j.entries {
		if j.entries[i].Result.Order == order {
			j.entries[i].Sent = true
		}
	}
	j.save()
}

// Unsent are the results that no hub has had yet.
func (j *Journal) Unsent() []wire.Result {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []wire.Result
	for _, e := range j.entries {
		if !e.Sent {
			out = append(out, e.Result)
		}
	}
	return out
}

func (j *Journal) save() { _ = config.WriteJSON(j.path, j.entries) }
