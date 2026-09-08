package clock

import "testing"

func TestCompare(t *testing.T) {
	a := Clock{"x": 2, "y": 1}
	cases := []struct {
		b    Clock
		want int
	}{
		{Clock{"x": 2, "y": 1}, Equal},
		{Clock{"x": 1, "y": 1}, Dominates},
		{Clock{"x": 3, "y": 1}, Dominated},
		{Clock{"x": 1, "y": 2}, Concurrent},
		{Clock{}, Dominates},
		{Clock{"z": 1}, Concurrent},
	}
	for _, c := range cases {
		if got := Compare(a, c.b); got != c.want {
			t.Errorf("Compare(%v,%v)=%d want %d", a, c.b, got, c.want)
		}
	}
}

func TestMergeBump(t *testing.T) {
	m := Merge(Clock{"x": 1}, Clock{"x": 3, "y": 2}).Bump("x")
	if m.String() != "x:4,y:2" {
		t.Fatalf("got %s", m.String())
	}
}
