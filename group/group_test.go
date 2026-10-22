package group

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

// fakeSecrets is the keychain of a test.
type fakeSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func newSecrets() *fakeSecrets { return &fakeSecrets{m: map[string]string{}} }

func (f *fakeSecrets) Set(_ context.Context, account, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[account] = value
	return nil
}

func (f *fakeSecrets) Get(_ context.Context, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[account]
	if !ok {
		return "", errors.New("no secret")
	}
	return v, nil
}

func (f *fakeSecrets) Delete(_ context.Context, account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, account)
}

// fakeClock is a clock that a test moves.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func member(name string) wire.GroupMember {
	return wire.GroupMember{Machine: name, Host: name, Addrs: []string{"100.64.0.1"}, Port: 7690, Roles: []string{"serve"}}
}

func newTestStore(t *testing.T, clock *fakeClock, self string) (*Store, *fakeSecrets) {
	t.Helper()
	sec := newSecrets()
	s := NewStore(filepath.Join(t.TempDir(), "group.json"), sec, clock.Now, nil)
	if err := s.EnsureCreated(member(self)); err != nil {
		t.Fatal(err)
	}
	return s, sec
}

func TestCreate(t *testing.T) {
	clock := newClock()
	s, sec := newTestStore(t, clock, "mbp")
	g, ok := s.Snapshot()
	if !ok || len(g.ID) != 16 || g.Home != "mbp" || g.Name != DefaultName || g.Version != 1 || len(g.Members) != 1 || len(g.Devices) != 1 {
		t.Fatalf("%+v", g)
	}
	if d := g.Devices[0]; d.Kind != wire.DeviceDaemon || d.Machine != "mbp" || len(d.DeviceID) != 16 {
		t.Fatalf("%+v", d)
	}
	key, _, err := decodeKeys(sec.m[secretAccount])
	if err != nil || len(key) != 32 {
		t.Fatal("no secret in the keychain", err)
	}
	// The file has mode 0600 and no secret.
	st, err := os.Stat(s.path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal(st, err)
	}
	b, _ := os.ReadFile(s.path)
	if strings.Contains(string(b), sec.m[secretAccount]) {
		t.Fatal("secret in group.json")
	}

	// A second store loads the same group. EnsureCreated keeps it.
	s2 := NewStore(s.path, sec, clock.Now, nil)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if err := s2.EnsureCreated(member("mbp")); err != nil {
		t.Fatal(err)
	}
	g2, _ := s2.Snapshot()
	if g2.ID != g.ID || g2.Version != g.Version {
		t.Fatalf("%+v", g2)
	}
	if id, home := s2.Info(); id != g.ID || home != "mbp" {
		t.Fatal(id, home)
	}
	// A new address increases the version.
	m := member("mbp")
	m.Addrs = []string{"100.64.0.9"}
	_ = s2.EnsureCreated(m)
	if g3, _ := s2.Snapshot(); g3.Version != g.Version+1 || g3.Members[0].Addrs[0] != "100.64.0.9" {
		t.Fatalf("%+v", g3)
	}

	// Without the secret, the group is not loaded and a new one is made.
	sec.Delete(context.Background(), secretAccount)
	s3 := NewStore(s.path, sec, clock.Now, nil)
	_ = s3.Load()
	if _, ok := s3.Snapshot(); ok {
		t.Fatal("loaded without the secret")
	}
}

func TestCredential(t *testing.T) {
	clock := newClock()
	s, _ := newTestStore(t, clock, "mbp")
	other, _ := newTestStore(t, clock, "mba")

	cred, err := s.Issue(wire.GroupDevice{Name: "OverSire on mbp", Kind: wire.DeviceApp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cred.Token, "osd1.") || strings.Count(cred.Token, ".") != 2 {
		t.Fatal(cred.Token)
	}
	c, ok := s.Verify(cred.Token)
	if !ok || c.DeviceID != cred.DeviceID || c.Kind != "app" || c.Machine != "mbp" || c.Name != "OverSire on mbp" || c.Loopback {
		t.Fatalf("%+v %v", c, ok)
	}
	// The same app gets the same device again.
	again, _ := s.Issue(wire.GroupDevice{Name: "OverSire on mbp", Kind: wire.DeviceApp})
	if again != cred {
		t.Fatalf("%+v", again)
	}

	// A credential of another group.
	ocred, _ := other.Issue(wire.GroupDevice{Name: "x"})
	if _, ok := s.Verify(ocred.Token); ok {
		t.Fatal("other group accepted")
	}
	// A bad HMAC.
	parts := strings.Split(cred.Token, ".")
	bad := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, ok := s.Verify(bad); ok {
		t.Fatal("bad hmac accepted")
	}
	// A changed payload with the old HMAC.
	var p payload
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &p)
	p.K = "daemon"
	nb, _ := json.Marshal(p)
	if _, ok := s.Verify("osd1." + base64.RawURLEncoding.EncodeToString(nb) + "." + parts[2]); ok {
		t.Fatal("changed payload accepted")
	}
	for _, junk := range []string{"", "osd1.", "osd2." + parts[1] + "." + parts[2], "osd1.!!.??", cred.Token + "x"} {
		if _, ok := s.Verify(junk); ok {
			t.Fatal("junk accepted:", junk)
		}
	}

	// Revoked.
	if err := s.Remove(cred.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(cred.Token); ok {
		t.Fatal("revoked accepted")
	}
	select {
	case id := <-s.Revoked():
		if id != cred.DeviceID {
			t.Fatal(id)
		}
	default:
		t.Fatal("no revoked id")
	}
}

func TestRotate(t *testing.T) {
	clock := newClock()
	s, sec := newTestStore(t, clock, "mbp")
	cred, _ := s.Issue(wire.GroupDevice{Name: "app"})
	if err := s.Rotate(); err != nil {
		t.Fatal(err)
	}
	if _, prev, _ := decodeKeys(sec.m[secretAccount]); prev == nil {
		t.Fatal("no old secret kept")
	}
	fresh, _ := s.Credential(cred.DeviceID)
	if fresh.Token == cred.Token {
		t.Fatal("same token after rotate")
	}
	if _, ok := s.Verify(cred.Token); !ok {
		t.Fatal("old token refused in the grace time")
	}
	clock.Add(RotateGrace + time.Second)
	if _, ok := s.Verify(cred.Token); ok {
		t.Fatal("old token accepted after the grace time")
	}
	if _, ok := s.Verify(fresh.Token); !ok {
		t.Fatal("new token refused")
	}
}

func TestRemoveAndHome(t *testing.T) {
	clock := newClock()
	s, _ := newTestStore(t, clock, "mbp")
	res, err := s.Admit(wire.JoinRequest{Machine: "mba", Host: "mba", Kind: wire.DeviceDaemon, Nonce: "n"})
	if err != nil || res.Secret == "" {
		t.Fatal(err)
	}
	app, _ := s.Admit(wire.JoinRequest{Machine: "mini", Kind: wire.DeviceApp, Nonce: "n"})
	if app.Secret != "" {
		t.Fatal("an app got the secret")
	}
	if err := s.SetHome("mba"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHome("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Remove("mbp"); !errors.Is(err, ErrSelf) {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	if err := s.Remove("mba"); err != nil {
		t.Fatal(err)
	}
	g, _ := s.Snapshot()
	if g.Home != "mbp" || len(g.Members) != 1 || len(g.Removed) != 1 || len(g.Revoked) != 1 {
		t.Fatalf("%+v", g)
	}
	if _, ok := s.Verify(res.Credential.Token); ok {
		t.Fatal("removed machine accepted")
	}
	if _, ok := s.Verify(app.Credential.Token); !ok {
		t.Fatal("app refused")
	}
	if err := s.Remove("nothing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// The machine joins again later.
	clock.Add(time.Minute)
	res2, _ := s.Admit(wire.JoinRequest{Machine: "mba", Host: "mba", Kind: wire.DeviceDaemon, Nonce: "n"})
	if res2.Credential.DeviceID == res.Credential.DeviceID {
		t.Fatal("same device id after removal")
	}
	if _, ok := s.Verify(res2.Credential.Token); !ok {
		t.Fatal("rejoined machine refused")
	}
	if g, _ := s.Snapshot(); len(g.Members) != 2 {
		t.Fatalf("%+v", g)
	}
}

// The contract fixtures decode, and normalize keeps their content.
func TestFixtures(t *testing.T) {
	for _, f := range []string{"two-members", "with-revoked"} {
		b, err := os.ReadFile(filepath.Join("..", "contract", "fixtures", "group", f+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var g wire.Group
		if err := json.Unmarshal(b, &g); err != nil {
			t.Fatal(err)
		}
		n := normalize(g, time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC))
		if !sameContent(n, g) {
			t.Fatalf("%s: normalize changed the content", f)
		}
	}
}
