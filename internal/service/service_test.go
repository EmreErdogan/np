package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBinaryFromDefinitions(t *testing.T) {
	units := map[string]string{
		"[Service]\nExecStart=/home/u/.local/bin/np daemon\nRestart=on-failure\n": "/home/u/.local/bin/np",
		"[Service]\nExecStart=/home/my user/bin/np daemon\n":                      "/home/my user/bin/np",
		"[Service]\nExecStart=-\"/opt/n p/np\" daemon\n":                          "/opt/n p/np",
		"[Service]\nExecStart=/usr/bin/np serve --flag\n":                         "/usr/bin/np",
		"[Unit]\nDescription=nothing to run\n":                                    "",
	}
	for unit, want := range units {
		if got := unitBinary(unit); got != want {
			t.Errorf("unitBinary(%q)=%q want %q", unit, got, want)
		}
	}
	plist := `<plist version="1.0"><dict>
	<key>Label</key><string>com.example.np</string>
	<key>ProgramArguments</key>
	<array><string>/Users/u/A &amp; B/np</string><string>daemon</string></array>
	<key>StandardOutPath</key><string>/Users/u/.np/daemon.log</string>
</dict></plist>`
	if got := plistBinary(plist); got != "/Users/u/A & B/np" {
		t.Fatalf("plistBinary=%q", got)
	}
	if got := plistBinary("<plist><dict><key>Label</key><string>x</string></dict></plist>"); got != "" {
		t.Fatalf("plist without arguments: %q", got)
	}
}

func TestSameFileFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "np-real")
	os.WriteFile(real, []byte("x"), 0o755)
	link := filepath.Join(dir, "np")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	other := filepath.Join(dir, "other")
	os.WriteFile(other, []byte("x"), 0o755)
	if !SameFile(real, link) || SameFile(real, other) || !SameFile(filepath.Join(dir, ".", "np-real"), real) {
		t.Fatal("SameFile")
	}
	// A path that does not exist is compared as written.
	if !SameFile("/nope/np", "/nope/np") || SameFile("/nope/np", "/nope/np2") {
		t.Fatal("SameFile on missing paths")
	}
}
