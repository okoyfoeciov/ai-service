# llm-service

A small localhost **LLM backend**: one static Go binary on `127.0.0.1:8770`,
speaking JSON and SSE. It has two clients:

- the **comart Chrome extension** (`~/comart`): word explanations,
  pronunciations and transcription;
- **Cairn's Memoir tab** (`~/cairn`, `src/memoir.ts`): proofreading and
  paraphrasing of journal entries.

**There is no UI here.** Both clients carry their own pages. `GET /` answers
one line of plain text and nothing else. Do not add a template, a static
directory or an `embed` back: a served copy of a client would have to be kept
in step with the real one by hand.

The API is deliberately stable. The clients were built against it as it is: no
CORS, no auth, no extra routes. Change it only together with the client that
calls it.

## Routes

| Route | Client | What it does |
| --- | --- | --- |
| `GET /` | health check | One line of `text/plain`. Keep it one line. |
| `POST /api/explain` | comart | Explains the chosen words. SSE events: `pron-data`, `chunk`, `error`, `done` (`done` is always last). An error after the stream opens is an `error` event; the HTTP status stays 200. |
| `GET /api/pron?word=&pos=` | comart | Merriam-Webster pronunciation. 404 `not_found` when MW has none, 502 `mw_unavailable` when MW refuses. |
| `GET /api/credits` | comart | Always `{"remaining":null,"low":false}`. The server tracks no balance. comart calls it after each explanation. |
| `POST /api/transcribe` | comart | The body is one recorded clip (`audio/*`). Sends it to Groq Whisper and returns `{"text": …}`. |
| `POST /api/memoir/check` | Cairn | Proofreads an entry (`{"content": …}`, max 12000 characters). Plain JSON. |
| `POST /api/memoir/paraphrase` | Cairn | Other ways to say a selected span (`{"text": …}`, max 600 characters). Plain JSON. |

The Memoir routes do not stream: one truncated list would lose every
alternative, so the server collects the whole answer and replies once.

## Install, update, uninstall

macOS and Linux are supported.

| OS | Service | Definition file |
| --- | --- | --- |
| macOS | launchd LaunchAgent `com.llm-service.backend` | `~/Library/LaunchAgents/com.llm-service.backend.plist` |
| Linux | systemd user unit `llm-service.service` | `~/.config/systemd/user/llm-service.service` |

```sh
./install.sh             # install, or update after a code change
./install.sh --dry-run   # build nothing, prompt for nothing, start nothing; write the service definition to a temp dir and lint it
./uninstall.sh           # stop the service and remove its definition
```

`install.sh` is idempotent. Every run does all of this:

1. Checks the OS and that `go` is on `PATH` (on macOS, for example
   `brew install go`).
2. Builds `bin/llm-service` (`CGO_ENABLED=0`).
3. Creates `llm-service.env` from `.env.example` (mode 600) if it is missing.
   For each empty API key (`GROQ_API_KEY`, `OPENROUTER_API_KEY`,
   `CEREBRAS_API_KEY`) it asks in a hidden prompt (a macOS dialog; on Linux a
   zenity dialog, or a hidden terminal prompt without a display). It never
   prints a key. The keys are required here: if you cancel, enter nothing, or
   no prompt can be shown, `install.sh` stops before step 4 and does not
   (re)start the service. It also stops if a key contains a space, quote,
   backslash, `$` or backtick.
4. Writes the service definition, lints it (`plutil -lint` or
   `systemd-analyze --user verify`) and (re)starts the service.
5. Polls `http://127.0.0.1:8770/` for about 15 s, and checks that the process
   on port 8770 is the service's own. A server started by hand cannot pass.

The first run needs the network (Go modules).

`--dry-run` builds nothing, never opens `llm-service.env` and starts nothing. It
prints the temp dir it wrote the definition to; delete that dir afterwards.

`uninstall.sh` removes only the service definition. It keeps `bin/`, `data/`,
`logs/` and `llm-service.env`, so `./install.sh` brings the service back.

Both service managers run `run.sh` at login, restart it whenever it exits, and
append its output to `logs/llm-service.log`. The systemd unit has
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
systemctl --user status llm-service
systemctl --user restart llm-service
systemctl --user stop llm-service                # until you start it, re-run install.sh, or reboot
tail -f logs/llm-service.log                     # the server's own log
journalctl --user -u llm-service                 # systemd's start/stop/crash messages only
```

macOS:

```sh
U=gui/$(id -u)
curl -fsS http://127.0.0.1:8770/
launchctl print $U/com.llm-service.backend | grep -E 'state|pid|last exit'
launchctl kickstart -k $U/com.llm-service.backend     # restart
launchctl bootout $U/com.llm-service.backend          # stop, until the next login or install.sh
launchctl bootstrap $U ~/Library/LaunchAgents/com.llm-service.backend.plist   # start again
tail -f logs/llm-service.log
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

`run.sh` is what both service managers run. It loads `llm-service.env`,
defaults `CACHE_DB` to a path in this folder, and `exec`s
`bin/llm-service`. Use it rather than the bare binary: the binary does not read
`llm-service.env` itself, and without `run.sh` it assumes the folder is
`~/llm-service`. Start the service again when you are done.

## Configuration

`llm-service.env` is a **secret** file (mode 600). `install.sh` creates it from
`.env.example`, which documents every key:

| Key | Default | Meaning |
| --- | --- | --- |
| `GROQ_API_KEY` | (empty) | Secret. Groq key for `/api/transcribe`. |
| `GROQ_MODEL` | `whisper-large-v3-turbo` | Groq transcription model. |
| `OPENROUTER_API_KEY` | (empty) | Secret. OpenRouter key for `/api/explain`. |
| `EXPLAIN_MODEL` | `google/gemini-3.8-flash` | OpenRouter model for `/api/explain`. |
| `EXPLAIN_REASONING_TOKENS` | `1024` | Thinking budget for that model (OpenRouter's `reasoning.max_tokens`). |
| `CEREBRAS_API_KEY` | (empty) | Secret. Cerebras key for `/api/memoir/*`. |
| `MEMOIR_MODEL` | `qwen-3.8-27b` | Cerebras model for `/api/memoir/*`. |
| `MEMOIR_REASONING_EFFORT` | `medium` | `low`, `medium`, `high`, or `none` to turn reasoning off. |
| `LISTEN_ADDR` | `127.0.0.1:8770` | Keep it. The clients and the health check expect this address. |

`CACHE_DB` is normally not set: `run.sh` defaults it to `data/cache.db`, so the
folder can move without an edit. On Linux the unit may only write `data/` and
`logs/` (`ProtectHome=read-only`), so do not point it outside `data/`.

`run.sh` sources the file with bash (`set -a`). Keep to plain `KEY=value` lines
and put single quotes around any value with spaces, `$` or parentheses (inside
double quotes, bash still expands `$`). A line bash cannot run stops the
server at start; the service manager retries every 10 s and the error is in
`logs/llm-service.log`.

After an edit, restart the service. A missing API key is only a warning at
start: `/api/transcribe` then answers 500, `/api/explain` sends an `error`
event and `/api/memoir/*` answer 503, each naming the missing key, until it is
set.

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

- `application/json` on `/api/explain` and `/api/memoir/*`;
- `audio/*` (or `video/*`) on `/api/transcribe`.

A web page can send a cross-site POST without a CORS preflight only as
`text/plain` or a form type, and a sandboxed iframe (`Origin: null`) passes the
guard. The Content-Type rule closes that gap.

Request limits:

- `/api/explain`: 5 MiB JSON body (413 `Request too large.`).
- `/api/memoir/*`: 5 MiB JSON body (a larger one fails as 400
  `Invalid JSON.`), plus the character caps in the route table.
- `/api/transcribe`: 25 MiB audio (413 `recording too long`), Groq's own limit.

**Still not protected:**

- Any program running on this machine, under any local user, can call every
  route and spend the OpenRouter, Cerebras and Groq quota. There is no rate
  limit.
- A web page can still send blind GET requests (for example an `<img>` tag):
  no Origin header is sent, so the guard lets them through. The page cannot
  read the answer. The GET routes bill nothing; `/api/pron` can at most cause
  Merriam-Webster lookups.
- The API keys are in a plain file on disk, `llm-service.env`. It is not
  encrypted: any program that runs as you (or as root) can read and use them.
  The older zips in `backups/` hold secrets too; keep that folder private.

## Models

| Used by | Provider | Model | Reasoning |
| --- | --- | --- | --- |
| `/api/explain` | OpenRouter | `google/gemini-3.8-flash` | 1024-token thinking budget |
| `/api/memoir/*` | Cerebras | `qwen-3.8-27b` | `medium` |

Both providers speak OpenAI's streaming chat completions, so one client
(`internal/llm`) serves both. The only difference is the request fields for
the token budget and reasoning, which `server.New` sets per provider:
OpenRouter takes `max_tokens` and `reasoning: {max_tokens, exclude}`; Cerebras
takes `max_completion_tokens` and `reasoning_effort` (or `disable_reasoning`).

How a call works:

- The prompt goes out as one user message. Reasoning is never streamed back to
  a client: only answer text reaches it.
- A 429 or 5xx before any text arrives is retried twice (after 1 s and 2 s,
  or the provider's `Retry-After` if longer; one over 5 s fails at once).
  Other HTTP errors fail at once, with the provider's message.
- An answer is judged when the stream ends. An error inside the stream, a
  connection that breaks before `[DONE]`, a stop at the token limit and an
  empty answer are all errors, even if some text already went out. A break
  after the final `finish_reason` is not: the answer is already whole. So neither
  the panel nor Memoir's parser takes a partial answer for a whole one.

**Why these models** (measured 2026-09-29 on 12 cases per route, blind-judged):
on Memoir, Qwen at `medium` was as good as the previous, much larger model and
answered 3–5× faster (check: 1.6 s median). On explain, Gemini 3.8 Flash clearly beat Qwen
at every reasoning level: Qwen got pronunciations wrong. **Never run Memoir
with reasoning off:** Qwen then breaks the JSON contract, edits other people's
quoted words and leaves unlisted edits in `corrected`.

Cerebras allows 150k tokens a minute on this account. An explain prompt is
about 10k tokens and a Memoir prompt 2–4k, so only a burst of parallel
requests reaches it; the retry covers a short one.

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

Results are cached for 30 days in `data/cache.db` (SQLite, key `mw:<word>`).
`/api/explain` answers cached words at once, sends the misses to MW at most 4
at a time, and waits at most 10 s in total before it explains without the rest.

## Code layout

```
cmd/llm-service/    main: config, cache, server, graceful shutdown
internal/config     configuration from the environment
internal/store      SQLite cache
internal/server     HTTP routing, the security guard, the handlers
internal/mw         Merriam-Webster pronunciation lookup
internal/prompt     the explain prompt (part1.txt, part2.txt)
internal/memoir     Memoir prompts (check.txt, paraphrase.txt) and result parsing
internal/llm        the streaming chat-completions client (OpenRouter, Cerebras)
install.sh, uninstall.sh, run.sh
```

The prompt `.txt` files are product text. `TestPromptParity` pins the Memoir
prompts byte for byte, so a change there must be deliberate.

## Build and test

```sh
CGO_ENABLED=0 go build -o bin/llm-service ./cmd/llm-service
go vet ./...
go test ./...
```

The result is one static binary with no cgo. It needs only the SQLite file it
creates on first run. After a build, restart the service to run the new binary
(`systemctl --user restart llm-service`, or
`launchctl kickstart -k gui/$(id -u)/com.llm-service.backend`), or just run
`./install.sh`.

## Version control and secrets

The code is in git (GitHub repository `okoyfoeciov/llm-service`). Git is the undo for code.

`.gitignore` keeps out build output (`bin/`), runtime state (`data/`, `logs/`,
`backups/`) and every secret: `llm-service.env`, any `*.env` and `*.har`
network captures. **Never commit a secret**, and never force-add an ignored
file.

Old `backups/` zips may hold secrets, so keep `backups/` private. Deleting
them is the owner's call.
