// Command stt-tray is a taskbar quick-action app: click to record, watch the
// progress, get the transcript on the clipboard. It drives the same engine
// (internal/core) as the CLI and shares the same history database and .env
// configuration. The UI is a single HTML page served from localhost; Windows
// shows it in a WebView2 popup, macOS/Linux open it in the default browser.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andrejsstepanovs/stt-litellm-cli/internal/core"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

//go:embed ui/index.html
var uiFS embed.FS

// window open tracking shared by the shell implementations.
var (
	winMu   sync.Mutex
	winOpen bool
)

func markWindowOpening() bool {
	winMu.Lock()
	defer winMu.Unlock()
	if winOpen {
		return false
	}
	winOpen = true
	return true
}

func setWindowClosed() {
	winMu.Lock()
	winOpen = false
	winMu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return v, false
	}
	return v, true
}

func main() {
	core.LoadDotEnv()
	url := os.Getenv("STT_LITELLM_URL")
	key := os.Getenv("STT_LITELLM_KEY")
	core.SetConfig(url, key)

	serverOnly := false
	for _, a := range os.Args[1:] {
		if a == "--server" {
			serverOnly = true
		}
		if a == "--version" {
			fmt.Printf("stt-tray %s\n", version)
			return
		}
	}

	store, err := core.OpenStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: history disabled: %v\n", err)
	}
	sess := newSession(store)
	sess.checkHealth()

	// keep the WebView2 profile out of the exe directory
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			os.Setenv("WEBVIEW2_USER_DATA_FOLDER", filepath.Join(base, "stt", "WebView2"))
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	baseURL := fmt.Sprintf("http://%s", ln.Addr())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		var ffmpeg bool
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpeg = true
		}
		writeJSON(w, map[string]any{
			"goos":     runtime.GOOS,
			"version":  version,
			"model":    os.Getenv("STT_MODEL"),
			"device":   os.Getenv("STT_DEVICE"),
			"proxy":    url,
			"ffmpeg":   ffmpeg,
			"db":       core.DBPath(),
			"canPause": true,
		})
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		// refresh in the background; a slow proxy must not hold the
		// browser's connection slots (this caused severe UI lag)
		go func() {
			ok, detail := core.HealthCheck()
			sess.setHealth(ok, detail)
		}()
		ok, detail, checked := sess.Health()
		writeJSON(w, map[string]any{"ok": ok, "detail": detail, "checked": checked})
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, sess.Status())
	})
	mux.HandleFunc("POST /api/record", func(w http.ResponseWriter, r *http.Request) {
		req, _ := readJSON[struct {
			Device string `json:"device"`
			Model  string `json:"model"`
		}](w, r)
		if err := sess.Record(req.Device, req.Model); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/stop", func(w http.ResponseWriter, r *http.Request) {
		sess.Stop()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/cancel", func(w http.ResponseWriter, r *http.Request) {
		sess.Cancel()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		sess.Pause()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/copy", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readJSON[struct {
			Text string `json:"text"`
		}](w, r)
		if !ok {
			return
		}
		writeJSON(w, map[string]any{"ok": core.CopyToClipboard(req.Text)})
	})
	mux.HandleFunc("POST /api/edit", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readJSON[struct {
			ID   int64  `json:"id"`
			Text string `json:"text"`
		}](w, r)
		if !ok {
			return
		}
		if store != nil {
			store.UpdateText(req.ID, req.Text)
		}
		writeJSON(w, map[string]any{"ok": store != nil, "copied": core.CopyToClipboard(req.Text)})
	})
	mux.HandleFunc("POST /api/retry", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readJSON[struct {
			ID int64 `json:"id"`
		}](w, r)
		if !ok {
			return
		}
		if err := sess.Retry(req.ID); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, map[string]any{"entries": []core.Entry{}})
			return
		}
		failed := r.URL.Query().Get("failed") == "1"
		limit := 200
		var entries []core.Entry
		if failed {
			entries = store.Failed(limit)
		} else {
			entries = store.Recent(limit)
		}
		if entries == nil {
			entries = []core.Entry{}
		}
		writeJSON(w, map[string]any{"entries": entries})
	})
	mux.HandleFunc("POST /api/delete", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readJSON[struct {
			ID int64 `json:"id"`
		}](w, r)
		if !ok {
			return
		}
		deleted := false
		if store != nil {
			if e, found := store.Get(req.ID); found && !sess.ownsWav(e.Path) {
				removeEntryFiles(e)
			}
			deleted = store.Delete(req.ID)
		}
		writeJSON(w, map[string]any{"ok": deleted})
	})
	mux.HandleFunc("POST /api/clear_failed", func(w http.ResponseWriter, r *http.Request) {
		removed := int64(0)
		if store != nil {
			for _, e := range store.Failed(10000) {
				if !sess.ownsWav(e.Path) {
					removeEntryFiles(e)
				}
			}
			removed = store.DeleteFailed()
		}
		writeJSON(w, map[string]any{"ok": true, "removed": removed})
	})
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("refresh") == "1" {
			devMu.Lock()
			devCachedAt = time.Time{}
			devMu.Unlock()
		}
		devs := cachedAudioDevices()
		if devs == nil {
			devs = []string{}
		}
		writeJSON(w, map[string]any{"devices": devs})
	})
	mux.HandleFunc("POST /api/window/hide", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	// enforce the "mic produced nothing in 10s" rule even without a window
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			sess.micFailGuard()
		}
	}()

	if serverOnly {
		fmt.Println(baseURL)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		return
	}

	// Auto-open the popup when a cycle starts from the tray and it is closed.
	go func() {
		prev := "idle"
		for range time.Tick(200 * time.Millisecond) {
			state, _, _ := sess.traySnapshot()
			busy := state == "starting" || state == "recording" || state == "transcribing"
			if busy && prev == "idle" {
				openWindow(baseURL + "/")
			}
			prev = state
		}
	}()

	// Tray owns the main thread (required on macOS; harmless elsewhere).
	if runtime.GOOS == "darwin" {
		runTray(sess, baseURL+"/", nil)
		select {} // caller owns the run loop; keep serving
	}
	runTray(sess, baseURL+"/", nil)
}

// removeEntryFiles deletes the leftover temp wav of an entry, if any.
func removeEntryFiles(e core.Entry) {
	if e.Path == "" {
		return
	}
	if _, err := os.Stat(e.Path); err == nil {
		os.Remove(e.Path)
	}
}

// ffmpegAudioDevices lists DirectShow audio input device names (windows).
func ffmpegAudioDevices() ([]string, error) {
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	cmd := exec.Command(p, "-hide_banner", "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	var out strings.Builder
	cmd.Stderr = &out
	cmd.Run() // always exits non-zero for -list_devices

	var devs []string
	// Newer ffmpeg: each line is  "Name" (audio)  or  "Name" (video).
	// Older ffmpeg: a "DirectShow audio devices" section of quoted lines.
	inAudio := false
	sectioned := false
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "DirectShow audio devices"):
			inAudio = true
			sectioned = true
			continue
		case strings.Contains(line, "DirectShow video devices"):
			inAudio = false
			sectioned = true
			continue
		}
		q := strings.Index(line, `"`)
		if q < 0 {
			continue
		}
		rest := line[q+1:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			continue
		}
		name := rest[:end]
		if name == "dummy" {
			continue
		}
		tail := strings.TrimSpace(rest[end+1:])
		if sectioned {
			// old format: inside the audio section every quoted line is a device
			if inAudio {
				devs = append(devs, name)
			}
			continue
		}
		if strings.HasPrefix(tail, "(audio)") {
			devs = append(devs, name)
		}
	}
	return devs, nil
}

// deviceCache avoids re-running ffmpeg enumeration on every dropdown open.
var (
	devMu       sync.Mutex
	devCached   []string
	devCachedAt time.Time
)

func cachedAudioDevices() []string {
	devMu.Lock()
	defer devMu.Unlock()
	if time.Since(devCachedAt) < time.Minute {
		return devCached
	}
	if runtime.GOOS == "windows" {
		devCached, _ = ffmpegAudioDevices()
	}
	devCachedAt = time.Now()
	return devCached
}
