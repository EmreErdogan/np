package merge

import "testing"

func TestUnified(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	b := "1\n2\n3\nfour\n5\n6\n7\n8\n9\n10\neleven\n"
	want := `--- a
+++ b
@@ -1,10 +1,11 @@
 1
 2
 3
-4
+four
 5
 6
 7
 8
 9
 10
+eleven
`
	if got := Unified([]byte(a), []byte(b), "a", "b", 3); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if Unified([]byte(a), []byte(a), "a", "a", 3) != "" {
		t.Fatal("identical inputs should give an empty diff")
	}
}

func TestHunksSplitFarApartChanges(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n"
	b := "one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\nfifteen\n"
	hs := Hunks([]byte(a), []byte(b), 3)
	if len(hs) != 2 {
		t.Fatalf("hunks=%d", len(hs))
	}
	if hs[0].Header() != "@@ -1,4 +1,4 @@" || hs[1].Header() != "@@ -12,4 +12,4 @@" {
		t.Fatalf("%q %q", hs[0].Header(), hs[1].Header())
	}
}

func TestDiffEdges(t *testing.T) {
	if d := Diff(nil, []byte("a\nb\n")); len(d) != 2 || d[0].Op != Add {
		t.Fatalf("from empty: %+v", d)
	}
	if d := Diff([]byte("a\n"), nil); len(d) != 1 || d[0].Op != Del {
		t.Fatalf("to empty: %+v", d)
	}
	if d := Diff([]byte("a\nb"), []byte("a\nb\n")); len(d) != 2 || d[0].Op != Same || d[1].Op != Same {
		t.Fatalf("trailing newline only: %+v", d)
	}
	if add, del := Stat([]byte("a\nb\n"), []byte("b\nc\nd\n")); add != 2 || del != 1 {
		t.Fatal(add, del)
	}
}
