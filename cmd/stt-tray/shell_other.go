//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// openWindow has no embedded webview outside Windows yet; the same UI opens
// in the system browser instead (identical frontend, identical API).
func openWindow(url string) {
	if !markWindowOpening() {
		return
	}
	go func() {
		defer setWindowClosed()
		openBrowser(url)
	}()
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}
