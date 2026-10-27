#!/usr/bin/env bash
# install.sh — build ai-service and keep it running on this machine.
#
# ai-service is the localhost LLM backend (127.0.0.1:8770) for the comart
# Chrome extension, Cairn's Memoir tab and daily-dictation. Nothing fronts the
# server, so loopback is the trust boundary.
#
# Supported hosts, and the service each one gets:
#   macOS  launchd LaunchAgent  ~/Library/LaunchAgents/com.ai-service.backend.plist
#   Linux  systemd user unit    ~/.config/systemd/user/ai-service.service
# Either way the service runs run.sh at login, restarts it whenever it exits,
# and appends its output to logs/ai-service.log.
#
# Idempotent. Re-run it after changing the code or after moving this folder
# (the service definition carries absolute paths). A real run rebuilds, checks
# the API keys, rewrites the service definition and restarts the service.
#
#   ./install.sh            do it
#   ./install.sh --dry-run  build nothing, prompt for nothing, start nothing:
#                           write the service definition to a temp dir and lint it
#
# Undo with ./uninstall.sh.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVFILE="$HERE/ai-service.env"
LABEL="com.ai-service.backend"   # macOS LaunchAgent label
UNIT="ai-service.service"        # Linux systemd user unit
HEALTH_URL="http://127.0.0.1:8770/"

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help) sed -n '2,/^set -euo pipefail$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "install.sh: unknown argument: $arg (try --help)" >&2; exit 2 ;;
  esac
done

say()  { printf '%s\n' "$*"; }
warn() { printf '\n!! WARNING: %s\n\n' "$*" >&2; }
die()  { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

cd "$HERE"

# --- a. host ----------------------------------------------------------------
OS="$(uname -s)"
case "$OS" in
  Darwin)
    say "[a] macOS $(sw_vers -productVersion 2>/dev/null || echo '?') — launchd"
    # A launchd-spawned /bin/bash has no macOS privacy (TCC) grant, so it cannot
    # open run.sh under these folders and the service crash-loops. Terminal has
    # its own grant, so the build and a dry run would still pass — hence a check
    # on the resolved path.
    REAL_HERE="$(pwd -P)"
    REAL_HOME="$(cd "$HOME" && pwd -P)"
    case "$REAL_HERE/" in
      "$REAL_HOME/Documents/"*|"$REAL_HOME/Desktop/"*|"$REAL_HOME/Downloads/"*|"$REAL_HOME/Library/Mobile Documents/"*|"$REAL_HOME/Library/CloudStorage/"*)
        loc_msg="$REAL_HERE is inside a macOS privacy-protected folder (~/Documents, ~/Desktop,
   ~/Downloads, iCloud Drive or ~/Library/CloudStorage). launchd jobs cannot read it, so the
   LaunchAgent would fail with 'Operation not permitted'. Move the folder elsewhere
   (e.g. ~/ai-service) and re-run install.sh from there."
        if [ "$DRY_RUN" = 1 ]; then warn "$loc_msg (dry run continues)"; else die "$loc_msg"; fi
        ;;
    esac
    ;;
  Linux)
    if ! command -v systemctl >/dev/null 2>&1; then
      if [ "$DRY_RUN" = 1 ]; then warn "systemctl not found — Linux support needs systemd (dry run continues)"
      else die "systemctl not found — Linux support needs systemd"; fi
    fi
    # systemd reads whitespace as a list separator and '%' as a specifier in the
    # unit's paths; refuse them rather than write a unit that points elsewhere.
    case "$HERE" in
      *[[:space:]%]*) die "the folder path must not contain whitespace or '%' (systemd unit paths): $HERE" ;;
    esac
    say "[a] Linux — systemd user unit"
    ;;
  *) die "unsupported OS: $OS (macOS or Linux only)" ;;
esac

# --- b. toolchain -----------------------------------------------------------
# In --dry-run a missing tool is a warning, not a stop: nothing gets built.
need() {  # need <cmd> <how to get it>
  if ! command -v "$1" >/dev/null 2>&1; then
    if [ "$DRY_RUN" = 1 ]; then
      warn "$1 not found — $2 (dry run continues)"
      return 0
    fi
    die "$1 not found — $2"
  fi
}
if [ "$OS" = Darwin ]; then
  need go "brew install go"
else
  need go "install Go from https://go.dev/dl/ (or your package manager)"
fi
say "[b] go $(go env GOVERSION 2>/dev/null || echo missing)"

# --- c. build ---------------------------------------------------------------
# Linux's tray is pure D-Bus, so the binary stays static there. macOS needs
# Cocoa through cgo for the menu-bar item, which needs the Xcode command-line
# tools (clang). The tray is always compiled in; it appears when either route
# has more than one provider configured.
if [ "$DRY_RUN" = 1 ]; then
  say "[c] dry run — skipping go build"
else
  mkdir -p "$HERE/bin"
  if [ "$OS" = Darwin ]; then
    if ! xcrun --find clang >/dev/null 2>&1; then
      die "Xcode Command Line Tools are required to build the menu-bar picker (macOS Cocoa): run 'xcode-select --install', then re-run."
    fi
    CGO_ENABLED=1 go build -o "$HERE/bin/ai-service" ./cmd/ai-service
  else
    CGO_ENABLED=0 go build -o "$HERE/bin/ai-service" ./cmd/ai-service
  fi
  say "[c] built bin/ai-service"
fi

# --- d. ai-service.env ------------------------------------------------------
# A SECRET FILE. Nothing below prints any part of it: emptiness is tested with
# grep -q, and a key typed into the prompt goes to disk through an environment
# variable, never argv and never stdout.
file_mode() {  # octal permission bits of a file, on either OS
  if [ "$OS" = Darwin ]; then stat -f '%Lp' "$1"; else stat -c '%a' "$1"; fi
}
# ask_secret <prompt>: read one hidden value from the user and print it on stdout.
# macOS: a hidden osascript dialog. Linux: a hidden zenity dialog when there is a
# display, else a hidden read from the terminal. Returns non-zero on cancel.
ask_secret() {
  if [ "$OS" = Darwin ]; then
    osascript -e "text returned of (display dialog \"$1\" with title \"ai-service install\" default answer \"\" with hidden answer)" 2>/dev/null
  elif command -v zenity >/dev/null 2>&1 && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
    zenity --entry --hide-text --title="ai-service install" --text="$1" 2>/dev/null
  elif [ -r /dev/tty ]; then
    local v
    IFS= read -rs -p "$1: " v </dev/tty || return 1
    printf '\n' >/dev/tty
    printf '%s' "$v"
  else
    return 1
  fi
}
# env_set <KEY>: is KEY= followed by a real value in ai-service.env? (Quotes
# alone, whitespace and a comment do not count.)
env_set() { grep -qE "^$1=[\"']?[^\"'[:space:]#]" "$ENVFILE"; }

# ask_key <KEY> <purpose> [allow-empty]: ask for one key in a hidden prompt and
# write it into ai-service.env. Cancelling, an empty value (unless allow-empty is
# 1, which returns 1 instead) or a value with a space, quote, backslash, $ or
# backtick stops the whole script.
ask_key() {
  local key="$1" purpose="$2" allow_empty="${3:-0}" val
  val=$(ask_secret "Enter $key (used for: $purpose)") || { echo "cancelled"; exit 1; }
  # Trim surrounding whitespace a paste may carry.
  val="${val#"${val%%[![:space:]]*}"}"
  val="${val%"${val##*[![:space:]]}"}"
  if [ -z "$val" ]; then
    [ "$allow_empty" = 1 ] && return 1
    echo "empty value"; exit 1
  fi
  case "$val" in
    *[[:space:]\'\"\\\$\`]*) echo "rejected: the key contains a space, quote, backslash, \$ or backtick"; exit 1 ;;
  esac
  # Every step below ends in an explicit `|| exit 1`, not in set -e: the caller
  # may run ask_key as the left side of `||`, and bash then turns errexit off for
  # this whole body. Without them a failed awk would still reach the mv and
  # replace ai-service.env with a truncated file.
  (
    umask 077
    tmp="$(mktemp "$HERE/.ai-service.env.XXXXXX")" || exit 1
    # $tmp holds the key until the mv: remove it on any other way out (a failed
    # awk, Ctrl-C). Harmless after the mv. The INT/TERM/HUP trap makes those
    # signals exit through the EXIT trap. .gitignore also excludes the name.
    trap 'rm -f "$tmp"' EXIT
    trap 'exit 1' INT TERM HUP
    # Replace the existing KEY= line (first one wins, duplicates are dropped)
    # or append one. The value arrives via ENVIRON, not awk -v, which would
    # put it in argv and interpret backslash escapes.
    LLM_KEY="$key" LLM_VAL="$val" awk '
      index($0, ENVIRON["LLM_KEY"] "=") == 1 { if (!done) { print ENVIRON["LLM_KEY"] "=" ENVIRON["LLM_VAL"]; done = 1 } next }
      { print }
      END { if (!done) print ENVIRON["LLM_KEY"] "=" ENVIRON["LLM_VAL"] }
    ' "$ENVFILE" >"$tmp" || exit 1
    chmod 600 "$tmp" || exit 1
    mv -f "$tmp" "$ENVFILE" || exit 1
  ) || { echo "could not write $ENVFILE; it was not changed" >&2; exit 1; }
  echo "saved $key"
}

# /api/transcribe/stream runs on ASSEMBLYAI_API_KEY and/or DEEPGRAM_API_KEY;
# /api/transcribe-verbatim uses OPENROUTER_TRANSCRIBE_API_KEY. /api/explain
# takes OPENROUTER_API_KEY and/or CLAUDE_API_KEY; Memoir takes
# MEMOIR_CLAUDE_API_KEY or MEMOIR_CEREBRAS_API_KEY. The blocks below handle all
# three routes.
if [ "$DRY_RUN" = 1 ]; then
  # A dry run does not open ai-service.env at all — not even grep -q.
  if [ -f "$ENVFILE" ]; then
    say "[d] dry run — ai-service.env exists (mode $(file_mode "$ENVFILE")); API key check skipped"
  else
    say "[d] dry run — ai-service.env missing; a real run creates it from .env.example and asks for the API keys"
  fi
else
  if [ ! -f "$ENVFILE" ]; then
    ( umask 077; cp "$HERE/.env.example" "$ENVFILE" )
    say "[d] created ai-service.env from .env.example"
  fi
  chmod 600 "$ENVFILE"
  # /api/transcribe/stream has two providers, AssemblyAI and Deepgram.
  # /api/explain has two, Claude and OpenRouter. Memoir still refuses more than
  # one, so stop before restarting the service into a configuration it would
  # reject.
  if env_set PARAKEET_URL; then
    die "ai-service.env still sets PARAKEET_URL, which is no longer supported: the local Parakeet integration was removed and the service would refuse to start. Delete the line (or comment it out), then re-run."
  fi
  if env_set GROQ_API_KEY || env_set GROQ_MODEL; then
    die "ai-service.env still sets GROQ_API_KEY or GROQ_MODEL. Groq was removed: /api/transcribe/stream now runs on AssemblyAI or Deepgram, and the service would refuse to start. Delete the retired lines (or comment them out), then re-run."
  fi
  if env_set MAI_TRANSCRIBE_API_KEY; then
    die "ai-service.env still sets MAI_TRANSCRIBE_API_KEY, which was replaced by OPENROUTER_TRANSCRIBE_API_KEY: MAI-Transcribe-2 now runs through OpenRouter, and the old Azure key makes the service refuse to start. Rename it (or comment it out), then re-run."
  fi
  if env_set AZURE_SPEECH_KEY || env_set AZURE_SPEECH_ENDPOINT; then
    die "ai-service.env still sets AZURE_SPEECH_KEY or AZURE_SPEECH_ENDPOINT. The direct Azure Speech path was removed; /api/transcribe-verbatim now uses OPENROUTER_TRANSCRIBE_API_KEY. Delete the retired lines (or comment them out), then re-run."
  fi
  if env_set MEMOIR_CEREBRAS_API_KEY && env_set MEMOIR_CLAUDE_API_KEY; then
    die "ai-service.env sets both MEMOIR_CEREBRAS_API_KEY and MEMOIR_CLAUDE_API_KEY, so it is unclear which provider answers /api/memoir/check and the service would refuse to start. Keep one and set the other to empty (KEY=) or comment it out, then re-run."
  fi
  # The old name is not read any more, and the service refuses to start with it.
  if env_set CEREBRAS_API_KEY; then
    die "ai-service.env still sets CEREBRAS_API_KEY, which was renamed MEMOIR_CEREBRAS_API_KEY (every Memoir variable starts with MEMOIR_) and makes the service refuse to start. Rename it, or comment the line out to stop using Cerebras, then re-run."
  fi
  # /api/transcribe/stream needs at least one of AssemblyAI or Deepgram;
  # /api/transcribe-verbatim uses OpenRouter's MAI-Transcribe-2. Ask for the
  # required first provider, then offer the other and the optional verbatim key.
  if ! env_set ASSEMBLYAI_API_KEY && ! env_set DEEPGRAM_API_KEY; then
    ask_key ASSEMBLYAI_API_KEY "microphone transcription via AssemblyAI Universal-3.5 Pro. Leave it empty to use Deepgram instead" 1 ||
      ask_key DEEPGRAM_API_KEY "microphone transcription via Deepgram Nova-3"
  fi
  if ! env_set DEEPGRAM_API_KEY; then
    ask_key DEEPGRAM_API_KEY "microphone transcription via Deepgram Nova-3. Leave it empty to skip" 1 ||
      say "[d] ai-service.env: DEEPGRAM_API_KEY left empty — /api/transcribe/stream stays on AssemblyAI"
  fi
  if ! env_set ASSEMBLYAI_API_KEY; then
    ask_key ASSEMBLYAI_API_KEY "microphone transcription via AssemblyAI Universal-3.5 Pro. Leave it empty to skip" 1 ||
      say "[d] ai-service.env: ASSEMBLYAI_API_KEY left empty — /api/transcribe/stream stays on Deepgram"
  fi
  if ! env_set OPENROUTER_TRANSCRIBE_API_KEY; then
    ask_key OPENROUTER_TRANSCRIBE_API_KEY "verbatim transcription via OpenRouter MAI-Transcribe-2. Leave it empty to skip" 1 ||
      say "[d] ai-service.env: OPENROUTER_TRANSCRIBE_API_KEY left empty — /api/transcribe-verbatim stays unconfigured"
  fi
  # Both live providers decode MediaRecorder WebM/MP4 to PCM with ffmpeg.
  if ! command -v ffmpeg >/dev/null 2>&1; then
    die "ffmpeg is required to decode comart's WebM/MP4 recordings for the AssemblyAI and Deepgram live APIs, but it is not on PATH. Install it (Debian/Ubuntu: sudo apt install ffmpeg; macOS: brew install ffmpeg), then re-run."
  fi
  say "[d] ffmpeg: $(command -v ffmpeg) — decodes comart's recordings for AssemblyAI and Deepgram"
  if env_set ASSEMBLYAI_API_KEY && env_set DEEPGRAM_API_KEY; then
    say "[d] ai-service.env: ASSEMBLYAI_API_KEY and DEEPGRAM_API_KEY are set — /api/transcribe/stream uses the tray picker (starts on AssemblyAI)"
  elif env_set ASSEMBLYAI_API_KEY; then
    say "[d] ai-service.env: ASSEMBLYAI_API_KEY is set — /api/transcribe/stream runs on AssemblyAI Universal-3.5 Pro"
  else
    say "[d] ai-service.env: DEEPGRAM_API_KEY is set — /api/transcribe/stream runs on Deepgram Nova-3"
  fi
  if env_set OPENROUTER_TRANSCRIBE_API_KEY; then
    say "[d] ai-service.env: OPENROUTER_TRANSCRIBE_API_KEY is set — /api/transcribe-verbatim runs on OpenRouter MAI-Transcribe-2"
  fi
  if env_set OPENROUTER_API_KEY && env_set CLAUDE_API_KEY; then
    say "[d] ai-service.env: OPENROUTER_API_KEY and CLAUDE_API_KEY are set — /api/explain uses the tray picker (starts on the Claude Platform)"
  elif env_set OPENROUTER_API_KEY; then
    say "[d] ai-service.env: OPENROUTER_API_KEY is set — /api/explain runs on OpenRouter"
    say "[d] add CLAUDE_API_KEY by hand to enable the explain picker"
  elif env_set CLAUDE_API_KEY; then
    say "[d] ai-service.env: CLAUDE_API_KEY is set — /api/explain runs on the Claude Platform"
    say "[d] add OPENROUTER_API_KEY by hand to enable the explain picker"
  else
    # Neither: ask for OpenRouter first, and an empty answer there means Claude.
    ask_key OPENROUTER_API_KEY "word explanations via OpenRouter. Leave it empty to use a Claude key instead" 1 ||
      ask_key CLAUDE_API_KEY "word explanations via the Claude Platform"
  fi
  if env_set MEMOIR_CLAUDE_API_KEY; then
    say "[d] ai-service.env: MEMOIR_CLAUDE_API_KEY is set — /api/memoir/check runs on the Claude Platform"
  elif env_set MEMOIR_CEREBRAS_API_KEY; then
    say "[d] ai-service.env: MEMOIR_CEREBRAS_API_KEY is set — /api/memoir/check runs on Cerebras"
  else
    # Neither: ask for Claude first (the default), and an empty answer means Cerebras.
    ask_key MEMOIR_CLAUDE_API_KEY "Memoir via the Claude Platform. Leave it empty to use a Cerebras key instead" 1 ||
      ask_key MEMOIR_CEREBRAS_API_KEY "Memoir via Cerebras"
  fi
fi

# --- e. service definition ---------------------------------------------------
if [ "$DRY_RUN" = 1 ]; then
  SVC_DIR="$(mktemp -d "${TMPDIR:-/tmp}/ai-service-install.XXXXXX")"
else
  # Neither launchd nor systemd creates a missing log directory.
  mkdir -p "$HERE/logs" "$HERE/data"
  if [ "$OS" = Darwin ]; then
    SVC_DIR="$HOME/Library/LaunchAgents"
  else
    SVC_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  fi
  mkdir -p "$SVC_DIR"
fi

xml() {  # XML-escape one string for a <string> element
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' \
                         -e 's/"/\&quot;/g' -e "s/'/\&apos;/g"
}

write_plist() {
  cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/bash</string>
		<string>$(xml "$HERE/run.sh")</string>
	</array>
	<key>WorkingDirectory</key>
	<string>$(xml "$HERE")</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
		<key>HOME</key>
		<string>$(xml "$HOME")</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$(xml "$HERE/logs/ai-service.log")</string>
	<key>StandardErrorPath</key>
	<string>$(xml "$HERE/logs/ai-service.log")</string>
</dict>
</plist>
EOF
}

write_unit() {
  cat <<EOF
# Written by $HERE/install.sh — edit that script, not this file; re-running it
# overwrites this unit. Remove with $HERE/uninstall.sh.
[Unit]
Description=ai-service — localhost LLM backend for comart, Cairn's Memoir tab and daily-dictation
Documentation=file://$HERE/README.md
# No After=network-online.target: that is a system target, which a user unit
# cannot order against. The server binds loopback, which needs no network.
# No start rate limit either, so a crash loop keeps retrying every RestartSec
# the way launchd's KeepAlive does, instead of giving up after five tries.
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=$HERE
# run.sh, not the binary: it sources ai-service.env with bash and defaults
# CACHE_DB relative to itself. Not an EnvironmentFile=, which parses quoting
# differently from bash.
ExecStart=$HERE/run.sh
Restart=always
RestartSec=10
StandardOutput=append:$HERE/logs/ai-service.log
StandardError=append:$HERE/logs/ai-service.log
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
# The only paths the service writes: the SQLite cache and its log.
ReadWritePaths=$HERE/data $HERE/logs

[Install]
WantedBy=default.target
EOF
}

if [ "$OS" = Darwin ]; then
  SVC_FILE="$SVC_DIR/$LABEL.plist"
  write_plist >"$SVC_FILE"
  chmod 644 "$SVC_FILE"
  plutil -lint -s "$SVC_FILE" || die "$SVC_FILE failed plutil -lint; nothing was loaded"
  say "[e] $SVC_FILE: plutil OK"
else
  SVC_FILE="$SVC_DIR/$UNIT"
  write_unit >"$SVC_FILE"
  chmod 644 "$SVC_FILE"
  if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze --user verify "$SVC_FILE" || die "$SVC_FILE failed systemd-analyze verify; nothing was started"
    say "[e] $SVC_FILE: systemd-analyze verify OK"
  else
    say "[e] wrote $SVC_FILE (systemd-analyze not found; not linted)"
  fi
fi

if [ "$DRY_RUN" = 1 ]; then
  say "[e] dry run — service definition written to $SVC_DIR; nothing was started"
  say "[f] dry run — skipping health check"
  say "dry run OK"
  exit 0
fi

if [ "$OS" = Darwin ]; then
  DOMAIN="gui/$(id -u)"
  launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
  launchctl enable "$DOMAIN/$LABEL" 2>/dev/null || true
  # bootout returns before launchd has finished tearing the old instance down
  # (a graceful shutdown can take up to ~15 s), and a bootstrap in that window
  # fails with "5: Input/output error". Retry for longer than that.
  ok=0
  for _ in $(seq 1 25); do
    if launchctl bootstrap "$DOMAIN" "$SVC_FILE" 2>/dev/null; then ok=1; break; fi
    sleep 1
  done
  if [ "$ok" != 1 ]; then
    launchctl bootstrap "$DOMAIN" "$SVC_FILE" || die "launchctl bootstrap $DOMAIN $SVC_FILE failed"
  fi
  say "[e] loaded $LABEL"
else
  systemctl --user daemon-reload
  systemctl --user enable "$UNIT" >/dev/null 2>&1 || die "systemctl --user enable $UNIT failed"
  systemctl --user restart "$UNIT" || die "systemctl --user restart $UNIT failed"
  say "[e] enabled and (re)started $UNIT"
  # A user unit only runs while you are logged in unless lingering is on.
  if command -v loginctl >/dev/null 2>&1 &&
     [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || true)" != yes ]; then
    say "    note: lingering is off, so the service stops when you log out. To keep it"
    say "    running without a login session: loginctl enable-linger $(id -un)"
  fi
fi

# --- f. health check --------------------------------------------------------
up=0
for _ in $(seq 1 15); do
  if curl -fsS -o /dev/null --max-time 2 "$HEALTH_URL" 2>/dev/null; then up=1; break; fi
  sleep 1
done
if [ "$up" != 1 ]; then
  say "[f] $HEALTH_URL did not answer within ~15s. Last lines of logs/ai-service.log:"
  tail -n 30 "$HERE/logs/ai-service.log" 2>/dev/null || say "    (no log yet)"
  exit 1
fi

service_pid() {  # the pid the service manager thinks it is running, or empty
  if [ "$OS" = Darwin ]; then
    launchctl print "gui/$(id -u)/$LABEL" 2>/dev/null | awk '/^[[:space:]]*pid = /{print $3; exit}' || true
  else
    local p
    p="$(systemctl --user show -p MainPID --value "$UNIT" 2>/dev/null || true)"
    if [ -n "$p" ] && [ "$p" != 0 ]; then printf '%s' "$p"; fi
  fi
}
port_pid() {  # the pid listening on 8770, or empty
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:8770 -sTCP:LISTEN -t 2>/dev/null | head -n 1 || true
  elif command -v ss >/dev/null 2>&1; then
    ss -Hltnp 'sport = :8770' 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -n 1 || true
  fi
}

# Something answering is not proof it is OUR instance: a server started by hand
# would hold the port while the managed one crash-loops on "address in use". That
# service has NO pid most of the time — log.Fatalf kills it in milliseconds and the
# service manager waits ~10s before the next try — so an absent pid is a failure,
# not a pass. Polled, because our own pid can take a moment to show.
svc_pid=""
listen_pid=""
for _ in 1 2 3 4 5; do
  svc_pid="$(service_pid)"
  listen_pid="$(port_pid)"
  if [ -n "$svc_pid" ] && [ "$svc_pid" = "$listen_pid" ]; then break; fi
  sleep 1
done
if [ -z "$svc_pid" ]; then
  warn "$HEALTH_URL answers (pid ${listen_pid:-?}), but the service has no running process —
   it is crash-looping, most likely on 'address in use'. Another server is holding
   port 8770 (a ./run.sh in a terminal?); stop it, then re-run install.sh.
   Log: $HERE/logs/ai-service.log"
  exit 1
fi
if [ -n "$listen_pid" ] && [ "$svc_pid" != "$listen_pid" ]; then
  warn "$HEALTH_URL answers from pid $listen_pid, but the service is pid $svc_pid.
   Another server is holding port 8770; stop it, then re-run install.sh."
  exit 1
fi
say "[f] $HEALTH_URL answers (pid ${listen_pid:-?}) — ai-service is up"
