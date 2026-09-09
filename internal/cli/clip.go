package cli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// copyToClipboard puts text on the clipboard and reports how. Over SSH the
// machine's own clipboard is not the one the user is looking at, so the
// OSC 52 escape sequence is used to hand the text to the local terminal
// (iTerm2, Terminal.app, kitty, WezTerm, Windows Terminal and others honour
// it). Locally the native tool is preferred, with OSC 52 as the fallback.
func copyToClipboard(text string) (string, error) {
	overSSH := os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != ""
	if !overSSH {
		if how, err := copyNative(text); err == nil {
			return how, nil
		}
	}
	if err := copyOSC52(text); err == nil {
		return "terminal (OSC 52)", nil
	}
	if how, err := copyNative(text); err == nil {
		return how, nil
	}
	return "", errors.New("no clipboard available (pbcopy, wl-copy, xclip, xsel, or a terminal that supports OSC 52)")
}

func copyNative(text string) (string, error) {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, []string{"wl-copy"})
		}
		if os.Getenv("DISPLAY") != "" {
			candidates = append(candidates, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
		}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return c[0], nil
		}
	}
	return "", errors.New("no native clipboard tool")
}

// copyOSC52 writes the clipboard escape to the controlling terminal.
func copyOSC52(text string) error {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer tty.Close()
	seq := fmt.Sprintf("\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(text)))
	if os.Getenv("TMUX") != "" { // tmux needs the sequence wrapped to pass it through
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	_, err = tty.WriteString(seq)
	return err
}
