# stt

A terminal speech-to-text CLI. Press a key, speak, stop, and the transcript is
printed and copied to your clipboard. It records locally, sends the audio to a
[LiteLLM](https://github.com/BerriAI/litellm) proxy for transcription, keeps a
history in SQLite, and learns how long transcription takes so it can show a live
progress bar.

```
stt  speech to text  ·  parakeet via litellm  ·  0.1.0

  [r] record    [h] history    [l] load failed    [q] quit
```

## Features

- Long-running TUI (Bubbletea) — record repeatedly without restarting
- Startup health check against LiteLLM `/health`, including whether your model is healthy
- `r` record, `space` stop, `p` pause/resume, `h` history, `l` failed recordings, `q` quit
- Live recording timer with pause accounting
- Transcription progress bar driven by a learned timing model
- Transcript copied to the clipboard automatically, with optional in-TUI editing (`e`)
- Markdown-rendered, word-wrapped transcript output (Glamour)
- SQLite history with timestamps, audio length and transcription time
- Failed recordings are kept (on disk and in the DB) and can be retried, even across restarts
- `--chart` prints the timing model as an ASCII scatter plot

## Requirements

- A recorder:
  - **Linux**: `arecord` (ALSA)
  - **macOS**: [SoX](https://sox.sourceforge.net/) (`rec`) or `ffmpeg`
  - **Windows**: `ffmpeg` (and `STT_DEVICE` set, see below)
- A clipboard tool (optional, for auto-copy):
  - **Linux**: `wl-copy` (Wayland), `xclip`, or `xsel`
  - **macOS**: `pbcopy` (built in)
- A running LiteLLM proxy with a transcription model (e.g. `parakeet-tdt`)

### macOS

Release binaries are unsigned, so macOS Gatekeeper may refuse to run them. Ad-hoc
sign the binary after downloading:

```sh
codesign --sign - --force /path/to/stt
```

## Configuration

Configuration is read from a `.env` file or the process environment. There are
no baked-in defaults for the proxy URL, key, or model — you must supply them.

Copy the example and fill it in:

```sh
cp .env.example .env
$EDITOR .env
```

`.env` is gitignored. It is loaded (without overriding already-set variables)
from, in order:

1. `$STT_ENV_FILE` if set
2. `./.env` in the working directory
3. `$XDG_CONFIG_HOME/stt/.env` (usually `~/.config/stt/.env`)

| Variable | Required | Description |
| --- | --- | --- |
| `STT_LITELLM_URL` | yes | Base URL of the LiteLLM proxy |
| `STT_LITELLM_KEY` | yes | API key sent as `Authorization: Bearer ...` |
| `STT_MODEL` | yes | Transcription model name on the proxy |
| `STT_DEVICE` | per-OS | Recording device |
| `STT_DB` | no | Path to the SQLite database |
| `STT_ENV_FILE` | no | Explicit path to a `.env` file to load first |
| `XDG_DATA_HOME` | no | Standard base dir used for the default DB location (`~/.local/share`) |
| `XDG_CONFIG_HOME` | no | Standard base dir for the secondary `.env` location (`~/.config`)

### `STT_DEVICE`

- **Linux**: ALSA device for `arecord` (default `pipewire`; e.g. `plughw:CARD=Microphone,DEV=0`)
- **macOS ffmpeg**: an avfoundation input spec (default `:0`)
- **Windows**: required — the DirectShow microphone name, e.g. `Microphone (USB Audio)`
  (list devices with `ffmpeg -list_devices true -f dshow -i dummy`)

Example:

```sh
export STT_LITELLM_URL=http://localhost:4000
export STT_LITELLM_KEY=sk-your-key
export STT_MODEL=whisper-1
stt
```

## Build and install

Requires Go (see `go.mod` for the version).

```sh
# build a local binary into dist/
task build

# install into $GOBIN
go install github.com/andrejsstepanovs/stt-litellm-cli@latest
```

Or plain Go:

```sh
go build -o stt .
go run main.go            # single-file layout, works without a build step
```

The version is injected at build time via ldflags; without it, `stt` reports `dev`.

## Usage

```sh
stt                       # start the TUI
stt --version             # print version/commit/date
stt --chart               # print the timing model chart and exit
stt -m whisper-1          # override the model for this run
```

### Keys

| Key | Action |
| --- | --- |
| `r` | Start recording |
| `space` | Stop recording and transcribe |
| `p` | Pause / resume recording |
| `h` | Show transcription history |
| `l` | Show failed recordings (load and retry) |
| `e` | Edit the transcript in a text area (on a successful result) |
| `↑` / `↓` (or `j`/`k`) | Move selection in history/failed lists |
| `enter` | Copy selected transcript, or retry selected failure |
| `n` | (On a failed result) discard and start a new recording |
| `q` / `Esc` | Quit |

While editing: `enter` saves the edited text to the database and clipboard,
`alt+enter` (or `ctrl+j`) inserts a newline, and `Esc` cancels.

## How it works

1. **Record** — a platform recorder writes a PCM WAV to a temp file.
2. **Transcribe** — the WAV is POSTed as multipart form data to
   `$STT_LITELLM_URL/v1/audio/transcriptions` with the configured model.
3. **Deliver** — the transcript is printed and copied to the clipboard.
4. **Persist** — a row is written to SQLite with the audio length, transcription
   time, model, status, text, and the audio bytes.
5. **Predict** — past successful runs feed a least-squares timing model that
   drives the progress bar while the next transcription runs.

### Recording and failure handling

Pressing `r` first shows **Initializing microphone…** until the recorder is
actually writing audio, so the timer and "Recording" state only appear once the
mic is live. While recording, a live **equalizer** shows the incoming volume
(`mic ▄▅▆▇…`) so you can see at a glance that your voice is being captured.

For the first 10 seconds the app also checks whether anything but silence is
captured: if not, it shows a *no audio detected* warning after ~2.5s and, if
still silent at 10s, stops and asks you to check the microphone and retry. If
the recorder process dies mid-recording (for example another app grabs the
device), recording stops with a clear message instead of silently capturing
nothing.

Every recording is inserted with `status='pending'`. On success it becomes
`status='success'`, the audio blob is cleared, and the WAV is deleted. On
failure it becomes `status='failed'`: the WAV is kept on disk, the audio blob
stays in the DB, and the error is shown with the saved file path.

If the request succeeds but returns **no text**, it is treated as a
*semi-failure* (`status='empty'`): the audio is kept, nothing is copied, and the
recording can be retried. Failed and empty recordings can be retried with `r`
immediately, or later via `l` (recovering from disk, or from the DB blob if the
file is gone).

## Data and prediction

History lives in a SQLite database (default
`~/.local/share/stt/stt.db`). Schema is managed with
[goose](https://github.com/pressly/goose) migrations embedded in the binary
(`migrations/`).

The transcription-time model is a least-squares linear fit
`t = slope * audio + intercept`, solved from the normal equations, using only
`status='success'` rows for the current model. Points whose residuals exceed
`2.5 sigma` are iteratively rejected as outliers, and `r²` is reported.

`stt --chart` renders the data and the fitted line:

```
  1.16s |                                                  ----*-----
  0.78s |                               --------*-
  0.00s | -
        +----------------------------------------------------------------
         0s                                                      8.4s

model: t = 0.150·audio + 90ms   ·   n=8   ·   1 outlier(s) removed   ·   r²=1.00
```

## Development

Tasks are defined in `Taskfile.yml` and run with [Task](https://taskfile.dev/).

```sh
task              # list tasks
task build        # build for the current platform
task build:all    # cross-compile release binaries into dist/
task test
task lint         # go vet + gofmt check
task fmt
task tidy
task tools        # install goreleaser
```

## Releasing

The version comes from the `VERSION` file at the repository root — there is no
git-describe or dirty-state logic. CI or a maintainer should keep the git tag
and `VERSION` in sync.

```sh
task snapshot     # build a local release snapshot (no publish)
task release      # publish (requires a git tag and GITHUB_TOKEN)
```

[GoReleaser](.goreleaser.yaml) produces raw, versioned binaries for Linux,
macOS, Windows, and FreeBSD, e.g.:

```
stt_v0.1.0_linux_amd64
stt_v0.1.0_linux_arm64
stt_v0.1.0_darwin_arm64
stt_v0.1.0_windows_amd64.exe
...
```

plus `checksums.txt`.
