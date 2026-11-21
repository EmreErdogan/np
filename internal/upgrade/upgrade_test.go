package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func archive(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "np", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// github serves a release the way github.com does: a redirect for the
// latest tag and plain downloads. It records every path asked for.
func github(t *testing.T, tag string, files map[string][]byte) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" ua="+r.Header.Get("User-Agent"))
		if tag == "" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "https://"+r.Host+"/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		data, ok := files[strings.TrimPrefix(r.URL.Path, "/releases/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old, oldUA := Base, UserAgent
	Base, UserAgent = srv.URL, "np/test"
	t.Cleanup(func() { Base, UserAgent = old, oldUA })
	return srv, &seen
}

func TestLatestAndInstallNeedNoAPI(t *testing.T) {
	asset := func(ver string) string {
		return fmt.Sprintf("np_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	}
	name := asset("9.9.9")
	tgz := archive(t, "new binary")
	sum := sha256.Sum256(tgz)
	_, seen := github(t, "v9.9.9", map[string][]byte{
		"v9.9.9/" + name:           tgz,
		"v9.9.9/checksums.txt":     []byte("deadbeef  other.tar.gz\n" + hex.EncodeToString(sum[:]) + "  " + name + "\n"),
		"v9.9.8/checksums.txt":     []byte("deadbeef  other.tar.gz\n"),
		"v9.9.7/" + asset("9.9.7"): tgz,
		"v9.9.7/checksums.txt":     []byte(strings.Repeat("0", 64) + "  " + asset("9.9.7") + "\n"),
		"v9.9.6/unrelated-thing":   nil,
	})
	ctx := context.Background()
	rel, err := Latest(ctx)
	if err != nil || rel.Tag != "v9.9.9" {
		t.Fatalf("rel=%+v err=%v", rel, err)
	}
	exe := filepath.Join(t.TempDir(), "np")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	if err := Install(ctx, rel, exe); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary" {
		t.Fatalf("exe=%q", got)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm()&0o100 == 0 {
		t.Fatal("installed binary must be executable")
	}
	want := []string{"/releases/latest ua=np/test", "/releases/download/v9.9.9/checksums.txt", "/releases/download/v9.9.9/" + name}
	if strings.Join(*seen, "|") != strings.Join(want, "|") {
		t.Fatalf("requests=%v", *seen)
	}

	// No build for this platform, a wrong checksum, a missing file: the
	// installed binary stays as it is.
	for tag, wantErr := range map[string]string{"v9.9.8": "no build for", "v9.9.7": "checksum mismatch", "v9.9.6": "not part of the release"} {
		err := Install(ctx, &Release{Tag: tag}, exe)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("%s: err=%v", tag, err)
		}
		if got, _ := os.ReadFile(exe); string(got) != "new binary" {
			t.Fatalf("%s: exe was replaced: %q", tag, got)
		}
	}
}

func TestLatestWithoutAnyRelease(t *testing.T) {
	github(t, "", nil)
	if _, err := Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "no release") {
		t.Fatalf("err=%v", err)
	}
}

func TestRefusalsSayWhenToRetry(t *testing.T) {
	reset := time.Now().Add(37 * time.Minute)
	cases := []struct {
		status int
		header [2]string
		want   string
	}{
		{429, [2]string{"Retry-After", "120"}, "try again after " + time.Now().Add(2*time.Minute).Format("15:04")},
		{403, [2]string{"X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10)}, "try again after " + reset.Format("15:04")},
		{403, [2]string{"X-Other", "1"}, "try again in a few minutes"},
		{500, [2]string{"X-Other", "1"}, "500 Internal Server Error"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(c.header[0], c.header[1])
			w.WriteHeader(c.status)
		}))
		old := Base
		Base = srv.URL
		_, err := Latest(context.Background())
		Base = old
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d %v: err=%v, want %q", c.status, c.header, err, c.want)
		}
		if c.status != 500 && !strings.Contains(err.Error(), "too many requests") {
			t.Errorf("%d: should name the cause: %v", c.status, err)
		}
	}
}
