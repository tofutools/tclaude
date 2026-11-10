//go:build darwin && cgo

package agentd

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>
#import <objc/runtime.h>

// tclaude_remove_status_item removes the tray's NSStatusItem from the menu
// bar. fyne.io/systray keeps it in its app delegate's statusItem ivar and
// has no API to remove it: its Quit only stops the run loop. Returns 0 once
// removed, a negative value when there was nothing it could remove.
static int tclaude_remove_status_item(void) {
	if (![NSThread isMainThread]) {
		return -1;
	}
	@autoreleasepool {
		if (NSApp == nil) {
			return -2;
		}
		id delegate = [NSApp delegate];
		if (delegate == nil) {
			return -2;
		}
		Ivar ivar = class_getInstanceVariable([delegate class], "statusItem");
		if (ivar == NULL) {
			return -3;
		}
		id item = object_getIvar(delegate, ivar);
		if (item == nil || ![item isKindOfClass:[NSStatusItem class]]) {
			return -4;
		}
		[[NSStatusBar systemStatusBar] removeStatusItem:(NSStatusItem *)item];
		// Let the removal reach the window server before the exec.
		[[NSRunLoop currentRunLoop] runUntilDate:[NSDate dateWithTimeIntervalSinceNow:0.2]];
	}
	return 0;
}
*/
import "C"

import "log/slog"

// releaseTrayStatusItem removes the tray icon from the menu bar before this
// process execs into something else (the relaunch helper, or an exec in
// place). The window server keeps a status item registered to the PID that
// made it for as long as that PID lives, and an exec keeps the PID: a
// waiting relaunch helper would otherwise hold a blank slot in the menu bar
// for the rest of the shell job. It must run on the main thread, after the
// tray loop has returned.
func releaseTrayStatusItem() {
	if rc := C.tclaude_remove_status_item(); rc != 0 {
		slog.Warn("agentd restart: could not remove the tray icon before the restart", "code", int(rc))
	}
}
