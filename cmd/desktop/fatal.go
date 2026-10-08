//go:build desktop

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// runtimePaths is what the app managed to secure before a failure. It is read
// by the error reporter, so a dialog can name the log file and the data
// directory the user cannot otherwise guess.
var runtimePaths struct {
	DataDir  string
	LogPath  string
	Rejected []dataDirChoice
}

// setupLogging sends slog output to a file in the data directory, and to
// stdout when one is attached.
//
// Why a file is mandatory: the desktop binary is built with -H windowsgui, so it
// has NO console — os.Stdout is a dead handle and every slog.Error went nowhere.
// That is the whole reason "it doesn't open" was the only symptom available.
//
// The file is deliberately FIRST in the MultiWriter. io.MultiWriter stops at the
// first writer that errors, and a dead stdout handle errors on every write, so
// putting stdout first would silently discard the record exactly when it is
// most needed.
func setupLogging(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "dockerpull-desktop.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return path, err
	}
	// The file handle lives for the process lifetime; closing it is the
	// process's job.
	slog.SetDefault(slog.New(slog.NewTextHandler(
		writerToUse(f), &slog.HandlerOptions{Level: slog.LevelInfo})))
	return path, nil
}

func writerToUse(f *os.File) writerMulti {
	return writerMulti{f: f, stdout: stdoutUsable()}
}

// writerMulti writes to the log file always, and to stdout when it is real.
type writerMulti struct {
	f      *os.File
	stdout bool
}

func (w writerMulti) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if w.stdout {
		// Best effort: a broken stdout must never lose the file record above.
		_, _ = os.Stdout.Write(p)
	}
	return n, err
}

// stdoutUsable reports whether stdout is a real handle. Under -H windowsgui it
// is not, and writing to it returns an error (or, worse, an invalid-handle
// panic on some Windows versions), so it is checked rather than assumed.
func stdoutUsable() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular() || fi.Mode()&os.ModeCharDevice != 0
}

// recordFatal appends a failure to the log file. It is separate from fatal() so
// the reporting path can be tested without a modal dialog.
func recordFatal(msg string) error {
	if runtimePaths.LogPath == "" {
		return fmt.Errorf("no log path")
	}
	f, err := os.OpenFile(runtimePaths.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintf(f, "[fatal] %s\n", msg)
	return err
}

// fatal reports an unrecoverable startup error through every channel available,
// then exits.
//
// Three channels, cheapest first: the log file (always), slog (harmless when
// stdout is dead), and a native message box (the only one a user double-clicking
// an icon will actually see).
func fatal(context string, err error) {
	msg := fmt.Sprintf("%s: %v", context, err)
	if recErr := recordFatal(msg); recErr != nil {
		// Nothing else to do: the dialog below carries the message.
		_ = recErr
	}
	slog.Error(context, "err", err, "log", runtimePaths.LogPath, "dataDir", runtimePaths.DataDir)
	showFatalDialog("DockerPull 无法启动", buildDialogBody(msg))
	os.Exit(1)
}

// buildDialogBody composes the dialog text: the failure, the two paths a bug
// report needs, and the candidates that were refused. Pure, so it is testable.
func buildDialogBody(msg string) string {
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\n")

	if runtimePaths.DataDir != "" {
		b.WriteString("数据目录: " + runtimePaths.DataDir + "\n")
	}
	if runtimePaths.LogPath != "" {
		b.WriteString("日志文件: " + runtimePaths.LogPath + "\n")
	}
	if rejected := rejectedDirs(runtimePaths.Rejected); len(rejected) > 0 {
		b.WriteString("\n已跳过的数据目录:\n")
		for _, r := range rejected {
			b.WriteString("  " + r + "\n")
		}
		b.WriteString("\n提示: 可用环境变量 APP_DATA_DIR 指定一个可写目录后重试。")
	}
	return b.String()
}
