#!/usr/bin/env bash
# install.sh — install or remove the optional GNOME Shell extension that keeps
# the ai-service tray icon at index 1 (second from the left).
#
#   ./install.sh              install or update, then enable
#   ./install.sh --uninstall  disable and remove
#   ./install.sh --help
set -euo pipefail

UUID="ai-service-tray@james.local"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$HERE/$UUID"
DST="${XDG_DATA_HOME:-$HOME/.local/share}/gnome-shell/extensions/$UUID"

say()  { printf '%s\n' "$*"; }
die()  { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
usage() { sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'; }

# list_with_uuid CURRENT: print CURRENT plus UUID, or fail if it is present.
list_with_uuid() {
    local current="$1"
    case "$current" in
        *"'$UUID'"*) return 1 ;;
        '@as []'|'[]') printf "['%s']\n" "$UUID" ;;
        *) printf "%s, '%s']\n" "${current%]}" "$UUID" ;;
    esac
}

# list_without_uuid CURRENT: print CURRENT without UUID, or fail if absent.
list_without_uuid() {
    local current="$1"
    case "$current" in
        *"'$UUID'"*) ;;
        *) return 1 ;;
    esac
    printf '%s\n' "$current" |
        sed -e "s/'$UUID', //" -e "s/, '$UUID'//" \
            -e "s/^\['$UUID'\]$/[]/"
}

# add_enabled removes UUID from disabled-extensions (which takes precedence)
# and adds it to enabled-extensions. Both keys are what GNOME Shell reads at
# startup and when its settings change, so this works before a new extension
# has been discovered by the running shell.
add_enabled() {
    local current new

    current="$(gsettings get org.gnome.shell enabled-extensions 2>/dev/null || echo '@as []')"
    if new="$(list_with_uuid "$current")"; then
        gsettings set org.gnome.shell enabled-extensions "$new" ||
            die "could not update org.gnome.shell enabled-extensions"
        say "enabled $UUID for the next GNOME Shell start"
    else
        say "already enabled: $UUID"
    fi

    current="$(gsettings get org.gnome.shell disabled-extensions 2>/dev/null || echo '@as []')"
    if new="$(list_without_uuid "$current")"; then
        gsettings set org.gnome.shell disabled-extensions "$new" ||
            die "could not update org.gnome.shell disabled-extensions"
        say "removed $UUID from disabled-extensions"
    fi

    if [ "$(gsettings get org.gnome.shell disable-user-extensions 2>/dev/null || echo false)" = "true" ]; then
        say "warning: org.gnome.shell disable-user-extensions is true; user extensions will not load" >&2
    fi
}

# remove_enabled clears UUID from both lists. gnome-extensions disable would
# otherwise leave it in disabled-extensions.
remove_enabled() {
    local current new

    current="$(gsettings get org.gnome.shell enabled-extensions 2>/dev/null || echo '@as []')"
    if new="$(list_without_uuid "$current")"; then
        gsettings set org.gnome.shell enabled-extensions "$new" || true
    fi

    current="$(gsettings get org.gnome.shell disabled-extensions 2>/dev/null || echo '@as []')"
    if new="$(list_without_uuid "$current")"; then
        gsettings set org.gnome.shell disabled-extensions "$new" || true
    fi
}

# --- arguments --------------------------------------------------------------
case "${1:-}" in
  "") ;;
  --uninstall)
    existed=0
    if [ -d "$DST" ]; then
        existed=1
    fi

    if command -v gnome-extensions >/dev/null 2>&1; then
        gnome-extensions disable "$UUID" >/dev/null 2>&1 || true
        gnome-extensions uninstall "$UUID" >/dev/null 2>&1 || true
    fi

    if command -v gsettings >/dev/null 2>&1; then
        remove_enabled
    fi

    if [ -d "$DST" ]; then
        rm -rf "$DST"
    fi
    if [ "$existed" = 1 ]; then
        say "removed $DST"
    else
        say "not installed: $DST"
    fi
    exit 0
    ;;
  -h|--help)
    usage
    exit 0
    ;;
  *)
    die "unknown argument: $1 (try --help)"
    ;;
esac

# --- install ----------------------------------------------------------------
[ -f "$SRC/metadata.json" ] || die "missing $SRC/metadata.json"
grep -q "\"uuid\"[[:space:]]*:[[:space:]]*\"$UUID\"" "$SRC/metadata.json" ||
    die "$SRC/metadata.json does not declare uuid $UUID"

command -v gsettings >/dev/null 2>&1 || die "gsettings not found; this extension needs GNOME Shell"
gsettings get org.gnome.shell enabled-extensions >/dev/null 2>&1 ||
    die "org.gnome.shell schema not found; this extension needs GNOME Shell"

mkdir -p "$(dirname "$DST")"
rm -rf "$DST"
cp -a "$SRC" "$DST"
say "installed $DST"

add_enabled
say "log out and back in to load the extension (Wayland loads extension code only at login)"
