//go:build desktop && windows

package main

import "testing"

// TestMessageBoxBindingResolves checks that the Win32 entry point the fatal
// handler depends on actually exists under the name we use.
//
// This is worth a test because the failure mode is nasty: a wrong DLL or
// procedure name does not fail to compile — syscall resolves lazily at first
// CALL, so the error would surface only in the one situation the dialog exists
// for (an already-failing startup), and as a panic rather than a message.
// LazyProc.Find() forces the DLL load and the symbol lookup now. (LazyDLL
// itself has no Find method; resolving the proc loads the DLL for us.)
func TestMessageBoxBindingResolves(t *testing.T) {
	if err := procMsgBoxW.Find(); err != nil {
		t.Fatalf("user32.dll!MessageBoxW not resolvable: %v", err)
	}
}
