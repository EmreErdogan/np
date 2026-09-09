package merge

import (
	"fmt"
	"strings"
)

// Op is the kind of a diff line.
type Op int

const (
	Same Op = iota
	Del
	Add
)

// Edit is one line of a diff.
type Edit struct {
	Op   Op
	Text string // without the trailing newline
}

// Hunk is a run of edits with the surrounding context, in unified-diff
// terms: lines FromStart.. of a and ToStart.. of b (1-based).
type Hunk struct {
	FromStart, FromLen int
	ToStart, ToLen     int
	Edits              []Edit
}

// Diff computes a line-level diff of a and b (LCS based). A missing
// trailing newline is not reported as a change.
func Diff(a, b []byte) []Edit {
	o, x := trimEOF(lines(a)), trimEOF(lines(b))
	mx := match(o, x)
	var out []Edit
	j := 0
	for i := range o {
		if mx[i] < 0 {
			out = append(out, Edit{Del, o[i]})
			continue
		}
		for ; j < mx[i]; j++ {
			out = append(out, Edit{Add, x[j]})
		}
		out = append(out, Edit{Same, o[i]})
		j++
	}
	for ; j < len(x); j++ {
		out = append(out, Edit{Add, x[j]})
	}
	return out
}

// trimEOF drops the empty element a trailing newline leaves behind.
func trimEOF(ls []string) []string {
	if n := len(ls); n > 0 && ls[n-1] == "" {
		return ls[:n-1]
	}
	return ls
}

// Hunks groups a diff into unified-diff hunks with context lines around
// each change. Identical inputs give no hunks.
func Hunks(a, b []byte, context int) []Hunk {
	edits := Diff(a, b)
	var hunks []Hunk
	i := 0
	for i < len(edits) {
		if edits[i].Op == Same {
			i++
			continue
		}
		// A change at i: extend backwards for context, then forwards over
		// further changes that are within 2*context of each other.
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i
		for end < len(edits) {
			if edits[end].Op != Same {
				end++
				continue
			}
			k := end
			for k < len(edits) && edits[k].Op == Same {
				k++
			}
			if k == len(edits) || k-end > 2*context {
				end += min(k-end, context)
				break
			}
			end = k
		}
		h := Hunk{Edits: edits[start:end]}
		h.FromStart, h.ToStart = 1, 1
		for _, e := range edits[:start] {
			if e.Op != Add {
				h.FromStart++
			}
			if e.Op != Del {
				h.ToStart++
			}
		}
		for _, e := range h.Edits {
			if e.Op != Add {
				h.FromLen++
			}
			if e.Op != Del {
				h.ToLen++
			}
		}
		hunks = append(hunks, h)
		i = end
	}
	return hunks
}

// Header is the "@@ -a,b +c,d @@" line of a hunk.
func (h Hunk) Header() string {
	return fmt.Sprintf("@@ -%s +%s @@", span(h.FromStart, h.FromLen), span(h.ToStart, h.ToLen))
}

func span(start, n int) string {
	if n == 1 {
		return fmt.Sprint(start)
	}
	if n == 0 && start > 0 {
		start--
	}
	return fmt.Sprintf("%d,%d", start, n)
}

// Unified renders a unified diff with the given file labels; empty when a
// and b are equal.
func Unified(a, b []byte, from, to string, context int) string {
	hunks := Hunks(a, b, context)
	if len(hunks) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", from, to)
	for _, h := range hunks {
		sb.WriteString(h.Header())
		sb.WriteByte('\n')
		for _, e := range h.Edits {
			sb.WriteByte(" -+"[e.Op])
			sb.WriteString(e.Text)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// Stat counts added and removed lines.
func Stat(a, b []byte) (added, removed int) {
	for _, e := range Diff(a, b) {
		switch e.Op {
		case Add:
			added++
		case Del:
			removed++
		}
	}
	return
}
