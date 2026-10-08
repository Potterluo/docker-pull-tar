package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// artifactError writes the right status for an artifact lookup failure: a
// missing row is 404, a row pointing outside our directories is 403, and
// anything else is a genuine server error.
func artifactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w)
	case errors.Is(err, errArtifactPathEscape):
		writeError(w, http.StatusForbidden, "文件路径不在允许的目录内")
	default:
		storeError(w, err)
	}
}

// handleListArtifacts implements F5: the local .tar library.
//
// It reconciles the artifacts table with the output directory first, so
// files dropped in by hand appear and files deleted outside the app
// disappear.
//
//	GET /api/artifacts
func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.tasks.Config(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}

	arts, err := s.tasks.ScanArtifactsIn(r.Context(), cfg.OutputDir)
	if err != nil {
		storeError(w, err)
		return
	}
	if arts == nil {
		arts = []store.Artifact{}
	}

	// Annotate each row with the command the user actually wants.
	type artifactView struct {
		store.Artifact
		LoadCommand string `json:"loadCommand"`
		Exists      bool   `json:"exists"`
	}
	views := make([]artifactView, 0, len(arts))
	for _, a := range arts {
		_, statErr := os.Stat(a.Path)
		views = append(views, artifactView{
			Artifact:    a,
			LoadCommand: "docker load -i " + a.Path,
			Exists:      statErr == nil,
		})
	}
	writeOK(w, map[string]any{"artifacts": views, "outputDir": cfg.OutputDir})
}

// handleDownloadArtifact streams a finished tar to the browser.
//
// The client supplies an artifact ID, never a path: the path comes from the
// store row and is validated against the configured directories before it
// is opened. This is the guard the legacy GUI lacked — there, a delete
// operation joined a client-supplied name onto a directory.
//
//	GET /api/artifacts/{id}
func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	art, err := s.lookupArtifact(r)
	if err != nil {
		artifactError(w, err)
		return
	}

	f, err := os.Open(art.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "文件不存在或已被删除")
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "文件不存在或已被删除")
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", sanitizeDownloadName(art.Name)))
	http.ServeContent(w, r, art.Name, info.ModTime(), f)
}

// handleDeleteArtifact removes a tar from disk and its row from the store.
func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	art, err := s.lookupArtifact(r)
	if err != nil {
		artifactError(w, err)
		return
	}
	if err := os.Remove(art.Path); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "删除文件失败: "+err.Error())
		return
	}
	if err := s.store.DeleteArtifact(r.Context(), art.ID); err != nil {
		storeError(w, err)
		return
	}
	s.hub.Publish(eventOf(tasks.EventArtifactDeleted, map[string]any{"id": art.ID}))
	writeOK(w, map[string]any{"id": art.ID})
}

// errArtifactPathEscape is returned when a stored artifact row points
// outside every directory this tool owns. It is its own error so the handler
// can answer 403 (a foreign path) rather than 500 (our bug).
var errArtifactPathEscape = errors.New("artifact path escapes the allowed directories")

// lookupArtifact resolves a path parameter to a stored row, and checks that
// the row's path still sits inside a directory this tool owns.
//
// The client never supplies a path — it supplies an id, and this function
// turns it into a file. The check matters because a row could have been
// written by an older build, edited by hand, or point at a path that was
// legitimate when the task ran and is not now.
//
// Note what is NOT in the allowed set: the artifact's own parent directory.
// Including it would make the check vacuous (every path is inside its own
// directory) — exactly the hole this function exists to close. A task
// started with an explicit `-o` is therefore trusted only when the stored
// task row corroborates the path; see taskOwnsPath.
func (s *Server) lookupArtifact(r *http.Request) (*store.Artifact, error) {
	id := trimPathParam(r.PathValue("id"))
	if id == "" {
		return nil, store.ErrNotFound
	}
	art, err := s.store.GetArtifact(r.Context(), id)
	if err != nil {
		return nil, err
	}

	cfg, err := s.tasks.Config(r.Context())
	if err != nil {
		return nil, err
	}

	allowed := []string{cfg.OutputDir, s.tasks.DataDir()}
	if dir := s.taskOwnsPath(r, art); dir != "" {
		allowed = append(allowed, dir)
	}
	if err := tasks.ValidateArtifactPath(art.Path, allowed...); err != nil {
		return nil, errArtifactPathEscape
	}
	return art, nil
}

// taskOwnsPath returns the directory of the task that produced this
// artifact, when a task row corroborates the path. An artifact discovered by
// scanning has no task id and is only ever accepted from the configured
// output directory.
func (s *Server) taskOwnsPath(r *http.Request, art *store.Artifact) string {
	if art.TaskID == "" {
		return ""
	}
	task, err := s.store.GetTask(r.Context(), art.TaskID)
	if err != nil {
		return ""
	}
	if !samePath(task.TarPath, art.Path) {
		return ""
	}
	return filepath.Dir(art.Path)
}

// samePath compares two paths for identity, tolerating case differences on
// case-insensitive filesystems.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// sanitizeDownloadName produces a header-safe file name. Control
// characters and quotes would let a crafted name break the
// Content-Disposition header.
func sanitizeDownloadName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "image.tar"
	}
	return out
}

// sortedArtifacts is a small helper used by tests and the dashboard.
func sortedArtifacts(in []store.Artifact) []store.Artifact {
	out := append([]store.Artifact(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
