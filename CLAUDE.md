# ai-service — project notes

One Go binary on `127.0.0.1:8770` (JSON and SSE). It serves three clients: the
comart Chrome extension (`~/comart`), Cairn's Memoir tab (`~/cairn`) and
daily-dictation (`~/daily-dictation`, `POST /api/transcribe-verbatim` only).
`README.md` is the full guide: install, service management, configuration,
security model and models.

In short, `config.Load` refuses to start when a route has more than one
possible provider: `/api/transcribe` has three (Groq, local Parakeet, Azure
MAI-Transcribe-2); `/api/explain` and `/api/memoir/check` have two each. Every
Memoir variable starts with `MEMOIR_` (plain `CLAUDE_API_KEY` is
`/api/explain`'s). The Azure MAI path needs `ffmpeg` on `PATH`:
`internal/server/audio.go` re-encodes every clip to mono 16 kHz WAV first.

## This is a BACKEND. The front ends live elsewhere.

- **There is no UI to fix here.** A complaint about how something looks, where
  a button sits or what a panel says belongs to a client: the comart extension
  (read `~/comart/CLAUDE.md` and `~/comart/NOTES.md` first), Cairn's Memoir tab
  (`~/cairn`, client code in `src/memoir.ts`) or daily-dictation
  (`~/daily-dictation`).
- **`GET /` is one line of `text/plain`**, on purpose. `install.sh`'s health
  check and comart's README use `http://127.0.0.1:8770/`, and a 404 at the
  root of a host that is plainly answering reads as a broken deploy. Keep it one
  line. Do not grow it into a status page.
- **Do not add a template, a static directory or an `embed` back.** A served
  copy of a client would have to be kept in step with the real one by hand.
- **Keep the comart client API stable.** comart uses its five routes with no
  CORS, no auth and no route of its own added since. Memoir's
  `/api/memoir/check` and daily-dictation's `/api/transcribe-verbatim` are the
  deliberate additions. Change a route only together with the client that
  calls it.

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
