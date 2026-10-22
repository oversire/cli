package group

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

var t0 = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)

func mem(name string, joined time.Time, addr string) wire.GroupMember {
	return wire.GroupMember{Machine: name, Host: name, Addrs: []string{addr}, Port: 7690, JoinedAt: joined}
}

func dev(id, machine string) wire.GroupDevice {
	return wire.GroupDevice{DeviceID: id, Name: "oversire on " + machine, Kind: wire.DeviceDaemon, Machine: machine, IssuedAt: t0}
}

func roster(version int, home string, members []wire.GroupMember, devices []wire.GroupDevice) wire.Group {
	return wire.Group{ID: "g1", Name: DefaultName, Home: home, Version: version, Members: members, Devices: devices}
}

func machines(g wire.Group) string {
	var s []string
	for _, m := range g.Members {
		s = append(s, m.Machine)
	}
	return fmt.Sprint(s)
}

func deviceIDs(g wire.Group) string {
	var s []string
	for _, d := range g.Devices {
		s = append(s, d.DeviceID)
	}
	return fmt.Sprint(s)
}

func TestMerge(t *testing.T) {
	now := t0.Add(48 * time.Hour)
	base := []wire.GroupMember{mem("mba", t0, "a"), mem("mbp", t0, "p")}
	baseDev := []wire.GroupDevice{dev("D1", "mbp"), dev("D2", "mba")}
	withRemoved := func(g wire.Group, r ...wire.RemovedMember) wire.Group { g.Removed = r; return g }
	withRevoked := func(g wire.Group, r ...string) wire.Group { g.Revoked = r; return g }

	cases := []struct {
		name          string
		local, remote wire.Group
		from, self    string
		version       int
		home          string
		members       string
		devices       string
	}{
		{
			name:   "concurrent adds give the union and a new version",
			local:  roster(4, "mbp", append(slices.Clone(base), mem("mini", t0.Add(time.Hour), "m")), append(slices.Clone(baseDev), dev("D3", "mini"))),
			remote: roster(4, "mbp", append(slices.Clone(base), mem("studio", t0.Add(time.Hour), "s")), append(slices.Clone(baseDev), dev("D4", "studio"))),
			from:   "mba", self: "mbp",
			version: 5, home: "mbp", members: "[mba mbp mini studio]", devices: "[D1 D2 D3 D4]",
		},
		{
			name:   "a higher version wins for home and keeps its version",
			local:  roster(3, "mbp", base, baseDev),
			remote: roster(5, "mba", base, baseDev),
			from:   "mba", self: "mbp",
			version: 5, home: "mba", members: "[mba mbp]", devices: "[D1 D2]",
		},
		{
			name:   "equal versions: the lower machine name (self) wins",
			local:  roster(3, "mbp", base, baseDev),
			remote: roster(3, "mba", base, baseDev),
			from:   "mbp2", self: "mbp",
			version: 4, home: "mbp", members: "[mba mbp]", devices: "[D1 D2]",
		},
		{
			name:   "equal versions: the lower machine name (from) wins",
			local:  roster(3, "mbp", base, baseDev),
			remote: roster(3, "mba", base, baseDev),
			from:   "mba", self: "mbp",
			version: 4, home: "mba", members: "[mba mbp]", devices: "[D1 D2]",
		},
		{
			name:   "a remove after the add wins",
			local:  withRemoved(roster(5, "mbp", base, baseDev), wire.RemovedMember{Machine: "mini", At: t0.Add(2 * time.Hour)}),
			remote: roster(5, "mbp", append(slices.Clone(base), mem("mini", t0.Add(time.Hour), "m")), append(slices.Clone(baseDev), dev("D3", "mini"))),
			from:   "mba", self: "mbp",
			version: 6, home: "mbp", members: "[mba mbp]", devices: "[D1 D2]",
		},
		{
			name:   "an add after the remove wins",
			local:  withRemoved(roster(5, "mbp", base, baseDev), wire.RemovedMember{Machine: "mini", At: t0.Add(-time.Hour)}),
			remote: roster(5, "mbp", append(slices.Clone(base), mem("mini", t0.Add(time.Hour), "m")), append(slices.Clone(baseDev), dev("D3", "mini"))),
			from:   "mba", self: "mbp",
			version: 6, home: "mbp", members: "[mba mbp mini]", devices: "[D1 D2 D3]",
		},
		{
			name:   "an old tombstone expires",
			local:  withRemoved(roster(5, "mbp", base, baseDev), wire.RemovedMember{Machine: "old", At: now.Add(-TombstoneTTL - time.Hour)}),
			remote: roster(5, "mbp", base, baseDev),
			from:   "mba", self: "mbp",
			version: 5, home: "mbp", members: "[mba mbp]", devices: "[D1 D2]",
		},
		{
			name:   "revoked is a union",
			local:  roster(5, "mbp", base, append(slices.Clone(baseDev), dev("A1", "mba"))),
			remote: withRevoked(roster(6, "mbp", base, baseDev), "A1"),
			from:   "mba", self: "mbp",
			version: 6, home: "mbp", members: "[mba mbp]", devices: "[D1 D2]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := merge(normalize(c.local, now), c.remote, c.from, c.self, now)
			if out.Version != c.version || out.Home != c.home || machines(out) != c.members || deviceIDs(out) != c.devices {
				t.Fatalf("got v%d home %s members %s devices %s", out.Version, out.Home, machines(out), deviceIDs(out))
			}
			// The other side gets the same content.
			back := merge(normalize(c.remote, now), out, c.self, c.from, now)
			if !sameContent(back, out) || back.Version != out.Version {
				t.Fatalf("not the same on the other side: v%d %s vs v%d %s", back.Version, machines(back), out.Version, machines(out))
			}
		})
	}
}

func TestMergeSelfEntry(t *testing.T) {
	local := roster(3, "mbp", []wire.GroupMember{mem("mba", t0, "a"), mem("mbp", t0, "new")}, nil)
	remote := roster(4, "mbp", []wire.GroupMember{mem("mba", t0, "a2"), mem("mbp", t0, "old")}, nil)
	seen := t0.Add(time.Hour)
	remote.Members[1].LastSeen = &seen
	out := merge(normalize(local, t0), remote, "mba", "mbp", t0)
	if out.Members[1].Addrs[0] != "new" || out.Members[0].Addrs[0] != "a2" {
		t.Fatalf("%+v", out.Members)
	}
	if out.Members[1].LastSeen == nil || !out.Members[1].LastSeen.Equal(seen) {
		t.Fatal("last seen not kept")
	}
	if out.Version != 5 {
		t.Fatal(out.Version)
	}
}
