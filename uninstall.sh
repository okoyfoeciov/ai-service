#!/usr/bin/env bash
# uninstall.sh — stop ai-service and remove the service definition install.sh wrote
# (macOS: the LaunchAgent plist; Linux: the systemd user unit).
#
# The reverse of install.sh's service step and nothing more. It deletes no data,
# no secrets and no build output: bin/, data/, logs/ and ai-service.env all
# stay exactly where they are, so ./install.sh brings everything back.
set -euo pipefail

LABEL="com.ai-service.backend"
UNIT="ai-service.service"

case "$(uname -s)" in
  Darwin)
    DOMAIN="gui/$(id -u)"
    PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
    if launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null; then
      echo "stopped $LABEL"
    else
      echo "$LABEL was not loaded"
    fi
    if [ -f "$PLIST" ]; then
      rm -f "$PLIST"
      echo "removed $PLIST"
    fi
    ;;
  Linux)
    UNIT_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$UNIT"
    if systemctl --user disable --now "$UNIT" >/dev/null 2>&1; then
      echo "stopped and disabled $UNIT"
    else
      echo "$UNIT was not running (or systemd could not be reached)"
    fi
    if [ -f "$UNIT_FILE" ]; then
      rm -f "$UNIT_FILE"
      systemctl --user daemon-reload || true
      echo "removed $UNIT_FILE"
    fi
    ;;
  *)
    echo "uninstall.sh: unsupported OS $(uname -s) (macOS or Linux only)" >&2
    exit 1
    ;;
esac

echo "Left alone: bin/, data/, logs/ and ai-service.env."
