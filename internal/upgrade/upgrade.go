// Package upgrade replaces the running binary with the latest GitHub release.
//
// It talks to github.com the way install.sh does: the latest tag comes from
// the redirect of /releases/latest and the files from /releases/download.
// The REST API is not used, because its unauthenticated quota (60 requests
// an hour per IP address, shared with everything else behind that address)
// made upgrades fail with a bare "403 Forbidden".
package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const Repo = "EmreErdogan/np"

// Base is where releases are looked up; tests point it at a local server.
var Base = "https://github.com/" + Repo

// UserAgent identifies np to GitHub ("np/<version>").
var UserAgent = "np"

// Release is a published version.
type Release struct {
	Tag string
}

var client = &http.Client{Timeout: 2 * time.Minute}

// noFollow answers with the redirect itself instead of following it.
var noFollow = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func get(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	return c.Do(req)
}

// statusError explains a refusal, saying when to try again if GitHub told us.
func statusError(resp *http.Response, what string) error {
	msg := fmt.Sprintf("github.com answered %s for %s", resp.Status, what)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		when := ""
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			when = time.Now().Add(time.Duration(secs) * time.Second).Format("15:04")
		} else if unix, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && unix > 0 {
			when = time.Unix(unix, 0).Format("15:04")
		}
		msg += ": too many requests from this network"
		if when != "" {
			msg += ", try again after " + when
		} else {
			msg += ", try again in a few minutes"
		}
	}
	return errors.New(msg)
}

// Latest finds the newest release from where /releases/latest redirects to.
func Latest(ctx context.Context) (*Release, error) {
	resp, err := get(ctx, noFollow, Base+"/releases/latest")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
			return nil, errors.New("no release has been published yet")
		}
		return nil, statusError(resp, "the latest release")
	}
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return nil, fmt.Errorf("unexpected answer for the latest release: %s", loc)
	}
	tag := path.Base(loc[i:])
	if tag == "" || tag == "tag" || strings.ContainsAny(tag, "?#") {
		return nil, fmt.Errorf("unexpected answer for the latest release: %s", loc)
	}
	return &Release{Tag: tag}, nil
}

func (r *Release) asset(name string) string {
	return Base + "/releases/download/" + r.Tag + "/" + name
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	resp, err := get(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s is not part of the release", path.Base(url))
	}
	if resp.StatusCode != 200 {
		return nil, statusError(resp, path.Base(url))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// Install downloads the release build for this platform, verifies it against
// checksums.txt, and atomically replaces exe with it.
func Install(ctx context.Context, rel *Release, exe string) error {
	ver := strings.TrimPrefix(rel.Tag, "v")
	name := fmt.Sprintf("np_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	sums, err := fetch(ctx, rel.asset("checksums.txt"))
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("release %s has no build for %s/%s", rel.Tag, runtime.GOOS, runtime.GOARCH)
	}
	archive, err := fetch(ctx, rel.asset(name))
	if err != nil {
		return err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return errors.New("checksum mismatch, refusing to install")
	}
	bin, err := extract(archive, "np")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(exe), ".np.upgrade")
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func extract(archive []byte, member string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in archive", member)
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == member && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}
