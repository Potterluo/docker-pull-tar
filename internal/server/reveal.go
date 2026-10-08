package server

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// handleRevealArtifact opens the artifact's directory in the OS file manager,
// selecting the file where the platform supports it.
//
// This has to be a server-side action: a browser cannot open a local folder, and
// a `file://` link is blocked. That makes the path guard the whole security
// story — the client still sends an ID, never a path, and the path comes from
// the store row and is validated by lookupArtifact against the directories the
// APPLICATION chose (the same guard as download/delete). Without it this
// endpoint would be "run the file manager on any path the caller names".
//
//	POST /api/artifacts/{id}/reveal
func (s *Server) handleRevealArtifact(w http.ResponseWriter, r *http.Request) {
	art, err := s.lookupArtifact(r)
	if err != nil {
		artifactError(w, err)
		return
	}
	// Re-check existence: revealing a row whose file was removed outside the app
	// would open the folder with nothing selected, which reads as "it did
	// nothing".
	if _, err := os.Stat(art.Path); err != nil {
		writeError(w, http.StatusNotFound, "文件不存在或已被删除")
		return
	}

	if err := revealInFileManager(art.Path); err != nil {
		writeError(w, http.StatusInternalServerError, "无法打开文件夹: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"id": art.ID, "path": art.Path})
}

// revealInFileManager opens the containing directory, selecting path if the
// platform's file manager can.
//
// Start (not Run): a file manager is a GUI process that may live as long as the
// user keeps the window open, so waiting for it would hang the request. The
// errors from Start are about whether the launcher could be spawned, which is
// the only thing we can meaningfully report.
func revealInFileManager(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	dir := filepath.Dir(abs)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// /select,<path> highlights the file. explorer.exe is notorious for
		// returning a non-zero exit code even on success, which is another
		// reason the result is not inspected.
		cmd = exec.Command("explorer", "/select,"+abs)
	case "darwin":
		cmd = exec.Command("open", "-R", abs)
	default:
		// No portable "select this file" on Linux; open the directory.
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		// Fall back to the directory itself: some launchers reject the select
		// argument but can still open a folder.
		fallback := exec.Command(fallbackOpener(), dir)
		if ferr := fallback.Start(); ferr != nil {
			return err
		}
		return nil
	}
	// Release the process so it does not become a zombie for the server's
	// lifetime; the file manager keeps running on its own.
	go func() { _ = cmd.Wait() }()
	return nil
}

func fallbackOpener() string {
	if runtime.GOOS == "windows" {
		return "explorer"
	}
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}
