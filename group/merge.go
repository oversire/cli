package group

import (
	"cmp"
	"encoding/json"
	"reflect"
	"slices"
	"time"

	"github.com/oversire/cli/wire"
)

// merge joins the local roster and the roster of the member from. self is this machine.
//
//   - Name, home and cloud: the higher version wins. On equal versions, the lower machine name of self and from wins.
//   - Members and devices: a union, minus removed machines and revoked devices. A tombstone removes a member that joined before it.
//   - Each member is the truth for its own entry. Else the later JoinedAt, then the winner side.
//   - Revoked and removed: a union. A tombstone stays for TombstoneTTL.
//   - The version is the higher one. It goes up by 1 on equal versions with different content,
//     and when the result is not the roster of the side with the higher version.
func merge(local, remote wire.Group, from, self string, now time.Time) wire.Group {
	remoteWins := remote.Version > local.Version || (remote.Version == local.Version && from < self)
	win, lose := local, remote
	if remoteWins {
		win, lose = remote, local
	}
	out := wire.Group{ID: local.ID, Name: win.Name, Home: win.Home, Cloud: win.Cloud, Version: max(local.Version, remote.Version)}
	out.Revoked = append(slices.Clone(local.Revoked), remote.Revoked...)
	out.Removed = append(slices.Clone(local.Removed), remote.Removed...)

	byMachine := map[string]wire.GroupMember{}
	pick := func(m wire.GroupMember, side string) {
		old, ok := byMachine[m.Machine]
		if !ok {
			byMachine[m.Machine] = m
			return
		}
		seen := later(old.LastSeen, m.LastSeen)
		switch {
		case !m.JoinedAt.Equal(old.JoinedAt):
			if m.JoinedAt.After(old.JoinedAt) {
				old = m
			}
		case m.Machine == self:
			if side == "local" {
				old = m
			}
		case m.Machine == from:
			if side == "remote" {
				old = m
			}
		case side == "win":
			old = m
		}
		old.LastSeen = seen
		byMachine[m.Machine] = old
	}
	// The order sets the priority: the later call wins on a tie.
	for _, m := range lose.Members {
		pick(m, "lose")
	}
	for _, m := range win.Members {
		pick(m, "win")
	}
	for _, m := range remote.Members {
		pick(m, "remote")
	}
	for _, m := range local.Members {
		pick(m, "local")
	}
	for _, m := range byMachine {
		out.Members = append(out.Members, m)
	}
	out.Devices = append(slices.Clone(win.Devices), lose.Devices...)
	out = normalize(out, now)

	tie := local.Version == remote.Version && !sameContent(local, remote)
	if tie || (!(local.Version == out.Version && sameContent(out, local)) && !(remote.Version == out.Version && sameContent(out, remote))) {
		out.Version++
	}
	return out
}

// normalize sorts and dedups the lists, drops old tombstones, and applies tombstones and revoked ids.
func normalize(g wire.Group, now time.Time) wire.Group {
	removed := map[string]time.Time{}
	for _, r := range g.Removed {
		if now.Sub(r.At) > TombstoneTTL {
			continue
		}
		if at, ok := removed[r.Machine]; !ok || r.At.After(at) {
			removed[r.Machine] = r.At
		}
	}
	g.Removed = []wire.RemovedMember{}
	for m, at := range removed {
		g.Removed = append(g.Removed, wire.RemovedMember{Machine: m, At: at})
	}

	gone := func(machine string, t time.Time) bool {
		at, ok := removed[machine]
		return ok && !at.Before(t)
	}
	g.Revoked = slices.Compact(slices.Sorted(slices.Values(g.Revoked)))

	members := []wire.GroupMember{}
	for _, m := range g.Members {
		if !gone(m.Machine, m.JoinedAt) && !slices.ContainsFunc(members, func(x wire.GroupMember) bool { return x.Machine == m.Machine }) {
			members = append(members, m)
		}
	}
	g.Members = members

	devices := []wire.GroupDevice{}
	for _, d := range g.Devices {
		if slices.Contains(g.Revoked, d.DeviceID) || gone(d.Machine, d.IssuedAt) ||
			slices.ContainsFunc(devices, func(x wire.GroupDevice) bool { return x.DeviceID == d.DeviceID }) {
			continue
		}
		devices = append(devices, d)
	}
	g.Devices = devices
	sortLists(&g)
	return g
}

// sortLists sorts the lists and makes nil lists empty.
func sortLists(g *wire.Group) {
	if g.Members == nil {
		g.Members = []wire.GroupMember{}
	}
	if g.Devices == nil {
		g.Devices = []wire.GroupDevice{}
	}
	if g.Revoked == nil {
		g.Revoked = []string{}
	}
	if g.Removed == nil {
		g.Removed = []wire.RemovedMember{}
	}
	slices.SortFunc(g.Members, func(a, b wire.GroupMember) int { return cmp.Compare(a.Machine, b.Machine) })
	slices.SortFunc(g.Devices, func(a, b wire.GroupDevice) int { return cmp.Compare(a.DeviceID, b.DeviceID) })
	slices.SortFunc(g.Removed, func(a, b wire.RemovedMember) int { return cmp.Compare(a.Machine, b.Machine) })
	slices.Sort(g.Revoked)
}

// sameContent compares two rosters without the version and the LastSeen times.
func sameContent(a, b wire.Group) bool {
	a, b = clone(a), clone(b)
	sortLists(&a)
	sortLists(&b)
	a.Version, b.Version = 0, 0
	for i := range a.Members {
		a.Members[i].LastSeen = nil
	}
	for i := range b.Members {
		b.Members[i].LastSeen = nil
	}
	return reflect.DeepEqual(canon(a), canon(b))
}

func equal(a, b wire.Group) bool { return a.Version == b.Version && sameContent(a, b) }

// canon gives the JSON form, so that nil and empty lists and time zones compare the same.
func canon(g wire.Group) any {
	b, _ := json.Marshal(g)
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

func clone(g wire.Group) wire.Group {
	b, _ := json.Marshal(g)
	var out wire.Group
	_ = json.Unmarshal(b, &out)
	return out
}

func sameMember(a, b wire.GroupMember) bool {
	return a.Machine == b.Machine && a.Host == b.Host && a.Port == b.Port && a.Version == b.Version &&
		slices.Equal(a.Addrs, b.Addrs) && slices.Equal(a.Roles, b.Roles)
}

func later(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil || a.After(*b) {
		return a
	}
	return b
}
