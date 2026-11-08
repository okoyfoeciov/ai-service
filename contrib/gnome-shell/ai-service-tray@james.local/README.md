# ai-service tray position (GNOME Shell)

A small GNOME Shell 48 extension that keeps the ai-service tray icon at index 1
of the top bar's right-hand box. The Voice Typing extension pins itself at index
0, so the visible order is:

    voice typing | ai-service | other tray icons | system indicators

GNOME's AppIndicator extension has no per-icon ordering setting: every
StatusNotifierItem is inserted at index 1 when it registers, so the icon that
registered last normally gets that slot. This extension watches the panel box
and moves ai-service back after any add or remove, so newly started apps, a
shell restart or an ai-service D-Bus reconnect cannot displace it.

The extension matches the StatusNotifierItem `Id` property, which
`internal/tray/tray_linux.go` sets to `ai-service`. If that id ever changes,
change `TARGET_ID` in `extension.js` too.

## Install

```sh
./install.sh
```

This copies the extension to
`${XDG_DATA_HOME:-$HOME/.local/share}/gnome-shell/extensions/ai-service-tray@james.local`
and enables it. On Wayland, GNOME Shell only loads newly installed extension
code at login, so log out and back in once after installing.

## Uninstall

```sh
./install.sh --uninstall
```
