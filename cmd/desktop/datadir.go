//go:build desktop

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// appDirName is the per-application folder under %APPDATA% / %LOCALAPPDATA%.
const appDirName = "DockerPull"

// dataDirChoice records one candidate directory and what happened to it, so a
// startup failure can tell the user *which* paths were refused and why.
type dataDirChoice struct {
	Label string
	Dir   string
	Err   error
}

// resolveDataDir picks a writable directory for the database, the blob cache
// and the WebView2 profile.
//
// Candidates, in order:
//
//	APP_DATA_DIR → %APPDATA%\<app> → %LOCALAPPDATA%\<app> → <exe dir>\data
//
// Every candidate is probed by actually creating and WRITING a file. Two
// reasons that matters:
//
//   - The previous version called os.UserConfigDir() unconditionally — which
//     on Windows is %APPDATA% — so it ignored APP_DATA_DIR entirely. An
//     operator whose roaming profile is redirected to OneDrive, disabled by
//     policy, or blocked by EDR therefore had no way to point the app
//     anywhere writable: it exited with "Access is denied" and, being a
//     -H windowsgui binary, said nothing at all.
//
//   - A directory that can be CREATED is not necessarily WRITABLE. os.MkdirAll
//     succeeding while the first write to app.db fails is precisely the
//     "readonly database"/CANTOPEN failure, and it is what the probe rules out
//     before the store is opened.
//
// It never returns an empty path: the last candidate sits next to the
// executable, which is writable whenever the binary itself is readable.
func resolveDataDir() (string, []dataDirChoice) {
	var choices []dataDirChoice

	add := func(label, dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		choices = append(choices, dataDirChoice{Label: label, Dir: dir})
	}

	// 1. An explicit choice always comes first. config.Load() has already run,
	//    so this sees both a real environment variable and a .env entry.
	add("APP_DATA_DIR", os.Getenv("APP_DATA_DIR"))
	// 2. Roaming app data: the conventional home for user settings.
	if dir, err := os.UserConfigDir(); err == nil {
		add("UserConfigDir (%APPDATA%)", filepath.Join(dir, appDirName))
	}
	// 3. Local app data. Survives a read-only or redirected roaming profile,
	//    and is the more appropriate home for a multi-gigabyte blob cache.
	if dir, err := os.UserCacheDir(); err == nil {
		add("UserCacheDir (%LOCALAPPDATA%)", filepath.Join(dir, appDirName))
	}
	// 4. Next to the executable: the portable install, and the last resort on a
	//    locked-down machine.
	if exe, err := os.Executable(); err == nil {
		add("executable directory", filepath.Join(filepath.Dir(exe), "data"))
	}

	for i := range choices {
		if err := probeWritable(choices[i].Dir); err != nil {
			choices[i].Err = err
			continue
		}
		return choices[i].Dir, choices
	}

	// Only reachable if the binary lives on a read-only volume AND every app
	// data location is denied. Still better than returning "", which would make
	// the store open in the working directory.
	fallback := filepath.Join(os.TempDir(), appDirName)
	_ = os.MkdirAll(fallback, 0o755)
	return fallback, choices
}

// probeWritable reports whether dir can be created AND written to.
//
// The write (and the sync) are the point: an ACL can permit directory creation
// while denying file writes, and a full or quota'd volume accepts buffered
// writes that fail on flush. Both would otherwise surface much later as an
// opaque database error.
func probeWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".dockerpull-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := f.Write([]byte("probe")); err != nil {
		_ = f.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// rejectedDirs renders the candidates that failed, for the log and the error
// dialog. This is the part a support request cannot reconstruct on its own.
func rejectedDirs(choices []dataDirChoice) []string {
	var out []string
	for _, c := range choices {
		if c.Err == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s — %s: %v", c.Dir, c.Label, c.Err))
	}
	return out
}
