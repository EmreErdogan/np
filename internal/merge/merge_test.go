package merge

import "testing"

func TestMerge(t *testing.T) {
	cases := []struct {
		name, base, a, b, want string
		ok                     bool
	}{
		{"disjoint edits", "1\n2\n3\n4\n5\n", "one\n2\n3\n4\n5\n", "1\n2\n3\n4\nfive\n", "one\n2\n3\n4\nfive\n", true},
		{"both append", "a\nb\n", "a\nb\nc\n", "a\nb\nc\n", "a\nb\nc\n", true},
		{"one appends", "a\nb\n", "a\nb\nc\n", "a\nb\n", "a\nb\nc\n", true},
		{"insert and delete apart", "1\n2\n3\n4\n", "1\nx\n2\n3\n4\n", "1\n2\n3\n", "1\nx\n2\n3\n", true},
		{"same line changed differently", "1\n2\n3\n", "1\ntwo\n3\n", "1\nzwei\n3\n", "", false},
		{"adjacent appends differ", "a\n", "a\nb\n", "a\nc\n", "", false},
		{"no trailing newline", "a\nb", "a\nb\nc", "A\nb", "A\nb\nc", true},
		{"empty base", "", "a\n", "b\n", "", false},
		{"identical results", "x\n", "y\n", "y\n", "y\n", true},
		{"delete vs keep", "1\n2\n3\n", "1\n3\n", "1\n2\n3\nnew\n", "1\n3\nnew\n", true},
	}
	for _, c := range cases {
		got, ok := Merge([]byte(c.base), []byte(c.a), []byte(c.b))
		if ok != c.ok || string(got) != c.want {
			t.Errorf("%s: got %q ok=%v, want %q ok=%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestMergeIsSymmetric(t *testing.T) {
	base, a, b := "1\n2\n3\n4\n5\n", "0\n1\n2\n3\n4\n5\n", "1\n2\n3\n4\n5\n6\n"
	x, _ := Merge([]byte(base), []byte(a), []byte(b))
	y, _ := Merge([]byte(base), []byte(b), []byte(a))
	if string(x) != string(y) || string(x) != "0\n1\n2\n3\n4\n5\n6\n" {
		t.Fatalf("%q vs %q", x, y)
	}
}

func TestIsText(t *testing.T) {
	if !IsText([]byte("hello\n")) || IsText([]byte("PNG\x00\x01")) {
		t.Fatal("IsText")
	}
}
