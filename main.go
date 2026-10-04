// Command stt is a terminal speech-to-text client. The engine lives in
// internal/core and is shared with the tray app (cmd/stt-tray).
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andrejsstepanovs/stt-litellm-cli/internal/core"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	defaultModel string
	history      *core.Store
)

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

type healthMsg struct {
	ok     bool
	detail string
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
	estFit      core.Fit
	estOK       bool
	actualMs    int64
	empty       bool
	audioDetect bool
	meter       []int

	healthChecked bool
	healthOK      bool
	healthDetail  string

	width      int
	height     int
	hist       []core.Entry
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

func checkHealth() tea.Cmd {
	return func() tea.Msg {
		ok, detail := core.HealthCheck()
		return healthMsg{ok: ok, detail: detail}
	}
}

func tick() tea.Cmd {
	return tea.Tick(20*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func transcribeCmd(path, modelName string, audioMs, recID int64) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		text, err := core.Transcribe(path, modelName)
		txMs := time.Since(start).Milliseconds()
		switch {
		case err != nil:
			history.MarkFailed(recID, txMs, err.Error())
		case strings.TrimSpace(text) == "":
			history.MarkEmpty(recID, txMs)
		default:
			history.MarkSuccess(recID, txMs, text)
		}
		return txDoneMsg{text: text, err: err, transcribe: txMs}
	}
}

func beginRecording(m model) (tea.Model, tea.Cmd) {
	wav, err := core.StartRecording(os.Getenv("STT_DEVICE"))
	if err != nil {
		m.err = err
		m.state = stResult
		return m, nil
	}
	r := core.CurrentRecorder()
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
	m.hist = history.Recent(200)
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
	m.hist = history.Failed(200)
	m.histCursor = 0
	m.histOffset = 0
	m.histMsg = ""
	m.listFailed = true
	m.state = stHistory
	return m, nil
}

func loadFailed(m model, e core.Entry) (tea.Model, tea.Cmd) {
	path := e.Path
	if fi, err := os.Stat(path); err != nil || fi.Size() < 1000 {
		if data := history.Audio(e.ID); len(data) > 0 {
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
	if e.Model != "" {
		m.model = e.Model
	}
	m.wav = path
	m.audioMs = e.AudioMs
	m.recID = e.ID
	return retryTranscribe(m)
}

func retryTranscribe(m model) (tea.Model, tea.Cmd) {
	m.text = ""
	m.err = nil
	m.empty = false
	m.estMs, m.estFit = history.Predict(m.audioMs, m.model)
	m.estOK = m.estFit.OK
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

// sampleLevel polls the recorder's current input level shortly after.
func sampleLevel(r *core.Recorder) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(100 * time.Millisecond)
		return levelMsg{peak: r.SamplePeak()}
	}
}

// waitRecorderReady polls the output file until the recorder is actually
// writing audio, so we only claim to be recording once the mic is live.
func waitRecorderReady(r *core.Recorder) tea.Cmd {
	return func() tea.Msg {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-r.Done():
				return recErrMsg{err: fmt.Errorf("microphone error: %s", r.StderrText())}
			case <-ticker.C:
				if fi, err := os.Stat(r.Path()); err == nil && fi.Size() > core.RecReadyBytes {
					return recReadyMsg{started: time.Now()}
				}
			case <-deadline:
				return recErrMsg{err: fmt.Errorf("microphone did not start (no audio captured within 10s)")}
			}
		}
	}
}

type recReadyMsg struct{ started time.Time }
type recErrMsg struct{ err error }
type levelMsg struct{ peak int }

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
			r := core.CurrentRecorder()
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
		r := core.CurrentRecorder()
		if r == nil || r.IsDone() {
			m.err = fmt.Errorf("recorder stopped unexpectedly — is another app using the microphone?")
			m.state = stResult
			return m, nil
		}
		m.audioDetect = m.audioDetect || msg.peak > core.AudioPeakThreshold
		m.pushLevel(msg.peak)
		if !m.audioDetect && m.recElapsed() >= core.MicFailAfter {
			core.StopActive()
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
			core.StopActive()
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
			core.CopyToClipboard(msg.text)
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
				history.UpdateText(m.recID, m.text)
				core.CopyToClipboard(m.text)
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
					if core.CopyToClipboard(m.hist[m.histCursor].Text) {
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
				core.StopActive()
				if m.wav != "" {
					os.Remove(m.wav)
					m.wav = ""
				}
				m.state = stIdle
			case "q", "ctrl+c", "esc":
				core.StopActive()
				if m.wav != "" {
					os.Remove(m.wav)
				}
				return m, tea.Quit
			}
		case stRecording:
			switch msg.String() {
			case " ":
				core.StopActive()
				if fi, err := os.Stat(m.wav); err != nil || fi.Size() < 1000 {
					os.Remove(m.wav)
					m.wav = ""
					m.empty = false
					m.err = fmt.Errorf("no audio captured (is the mic busy?)")
					m.state = stResult
					return m, nil
				}
				m.audioMs, _ = core.WavDurationMs(m.wav)
				if m.audioMs <= 0 {
					m.audioMs = m.recElapsed().Milliseconds()
				}
				m.estMs, m.estFit = history.Predict(m.audioMs, m.model)
				m.estOK = m.estFit.OK
				audio, _ := os.ReadFile(m.wav)
				m.recID = history.CreateRecording(m.audioMs, m.model, m.wav, audio)
				m.text = ""
				m.err = nil
				m.empty = false
				m.state = stTranscribing
				m.start = time.Now()
				m.spinner.Style = txSpinnerStyle
				return m, tea.Batch(m.spinner.Tick, tick(), transcribeCmd(m.wav, m.model, m.audioMs, m.recID))
			case "p", "P":
				r := core.CurrentRecorder()
				if r == nil {
					return m, nil
				}
				if m.paused {
					r.Resume()
					m.pausedTotal += time.Since(m.pausedAt)
					m.paused = false
				} else {
					r.Pause()
					m.pausedAt = time.Now()
					m.paused = true
				}
				return m, nil
			case "q", "ctrl+c", "esc":
				core.StopActive()
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
				ts := e.Created.Format("2006-01-02 15:04:05")
				var label string
				if m.listFailed {
					label = e.ErrMsg
					if label == "" {
						label = e.Status
					}
				} else {
					label = e.Text
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
		if !m.audioDetect && m.recElapsed() > core.MicWarnAfter {
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
			b.WriteString("  " + dimStyle.Render(m.estFit.Describe()) + "\n")
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
	core.LoadDotEnv()
	litellmURL := os.Getenv("STT_LITELLM_URL")
	litellmKey := os.Getenv("STT_LITELLM_KEY")
	defaultModel = os.Getenv("STT_MODEL")
	core.SetConfig(litellmURL, litellmKey)

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

	h, err := core.OpenStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: history disabled: %v\n", err)
	} else {
		history = h
		defer history.Close()
	}

	if *chart {
		history.PrintChart(os.Stdout, *modelName)
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
		core.StopActive()
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
