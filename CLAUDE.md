# ai-service — project notes

One Go binary on `127.0.0.1:8770` (JSON and SSE). It serves three clients: the
comart Chrome extension (`~/comart`), Cairn's Memoir tab (`~/cairn`) and
daily-dictation (`~/daily-dictation`, which uses the streaming transcription
routes and `/api/transcribe-verbatim`). `README.md` is the full guide: install, service
management, configuration, security model and models.

In short, `/api/transcribe/stream` runs on AssemblyAI Universal-3.5 Pro or
Deepgram Nova-3 (both live APIs); at least one of `ASSEMBLYAI_API_KEY` and
`DEEPGRAM_API_KEY` is required, and the service refuses to start with neither.
`/api/transcribe-verbatim` still uses OpenRouter's MAI-Transcribe-2, with
`verbose_json`, word timestamps and Azure's verbatim style. `/api/explain` may
use Claude Platform or OpenRouter, both at once with a tray picker;
`/api/memoir/check` still has two and refuses both. Every Memoir variable
starts with `MEMOIR_` (plain `CLAUDE_API_KEY` is `/api/explain`'s); the
transcription keys are `ASSEMBLYAI_API_KEY`, `DEEPGRAM_API_KEY` and
`OPENROUTER_TRANSCRIBE_API_KEY` (verbatim; plain `OPENROUTER_API_KEY` is
`/api/explain`'s). Both streaming providers need `ffmpeg` on `PATH`:
`/api/transcribe/stream` decodes MediaRecorder WebM/Opus to 16 kHz PCM, then
sends it to AssemblyAI's `/v1/transcribe/live` or Deepgram's
`wss://api.deepgram.com/v1/listen` (`model=nova-3`, `language=en`, which is
Deepgram's cheaper Nova-3 Monolingual tier). The browser clients use
`/api/transcribe/ws` for the same stream: Chrome blocks a streaming `fetch` body
to loopback from an extension page, so the WS handler forwards MediaRecorder
frames to the stream route over loopback. The tray (`internal/tray`) has a
**Transcribe with** section and an **Explain with** section, each shown only
when that route has more than one configured provider. Linux's tray is a small
in-repo D-Bus backend that intentionally omits `Activate`, because GNOME then
opens the menu on the first click instead of waiting out the double-click
interval; macOS links Cocoa, so its build needs cgo.

To add a transcribe provider later, extend `resolveTranscribe` with its key,
add its `providerLabel` in `internal/tray`, and add a live case to
`handleTranscribeStream` in `internal/server/transcribe_stream.go`. There is no
batch fallback: a provider must implement streaming. To add an explain provider,
extend `resolveExplain` and `newExplainLLMs` and add its `providerLabel`.

Every `/api/explain` request logs provider, model, status, outcome, item count,
pronunciation lookups and duration; tray switches log
`transcribe provider: … (tray)` / `explain provider: … (tray)`.

## This is a BACKEND. The front ends live elsewhere.

- **There is no web UI to fix here.** A complaint about how something looks,
  where a button sits or what a panel says belongs to a client: the comart
  extension (read `~/comart/CLAUDE.md` and `~/comart/NOTES.md` first), Cairn's
  Memoir tab (`~/cairn`, client code in `src/memoir.ts`) or daily-dictation
  (`~/daily-dictation`). The one native UI is the provider pickers, kept for
  future transcribe and explain providers and shown only when a route has more
  than one configured (`internal/tray`).
- **`GET /` is one line of `text/plain`**, on purpose. `install.sh`'s health
  check and comart's README use `http://127.0.0.1:8770/`, and a 404 at the
  root of a host that is plainly answering reads as a broken deploy. Keep it one
  line. Do not grow it into a status page.
- **Do not add a served template, a static directory or an `embed` for one.** A
  served copy of a client would have to be kept in step with the real one by
  hand. The tray's two icons are embedded in the binary and never served over
  HTTP, which is not an exception to this rule.
- **Keep the comart client API stable.** comart uses its seven routes with no
  CORS and no auth. Memoir's `/api/memoir/check`, daily-dictation's
  `/api/transcribe-verbatim` and the clients' streaming transcription routes
  (`POST /api/transcribe/stream` and its `GET /api/transcribe/ws` transport;
  AssemblyAI live, buffered for others) are the deliberate additions. Change a
  route only together with the client that calls it.

## Hosting and security

- It runs on macOS (launchd LaunchAgent `com.ai-service.backend`) or Linux
  (systemd user unit `ai-service.service`). Git shares this file with every
  copy, so check `uname -s` before you run service commands. `./install.sh`
  writes either definition and restarts the service (`--dry-run` only writes
  to a temp dir). Re-run it after moving this folder, because the definition
  holds absolute paths.
- The owner's current host is Debian Linux (systemd user unit, lingering on).
- The old comart backend units (`comart.service` and `comart-serve.service` on
  Linux, `com.comart.backend` on macOS) use the same port 8770. Keep them
  disabled; on the Debian host they are masked.
- Nothing fronts the server: **loopback is the trust boundary**, and there is
  no auth by design. The Host/Origin guard (`loopbackOnly` in
  `internal/server/server.go`) and the Content-Type rule on the billed routes
  keep web pages out. Do not weaken them.
- On Cerebras, keep Memoir's reasoning on (`MEMOIR_REASONING_EFFORT` not
  `none`): without it Qwen breaks the JSON contract and edits text it must
  leave alone.
- `internal/prompt/part1.txt`, `part2.txt` and `internal/memoir/check.txt` are
  product prompts. `TestPromptParity` pins `check.txt` by hash. Do not edit it
  in passing.

## Version control and secrets

- **Git.** GitHub repo `okoyfoeciov/ai-service`. Git is the undo for code.
- **Secrets never go into git, and never into the chat.** Do not read or print
  `ai-service.env` (it holds the API keys), `*.har` captures, `data/` or
  `backups/`. `.gitignore` keeps them out; never force-add an ignored file.
  Old `backups/` zips may hold secrets.
