//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/jchv/go-webview2"
)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

const (
	hwndTopmost = ^uintptr(0) // -1
	swpNoSize   = 0x0001
	swpNoMove   = 0x0002
)

// openWindow shows the WebView2 popup. Safe to call from any goroutine; a
// second call while the window is open is ignored.
func openWindow(url string) {
	if !markWindowOpening() {
		return
	}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer setWindowClosed()

		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintln(os.Stderr, "webview crashed:", r)
				openBrowser(url)
			}
		}()

		w := webview2.New(false)
		if w == nil {
			fmt.Fprintln(os.Stderr, "webview2 unavailable, opening browser instead")
			openBrowser(url)
			return
		}
		w.SetTitle("stt — speech to text")
		w.SetSize(560, 680, webview2.HintNone)
		w.Navigate(url)
		w.Dispatch(func() {
			if hwnd := w.Window(); hwnd != nil {
				procSetWindowPos.Call(uintptr(hwnd), hwndTopmost, 0, 0, 0, 0, swpNoSize|swpNoMove)
				procSetForegroundWindow.Call(uintptr(hwnd))
			}
		})
		w.Run()
	}()
}

func openBrowser(url string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
