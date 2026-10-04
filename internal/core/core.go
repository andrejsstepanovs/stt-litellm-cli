// Package core holds the stt engine shared by the TUI and the Windows tray
// app: microphone recording, LiteLLM transcription, the history store, the
// clipboard helper and configuration loading.
package core

import (
	"bytes"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/atotto/clipboard"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// Configuration is loaded from a .env file (see LoadDotEnv) or the
// environment. There are no baked-in defaults for the proxy URL or key.
var (
	litellmURL string
	litellmKey string
)

// SetConfig points the transcription and health-check helpers at a proxy.
func SetConfig(url, key string) {
	litellmURL = url
	litellmKey = key
}

// BaseURL returns the configured proxy URL ("" when unset).
func BaseURL() string { return litellmURL }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LoadDotEnv loads KEY=VALUE pairs into the process environment from an
// STT_ENV_FILE path, a .env next to the executable, a .env in the working
// directory, and the user config dir. Existing environment variables always
// win. Lines starting with # are ignored.
func LoadDotEnv() {
	var paths []string
	if p := os.Getenv("STT_ENV_FILE"); p != "" {
		paths = append(paths, p)
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), ".env"))
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

const RecReadyBytes = 1024

const (
	AudioPeakThreshold = 16
	MicWarnAfter       = 2500 * time.Millisecond
	MicFailAfter       = 10 * time.Second
)

type Recorder struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	buf     *bytes.Buffer
	path    string

	pr    *os.File
	stdin io.WriteCloser

	mu      sync.Mutex
	peak    int
	detect  bool
	parsed  bool
	dataOff int
	bits    int
	isFloat bool
	head    []byte
}

// IsDone reports whether the recorder process has exited.
func (r *Recorder) IsDone() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// Done exposes the channel closed when the recorder process has exited.
func (r *Recorder) Done() <-chan struct{} { return r.done }

// Path returns the WAV file the recorder is writing to.
func (r *Recorder) Path() string { return r.path }

// SamplePeak returns the highest sample level observed since the last call,
// normalized to the 16-bit range. Levels are computed from the recorder's WAV
// stream as it is produced (see pump), so this works regardless of how the
// recorder buffers its output or which sample format it uses.
func (r *Recorder) SamplePeak() int {
	r.mu.Lock()
	p := r.peak
	r.peak = 0
	r.mu.Unlock()
	return p
}

// AudioDetected reports whether any sample above AudioPeakThreshold has been
// seen since the recorder started. Unlike SamplePeak this is cumulative, so
// it is safe to check from multiple consumers.
func (r *Recorder) AudioDetected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.detect
}

func (r *Recorder) note(b []byte) {
	p := peakSamples(b, r.bits, r.isFloat)
	r.mu.Lock()
	if p > r.peak {
		r.peak = p
	}
	if p > AudioPeakThreshold {
		r.detect = true
	}
	r.mu.Unlock()
}

// consume parses the WAV header from the front of the stream, then samples the
// audio data that follows.
func (r *Recorder) consume(b []byte) {
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
func (r *Recorder) pump() {
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
func (r *Recorder) finalize(f *os.File) {
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

// Stop ends the recording and waits for the WAV file to be finalized. On
// POSIX the recorder gets SIGINT; ffmpeg on Windows is asked to quit via the
// 'q' command on stdin (console signals are not available to us), falling
// back to a hard kill.
func (r *Recorder) Stop() {
	if r.cmd != nil && r.cmd.Process != nil {
		if runtime.GOOS == "windows" {
			// a suspended recorder would never see the quit command
			resumeProcess(r.cmd.Process.Pid)
		} else {
			killSignal(r.cmd.Process.Pid, "-CONT")
		}
		if runtime.GOOS == "windows" && r.stdin != nil {
			r.stdin.Write([]byte("q"))
			r.stdin.Close()
			r.stdin = nil
		} else if r.cmd != nil && r.cmd.Process != nil {
			r.cmd.Process.Signal(os.Interrupt)
		}
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

func (r *Recorder) StderrText() string {
	s := strings.TrimSpace(r.buf.String())
	if s == "" {
		return "recorder exited"
	}
	return s
}

func (r *Recorder) Pause() {
	if r.cmd == nil || r.cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		suspendProcess(r.cmd.Process.Pid)
		return
	}
	killSignal(r.cmd.Process.Pid, "-STOP")
}

func (r *Recorder) Resume() {
	if r.cmd == nil || r.cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		resumeProcess(r.cmd.Process.Pid)
		return
	}
	killSignal(r.cmd.Process.Pid, "-CONT")
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
	activeR *Recorder
)

func setActive(r *Recorder) {
	recMu.Lock()
	activeR = r
	recMu.Unlock()
}

// CurrentRecorder returns the active recorder, if any.
func CurrentRecorder() *Recorder {
	recMu.Lock()
	defer recMu.Unlock()
	return activeR
}

// StopActive stops the active recorder, if any.
func StopActive() {
	recMu.Lock()
	r := activeR
	activeR = nil
	recMu.Unlock()
	if r != nil {
		r.Stop()
	}
}

// recordCommand builds the platform-appropriate recording command.
//   - linux:   arecord (ALSA)
//   - darwin:  rec (SoX) if available, otherwise ffmpeg (avfoundation)
//   - windows: ffmpeg (dshow); device is the microphone name
//
// The command writes a WAV stream to stdout and finalizes it on SIGINT; the
// caller tees it to disk and samples levels in real time.
func recordCommand(device string) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		if p, err := exec.LookPath("rec"); err == nil {
			return exec.Command(p, "-q", "-c", "1", "-r", "44100", "-b", "16", "-t", "wav", "-"), nil
		}
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			if device == "" {
				device = envOr("STT_DEVICE", ":0")
			}
			return exec.Command(p, "-hide_banner", "-loglevel", "error", "-flush_packets", "1",
				"-f", "avfoundation", "-i", device, "-ac", "1", "-ar", "44100", "-f", "wav", "-"), nil
		}
		return nil, fmt.Errorf("no recorder found: install SoX (brew install sox) or ffmpeg (brew install ffmpeg)")
	case "windows":
		p, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil, fmt.Errorf("ffmpeg not found: install it and set STT_DEVICE to your microphone name")
		}
		if device == "" {
			device = os.Getenv("STT_DEVICE")
		}
		if device == "" {
			return nil, fmt.Errorf("set STT_DEVICE to a microphone name (list with: ffmpeg -list_devices true -f dshow -i dummy)")
		}
		return exec.Command(p, "-hide_banner", "-loglevel", "error", "-flush_packets", "1",
			"-f", "dshow", "-i", "audio="+device, "-ac", "1", "-ar", "44100", "-f", "wav", "-"), nil
	default:
		if device == "" {
			device = envOr("STT_DEVICE", "pipewire")
		}
		return exec.Command("arecord", "-f", "cd", "-t", "wav", "-q", "-D", device, "-"), nil
	}
}

// StartRecording launches the recorder writing to a temporary WAV file.
// An empty device name falls back to the STT_DEVICE environment variable and
// the platform default.
func StartRecording(device string) (string, error) {
	tmp, err := os.CreateTemp("", "stt-*.wav")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	tmp.Close()

	cmd, err := recordCommand(device)
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
	// ffmpeg on Windows has no usable console signal, so keep a stdin pipe
	// open for the 'q' quit command (see Recorder.Stop).
	var stdin io.WriteCloser
	if runtime.GOOS == "windows" {
		stdin, _ = cmd.StdinPipe()
	}
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		os.Remove(name)
		return "", fmt.Errorf("recorder: %w", err)
	}
	pw.Close()

	r := &Recorder{cmd: cmd, done: make(chan struct{}), buf: &buf, path: name, pr: pr, bits: 16}
	r.stdin = stdin
	go r.pump()
	setActive(r)
	return name, nil
}

// Shared clients so repeated calls reuse pooled connections instead of
// opening a fresh TCP+TLS handshake per request.
var (
	healthClient = &http.Client{Timeout: 6 * time.Second}
	txClient     = &http.Client{Timeout: 120 * time.Second}
)

// HealthCheck probes the LiteLLM /health endpoint so the user knows before
// recording whether the proxy (and our model) is actually ready.
func HealthCheck() (bool, string) {
	url := litellmURL + "/health"
	key := litellmKey
	model := os.Getenv("STT_MODEL")

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := healthClient.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, resp.Status + " — " + firstLine(string(body))
	}
	var h struct {
		Unhealthy []struct {
			Model string `json:"model"`
		} `json:"unhealthy_endpoints"`
	}
	if json.Unmarshal(body, &h) == nil {
		for _, u := range h.Unhealthy {
			if model == "" || strings.Contains(u.Model, model) {
				return false, "model \"" + model + "\" is unhealthy"
			}
		}
	}
	return true, "ready"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:119] + "…"
	}
	return s
}

// Transcribe sends a WAV file to the configured proxy and returns the text.
func Transcribe(path, model string) (string, error) {
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

	resp, err := txClient.Do(req)
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

// CopyToClipboard copies text using the native clipboard API (windows, macOS,
// X11), falling back to external tools.
func CopyToClipboard(text string) bool {
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

// DBPath returns the location of the shared history database.
func DBPath() string { return dbPath() }

// OpenStore opens (and migrates) the shared history database.
func OpenStore() (*Store, error) {
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
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	goose.SetBaseFS(embedMigrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(db, "migrations")
}

// Store is the shared transcription history database.
type Store struct {
	db *sql.DB
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) CreateRecording(audioMs int64, model, path string, audio []byte) int64 {
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

func (s *Store) MarkSuccess(id, transcribeMs int64, text string) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET status='success', transcribe_ms=?, chars=?, text=?, error='', audio=NULL WHERE id=?`,
		transcribeMs, len([]rune(text)), text, id,
	)
}

func (s *Store) MarkFailed(id, transcribeMs int64, errMsg string) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET status='failed', transcribe_ms=?, error=? WHERE id=?`,
		transcribeMs, errMsg, id,
	)
}

// MarkEmpty records a response that succeeded but produced no text. The audio
// is kept so it can be retried.
func (s *Store) MarkEmpty(id, transcribeMs int64) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET status='empty', transcribe_ms=?, error='empty transcription' WHERE id=?`,
		transcribeMs, id,
	)
}

// UpdateText stores a manually edited transcript.
func (s *Store) UpdateText(id int64, text string) {
	if s == nil || id == 0 {
		return
	}
	s.db.Exec(
		`UPDATE transcriptions SET text=?, chars=? WHERE id=?`,
		text, len([]rune(text)), id,
	)
}

// Entry is one row of the transcription history.
type Entry struct {
	ID         int64
	Created    time.Time
	AudioMs    int64
	Transcribe int64
	Text       string
	Status     string
	Path       string
	ErrMsg     string
	Model      string
}

func (s *Store) Recent(limit int) []Entry {
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

func (s *Store) Failed(limit int) []Entry {
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

func scanEntries(rows *sql.Rows) []Entry {
	var out []Entry
	for rows.Next() {
		var (
			id, created, audioMs, transcribeMs int64
			text, status, path, errMsg, model  string
		)
		if err := rows.Scan(&id, &created, &audioMs, &transcribeMs, &text, &status, &path, &errMsg, &model); err != nil {
			return out
		}
		out = append(out, Entry{
			ID:         id,
			Created:    time.UnixMilli(created),
			AudioMs:    audioMs,
			Transcribe: transcribeMs,
			Text:       text,
			Status:     status,
			Path:       path,
			ErrMsg:     errMsg,
			Model:      model,
		})
	}
	return out
}

// Get returns a single history entry by id.
func (s *Store) Get(id int64) (Entry, bool) {
	if s == nil || id == 0 {
		return Entry{}, false
	}
	row := s.db.QueryRow(
		`SELECT id, created_at, audio_ms, transcribe_ms, text, status, path, error, model
		 FROM transcriptions WHERE id=?`, id)
	var (
		rid, created, audioMs, transcribeMs int64
		text, status, path, errMsg, model   string
	)
	if err := row.Scan(&rid, &created, &audioMs, &transcribeMs, &text, &status, &path, &errMsg, &model); err != nil {
		return Entry{}, false
	}
	return Entry{
		ID:         rid,
		Created:    time.UnixMilli(created),
		AudioMs:    audioMs,
		Transcribe: transcribeMs,
		Text:       text,
		Status:     status,
		Path:       path,
		ErrMsg:     errMsg,
		Model:      model,
	}, true
}

// Delete removes a single history entry. Reports whether a row was removed.
func (s *Store) Delete(id int64) bool {
	if s == nil || id == 0 {
		return false
	}
	res, err := s.db.Exec(`DELETE FROM transcriptions WHERE id=?`, id)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// DeleteFailed removes all failed, pending and empty entries and returns
// how many rows were removed.
func (s *Store) DeleteFailed() int64 {
	if s == nil {
		return 0
	}
	res, err := s.db.Exec(`DELETE FROM transcriptions WHERE status IN ('failed','pending','empty')`)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// Audio returns the stored audio blob for an entry (nil when absent).
func (s *Store) Audio(id int64) []byte {
	if s == nil || id == 0 {
		return nil
	}
	var data []byte
	s.db.QueryRow(`SELECT audio FROM transcriptions WHERE id=?`, id).Scan(&data)
	return data
}

// Point is one observation: audio length X and transcription time Y, in ms.
type Point struct{ X, Y float64 }

// Fit is the result of a linear model y = slope*x + intercept.
type Fit struct {
	Slope     float64
	Intercept float64
	N         int
	Outliers  int
	R2        float64
	OK        bool
}

// Describe renders the fit as a human-readable summary line.
func (f Fit) Describe() string {
	if !f.OK {
		return "no data yet"
	}
	s := fmt.Sprintf("t = %.3f·audio + %.0fms   ·   n=%d", f.Slope, f.Intercept, f.N)
	if f.Outliers > 0 {
		s += fmt.Sprintf("   ·   %d outlier(s) removed", f.Outliers)
	}
	if f.N >= 2 {
		s += fmt.Sprintf("   ·   r²=%.2f", f.R2)
	}
	return s
}

// fitPoints builds a least-squares line, iteratively rejecting outliers.
func fitPoints(points []Point) Fit {
	pts := make([]Point, 0, len(points))
	for _, p := range points {
		if p.X > 0 && p.Y > 0 && !math.IsNaN(p.X) && !math.IsNaN(p.Y) {
			pts = append(pts, p)
		}
	}
	if len(pts) == 0 {
		return Fit{}
	}
	if len(pts) == 1 {
		return Fit{Slope: pts[0].Y / pts[0].X, N: 1, OK: true}
	}

	work := append([]Point(nil), pts...)
	for iter := 0; iter < 5 && len(work) > 3; iter++ {
		f := ols(work)
		mean, std := residualStats(work, f)
		if std == 0 {
			break
		}
		kept := make([]Point, 0, len(work))
		removed := 0
		for _, p := range work {
			r := p.Y - (f.Slope*p.X + f.Intercept)
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
	if f.Slope <= 0 {
		var sx, sy float64
		for _, p := range work {
			sx += p.X
			sy += p.Y
		}
		if sx > 0 {
			f = Fit{Slope: sy / sx, N: len(work)}
		}
	}
	f.N = len(work)
	f.Outliers = len(pts) - len(work)
	f.R2 = rSquared(work, f)
	f.OK = true
	return f
}

// ols solves the normal equations for the best-fit line using linear algebra
// (Cramer's rule on the 2x2 system), without an external dependency.
func ols(pts []Point) Fit {
	n := float64(len(pts))
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		sx += p.X
		sy += p.Y
		sxx += p.X * p.X
		sxy += p.X * p.Y
	}
	// Normal equations: [n Sx; Sx Sxx] [intercept; slope] = [Sy; Sxy]
	a11, a12, a21, a22 := n, sx, sx, sxx
	b1, b2 := sy, sxy
	det := a11*a22 - a12*a21
	if math.Abs(det) < 1e-9 {
		return Fit{Intercept: sy / n, N: len(pts)}
	}
	intercept := (b1*a22 - a12*b2) / det
	slope := (a11*b2 - b1*a21) / det
	return Fit{Slope: slope, Intercept: intercept, N: len(pts)}
}

func residualStats(pts []Point, f Fit) (mean, std float64) {
	if len(pts) == 0 {
		return 0, 0
	}
	var sum float64
	rs := make([]float64, len(pts))
	for i, p := range pts {
		rs[i] = p.Y - (f.Slope*p.X + f.Intercept)
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

func rSquared(pts []Point, f Fit) float64 {
	if len(pts) < 2 {
		return 0
	}
	var meanY float64
	for _, p := range pts {
		meanY += p.Y
	}
	meanY /= float64(len(pts))
	var ssRes, ssTot float64
	for _, p := range pts {
		pred := f.Slope*p.X + f.Intercept
		ssRes += (p.Y - pred) * (p.Y - pred)
		ssTot += (p.Y - meanY) * (p.Y - meanY)
	}
	if ssTot == 0 {
		return 0
	}
	return 1 - ssRes/ssTot
}

// Points returns the timing observations for a model.
func (s *Store) Points(model string) []Point {
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

	var out []Point
	for rows.Next() {
		var x, y int64
		if err := rows.Scan(&x, &y); err != nil {
			return out
		}
		out = append(out, Point{X: float64(x), Y: float64(y)})
	}
	return out
}

// Predict estimates transcription time for audioMs and returns the fitted model.
func (s *Store) Predict(audioMs int64, model string) (int64, Fit) {
	f := fitPoints(s.Points(model))
	if !f.OK {
		return 0, f
	}
	est := f.Slope*float64(audioMs) + f.Intercept
	if est < 0 {
		est = 0
	}
	return int64(est), f
}

// PrintChart renders a scatter plot of past timings and the fitted line.
func (s *Store) PrintChart(w io.Writer, model string) {
	pts := s.Points(model)
	if len(pts) == 0 {
		fmt.Fprintln(w, "no successful transcriptions yet")
		return
	}
	f := fitPoints(pts)

	const width, height = 64, 18
	var maxX, maxY float64
	for _, p := range pts {
		if p.X > maxX {
			maxX = p.X
		}
		if p.Y > maxY {
			maxY = p.Y
		}
	}
	if f.OK {
		for _, x := range []float64{0, maxX} {
			y := f.Slope*x + f.Intercept
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

	if f.OK {
		for c := 0; c < width; c++ {
			x := float64(c) / float64(width-1) * maxX
			r := row(f.Slope*x + f.Intercept)
			if r >= 0 && r < height {
				grid[r][c] = '-'
			}
		}
	}
	for _, p := range pts {
		c, r := col(p.X), row(p.Y)
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
	fmt.Fprintf(w, "\nmodel: %s\n", f.Describe())
	fmt.Fprintf(w, "legend: * transcription, - least-squares fit (outliers rejected)\n")
}

// WavDurationMs reads a PCM WAV file's header and returns its duration in
// milliseconds. It returns an error if the format cannot be determined.
func WavDurationMs(path string) (int64, error) {
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
