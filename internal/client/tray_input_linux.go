//go:build linux

package client

import (
	"fmt"
	"os/exec"
	"strings"
)

// promptInput shows an input dialog via zenity or kdialog. Returns an empty
// string when the user cancels.
func promptInput(title, prompt string) (string, error) {
	if path, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.Command(path, "--entry", "--title", title, "--text", prompt).Output()
		if err != nil {
			return "", nil // treat non-zero exit as cancel
		}
		return strings.TrimSpace(string(out)), nil
	}
	if path, err := exec.LookPath("kdialog"); err == nil {
		out, err := exec.Command(path, "--title", title, "--inputbox", prompt).Output()
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(string(out)), nil
	}
	return "", fmt.Errorf("no dialog tool found (install zenity or kdialog), use 'vlgr-client --add' instead")
}

// openBrowser opens a URL in the default browser.
func openBrowser(url string) {
	exec.Command("xdg-open", url).Start()
}
