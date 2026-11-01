package review

import (
	"slices"
	"sort"
)

// Order is the merge order of the repos of a group: the daemon config review.order,
// for example [agent-binder, oversire, cloud, cli].
type Order []string

// Sort gives names in merge order: the names of o first, in the order of o, then the others in alphabetic order.
func (o Order) Sort(names []string) []string {
	out := slices.Clone(names)
	rank := func(n string) int {
		if i := slices.Index(o, n); i >= 0 {
			return i
		}
		return len(o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}
