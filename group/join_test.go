package group

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/oversire/cli/wire"
)

type frames struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (f *frames) add(m any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := m.(map[string]any); ok {
		f.msgs = append(f.msgs, v)
	}
}

func (f *frames) last(typ string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.msgs) - 1; i >= 0; i-- {
		if f.msgs[i]["type"] == typ {
			return f.msgs[i]
		}
	}
	return nil
}

func joinReq(machine, code string) wire.JoinRequest {
	return wire.JoinRequest{Code: code, Machine: machine, Host: machine, Kind: wire.DeviceDaemon, Nonce: "0123456789abcdef"}
}

func TestCodes(t *testing.T) {
	if c := newCode(); len(c) != 8 || strings.Trim(c, codeAlphabet) != "" {
		t.Fatal(c)
	}
	if len(codeAlphabet) != 32 {
		t.Fatal("alphabet")
	}
	if NormalizeCode("k7q2-m9xd") != "K7Q2M9XD" || NormalizeCode("O1L I") != "0111" {
		t.Fatal(NormalizeCode("O1L I"))
	}
	if FormatCode("K7Q2M9XD") != "K7Q2-M9XD" {
		t.Fatal()
	}
}

func TestInvite(t *testing.T) {
	clock := newClock()
	s, _ := newTestStore(t, clock, "mbp")
	j := NewJoins(s, nil, clock.Now)

	inv, err := j.Invite()
	if err != nil || len(inv.Code) != 9 || inv.Via != "mbp" || !inv.ExpiresAt.Equal(clock.Now().Add(InviteTTL)) {
		t.Fatalf("%+v %v", inv, err)
	}
	if _, err := j.Join(joinReq("mba", "AAAA-AAAA")); !errors.Is(err, ErrBadCode) {
		t.Fatal(err)
	}
	res, err := j.Join(joinReq("mba", strings.ToLower(inv.Code)))
	if err != nil || res.Secret == "" || res.Credential.Token == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, ok := s.Verify(res.Credential.Token); !ok {
		t.Fatal("credential refused")
	}
	// One use.
	if _, err := j.Join(joinReq("mini", inv.Code)); !errors.Is(err, ErrBadCode) {
		t.Fatal(err)
	}
	// Expiry.
	inv2, _ := j.Invite()
	clock.Add(InviteTTL)
	if _, err := j.Join(joinReq("mini", inv2.Code)); !errors.Is(err, ErrBadCode) {
		t.Fatal(err)
	}
	// An app gets no secret.
	inv3, _ := j.Invite()
	app := joinReq("mini", inv3.Code)
	app.Kind = wire.DeviceApp
	if res, err := j.Join(app); err != nil || res.Secret != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRequests(t *testing.T) {
	clock := newClock()
	s, _ := newTestStore(t, clock, "mbp")
	var f frames
	j := NewJoins(s, f.add, clock.Now)

	p, err := j.Request(joinReq("mba", ""))
	if err != nil || len(p.Confirm) != 4 || !p.ExpiresAt.Equal(clock.Now().Add(RequestTTL)) {
		t.Fatalf("%+v %v", p, err)
	}
	fr := f.last("joinRequest")
	if fr == nil || fr["request"] != p.Request || fr["confirm"] != p.Confirm || fr["machine"] != "mba" {
		t.Fatalf("%+v", fr)
	}
	if l := j.Pending(); len(l) != 1 || l[0].Request != p.Request || l[0].Confirm != p.Confirm {
		t.Fatalf("%+v", l)
	}
	// The wrong nonce.
	if _, _, err := j.Poll(p.Request, "other-nonce"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, st, err := j.Poll(p.Request, "0123456789abcdef"); err != nil || st != StatusPending {
		t.Fatal(st, err)
	}
	if err := j.Decide(p.Request, true); err != nil {
		t.Fatal(err)
	}
	res, st, err := j.Poll(p.Request, "0123456789abcdef")
	if err != nil || st != StatusApproved || res.Secret == "" {
		t.Fatal(st, err)
	}
	if err := j.Decide(p.Request, false); !errors.Is(err, ErrDecided) {
		t.Fatal(err)
	}

	// Deny.
	p2, _ := j.Request(joinReq("mini", ""))
	if err := j.Decide(p2.Request, false); err != nil {
		t.Fatal(err)
	}
	if _, st, _ := j.Poll(p2.Request, "0123456789abcdef"); st != StatusDenied {
		t.Fatal(st)
	}

	// Approve after expiry.
	p3, _ := j.Request(joinReq("studio", ""))
	clock.Add(RequestTTL)
	if err := j.Decide(p3.Request, true); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	if _, st, _ := j.Poll(p3.Request, "0123456789abcdef"); st != StatusExpired {
		t.Fatal(st)
	}
	if err := j.Decide("r-none", true); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// At most MaxOpen open requests.
	for i := range MaxOpen {
		if _, err := j.Request(joinReq("m"+string(rune('a'+i)), "")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.Request(joinReq("mz", "")); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	// A bad request.
	if _, err := j.Request(wire.JoinRequest{Machine: "x", Nonce: "short"}); !errors.Is(err, ErrBadInput) {
		t.Fatal(err)
	}
}

func TestRateLimit(t *testing.T) {
	clock := newClock()
	s, _ := newTestStore(t, clock, "mbp")
	j := NewJoins(s, nil, clock.Now)
	for range RateLimit {
		if !j.allow("100.64.0.2:5555") {
			t.Fatal("refused too early")
		}
	}
	if j.allow("100.64.0.2:6666") {
		t.Fatal("not limited")
	}
	if !j.allow("100.64.0.3:5555") {
		t.Fatal("other peer limited")
	}
	clock.Add(RateWindow)
	if !j.allow("100.64.0.2:5555") {
		t.Fatal("still limited after the window")
	}
}

func TestOfferNeedsAlone(t *testing.T) {
	clock := newClock()
	s, _ := newTestStore(t, clock, "mbp")
	j := NewJoins(s, nil, clock.Now)
	if _, err := j.Offer(); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Admit(joinReq("mba", ""))
	if _, err := j.Offer(); !errors.Is(err, ErrInGroup) {
		t.Fatal(err)
	}
	if _, err := j.TakeOffer(t.Context(), wire.GroupInvite{Code: "AAAA-AAAA", Via: "x"}); !errors.Is(err, ErrInGroup) {
		t.Fatal(err)
	}
}
