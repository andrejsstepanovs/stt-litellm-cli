package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/andrejsstepanovs/stt-litellm-cli/internal/core"
)

// session holds the state of one recording/transcription cycle. It mirrors
// the TUI state machine in the main command, but drives it through HTTP so
// any frontend (tray popup, browser tab) can render it.
type session struct {
	mu     sync.Mutex
	store  *core.Store
	state  string // idle, starting, recording, transcribing, result
	err    error
	text   string
	empty  bool
	copied bool

	wav      string
	model    string
	device   string
	audioMs  int64
	actualMs int64
	recID    int64

	start       time.Time
	paused      bool
	pausedAt    time.Time
	pausedTotal time.Duration
	audioDetect bool

	healthOK      bool
	healthDetail  string
	healthChecked bool

	onChange func()
}

func newSession(store *core.Store) *session {
	return &session{store: store, state: "idle"}
}

func (s *session) notify() {
	if s.onChange != nil {
		go s.onChange()
	}
}

func (s *session) setHealth(ok bool, detail string) {
	s.mu.Lock()
	s.healthChecked = true
	s.healthOK = ok
	s.healthDetail = detail
	s.mu.Unlock()
}

func (s *session) checkHealth() {
	go func() {
		ok, detail := core.HealthCheck()
		s.setHealth(ok, detail)
		s.notify()
	}()
}

func (s *session) recElapsed() time.Duration {
	d := time.Since(s.start) - s.pausedTotal
	if s.paused {
		d -= time.Since(s.pausedAt)
	}
	if d < 0 {
		d = 0
	}
	return d
}

// Record starts a new recording. device and model may be empty, in which
// case the STT_DEVICE / STT_MODEL environment defaults apply.
func (s *session) Record(device, model string) error {
	s.mu.Lock()
	switch s.state {
	case "starting", "recording", "transcribing":
		s.mu.Unlock()
		return fmt.Errorf("already busy (%s)", s.state)
	}
	if model == "" {
		model = os.Getenv("STT_MODEL")
	}
	if device == "" {
		device = os.Getenv("STT_DEVICE")
	}
	s.mu.Unlock()

	wav, err := core.StartRecording(device)
	if err != nil {
		s.mu.Lock()
		s.state = "result"
		s.err = err
		s.mu.Unlock()
		s.notify()
		return err
	}

	s.mu.Lock()
	s.state = "starting"
	s.wav = wav
	s.device = device
	s.model = model
	s.recID = 0
	s.text = ""
	s.err = nil
	s.empty = false
	s.copied = false
	s.audioDetect = false
	s.audioMs = 0
	s.actualMs = 0
	s.start = time.Now()
	s.paused = false
	s.pausedTotal = 0
	s.pausedAt = time.Time{}
	s.mu.Unlock()
	s.notify()

	r := core.CurrentRecorder()
	go func() {
		// wait for the mic to actually produce audio (same as the TUI)
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-r.Done():
				s.mu.Lock()
				s.state = "result"
				s.err = fmt.Errorf("microphone error: %s", r.StderrText())
				s.wav = ""
				os.Remove(wav)
				s.mu.Unlock()
				s.notify()
				return
			case <-ticker.C:
				if fi, err := os.Stat(r.Path()); err == nil && fi.Size() > core.RecReadyBytes {
					s.mu.Lock()
					s.state = "recording"
					s.start = time.Now()
					s.mu.Unlock()
					s.notify()
					return
				}
			case <-deadline:
				core.StopActive()
				s.mu.Lock()
				s.state = "result"
				s.err = fmt.Errorf("microphone did not start (no audio captured within 10s)")
				s.wav = ""
				os.Remove(wav)
				s.mu.Unlock()
				s.notify()
				return
			}
		}
	}()
	return nil
}

// Stop finishes the recording and starts transcription in the background.
func (s *session) Stop() {
	s.mu.Lock()
	if s.state != "recording" && s.state != "starting" {
		s.mu.Unlock()
		return
	}
	wav := s.wav
	elapsed := s.recElapsed().Milliseconds()
	// claim the cycle so the mic-fail guard cannot interfere
	s.state = "stopping"
	s.mu.Unlock()

	core.StopActive()

	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := os.Stat(wav)
	if err != nil || fi.Size() < 1000 {
		os.Remove(wav)
		s.state = "result"
		s.wav = ""
		s.err = fmt.Errorf("no audio captured (is the mic busy?)")
		s.notify()
		return
	}

	audioMs, _ := core.WavDurationMs(wav)
	if audioMs <= 0 {
		audioMs = elapsed
	}

	audio, _ := os.ReadFile(wav)
	s.audioMs = audioMs
	s.recID = s.store.CreateRecording(audioMs, s.model, wav, audio)
	s.state = "transcribing"
	s.start = time.Now()
	s.notify()

	go s.transcribe(wav, s.model, audioMs, s.recID)
}

// transcribe runs the proxy call and finalizes the cycle. It is the shared
// tail of both a fresh recording and a retry.
func (s *session) transcribe(wav, model string, audioMs, recID int64) {
	start := time.Now()
	text, err := core.Transcribe(wav, model)
	txMs := time.Since(start).Milliseconds()

	empty := err == nil && text == ""
	switch {
	case err != nil:
		s.store.MarkFailed(recID, txMs, err.Error())
	case empty:
		s.store.MarkEmpty(recID, txMs)
	default:
		s.store.MarkSuccess(recID, txMs, text)
	}

	copied := false
	if err == nil && !empty {
		copied = core.CopyToClipboard(text)
		os.Remove(wav) // success: the transcript lives in the DB now
	}

	s.mu.Lock()
	s.state = "result"
	s.text = text
	s.err = err
	s.empty = empty
	s.copied = copied
	s.actualMs = txMs
	s.mu.Unlock()
	s.notify()
}

// Retry re-transcribes a failed/empty history entry (same audio-extraction
// fallback as the TUI: original file first, stored blob second).
func (s *session) Retry(id int64) error {
	s.mu.Lock()
	switch s.state {
	case "starting", "recording", "transcribing":
		s.mu.Unlock()
		return fmt.Errorf("busy (%s)", s.state)
	}
	s.mu.Unlock()

	e, ok := s.store.Get(id)
	if !ok {
		return fmt.Errorf("no such entry: %d", id)
	}
	path := e.Path
	if fi, err := os.Stat(path); err != nil || fi.Size() < 1000 {
		if data := s.store.Audio(e.ID); len(data) > 0 {
			if tmp, err := os.CreateTemp("", "stt-*.wav"); err == nil {
				tmp.Write(data)
				tmp.Close()
				path = tmp.Name()
			}
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() < 1000 {
		return fmt.Errorf("audio unavailable for this recording")
	}

	model := e.Model
	if model == "" {
		model = os.Getenv("STT_MODEL")
	}

	s.mu.Lock()
	s.state = "transcribing"
	s.wav = path
	s.model = model
	s.recID = e.ID
	s.audioMs = e.AudioMs
	s.text = ""
	s.err = nil
	s.empty = false
	s.copied = false
	s.start = time.Now()
	s.mu.Unlock()
	s.notify()

	wav := path
	go s.transcribe(wav, model, e.AudioMs, e.ID)
	return nil
}

// Cancel stops and discards the current cycle.
func (s *session) Cancel() {
	s.mu.Lock()
	busy := s.state == "starting" || s.state == "recording"
	wav := s.wav
	s.state = "idle"
	s.wav = ""
	s.err = nil
	s.text = ""
	s.mu.Unlock()
	if busy {
		core.StopActive()
	}
	if wav != "" {
		os.Remove(wav)
	}
	s.notify()
}

// Pause/resume are no-ops on Windows (console signals are unavailable);
// the UI hides them there.
func (s *session) Pause() {
	if r := core.CurrentRecorder(); r != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.paused {
			r.Resume()
			s.pausedTotal += time.Since(s.pausedAt)
			s.paused = false
		} else {
			r.Pause()
			s.pausedAt = time.Now()
			s.paused = true
		}
		s.notify()
	}
}

// LastDevice/LastModel return the device/model of the most recent cycle so
// the tray can repeat it.
func (s *session) LastDevice() string { s.mu.Lock(); defer s.mu.Unlock(); return s.device }
func (s *session) LastModel() string  { s.mu.Lock(); defer s.mu.Unlock(); return s.model }

// Busy reports whether a cycle is running (without touching the recorder).
func (s *session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case "starting", "recording", "transcribing":
		return true
	}
	return false
}

// Health returns the last known health state without blocking on the proxy.
func (s *session) Health() (ok bool, detail string, checked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthOK, s.healthDetail, s.healthChecked
}

// ownsWav reports whether the session is currently using this wav file
// (an in-flight recording or retry), so callers must not remove it.
func (s *session) ownsWav(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return path != "" && path == s.wav
}

// traySnapshot is a cheap summary for the tray icon/tooltip. Unlike Status it
// does not consume the recorder's peak level (that belongs to the UI poll).
func (s *session) traySnapshot() (state string, elapsed time.Duration, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state = s.state
	if s.state == "starting" || s.state == "recording" || s.state == "transcribing" {
		elapsed = time.Since(s.start)
	}
	if s.state == "result" && s.err != nil {
		errText = s.err.Error()
	}
	return state, elapsed, errText
}

// Status is the polled snapshot the UI renders.
func (s *session) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := map[string]any{
		"state":         s.state,
		"model":         s.model,
		"device":        s.device,
		"healthOK":      s.healthOK,
		"healthDetail":  s.healthDetail,
		"healthChecked": s.healthChecked,
		"copied":        s.copied,
		"paused":        s.paused,
		"audioMs":       s.audioMs,
		"actualMs":      s.actualMs,
		"recID":         s.recID,
		"goos":          runtime.GOOS,
	}
	if s.state == "starting" || s.state == "recording" {
		st["elapsedMs"] = s.recElapsed().Milliseconds()
		detect := false
		if r := core.CurrentRecorder(); r != nil {
			st["peak"] = r.SamplePeak()
			detect = r.AudioDetected()
		} else {
			st["peak"] = 0
		}
		st["audioDetect"] = detect
		if !detect {
			if s.recElapsed() > core.MicWarnAfter {
				st["micWarn"] = true
			}
			if s.recElapsed() >= core.MicFailAfter {
				st["micFail"] = true
			}
		}
	}
	if s.state == "transcribing" {
		st["elapsedMs"] = time.Since(s.start).Milliseconds()
		est, f := s.store.Predict(s.audioMs, s.model)
		st["hasEst"] = f.OK && est > 0
		st["estMs"] = est
		st["estDescribe"] = f.Describe()
	}
	if s.state == "result" {
		if s.err != nil {
			st["err"] = s.err.Error()
		}
		st["text"] = s.text
		st["empty"] = s.empty
		st["retryable"] = (s.err != nil || s.empty) && s.wav != ""
	}
	return st
}

// micFailGuard enforces the "no audio in 10s" rule; the app calls it from a
// ticker so the rule holds even when no window is polling.
func (s *session) micFailGuard() {
	s.mu.Lock()
	if s.state != "recording" {
		s.mu.Unlock()
		return
	}
	detect := false
	if r := core.CurrentRecorder(); r != nil {
		detect = r.AudioDetected()
	}
	if detect || s.recElapsed() < core.MicFailAfter {
		s.mu.Unlock()
		return
	}
	wav := s.wav
	s.state = "stopping"
	s.mu.Unlock()

	core.StopActive()
	if wav != "" {
		os.Remove(wav)
	}
	s.mu.Lock()
	s.state = "result"
	s.wav = ""
	s.err = fmt.Errorf("no audio captured in the first 10s — check your microphone and try again")
	s.mu.Unlock()
	s.notify()
}
