// Package ci answers the CI reads of the agents (OVERSIRE-544). The daemon reads the CI docs of the mirror feed that the
// source app writes. It makes no vendor call. A miss goes to the source app as one member call.
package ci

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oversire/cli/wire"
	_ "modernc.org/sqlite"
)

const (
	runsKeep      = 7 * 24 * time.Hour
	logsKeep      = 7 * 24 * time.Hour
	logOpenKeep   = 2 * time.Minute
	workflowsKeep = 24 * time.Hour
)

// Book keeps what the mirror does not keep: the runs history, the logs and the workflows (<data>/ci.db).
// The mirror holds only the last runs of each repo.
type Book struct {
	db  *sql.DB
	now func() time.Time
}

// CachedLog is the lines of one job log that the daemon got from the app.
type CachedLog struct {
	Repo, Job, Run string
	Lines          []string
	Complete       bool
	Fetched        time.Time
}

// RunQuery picks runs. Empty fields match all. Index maps PR to SHA or Branch before it asks the Book.
type RunQuery struct {
	Repo, Branch, SHA, Workflow string
	PR                          int
	State                       wire.CIRollupState
	Limit                       int
}

// Open opens or makes the book at path.
func Open(path string, now func() time.Time) (*Book, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (repo TEXT NOT NULL, id TEXT NOT NULL, attempt INTEGER NOT NULL, body TEXT NOT NULL,
			updated INTEGER NOT NULL, PRIMARY KEY (repo, id));
		CREATE INDEX IF NOT EXISTS runs_updated ON runs(repo, updated);
		CREATE TABLE IF NOT EXISTS logs (repo TEXT NOT NULL, job TEXT NOT NULL, run TEXT NOT NULL, lines TEXT NOT NULL,
			complete INTEGER NOT NULL, fetched INTEGER NOT NULL, PRIMARY KEY (repo, job));
		CREATE TABLE IF NOT EXISTS workflows (repo TEXT PRIMARY KEY, body TEXT NOT NULL, fetched INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("ci: open %s: %w", path, err)
	}
	return &Book{db: db, now: now}, nil
}

func (b *Book) Close() error { return b.db.Close() }

// PutRuns stores the runs of a repo. A run replaces the stored run with the same id unless the stored attempt is newer.
func (b *Book) PutRuns(repo string, runs []wire.CIRun) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range runs {
		updated := parseTime(r.UpdatedAt)
		if updated.IsZero() {
			updated = b.now()
		}
		body, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO runs (repo, id, attempt, body, updated) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(repo, id) DO UPDATE SET attempt = excluded.attempt, body = excluded.body, updated = excluded.updated
			WHERE excluded.attempt >= runs.attempt`, repo, r.ID, max(r.Attempt, 1), string(body), updated.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanRuns(rows *sql.Rows) ([]wire.CIRun, error) {
	defer rows.Close()
	var out []wire.CIRun
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var r wire.CIRun
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Runs gives the runs that match q, newest first by updatedAt.
func (b *Book) Runs(q RunQuery) ([]wire.CIRun, error) {
	rows, err := b.db.Query("SELECT body FROM runs WHERE repo = ? ORDER BY updated DESC, id DESC", q.Repo)
	if err != nil {
		return nil, err
	}
	all, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	var out []wire.CIRun
	for _, r := range all {
		if q.matches(r) {
			out = append(out, r)
			if q.Limit > 0 && len(out) == q.Limit {
				break
			}
		}
	}
	return out, nil
}

func (q RunQuery) matches(r wire.CIRun) bool {
	switch {
	case q.Branch != "" && r.HeadBranch != q.Branch,
		q.SHA != "" && !strings.EqualFold(r.HeadSha, q.SHA),
		q.Workflow != "" && !strings.EqualFold(workflowOf(r), q.Workflow):
		return false
	}
	if q.State != "" {
		s, _ := runSet{runView(r)}.state(nil)
		return s == q.State
	}
	return true
}

// Run gives one run by id.
func (b *Book) Run(repo, id string) (wire.CIRun, bool, error) {
	rows, err := b.db.Query("SELECT body FROM runs WHERE repo = ? AND id = ?", repo, id)
	if err != nil {
		return wire.CIRun{}, false, err
	}
	runs, err := scanRuns(rows)
	if err != nil || len(runs) == 0 {
		return wire.CIRun{}, false, err
	}
	return runs[0], true, nil
}

// Log gives the cached log of a job.
func (b *Book) Log(repo, job string) (CachedLog, bool, error) {
	l := CachedLog{Repo: repo, Job: job}
	var lines string
	var fetched int64
	err := b.db.QueryRow("SELECT run, lines, complete, fetched FROM logs WHERE repo = ? AND job = ?", repo, job).
		Scan(&l.Run, &lines, &l.Complete, &fetched)
	if err == sql.ErrNoRows {
		return CachedLog{}, false, nil
	}
	if err != nil {
		return CachedLog{}, false, err
	}
	if err := json.Unmarshal([]byte(lines), &l.Lines); err != nil {
		return CachedLog{}, false, err
	}
	l.Fetched = time.UnixMilli(fetched).UTC()
	return l, true, nil
}

func (b *Book) PutLog(l CachedLog) error {
	lines, err := json.Marshal(slices.Clone(l.Lines))
	if err != nil {
		return err
	}
	_, err = b.db.Exec(`INSERT INTO logs (repo, job, run, lines, complete, fetched) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo, job) DO UPDATE SET run = excluded.run, lines = excluded.lines, complete = excluded.complete, fetched = excluded.fetched`,
		l.Repo, l.Job, l.Run, string(lines), l.Complete, l.Fetched.UnixMilli())
	return err
}

// Workflows gives the cached workflows of a repo.
func (b *Book) Workflows(repo string) (wire.CIWorkflows, bool, error) {
	var body string
	err := b.db.QueryRow("SELECT body FROM workflows WHERE repo = ?", repo).Scan(&body)
	if err == sql.ErrNoRows {
		return wire.CIWorkflows{}, false, nil
	}
	if err != nil {
		return wire.CIWorkflows{}, false, err
	}
	var w wire.CIWorkflows
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		return wire.CIWorkflows{}, false, err
	}
	return w, true, nil
}

func (b *Book) PutWorkflows(w wire.CIWorkflows) error {
	body, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = b.db.Exec(`INSERT INTO workflows (repo, body, fetched) VALUES (?, ?, ?)
		ON CONFLICT(repo) DO UPDATE SET body = excluded.body, fetched = excluded.fetched`, w.Repo, string(body), b.now().UnixMilli())
	return err
}

// Prune drops the runs and the complete logs that are older than 7 days, the logs that are not complete after 2 min,
// and the workflows that are older than 1 day.
func (b *Book) Prune(now time.Time) error {
	for _, q := range []struct {
		sql string
		age time.Duration
	}{
		{"DELETE FROM runs WHERE updated < ?", runsKeep},
		{"DELETE FROM logs WHERE complete = 1 AND fetched < ?", logsKeep},
		{"DELETE FROM logs WHERE complete = 0 AND fetched < ?", logOpenKeep},
		{"DELETE FROM workflows WHERE fetched < ?", workflowsKeep},
	} {
		if _, err := b.db.Exec(q.sql, now.Add(-q.age).UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}
