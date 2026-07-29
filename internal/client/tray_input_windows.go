//go:build windows

package client

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"syscall"
)

// promptInput shows a native input box via the VisualBasic Interaction
// helper. Returns an empty string when the user cancels.
func promptInput(title, prompt string) (string, error) {
	script := fmt.Sprintf(
		"Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.Interaction]::InputBox(%s, %s, '')",
		psQuote(prompt), psQuote(title))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// showMessage displays a native message box.
func showMessage(title, text string) {
	script := fmt.Sprintf(
		"Add-Type -AssemblyName Microsoft.VisualBasic; [void][Microsoft.VisualBasic.Interaction]::MsgBox(%s, 0, %s)",
		psQuote(text), psQuote(title))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		log.Printf("[tray] message dialog: %v", err)
	}
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// openBrowser opens a URL in the default browser.
func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.Start()
}
