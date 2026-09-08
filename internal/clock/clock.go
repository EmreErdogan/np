// Package clock implements a small vector clock used to order note versions
// across peers without a central authority.
package clock

import (
	"fmt"
	"sort"
	"strings"
)

// Clock maps a node name to that node's edit counter for one note.
type Clock map[string]int

// Result of comparing two clocks.
const (
	Equal      = 0
	Dominates  = 1  // a > b: a has seen everything b has, and more
	Dominated  = -1 // a < b
	Concurrent = 2  // neither has seen the other's latest edits
)

// Copy returns an independent copy.
func (c Clock) Copy() Clock {
	out := make(Clock, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// Bump returns a copy with node's counter incremented.
func (c Clock) Bump(node string) Clock {
	out := c.Copy()
	out[node]++
	return out
}

// Merge returns the element-wise maximum of a and b.
func Merge(a, b Clock) Clock {
	out := a.Copy()
	for k, v := range b {
		if v > out[k] {
			out[k] = v
		}
	}
	return out
}

// Compare orders a against b.
func Compare(a, b Clock) int {
	aGreater, bGreater := false, false
	for k, v := range a {
		if v > b[k] {
			aGreater = true
		}
	}
	for k, v := range b {
		if v > a[k] {
			bGreater = true
		}
	}
	switch {
	case aGreater && bGreater:
		return Concurrent
	case aGreater:
		return Dominates
	case bGreater:
		return Dominated
	default:
		return Equal
	}
}

// String renders the clock deterministically, e.g. "server:3,laptop:1".
func (c Clock) String() string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, c[k]))
	}
	return strings.Join(parts, ",")
}
