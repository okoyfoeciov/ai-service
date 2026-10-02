#!/usr/bin/env bash
# install.sh — build llm-service and keep it running on this machine.
#
# llm-service is the localhost LLM backend (127.0.0.1:8770) for the comart
# Chrome extension and Cairn's Memoir tab. Nothing fronts the server, so
# loopback is the trust boundary.
#
# Supported hosts, and the service each one gets:
#   macOS  launchd LaunchAgent  ~/Library/LaunchAgents/com.llm-service.backend.plist
#   Linux  systemd user unit    ~/.config/systemd/user/llm-service.service
# Either way the service runs run.sh at login, restarts it whenever it exits,
# and appends its output to logs/llm-service.log.
#
# Idempotent. Re-run it after changing the code or after moving this folder
# (the service definition carries absolute paths). Every run rebuilds, checks
# the API keys, rewrites the service definition and restarts the service.
#
#   ./install.sh            do it
#   ./install.sh --dry-run  build nothing, prompt for nothing, start nothing:
#                           write the service definition to a temp dir and lint it
#
# Undo with ./uninstall.sh.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVFILE="$HERE/llm-service.env"
LABEL="com.llm-service.backend"   # macOS LaunchAgent label
UNIT="llm-service.service"        # Linux systemd user unit
HEALTH_URL="http://127.0.0.1:8770/"

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
   (e.g. ~/llm-service) and re-run install.sh from there."
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
if [ "$DRY_RUN" = 1 ]; then
  say "[c] dry run — skipping go build"
else
  mkdir -p "$HERE/bin"
  CGO_ENABLED=0 go build -o "$HERE/bin/llm-service" ./cmd/llm-service
  say "[c] built bin/llm-service"
fi

# --- d. llm-service.env ------------------------------------------------------
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
    osascript -e "text returned of (display dialog \"$1\" with title \"llm-service install\" default answer \"\" with hidden answer)" 2>/dev/null
  elif command -v zenity >/dev/null 2>&1 && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
    zenity --entry --hide-text --title="llm-service install" --text="$1" 2>/dev/null
  elif [ -r /dev/tty ]; then
    local v
    IFS= read -rs -p "$1: " v </dev/tty || return 1
    printf '\n' >/dev/tty
    printf '%s' "$v"
  else
    return 1
  fi
}
# KEYS: every API key the service needs, with what it is used for.
KEYS=(
  "GROQ_API_KEY|transcription via Groq Whisper"
  "OPENROUTER_API_KEY|word explanations via OpenRouter"
  "CEREBRAS_API_KEY|Memoir via Cerebras"
  "AZURE_SPEECH_KEY|verbatim transcription via Azure MAI-Transcribe-2"
)
if [ "$DRY_RUN" = 1 ]; then
  # A dry run does not open llm-service.env at all — not even grep -q.
  if [ -f "$ENVFILE" ]; then
    say "[d] dry run — llm-service.env exists (mode $(file_mode "$ENVFILE")); API key check skipped"
  else
    say "[d] dry run — llm-service.env missing; a real run creates it from .env.example and asks for the API keys"
  fi
else
  if [ ! -f "$ENVFILE" ]; then
    ( umask 077; cp "$HERE/.env.example" "$ENVFILE" )
    say "[d] created llm-service.env from .env.example"
  fi
  chmod 600 "$ENVFILE"
  for entry in "${KEYS[@]}"; do
    key="${entry%%|*}"
    purpose="${entry#*|}"
    if grep -qE "^$key=[\"']?[^\"'[:space:]#]" "$ENVFILE"; then
      say "[d] llm-service.env: $key is set"
      continue
    fi
    val=$(ask_secret "Enter $key (used for: $purpose)") || { echo "cancelled"; exit 1; }
    # Trim surrounding whitespace a paste may carry.
    val="${val#"${val%%[![:space:]]*}"}"
    val="${val%"${val##*[![:space:]]}"}"
    [ -n "$val" ] || { echo "empty value"; exit 1; }
    case "$val" in
      *[[:space:]\'\"\\\$\`]*) echo "rejected: the key contains a space, quote, backslash, \$ or backtick"; exit 1 ;;
    esac
    (
      umask 077
      tmp="$(mktemp "$HERE/.llm-service.env.XXXXXX")"
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
      ' "$ENVFILE" >"$tmp"
      chmod 600 "$tmp"
      mv -f "$tmp" "$ENVFILE"
    )
    unset val
    echo "saved $key"
  done
fi

# --- e. service definition ---------------------------------------------------
if [ "$DRY_RUN" = 1 ]; then
  SVC_DIR="$(mktemp -d "${TMPDIR:-/tmp}/llm-service-install.XXXXXX")"
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
		<string>/usr/bin:/bin:/usr/sbin:/sbin</string>
		<key>HOME</key>
		<string>$(xml "$HOME")</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$(xml "$HERE/logs/llm-service.log")</string>
	<key>StandardErrorPath</key>
	<string>$(xml "$HERE/logs/llm-service.log")</string>
</dict>
</plist>
EOF
}

write_unit() {
  cat <<EOF
# Written by $HERE/install.sh — edit that script, not this file; re-running it
# overwrites this unit. Remove with $HERE/uninstall.sh.
[Unit]
Description=llm-service — localhost LLM backend for the comart extension and Cairn's Memoir tab
Documentation=file://$HERE/README.md
# No After=network-online.target: that is a system target, which a user unit
# cannot order against. The server binds loopback, which needs no network.
# No start rate limit either, so a crash loop keeps retrying every RestartSec
# the way launchd's KeepAlive does, instead of giving up after five tries.
StartLimitIntervalSec=0

[Service]
Type=simple
WorkingDirectory=$HERE
# run.sh, not the binary: it sources llm-service.env with bash and defaults
# CACHE_DB relative to itself. Not an EnvironmentFile=, which parses quoting
# differently from bash.
ExecStart=$HERE/run.sh
Restart=always
RestartSec=10
StandardOutput=append:$HERE/logs/llm-service.log
StandardError=append:$HERE/logs/llm-service.log
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
    # Before the first build bin/ may not exist yet; verify only warns about it.
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
  say "[f] $HEALTH_URL did not answer within ~15s. Last lines of logs/llm-service.log:"
  tail -n 30 "$HERE/logs/llm-service.log" 2>/dev/null || say "    (no log yet)"
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
   Log: $HERE/logs/llm-service.log"
  exit 1
fi
if [ -n "$listen_pid" ] && [ "$svc_pid" != "$listen_pid" ]; then
  warn "$HEALTH_URL answers from pid $listen_pid, but the service is pid $svc_pid.
   Another server is holding port 8770; stop it, then re-run install.sh."
  exit 1
fi
say "[f] $HEALTH_URL answers (pid ${listen_pid:-?}) — llm-service is up"
