# ai-service tray position (GNOME Shell)

A small GNOME Shell 48 extension that keeps the ai-service tray icon at index 0
of the top bar's right-hand box, so the visible order is:

    ai-service | other tray icons | system indicators

GNOME's AppIndicator extension has no per-icon ordering setting: every
StatusNotifierItem is inserted at index 1 when it registers, and an extension
that adds its own button to the top bar without naming a position gets index 0
(`Main.panel.addToStatusArea()` defaults to it), in front of ai-service. This
extension watches the panel box and moves ai-service back to the front after
any add or remove, and re-checks every 500 ms for reorders that emit no signal,
so newly started apps, other extensions, a shell restart or an ai-service D-Bus
reconnect cannot displace it.

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
