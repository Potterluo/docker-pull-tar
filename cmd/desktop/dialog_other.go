//go:build desktop && !windows

package main

import (
	"fmt"
	"os"
)

// showFatalDialog falls back to stderr on platforms without a built-in
// message-box API. The log file remains the primary record, and on Linux/macOS
// a GUI launch from a terminal still shows this.
func showFatalDialog(title, body string) {
	fmt.Fprintf(os.Stderr, "\n=== %s ===\n%s\n", title, body)
}
