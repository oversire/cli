package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

type fakeChecker struct {
	r   Result
	err error
	n   int
}

func (f *fakeChecker) Check(context.Context) (Result, error) { f.n++; return f.r, f.err }

var at = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

func TestLoopKeepsTheFailure(t *testing.T) {
	f := &fakeChecker{r: Result{Latest: "0.2.2"}, err: errors.New("update: no write access to /usr/local/bin")}
	l := NewLoop(f, func() time.Time { return at })
	if l.Status() != nil {
		t.Fatal("status before a check")
	}
	_, _ = l.Check(context.Background())
	want := wire.UpdateStatus{CheckedAt: at, Latest: "0.2.2", Error: "update: no write access to /usr/local/bin"}
	if s := l.Status(); s == nil || *s != want {
		t.Fatalf("%+v", s)
	}
	// A later good check clears the error.
	f.err = nil
	_, _ = l.Check(context.Background())
	if s := l.Status(); s.Error != "" || s.Latest != "0.2.2" {
		t.Fatalf("%+v", s)
	}
}

func TestLoopDevBuild(t *testing.T) {
	l := NewLoop(&fakeChecker{err: ErrDev}, nil)
	if s, err := l.Check(context.Background()); !errors.Is(err, ErrDev) || s.Error == "" || l.Status() != nil {
		t.Fatal(s, err, l.Status())
	}
}

func TestUpdateRoute(t *testing.T) {
	f := &fakeChecker{r: Result{Latest: "0.2.2", Installed: "0.2.2"}}
	l := NewLoop(f, func() time.Time { return at })
	r := l.Routes()[0]
	if r.Pattern != "POST /v1/update" || r.Access != server.Member {
		t.Fatalf("%+v", r)
	}
	w := httptest.NewRecorder()
	r.Handle(w, httptest.NewRequest(http.MethodPost, "/v1/update", nil))
	var got wire.UpdateStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Installed != "0.2.2" || !got.CheckedAt.Equal(at) {
		t.Fatal(w.Body.String(), err)
	}
	select {
	case v := <-l.Installed():
		if v != "0.2.2" {
			t.Fatal(v)
		}
	default:
		t.Fatal("no restart after the install")
	}
}

func TestLoopHint(t *testing.T) {
	f := &fakeChecker{r: Result{Latest: "0.2.2", Installed: "0.2.2"}}
	l := NewLoop(f, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.Hint()
	go l.Run(ctx)
	select {
	case <-l.Installed():
	case <-ctx.Done():
		t.Fatal("the hint made no check")
	}
}
