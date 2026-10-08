//go:build desktop && windows

package main

import (
	"syscall"
	"unsafe"
)

// Message box flags: an OK button, the error icon, and topmost + foreground so
// the dialog is not hidden behind whatever else is on screen.
const (
	mbOK            = 0x00000000
	mbIconError     = 0x00000010
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
)

var (
	user32      = syscall.NewLazyDLL("user32.dll")
	procMsgBoxW = user32.NewProc("MessageBoxW")
)

// showFatalDialog shows a native message box.
//
// It calls user32 directly rather than adding a dialog dependency: the desktop
// target already links the Win32 API, and a startup failure is exactly the
// situation where dragging in a library — with its own initialisation and
// failure modes — is the wrong trade.
//
// HWND 0 (no owner) is intentional: this runs when there is no window yet, and
// MB_TOPMOST keeps the box visible anyway.
func showFatalDialog(title, body string) {
	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	bodyPtr, err := syscall.UTF16PtrFromString(body)
	if err != nil {
		return
	}
	_, _, _ = procMsgBoxW.Call(
		0,
		uintptr(unsafe.Pointer(bodyPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(mbOK|mbIconError|mbSetForeground|mbTopmost),
	)
}
