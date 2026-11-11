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
use Claude Platform, OpenRouter or Cerebras, and `/api/ask` may use Claude
Platform, Cerebras or OpenRouter (DeepSeek V4.1 Flash 1017), each at once with
its own tray picker. The Cerebras key is shared: `ASK_CEREBRAS_API_KEY` enables
Cerebras on both explain and ask. A Claude or OpenRouter Ask key creates a web
(search + fetch) and a no-search choice; the no-search choice sends no tools and
uses the plain Ask prompt. All
three Ask providers accept images, which `/api/ask` sends after the prompt in
capture order; the image bytes live in `/api/screenshot`'s in-memory store, and
the client sends ids. The web choices carry OpenRouter's
`openrouter:web_search` and `openrouter:web_fetch` server tools or Claude's
basic web search and fetch tools (direct calls, at most two of each per
request). When `BRAVE_SEARCH_API_KEY` is set, **every** Ask choice also carries
the native client tools `search_images` and `show_images`: the main model
decides whether to call them, Brave returns ten filtered candidates (Claude
sees the top five as image blocks; OpenRouter/Cerebras see ten metadata rows),
and `show_images` sends the model's chosen IDs to the card as an `images` SSE
event. Four `search_images` calls per Ask round. `/api/memoir/check` still has two and
refuses both. Every Memoir variable
starts with `MEMOIR_` (plain `CLAUDE_API_KEY` is `/api/explain`'s); the ask
keys are `ASK_CLAUDE_API_KEY`, `ASK_CEREBRAS_API_KEY` and
`ASK_OPENROUTER_API_KEY`; `BRAVE_SEARCH_API_KEY` is optional and adds the Ask
image tools; and `ASK_CEREBRAS_API_KEY` also enables Cerebras for
`/api/explain`; the transcription
keys are `ASSEMBLYAI_API_KEY`, `DEEPGRAM_API_KEY` and
`OPENROUTER_TRANSCRIBE_API_KEY` (verbatim; plain `OPENROUTER_API_KEY` is
`/api/explain`'s). Both streaming providers need `ffmpeg` on `PATH`:
`/api/transcribe/stream` decodes MediaRecorder WebM/Opus to 16 kHz PCM, then
sends it to AssemblyAI's `/v1/transcribe/live` (asking for word timestamps and
returning `first_word_ms` / `last_word_ms`, which comart trims the microphone
clip's lead-in and lead-out to)
or Deepgram's
`wss://api.deepgram.com/v1/listen` (`model=nova-3`, `language=en`, which is
Deepgram's cheaper Nova-3 Monolingual tier). The browser clients use
`/api/transcribe/ws` for the same stream: Chrome blocks a streaming `fetch` body
to loopback from an extension page, so the WS handler forwards MediaRecorder
frames to the stream route over loopback. The tray (`internal/tray`) has
**Transcribe with**, **Explain with** and **Ask with** sections, each shown
only when that route has more than one configured provider. Linux's tray is a small
in-repo D-Bus backend that intentionally omits `Activate`, because GNOME then
opens the menu on the first click instead of waiting out the double-click
interval; macOS links Cocoa, so its build needs cgo.

To add a transcribe provider later, extend `resolveTranscribe` with its key,
add its `providerLabel` in `internal/tray`, and add a live case to
`handleTranscribeStream` in `internal/server/transcribe_stream.go`. There is no
batch fallback: a provider must implement streaming. To add an explain provider,
extend `resolveExplain` and `newExplainLLMs` and add its `providerLabel`; the
ask route follows the same pattern in `resolveAsk` and `newAskLLMs`.

Every `/api/explain` request logs provider, model, status, outcome, item count,
pronunciation lookups and duration; `/api/ask` logs provider, model, status,
outcome, item, context and earlier-round counts. Tray switches log
`transcribe provider: … (tray)` / `explain provider: … (tray)` /
`ask provider: … (tray)`.

## This is a BACKEND. The front ends live elsewhere.

- **There is no web UI to fix here.** A complaint about how something looks,
  where a button sits or what a panel says belongs to a client: the comart
  extension (read `~/comart/CLAUDE.md` and `~/comart/NOTES.md` first), Cairn's
  Memoir tab (`~/cairn`, client code in `src/memoir.ts`) or daily-dictation
  (`~/daily-dictation`). The one native UI is the provider pickers, kept for
  future transcribe, explain and ask providers and shown only when a route has
  more than one configured (`internal/tray`).
- **`GET /` is one line of `text/plain`**, on purpose. `install.sh`'s health
  check and comart's README use `http://127.0.0.1:8770/`, and a 404 at the
  root of a host that is plainly answering reads as a broken deploy. Keep it one
  line. Do not grow it into a status page.
- **Do not add a served template, a static directory or an `embed` for one.** A
  served copy of a client would have to be kept in step with the real one by
  hand. The tray's two icons are embedded in the binary and never served over
  HTTP, which is not an exception to this rule.
- **Keep the comart client API stable.** comart uses its routes with no CORS
  and no auth. Its `/api/ask` answers a spoken question over passages the
  reader picked, its optional `history` (earlier question/answer pairs in the
  same Ask card) keeps a follow-up's conversation, its optional `screenshots`
  names images from `/api/screenshot` in capture order, and its optional
  `timeline` interleaves selected text and those screenshots in the order the
  reader added them. `BuildAsk` renders that order under `# CONTEXT TIMELINE`.
  With `BRAVE_SEARCH_API_KEY` set, the same stream carries `images` events from
  the native image tools (`{"round":n,"status":"searching"|"empty"|"error"|"ready",…}`);
  `search_images` reports the first three without displaying, `show_images`
  reports `ready`. The stream carries URLs, not image bytes; comart preloads
  the Brave thumbnail and the original image.
- **`/api/screenshot` is Linux/GNOME and macOS.** On Linux it opens
  `org.freedesktop.portal.Screenshot` with `interactive: true`, reads the PNG
  the portal saves under `~/Pictures/Screenshots`, keeps it in memory under an
  unguessable id, and deletes the file immediately. On macOS it runs
  `/usr/sbin/screencapture -i -s -x -t png` into a private 0700 temp directory
  (`-s` blocks Space's window mode, so it is one drag, captured on mouse
  release). That directory is removed on every path, the process is
  SIGTERMed then killed when the request ends, only one capture runs at a time
  (429 otherwise; comart reads 409 as a cancel), and stale directories from a killed service are swept. The
  service binary needs Screen Recording permission in System Settings. `POST /api/screenshot/release`, not the TTL,
  is the normal cleanup: a non-Ask Stop releases that take's ids, and the Ask
  card's dismissal releases every round. The TTL is the backstop. One Ask
  card is capped at 10 screenshots and 8 MiB of base64 on the server, matching
  the client's camera cap; images are scaled to a 1200px long edge. The systemd
  unit must keep the Screenshots directory in `ReadWritePaths=` or the PNG is
  left on disk. Memoir's
  `/api/memoir/check`, daily-dictation's
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
- Models and thinking depth are fixed in `internal/config`, not environment
  settings: Claude effort `medium`, OpenRouter explain 1024 thinking tokens,
  OpenRouter ask `reasoning.effort: high` (DeepSeek V4.1 Flash 1017), Claude
  ask with or without the basic direct web search and fetch tools (max 2 of
  each when on), and Cerebras `reasoning_effort: medium`. Keep Cerebras
  reasoning on: without
  it Qwen breaks Memoir's JSON contract and edits text it must leave alone.
- `internal/prompt/part1.txt`, `part2.txt` and `internal/memoir/check.txt` are
  product prompts. `TestPromptParity` pins `check.txt` by hash. Do not edit it
  in passing.

## Version control and secrets

- **Git.** GitHub repo `okoyfoeciov/ai-service`. Git is the undo for code.
- **Secrets never go into git, and never into the chat.** Do not read or print
  `ai-service.env` (it holds the API keys), `*.har` captures, `data/` or
  `backups/`. `.gitignore` keeps them out; never force-add an ignored file.
  Old `backups/` zips may hold secrets.
