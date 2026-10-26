# ai-service

A small localhost **LLM backend**: one static Go binary on `127.0.0.1:8770`,
speaking JSON and SSE. It has three clients:

- the **comart Chrome extension** (`~/comart`): word explanations,
  pronunciations, mic transcription and the mic card's writing check;
- **Cairn's Memoir tab** (`~/cairn`, `src/memoir.ts`): proofreading of
  journal entries;
- **daily-dictation** (`~/daily-dictation`): verbatim transcripts through
  `POST /api/transcribe-verbatim`.

**There is no UI here.** All three clients carry their own pages. `GET /`
answers one line of plain text and nothing else. Do not add a template, a
static directory or an `embed` back: a served copy of a client would have to be
kept in step with the real one by hand.

The comart API is deliberately stable: the extension was built against it as it
is, with no CORS, no auth and no route of its own added since. Memoir's
`/api/memoir/check` and daily-dictation's `/api/transcribe-verbatim` are the
deliberate additions. Change a route only together with the client that calls
it.

## Routes

| Route | Client | What it does |
| --- | --- | --- |
| `GET /` | health check | One line of `text/plain`. Keep it one line. |
| `POST /api/explain` | comart | Explains the chosen words. SSE events: `pron-data`, `chunk`, `error`, `done` (`done` is always last). An error after the stream opens is an `error` event; the HTTP status stays 200. |
| `GET /api/pron?word=&pos=` | comart | Merriam-Webster pronunciation. 404 `not_found` when MW has none, 502 `mw_unavailable` when MW refuses. |
| `GET /api/credits` | comart | Always `{"low":false,"remaining":null}`. The server tracks no balance. comart calls it after each explanation. |
| `POST /api/transcribe` | comart | The body is one recorded clip (`audio/*` or `video/*`). Sends it to Groq Whisper or OpenRouter's MAI-Transcribe-2 (whichever the tray picker has selected) and returns `{"text": …}`. |
| `POST /api/transcribe-verbatim` | daily-dictation | Multipart field `audio` (one segment file). Sends it to OpenRouter's MAI-Transcribe-2 (verbatim, word timestamps) and returns `{"lines": […], "words": […]}` with seconds timings. |
| `POST /api/memoir/check` | Cairn; comart | Proofreads an entry (`{"content": …}`, max 12000 UTF-16 code units). Plain JSON. |

The Memoir route does not stream: the client renders the finished result, so
the server collects the whole answer and replies once.

On a transcription error the response carries `openRouterStatus`; the verbatim
route also keeps the historical `azureStatus` field with the same value because
daily-dictation already classifies permanent vs retryable failures from it.

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
   at least one of `OPENROUTER_TRANSCRIBE_API_KEY` and `GROQ_API_KEY`;
   one of `OPENROUTER_API_KEY` or `CLAUDE_API_KEY`; and one of
   `MEMOIR_CLAUDE_API_KEY` or `MEMOIR_CEREBRAS_API_KEY`. The prompt is a macOS
   dialog; on Linux a zenity dialog, or a hidden terminal prompt without a
   display. For the transcribe provider it asks for
   `OPENROUTER_TRANSCRIBE_API_KEY` first, then `GROQ_API_KEY`; set both by hand
   to get the tray picker. For the explain key it asks for OpenRouter first; an
   empty answer there moves on to Claude. For the Memoir key it asks for Claude
   first; an empty answer there moves on to Cerebras. It stops if more than one
   variable of the explain or Memoir route is already set, if `PARAKEET_URL` is
   still there, if the retired `MAI_TRANSCRIBE_API_KEY`, `AZURE_SPEECH_KEY` or
   `AZURE_SPEECH_ENDPOINT` is there, or if the retired `CEREBRAS_API_KEY` is
   still there. If `OPENROUTER_TRANSCRIBE_API_KEY` is set (alone or with Groq),
   it also checks that `ffmpeg` is on `PATH` (the server converts comart's WebM
   recordings with it).
   It never prints a key. The keys are required here: if you cancel, enter nothing, or
   no prompt can be shown, `install.sh` stops before step 4 and does not
   (re)start the service. It also stops if a key contains a space, quote,
   backslash, `$` or backtick.
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
| `GROQ_API_KEY` | (empty) | Secret. Groq key for `/api/transcribe`. Set this, `OPENROUTER_TRANSCRIBE_API_KEY`, or both. |
| `GROQ_MODEL` | `whisper-large-v3-turbo` | Groq transcription model. |
| `OPENROUTER_TRANSCRIBE_API_KEY` | (empty) | Secret. OpenRouter key for MAI-Transcribe-2 on **both** `/api/transcribe` and `/api/transcribe-verbatim`. Set this, `GROQ_API_KEY`, or both. It is separate from `OPENROUTER_API_KEY` (that one is `/api/explain`'s), so the explain one-key rule stays intact and the two routes may use different keys. Requires `ffmpeg` on `PATH`: see below. |
| `OPENROUTER_API_KEY` | (empty) | Secret. OpenRouter key for `/api/explain` only. Set this **or** `CLAUDE_API_KEY`, never both. |
| `CLAUDE_API_KEY` | (empty) | Secret. Claude Platform (`api.anthropic.com`) key for `/api/explain`. Set this **or** `OPENROUTER_API_KEY`, never both. |
| `EXPLAIN_MODEL` | `google/gemini-3.8-flash` on OpenRouter, `claude-opus-5-5` on Claude | Model id for `/api/explain`, for whichever provider the key selects. With a Claude key it must start with `claude-`. |
| `EXPLAIN_REASONING_TOKENS` | `1024` | OpenRouter only: thinking budget (`reasoning.max_tokens`). Claude ignores it. |
| `EXPLAIN_EFFORT` | `medium` | Claude only: `low`, `medium`, `high`, `xhigh` or `max` (`output_config.effort`). Claude models take no separate thinking-token budget, so this is the thinking control (`max_tokens` still caps the answer). OpenRouter ignores it. |
| `MEMOIR_CLAUDE_API_KEY` | (empty) | Secret. Claude Platform key for `/api/memoir/check`. Set this **or** `MEMOIR_CEREBRAS_API_KEY`, never both. A separate variable from `CLAUDE_API_KEY` (that one is `/api/explain`'s), and it may hold the same key. |
| `MEMOIR_CEREBRAS_API_KEY` | (empty) | Secret. Cerebras key for `/api/memoir/check`. Set this **or** `MEMOIR_CLAUDE_API_KEY`, never both. Called `CEREBRAS_API_KEY` before 2026-10-20: the old name makes the service refuse to start. |
| `MEMOIR_MODEL` | `claude-opus-5-5` on Claude, `qwen-3.8-27b` on Cerebras | Model id for `/api/memoir/check`, for whichever provider the key selects. With a Claude key it must start with `claude-`. |
| `MEMOIR_EFFORT` | `medium` | Claude only: `low`, `medium`, `high`, `xhigh` or `max` (`output_config.effort`). Cerebras ignores it. |
| `MEMOIR_REASONING_EFFORT` | `medium` | Cerebras only: `low`, `medium`, `high`, or `none` to turn reasoning off. Claude ignores it. |
| `LISTEN_ADDR` | `127.0.0.1:8770` | Keep it. The clients and the health check expect this address. |

**OpenRouter transcribe formats.** OpenRouter's STT endpoint takes base64 JSON,
not the multipart upload the old Azure path used. MAI-Transcribe-2 is served by
Azure, so OpenRouter caps uploads at **25 MB** per request. The `audio/webm`
that comart's `MediaRecorder` produces is not worth trusting across providers,
so `/api/transcribe` converts every clip to mono 16 kHz WAV with `ffmpeg` first.
`install.sh` checks for `ffmpeg` when `OPENROUTER_TRANSCRIBE_API_KEY` is set
(Debian/Ubuntu: `sudo apt install ffmpeg`; macOS: `brew install ffmpeg`).
`/api/transcribe-verbatim` sends daily-dictation's file as-is in WAV, MP3, FLAC,
M4A, OGG, WebM or AAC format; its 25 MB cap is about 26 minutes of 128 kbps MP3.
OpenRouter also documents a 60-second upstream processing deadline. MAI is
roughly 300× real-time, so the 50–100 s clips daily-dictation and comart send
finish far below it; chunk anything much longer.

`CACHE_DB` is normally not set: `run.sh` defaults it to `data/cache.db`, so the
folder can move without an edit. On Linux the unit is sandboxed with
`ProtectSystem=strict` and `ProtectHome=read-only`, with only `data/` and
`logs/` writable, so do not point it outside `data/`.

`run.sh` sources the file with bash (`set -a`). Keep to plain `KEY=value` lines
and put single quotes around any value with spaces, `$` or parentheses (inside
double quotes, bash still expands `$`). A line bash cannot run stops the
server at start; the service manager retries every 10 s and the error is in
`logs/ai-service.log`.

After an edit, restart the service. A missing API key is only a warning at
start: `/api/transcribe-verbatim` answers 500, `/api/explain` sends an `error`
event and `/api/memoir/check` answers 503, each naming the missing key, until it
is set. `/api/transcribe` is the exception: with neither of its keys the service
refuses to start.

**The service refuses to start** (it logs `config: …` and exits 1; the service
manager keeps retrying every 10 s) when a route has no usable provider:

- neither `GROQ_API_KEY` nor `OPENROUTER_TRANSCRIBE_API_KEY` is set;
- the retired `PARAKEET_URL` is still set (the local Parakeet integration was
  removed);
- the retired `MAI_TRANSCRIBE_API_KEY`, `AZURE_SPEECH_KEY` or
  `AZURE_SPEECH_ENDPOINT` is still set. MAI-Transcribe-2 now goes through
  OpenRouter, which both transcription routes read from
  `OPENROUTER_TRANSCRIBE_API_KEY`;
- both `OPENROUTER_API_KEY` and `CLAUDE_API_KEY` are set. An empty value counts
  as unset, so leave the unused key as an empty `KEY=` line (that also beats
  a copy of the variable in your login environment, because `run.sh` sources the
  file over it);
- `CLAUDE_API_KEY` is set and `EXPLAIN_MODEL` is not a `claude-…` id (for
  example a leftover `google/gemini-3.8-flash`), or `EXPLAIN_EFFORT` is not one
  of the five levels;
- both `MEMOIR_CEREBRAS_API_KEY` and `MEMOIR_CLAUDE_API_KEY` are set (same rule:
  leave the unused one empty or commented out);
- `MEMOIR_CLAUDE_API_KEY` is set and `MEMOIR_MODEL` is not a `claude-…` id (for
  example a leftover `qwen-3.8-27b`), or `MEMOIR_EFFORT` is not one of the five
  levels;
- the retired `CEREBRAS_API_KEY` is still set. It is `MEMOIR_CEREBRAS_API_KEY`
  now; the old name would otherwise sit in the file doing nothing.

Why: with multiple possible providers it is easy to believe one model answers while
another does. Every start logs the route it chose, for example
`transcribe route: OpenRouter, model microsoft/mai-transcribe-2`,
`explain route: Claude Platform, model claude-opus-5-5, effort medium` and the
same for `memoir route: …`, and every Claude request whose stream starts logs
the model it asked for next to the model the API says answered (`llm[Claude/explain]`
or `llm[Claude/memoir]`). The Anthropic SDK's own `ANTHROPIC_*` environment variables are
switched off, so a stray `ANTHROPIC_BASE_URL` or `ANTHROPIC_AUTH_TOKEN` cannot
re-route the call.

### Tray picker (both transcribe providers)

Set both `GROQ_API_KEY` and `OPENROUTER_TRANSCRIBE_API_KEY` and the service
shows a tray icon: the macOS menu bar or the Linux status area (GNOME's
AppIndicator extension, KDE, and anything else implementing StatusNotifierItem).
Clicking it opens two check items, **Groq Whisper** and
**MAI-Transcribe-2 (OpenRouter)**; the checked one answers `/api/transcribe`.
There is no tray icon with only one key, and the choice does not touch any
client: comart keeps calling the same route.

- The service starts on Groq and remembers a pick in the SQLite cache
  (`data/cache.db`, key `transcribe:provider`), so a restart keeps it.
- If a saved pick's key is later removed, the service falls back to the first
  configured provider instead of routing to a service with no credentials.
- Linux needs the service to have the session D-Bus address; the systemd user
  manager imports it at login. Without a desktop session the service still
  serves HTTP, logs `tray: no session D-Bus …`, and skips the icon.
- macOS builds the tray with cgo/Cocoa, so `install.sh` needs the Xcode
  command-line tools (`xcode-select --install`). Linux uses a small in-repo
  StatusNotifierItem/dbusmenu backend (`internal/tray/tray_linux.go`) and keeps
  the static `CGO_ENABLED=0` build. It deliberately omits the optional
  `Activate` method: GNOME's AppIndicator extension waits out the double-click
  interval before opening a menu for items that advertise it, which is what
  made fyne's Linux backend feel about a second slow. Without it, one click
  opens the picker.
- The tray is part of the service process, not an HTTP route. It writes only the
  cache row above; no new listening socket, no extra file.

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

- `application/json` on `/api/explain` and `/api/memoir/check`;
- `audio/*` (or `video/*`) on `/api/transcribe`;
- `multipart/form-data` on `/api/transcribe-verbatim`.

A web page can send a cross-site POST without a CORS preflight only as
`text/plain` or a form type, and a sandboxed iframe (`Origin: null`) passes the
guard. The Content-Type rule closes that gap.

Request limits:

- `/api/explain`: 5 MiB JSON body (413 `Request too large.`).
- `/api/memoir/check`: 5 MiB JSON body (a larger one fails as 400
  `Invalid JSON.`), plus the character caps in the route table.
- `/api/transcribe`: 25 MiB audio (413 `recording too long`). 25 MiB is
  Groq's limit, applied to every transcribe provider.
- `/api/transcribe-verbatim`: 25 MiB audio (413 `audio too long`). This is
  OpenRouter's cap for the Azure-backed MAI-Transcribe-2 endpoint; base64 adds
  about a third on the wire.

**Still not protected:**

- Any program running on this machine, under any local user, can call every
  route and spend the OpenRouter, Claude, Cerebras and Groq quota.
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
| `/api/memoir/check` | Claude Platform, if `MEMOIR_CLAUDE_API_KEY` is set | `claude-opus-5-5` | effort `medium` |
| `/api/memoir/check` | Cerebras, if `MEMOIR_CEREBRAS_API_KEY` is set | `qwen-3.8-27b` | `medium` |
| `/api/transcribe` | OpenRouter, if `OPENROUTER_TRANSCRIBE_API_KEY` is set | `microsoft/mai-transcribe-2` (every clip converted to WAV by `ffmpeg`) | — |
| `/api/transcribe` | Groq, if `GROQ_API_KEY` is set (the starting default when both are configured) | `whisper-large-v3-turbo` | — |
| `/api/transcribe-verbatim` | OpenRouter (`OPENROUTER_TRANSCRIBE_API_KEY`) | `microsoft/mai-transcribe-2` (verbatim, word timestamps) | — |

OpenRouter and Cerebras speak OpenAI's streaming chat completions, so one client
(`internal/llm`'s `Client`) serves both. The only difference is the request fields for
the token budget and reasoning, which `server.New` sets per provider:
OpenRouter takes `max_tokens` and `reasoning: {max_tokens, exclude}`; Cerebras
takes `max_completion_tokens` and `reasoning_effort` (or `disable_reasoning`).

Claude is called directly on the Claude Platform with the official Go SDK, for
whichever of the two routes (explain, Memoir) has a Claude key
(`internal/llm/claude.go`), through the Messages API. The request carries the
model, `max_tokens`, adaptive thinking (`thinking: {type: "adaptive"}`,
requested explicitly because omitting it means no thinking on some older
models), `output_config.effort`, the prompt and the SDK's stream flag, and
nothing else. Thinking text is dropped, never streamed. Effort is the only
thinking-depth control: Opus 5.5
rejects a separate thinking-token budget, while `max_tokens` still caps the
answer. No refusal fallback is enabled: it would answer from another model
after a refusal, which is the silent re-routing the one-key rule exists to
prevent. A refusal reaches the client as an `error` event on explain, and as a
502 on Memoir. Retries,
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
internal/server     HTTP routing, the security guard, the handlers
internal/tray       the /api/transcribe provider picker (macOS menu bar, Linux status area)
internal/mw         Merriam-Webster pronunciation lookup
internal/prompt     the explain prompt (part1.txt, part2.txt)
internal/memoir     Memoir prompt (check.txt) and result parsing
internal/llm        streaming model clients: chat completions (OpenRouter, Cerebras) and Claude
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
OpenRouter MAI mic path needs `ffmpeg` on `PATH`. After a build, restart the service to
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
