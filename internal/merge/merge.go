// Package merge implements a line-based three-way merge (diff3) used to
// reconcile concurrent edits of the same note. Changes that touch different
// regions are combined; overlapping changes are reported as a conflict and
// left to the caller.
package merge

import (
	"bytes"
	"strings"
)

// IsText reports whether data looks like text (no NUL byte in the first 8 KiB).
func IsText(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	return !bytes.ContainsRune(head, 0)
}

// Merge combines a and b, both derived from base. It returns the merged
// content and true when every change could be applied without overlap.
// Otherwise it returns nil and false.
func Merge(base, a, b []byte) ([]byte, bool) {
	if bytes.Equal(a, b) {
		return a, true
	}
	if bytes.Equal(base, a) {
		return b, true
	}
	if bytes.Equal(base, b) {
		return a, true
	}
	o, x, y := lines(base), lines(a), lines(b)
	mx, my := match(o, x), match(o, y)

	var out []string
	lo, lx, ly := 0, 0, 0
	for lo < len(o) || lx < len(x) || ly < len(y) {
		// Find the next base line that survives in both a and b.
		i := 0
		for lo+i < len(o) && (mx[lo+i] < 0 || my[lo+i] < 0) {
			i++
		}
		ex, ey := len(x), len(y)
		if lo+i < len(o) {
			ex, ey = mx[lo+i], my[lo+i]
		}
		if i == 0 && ex == lx && ey == ly {
			// Stable run: copy while all three keep agreeing.
			for lo < len(o) && mx[lo] == lx && my[lo] == ly {
				out = append(out, o[lo])
				lo, lx, ly = lo+1, lx+1, ly+1
			}
			continue
		}
		// Unstable chunk: base[lo:lo+i], x[lx:ex], y[ly:ey].
		ob, xb, yb := o[lo:lo+i], x[lx:ex], y[ly:ey]
		switch {
		case equal(xb, yb):
			out = append(out, xb...)
		case equal(ob, xb):
			out = append(out, yb...)
		case equal(ob, yb):
			out = append(out, xb...)
		default:
			return nil, false
		}
		lo, lx, ly = lo+i, ex, ey
	}
	return join(out), true
}

// lines splits on newlines. A trailing newline yields a final empty
// element, so joining with "\n" reproduces the input exactly and a change
// in the trailing newline merges like any other line.
func lines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(string(data), "\n")
}

func join(ls []string) []byte {
	return []byte(strings.Join(ls, "\n"))
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// match returns, for every line of o, the index of the line it is matched
// with in x under a longest common subsequence, or -1.
func match(o, x []string) []int {
	n, m := len(o), len(x)
	// dp[i][j] = LCS length of o[i:], x[j:].
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if o[i] == x[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	res := make([]int, n)
	for i := range res {
		res[i] = -1
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case o[i] == x[j]:
			res[i] = j
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return res
}
