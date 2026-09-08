// Package service installs `np daemon` as a per-user background service:
// a systemd user unit on Linux, a launchd agent on macOS.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	systemdName = "np.service"
	launchdName = "com.github.emreerdogan.np"
)

// Install writes the service definition for exe and starts it.
func Install(exe, logPath string) (string, error) {
	switch runtime.GOOS {
	case "linux":
		return installSystemd(exe)
	case "darwin":
		return installLaunchd(exe, logPath)
	default:
		return "", fmt.Errorf("service install is not supported on %s", runtime.GOOS)
	}
}

// Uninstall stops and removes the service.
func Uninstall() error {
	switch runtime.GOOS {
	case "linux":
		run("systemctl", "--user", "disable", "--now", systemdName)
		os.Remove(systemdPath())
		return run("systemctl", "--user", "daemon-reload")
	case "darwin":
		run("launchctl", "bootout", domain(), launchdPath())
		return os.Remove(launchdPath())
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
}

// Restart restarts the service if it is installed; a no-op otherwise.
func Restart() (bool, error) {
	switch runtime.GOOS {
	case "linux":
		if !fileExists(systemdPath()) {
			return false, nil
		}
		return true, run("systemctl", "--user", "restart", systemdName)
	case "darwin":
		if !fileExists(launchdPath()) {
			return false, nil
		}
		return true, run("launchctl", "kickstart", "-k", domain()+"/"+launchdName)
	}
	return false, nil
}

// Status returns a short description of the service state.
func Status() (string, error) {
	switch runtime.GOOS {
	case "linux":
		out, _ := exec.Command("systemctl", "--user", "is-active", systemdName).CombinedOutput()
		return strings.TrimSpace(string(out)), nil
	case "darwin":
		out, err := exec.Command("launchctl", "print", domain()+"/"+launchdName).CombinedOutput()
		if err != nil {
			return "not loaded", nil
		}
		for _, l := range strings.Split(string(out), "\n") {
			if strings.Contains(l, "state =") {
				return strings.TrimSpace(l), nil
			}
		}
		return "loaded", nil
	default:
		return "", fmt.Errorf("not supported on %s", runtime.GOOS)
	}
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// ---- systemd ----------------------------------------------------------------

func systemdPath() string {
	return filepath.Join(home(), ".config", "systemd", "user", systemdName)
}

func installSystemd(exe string) (string, error) {
	unit := fmt.Sprintf(`[Unit]
Description=np tailnet notes daemon
After=network-online.target tailscaled.service

[Service]
ExecStart=%s daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe)
	p := systemdPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return "", err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return "", err
	}
	if err := run("systemctl", "--user", "enable", "--now", systemdName); err != nil {
		return "", err
	}
	return p, nil
}

// ---- launchd ----------------------------------------------------------------

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func launchdPath() string {
	return filepath.Join(home(), "Library", "LaunchAgents", launchdName+".plist")
}

func installLaunchd(exe, logPath string) (string, error) {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string><string>daemon</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdName, exe, logPath, logPath)
	p := launchdPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	run("launchctl", "bootout", domain(), p) // ignore: may not be loaded
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return "", err
	}
	if err := run("launchctl", "bootstrap", domain(), p); err != nil {
		return "", err
	}
	return p, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
