# ai-service — project notes

One Go binary on `127.0.0.1:8770` (JSON and SSE). It serves three clients: the
comart Chrome extension (`~/comart`), Cairn's Memoir tab (`~/cairn`), and
daily-dictation (`~/daily-dictation`, `POST /api/transcribe-verbatim` only).
`README.md` is the full guide: install, service management, configuration,
security model and models. `/api/explain` runs on OpenRouter
(`google/gemini-3.8-flash`) or, when `CLAUDE_API_KEY` is set instead, on the
Claude Platform (`claude-opus-5-5`, effort `medium`); `config.Load` refuses to
start with both keys set. `/api/memoir/*` runs the same way: on Claude
(`claude-opus-5-5`, effort `medium`) when `MEMOIR_CLAUDE_API_KEY` is set, or on
Cerebras (`qwen-3.8-27b`) when `MEMOIR_CEREBRAS_API_KEY` is. Every Memoir
variable starts with `MEMOIR_`, and the same refusal applies to both keys.
`/api/transcribe` (comart's Exscriptor mic recorder) runs on Groq
(`GROQ_API_KEY`), local Parakeet (`PARAKEET_URL`, `~/parakeet-ov` on
`127.0.0.1:8790`), or Azure Speech (`MAI_TRANSCRIBE_API_KEY`,
`MAI-Transcribe-2`); `config.Load` refuses to start with more than one of the
three set. The Azure route needs `ffmpeg` on `PATH`, because comart records
WebM and Azure only decodes WAV/MP3/OGG/FLAC: `internal/server/audio.go`
re-encodes every clip to mono 16 kHz WAV first. `/api/transcribe-verbatim` (daily-dictation's import pipeline) is a
separate route on Azure Speech (`MAI-Transcribe-2`, `AZURE_SPEECH_KEY`).

## This is a BACKEND. The front ends live elsewhere.

- **There is no UI to fix here.** A complaint about how something looks, where
  a button sits or what a panel says belongs to a client: the comart extension
  (read `~/comart/CLAUDE.md` and `~/comart/NOTES.md` first) or Cairn's Memoir
  tab (`~/cairn`, client code in `src/memoir.ts`).
- **comart's docs are out of date about this backend.** They still call it
  `back-end/` (`~/comart/back-end`). That folder is gone: it became this
  directory on 2026-09-17, so their `back-end/CLAUDE.md` means this file.
  Their `com.comart.backend`, `back-end/logs/comart.log`,
  `deploy/r2_backup.py` and "macOS only" are old too (`deploy/` is gone).
  The current names are
  `com.ai-service.backend` (macOS) or `ai-service.service` (Linux) and
  `logs/ai-service.log`.
- **`GET /` is one line of `text/plain`**, on purpose. `install.sh`'s health
  check and comart's README use `http://127.0.0.1:8770/`, and a 404 at the
  root of a host that is plainly answering reads as a broken deploy. Keep it one
  line. Do not grow it into a status page.
- **Do not add a template, a static directory or an `embed` back.** A served
  copy of a client would have to be kept in step with the real one by hand.
- **The client API stays as it is.** The comart extension was built against it
  with no server change: no CORS, no auth, no new route. Keep that. Memoir's
  one route (`/api/memoir/check`) and daily-dictation's one
  (`/api/transcribe-verbatim`) are the deliberate additions. Change a route
  only together with the client that calls it.

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
- `internal/prompt/*.txt` and `internal/memoir/*.txt` are product prompts.
  `TestPromptParity` pins the Memoir prompt by hash. Do not edit it in passing.

## Version control and secrets

- **Git.** GitHub repo `okoyfoeciov/ai-service`. Git is the undo for code.
- **Secrets never go into git, and never into the chat.** Do not read or print
  `ai-service.env` (it holds the API keys), `*.har` captures, `data/` or
  `backups/`. `.gitignore` keeps them out; never force-add an ignored file.
  Old `backups/` zips may hold secrets.
