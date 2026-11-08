// Keep the ai-service StatusNotifierItem at index 1 of GNOME Shell's right
// panel box. The Voice Typing extension owns index 0.
//
// GNOME's AppIndicator extension inserts every tray icon at index 1 when it
// registers, so the slot belongs to whichever icon registered last. This
// extension watches the panel box instead and moves ai-service back whenever
// another icon is added or removed, including after a shell restart or after
// ai-service re-registers on a new session bus. The move runs from an idle
// callback so the AppIndicator extension can finish its own insertion first,
// and a light periodic pass catches reorders done without a signal.

import GLib from 'gi://GLib';

import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';

const TARGET_ID = 'ai-service';
const TARGET_INDEX = 1;

export default class AiServiceTrayPositionExtension extends Extension {
    enable() {
        this._box = Main.panel._rightBox;
        this._reorderIdleId = 0;
        this._verifyId = 0;
        this._moving = false;

        // child-added emits when the Voice Typing button or another tray icon
        // appears; child-removed covers one disappearing above ai-service,
        // which would otherwise shift ai-service into slot 0.
        this._childAddedId = this._box.connect('child-added', () =>
            this._scheduleReorder());
        this._childRemovedId = this._box.connect('child-removed', () =>
            this._scheduleReorder());

        // The icon may already be in the panel when the extension is enabled.
        this._scheduleReorder();

        // Other extensions reorder their own indicators with
        // set_child_at_index(), which emits no signal. The Voice Typing
        // extension does exactly that to stay at index 0. A cheap verification
        // pass keeps ai-service at index 1 even when a reorder races ours.
        this._verifyId = GLib.timeout_add(
            GLib.PRIORITY_DEFAULT_IDLE, 500, () => {
                this._pin();
                return GLib.SOURCE_CONTINUE;
            });
    }

    disable() {
        if (this._box) {
            if (this._childAddedId)
                this._box.disconnect(this._childAddedId);
            if (this._childRemovedId)
                this._box.disconnect(this._childRemovedId);
        }
        this._childAddedId = 0;
        this._childRemovedId = 0;

        if (this._reorderIdleId) {
            GLib.source_remove(this._reorderIdleId);
            this._reorderIdleId = 0;
        }
        if (this._verifyId) {
            GLib.source_remove(this._verifyId);
            this._verifyId = 0;
        }

        this._box = null;
    }

    _scheduleReorder() {
        if (this._reorderIdleId)
            return;

        this._reorderIdleId = GLib.idle_add(GLib.PRIORITY_DEFAULT_IDLE, () => {
            this._reorderIdleId = 0;
            this._pin();
            return GLib.SOURCE_REMOVE;
        });
    }

    _pin() {
        const box = this._box;
        if (!box || this._moving)
            return;

        const container = this._findContainer();
        if (!container || container.get_parent() !== box)
            return;
        // set_child_at_index() needs a real destination. With one child there
        // is no slot 1 yet, and moving the only icon is meaningless anyway.
        if (TARGET_INDEX >= box.get_n_children())
            return;
        if (box.get_child_at_index(TARGET_INDEX) === container)
            return;

        // set_child_at_index() does not emit child-added, but guard against
        // it in case a future Clutter version changes that.
        this._moving = true;
        try {
            box.set_child_at_index(container, TARGET_INDEX);
        } finally {
            this._moving = false;
        }
    }

    _findContainer() {
        const box = this._box;
        for (const indicator of Object.values(Main.panel.statusArea)) {
            try {
                // IndicatorStatusIcon (AppIndicator extension) exposes the SNI
                // object as _indicator; its id is the StatusNotifierItem Id.
                if (indicator?._indicator?.id !== TARGET_ID)
                    continue;
                const container = indicator.container;
                if (container?.get_parent() === box)
                    return container;
            } catch {
                // An indicator can be mid-destroy while the timer runs; it is
                // not a candidate then.
            }
        }
        return null;
    }
}
