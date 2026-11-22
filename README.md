# ai-service

A small localhost **LLM backend**: one static Go binary on `127.0.0.1:8770`,
speaking JSON and SSE. It has four clients:

- the **comart Chrome extension** (`~/comart/chrome`): word
  explanations and pronunciations (its microphone was removed on 2026-11-21);
- **comart's desktop app** (`~/comart`, the transcript card): mic
  transcription through `POST /api/transcribe/stream`, streamed by its
  Electron main process, and the card's writing check, Ask and screenshots;
- **Cairn's Memoir tab** (`~/cairn`, `src/memoir.ts`): proofreading of
  journal entries;
- **daily-dictation** (`~/daily-dictation`): verbatim transcripts through
  `POST /api/transcribe-verbatim`.

**There is no UI here.** All four clients carry their own UI. `GET /`
answers one line of plain text and nothing else. Do not add a template, a
static directory or an `embed` back: a served copy of a client would have to be
kept in step with the real one by hand.

The comart API is deliberately stable: no CORS, no auth, and only the routes
the clients actually call. `/api/ask` is the desktop app's spoken-question route, and
`/api/screenshot` is its screen-capture route on Linux and macOS (the image is kept here
under an id and released by the client); Memoir's `/api/memoir/check` and
daily-dictation's `/api/transcribe-verbatim` are the other deliberate
additions. Change a route only together with the client that calls it.

## Routes

| Route | Client | What it does |
| --- | --- | --- |
| `GET /` | health check | One line of `text/plain`. Keep it one line. |
| `POST /api/explain` | comart; Cairn (EPUB reader) | Explains the chosen words. SSE events: `pron-data`, `chunk`, `error`, `done` (`done` is always last). An error after the stream opens is an `error` event; the HTTP status stays 200. |
| `POST /api/ask` | comart (desktop app) | Answers a spoken question about passages the reader picked. Body: `{"question": …, "items": [{phrase, words}], "contexts": [{phrase, text}], "history": [{question, answer}], "screenshots": [id, …], "timeline": [{"kind":"text","phrase":…} | {"kind":"screenshot","screenshot":id}]}`. `history` is optional and carries the earlier rounds of the same Ask card; `screenshots` is optional and names images stored by `/api/screenshot`, in capture order; `timeline` is optional and lists the selected text and screenshots in the exact order the reader added them this round, so the prompt can interleave them. Same SSE events as `/api/explain` (`chunk`, `error`, `done`), no pronunciation prefetch, plus an initial `search` event for every Ask (`{"active":false,"enabled":true}`) and `{"active":true|false,"enabled":true}` when the provider's own search or fetch starts and its results arrive (Claude's native tools; OpenRouter's server tools, reported by its Responses API). It uses the selected **ask** choice/model (separate from explain), always answers in simple English for a non-native speaker, and asks for Markdown lists with one item per line when the answer is a list. Both Ask providers accept images; the prompt labels them in capture order and treats their text as data. Both can search the web and fetch full pages or PDFs; Claude uses its basic direct tools, and OpenRouter runs GPT-6.2 Sol's on Exa. When `BRAVE_SEARCH_API_KEY` is set, every Ask choice also gets two native client tools: `search_images({"query":…})` and `show_images({"image_ids":[…]})`. The main model decides whether to call them. `search_images` asks Brave's Image Search API for 10 results (`country=ALL`, `search_lang=en`, `safesearch=strict`, `spellcheck=false`) and returns candidate IDs; it never displays anything. `show_images` sends the chosen IDs as an `images` SSE event (`{"round":n,"status":"ready","images":[{id,title,source,page,thumb,original,width,height}]}`) for the card's gallery. Each Ask round is capped at four `search_images` calls. Claude receives the top five candidates as image blocks in its tool result; OpenRouter receives ten metadata rows. Search failures and empty results send a non-fatal `images` event with `status:"error"|"empty"`; the answer continues. |
| `GET /api/pron?word=&pos=` | comart; Cairn (EPUB reader) | Merriam-Webster pronunciation. 404 `not_found` when MW has none, 502 `mw_unavailable` when MW refuses. |
| `GET /api/credits` | comart | Always `{"low":false,"remaining":null}`. The server tracks no balance. comart calls it after each explanation. |
| `POST /api/screenshot` | comart (desktop app) | Linux/GNOME and macOS (other platforms answer 501; macOS needs Screen Recording permission for the service in System Settings → Privacy & Security). Opens the desktop's interactive area selection (the GNOME portal, or macOS `screencapture -i -s`; one drag, captured on mouse release), stores the image in memory under an unguessable id, and returns `{"id", "thumb", "width", "height"}`. The full image never leaves the backend; the client keeps the id and a small thumbnail. The capture file is deleted as soon as it is read, including on a failed or oversized read; on macOS it lives in a private temp directory that is removed on every path, a second request while a selection is open gets 429 (not 409, which comart reads as a cancel), and ending the request kills `screencapture`. One Ask card is capped at 10 screenshots and 8 MiB of base64; images are scaled to a 1200px long edge (PNG under 512 KiB, otherwise JPEG) to keep each request within the Ask models' image limits. |
| `POST /api/screenshot/release` | comart (desktop app) | Deletes screenshots by id: `{"ids":[…]}`. Called when a non-Ask Stop discards a take and when the Ask card (the parent of every round) closes. Idempotent; a two-hour idle TTL is the backstop. |
| `POST /api/transcribe/stream` | comart (desktop app, its Electron main process) | The body is one recording streamed while it is made (`audio/*` or `video/*`). Sends it to the selected provider's live API: AssemblyAI `/v1/transcribe/live` or Deepgram `wss://api.deepgram.com/v1/listen`, with `ffmpeg` decoding the WebM/Opus stream to PCM. Answer: `{"text": …}`, plus `"first_word_ms"` and `"last_word_ms"` (milliseconds from the first sample) when the provider aligned the words — AssemblyAI does, and comart cuts the microphone clip's lead-in/lead-out there. Optional query `timestamps`: exactly `0` or `false` asks AssemblyAI for `"timestamps": false` and the answer is `{"text": …}` only; no query or any other value (`FALSE`, `off`, …) keeps the word timings. Deepgram ignores it. |
| `POST /api/transcribe-verbatim` | daily-dictation | Multipart field `audio` (one segment file). Sends it to OpenRouter's MAI-Transcribe-2 (verbatim, word timestamps) and returns `{"lines": […], "words": […]}` with seconds timings. |
| `POST /api/memoir/check` | Cairn; comart (desktop app) | Proofreads an entry (`{"content": …}`, max 12000 UTF-16 code units). Plain JSON. |

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
   `OPENROUTER_API_KEY` and/or `CLAUDE_API_KEY` for `/api/explain`;
   `ASK_CLAUDE_API_KEY` and/or `ASK_OPENROUTER_API_KEY`; the optional
   `BRAVE_SEARCH_API_KEY` for Ask's native image tools; and
   `MEMOIR_OPENROUTER_API_KEY` and/or `MEMOIR_CLAUDE_API_KEY`. The prompt is a macOS dialog; on Linux a zenity
   dialog, or a hidden terminal prompt without a display. For the transcribe
   providers it asks for AssemblyAI first; an empty answer there moves on to
   Deepgram, and an empty answer for either optional provider just leaves it
   out. An empty answer for `OPENROUTER_TRANSCRIBE_API_KEY` leaves verbatim
   unconfigured. For explain, OpenRouter is asked first; an empty answer there
   moves on to Claude. For ask, Claude is asked first; an empty answer there
   moves on to OpenRouter. Each Claude or OpenRouter ask key gives one
   web-enabled (search + fetch) tray choice. If only some of a route's
   keys are present, the missing ones can be added by hand to add that
   provider. For Memoir, OpenRouter (GPT-6.1 Terra) is asked first; an empty
   answer there moves on to Claude, and an empty answer for both leaves Memoir
   unconfigured. With only `MEMOIR_CLAUDE_API_KEY` set, it offers
   `MEMOIR_OPENROUTER_API_KEY` as a second choice (empty skips it). It stops if `PARAKEET_URL` is still there, if the retired
   `GROQ_API_KEY` or `GROQ_MODEL` is there, or if the retired
   `MAI_TRANSCRIBE_API_KEY`, `AZURE_SPEECH_KEY` or
   `AZURE_SPEECH_ENDPOINT` is there. It also checks that `ffmpeg` is on
   `PATH`, because both live transcription paths decode MediaRecorder WebM/MP4
   to PCM.
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
| `ASSEMBLYAI_API_KEY` | (empty) | Secret. AssemblyAI key for Universal-3.6 Pro. Set this and/or `DEEPGRAM_API_KEY`; at least one is required. The streaming path sends the recording to `sync.assemblyai.com/v1/transcribe/live`, with `ffmpeg` decoding MediaRecorder WebM/MP4 to 16 kHz mono PCM. |
| `DEEPGRAM_API_KEY` | (empty) | Secret. Deepgram key for Nova-3 live transcription. Set this and/or `ASSEMBLYAI_API_KEY`; at least one is required. The stream goes to `wss://api.deepgram.com/v1/listen`; `ffmpeg` decodes MediaRecorder WebM/MP4 to 16 kHz mono PCM. |
| `OPENROUTER_TRANSCRIBE_API_KEY` | (empty) | Secret. OpenRouter key for MAI-Transcribe-2 on `/api/transcribe-verbatim` only. It is separate from `OPENROUTER_API_KEY` (that one is `/api/explain`'s), so each route keeps its own key. |
| `OPENROUTER_API_KEY` | (empty) | Secret. OpenRouter key for `/api/explain`. It adds one explain choice: GPT-6.2 Sol (OpenAI fast tier). Set this and/or `CLAUDE_API_KEY`; the tray picker chooses. |
| `CLAUDE_API_KEY` | (empty) | Secret. Claude Platform (`api.anthropic.com`) key for `/api/explain`. Set this and/or `OPENROUTER_API_KEY`; with both, the tray picker chooses. |
| `ASK_CLAUDE_API_KEY` | (empty) | Secret. Claude Platform (`api.anthropic.com`) key for `/api/ask`. It adds the Claude search + fetch choice to the ask tray. Set this and/or `ASK_OPENROUTER_API_KEY`; any additional key adds its own choice(s). Separate from `CLAUDE_API_KEY` (that one is `/api/explain`'s), though it may hold the same key. |
| `ASK_OPENROUTER_API_KEY` | (empty) | Secret. OpenRouter key for `/api/ask`'s GPT-6.2 Sol provider. It adds the OpenRouter search + fetch choice to the ask tray. Set this and/or `ASK_CLAUDE_API_KEY`; any additional key adds its own choice(s). Separate from `OPENROUTER_API_KEY` (that one is `/api/explain`'s), though it may hold the same key. |
| `BRAVE_SEARCH_API_KEY` | (empty) | Optional secret. Brave Search API key for `/api/ask`'s native `search_images` and `show_images` tools. When set, every Ask choice can search for and display images. Empty leaves Ask exactly as before. The image endpoint is queried with `count=10`, `country=ALL`, `search_lang=en`, `safesearch=strict` and `spellcheck=false`. |
| `MEMOIR_CLAUDE_API_KEY` | (empty) | Secret. Claude Platform key for `/api/memoir/check`. Set this and/or `MEMOIR_OPENROUTER_API_KEY`; with both, the tray picker chooses. A separate variable from `CLAUDE_API_KEY` (that one is `/api/explain`'s), and it may hold the same key. |
| `MEMOIR_OPENROUTER_API_KEY` | (empty) | Secret. OpenRouter key for `/api/memoir/check`'s GPT-6.1 Terra choice (OpenAI flex tier). Set this and/or `MEMOIR_CLAUDE_API_KEY`; with both, the tray picker chooses. A separate variable from `OPENROUTER_API_KEY` (that one is `/api/explain`'s), and it may hold the same key. |
| `MEMOIR_MODEL` | `claude-opus-5-5` | Model id for `/api/memoir/check`'s Claude choice. It must start with `claude-`. The OpenRouter choice's model is fixed. |
| `LISTEN_ADDR` | `127.0.0.1:8770` | Keep it. The clients and the health check expect this address. |

Models and thinking depth are fixed in `internal/config`, not environment
settings: Claude uses `output_config.effort` `medium`; OpenRouter uses
`reasoning.effort` `low` for GPT-6.2 Sol on
`/api/explain`, `reasoning.effort` `medium` for GPT-6.2 Sol on `/api/ask`, and
`reasoning.effort` `low` for GPT-6.1 Terra on `/api/memoir/check`. A Claude or
OpenRouter Ask key creates one tray choice and always sends the provider's web
tools (Claude's basic direct search and fetch, max 2 of each with 30k tokens
per fetch; OpenRouter Exa, up to 10 results and two searches per request, plus
two fetches with 30k tokens each). There are no `*_EFFORT`,
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
universal-3-6-pro`. AssemblyAI retired `universal-3-5-pro` on the Sync API and
routes it to 3.6; the response does not name the model that served it, so the
header names 3.6 directly. AssemblyAI-STT-3.7 (November 2026) is not on the
Sync API yet. comart's desktop app sends its MediaRecorder WebM/MP4 stream to
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
could be aligned. AssemblyAI says the alignment has "an additional latency
cost" and gives no number. One 8 s clip measured before Universal-3.6 Pro went
from 598 ms to 504 ms without timestamps (`b63fe17`). But the service log for
2026-11-14 to 11-21 shows no measurable difference: medians with and without
timestamps are within about 20 ms of each other, in both directions
(comart's `docs/word-timestamps.md` has the table). A caller that
only wants the text can send `?timestamps=0` (or exactly `false`): the config
part then says `"timestamps":false` and the answer carries neither field.
comart sends no query, so it keeps its timings. No client sends the
query today; the option stays because it is cheap to keep, and that page
discusses it for the takes that play no clip (the white and violet Stops).

comart's desktop app streams straight to this route as one chunked HTTP/1.1
`POST` per take. A page cannot: Chromium refuses a streaming `fetch` upload
over HTTP/1.1. So the desktop app's recorder window hands each MediaRecorder
chunk to its Electron main process over IPC, and main streams them with Node's
`http` module. There is no WebSocket transport: `GET /api/transcribe/ws`, which
forwarded a page's frames here, was removed on 2026-11-22.

Measured on this host with a warm connection on 2026-10-26, on Universal-3.5
Pro and before timestamps were on, the post-stop wait was ~0.35 s for a 6 s clip
and ~0.9 s for a 30 s clip. The service log for 2026-11-12 to 11-19 (253
requests, timestamps on) shows a median of ~0.24 s up to 10 s and ~0.28 s for
20–120 s clips. Transcripts were identical in 24 paired
runs on one clip. Live audio is capped at 120 s by AssemblyAI; comart's
microphone stops at 119 s so the 200ms cap poll and encoder chunking cannot
push a clip over the provider's hard edge. The live request has a 300 s deadline, but the
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

Measured end to end on 2026-10-27 through comart’s transport at the time, the
WebSocket route `GET /api/transcribe/ws` (removed on 2026-11-22), with real
WebM/Opus chunks fed at 1× real time. The timer starts when the client sends
the text frame `stop` and stops when the final JSON transcript arrives. Deepgram is the median of six runs per clip;
AssemblyAI is the median of three. Ranges are in parentheses. The AssemblyAI
column is Universal-3.5 Pro, before the move to 3.6.

| Clip | AssemblyAI (3.5 Pro) | Deepgram Nova-3 |
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

Every `/api/memoir/check` request logs the same way, with the number of
issues it returned:

```text
memoir[openrouter]: model=openai/gpt-6.1-terra status=200 outcome=ok issues=7 duration=6.1s
```

Tray switches are logged separately as `transcribe provider: … (tray)`,
`explain provider: … (tray)`, `ask provider: … (tray)` and
`memoir provider: … (tray)`.

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
missing key(s), until one is set. `/api/transcribe/stream` is the exception: without either
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
- `MEMOIR_CLAUDE_API_KEY` is set and `MEMOIR_MODEL` is not a `claude-…` id.

Why: with multiple possible providers it is easy to believe one model answers while
another does. Every start logs the routes it chose, for example
`transcribe route: AssemblyAI live, model universal-3-6-pro`,
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
appears when `/api/transcribe/stream`, `/api/explain`, `/api/ask` or
`/api/memoir/check` has at least two entries:

- **Transcribe with** — AssemblyAI Universal-3.6 Pro and Deepgram Nova-3.
- **Explain with** — Claude Platform Opus 5.5 (thinking medium) and
  OpenRouter GPT-6.2 Sol (fast, thinking low).
- **Ask with** — Claude Platform Opus 5.5 (search + fetch, thinking medium)
  and OpenRouter GPT-6.2 Sol (search + fetch, thinking medium).
- **Memoir with** — Claude Platform (thinking medium; Opus 5.5 unless
  `MEMOIR_MODEL` names another) and OpenRouter GPT-6.1 Terra (flex, thinking
  low).

Every LLM entry names its thinking effort, read from `internal/config`; the
transcription models have none.

The Ask and Memoir pickers appear only when both of that route's keys are set. With one choice on
every route there is no icon. The
tray lives in the macOS menu bar or the Linux status area
(GNOME's AppIndicator extension, KDE, and anything else implementing
StatusNotifierItem). Adding another provider brings its section back without
any client change, because comart keeps calling the same routes.

- Each route starts on its first configured provider and remembers a pick in
  the SQLite cache (`data/cache.db`, keys `transcribe:provider`,
  `explain:provider`, `ask:provider` and `memoir:provider`), so a restart
  keeps it.
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
  that slot, and another extension's own button takes index 0 unless it names
  a position. The optional extension in `contrib/gnome-shell/` keeps this icon
  at index 0, the leftmost icon of the top bar's right-hand box. It is
  installed and enabled on its own, not by the service's
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
- `audio/*` (or `video/*`) on `/api/transcribe/stream`;
- `multipart/form-data` on `/api/transcribe-verbatim`.

A web page can send a cross-site POST without a CORS preflight only as
`text/plain` or a form type, and a sandboxed iframe (`Origin: null`) passes the
guard. The Content-Type rule closes that gap.

`/api/screenshot` can only ask the desktop (the portal on Linux,
`screencapture` on macOS) to open its interactive selection; it cannot read screen pixels without the person at the machine
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
  route and spend the OpenRouter, AssemblyAI, Deepgram and Claude quota.
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
| `/api/explain` | OpenRouter, if `OPENROUTER_API_KEY` is set | `openai/gpt-6.2-sol` (OpenAI fast tier, standard tier as fallback) | effort `low` |
| `/api/ask` | Claude Platform, if `ASK_CLAUDE_API_KEY` is set | `claude-opus-5-5` | effort `medium`; basic web search + fetch, direct, max 2 each |
| `/api/ask` | OpenRouter, if `ASK_OPENROUTER_API_KEY` is set | `openai/gpt-6.2-sol` (OpenAI standard tier only) | effort `medium`; web search + fetch on (Exa) |
| `/api/memoir/check` | Claude Platform, if `MEMOIR_CLAUDE_API_KEY` is set | `claude-opus-5-5` | effort `medium` |
| `/api/memoir/check` | OpenRouter, if `MEMOIR_OPENROUTER_API_KEY` is set | `openai/gpt-6.1-terra` (OpenAI flex tier, standard tier as fallback) | effort `low` |
| `/api/transcribe/stream` | AssemblyAI, if `ASSEMBLYAI_API_KEY` is set | `universal-3-6-pro` (`/v1/transcribe/live`; WebM decoded to PCM by `ffmpeg`) | — |
| `/api/transcribe/stream` | Deepgram, if `DEEPGRAM_API_KEY` is set | `nova-3` (`wss://api.deepgram.com/v1/listen`; monolingual `language=en`) | — |
| `/api/transcribe-verbatim` | OpenRouter (`OPENROUTER_TRANSCRIBE_API_KEY`) | `microsoft/mai-transcribe-2` (verbatim, word timestamps) | — |

Each explain key adds one choice, so the explain tray section appears when
both `CLAUDE_API_KEY` and `OPENROUTER_API_KEY` are set. GPT-6.2 Sol keeps the
`openrouter` id that GPT-6.1 Sol and Gemini 3.8 Flash used before it, so a
saved choice of either moves to GPT-6.2 Sol; a choice saved for Kimi K3.1 Pro
(removed 2026-11-18) moves there too. The Ask keys and tray section are
independent of explain's; its Claude and OpenRouter keys each add one
search-enabled (search + fetch) choice.

OpenRouter speaks OpenAI's streaming chat completions, so one client
(`internal/llm`'s `Client`) serves explain and Memoir. Ask's OpenRouter choice
uses OpenRouter's Responses API (`/api/v1/responses`, `internal/llm`'s
`Responses`) instead, because only its stream says when a server-side search
or fetch starts and ends (`response.output_item.added`/`.done` with item type
`openrouter:web_search` or `openrouter:web_fetch`), which the card shows as
"Searching the web…". Chat completions only sends `: OPENROUTER PROCESSING`
keep-alive comments while a tool runs, and it sends the same comments during a
long silent reasoning pause (measured 2026-11-22), so they cannot tell a search
from thinking. The request fields for the token budget
and reasoning are fixed for every route in `internal/config`:
OpenRouter explain takes `max_tokens`, `reasoning: {effort, exclude}` and a
`provider` list with `allow_fallbacks: false`, so a request never leaves the
listed endpoints (`openai/fast`, then `openai`);
OpenRouter ask (Responses API) takes `max_output_tokens`,
`reasoning: {effort: "medium"}` (the Responses API ignores `exclude` and always
returns reasoning items; the client drops them) and the provider list `openai`
alone with `allow_fallbacks: false`, and also adds the `openrouter:web_search`
and `openrouter:web_fetch` server tools. The API is stateless, so a client-tool
round (`search_images`, `show_images`) resends the user message, every output
item so far (reasoning items carry the server tools' results) and one
`function_call_output` per call.
OpenRouter Memoir takes `max_tokens`, `reasoning: {effort: "low", exclude}` and
the provider list `openai/flex`, then `openai`, with `allow_fallbacks: false`.

**Prompt caching on explain.** The explain prompt puts every fixed byte first
(`part1.txt` + `part2.txt`, about 9,000 tokens of rules) and everything that
changes after it: the chosen items, the passage, earlier replies,
pronunciations, and the closing `task.txt`. A provider reuses the processed
form of an identical prefix for several minutes (at least 12 in tests), so after
the first click only the short request part is billed in full. Sol reuses the
prefix only when it is marked (measured on GPT-6.1 Sol), so its client sends
the prompt as two text blocks with `cache_control: {type: "ephemeral"}` on the
rules block. Measured on 12 cases: GPT-6.1 Sol fell from about $0.026 to $0.0045 per
click on the standard tier
(the fast tier costs twice as much per token). Claude caches only with an explicit marker,
which its client does not send yet.

**Ask's native image tools.** When `BRAVE_SEARCH_API_KEY` is set, every Ask
choice carries `search_images` and `show_images` as client tools alongside the
provider's own web tools. The **main answer model** decides whether to call
them; the backend only executes. `search_images` returns candidate IDs and
metadata and never displays anything. `show_images` is the only tool that
sends a `ready` event, so the reader's gallery is exactly the model's
selection; `search_images` reports `searching`, `empty` or `error` without
displaying a picture. Claude's tool result can carry image blocks, so it receives the top
five 500px thumbnails and chooses visually; OpenRouter receives ten metadata
rows. Each Ask round is capped at four `search_images` calls. The
Claude loop has one provider quirk: if Claude calls a client tool and a
`web_search`/`web_fetch` server tool in the same turn, the server tool is
deferred, and the user message that carries the client `tool_result` must
contain only tool-result blocks (no text). `internal/llm/claude.go` sends that
shape; OpenRouter executes its server tools itself.

`/api/ask` also carries screenshots. Both Ask models are vision-capable:
Claude accepts image blocks, and OpenRouter lists
`openai/gpt-6.2-sol` as `text+image+file->text`. The clients therefore
send one user message with the prompt
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

**Why these models** (explain measured 2026-11-15 on 12 cases, three runs
each, blind-judged by two judges with a verifier): GPT-6.1 Sol at `low` wrote
the best explanations of every model fast enough for the route, about one
point (of 10) above Gemini 3.8 Flash with no verified major error; Kimi K3.1
Pro with thinking off matched Gemini 4 Hydrogen's quality at about 2.5 s per
answer. Grok 4.7 and Qwen 3.8 Max cannot turn thinking off and took 40–120 s;
DeepSeek V4.1 Flash 1017 beat Gemini 3.8 Flash only with thinking on (15–70 s).
The cache-friendly layout and its closing reminder scored the same as the old
layout for both models. On 2026-11-18 explain moved from GPT-6.1 Sol to
GPT-6.2 Sol (released 2026-11-17) on the same endpoints at the same per-token
prices; the benchmark above was not re-run for it. On the same day Ask's
OpenRouter choice moved from DeepSeek V4.1 Flash 1017 at `high` to GPT-6.2 Sol
at `medium` on OpenAI's standard tier, at the owner's choice; it was not
benchmarked, and Sol's list price per token is far higher ($2 / $10 per
million against $0.036 / $0.50).

**Kimi K3.1 Pro was removed from explain on 2026-11-18** after a new accuracy
benchmark, built without the earlier data: 61 passages with 105 picked items
(sense selection, phrasal verbs and idioms, word origins including famous
false ones, real and apparent ambiguity, part of speech and pronunciation,
rare and technical words), answer keys checked by a second pass, three runs
per model at the settings then in use, blind-judged for accuracy only with
every serious error re-checked by a second judge. GPT-6.2 Sol at `low` had no
confirmed serious error in 183 answers and 166 answers with no error at all;
Kimi K3.1 Pro with thinking off had a serious error in 29 of 183 (19 invented
or folk word origins, 8 wrong pronunciations, 3 wrong meanings, 1 wrong usage
note) and 51 clean answers. The set is hard on purpose, so Kimi's rate on
everyday words is lower than that; Kimi with thinking on was not tested. A
tray choice saved for Kimi moves to GPT-6.2 Sol
(`config.RetiredExplainProviders`).

**Memoir** (measured 2026-11-16, blind-judged against answer keys on 28
synthetic journal entries with 221 planted mistakes, plus traps: other
people's quotes, Vietnamese words, code, an injected instruction, clean
entries and correct-but-odd sentences): Claude Opus 5.5 caught 220, and
GPT-6.1 Terra at `low` on OpenAI's flex tier caught 218 with no wrong fix and
no change to protected text, for about a fifteenth of the cost (about $0.0025
per check against $0.04). GPT-6.1 Sol, Sonnet 5.5, Kimi K3.1 Pro, Gemini 4
Hydrogen, Haiku 5.5, GPT-6 Luna, Muse Ember and DeepSeek V4.1 Flash 1017 (4-bit
and official) caught fewer, made more wrong or needless edits, or cost more. Flex
answered every request in the test; Cairn and comart wait up to five minutes
for a check.

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
                    screenshot store, its Linux/GNOME D-Bus portal and macOS screencapture
internal/tray       the provider pickers for /api/transcribe/stream, /api/explain, /api/ask and /api/memoir/check (macOS menu bar, Linux status area)
internal/mw         Merriam-Webster pronunciation lookup
internal/prompt     the explain prompt (part1.txt, part2.txt, task.txt)
internal/memoir     Memoir prompt (check.txt) and result parsing
internal/llm        streaming model clients: chat completions and Responses (OpenRouter) and Claude
contrib/gnome-shell optional GNOME Shell extension that pins the tray icon at index 0
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
