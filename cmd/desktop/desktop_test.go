//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the startup-robustness code that a GUI launch depends on.
// They are the only automated coverage for it: the failure mode they guard
// against ("double-click does nothing") is invisible to every other test in
// the repo.
//
// Run with: go test -tags desktop ./cmd/desktop/

// TestProbeWritableAcceptsARealDirectory pins the positive case, so a future
// change cannot make the probe reject everything and quietly push every user
// onto the temp-dir fallback.
func TestProbeWritableAcceptsARealDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := probeWritable(dir); err != nil {
		t.Fatalf("probeWritable(%q) = %v, want nil", dir, err)
	}
	// It must also have cleaned up after itself: a stray probe file in the
	// user's data directory would be a bug in its own right.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dockerpull-probe-") {
			t.Errorf("probe file %q left behind", e.Name())
		}
	}
}

// TestProbeWritableRejectsAFileInTheWay is the deterministic negative case:
// a path whose parent is a regular FILE can never be created, on any platform.
//
// This is the class of failure the chain exists for — a redirected or
// read-only location that os.MkdirAll cannot fix.
func TestProbeWritableRejectsAFileInTheWay(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := probeWritable(filepath.Join(blocker, "data")); err == nil {
		t.Fatalf("probeWritable under a regular file returned nil, want an error")
	}
}

// TestResolveDataDirHonoursAPPDataDir is the regression test for the defect
// that made the app unredirectable: cmd/desktop used to overwrite the data
// directory with os.UserConfigDir() unconditionally, so APP_DATA_DIR had no
// effect and a user with an unwritable %APPDATA% had no way out.
func TestResolveDataDirHonoursAPPDataDir(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "explicit")
	t.Setenv("APP_DATA_DIR", explicit)

	got, choices := resolveDataDir()
	if got != explicit {
		t.Errorf("resolveDataDir() = %q, want the explicit APP_DATA_DIR %q", got, explicit)
	}
	if len(choices) == 0 {
		t.Fatal("no candidates reported")
	}
	// The explicit choice must be tried FIRST, so an operator's decision wins
	// over the conventional locations.
	if choices[0].Dir != explicit {
		t.Errorf("first candidate = %q (%s), want the explicit dir", choices[0].Dir, choices[0].Label)
	}
	if choices[0].Err != nil {
		t.Errorf("the explicit dir was rejected: %v", choices[0].Err)
	}
}

// TestResolveDataDirFallsBackAndRecordsWhy proves the chain moves on AND that
// the reason survives, because that reason is what the error dialog and the log
// show the user.
func TestResolveDataDirFallsBackAndRecordsWhy(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	explicit := filepath.Join(blocker, "data") // impossible to create
	t.Setenv("APP_DATA_DIR", explicit)

	got, choices := resolveDataDir()
	if got == explicit {
		t.Fatalf("resolveDataDir() kept an unwritable APP_DATA_DIR %q", got)
	}
	if got == "" {
		t.Fatal("resolveDataDir() returned an empty path; the store would open in the working directory")
	}

	// The rejected candidate must be recorded with its reason.
	var recorded bool
	for _, c := range choices {
		if c.Dir == explicit {
			recorded = c.Err != nil
		}
	}
	if !recorded {
		t.Errorf("the unwritable APP_DATA_DIR was not recorded as rejected: %+v", choices)
	}
	if len(rejectedDirs(choices)) == 0 {
		t.Error("rejectedDirs() reported nothing, so the dialog would not explain the failure")
	}

	// Whatever was chosen must genuinely be writable, or the fallback is
	// pointless.
	if err := probeWritable(got); err != nil {
		t.Errorf("the chosen directory %q is not writable: %v", got, err)
	}
}

// TestExecutableDirectoryIsTheLastResort pins the final candidate. If this ever
// stops being last, the chain loses its guaranteed-writable option.
func TestExecutableDirectoryIsTheLastResort(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	want := filepath.Join(filepath.Dir(exe), "data")

	_, choices := resolveDataDir()
	if len(choices) == 0 {
		t.Fatal("no candidates")
	}
	last := choices[len(choices)-1]
	if last.Dir != want {
		t.Errorf("last candidate = %q (%s), want the executable directory %q", last.Dir, last.Label, want)
	}
}

// TestBuildDialogBodyNamesThePathsAndTheRejections protects the content of the
// message box: a dialog that says only "failed" would leave the user exactly
// where the silent-exit bug did.
func TestBuildDialogBodyNamesThePathsAndTheRejections(t *testing.T) {
	saved := runtimePaths
	t.Cleanup(func() { runtimePaths = saved })

	runtimePaths.DataDir = `C:\data`
	runtimePaths.LogPath = `C:\data\dockerpull-desktop.log`
	runtimePaths.Rejected = []dataDirChoice{
		{Label: "UserConfigDir (%APPDATA%)", Dir: `C:\Users\x\AppData\Roaming\DockerPull`, Err: os.ErrPermission},
	}

	body := buildDialogBody("启动失败: open store: Access is denied.")

	for _, want := range []string{
		"启动失败",                   // the failure
		`C:\data`,                // where data went
		`dockerpull-desktop.log`, // where to read more
		"已跳过的数据目录",               // why we are not there
		"APP_DATA_DIR",           // the way out
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog body does not mention %q:\n%s", want, body)
		}
	}
}

// TestRecordFatalWritesTheLog covers the durable channel. The dialog is
// transient (and modal); the log is what a user can copy into a bug report.
func TestRecordFatalWritesTheLog(t *testing.T) {
	saved := runtimePaths
	t.Cleanup(func() { runtimePaths = saved })

	logPath := filepath.Join(t.TempDir(), "dockerpull-desktop.log")
	runtimePaths.LogPath = logPath

	if err := recordFatal("启动失败: something specific"); err != nil {
		t.Fatalf("recordFatal: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "[fatal] 启动失败: something specific") {
		t.Errorf("log does not contain the failure:\n%s", data)
	}
}
