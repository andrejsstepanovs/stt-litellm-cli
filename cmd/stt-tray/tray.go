package main

import (
	"fmt"
	"os"
	"time"

	"fyne.io/systray"
)

// runTray owns the notification-area icon. Left-clicking the icon toggles
// recording (the "quick action"); the right-click menu has record, status
// window and quit. The icon color and tooltip track the session state.
func runTray(sess *session, url string, onExit func()) {
	sess.onChange = func() { refreshTray(sess) }

	onReady := func() {
		rec := systray.AddMenuItem("● Record", "Start recording")
		show := systray.AddMenuItem("Show status", "Open the status window")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit stt", "Stop the app")
		rec.Enable()

		// Left click on the icon itself: record/stop quick action.
		systray.SetOnTapped(func() { toggleRecord(sess) })

		go func() {
			for range rec.ClickedCh {
				toggleRecord(sess)
			}
		}()
		go func() {
			for range show.ClickedCh {
				openWindow(url)
			}
		}()
		go func() {
			for range quit.ClickedCh {
				systray.Quit()
			}
		}()

		refreshTray(sess)
		// keep the tooltip/elapsed fresh while a cycle runs
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				if sess.Busy() {
					refreshTray(sess)
				}
			}
		}()
	}

	if onExit != nil {
		systray.Register(onReady, onExit) // darwin: caller owns the main loop
	} else {
		systray.Run(onReady, nil)
	}
}

func toggleRecord(sess *session) {
	if sess.Busy() {
		sess.Stop()
	} else {
		if err := sess.Record(sess.LastDevice(), sess.LastModel()); err != nil {
			fmt.Fprintln(os.Stderr, "record:", err)
		}
	}
}

var (
	lastTip   string
	lastColor uint32
)

func refreshTray(sess *session) {
	state, elapsed, errText := sess.traySnapshot()

	var color uint32
	var tip string
	switch state {
	case "starting":
		color, tip = colTx, fmt.Sprintf("stt — starting mic (%s)", fmtElapsedTray(elapsed))
	case "recording":
		color, tip = colRec, fmt.Sprintf("stt — recording %s (click to stop)", fmtElapsedTray(elapsed))
	case "stopping":
		color, tip = colTx, "stt — finalizing…"
	case "transcribing":
		color, tip = colTx, fmt.Sprintf("stt — transcribing %s", fmtElapsedTray(elapsed))
	case "result":
		if errText != "" {
			color, tip = colErr, "stt — error: "+firstLineTray(errText)
		} else {
			color, tip = colOK, "stt — copied to clipboard ✓"
		}
	default:
		color, tip = colIdle, "stt — idle (click to record)"
	}

	// Shell_NotifyIcon is not free; skip when nothing changed.
	if tip != lastTip {
		systray.SetTooltip(tip)
		lastTip = tip
	}
	if color != lastColor {
		systray.SetIcon(makeICO(color))
		lastColor = color
	}
}

func fmtElapsedTray(d time.Duration) string {
	d -= d % time.Second
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func firstLineTray(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 80 {
		s = s[:79] + "…"
	}
	return s
}
