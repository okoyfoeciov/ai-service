# ai-service

A small localhost **LLM backend**: one static Go binary on `127.0.0.1:8770`,
speaking JSON and SSE. It has four clients:

- the **comart Chrome extension** (`~/comart`): word explanations,
  pronunciations, mic transcription and the mic card's writing check;
- **Cairn's Memoir tab** (`~/cairn`, `src/memoir.ts`): proofreading of
  journal entries;
- **daily-dictation** (`~/daily-dictation`): verbatim transcripts through
  `POST /api/transcribe-verbatim`;
- **voice-typing** (`~/voice-typing`, GNOME top-bar dictation): text-only
  transcripts through `POST /api/transcribe/stream?timestamps=0`.

**There is no UI here.** All four clients carry their own UI. `GET /`
answers one line of plain text and nothing else. Do not add a template, a
static directory or an `embed` back: a served copy of a client would have to be
kept in step with the real one by hand.

The comart API is deliberately stable: no CORS, no auth, and only the routes
the clients actually call. `/api/ask` is comart's spoken-question route, and
`/api/screenshot` is its Linux screen-capture route (the image is kept here
under an id and released by the client); Memoir's `/api/memoir/check` and
daily-dictation's `/api/transcribe-verbatim` are the other deliberate
additions. Change a route only together with the client that calls it.

## Routes

| Route | Client | What it does |
| --- | --- | --- |
| `GET /` | health check | One line of `text/plain`. Keep it one line. |
| `POST /api/explain` | comart | Explains the chosen words. SSE events: `pron-data`, `chunk`, `error`, `done` (`done` is always last). An error after the stream opens is an `error` event; the HTTP status stays 200. |
| `POST /api/ask` | comart | Answers a spoken question about passages the reader picked. Body: `{"question": …, "items": [{phrase, words}], "contexts": [{phrase, text}], "history": [{question, answer}], "screenshots": [id, …], "timeline": [{"kind":"text","phrase":…} | {"kind":"screenshot","screenshot":id}]}`. `history` is optional and carries the earlier rounds of the same Ask card; `screenshots` is optional and names images stored by `/api/screenshot`, in capture order; `timeline` is optional and lists the selected text and screenshots in the exact order the reader added them this round, so the prompt can interleave them. Same SSE events as `/api/explain` (`chunk`, `error`, `done`), no pronunciation prefetch, plus an initial `search` event for every Ask (`{"active":false,"enabled":true|false}`) and, for Claude's web choice, `{"active":true|false,"enabled":true}` when a native search or fetch starts and its results arrive. OpenRouter's web choice sets `enabled:true` but sends no active transitions; the no-search choices and Cerebras set `enabled:false`. It uses the selected **ask** choice/model (separate from explain), always answers in simple English for a non-native speaker, and asks for Markdown lists with one item per line when the answer is a list. All three ask providers accept images; the prompt labels them in capture order and treats their text as data. The Claude and OpenRouter web choices can search the web and fetch full pages or PDFs; Claude uses its basic direct tools, and OpenRouter runs DeepSeek's on Exa. |
| `GET /api/pron?word=&pos=` | comart | Merriam-Webster pronunciation. 404 `not_found` when MW has none, 502 `mw_unavailable` when MW refuses. |
| `GET /api/credits` | comart | Always `{"low":false,"remaining":null}`. The server tracks no balance. comart calls it after each explanation. |
| `POST /api/screenshot` | comart | Linux/GNOME only. Opens the desktop portal's interactive area selection, stores the image in memory under an unguessable id, and returns `{"id", "thumb", "width", "height"}`. The full image never leaves the backend; the client keeps the id and a small thumbnail. The portal's file is deleted as soon as it is read, including on a failed or oversized read. One Ask card is capped at 10 screenshots and 8 MiB of base64; images are scaled to a 1200px long edge (PNG under 512 KiB, otherwise JPEG) to stay inside Cerebras' request limit. |
| `POST /api/screenshot/release` | comart | Deletes screenshots by id: `{"ids":[…]}`. Called when a non-Ask Stop discards a take and when the Ask card (the parent of every round) closes. Idempotent; a two-hour idle TTL is the backstop. |
| `POST /api/transcribe/stream` | comart; daily-dictation; voice-typing (`?timestamps=0`) | The body is one recording streamed while it is made (`audio/*` or `video/*`). Sends it to the selected provider's live API: AssemblyAI `/v1/transcribe/live` or Deepgram `wss://api.deepgram.com/v1/listen`, with `ffmpeg` decoding the WebM/Opus stream to PCM. Answer: `{"text": …}`, plus `"first_word_ms"` and `"last_word_ms"` (milliseconds from the first sample) when the provider aligned the words — AssemblyAI does, and comart cuts the microphone clip's lead-in/lead-out there. Optional query `timestamps`: exactly `0` or `false` asks AssemblyAI for `"timestamps": false` and the answer is `{"text": …}` only; no query or any other value (`FALSE`, `off`, …) keeps the word timings. Deepgram ignores it. |
| `GET /api/transcribe/ws?type=<Content-Type>` | comart; daily-dictation | The WebSocket transport for the route above. Chrome refuses a streaming `fetch` request body to loopback from an extension page, so the browser sends the MediaRecorder chunks as binary frames and the text frame `stop` to finish. ai-service forwards the same bytes to `/api/transcribe/stream` over loopback and answers with one text frame: `{"text": …}` (and `first_word_ms` when it is known) or `{"error": …}`. |
| `POST /api/transcribe-verbatim` | daily-dictation | Multipart field `audio` (one segment file). Sends it to OpenRouter's MAI-Transcribe-2 (verbatim, word timestamps) and returns `{"lines": […], "words": […]}` with seconds timings. |
| `POST /api/memoir/check` | Cairn; comart | Proofreads an entry (`{"content": …}`, max 12000 UTF-16 code units). Plain JSON. |

The Memoir route does not stream: the client renders the finished result, so
the server collects the whole answer and replies once.

On an AssemblyAI transcription error the response carries `assemblyAIStatus`
and `retryAfter`; Deepgram errors carry the provider detail in `detail`. The
verbatim route carries `openRouterStatus`; it also keeps the historical
`azureStatus` field with the same value because daily-dictation already
classifies permanent vs retryable failures from it.

## Install, update, uninstall

macOS and Linux are supported.

| OS | Service | Definition file |
| --- | --- | --- |
| macOS | launchd LaunchAgent `com.ai-service.backend` | `~/Library/LaunchAgents/com.ai-service.backend.plist` |
| Linux | systemd user unit `ai-service.service` | `~/.config/systemd/user/ai-service.service` |

```sh
./install.sh             # install, or update after a code change
./install.sh --dry-run   # build nothing, prompt for nothing, start nothing; write the service definition to a temp dir and lint it
./uninstall.sh           # stop the service and remove its definition
```

`install.sh` is idempotent. Every real run does all of this (`--dry-run` skips
the build, the env file and the restart):

1. Checks the OS and that `go` is on `PATH` (on macOS, for example
   `brew install go`).
2. Builds `bin/ai-service` (`CGO_ENABLED=0` on Linux; on macOS cgo is on so
   the menu-bar picker can use Cocoa, which needs the Xcode command-line
   tools).
3. Creates `ai-service.env` from `.env.example` (mode 600) if it is missing.
   For each missing credential it asks in a hidden prompt:
   `ASSEMBLYAI_API_KEY` and/or `DEEPGRAM_API_KEY` for `/api/transcribe/stream`
   (at least one is required),
   `OPENROUTER_TRANSCRIBE_API_KEY` for `/api/transcribe-verbatim` (optional),
   `OPENROUTER_API_KEY` and/or `CLAUDE_API_KEY` for `/api/explain` (the
   shared `ASK_CEREBRAS_API_KEY` below also serves it);
   one or more of `ASK_CLAUDE_API_KEY`, `ASK_CEREBRAS_API_KEY` and
   `ASK_OPENROUTER_API_KEY`; and one of `MEMOIR_CLAUDE_API_KEY` or
   `MEMOIR_CEREBRAS_API_KEY`. The prompt is a macOS dialog; on Linux a zenity
   dialog, or a hidden terminal prompt without a display. For the transcribe
   providers it asks for AssemblyAI first; an empty answer there moves on to
   Deepgram, and an empty answer for either optional provider just leaves it
   out. An empty answer for `OPENROUTER_TRANSCRIBE_API_KEY` leaves verbatim
   unconfigured. For explain, OpenRouter is asked first; an empty answer there
   moves on to Claude. For ask, Claude is asked first; empty answers move on to
   Cerebras and then OpenRouter, and that Cerebras key also adds Cerebras to
   explain. A Claude or OpenRouter ask key gives both a
   web (search + fetch) and a no-search tray choice. If only some of a route's
   keys are present, the missing ones can be added by hand to add that
   provider. For the Memoir key it asks for Claude first; an empty answer there
   moves on to Cerebras. It stops if more than one
   variable of the Memoir route is already set, if `PARAKEET_URL` is
   still there, if the retired `GROQ_API_KEY` or `GROQ_MODEL` is there, if the
   retired `MAI_TRANSCRIBE_API_KEY`, `AZURE_SPEECH_KEY` or
   `AZURE_SPEECH_ENDPOINT` is there, or if the retired `CEREBRAS_API_KEY` is
   still there. It also checks that `ffmpeg` is on `PATH`, because both live
   transcription paths decode MediaRecorder WebM/MP4 to PCM.
   It never prints a key. Required keys are enforced here: if you cancel, enter
   nothing for a required key, or no prompt can be shown, `install.sh` stops
   before step 4 and does not (re)start the service. An empty
   `OPENROUTER_TRANSCRIBE_API_KEY` only skips verbatim. It also stops if a key
   contains a space, quote, backslash, `$` or backtick.
4. Writes the service definition, lints it (`plutil -lint` or
   `systemd-analyze --user verify`) and (re)starts the service.
5. Polls `http://127.0.0.1:8770/` for about 15 s, and, when `lsof` or `ss` is
   available, checks that the process on port 8770 is the service's own. A
   server started by hand cannot pass.

The first run needs the network (Go modules).

`--dry-run` builds nothing, never opens `ai-service.env` and starts nothing. It
prints the temp dir it wrote the definition to; delete that dir afterwards.

`uninstall.sh` removes only the service definition. It keeps `bin/`, `data/`,
`logs/` and `ai-service.env`, so `./install.sh` brings the service back.

Both service managers run `run.sh` at login, restart it whenever it exits, and
append its output to `logs/ai-service.log`. The systemd unit has
`StartLimitIntervalSec=0`, so like launchd's `KeepAlive` it never gives up
restarting.

**Linux: lingering.** A systemd user unit runs only while you are logged in,
unless lingering is on. `install.sh` prints a note when it is off. To keep the
service running without a login session:

```sh
loginctl enable-linger "$(id -un)"
```

**Moved the folder? Re-run `./install.sh`.** The service definition holds
absolute paths. `run.sh` finds everything else relative to itself.

- macOS: keep the folder out of `~/Documents`, `~/Desktop`, `~/Downloads`,
  iCloud Drive and `~/Library/CloudStorage`. launchd jobs cannot read those
  folders (privacy protection), so `install.sh` refuses them.
- Linux: the path must not contain whitespace or `%` (systemd would misread it).

## Manage the running service

Linux:

```sh
curl -fsS http://127.0.0.1:8770/                 # is it up? one line of text
systemctl --user status ai-service
systemctl --user restart ai-service
systemctl --user stop ai-service                # until you start it, re-run install.sh, or reboot
tail -f logs/ai-service.log                     # the server's own log
journalctl --user -u ai-service                 # systemd's start/stop/crash messages only
```

macOS:

```sh
U=gui/$(id -u)
curl -fsS http://127.0.0.1:8770/
launchctl print $U/com.ai-service.backend | grep -E 'state|pid|last exit'
launchctl kickstart -k $U/com.ai-service.backend     # restart
launchctl bootout $U/com.ai-service.backend          # stop, until the next login or install.sh
launchctl bootstrap $U ~/Library/LaunchAgents/com.ai-service.backend.plist   # start again
tail -f logs/ai-service.log
```

On macOS these are **gui-domain** agents (`gui/<uid>`), and on Linux a **user**
unit (`systemctl --user`). Asked in the system domain, both managers say they
know nothing about the service, while it answers on 8770. Killing the process
does not stop it: the manager starts it again.

Shutdown is graceful. On SIGTERM or SIGINT the server stops accepting, gives
running requests up to 10 s, then cuts them off. It then waits up to 5 s more
for the cut-off handlers to finish and closes the cache. A second signal kills
it at once.

## Run by hand

Stop the service first, or port 8770 is taken. Then:

```sh
./run.sh      # foreground; Ctrl-C stops it
```

`run.sh` is what both service managers run. It loads `ai-service.env`,
defaults `CACHE_DB` to a path in this folder, and `exec`s
`bin/ai-service`. Use it rather than the bare binary: the binary does not read
`ai-service.env` itself, and without `run.sh` it assumes the folder is
`~/ai-service`. Start the service again when you are done.

## Configuration

`ai-service.env` is a **secret** file (mode 600). `install.sh` creates it from
`.env.example`, which documents every key:

| Key | Default | Meaning |
| --- | --- | --- |
| `ASSEMBLYAI_API_KEY` | (empty) | Secret. AssemblyAI key for Universal-3.5 Pro. Set this and/or `DEEPGRAM_API_KEY`; at least one is required. The streaming path sends the recording to `sync.assemblyai.com/v1/transcribe/live`, with `ffmpeg` decoding MediaRecorder WebM/MP4 to 16 kHz mono PCM. |
| `DEEPGRAM_API_KEY` | (empty) | Secret. Deepgram key for Nova-3 live transcription. Set this and/or `ASSEMBLYAI_API_KEY`; at least one is required. The stream goes to `wss://api.deepgram.com/v1/listen`; `ffmpeg` decodes MediaRecorder WebM/MP4 to 16 kHz mono PCM. |
| `OPENROUTER_TRANSCRIBE_API_KEY` | (empty) | Secret. OpenRouter key for MAI-Transcribe-2 on `/api/transcribe-verbatim` only. It is separate from `OPENROUTER_API_KEY` (that one is `/api/explain`'s), so each route keeps its own key. |
| `OPENROUTER_API_KEY` | (empty) | Secret. OpenRouter key for `/api/explain`. Set this, `CLAUDE_API_KEY` and/or `ASK_CEREBRAS_API_KEY`; with more than one, the tray picker chooses. |
| `CLAUDE_API_KEY` | (empty) | Secret. Claude Platform (`api.anthropic.com`) key for `/api/explain`. Set this, `OPENROUTER_API_KEY` and/or `ASK_CEREBRAS_API_KEY`; with more than one, the tray picker chooses. |
| `ASK_CLAUDE_API_KEY` | (empty) | Secret. Claude Platform (`api.anthropic.com`) key for `/api/ask`. It creates a web (search + fetch) and a no-search choice in the ask tray. Set this and/or `ASK_CEREBRAS_API_KEY` or `ASK_OPENROUTER_API_KEY`; any additional key adds its own choice(s). Separate from `CLAUDE_API_KEY` (that one is `/api/explain`'s), though it may hold the same key. |
| `ASK_CEREBRAS_API_KEY` | (empty) | Secret. Cerebras key for `/api/ask` and `/api/explain`: it creates one Ask choice and one Explain choice. Set this and/or `ASK_CLAUDE_API_KEY` or `ASK_OPENROUTER_API_KEY` for Ask; the Cerebras key is shared with Explain on purpose. Separate from `MEMOIR_CEREBRAS_API_KEY` so Memoir keeps its own provider. |
| `ASK_OPENROUTER_API_KEY` | (empty) | Secret. OpenRouter key for `/api/ask`'s DeepSeek V4.1 Flash 1017 provider. It creates a web (search + fetch) and a no-search choice in the ask tray. Set this and/or `ASK_CLAUDE_API_KEY` or `ASK_CEREBRAS_API_KEY`; any additional key adds its own choice(s). Separate from `OPENROUTER_API_KEY` (that one is `/api/explain`'s), though it may hold the same key. |
| `MEMOIR_CLAUDE_API_KEY` | (empty) | Secret. Claude Platform key for `/api/memoir/check`. Set this **or** `MEMOIR_CEREBRAS_API_KEY`, never both. A separate variable from `CLAUDE_API_KEY` (that one is `/api/explain`'s), and it may hold the same key. |
| `MEMOIR_CEREBRAS_API_KEY` | (empty) | Secret. Cerebras key for `/api/memoir/check`. Set this **or** `MEMOIR_CLAUDE_API_KEY`, never both. Called `CEREBRAS_API_KEY` before 2026-10-20: the old name makes the service refuse to start. |
| `MEMOIR_MODEL` | `claude-opus-5-5` on Claude, `qwen-3.8-27b` on Cerebras | Model id for `/api/memoir/check`, for whichever provider the key selects. With a Claude key it must start with `claude-`. |
| `LISTEN_ADDR` | `127.0.0.1:8770` | Keep it. The clients and the health check expect this address. |

Models and thinking depth are fixed in `internal/config`, not environment
settings: Claude uses `output_config.effort` `medium`; OpenRouter uses a
1024-token thinking budget for `/api/explain` and `reasoning.effort` `high`
for `/api/ask`; and Cerebras uses `reasoning_effort` `medium`. A Claude or
OpenRouter Ask key creates two tray choices: the web-enabled one also sends
the provider's web tools (Claude's basic direct search and fetch, max 2 of
each with 30k tokens per fetch; OpenRouter Exa, up to 10 results and two
searches per request, plus two fetches with 30k tokens each), and the
no-search one sends no tools. There are no `*_EFFORT`,
`*_REASONING_EFFORT` or `EXPLAIN_REASONING_TOKENS` settings; leftover lines
from older installs are ignored.

**OpenRouter verbatim formats.** OpenRouter's STT endpoint takes base64 JSON,
not multipart. MAI-Transcribe-2 is served by Azure, so OpenRouter caps uploads at
**25 MB** per request. `/api/transcribe-verbatim` sends daily-dictation's file
as-is in WAV, MP3, FLAC, M4A, OGG, WebM or AAC format; its 25 MB cap is about
26 minutes of 128 kbps MP3. OpenRouter also documents a 60-second upstream
processing deadline. MAI is roughly 300× real-time, so the 50–100 s clips
daily-dictation sends finish far below it; chunk anything much longer.

**AssemblyAI streaming.** `ASSEMBLYAI_API_KEY` is the `/api/transcribe/stream`
provider. It uses AssemblyAI's native
`POST https://sync.assemblyai.com/v1/transcribe/live`, with `X-AAI-Model:
universal-3-5-pro`. The clients send their MediaRecorder WebM/MP4 stream to
`/api/transcribe/stream` while recording; ai-service pipes it through `ffmpeg`
to 16 kHz mono PCM, sends the multipart `config` part first and the PCM `audio`
part second, and closes the AssemblyAI request when the client body ends. That
close is the finish signal; AssemblyAI returns one final JSON transcript, not
per-chunk results. The config part is
`{"sample_rate":16000,"channels":1,"timestamps":true}`, so each word
carries millisecond `start`/`end`; `first_word_ms` and
`last_word_ms` are the first aligned word's start and the last aligned word's
end, which comart uses to trim the silence before and after speech. Timestamps are exact
or absent (AssemblyAI never estimates one), so the field is omitted when no word
could be aligned; they add roughly 0.1–0.3 s to the post-stop wait. A caller that
only wants the text (voice-typing) sends `?timestamps=0` (or exactly `false`):
the config part then says `"timestamps":false` and the answer carries neither
field. The WS transport forwards without a query (a `timestamps` on the WS URL
is ignored), so comart keeps its timings. The browser clients reach this route through
`/api/transcribe/ws`: Chrome blocks a streaming `fetch` body to loopback from an
extension page, while a WebSocket to the same host works. The WS handler only
carries bytes; the decoding is the stream route's.

Measured on this host with a warm connection, the post-stop wait was ~0.35 s for
a 6 s clip and ~0.9 s for a 30 s clip. Transcripts were identical in 24 paired
runs on one clip. Live audio is capped at 120 s by AssemblyAI; the mic clients
stop at 119 s so the 200ms cap poll and encoder chunking cannot push a clip
over the provider's hard edge. The live request has a 300 s deadline, but the
HTTP client that carries it has a 10-minute backstop — a two-minute recording
plus the final answer must not be cut off by the old 120 s client timeout.
`/api/transcribe/stream` caps the uploaded body at 25 MB.

**Deepgram streaming.** `DEEPGRAM_API_KEY` uses Deepgram's live WebSocket at
`wss://api.deepgram.com/v1/listen` with `model=nova-3` and `language=en`.
That is Deepgram's **Nova-3 Monolingual** tier: a specific language is cheaper
than `language=multi` (Nova-3 Multilingual), and this project is English-only.
The request asks for `smart_format=true`, `punctuate=true`,
`encoding=linear16`, `sample_rate=16000`, `channels=1`, `interim_results=false`
and `endpointing=300`. ai-service sends the ffmpeg-produced PCM as binary
WebSocket frames, then sends `{"type":"CloseStream"}`. Deepgram emits `Results`
at speech boundaries, so the handler joins every final (`is_final: true`)
transcript and returns the usual `{"text": …}`.

Measured end to end on 2026-10-27 through comart’s exact WebSocket transport
(`GET /api/transcribe/ws`), with real WebM/Opus chunks fed at 1× real time. The
timer starts when the client sends the text frame `stop` and stops when the
final JSON transcript arrives. Deepgram is the median of six runs per clip;
AssemblyAI is the median of three. Ranges are in parentheses.

| Clip | AssemblyAI | Deepgram Nova-3 |
| --- | --- | --- |
| 6 s | 394 ms (390–394) | 309 ms (237–346) |
| 12 s | 520 ms (515–523) | 396 ms (258–440) |
| 20 s | 657 ms (657–659) | 297 ms (263–320) |
| 30 s | 912 ms (904–935) | 287 ms (262–312) |

Deepgram stays roughly flat as the clip grows and is about 1.3× faster at 6 s,
rising to about 3.2× faster at 30 s. AssemblyAI’s post-stop time grows with
duration.

Every `/api/transcribe/stream` request now logs one line naming the provider,
the HTTP status, the decoded audio duration, the total handler time and the
post-stop time (from end of audio input to the final response), for example:

```text
transcribe[deepgram]: type=audio/webm status=200 audio=6.08s total=6.42s post-stop=380ms
```

The line never includes audio or transcript text.

Every `/api/explain` request logs the selected provider and model, HTTP status,
outcome, item count, pronunciation lookups and duration:

```text
explain[claude]: model=claude-opus-5-5 status=200 outcome=ok items=3 prons=2 duration=1.24s
```

Tray switches are logged separately as `transcribe provider: … (tray)`,
`explain provider: … (tray)` and `ask provider: … (tray)`.

`CACHE_DB` is normally not set: `run.sh` defaults it to `data/cache.db`, so the
folder can move without an edit. On Linux the unit is sandboxed with
`ProtectSystem=strict` and `ProtectHome=read-only`, with `data/`, `logs/` and
the desktop's `~/Pictures/Screenshots` directory writable. The screenshot
directory is the one exception because GNOME's portal saves every interactive
capture there; ai-service reads it and deletes the PNG immediately. Do not
point anything else outside `data/`.

`run.sh` sources the file with bash (`set -a`). Keep to plain `KEY=value` lines
and put single quotes around any value with spaces, `$` or parentheses (inside
double quotes, bash still expands `$`). A line bash cannot run stops the
server at start; the service manager retries every 10 s and the error is in
`logs/ai-service.log`.

After an edit, restart the service. A missing API key is only a warning at
start: `/api/transcribe-verbatim` answers 500, `/api/explain` and `/api/ask`
send an `error` event and `/api/memoir/check` answers 503, each naming the
missing key, until it is set. `/api/transcribe/stream` is the exception: without either
`ASSEMBLYAI_API_KEY` or `DEEPGRAM_API_KEY` the service refuses to start.

**The service refuses to start** (it logs `config: …` and exits 1; the service
manager keeps retrying every 10 s) when a route has no usable provider:

- neither `ASSEMBLYAI_API_KEY` nor `DEEPGRAM_API_KEY` is set, so
  `/api/transcribe/stream` has no provider;
- the retired `PARAKEET_URL` is still set (the local Parakeet integration was
  removed);
- the retired `GROQ_API_KEY` or `GROQ_MODEL` is still set. Groq was removed when
  `/api/transcribe/stream` moved to AssemblyAI and Deepgram;
- the retired `MAI_TRANSCRIBE_API_KEY`, `AZURE_SPEECH_KEY` or
  `AZURE_SPEECH_ENDPOINT` is still set. MAI-Transcribe-2 now goes through
  OpenRouter, and `/api/transcribe-verbatim` reads
  `OPENROUTER_TRANSCRIBE_API_KEY`;
- both `MEMOIR_CEREBRAS_API_KEY` and `MEMOIR_CLAUDE_API_KEY` are set (same rule:
  leave the unused one empty or commented out);
- `MEMOIR_CLAUDE_API_KEY` is set and `MEMOIR_MODEL` is not a `claude-…` id (for
  example a leftover `qwen-3.8-27b`);
- the retired `CEREBRAS_API_KEY` is still set. It is `MEMOIR_CEREBRAS_API_KEY`
  now; the old name would otherwise sit in the file doing nothing.

Why: with multiple possible providers it is easy to believe one model answers while
another does. Every start logs the routes it chose, for example
`transcribe route: AssemblyAI live, model universal-3-5-pro`,
`explain route: Claude Platform, model claude-opus-5-5, effort medium` (or every
explain provider with `N providers; tray picker, starting on …`),
`ask route: …`, and the same for `memoir route: …`, and every Claude request
whose stream starts logs the model it asked for next to the model the API says
answered (`llm[Claude/explain]`, `llm[Claude/ask]` or `llm[Claude/memoir]`).
The Anthropic SDK's own `ANTHROPIC_*` environment variables are
switched off, so a stray `ANTHROPIC_BASE_URL` or `ANTHROPIC_AUTH_TOKEN` cannot
re-route the call.

### Tray pickers

The tray has one picker section per route with more than one choice. It
appears when `/api/transcribe/stream`, `/api/explain` or `/api/ask` has at
least two entries:

- **Transcribe with** — AssemblyAI Universal-3.5 Pro and Deepgram Nova-3.
- **Explain with** — Claude Platform Opus 5.5, OpenRouter Gemini 3.8 Flash and
  Cerebras Qwen 3.8 27B.
- **Ask with** — Claude Platform Opus 5.5 (search + fetch), Claude Platform
  Opus 5.5, Cerebras Qwen 3.8 27B, OpenRouter DeepSeek V4.1 Flash 1017
  (search + fetch), and OpenRouter DeepSeek V4.1 Flash 1017.

A single Claude or OpenRouter Ask key already gives two choices, so its picker
appears without a second credential. A saved `ask:provider` value of `claude`
or `openrouter` now means the web choice; pick the no-search entry once if
you want the old behavior. With one choice on every route there is no icon. The
tray lives in the macOS menu bar or the Linux status area
(GNOME's AppIndicator extension, KDE, and anything else implementing
StatusNotifierItem). Adding another provider brings its section back without
any client change, because comart keeps calling the same routes.

- Each route starts on its first configured provider and remembers a pick in
  the SQLite cache (`data/cache.db`, keys `transcribe:provider`,
  `explain:provider` and `ask:provider`), so a restart keeps it.
- If a saved pick's key is later removed, that route falls back to its first
  configured provider instead of routing to a service with no credentials.
- Linux needs the service to have the session D-Bus address; the systemd user
  manager imports it at login. Without a desktop session the service still
  serves HTTP, logs `tray: no session D-Bus …`, and skips the icon.
- The service outlives the session bus: it keeps running through a logout
  (lingering), while GNOME restarts the user's `dbus.service`. The tray then
  logs `tray: session bus lost; reconnecting`, retries on a new private
  connection after 1 s, then backs off to every 5 s (only the first failure
  is logged, as `tray: reconnect: …`), publishes the same icon and menu
  (current picks kept), registers again and logs `tray: reconnected`, so the
  icon is back after the next login without a service restart.
- macOS builds the tray with cgo/Cocoa, so `install.sh` needs the Xcode
  command-line tools (`xcode-select --install`). Linux uses a small in-repo
  StatusNotifierItem/dbusmenu backend (`internal/tray/tray_linux.go`) and keeps
  the static `CGO_ENABLED=0` build. It deliberately omits the optional
  `Activate` method: GNOME's AppIndicator extension waits out the double-click
  interval before opening a menu for items that advertise it. Without it, one
  click opens the picker.
- Linux registers the item by object path only. Also requesting a well-known
  `org.kde.StatusNotifierItem-<pid>` name makes the GNOME AppIndicator watcher
  discover the same item twice after a shell restart, which shows two identical
  tray icons.
- GNOME has no per-icon tray-order setting: the AppIndicator extension inserts
  every icon at index 1 when it registers, so the last icon to register gets
  that slot. The optional extension in `contrib/gnome-shell/` keeps this icon
  at index 1 (second from the left) while the Voice Typing extension keeps
  index 0. It is installed and enabled on its own, not by the service's
  `install.sh`; see `contrib/gnome-shell/ai-service-tray@james.local/README.md`.
- The tray is part of the service process, not an HTTP route. It writes only the
  two cache rows above; no new listening socket, no extra file.

## Security model

There is no auth, by design. **Loopback is the trust boundary**: the server
binds `127.0.0.1` and nothing fronts it.

Binding loopback keeps other machines out, but not web pages open in this
machine's browser. So every request passes a browser-origin guard
(`loopbackOnly` in `internal/server/server.go`). Anything that fails it gets
403 `{"error":"Forbidden."}`.

- The `Host` must be `127.0.0.1`, `localhost` or `::1` (or empty). This stops
  DNS rebinding, where a web page reaches the server under its own host name
  and reads the answers.
- An `Origin` header, if present, must be `null`, a non-web scheme
  (`chrome-extension://`, `file://`, …) or an http(s) origin on a loopback
  host. This stops cross-site POSTs from web pages.

The comart extension (`chrome-extension://` pages) and Cairn (Electron,
`file://` or no Origin) pass unchanged.

The billed routes also require the Content-Type the clients really send, and
answer 415 otherwise:

- `application/json` on `/api/explain`, `/api/ask`, `/api/screenshot`,
  `/api/screenshot/release` and `/api/memoir/check`;
- `audio/*` (or `video/*`) on `/api/transcribe/stream` and as the `type` query on
  `/api/transcribe/ws`;
- `multipart/form-data` on `/api/transcribe-verbatim`.

A web page can send a cross-site POST without a CORS preflight only as
`text/plain` or a form type, and a sandboxed iframe (`Origin: null`) passes the
guard. The Content-Type rule closes that gap.

`/api/screenshot` can only ask the desktop portal to open its interactive
selection; it cannot read screen pixels without the person at the machine
choosing an area. The capture is stored in memory, never served back over HTTP,
and deleted by `/api/screenshot/release` or the idle TTL.

Request limits:

- `/api/explain` and `/api/ask`: 5 MiB JSON body (413 `Request too large.`).
- `/api/screenshot`: 4 KiB JSON body (413); `/api/screenshot/release`: 5 MiB. Both are loopback-only and require `application/json`.
- `/api/memoir/check`: 5 MiB JSON body (a larger one fails as 400
  `Invalid JSON.`), plus the character caps in the route table.
- `/api/transcribe/stream`: 25 MiB streamed audio (413 `recording too long`).
  A 60 s MediaRecorder clip is well under 1 MiB; a two-minute clip is roughly 2 MiB.
- `/api/transcribe-verbatim`: 25 MiB audio (413 `audio too long`). This is
  OpenRouter's cap for the Azure-backed MAI-Transcribe-2 endpoint; base64 adds
  about a third on the wire.

**Still not protected:**

- Any program running on this machine, under any local user, can call every
  route and spend the OpenRouter, AssemblyAI, Deepgram, Claude and Cerebras
  quota.
  There is no rate limit.
- A web page can still send blind GET requests (for example an `<img>` tag):
  no Origin header is sent, so the guard lets them through. The page cannot
  read the answer. The GET routes bill nothing; `/api/pron` can at most cause
  Merriam-Webster lookups.
- The API keys are in a plain file on disk, `ai-service.env`. It is not
  encrypted: any program that runs as you (or as root) can read and use them.
  Older zips in `backups/`, if any, hold secrets too; keep that folder private.

## Models

| Used by | Provider | Model | Reasoning |
| --- | --- | --- | --- |
| `/api/explain` | Claude Platform, if `CLAUDE_API_KEY` is set | `claude-opus-5-5` | effort `medium` |
| `/api/explain` | OpenRouter, if `OPENROUTER_API_KEY` is set | `google/gemini-3.8-flash` | 1024-token thinking budget |
| `/api/explain` | Cerebras, if `ASK_CEREBRAS_API_KEY` is set | `qwen-3.8-27b` | `medium` |
| `/api/ask` | Claude Platform, if `ASK_CLAUDE_API_KEY` is set | `claude-opus-5-5` | effort `medium`; basic web search + fetch, direct, max 2 each |
| `/api/ask` | Claude Platform, no-search choice | `claude-opus-5-5` | effort `medium`; no web tools |
| `/api/ask` | Cerebras, if `ASK_CEREBRAS_API_KEY` is set | `qwen-3.8-27b` | `medium` |
| `/api/ask` | OpenRouter, if `ASK_OPENROUTER_API_KEY` is set | `deepseek/deepseek-v4.1-flash-1017` | effort `high`; web search + fetch on |
| `/api/ask` | OpenRouter, no-search choice | `deepseek/deepseek-v4.1-flash-1017` | effort `high`; no web tools |
| `/api/memoir/check` | Claude Platform, if `MEMOIR_CLAUDE_API_KEY` is set | `claude-opus-5-5` | effort `medium` |
| `/api/memoir/check` | Cerebras, if `MEMOIR_CEREBRAS_API_KEY` is set | `qwen-3.8-27b` | `medium` |
| `/api/transcribe/stream` | AssemblyAI, if `ASSEMBLYAI_API_KEY` is set | `universal-3-5-pro` (`/v1/transcribe/live`; WebM decoded to PCM by `ffmpeg`) | — |
| `/api/transcribe/stream` | Deepgram, if `DEEPGRAM_API_KEY` is set | `nova-3` (`wss://api.deepgram.com/v1/listen`; monolingual `language=en`) | — |
| `/api/transcribe-verbatim` | OpenRouter (`OPENROUTER_TRANSCRIBE_API_KEY`) | `microsoft/mai-transcribe-2` (verbatim, word timestamps) | — |

When more than one `/api/explain` key is set, its tray section chooses between
the Claude, OpenRouter and Cerebras rows; `ASK_CEREBRAS_API_KEY` supplies the
last one and is shared with Ask. The Ask keys and tray section are otherwise
independent of explain's; its Claude and OpenRouter keys each
offer a web (search + fetch) and a no-search choice.

OpenRouter and Cerebras speak OpenAI's streaming chat completions, so one client
(`internal/llm`'s `Client`) serves both. The only difference is the request fields for
the token budget and reasoning, which `internal/config` fixes for every route:
OpenRouter explain takes `max_tokens` and `reasoning: {max_tokens: 1024, exclude}`;
OpenRouter ask takes `max_tokens` and `reasoning: {effort: "high", exclude}`, and
its web choice also adds the `openrouter:web_search` and `openrouter:web_fetch`
server tools; Cerebras takes `max_completion_tokens` and
`reasoning_effort: medium` on explain, ask and Memoir.

`/api/ask` also carries screenshots. All three Ask models are vision-capable:
Cerebras documents `qwen-3.8-27b` base64 PNG/JPEG image inputs, Claude accepts
image blocks, and OpenRouter lists `deepseek/deepseek-v4.1-flash-1017` as
`text+image->text`. The clients therefore send one user message with the prompt
text followed by the images in capture order; the prompt says how to read that
order. A future Ask provider must implement `llm.ImageStreamer`; otherwise a
request with screenshots reports that the model does not accept them.

Claude is called directly on the Claude Platform with the official Go SDK, for
whichever of the routes (explain, ask, Memoir) has a Claude key
(`internal/llm/claude.go`), through the Messages API. The request carries the
model, `max_tokens`, adaptive thinking (`thinking: {type: "adaptive"}`,
requested explicitly because omitting it means no thinking on some older
models), `output_config.effort: medium`, the prompt and the SDK's stream flag,
and nothing else. Thinking text is dropped, never streamed. Effort is the only
thinking-depth control: Opus 5.5
rejects a separate thinking-token budget, while `max_tokens` still caps the
answer. No refusal fallback is enabled: it would answer from another model
after a refusal, which is the silent re-routing the explicit provider choice
exists to prevent. A refusal reaches the client as an `error` event on explain,
and as a 502 on Memoir. Retries,
error codes and the "judge the answer at the end" rule below apply to Claude
too; Claude's stream is complete once its stop reason has arrived.

How a call works:

- The prompt goes out as one user message. Reasoning is never streamed back to
  a client: only answer text reaches it.
- A 429 or 5xx before any text arrives is retried twice (after 1 s and 2 s,
  or the provider's `Retry-After` if longer; one over 5 s fails at once).
  Other HTTP errors fail at once, with the provider's message.
- An answer is judged when the stream ends. An error inside the stream, a
  connection that breaks before `[DONE]` (Claude: before a stop reason), a stop
  at the token limit and an empty answer are all errors, even if some text
  already went out. A break after the final `finish_reason` is not: the answer
  is already whole. So neither the panel nor Memoir's parser takes a partial
  answer for a whole one.

**Why these models** (measured 2026-09-29 on 12 cases per route, blind-judged;
Memoir moved to Claude Opus 5.5 on 2026-10-20 and Qwen stays as the Cerebras
option):
on Memoir, Qwen at `medium` was as good as the previous, much larger model and
answered 3–5× faster (check: 1.6 s median). On explain, Gemini 3.8 Flash clearly beat Qwen
at every reasoning level: Qwen got pronunciations wrong. **Never run Memoir on
Cerebras with reasoning off:** Qwen then breaks the JSON contract, edits other people's
quoted words and leaves unlisted edits in `corrected`.

As of 2026-10, Cerebras allows 150k tokens a minute on this account. An explain
prompt is about 10k tokens and a Memoir prompt 2–4k, so only a burst of
parallel requests reaches it; the retry covers a short one.

## Merriam-Webster

`www.merriam-webster.com` sits behind Cloudflare's bot challenge, which blocks
a plain HTTP client. `internal/mw` gets past it with a TLS/HTTP2 fingerprint of
current Chrome (`github.com/bogdanfinn/tls-client`, profile `Chrome_146`). No
browser and no extra service are involved.

Older Chrome profiles get flagged. **If lookups start returning
`mw_unavailable`, move to a newer Chrome profile.** In `internal/mw/mw.go`,
change the Chrome version in three places together: `profiles.Chrome_146`,
`UA`, and the `sec-ch-ua` header in `browserHeaders`. Cloudflare checks that
the User-Agent matches the TLS fingerprint, so a profile change alone is not
enough. If the pinned tls-client has no newer profile (v1.15.1 stops at
`Chrome_146`), update it first:
`go get github.com/bogdanfinn/tls-client@latest && go mod tidy`. Then re-run
`./install.sh` to rebuild and restart.

Results are cached for 30 days in `data/cache.db` (SQLite, key prefix `mw:`;
the rest of the key is a lowercased, escaped headword).
`/api/explain` answers cached words at once, sends the misses to MW at most 4
at a time, and waits at most 10 s in total before it explains without the rest.

## Code layout

```
cmd/ai-service/    main: config, cache, server, graceful shutdown
internal/config     configuration from the environment
internal/store      SQLite cache
internal/server     HTTP routing, the security guard, the handlers, the in-memory
                    screenshot store and its Linux/GNOME D-Bus portal
internal/tray       the provider pickers for /api/transcribe/stream, /api/explain and /api/ask (macOS menu bar, Linux status area)
internal/mw         Merriam-Webster pronunciation lookup
internal/prompt     the explain prompt (part1.txt, part2.txt)
internal/memoir     Memoir prompt (check.txt) and result parsing
internal/llm        streaming model clients: chat completions (OpenRouter, Cerebras) and Claude
contrib/gnome-shell optional GNOME Shell extension that pins the tray icon at index 1
install.sh, uninstall.sh, run.sh
```

The prompt `.txt` files are product text. `TestPromptParity` pins the Memoir
prompt byte for byte, so a change to `internal/memoir/check.txt` must be
deliberate.

## Build and test

```sh
CGO_ENABLED=0 go build -o bin/ai-service ./cmd/ai-service   # Linux: static
CGO_ENABLED=1 go build -o bin/ai-service ./cmd/ai-service   # macOS: Cocoa tray
go vet ./...
go test ./...
```

On Linux the result is one static binary with no cgo; macOS links Cocoa for the
menu-bar picker. At runtime it needs the SQLite
file it creates on first run; credentials come from `ai-service.env`, and the
AssemblyAI and Deepgram streaming paths need `ffmpeg` on `PATH`. After a build, restart the service to
run the new binary
(`systemctl --user restart ai-service`, or
`launchctl kickstart -k gui/$(id -u)/com.ai-service.backend`), or just run
`./install.sh`.

## Version control and secrets

The code is in git (GitHub repository `okoyfoeciov/ai-service`). Git is the undo for code.

`.gitignore` keeps out build output (`bin/`), runtime state (`data/`, `logs/`,
`backups/`) and every secret: `ai-service.env`, any `*.env` and `*.har`
network captures. **Never commit a secret**, and never force-add an ignored
file.

Old `backups/` zips, if any, may hold secrets; see `Security model` above.
