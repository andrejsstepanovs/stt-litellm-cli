package main

import (
	"bytes"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Configuration is loaded from a .env file (see loadDotEnv) or the environment.
// There are no baked-in defaults for the proxy URL, key, or model.
var (
	litellmURL   string
	litellmKey   string
	defaultModel string
	history      *store
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadDotEnv loads KEY=VALUE pairs into the process environment from an
// STT_ENV_FILE path, a .env in the working directory, and the user config dir.
// Existing environment variables always win. Lines starting with # are ignored.
func loadDotEnv() {
	var paths []string
	if p := os.Getenv("STT_ENV_FILE"); p != "" {
		paths = append(paths, p)
	}
	paths = append(paths, ".env")
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "stt", ".env"))
	}
	for _, p := range paths {
		loadEnvFile(p)
	}
}

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

type recorder struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	buf     *bytes.Buffer
	path    string

	pr *os.File

	mu      sync.Mutex
	peak    int
	parsed  bool
	dataOff int
	bits    int
	isFloat bool
	head    []byte
}

// isDone reports whether the recorder process has exited.
func (r *recorder) isDone() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// samplePeak returns the highest sample level observed since the last call,
// normalized to the 16-bit range. Levels are computed from the recorder's WAV
// stream as it is produced (see pump), so this works regardless of how the
// recorder buffers its output or which sample format it uses.
func (r *recorder) samplePeak() int {
	r.mu.Lock()
	p := r.peak
	r.peak = 0
	r.mu.Unlock()
	return p
}

func (r *recorder) note(b []byte) {
	p := peakSamples(b, r.bits, r.isFloat)
	r.mu.Lock()
	if p > r.peak {
		r.peak = p
	}
	r.mu.Unlock()
}

// consume parses the WAV header from the front of the stream, then samples the
// audio data that follows.
func (r *recorder) consume(b []byte) {
	if !r.parsed {
		r.head = append(r.head, b...)
		off, bits, isFloat, ok := parseWAVHeader(r.head)
		if !ok {
			return
		}
		r.parsed = true
		r.dataOff = off
		r.bits = bits
		r.isFloat = isFloat
		if off < len(r.head) {
			r.note(r.head[off:])
		}
		r.head = nil
		return
	}
	r.note(b)
}

// pump copies the recorder's WAV stream to disk and samples levels in real time.
func (r *recorder) pump() {
	f, ferr := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	buf := make([]byte, 8192)
	for {
		n, err := r.pr.Read(buf)
		if n > 0 {
			if ferr == nil {
				f.Write(buf[:n])
			}
			r.consume(buf[:n])
		}
		if err != nil {
			break
		}
	}
	r.pr.Close()
	if ferr == nil {
		r.finalize(f)
		f.Close()
	}
	r.waitErr = r.cmd.Wait()
	close(r.done)
}

// finalize writes correct RIFF and data chunk sizes, since a recorder writing
// to a pipe cannot seek back to patch the header itself.
func (r *recorder) finalize(f *os.File) {
	if !r.parsed || r.dataOff <= 0 {
		return
	}
	fi, err := f.Stat()
	if err != nil {
		return
	}
	size := fi.Size()
	if size < 8 {
		return
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(size-8))
	f.WriteAt(b[:], 4)
	if size > int64(r.dataOff) {
		binary.LittleEndian.PutUint32(b[:], uint32(size-int64(r.dataOff)))
		f.WriteAt(b[:], int64(r.dataOff)-4)
	}
}

// parseWAVHeader finds the data chunk offset and sample format in a WAV header.
// It returns ok=false until the header is complete.
func parseWAVHeader(b []byte) (dataOff, bits int, isFloat, ok bool) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, false, false
	}
	bits = 16
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		pos += 8
		if id == "fmt " {
			if pos+size > len(b) {
				return 0, 0, false, false
			}
			if size >= 16 {
				format := binary.LittleEndian.Uint16(b[pos : pos+2])
				bits = int(binary.LittleEndian.Uint16(b[pos+14 : pos+16]))
				isFloat = format == 3
				if format == 0xFFFE && size >= 26 {
					isFloat = binary.LittleEndian.Uint16(b[pos+24:pos+26]) == 3
				}
			}
			pos += size + size%2
			continue
		}
		if id == "data" {
			return pos, bits, isFloat, true
		}
		if pos+size > len(b) {
			return 0, 0, false, false
		}
		pos += size + size%2
	}
	return 0, 0, false, false
}

// peakSamples returns the peak absolute sample value in b scaled to 16-bit.
func peakSamples(b []byte, bits int, isFloat bool) int {
	peak := 0.0
	switch {
	case isFloat && bits == 32:
		for i := 0; i+4 <= len(b); i += 4 {
			v := math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i:]))))
			if v > peak {
				peak = v
			}
		}
		return int(peak * 32767)
	case isFloat && bits == 64:
		for i := 0; i+8 <= len(b); i += 8 {
			v := math.Abs(math.Float64frombits(binary.LittleEndian.Uint64(b[i:])))
			if v > peak {
				peak = v
			}
		}
		return int(peak * 32767)
	case bits == 8:
		for i := 0; i < len(b); i++ {
			v := math.Abs(float64(int(b[i]) - 128))
			if v > peak {
				peak = v
			}
		}
		return int(peak * 256)
	case bits == 24:
		for i := 0; i+3 <= len(b); i += 3 {
			v := int32(b[i]) | int32(b[i+1])<<8 | int32(b[i+2])<<16
			if v&0x800000 != 0 {
				v |= ^int32(0xFFFFFF)
			}
			a := math.Abs(float64(v)) / 256
			if a > peak {
				peak = a
			}
		}
		return int(peak)
	case bits == 32:
		for i := 0; i+4 <= len(b); i += 4 {
			a := math.Abs(float64(int32(binary.LittleEndian.Uint32(b[i:])))) / 65536
			if a > peak {
				peak = a
			}
		}
		return int(peak)
	default: // 16-bit
		for i := 0; i+2 <= len(b); i += 2 {
			v := math.Abs(float64(int16(binary.LittleEndian.Uint16(b[i : i+2]))))
			if v > peak {
				peak = v
			}
		}
		return int(peak)
	}
}

func (r *recorder) stop() {
	if r.cmd != nil && r.cmd.Process != nil {
		killSignal(r.cmd.Process.Pid, "-CONT")
		r.cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		if r.cmd != nil && r.cmd.Process != nil {
			r.cmd.Process.Kill()
		}
		<-r.done
	}
}

func (r *recorder) stderrText() string {
	s := strings.TrimSpace(r.buf.String())
	if s == "" {
		return "recorder exited"
	}
	return s
}

func (r *recorder) pause() {
	if r.cmd != nil && r.cmd.Process != nil {
		killSignal(r.cmd.Process.Pid, "-STOP")
	}
}

func (r *recorder) resume() {
	if r.cmd != nil && r.cmd.Process != nil {
		killSignal(r.cmd.Process.Pid, "-CONT")
	}
}

// killSignal sends a signal to a process. POSIX platforms have the `kill`
// utility; elsewhere this is a no-op so builds remain portable.
func killSignal(pid int, sig string) {
	if runtime.GOOS == "windows" {
		return
	}
	exec.Command("kill", sig, strconv.Itoa(pid)).Run()
}

var (
	recMu   sync.Mutex
	activeR *recorder
)

func setActiveRec(r *recorder) {
	recMu.Lock()
	activeR = r
	recMu.Unlock()
}

func currentRec() *recorder {
	recMu.Lock()
	defer recMu.Unlock()
	return activeR
}

func stopActive() {
	recMu.Lock()
	r := activeR
	activeR = nil
	recMu.Unlock()
	if r != nil {
		r.stop()
	}
}

const recReadyBytes = 1024

const (
	audioPeakThreshold = 16
	micWarnAfter       = 2500 * time.Millisecond
	micFailAfter       = 10 * time.Second
)

type recReadyMsg struct{ started time.Time }
type recErrMsg struct{ err error }
type levelMsg struct{ peak int }

// sampleLevel polls the recorder's current input level shortly after.
func sampleLevel(r *recorder) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(100 * time.Millisecond)
		return levelMsg{peak: r.samplePeak()}
	}
}

// waitRecorderReady polls the output file until the recorder is actually
// writing audio, so we only claim to be recording once the mic is live.
func waitRecorderReady(r *recorder) tea.Cmd {
	return func() tea.Msg {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-r.done:
				return recErrMsg{err: fmt.Errorf("microphone error: %s", r.stderrText())}
			case <-ticker.C:
				if fi, err := os.Stat(r.path); err == nil && fi.Size() > recReadyBytes {
					return recReadyMsg{started: time.Now()}
				}
			case <-deadline:
				return recErrMsg{err: fmt.Errorf("microphone did not start (no audio captured within 10s)")}
			}
		}
	}
}

// recordCommand builds the platform-appropriate recording command.
//   - linux:   arecord (ALSA)
//   - darwin:  rec (SoX) if available, otherwise ffmpeg (avfoundation)
//   - windows: ffmpeg (dshow); set STT_DEVICE to the microphone name
//
// The command writes a WAV stream to stdout and finalizes it on SIGINT; the
// caller tees it to disk and samples levels in real time.
func recordCommand() (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		if p, err := exec.LookPath("rec"); err == nil {
			return exec.Command(p, "-q", "-c", "1", "-r", "44100", "-b", "16", "-t", "wav", "-"), nil
		}
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			dev := envOr("STT_DEVICE", ":0")
			return exec.Command(p, "-hide_banner", "-loglevel", "error", "-flush_packets", "1",
				"-f", "avfoundation", "-i", dev, "-ac", "1", "-ar", "44100", "-f", "wav", "-"), nil
		}
		return nil, fmt.Errorf("no recorder found: install SoX (brew install sox) or ffmpeg (brew install ffmpeg)")
	case "windows":
		p, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil, fmt.Errorf("ffmpeg not found: install it and set STT_DEVICE to your microphone name")
		}
		dev := os.Getenv("STT_DEVICE")
		if dev == "" {
			return nil, fmt.Errorf("set STT_DEVICE to a microphone name (list with: ffmpeg -list_devices true -f dshow -i dummy)")
		}
		return exec.Command(p, "-hide_banner", "-loglevel", "error", "-flush_packets", "1",
			"-f", "dshow", "-i", "audio="+dev, "-ac", "1", "-ar", "44100", "-f", "wav", "-"), nil
	default:
		dev := envOr("STT_DEVICE", "pipewire")
		return exec.Command("arecord", "-f", "cd", "-t", "wav", "-q", "-D", dev, "-"), nil
	}
}

func startRecording() (string, error) {
	tmp, err := os.CreateTemp("", "stt-*.wav")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	tmp.Close()

	cmd, err := recordCommand()
	if err != nil {
		os.Remove(name)
		return "", err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		os.Remove(name)
		return "", err
	}
	var buf bytes.Buffer
	cmd.Stdout = pw
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		os.Remove(name)
		return "", fmt.Errorf("recorder: %w", err)
	}
	pw.Close()

	r := &recorder{cmd: cmd, done: make(chan struct{}), buf: &buf, path: name, pr: pr, bits: 16}
	go r.pump()
	setActiveRec(r)
	return name, nil
}

type healthMsg struct {
	ok     bool
	detail string
}

// checkHealth probes the LiteLLM /health endpoint once at startup so the user
// knows before recording whether the proxy (and our model) is actually ready.
func checkHealth() tea.Cmd {
	url := litellmURL + "/health"
	key := litellmKey
	model := defaultModel
	return func() tea.Msg {
		client := &http.Client{Timeout: 6 * time.Second}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return healthMsg{ok: false, detail: err.Error()}
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			return healthMsg{ok: false, detail: err.Error()}
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return healthMsg{ok: false, detail: resp.Status + " — " + firstLine(string(body))}
		}
		var h struct {
			Unhealthy []struct {
				Model string `json:"model"`
			} `json:"unhealthy_endpoints"`
		}
		if json.Unmarshal(body, &h) == nil {
			for _, u := range h.Unhealthy {
				if model == "" || strings.Contains(u.Model, model) {
					return healthMsg{ok: false, detail: "model \"" + model + "\" is unhealthy"}
				}
			}
		}
		return healthMsg{ok: true, detail: "ready"}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 120)
}

func transcribe(path, model string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	fw, err := w.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", err
	}
	audio, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer audio.Close()
	if _, err := io.Copy(fw, audio); err != nil {
		return "", err
	}
	if err := w.WriteField("model", model); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", litellmURL+"/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+litellmKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcription failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return strings.TrimSpace(result.Text), nil
}

var clipboardCmds = [][]string{
	{"wl-copy"},
	{"xclip", "-selection", "clipboard"},
	{"xsel", "--clipboard", "--input"},
	{"pbcopy"},
	{"cmd", "/c", "clip"}, // windows fallback
}

func copyToClipboard(text string) bool {
	// Native clipboard API first (windows, macOS, X11).
	if err := clipboard.WriteAll(text); err == nil {
		return true
	}
	for _, parts := range clipboardCmds {
		c := exec.Command(parts[0], parts[1:]...)
		if runtime.GOOS == "windows" {
			// clip.exe interprets stdin using the console codepage unless a
			// UTF-16LE BOM is present, so transcode to keep non-ASCII intact.
			var buf bytes.Buffer
			buf.WriteString("\xff\xfe")
			for _, r := range text {
				r1, r2 := utf16.EncodeRune(r)
				if r1 == 0xfffd && r2 == 0xfffd {
					r1, r2 = r, 0
				}
				binary.Write(&buf, binary.LittleEndian, uint16(r1))
				if r2 != 0 {
					binary.Write(&buf, binary.LittleEndian, uint16(r2))
				}
			}
			c.Stdin = &buf
		} else {
			c.Stdin = strings.NewReader(text)
		}
		if err := c.Run(); err == nil {
			return true
		}
	}
	return false
}

//go:embed migrations/*.sql
var embedMigrations embed.FS

type store struct {
	db *sql.DB
}

func dbPath() string {
	if p := os.Getenv("STT_DB"); p != "" {
		return p
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "stt", "stt.db")
}

func openStore() (*store, error) {
	path := dbPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

func migrate(db *sql.DB) error {
	goose.SetBaseFS(embedMigrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(db, "migrations")
}

func (s *store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *store) createRecording(audioMs int64, model, path string, audio []byte) int64 {
	if s == nil {
		return 0
	}
	res, err := s.db.Exec(
		`INSERT INTO transcriptions (created_at, audio_ms, transcribe_ms, model, chars, text, status, path, error, audio)
		 VALUES (?, ?, 0, ?, 0, '', 'pending', ?, '', ?)`,
		time.Now().UnixMilli(), audioMs, model, path, audio,
	)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func (s *store) markSuccess(id, transcribeMs int64, text string) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET status='success', transcribe_ms=?, chars=?, text=?, error='', audio=NULL WHERE id=?`,
		transcribeMs, len([]rune(text)), text, id,
	)
}

func (s *store) markFailed(id, transcribeMs int64, errMsg string) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET status='failed', transcribe_ms=?, error=? WHERE id=?`,
		transcribeMs, errMsg, id,
	)
}

// markEmpty records a response that succeeded but produced no text. The audio
// is kept so it can be retried.
func (s *store) markEmpty(id, transcribeMs int64) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET status='empty', transcribe_ms=?, error='empty transcription' WHERE id=?`,
		transcribeMs, id,
	)
}

// updateText stores a manually edited transcript.
func (s *store) updateText(id int64, text string) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET text=?, chars=? WHERE id=?`,
		text, len([]rune(text)), id,
	)
}

type entry struct {
	id         int64
	created    time.Time
	audioMs    int64
	transcribe int64
	text       string
	status     string
	path       string
	errMsg     string
	model      string
}

func (s *store) recent(limit int) []entry {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT id, created_at, audio_ms, transcribe_ms, text, status, path, error, model
		 FROM transcriptions WHERE status='success' ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (s *store) failed(limit int) []entry {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT id, created_at, audio_ms, transcribe_ms, text, status, path, error, model
		 FROM transcriptions WHERE status IN ('failed','pending','empty') ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanEntries(rows)
}

func scanEntries(rows *sql.Rows) []entry {
	var out []entry
	for rows.Next() {
		var (
			id, created, audioMs, transcribeMs int64
			text, status, path, errMsg, model  string
		)
		if err := rows.Scan(&id, &created, &audioMs, &transcribeMs, &text, &status, &path, &errMsg, &model); err != nil {
			return out
		}
		out = append(out, entry{
			id:         id,
			created:    time.UnixMilli(created),
			audioMs:    audioMs,
			transcribe: transcribeMs,
			text:       text,
			status:     status,
			path:       path,
			errMsg:     errMsg,
			model:      model,
		})
	}
	return out
}

func (s *store) audio(id int64) []byte {
	if s == nil || id == 0 {
		return nil
	}
	var data []byte
	s.db.QueryRow(`SELECT audio FROM transcriptions WHERE id=?`, id).Scan(&data)
	return data
}

// point is one observation: audio length x and transcription time y, in ms.
type point struct{ x, y float64 }

// fit is the result of a linear model y = slope*x + intercept.
type fit struct {
	slope     float64
	intercept float64
	n         int
	outliers  int
	r2        float64
	ok        bool
}

// fitPoints builds a least-squares line, iteratively rejecting outliers.
func fitPoints(points []point) fit {
	pts := make([]point, 0, len(points))
	for _, p := range points {
		if p.x > 0 && p.y > 0 && !math.IsNaN(p.x) && !math.IsNaN(p.y) {
			pts = append(pts, p)
		}
	}
	if len(pts) == 0 {
		return fit{}
	}
	if len(pts) == 1 {
		return fit{slope: pts[0].y / pts[0].x, n: 1, ok: true}
	}

	work := append([]point(nil), pts...)
	for iter := 0; iter < 5 && len(work) > 3; iter++ {
		f := ols(work)
		mean, std := residualStats(work, f)
		if std == 0 {
			break
		}
		kept := make([]point, 0, len(work))
		removed := 0
		for _, p := range work {
			r := p.y - (f.slope*p.x + f.intercept)
			if math.Abs(r-mean) > 2.5*std {
				removed++
				continue
			}
			kept = append(kept, p)
		}
		if removed == 0 || len(kept) < 2 {
			break
		}
		work = kept
	}

	f := ols(work)
	if f.slope <= 0 {
		var sx, sy float64
		for _, p := range work {
			sx += p.x
			sy += p.y
		}
		if sx > 0 {
			f = fit{slope: sy / sx, n: len(work)}
		}
	}
	f.n = len(work)
	f.outliers = len(pts) - len(work)
	f.r2 = rSquared(work, f)
	f.ok = true
	return f
}

// ols solves the normal equations for the best-fit line using linear algebra
// (Cramer's rule on the 2x2 system), without an external dependency.
func ols(pts []point) fit {
	n := float64(len(pts))
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		sx += p.x
		sy += p.y
		sxx += p.x * p.x
		sxy += p.x * p.y
	}
	// Normal equations: [n Sx; Sx Sxx] [intercept; slope] = [Sy; Sxy]
	a11, a12, a21, a22 := n, sx, sx, sxx
	b1, b2 := sy, sxy
	det := a11*a22 - a12*a21
	if math.Abs(det) < 1e-9 {
		return fit{intercept: sy / n, n: len(pts)}
	}
	intercept := (b1*a22 - a12*b2) / det
	slope := (a11*b2 - b1*a21) / det
	return fit{slope: slope, intercept: intercept, n: len(pts)}
}

func residualStats(pts []point, f fit) (mean, std float64) {
	if len(pts) == 0 {
		return 0, 0
	}
	var sum float64
	rs := make([]float64, len(pts))
	for i, p := range pts {
		rs[i] = p.y - (f.slope*p.x + f.intercept)
		sum += rs[i]
	}
	mean = sum / float64(len(pts))
	var ss float64
	for _, r := range rs {
		ss += (r - mean) * (r - mean)
	}
	std = math.Sqrt(ss / float64(len(pts)))
	return mean, std
}

func rSquared(pts []point, f fit) float64 {
	if len(pts) < 2 {
		return 0
	}
	var meanY float64
	for _, p := range pts {
		meanY += p.y
	}
	meanY /= float64(len(pts))
	var ssRes, ssTot float64
	for _, p := range pts {
		pred := f.slope*p.x + f.intercept
		ssRes += (p.y - pred) * (p.y - pred)
		ssTot += (p.y - meanY) * (p.y - meanY)
	}
	if ssTot == 0 {
		return 0
	}
	return 1 - ssRes/ssTot
}

func (s *store) points(model string) []point {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT audio_ms, transcribe_ms FROM transcriptions
		 WHERE status='success' AND audio_ms > 0 AND transcribe_ms > 0 AND model = ? ORDER BY id`,
		model,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []point
	for rows.Next() {
		var x, y int64
		if err := rows.Scan(&x, &y); err != nil {
			return out
		}
		out = append(out, point{x: float64(x), y: float64(y)})
	}
	return out
}

// predict estimates transcription time for audioMs and returns the fitted model.
func (s *store) predict(audioMs int64, model string) (int64, fit) {
	f := fitPoints(s.points(model))
	if !f.ok {
		return 0, f
	}
	est := f.slope*float64(audioMs) + f.intercept
	if est < 0 {
		est = 0
	}
	return int64(est), f
}

func (f fit) describe() string {
	if !f.ok {
		return "no data yet"
	}
	s := fmt.Sprintf("t = %.3f·audio + %.0fms   ·   n=%d", f.slope, f.intercept, f.n)
	if f.outliers > 0 {
		s += fmt.Sprintf("   ·   %d outlier(s) removed", f.outliers)
	}
	if f.n >= 2 {
		s += fmt.Sprintf("   ·   r²=%.2f", f.r2)
	}
	return s
}

// printChart renders a scatter plot of past timings and the fitted line.
func (s *store) printChart(w io.Writer, model string) {
	pts := s.points(model)
	if len(pts) == 0 {
		fmt.Fprintln(w, "no successful transcriptions yet")
		return
	}
	f := fitPoints(pts)

	const width, height = 64, 18
	var maxX, maxY float64
	for _, p := range pts {
		if p.x > maxX {
			maxX = p.x
		}
		if p.y > maxY {
			maxY = p.y
		}
	}
	if f.ok {
		for _, x := range []float64{0, maxX} {
			y := f.slope*x + f.intercept
			if y > maxY {
				maxY = y
			}
		}
	}
	if maxX <= 0 {
		maxX = 1
	}
	if maxY <= 0 {
		maxY = 1
	}
	maxX *= 1.05
	maxY *= 1.1

	grid := make([][]byte, height)
	for i := range grid {
		grid[i] = bytes.Repeat([]byte{' '}, width)
	}
	col := func(x float64) int { return int(x/maxX*float64(width-1) + 0.5) }
	row := func(y float64) int {
		if y < 0 {
			y = 0
		}
		return height - 1 - int(y/maxY*float64(height-1)+0.5)
	}

	if f.ok {
		for c := 0; c < width; c++ {
			x := float64(c) / float64(width-1) * maxX
			r := row(f.slope*x + f.intercept)
			if r >= 0 && r < height {
				grid[r][c] = '-'
			}
		}
	}
	for _, p := range pts {
		c, r := col(p.x), row(p.y)
		if c >= 0 && c < width && r >= 0 && r < height {
			grid[r][c] = '*'
		}
	}

	for i, line := range grid {
		yv := maxY * (1 - float64(i)/float64(height-1))
		fmt.Fprintf(w, "%6.2fs | %s\n", yv/1000, string(line))
	}
	fmt.Fprintf(w, "        +%s\n", strings.Repeat("-", width))
	fmt.Fprintf(w, "         0s%-*s%.1fs\n", width-10, "", maxX/1000)
	fmt.Fprintf(w, "\nmodel: %s\n", f.describe())
	fmt.Fprintf(w, "legend: * transcription, - least-squares fit (outliers rejected)\n")
}

// wavDurationMs reads a PCM WAV file's header and returns its duration in
// milliseconds. It returns an error if the format cannot be determined.
func wavDurationMs(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	header := make([]byte, 12)
	if _, err := io.ReadFull(f, header); err != nil {
		return 0, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, fmt.Errorf("not a WAV file")
	}

	var sampleRate, channels, bits, dataSize int64
	for {
		var chunk [8]byte
		if _, err := io.ReadFull(f, chunk[:]); err != nil {
			break
		}
		id := string(chunk[0:4])
		size := int64(binary.LittleEndian.Uint32(chunk[4:8]))

		if id == "fmt " {
			fmtData := make([]byte, size)
			if _, err := io.ReadFull(f, fmtData); err != nil {
				return 0, err
			}
			if len(fmtData) >= 16 {
				channels = int64(binary.LittleEndian.Uint16(fmtData[2:4]))
				sampleRate = int64(binary.LittleEndian.Uint32(fmtData[4:8]))
				bits = int64(binary.LittleEndian.Uint16(fmtData[14:16]))
			}
			if size%2 == 1 {
				f.Seek(1, io.SeekCurrent)
			}
			continue
		}
		if id == "data" {
			dataSize = size
			break
		}
		if _, err := f.Seek(size+size%2, io.SeekCurrent); err != nil {
			break
		}
	}

	byteRate := sampleRate * channels * bits / 8
	if byteRate <= 0 || dataSize <= 0 {
		return 0, fmt.Errorf("invalid WAV format")
	}
	return dataSize * 1000 / byteRate, nil
}

type state int

const (
	stIdle state = iota
	stStarting
	stRecording
	stTranscribing
	stResult
	stEditing
	stHistory
)

type txDoneMsg struct {
	text       string
	err        error
	transcribe int64
}

type tickMsg time.Time

type model struct {
	state       state
	spinner     spinner.Model
	prog        progress.Model
	ta          textarea.Model
	start       time.Time
	wav         string
	text        string
	err         error
	model       string
	paused      bool
	pausedAt    time.Time
	pausedTotal time.Duration
	audioMs     int64
	estMs       int64
	estFit      fit
	estOK       bool
	actualMs    int64
	empty       bool
	audioDetect bool
	meter       []int
	dbWarn      string

	healthChecked bool
	healthOK      bool
	healthDetail  string

	width      int
	height     int
	hist       []entry
	histCursor int
	histOffset int
	histMsg    string
	listFailed bool
	recID      int64
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	keyStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	recStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	txStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("45"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	textStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	recSpinnerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	txSpinnerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
)

func initialModel(modelName string) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	pr := progress.New(progress.WithSolidFill("#00d7ff"))
	pr.Width = 40
	pr.ShowPercentage = true

	ta := textarea.New()
	ta.Placeholder = "Edit the transcript…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetWidth(60)
	ta.SetHeight(8)
	// Enter is reserved for saving; use alt+enter (or ctrl+j) for a newline.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))

	return model{state: stIdle, spinner: s, prog: pr, ta: ta, model: modelName, width: 80, height: 24}
}

func (m model) Init() tea.Cmd { return checkHealth() }

func tick() tea.Cmd {
	return tea.Tick(20*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func transcribeCmd(path, modelName string, audioMs, recID int64) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		text, err := transcribe(path, modelName)
		txMs := time.Since(start).Milliseconds()
		switch {
		case err != nil:
			history.markFailed(recID, txMs, err.Error())
		case strings.TrimSpace(text) == "":
			history.markEmpty(recID, txMs)
		default:
			history.markSuccess(recID, txMs, text)
		}
		return txDoneMsg{text: text, err: err, transcribe: txMs}
	}
}

func beginRecording(m model) (tea.Model, tea.Cmd) {
	wav, err := startRecording()
	if err != nil {
		m.err = err
		m.state = stResult
		return m, nil
	}
	r := currentRec()
	m.wav = wav
	m.recID = 0
	m.text = ""
	m.err = nil
	m.empty = false
	m.audioDetect = false
	m.start = time.Now()
	m.paused = false
	m.pausedTotal = 0
	m.pausedAt = time.Time{}
	m.spinner.Style = recSpinnerStyle
	m.state = stStarting
	return m, tea.Batch(m.spinner.Tick, waitRecorderReady(r))
}

func openHistory(m model) (tea.Model, tea.Cmd) {
	m.hist = history.recent(200)
	m.histCursor = 0
	m.histOffset = 0
	m.histMsg = ""
	m.listFailed = false
	m.state = stHistory
	return m, nil
}

func (m *model) resizeEditor() {
	w := m.width - 4
	if w < 20 {
		w = 20
	}
	h := m.height - 8
	if h < 3 {
		h = 3
	}
	m.ta.SetWidth(w)
	m.ta.SetHeight(h)
}

func openEditor(m model) (tea.Model, tea.Cmd) {
	m.ta.SetValue(m.text)
	m.ta.CursorEnd()
	m.resizeEditor()
	cmd := m.ta.Focus()
	m.state = stEditing
	return m, cmd
}

func openFailed(m model) (tea.Model, tea.Cmd) {
	m.hist = history.failed(200)
	m.histCursor = 0
	m.histOffset = 0
	m.histMsg = ""
	m.listFailed = true
	m.state = stHistory
	return m, nil
}

func loadFailed(m model, e entry) (tea.Model, tea.Cmd) {
	path := e.path
	if fi, err := os.Stat(path); err != nil || fi.Size() < 1000 {
		if data := history.audio(e.id); len(data) > 0 {
			if tmp, err := os.CreateTemp("", "stt-*.wav"); err == nil {
				tmp.Write(data)
				tmp.Close()
				path = tmp.Name()
			}
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() < 1000 {
		m.histMsg = errStyle.Render("audio unavailable for this recording")
		return m, nil
	}
	if e.model != "" {
		m.model = e.model
	}
	m.wav = path
	m.audioMs = e.audioMs
	m.recID = e.id
	return retryTranscribe(m)
}

func retryTranscribe(m model) (tea.Model, tea.Cmd) {
	m.text = ""
	m.err = nil
	m.empty = false
	m.estMs, m.estFit = history.predict(m.audioMs, m.model)
	m.estOK = m.estFit.ok
	m.state = stTranscribing
	m.start = time.Now()
	m.spinner.Style = txSpinnerStyle
	return m, tea.Batch(m.spinner.Tick, tick(), transcribeCmd(m.wav, m.model, m.audioMs, m.recID))
}

func (m model) recElapsed() time.Duration {
	d := time.Since(m.start) - m.pausedTotal
	if m.paused {
		d -= time.Since(m.pausedAt)
	}
	if d < 0 {
		d = 0
	}
	return d
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
			w := msg.Width - 24
			if w > 60 {
				w = 60
			}
			if w < 10 {
				w = 10
			}
			m.prog.Width = w
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
		m.resizeEditor()
		return m, nil

	case healthMsg:
		m.healthChecked = true
		m.healthOK = msg.ok
		m.healthDetail = msg.detail
		return m, nil

	case tickMsg:
		if m.state == stStarting || m.state == stRecording || m.state == stTranscribing {
			return m, tick()
		}
		return m, nil

	case spinner.TickMsg:
		if m.state == stStarting || m.state == stRecording || m.state == stTranscribing {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case recReadyMsg:
		if m.state == stStarting {
			m.start = msg.started
			m.paused = false
			m.pausedTotal = 0
			m.audioDetect = false
			m.meter = m.meter[:0]
			m.spinner.Style = recSpinnerStyle
			m.state = stRecording
			r := currentRec()
			if r == nil {
				return m, tick()
			}
			return m, tea.Batch(tick(), sampleLevel(r))
		}
		return m, nil

	case levelMsg:
		if m.state != stRecording {
			return m, nil
		}
		r := currentRec()
		if r == nil || r.isDone() {
			m.err = fmt.Errorf("recorder stopped unexpectedly — is another app using the microphone?")
			m.state = stResult
			return m, nil
		}
		m.audioDetect = m.audioDetect || msg.peak > audioPeakThreshold
		m.pushLevel(msg.peak)
		if !m.audioDetect && m.recElapsed() >= micFailAfter {
			stopActive()
			if m.wav != "" {
				os.Remove(m.wav)
				m.wav = ""
			}
			m.err = fmt.Errorf("no audio captured in the first 10s — check your microphone and try again")
			m.state = stResult
			return m, nil
		}
		return m, sampleLevel(r)

	case recErrMsg:
		if m.state == stStarting || m.state == stRecording {
			stopActive()
			if m.wav != "" {
				os.Remove(m.wav)
				m.wav = ""
			}
			m.err = msg.err
			m.state = stResult
		}
		return m, nil

	case txDoneMsg:
		m.text = msg.text
		m.err = msg.err
		m.actualMs = msg.transcribe
		m.empty = msg.err == nil && msg.text == ""
		m.state = stResult
		if msg.err == nil && !m.empty {
			copyToClipboard(msg.text)
			if m.wav != "" {
				os.Remove(m.wav)
				m.wav = ""
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch m.state {
		case stIdle:
			switch msg.String() {
			case "r":
				return beginRecording(m)
			case "h", "H":
				return openHistory(m)
			case "l", "L":
				return openFailed(m)
			case "q", "ctrl+c", "esc":
				return m, tea.Quit
			}
		case stResult:
			retryable := (m.err != nil || m.empty) && m.wav != ""
			switch msg.String() {
			case "r":
				if retryable {
					return retryTranscribe(m)
				}
				return beginRecording(m)
			case "n":
				if retryable {
					if m.wav != "" {
						os.Remove(m.wav)
						m.wav = ""
					}
					return beginRecording(m)
				}
			case "h", "H":
				return openHistory(m)
			case "l", "L":
				return openFailed(m)
			case "e", "E":
				if !retryable && m.err == nil && m.text != "" {
					return openEditor(m)
				}
			case "q", "ctrl+c", "esc":
				return m, tea.Quit
			}
		case stEditing:
			switch msg.String() {
			case "enter":
				m.text = strings.TrimSpace(m.ta.Value())
				history.updateText(m.recID, m.text)
				copyToClipboard(m.text)
				m.ta.Blur()
				m.empty = false
				m.state = stResult
				return m, nil
			case "esc":
				m.ta.Blur()
				m.state = stResult
				return m, nil
			case "ctrl+c":
				return m, tea.Quit
			default:
				var cmd tea.Cmd
				m.ta, cmd = m.ta.Update(msg)
				return m, cmd
			}
		case stHistory:
			switch msg.String() {
			case "up", "k":
				if m.histCursor > 0 {
					m.histCursor--
				}
				m.histMsg = ""
				m.clampOffset()
			case "down", "j":
				if m.histCursor < len(m.hist)-1 {
					m.histCursor++
				}
				m.histMsg = ""
				m.clampOffset()
			case "enter", " ":
				if len(m.hist) > 0 {
					if m.listFailed {
						return loadFailed(m, m.hist[m.histCursor])
					}
					if copyToClipboard(m.hist[m.histCursor].text) {
						m.histMsg = okStyle.Render("✓ copied to clipboard")
					} else {
						m.histMsg = errStyle.Render("no clipboard tool found")
					}
				}
			case "h", "esc":
				m.state = stIdle
			case "q", "ctrl+c":
				return m, tea.Quit
			}
		case stStarting:
			switch msg.String() {
			case " ":
				stopActive()
				if m.wav != "" {
					os.Remove(m.wav)
					m.wav = ""
				}
				m.state = stIdle
			case "q", "ctrl+c", "esc":
				stopActive()
				if m.wav != "" {
					os.Remove(m.wav)
				}
				return m, tea.Quit
			}
		case stRecording:
			switch msg.String() {
			case " ":
				stopActive()
				if fi, err := os.Stat(m.wav); err != nil || fi.Size() < 1000 {
					os.Remove(m.wav)
					m.wav = ""
					m.empty = false
					m.err = fmt.Errorf("no audio captured (is the mic busy?)")
					m.state = stResult
					return m, nil
				}
				m.audioMs, _ = wavDurationMs(m.wav)
				if m.audioMs <= 0 {
					m.audioMs = m.recElapsed().Milliseconds()
				}
				m.estMs, m.estFit = history.predict(m.audioMs, m.model)
				m.estOK = m.estFit.ok
				audio, _ := os.ReadFile(m.wav)
				m.recID = history.createRecording(m.audioMs, m.model, m.wav, audio)
				m.text = ""
				m.err = nil
				m.empty = false
				m.state = stTranscribing
				m.start = time.Now()
				m.spinner.Style = txSpinnerStyle
				return m, tea.Batch(m.spinner.Tick, tick(), transcribeCmd(m.wav, m.model, m.audioMs, m.recID))
			case "p", "P":
				r := currentRec()
				if r == nil {
					return m, nil
				}
				if m.paused {
					r.resume()
					m.pausedTotal += time.Since(m.pausedAt)
					m.paused = false
				} else {
					r.pause()
					m.pausedAt = time.Now()
					m.paused = true
				}
				return m, nil
			case "q", "ctrl+c", "esc":
				stopActive()
				if m.wav != "" {
					os.Remove(m.wav)
				}
				return m, tea.Quit
			}
		case stTranscribing:
			if msg.String() == "q" || msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func fmtElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.3fs", d.Seconds())
}

func fmtSeconds(ms int64) string {
	return fmt.Sprintf("%.3fs", float64(ms)/1000)
}

func plural(n int) string {
	if n == 1 {
		return "1 recording"
	}
	return fmt.Sprintf("%d recordings", n)
}

func (m *model) clampOffset() {
	vis := m.height - 6
	if vis < 1 {
		vis = 1
	}
	if m.histCursor < m.histOffset {
		m.histOffset = m.histCursor
	}
	if m.histCursor >= m.histOffset+vis {
		m.histOffset = m.histCursor - vis + 1
	}
	if m.histOffset < 0 {
		m.histOffset = 0
	}
}

func truncate(s string, width int) string {
	if width < 4 {
		width = 4
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}

// renderMarkdown renders arbitrary text (typically a plain transcript) wrapped
// to the given width using Glamour. On any failure it falls back to raw text.
func renderMarkdown(s string, width int) string {
	if width < 20 {
		width = 20
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return s
	}
	out, err := r.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

func (m *model) pushLevel(peak int) {
	width := (m.width - 24) * 3 / 10
	if width < 8 {
		width = 8
	}
	if width > 45 {
		width = 45
	}
	m.meter = append(m.meter, levelFromPeak(peak))
	if len(m.meter) > width {
		m.meter = m.meter[len(m.meter)-width:]
	}
}

// levelFromPeak maps a raw 16-bit peak to 0..8 for the equalizer (log scale).
func levelFromPeak(peak int) int {
	const lo, hi = 16.0, 32767.0
	if float64(peak) <= lo {
		return 0
	}
	x := (math.Log10(float64(peak)) - math.Log10(lo)) / (math.Log10(hi) - math.Log10(lo))
	lvl := 1 + int(x*7)
	if lvl < 1 {
		lvl = 1
	}
	if lvl > 8 {
		lvl = 8
	}
	return lvl
}

var meterRunes = []rune(" ▁▂▃▄▅▆▇█")

func (m model) meterView() string {
	if len(m.meter) == 0 {
		return ""
	}
	var b strings.Builder
	for _, v := range m.meter {
		if v < 0 {
			v = 0
		}
		if v >= len(meterRunes) {
			v = len(meterRunes) - 1
		}
		b.WriteRune(meterRunes[v])
	}
	return meterStyle(m.meter[len(m.meter)-1]).Render(b.String())
}

func meterStyle(level int) lipgloss.Style {
	switch {
	case level >= 7:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	case level >= 4:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	}
}

func (m model) healthView() string {
	switch {
	case !m.healthChecked:
		return dimStyle.Render("litellm: checking…")
	case m.healthOK:
		return okStyle.Render("✓ litellm ready")
	default:
		return errStyle.Render("✗ litellm not ready: " + m.healthDetail)
	}
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("stt") + dimStyle.Render("  speech to text  ·  parakeet via litellm  ·  "+version) + "\n\n")

	switch m.state {
	case stIdle:
		b.WriteString("  " + m.healthView() + "\n\n")
		b.WriteString("  " + keyStyle.Render("[r]") + " record    " + keyStyle.Render("[h]") + " history    " + keyStyle.Render("[l]") + " load failed    " + keyStyle.Render("[q]") + " quit\n")

	case stHistory:
		title := "History"
		if m.listFailed {
			title = "Failed recordings"
		}
		b.WriteString("  " + titleStyle.Render(title) + dimStyle.Render("  "+plural(len(m.hist))) + "\n\n")
		if len(m.hist) == 0 {
			if m.listFailed {
				b.WriteString("  " + dimStyle.Render("no failed recordings") + "\n")
			} else {
				b.WriteString("  " + dimStyle.Render("no transcriptions yet") + "\n")
			}
		} else {
			vis := m.height - 8
			if vis < 1 {
				vis = 1
			}
			end := m.histOffset + vis
			if end > len(m.hist) {
				end = len(m.hist)
			}
			for i := m.histOffset; i < end; i++ {
				e := m.hist[i]
				ts := e.created.Format("2006-01-02 15:04:05")
				var label string
				if m.listFailed {
					label = e.errMsg
					if label == "" {
						label = e.status
					}
				} else {
					label = e.text
				}
				label = truncate(label, m.width-26)
				if i == m.histCursor {
					b.WriteString("  " + keyStyle.Render("▶ ") + textStyle.Bold(true).Render(ts+"  "+label) + "\n")
				} else {
					b.WriteString("    " + dimStyle.Render(ts) + "  " + textStyle.Render(label) + "\n")
				}
			}
		}
		b.WriteString("\n")
		if m.histMsg != "" {
			b.WriteString("  " + m.histMsg + "\n")
		}
		if m.listFailed {
			b.WriteString("  " + dimStyle.Render("↑/↓ select   ·   enter retry   ·   h/esc back") + "\n")
		} else {
			b.WriteString("  " + dimStyle.Render("↑/↓ select   ·   enter copy   ·   h/esc back") + "\n")
		}

	case stStarting:
		b.WriteString("  " + m.spinner.View() + " " + titleStyle.Render("Initializing microphone…") + "\n")
		b.WriteString("  " + dimStyle.Render("waiting for the device to start capturing   ·   q to cancel") + "\n")

	case stRecording:
		if m.paused {
			b.WriteString("  " + okStyle.Render("⏸ Paused") + "  " + titleStyle.Render(fmtElapsed(m.recElapsed())) + "\n")
			b.WriteString("  " + dimStyle.Render("p to resume   ·   space to stop   ·   q to cancel") + "\n")
		} else {
			b.WriteString("  " + m.spinner.View() + " " + recStyle.Render("Recording") + "  " + titleStyle.Render(fmtElapsed(m.recElapsed())) + "\n")
			b.WriteString("  " + dimStyle.Render("space to stop   ·   p to pause   ·   q to cancel") + "\n")
		}
		if mv := m.meterView(); mv != "" {
			b.WriteString("  mic " + mv + "\n")
		}
		if !m.audioDetect && m.recElapsed() > micWarnAfter {
			b.WriteString("  " + warnStyle.Render("no audio detected — check your microphone") + "\n")
		}

	case stTranscribing:
		elapsed := time.Since(m.start)
		b.WriteString("  " + m.spinner.View() + " " + txStyle.Render("Transcribing") + "  " + txStyle.Render(fmtElapsed(elapsed)) + "\n")
		if m.estOK && m.estMs > 0 {
			pct := float64(elapsed.Milliseconds()) / float64(m.estMs)
			if pct > 0.99 {
				pct = 0.99
			}
			if pct < 0 {
				pct = 0
			}
			b.WriteString("  " + m.prog.ViewAs(pct) + "\n")
			b.WriteString("  " + dimStyle.Render(m.estFit.describe()) + "\n")
		} else {
			b.WriteString("  " + dimStyle.Render("estimating from history (none yet)") + "\n")
		}

	case stEditing:
		b.WriteString("  " + titleStyle.Render("Edit transcript") + "\n\n")
		b.WriteString(m.ta.View() + "\n")
		b.WriteString("  " + dimStyle.Render("enter save   ·   alt+enter newline   ·   esc cancel") + "\n")

	case stResult:
		retryable := (m.err != nil || m.empty) && m.wav != ""
		editable := !retryable && m.err == nil && m.text != ""
		if m.err != nil {
			b.WriteString("  " + errStyle.Render("Error: "+m.err.Error()) + "\n")
		} else if m.empty {
			b.WriteString("  " + warnStyle.Render("nothing transcribed — audio kept, you can retry") + "\n")
		} else {
			b.WriteString(renderMarkdown(m.text, m.width-6))
			b.WriteString("\n\n  " + okStyle.Render("✓ copied to clipboard") + "\n")
		}
		if m.audioMs > 0 {
			b.WriteString("  " + dimStyle.Render(fmt.Sprintf("audio %s · transcribed %s", fmtSeconds(m.audioMs), fmtSeconds(m.actualMs))) + "\n")
		}
		if retryable {
			b.WriteString("  " + dimStyle.Render("audio saved: "+m.wav) + "\n")
		}
		b.WriteString("\n")
		if retryable {
			b.WriteString("  " + keyStyle.Render("[r]") + " retry    " + keyStyle.Render("[n]") + " new recording    " + keyStyle.Render("[h]") + " history    " + keyStyle.Render("[l]") + " load failed    " + keyStyle.Render("[q]") + " quit\n")
		} else {
			editHint := ""
			if editable {
				editHint = keyStyle.Render("[e]") + " edit    "
			}
			b.WriteString("  " + editHint + keyStyle.Render("[r]") + " record again    " + keyStyle.Render("[h]") + " history    " + keyStyle.Render("[l]") + " load failed    " + keyStyle.Render("[q]") + " quit\n")
		}
	}

	return b.String()
}

func main() {
	loadDotEnv()
	litellmURL = os.Getenv("STT_LITELLM_URL")
	litellmKey = os.Getenv("STT_LITELLM_KEY")
	defaultModel = os.Getenv("STT_MODEL")

	modelName := flag.String("m", defaultModel, "transcription model")
	chart := flag.Bool("chart", false, "print the timing-model chart and exit")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("stt %s\n", version)
		if commit != "none" {
			fmt.Printf("commit: %s\n", commit)
		}
		if date != "unknown" {
			fmt.Printf("built:  %s\n", date)
		}
		return
	}

	h, err := openStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: history disabled: %v\n", err)
	} else {
		history = h
		defer history.Close()
	}

	if *chart {
		history.printChart(os.Stdout, *modelName)
		return
	}

	var missing []string
	if litellmURL == "" {
		missing = append(missing, "STT_LITELLM_URL")
	}
	if litellmKey == "" {
		missing = append(missing, "STT_LITELLM_KEY")
	}
	if *modelName == "" {
		missing = append(missing, "STT_MODEL")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "missing configuration: %s\n\n"+
			"Set them in a .env file next to the working directory (see .env.example),\n"+
			"in $XDG_CONFIG_HOME/stt/.env, or in the environment.\n",
			strings.Join(missing, ", "))
		os.Exit(1)
	}

	p := tea.NewProgram(initialModel(*modelName), tea.WithAltScreen())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	go func() {
		<-sig
		stopActive()
		p.ReleaseTerminal()
		os.Exit(0)
	}()

	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if m, ok := finalModel.(model); ok && m.text != "" {
		fmt.Println(m.text)
	}
}
