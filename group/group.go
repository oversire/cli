// Package group keeps the "My machines" group: the roster, the group secret, the device credentials,
// the join flows and the roster sync between members.
package group

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	// DefaultName is the name of a new group.
	DefaultName = "My machines"
	// TombstoneTTL is the time that a removed machine stays in Removed.
	TombstoneTTL = 90 * 24 * time.Hour
	// RotateGrace is the time that the old secret stays valid after a rotate.
	RotateGrace = time.Hour

	tokenPrefix   = "osd1."
	secretAccount = "group"
	keySize       = 32
)

var (
	ErrNoGroup    = errors.New("group: this machine has no group")
	ErrOtherGroup = errors.New("group: the roster is of another group")
	ErrRemoved    = errors.New("group: this machine was removed from the group")
	ErrNotFound   = errors.New("group: no such machine or device")
	ErrSelf       = errors.New("group: this machine cannot remove itself")
	ErrConflict   = errors.New("group: a rotate on a machine with a lower name wins")
)

// Secrets keeps the group secret (secret.Store). The value goes to child processes on stdin only.
type Secrets interface {
	Set(ctx context.Context, account, value string) error
	Get(ctx context.Context, account string) (string, error)
	Delete(ctx context.Context, account string)
}

// stored is the content of group.json. The keys are not in it.
type stored struct {
	wire.Group
	Self      string     `json:"self,omitempty"`
	PrevUntil *time.Time `json:"prevUntil,omitempty"`
	Rekeying  bool       `json:"rekeying,omitempty"`
}

// Store holds the group of this machine. It implements server.Verifier.
type Store struct {
	path    string
	secrets Secrets
	clock   func() time.Time
	notify  func()

	mu        sync.Mutex
	g         *wire.Group
	self      wire.GroupMember
	key       []byte
	prev      []byte // the secret before the last rotate, valid until prevUntil
	prevUntil time.Time
	rekeying  bool // this machine rotated and pushes the new secret to the members

	revoked chan string
	changed chan struct{}
}

// NewStore makes a store for path (group.json). notify runs after each change of the group, also after a merge.
// It runs with the store lock held, so it must not call the store.
func NewStore(path string, secrets Secrets, clock func() time.Time, notify func()) *Store {
	if clock == nil {
		clock = time.Now
	}
	if notify == nil {
		notify = func() {}
	}
	return &Store{path: path, secrets: secrets, clock: clock, notify: notify,
		revoked: make(chan string, 64), changed: make(chan struct{}, 1)}
}

// Load reads group.json and the secret. A group without its secret is not loaded.
func (s *Store) Load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st stored
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("group: read %s: %w", s.path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := s.secrets.Get(ctx, secretAccount)
	if err != nil {
		return nil
	}
	key, prev, err := decodeKeys(v)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := normalize(st.Group, s.clock())
	s.g, s.key, s.prev, s.rekeying = &g, key, prev, st.Rekeying
	if st.PrevUntil != nil {
		s.prevUntil = *st.PrevUntil
	}
	if st.Self != "" {
		for _, m := range g.Members {
			if m.Machine == st.Self {
				s.self = m
			}
		}
	}
	return nil
}

// EnsureCreated makes a group of one when there is no group: a new secret, home is this machine, and self is the first member.
// With a group, it updates the self entry when its addresses changed.
func (s *Store) EnsureCreated(self wire.GroupMember) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureLocked(self)
}

func (s *Store) ensureLocked(self wire.GroupMember) error {
	now := s.clock()
	if self.JoinedAt.IsZero() {
		self.JoinedAt = now
	}
	s.self = self
	if s.g != nil {
		i := slices.IndexFunc(s.g.Members, func(m wire.GroupMember) bool { return m.Machine == self.Machine })
		if i < 0 {
			// The group does not have this machine any more: start a new group of one.
			s.dropLocked()
			return s.ensureLocked(self)
		}
		old := s.g.Members[i]
		self.JoinedAt, self.LastSeen = old.JoinedAt, old.LastSeen
		s.self = self
		if !sameMember(old, self) {
			s.g.Members[i] = self
			return s.commitLocked(true)
		}
		if s.selfDeviceLocked() == "" {
			s.addDeviceLocked(wire.GroupDevice{Name: daemonName(self.Machine), Kind: wire.DeviceDaemon, Machine: self.Machine})
			return s.commitLocked(true)
		}
		return nil
	}
	key := make([]byte, keySize)
	rand.Read(key)
	id := make([]byte, 8)
	rand.Read(id)
	g := wire.Group{ID: hex.EncodeToString(id), Name: DefaultName, Home: self.Machine, Members: []wire.GroupMember{self}}
	s.g, s.key, s.prev, s.prevUntil, s.rekeying = &g, key, nil, time.Time{}, false
	s.addDeviceLocked(wire.GroupDevice{Name: daemonName(self.Machine), Kind: wire.DeviceDaemon, Machine: self.Machine})
	if err := s.saveKeysLocked(); err != nil {
		s.g, s.key = nil, nil
		return err
	}
	return s.commitLocked(true)
}

// Snapshot gives a copy of the group. ok is false before the group exists.
func (s *Store) Snapshot() (wire.Group, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return wire.Group{}, false
	}
	return clone(*s.g), true
}

// Info gives the group id and the home machine for DaemonInfo. Both are empty before the group exists.
func (s *Store) Info() (id, home string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return "", ""
	}
	return s.g.ID, s.g.Home
}

// Self gives the member entry of this machine.
func (s *Store) Self() wire.GroupMember {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.self
}

// Alone tells if this machine has no group, or a group of one.
func (s *Store) Alone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.g == nil || len(s.g.Members) <= 1
}

// Revoked gives device ids when they are revoked, so that the server closes their sockets.
func (s *Store) Revoked() <-chan string { return s.revoked }

// Changed gets a value after a local change. The syncer then pokes the members.
func (s *Store) Changed() <-chan struct{} { return s.changed }

// Merge takes the roster of the member from. See the merge rules in merge.
func (s *Store) Merge(from string, remote wire.Group) (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return false, ErrNoGroup
	}
	if remote.ID != s.g.ID {
		return false, ErrOtherGroup
	}
	now := s.clock()
	out := merge(*s.g, remote, from, s.self.Machine, now)
	if !slices.ContainsFunc(out.Members, func(m wire.GroupMember) bool { return m.Machine == s.self.Machine }) {
		return false, ErrRemoved
	}
	if equal(out, *s.g) {
		return false, nil
	}
	fresh := newRevoked(s.g.Revoked, out.Revoked)
	s.g = &out
	if err := s.saveLocked(); err != nil {
		return true, err
	}
	for _, id := range fresh {
		s.sendRevoked(id)
	}
	s.notify()
	return true, nil
}

// Seen sets the last time that a pull from machine worked. It does not change the version.
func (s *Store) Seen(machine string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return
	}
	now := s.clock().UTC()
	for i := range s.g.Members {
		if s.g.Members[i].Machine == machine {
			s.g.Members[i].LastSeen = &now
		}
	}
}

// Issue adds a device and gives its credential. An active device with the same kind, machine and name gets its credential again.
func (s *Store) Issue(d wire.GroupDevice) (wire.DeviceCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return wire.DeviceCredential{}, ErrNoGroup
	}
	if d.Machine == "" {
		d.Machine = s.self.Machine
	}
	if d.Kind == "" {
		d.Kind = wire.DeviceApp
	}
	if d.Name == "" {
		d.Name = appName(d.Machine)
	}
	dev, added := s.addDeviceLocked(d)
	if added {
		if err := s.commitLocked(true); err != nil {
			return wire.DeviceCredential{}, err
		}
	}
	return wire.DeviceCredential{DeviceID: dev.DeviceID, Token: s.tokenLocked(dev, s.key)}, nil
}

// Credential gives the current credential of an active device.
func (s *Store) Credential(deviceID string) (wire.DeviceCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return wire.DeviceCredential{}, ErrNoGroup
	}
	for _, d := range s.g.Devices {
		if d.DeviceID == deviceID {
			return wire.DeviceCredential{DeviceID: d.DeviceID, Token: s.tokenLocked(d, s.key)}, nil
		}
	}
	return wire.DeviceCredential{}, ErrNotFound
}

// Tokens gives the credentials of this daemon to call other members: first the one that all members accept.
// During a rotate, the first one uses the old secret.
func (s *Store) Tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.selfDeviceLocked()
	if id == "" {
		return nil
	}
	i := slices.IndexFunc(s.g.Devices, func(d wire.GroupDevice) bool { return d.DeviceID == id })
	d := s.g.Devices[i]
	cur := s.tokenLocked(d, s.key)
	if s.prev != nil && s.clock().Before(s.prevUntil) {
		return []string{s.tokenLocked(d, s.prev), cur}
	}
	return []string{cur}
}

// Admit adds the machine or app of r and gives the join result. Only a daemon gets the secret.
func (s *Store) Admit(r wire.JoinRequest) (wire.JoinResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return wire.JoinResult{}, ErrNoGroup
	}
	if r.Machine == "" {
		return wire.JoinResult{}, errors.New("group: the request has no machine")
	}
	kind := r.Kind
	if kind == "" {
		kind = wire.DeviceDaemon
	}
	name := appName(r.Machine)
	if kind == wire.DeviceDaemon {
		if r.Machine == s.self.Machine {
			return wire.JoinResult{}, errors.New("group: the machine has the same name as this machine")
		}
		name = daemonName(r.Machine)
		m := wire.GroupMember{Machine: r.Machine, Host: r.Host, Addrs: r.Addrs, Port: r.Port, Roles: r.Roles, JoinedAt: s.clock().UTC()}
		if m.Host == "" {
			m.Host = r.Machine
		}
		s.g.Members = slices.DeleteFunc(s.g.Members, func(x wire.GroupMember) bool { return x.Machine == r.Machine })
		s.g.Members = append(s.g.Members, m)
	}
	dev, _ := s.addDeviceLocked(wire.GroupDevice{Name: name, Kind: kind, Machine: r.Machine})
	if err := s.commitLocked(true); err != nil {
		return wire.JoinResult{}, err
	}
	res := wire.JoinResult{Group: clone(*s.g), Credential: wire.DeviceCredential{DeviceID: dev.DeviceID, Token: s.tokenLocked(dev, s.key)}}
	if kind == wire.DeviceDaemon {
		res.Secret = base64.StdEncoding.EncodeToString(s.key)
	}
	return res, nil
}

// Adopt replaces the group of this machine with the group of a join result, and adds self as a member.
func (s *Store) Adopt(res wire.JoinResult) error {
	key, err := base64.StdEncoding.DecodeString(res.Secret)
	if err != nil || len(key) != keySize {
		return errors.New("group: the join result has no valid secret")
	}
	if res.Group.ID == "" {
		return errors.New("group: the join result has no group")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, oldKey := s.g, s.key
	g := normalize(clone(res.Group), s.clock())
	s.g, s.key, s.prev, s.prevUntil, s.rekeying = &g, key, nil, time.Time{}, false
	self := s.self
	self.JoinedAt = s.clock().UTC()
	if i := slices.IndexFunc(g.Members, func(m wire.GroupMember) bool { return m.Machine == self.Machine }); i >= 0 {
		self.JoinedAt = g.Members[i].JoinedAt
		g.Members[i] = self
	} else {
		g.Members = append(g.Members, self)
	}
	s.self = self
	if err := s.saveKeysLocked(); err != nil {
		s.g, s.key = old, oldKey
		return err
	}
	return s.commitLocked(true)
}

// Reset deletes the group and the secret, and makes a new group of one.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked()
	self := s.self
	self.JoinedAt, self.LastSeen = time.Time{}, nil
	return s.ensureLocked(self)
}

func (s *Store) dropLocked() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.secrets.Delete(ctx, secretAccount)
	_ = os.Remove(s.path)
	s.g, s.key, s.prev, s.prevUntil, s.rekeying = nil, nil, nil, time.Time{}, false
}

// Remove removes a machine (a tombstone, and its devices are revoked) or one device.
func (s *Store) Remove(machineOrDevice string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return ErrNoGroup
	}
	now := s.clock().UTC()
	machine := ""
	if slices.ContainsFunc(s.g.Members, func(m wire.GroupMember) bool { return m.Machine == machineOrDevice }) {
		machine = machineOrDevice
	} else if i := slices.IndexFunc(s.g.Devices, func(d wire.GroupDevice) bool { return d.DeviceID == machineOrDevice }); i >= 0 {
		d := s.g.Devices[i]
		if d.Kind == wire.DeviceDaemon && slices.ContainsFunc(s.g.Members, func(m wire.GroupMember) bool { return m.Machine == d.Machine }) {
			machine = d.Machine
		} else {
			s.revokeLocked(d.DeviceID)
		}
	} else {
		return ErrNotFound
	}
	if machine != "" {
		if machine == s.self.Machine {
			return ErrSelf
		}
		s.g.Removed = slices.DeleteFunc(s.g.Removed, func(r wire.RemovedMember) bool { return r.Machine == machine })
		s.g.Removed = append(s.g.Removed, wire.RemovedMember{Machine: machine, At: now})
		s.g.Members = slices.DeleteFunc(s.g.Members, func(m wire.GroupMember) bool { return m.Machine == machine })
		for _, d := range slices.Clone(s.g.Devices) {
			if d.Machine == machine {
				s.revokeLocked(d.DeviceID)
			}
		}
		if s.g.Home == machine {
			s.g.Home = s.self.Machine
		}
	}
	return s.commitLocked(true)
}

// SetHome moves home to a member.
func (s *Store) SetHome(machine string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return ErrNoGroup
	}
	if !slices.ContainsFunc(s.g.Members, func(m wire.GroupMember) bool { return m.Machine == machine }) {
		return ErrNotFound
	}
	if s.g.Home == machine {
		return nil
	}
	s.g.Home = machine
	return s.commitLocked(true)
}

// Rotate makes a new secret. The old secret stays valid for RotateGrace, while the syncer pushes the new one to the members.
// All devices get new credentials: a client gets its new one through POST /v1/group/devices or /v1/group/renew.
func (s *Store) Rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return ErrNoGroup
	}
	key := make([]byte, keySize)
	rand.Read(key)
	s.prev, s.key = s.key, key
	s.prevUntil = s.clock().Add(RotateGrace)
	s.rekeying = len(s.g.Members) > 1
	if err := s.saveKeysLocked(); err != nil {
		s.key, s.prev = s.prev, nil
		return err
	}
	return s.commitLocked(true)
}

// Rekey gives the secret to push to the members while this machine rotates. ok is false after the grace time.
func (s *Store) Rekey() (secret string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.rekeying || s.g == nil {
		return "", false
	}
	if !s.clock().Before(s.prevUntil) {
		s.rekeying = false
		_ = s.saveLocked()
		return "", false
	}
	return base64.StdEncoding.EncodeToString(s.key), true
}

// TakeRekey takes the new secret that the member from pushed after its rotate.
func (s *Store) TakeRekey(from, secret string) error {
	key, err := base64.StdEncoding.DecodeString(secret)
	if err != nil || len(key) != keySize {
		return errors.New("group: the secret is not valid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return ErrNoGroup
	}
	if hmac.Equal(key, s.key) {
		return nil
	}
	if s.rekeying && from > s.self.Machine {
		return ErrConflict
	}
	old := s.key
	s.prev, s.key, s.rekeying = old, key, false
	s.prevUntil = s.clock().Add(RotateGrace)
	if err := s.saveKeysLocked(); err != nil {
		s.key, s.prev = old, nil
		return err
	}
	s.notify()
	return s.saveLocked()
}

// Verify checks a credential: the HMAC, the group id, a revoked device and a removed machine.
func (s *Store) Verify(token string) (server.Caller, bool) {
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok {
		return server.Caller{}, false
	}
	body, mac, ok := strings.Cut(rest, ".")
	if !ok {
		return server.Caller{}, false
	}
	sum, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		return server.Caller{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return server.Caller{}, false
	}
	var p payload
	if json.Unmarshal(raw, &p) != nil {
		return server.Caller{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil || s.key == nil {
		return server.Caller{}, false
	}
	good := hmac.Equal(sum, sign(s.key, body))
	if s.prev != nil && s.clock().Before(s.prevUntil) {
		good = hmac.Equal(sum, sign(s.prev, body)) || good
	}
	if !good || p.G != s.g.ID || slices.Contains(s.g.Revoked, p.D) {
		return server.Caller{}, false
	}
	for _, r := range s.g.Removed {
		if r.Machine == p.M && r.At.Unix() >= p.Iat {
			return server.Caller{}, false
		}
	}
	c := server.Caller{DeviceID: p.D, Kind: p.K, Machine: p.M}
	for _, d := range s.g.Devices {
		if d.DeviceID == p.D {
			c.Name = d.Name
		}
	}
	return c, true
}

// payload is the JSON part of a credential.
type payload struct {
	G   string `json:"g"`
	D   string `json:"d"`
	K   string `json:"k"`
	M   string `json:"m"`
	Iat int64  `json:"iat"`
}

func sign(key []byte, body string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(tokenPrefix + body))
	return h.Sum(nil)
}

// tokenLocked makes the credential of d. The same device and key always give the same token.
func (s *Store) tokenLocked(d wire.GroupDevice, key []byte) string {
	b, _ := json.Marshal(payload{G: s.g.ID, D: d.DeviceID, K: string(d.Kind), M: d.Machine, Iat: d.IssuedAt.Unix()})
	body := base64.RawURLEncoding.EncodeToString(b)
	return tokenPrefix + body + "." + base64.RawURLEncoding.EncodeToString(sign(key, body))
}

func (s *Store) selfDeviceLocked() string {
	if s.g == nil {
		return ""
	}
	for _, d := range s.g.Devices {
		if d.Kind == wire.DeviceDaemon && d.Machine == s.self.Machine {
			return d.DeviceID
		}
	}
	return ""
}

// addDeviceLocked adds d, or gives the active device with the same kind, machine and name.
func (s *Store) addDeviceLocked(d wire.GroupDevice) (wire.GroupDevice, bool) {
	for _, x := range s.g.Devices {
		if x.Kind == d.Kind && x.Machine == d.Machine && x.Name == d.Name {
			return x, false
		}
	}
	id := make([]byte, 8)
	rand.Read(id)
	d.DeviceID = strings.ToUpper(hex.EncodeToString(id))
	d.IssuedBy = s.self.Machine
	d.IssuedAt = s.clock().UTC().Truncate(time.Second)
	s.g.Devices = append(s.g.Devices, d)
	return d, true
}

func (s *Store) revokeLocked(id string) {
	s.g.Devices = slices.DeleteFunc(s.g.Devices, func(d wire.GroupDevice) bool { return d.DeviceID == id })
	if !slices.Contains(s.g.Revoked, id) {
		s.g.Revoked = append(s.g.Revoked, id)
		s.sendRevoked(id)
	}
}

func (s *Store) sendRevoked(id string) {
	select {
	case s.revoked <- id:
	default:
	}
}

// commitLocked increases the version, saves, and tells the server and the syncer.
func (s *Store) commitLocked(local bool) error {
	g := normalize(*s.g, s.clock())
	g.Version++
	s.g = &g
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.notify()
	if local {
		select {
		case s.changed <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *Store) saveLocked() error {
	st := stored{Group: *s.g, Self: s.self.Machine, Rekeying: s.rekeying}
	if s.prev != nil {
		t := s.prevUntil
		st.PrevUntil = &t
	}
	return config.WriteJSON(s.path, st)
}

// saveKeysLocked writes the secret and the old secret in one value: "<cur>" or "<cur>,<prev>".
func (s *Store) saveKeysLocked() error {
	v := base64.StdEncoding.EncodeToString(s.key)
	if s.prev != nil {
		v += "," + base64.StdEncoding.EncodeToString(s.prev)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.secrets.Set(ctx, secretAccount, v); err != nil {
		return fmt.Errorf("group: store the secret: %w", err)
	}
	return nil
}

func decodeKeys(v string) (key, prev []byte, err error) {
	cur, old, _ := strings.Cut(strings.TrimSpace(v), ",")
	if key, err = base64.StdEncoding.DecodeString(cur); err != nil || len(key) != keySize {
		return nil, nil, errors.New("group: the secret is not valid")
	}
	if old != "" {
		if prev, err = base64.StdEncoding.DecodeString(old); err != nil || len(prev) != keySize {
			prev = nil
		}
	}
	return key, prev, nil
}

func daemonName(machine string) string { return "oversire on " + machine }
func appName(machine string) string    { return "OverSire on " + machine }

func newRevoked(old, now []string) []string {
	var out []string
	for _, id := range now {
		if !slices.Contains(old, id) {
			out = append(out, id)
		}
	}
	return out
}
