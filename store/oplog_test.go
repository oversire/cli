package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

var start = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

func fixture(t *testing.T, kind, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "contract", "fixtures", kind, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func storeOp(t *testing.T, name string) wire.StoreOp {
	t.Helper()
	var op wire.StoreOp
	if err := wire.Decode(fixture(t, "store-op", name), &op); err != nil {
		t.Fatal(err)
	}
	return op
}

func hlc(ms int64, counter int, device string) string {
	return fmt.Sprintf("%015d-%06d-%s", ms, counter, device)
}

func openLog(t *testing.T) *OpLog {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "store.db"), func() time.Time { return start })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func mustApply(t *testing.T, l *OpLog, ops ...wire.StoreOp) wire.StoreApplyResult {
	t.Helper()
	res, err := l.Apply(ops)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mustPull(t *testing.T, l *OpLog, after, limit int) wire.StorePull {
	t.Helper()
	p, err := l.Pull(after, limit)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ids(ops []wire.StoreOp) []string {
	out := []string{}
	for _, o := range ops {
		out = append(out, o.ID)
	}
	return out
}

func sameOps(t *testing.T, want, got []wire.StoreOp) {
	t.Helper()
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	var x, y any
	json.Unmarshal(a, &x)
	json.Unmarshal(b, &y)
	if fmt.Sprint(x) != fmt.Sprint(y) {
		t.Fatalf("want %s\ngot  %s", a, b)
	}
}

// The tests below run the sequences of cloud/test/store.test.ts and expect the same answers.

func TestKeepsTheLastWrite(t *testing.T) {
	l := openLog(t)
	a, b := storeOp(t, "conflict-a.json"), storeOp(t, "conflict-b.json")
	first := mustApply(t, l, b)
	sameOps(t, []wire.StoreOp{b}, first.Accepted)
	if first.Head != 1 {
		t.Fatal(first.Head)
	}
	if res := mustApply(t, l, a); len(res.Accepted) != 0 || res.Head != 1 {
		t.Fatalf("an older hlc loses: %+v", res)
	}
	p := mustPull(t, l, 0, 0)
	if p.Cursor != 1 || p.More || p.Reset {
		t.Fatalf("%+v", p)
	}
	sameOps(t, []wire.StoreOp{b}, p.Ops)
	if p := mustPull(t, l, 1, 0); len(p.Ops) != 0 || p.Cursor != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestPagesAndMovesTheCursor(t *testing.T) {
	l := openLog(t)
	put := storeOp(t, "put.json")
	var list []wire.StoreOp
	for i := 1; i <= 3; i++ {
		op := put
		op.ID, op.HLC = fmt.Sprintf("n%d", i), hlc(1_790_000_000_000+int64(i), 0, "mac")
		op.Record = json.RawMessage(strings.Replace(string(put.Record), `"id": "p1"`, fmt.Sprintf(`"id": "n%d"`, i), 1))
		list = append(list, op)
	}
	mustApply(t, l, list...)
	one := mustPull(t, l, 0, 2)
	if one.Cursor != 2 || !one.More || !slices.Equal(ids(one.Ops), []string{"n1", "n2"}) {
		t.Fatalf("%+v", one)
	}
	two := mustPull(t, l, 2, 2)
	if two.Cursor != 3 || two.More || !slices.Equal(ids(two.Ops), []string{"n3"}) {
		t.Fatalf("%+v", two)
	}
}

func TestRejectsBadBatches(t *testing.T) {
	put := storeOp(t, "put.json")
	del := storeOp(t, "delete.json")
	var patch map[string]any
	json.Unmarshal(fixture(t, "suggestion", "item-patch.json"), &patch)
	patch["state"] = "maybe"
	bad, _ := json.Marshal(patch)

	body := func(ops ...any) []byte { b, _ := json.Marshal(map[string]any{"ops": ops}); return b }
	with := func(f func(*wire.StoreOp)) wire.StoreOp { op := put; f(&op); return op }
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"schema", body(with(func(o *wire.StoreOp) { o.Schema = 9 })), "unknown store schema 9"},
		{"local collection", body(with(func(o *wire.StoreOp) { o.Collection = "queue-lane-state" })), "ops[0]: queue-lane-state does not sync"},
		{"hub only", body(with(func(o *wire.StoreOp) { o.Collection = "work-orders" })), "ops[0]: only the hub writes work-orders"},
		{"bad record", body(wire.StoreOp{Schema: 2, Collection: "suggestions", ID: "s", Op: wire.StorePut, Record: bad, HLC: put.HLC, Device: "mac"}),
			"ops[0].record.state: not one of open, accepted, dismissed, expired, done, failed"},
		{"delete with record", body(with(func(o *wire.StoreOp) { o.Op = wire.StoreDelete })), "ops[0]: a delete has no record"},
		{"put without record", body(with(func(o *wire.StoreOp) { o.Record = nil })), "ops[0]: a put needs a record"},
		{"bad hlc", body(with(func(o *wire.StoreOp) { o.HLC = "now" })), "ops[0].hlc: not a hybrid clock"},
		{"unknown field", []byte(`{"ops":[{"schema":2,"collection":"pins","id":"p","op":"delete","hlc":"` + del.HLC + `","device":"d","x":1}]}`), "unknown field"},
		{"too many", body(slices.Repeat([]any{del}, MaxOps+1)...), "ops must be an array of at most 100"},
		{"no ops", []byte(`{}`), "ops must be an array of at most 100"},
	}
	for _, c := range cases {
		_, err := DecodeOps(c.body)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := DecodeOps(body(with(func(o *wire.StoreOp) { o.Schema = 1 }), del)); err != nil {
		t.Fatal("the schema before is still good:", err)
	}
}

func TestPrunesTombstonesAndResets(t *testing.T) {
	l := openLog(t)
	del := storeOp(t, "delete.json")
	del2 := del
	del2.ID = "p2"
	mustApply(t, l, storeOp(t, "conflict-a.json"), del, del2)
	if p := mustPull(t, l, 0, 0); len(p.Ops) != 3 {
		t.Fatalf("%+v", p)
	}
	if err := l.Prune(start.Add(TombstoneTTL - time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p := mustPull(t, l, 1, 0); p.Reset || len(p.Ops) != 2 {
		t.Fatalf("a young tombstone stays: %+v", p)
	}
	if err := l.Prune(start.Add(TombstoneTTL + 24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	fresh := mustPull(t, l, 0, 0)
	if fresh.Reset || !slices.Equal(ids(fresh.Ops), []string{"n1"}) || fresh.Cursor != 3 {
		t.Fatalf("a read from the start is no reset: %+v", fresh)
	}
	behind := mustPull(t, l, 1, 0)
	if !behind.Reset || !slices.Equal(ids(behind.Ops), []string{"n1"}) || behind.Cursor != 3 {
		t.Fatalf("%+v", behind)
	}
	if p := mustPull(t, l, 3, 0); p.Reset || len(p.Ops) != 0 {
		t.Fatalf("a client at the head gets no reset: %+v", p)
	}
	if l.Head() != 3 {
		t.Fatal("seq never goes back:", l.Head())
	}
	var n int
	l.db.QueryRow("SELECT COUNT(*) FROM ops").Scan(&n)
	if n != 1 {
		t.Fatal("the log keeps only the rows of live records:", n)
	}
	// The next write gets seq 4, not a used number.
	op := storeOp(t, "put.json")
	op.ID = "p3"
	if res := mustApply(t, l, op); res.Head != 4 {
		t.Fatal(res.Head)
	}
}

func TestHLCOrder(t *testing.T) {
	l := openLog(t)
	put := storeOp(t, "put.json")
	at := func(ms int64, counter int, device string) wire.StoreOp {
		op := put
		op.HLC = hlc(ms, counter, device)
		return op
	}
	steps := []struct {
		op   wire.StoreOp
		wins bool
	}{
		{at(1000, 0, "mac"), true},
		{at(1000, 0, "mac"), false}, // the same stamp loses
		{at(999, 9, "phone"), false},
		{at(1000, 1, "phone"), true}, // the counter breaks a tie of the time
		{at(1000, 1, "zz"), true},    // the device breaks a tie of the counter
		{wire.StoreOp{Schema: 2, Collection: "pins", ID: "p1", Op: wire.StoreDelete, HLC: hlc(2000, 0, "mac"), Device: "mac"}, true},
		{at(1500, 0, "mac"), false}, // a late put does not undo a newer delete
	}
	for i, s := range steps {
		if got := len(mustApply(t, l, s.op).Accepted) == 1; got != s.wins {
			t.Fatalf("step %d: wins %v", i, got)
		}
	}
	p := mustPull(t, l, 0, 0)
	if len(p.Ops) != 1 || p.Ops[0].Op != wire.StoreDelete || p.Ops[0].Record != nil || p.Cursor != 4 {
		t.Fatalf("%+v", p)
	}
}

func TestReopenKeepsTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	l, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, l, storeOp(t, "put.json"))
	l.Close()
	l, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Head() != 1 || len(mustPull(t, l, 0, 0).Ops) != 1 {
		t.Fatal("lost the log")
	}
}

// Each record fixture of a synced collection passes the record rules, like the decoders of the cloud.
func TestRecordFixturesPass(t *testing.T) {
	kinds := map[string]string{
		"suggestion": "suggestions", "memory-entry": "memory", "job": "jobs", "item-areas": "item-areas", "planner-settings": "planner-settings",
		"area-catalog": "area-catalog", "area-candidates": "area-candidates", "changed-paths": "changed-paths", "pin": "pins", "note": "notes",
		"snooze": "snoozes", "queue-lane": "queue-lanes", "agent-link": "agent-links", "ignored-failure": "ignored-failures",
		"queue-trigger": "queue-triggers",
	}
	for kind, collection := range kinds {
		paths, _ := filepath.Glob(filepath.Join("..", "contract", "fixtures", kind, "*.json"))
		if len(paths) == 0 {
			t.Fatal("no fixtures for", kind)
		}
		for _, p := range paths {
			if strings.HasPrefix(filepath.Base(p), "v1-") {
				continue // a schema 1 record that the cloud does not accept now
			}
			rec, _ := os.ReadFile(p)
			op := wire.StoreOp{Schema: 2, Collection: collection, ID: "x", Op: wire.StorePut, Record: rec, HLC: hlc(1, 0, "mac"), Device: "mac"}
			if err := check(op, "op"); err != nil {
				t.Errorf("%s/%s: %v", kind, filepath.Base(p), err)
			}
		}
	}
}

func TestOpenMakesTheFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "share", "oversire")
	l, err := Open(filepath.Join(dir, "store.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if st, err := os.Stat(dir); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o700) {
		t.Fatalf("folder: %v %v", st, err)
	}
}
