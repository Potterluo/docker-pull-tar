package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/config"
	"github.com/Potterluo/docker-pull-tar/internal/events"
	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// server_test.go drives the REAL handler — the full route table, the real
// middleware chain and a real SQLite store — through httptest. Testing the
// mux rather than calling handler methods directly is what makes these
// assertions meaningful: a route registered under the wrong method or a
// pattern that does not match fails here and nowhere else.

type testServer struct {
	t       *testing.T
	handler http.Handler
	store   store.Store
	tasks   *tasks.Manager
	hub     *events.Hub
	dataDir string
	output  string
	// srv is the real Server, kept so a test can assert on the wiring the
	// handlers actually use (the searcher map) rather than rebuilding it.
	srv *Server
}

// searchers exposes the live search backends the handlers resolve against.
func (ts *testServer) searchers() map[string]registry.Searcher {
	ts.t.Helper()
	return ts.srv.searchers
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	dataDir := t.TempDir()
	outputDir := t.TempDir()

	cfg := &config.Config{
		Port:        0,
		Bind:        "loopback",
		DataDir:     dataDir,
		DBType:      "sqlite",
		AutoMigrate: true,
		LogLevel:    "error",
	}
	st, err := store.New(&store.StorageConfig{Type: "sqlite", AutoMigrate: true}, dataDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hub := events.New()
	mgr := tasks.NewManager(st, hub, dataDir, "test")
	if err := mgr.EnsureConfig(t.Context()); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	// Pin the output directory to a per-test temp dir so nothing writes
	// into the repository.
	if _, err := mgr.SaveConfig(t.Context(), map[string]string{tasks.KeyOutputDir: outputDir}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	srv := New(cfg, st, hub, mgr)
	handler, err := srv.BuildHandler()
	if err != nil {
		t.Fatalf("BuildHandler: %v", err)
	}
	return &testServer{t: t, handler: handler, store: st, tasks: mgr, hub: hub, dataDir: dataDir, output: outputDir, srv: srv}
}

// do issues a request and returns the raw response plus body.
func (ts *testServer) do(method, path string, body any) (*httptest.ResponseRecorder, []byte) {
	ts.t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = strings.NewReader(v)
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				ts.t.Fatalf("marshal body: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

// envelope decodes a response body into the standard envelope.
func (ts *testServer) envelope(method, path string, body any) (int, map[string]any) {
	ts.t.Helper()
	rec, raw := ts.do(method, path, body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		ts.t.Fatalf("%s %s: body is not JSON (%q): %v", method, path, string(raw), err)
	}
	return rec.Code, out
}

func TestHealthProbes(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/healthz", "/livez", "/readyz"} {
		rec, raw := ts.do(http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if strings.TrimSpace(string(raw)) != "ok" {
			t.Errorf("GET %s body = %q, want \"ok\"", path, string(raw))
		}
	}
}

func TestStatusEnvelope(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodGet, "/api/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if env["ok"] != true {
		t.Errorf("ok = %v, want true", env["ok"])
	}
	// The old auth-driven probe reported `configured`; a no-auth build must
	// say so explicitly so an older UI bundle does not gate on it.
	if got, _ := env["auth"].(bool); got {
		t.Errorf("auth = true, want false (single-user tool)")
	}
	if env["dataDir"] == "" {
		t.Errorf("dataDir is empty")
	}
	if env["outputDir"] == "" {
		t.Errorf("outputDir is empty")
	}
}

// TestErrorEnvelopeShape pins the ONE response contract everything on the
// client branches on.
func TestErrorEnvelopeShape(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodGet, "/api/tasks/nope", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if env["ok"] != false {
		t.Errorf("ok = %v, want false", env["ok"])
	}
	if s, _ := env["error"].(string); s == "" {
		t.Errorf("error message is missing from %v", env)
	}
}

func TestSeededSourcesAndSettings(t *testing.T) {
	ts := newTestServer(t)

	code, env := ts.envelope(http.MethodGet, "/api/sources?kind=search", nil)
	if code != http.StatusOK {
		t.Fatalf("sources status = %d", code)
	}
	list, _ := env["sources"].([]any)
	if len(list) < 2 {
		t.Fatalf("seeded search sources = %d, want >= 2", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["id"] == "" {
		t.Errorf("seeded source has no id")
	}
	// No auth means no ownership column — this is a contract assertion, not
	// an implementation detail.
	if _, present := first["userId"]; present {
		t.Errorf("source row carries userId, but there is no auth")
	}

	code, env = ts.envelope(http.MethodGet, "/api/sources?kind=mirror", nil)
	if code != http.StatusOK {
		t.Fatalf("mirrors status = %d", code)
	}
	mirrors, _ := env["sources"].([]any)
	if len(mirrors) < 5 {
		t.Errorf("seeded mirrors = %d, want >= 5", len(mirrors))
	}

	code, env = ts.envelope(http.MethodGet, "/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("settings status = %d", code)
	}
	settings, _ := env["settings"].(map[string]any)
	if settings["verify_tls"] != "true" {
		t.Errorf("verify_tls = %v, want \"true\" (TLS is verified by default)", settings["verify_tls"])
	}

	// An unknown key must be rejected, not stored: a typo that silently
	// does nothing is worse than a 400.
	if code, _ := ts.envelope(http.MethodPut, "/api/settings", map[string]string{"nope": "1"}); code != http.StatusBadRequest {
		t.Errorf("PUT /api/settings with an unknown key = %d, want 400", code)
	}
	// A known key round-trips.
	if code, env := ts.envelope(http.MethodPut, "/api/settings", map[string]string{"workers": "7"}); code != http.StatusOK {
		t.Errorf("PUT /api/settings workers = %d (%v)", code, env)
	} else if _, env := ts.envelope(http.MethodGet, "/api/settings", nil); env["settings"].(map[string]any)["workers"] != "7" {
		t.Errorf("workers did not persist")
	}
}

func TestSettingsValuesAreClamped(t *testing.T) {
	ts := newTestServer(t)
	// A nonsensical worker count must not open thousands of sockets; the
	// resolver clamps it rather than trusting the stored string.
	if code, _ := ts.envelope(http.MethodPut, "/api/settings", map[string]string{"workers": "9999"}); code != http.StatusOK {
		t.Fatalf("PUT workers = %d", code)
	}
	_, env := ts.envelope(http.MethodGet, "/api/settings", nil)
	settings := env["settings"].(map[string]any)
	if settings["workers"] != "32" {
		t.Errorf("workers = %v, want the clamped 32", settings["workers"])
	}
}

func TestCreateTaskValidatesInput(t *testing.T) {
	ts := newTestServer(t)

	cases := []struct {
		name string
		body any
	}{
		{"empty body", ""},
		{"missing image", map[string]any{"image": ""}},
		{"blank image", map[string]any{"image": "   "}},
		{"invalid json", "{not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, env := ts.envelope(http.MethodPost, "/api/tasks", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %v)", code, env)
			}
			if env["ok"] != false {
				t.Errorf("ok = %v, want false", env["ok"])
			}
		})
	}
}

// TestCreateTaskRejectsInvalidUTF8 protects the readJSON rule: invalid bytes
// must be a 400, never silently replaced with U+FFFD and stored.
func TestCreateTaskRejectsInvalidUTF8(t *testing.T) {
	ts := newTestServer(t)
	body := `{"image":"` + string([]byte{0xff, 0xfe}) + `"}`
	code, _ := ts.envelope(http.MethodPost, "/api/tasks", body)
	if code != http.StatusBadRequest {
		t.Errorf("invalid UTF-8 body = %d, want 400", code)
	}
}

// TestTaskLifecycleOnUnknownID checks that a lifecycle verb on a missing
// task is a 404 rather than a 500 or a silent ok.
func TestTaskLifecycleOnUnknownID(t *testing.T) {
	ts := newTestServer(t)
	for _, verb := range []string{"pause", "resume", "cancel", "retry"} {
		code, env := ts.envelope(http.MethodPost, "/api/tasks/missing/"+verb, nil)
		if code != http.StatusNotFound {
			t.Errorf("POST /api/tasks/missing/%s = %d, want 404 (%v)", verb, code, env)
		}
	}
}

// TestTaskRowAndLayersRoundTrip creates a task row directly (no network) and
// asserts the API returns it with its layers, so the /tasks page has
// something to render.
func TestTaskRowAndLayersRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	ctx := t.Context()

	task := &store.Task{
		ID: "task_roundtrip", Ref: "registry-1.docker.io/library/nginx:latest",
		Registry: "registry-1.docker.io", Repository: "library/nginx", Tag: "latest",
		Platform: "linux/amd64", Status: tasks.StatusRunning, WorkDir: ts.dataDir + "/work/x",
		Workers: 4, VerifyTLS: true, TotalBytes: 100, DownloadedBytes: 40, Speed: 1024,
	}
	if err := ts.store.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	layers := []store.TaskLayer{
		{ID: "l1", TaskID: task.ID, Digest: "sha256:aaa", Kind: "config", Name: "Config", Position: 0, Size: 20, Downloaded: 20, Status: "completed"},
		{ID: "l2", TaskID: task.ID, Digest: "sha256:bbb", Kind: "layer", Name: "bbb", Position: 1, Size: 80, Downloaded: 20, Status: "downloading"},
	}
	if err := ts.store.ReplaceTaskLayers(ctx, task.ID, layers); err != nil {
		t.Fatalf("ReplaceTaskLayers: %v", err)
	}

	code, env := ts.envelope(http.MethodGet, "/api/tasks/"+task.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("GET task = %d (%v)", code, env)
	}
	got, _ := env["task"].(map[string]any)
	if got["id"] != task.ID {
		t.Errorf("id = %v, want %v", got["id"], task.ID)
	}
	if got["verifyTls"] != true {
		t.Errorf("verifyTls = %v, want true (positive flag survived the store)", got["verifyTls"])
	}
	gotLayers, _ := got["layers"].([]any)
	if len(gotLayers) != 2 {
		t.Fatalf("layers = %d, want 2", len(gotLayers))
	}
	first, _ := gotLayers[0].(map[string]any)
	if first["kind"] != "config" || first["position"].(float64) != 0 {
		t.Errorf("layer order lost: %v", first)
	}

	// The list endpoint must carry layers too (the page renders progress
	// straight from the list).
	_, env = ts.envelope(http.MethodGet, "/api/tasks", nil)
	listed, _ := env["tasks"].([]any)
	if len(listed) != 1 {
		t.Fatalf("tasks = %d, want 1", len(listed))
	}
	firstTask, _ := listed[0].(map[string]any)
	if l, _ := firstTask["layers"].([]any); len(l) != 2 {
		t.Errorf("list rows are missing their layers")
	}

	// Delete removes it, and it is then a 404.
	if code, _ := ts.envelope(http.MethodDelete, "/api/tasks/"+task.ID, nil); code != http.StatusOK {
		t.Errorf("DELETE task = %d", code)
	}
	if code, _ := ts.envelope(http.MethodGet, "/api/tasks/"+task.ID, nil); code != http.StatusNotFound {
		t.Errorf("deleted task read = %d, want 404", code)
	}
}

// TestArtifactPathIsNeverClientSupplied is the security assertion for F5.
//
// Two properties:
//  1. The route takes an artifact ID, not a path. An unknown id is a 404 and
//     never touches the filesystem. (Note: a raw "../.." in the URL is
//     cleaned and redirected by net/http's ServeMux before it reaches us,
//     so the interesting case is a row whose stored path is foreign.)
//  2. Even a row that EXISTS is refused when its stored path lies outside
//     the directories this tool owns — the check that the legacy Gradio UI's
//     delete path was missing.
func TestArtifactPathIsNeverClientSupplied(t *testing.T) {
	ts := newTestServer(t)
	ctx := t.Context()

	// 1. Unknown ids.
	for _, id := range []string{"does-not-exist", "..%2f..%2fapp.db", "%2e%2e%2fapp.db"} {
		code, env := ts.envelope(http.MethodGet, "/api/artifacts/"+id, nil)
		if code != http.StatusNotFound {
			t.Errorf("GET /api/artifacts/%s = %d, want 404 (%v)", id, code, env)
		}
	}

	// 2. A row pointing outside every owned directory.
	foreignDir := t.TempDir()
	foreignPath := filepath.Join(foreignDir, "secret.tar")
	if err := os.WriteFile(foreignPath, []byte("not yours"), 0o644); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}
	row := &store.Artifact{
		ID: "art_foreign", Name: "secret.tar", Path: foreignPath, Size: int64(len("not yours")),
	}
	if err := ts.store.CreateArtifact(ctx, row); err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	code, env := ts.envelope(http.MethodGet, "/api/artifacts/"+row.ID, nil)
	if code != http.StatusForbidden {
		t.Errorf("download of an out-of-tree path = %d, want 403 (%v)", code, env)
	}

	// The delete path must refuse it too, and must leave the file alone.
	code, env = ts.envelope(http.MethodDelete, "/api/artifacts/"+row.ID, nil)
	if code != http.StatusForbidden {
		t.Errorf("delete of an out-of-tree path = %d, want 403 (%v)", code, env)
	}
	if _, err := os.Stat(foreignPath); err != nil {
		t.Errorf("the out-of-tree file was removed: %v", err)
	}
}

// TestArtifactDownloadAndDelete walks the artifact lifecycle with a real
// file: create the tar on disk, reconcile it in, download it, delete it.
func TestArtifactDownloadAndDelete(t *testing.T) {
	ts := newTestServer(t)

	name := "library_nginx_latest_amd64.tar"
	path := filepath.Join(ts.output, name)
	if err := os.WriteFile(path, []byte("fake tar bytes"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	// Reconciliation must discover a file that appeared on disk.
	code, env := ts.envelope(http.MethodGet, "/api/artifacts", nil)
	if code != http.StatusOK {
		t.Fatalf("list artifacts = %d", code)
	}
	arts, _ := env["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("artifacts = %d, want 1 (the scan must find %s)", len(arts), name)
	}
	art, _ := arts[0].(map[string]any)
	id, _ := art["id"].(string)
	if id == "" {
		t.Fatalf("artifact has no id: %v", art)
	}
	if art["loadCommand"] != "docker load -i "+path {
		t.Errorf("loadCommand = %v, want the docker load hint", art["loadCommand"])
	}

	// Download streams the bytes with a tar content type.
	rec, raw := ts.do(http.MethodGet, "/api/artifacts/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download artifact = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-tar" {
		t.Errorf("Content-Type = %q, want application/x-tar", got)
	}
	if string(raw) != "fake tar bytes" {
		t.Errorf("downloaded body = %q", string(raw))
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, name) {
		t.Errorf("Content-Disposition = %q, want it to name %s", cd, name)
	}

	// Delete removes the row AND the file.
	if code, env := ts.envelope(http.MethodDelete, "/api/artifacts/"+id, nil); code != http.StatusOK {
		t.Fatalf("delete artifact = %d (%v)", code, env)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("the file still exists after delete")
	}
	_, env = ts.envelope(http.MethodGet, "/api/artifacts", nil)
	if arts, _ := env["artifacts"].([]any); len(arts) != 0 {
		t.Errorf("artifacts = %d after delete, want 0", len(arts))
	}
}

// TestStatsCountsWhatExists checks the dashboard aggregate.
func TestStatsCountsWhatExists(t *testing.T) {
	ts := newTestServer(t)
	ctx := t.Context()

	for i, status := range []string{tasks.StatusSucceeded, tasks.StatusFailed, tasks.StatusRunning} {
		task := &store.Task{
			ID: "t" + string(rune('a'+i)), Ref: "r", Status: status, TarSize: 100,
		}
		if err := ts.store.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}

	code, env := ts.envelope(http.MethodGet, "/api/stats", nil)
	if code != http.StatusOK {
		t.Fatalf("stats = %d", code)
	}
	if env["tasksTotal"].(float64) != 3 {
		t.Errorf("tasksTotal = %v, want 3", env["tasksTotal"])
	}
	if env["tasksSucceeded"].(float64) != 1 {
		t.Errorf("tasksSucceeded = %v, want 1", env["tasksSucceeded"])
	}
	if env["tasksFailed"].(float64) != 1 {
		t.Errorf("tasksFailed = %v, want 1", env["tasksFailed"])
	}
	if env["tasksRunning"].(float64) != 1 {
		t.Errorf("tasksRunning = %v, want 1", env["tasksRunning"])
	}
}

// safeRecorder is a mutex-guarded ResponseRecorder.
//
// httptest.ResponseRecorder is NOT safe for concurrent use, and an SSE test
// inherently has two goroutines: the handler writing frames and the test
// watching for the first one. Reading the recorder directly from the test
// goroutine while the handler runs is a genuine data race (caught by
// `go test -race`), not a false positive.
type safeRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func newSafeRecorder() *safeRecorder {
	return &safeRecorder{rec: httptest.NewRecorder()}
}

func (s *safeRecorder) Header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Header()
}

func (s *safeRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(b)
}

func (s *safeRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(code)
}

// Flush satisfies http.Flusher, which the SSE handler requires; without it
// the handler would answer 500 instead of streaming.
func (s *safeRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

// contentType reports the header so far, copied under the lock.
func (s *safeRecorder) contentType() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Header().Get("Content-Type")
}

// snapshot copies the recorded response under the lock.
func (s *safeRecorder) snapshot() (int, http.Header, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hdr := s.rec.Header().Clone()
	return s.rec.Code, hdr, s.rec.Body.String()
}

// TestSSEHelloFrame: the UI opens the stream and waits for a frame. Without
// the hello it cannot tell a working stream from a dead one.
//
// The handler deliberately blocks until the request context is cancelled, so
// this test drives it in a goroutine and cancels — which also asserts the
// stream shuts down cleanly instead of leaking a goroutine per reload.
func TestSSEHelloFrame(t *testing.T) {
	ts := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	rec := newSafeRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.handler.ServeHTTP(rec, req)
	}()

	// Wait for the handler to claim the response as an event stream, which
	// happens before it writes (and flushes) the hello frame.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec.contentType() == "text/event-stream" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE handler did not return after the request context was cancelled (goroutine leak)")
	}

	code, hdr, body := rec.snapshot()
	if got := hdr.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "hello") {
		t.Errorf("first frame is not the hello: %q", body)
	}
	// Proxies must not buffer the stream.
	if got := hdr.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want \"no\"", got)
	}
}

// TestSecurityHeaders asserts the middleware still wraps the API.
func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	rec, _ := ts.do(http.MethodGet, "/api/status", nil)
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing security header %s", h)
		}
	}
}

// TestSearchRejectsUnknownSource: a typo in a source id must be a 400, not a
// silent fallback that searches somewhere else and returns plausible results.
func TestSearchRejectsUnknownSource(t *testing.T) {
	ts := newTestServer(t)

	if code, _ := ts.envelope(http.MethodGet, "/api/search?q=", nil); code != http.StatusBadRequest {
		t.Errorf("search without a keyword = %d, want 400", code)
	}
	if code, _ := ts.envelope(http.MethodGet, "/api/search?q=nginx&source=not-a-source", nil); code != http.StatusBadRequest {
		t.Errorf("search with an unknown source = %d, want 400", code)
	}
	if code, _ := ts.envelope(http.MethodGet, "/api/tags?repository=", nil); code != http.StatusBadRequest {
		t.Errorf("tags without a repository = %d, want 400", code)
	}
	if code, _ := ts.envelope(http.MethodGet, "/api/sources?kind=bogus", nil); code != http.StatusBadRequest {
		t.Errorf("sources with a bogus kind = %d, want 400", code)
	}
}

// TestSourceCRUD covers the settings page's data-source editor.
func TestSourceCRUD(t *testing.T) {
	ts := newTestServer(t)

	code, env := ts.envelope(http.MethodPost, "/api/sources", map[string]any{
		"kind": "mirror", "name": "内部仓库", "host": "harbor.internal:5000",
	})
	if code != http.StatusOK {
		t.Fatalf("create source = %d (%v)", code, env)
	}
	created, _ := env["source"].(map[string]any)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("created source has no id: %v", created)
	}

	// A duplicate name for the same kind must be a 409, not a second row.
	if code, _ := ts.envelope(http.MethodPost, "/api/sources", map[string]any{
		"kind": "mirror", "name": "内部仓库", "host": "other.host",
	}); code != http.StatusConflict {
		t.Errorf("duplicate source = %d, want 409", code)
	}

	// Validation: a mirror needs a host, a search source needs a URL.
	if code, _ := ts.envelope(http.MethodPost, "/api/sources", map[string]any{
		"kind": "mirror", "name": "缺地址",
	}); code != http.StatusBadRequest {
		t.Errorf("mirror without host = %d, want 400", code)
	}
	if code, _ := ts.envelope(http.MethodPost, "/api/sources", map[string]any{
		"kind": "search", "name": "缺 URL",
	}); code != http.StatusBadRequest {
		t.Errorf("search source without url = %d, want 400", code)
	}

	if code, env := ts.envelope(http.MethodPut, "/api/sources/"+id, map[string]any{
		"name": "内部仓库 v2", "enabled": false,
	}); code != http.StatusOK {
		t.Fatalf("update source = %d (%v)", code, env)
	} else if src, _ := env["source"].(map[string]any); src["enabled"] != false {
		t.Errorf("enabled = %v, want false", src["enabled"])
	}

	if code, _ := ts.envelope(http.MethodDelete, "/api/sources/"+id, nil); code != http.StatusOK {
		t.Errorf("delete source = %d", code)
	}
	if code, _ := ts.envelope(http.MethodDelete, "/api/sources/"+id, nil); code != http.StatusNotFound {
		t.Errorf("delete a missing source = %d, want 404", code)
	}
}

// TestBuiltinSearchSourceCannotBeDeleted: the built-in id is the fallback a
// fresh install and the CLI resolve, so removing it would break them.
func TestBuiltinSearchSourceCannotBeDeleted(t *testing.T) {
	ts := newTestServer(t)
	_, env := ts.envelope(http.MethodGet, "/api/sources?kind=search", nil)
	list, _ := env["sources"].([]any)
	if len(list) == 0 {
		t.Fatal("no seeded search sources")
	}
	first, _ := list[0].(map[string]any)
	id, _ := first["id"].(string)

	code, body := ts.envelope(http.MethodDelete, "/api/sources/"+id, nil)
	if code != http.StatusConflict {
		t.Errorf("deleting a built-in source = %d, want 409 (%v)", code, body)
	}
}

// TestStatusCarriesConfigured protects the /api/status contract: the field is
// declared by CONTRACT §6 and by the client's Status type, so it must exist in
// the response rather than always being undefined. Nothing reads it today
// (there is nothing left to configure), which is exactly why a test has to.
func TestStatusCarriesConfigured(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodGet, "/api/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	got, ok := env["configured"].(bool)
	if !ok {
		t.Fatalf("configured is %T / absent (%v), want a bool", env["configured"], env["configured"])
	}
	if !got {
		t.Errorf("configured = false; the tool is ready out of the box")
	}
}

// TestDefaultMirrorAcceptsACustomHost protects the 设为默认 button for a
// user-added mirror.
//
// The settings key used to accept only a built-in mirror id, so marking a
// custom row (an internal Harbor, say) as default was stored on the row and
// then silently ignored — the default stayed registry-1.docker.io and the
// user's pull went to the wrong registry. The key now accepts an id OR a host,
// and setDefaultSource records the host.
func TestDefaultMirrorAcceptsACustomHost(t *testing.T) {
	ts := newTestServer(t)

	// Create a custom mirror and mark it default.
	code, env := ts.envelope(http.MethodPost, "/api/sources", map[string]any{
		"kind": "mirror", "name": "内部 Harbor", "host": "harbor.internal:5000", "isDefault": true,
	})
	if code != http.StatusOK {
		t.Fatalf("create custom mirror = %d (%v)", code, env)
	}

	// The setting must now name that host, not the shipped default.
	_, env = ts.envelope(http.MethodGet, "/api/settings", nil)
	settings, _ := env["settings"].(map[string]any)
	got, _ := settings["default_mirror"].(string)
	if got != "harbor.internal:5000" {
		t.Errorf("default_mirror = %q, want the custom host — 设为默认 was ignored", got)
	}

	// And it must survive the resolver, which used to reset any non-built-in
	// value back to "dockerhub".
	cfg, err := ts.tasks.Config(t.Context())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.DefaultMirror != "harbor.internal:5000" {
		t.Errorf("resolved DefaultMirror = %q, want the custom host kept", cfg.DefaultMirror)
	}

	// A bare word that is neither an id nor a host still falls back, so a
	// deleted mirror does not leave the tool pointing at nothing.
	if code, _ := ts.envelope(http.MethodPut, "/api/settings", map[string]string{
		"default_mirror": "mirror-that-was-deleted",
	}); code != http.StatusOK {
		t.Fatalf("PUT default_mirror = %d", code)
	}
	cfg, err = ts.tasks.Config(t.Context())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.DefaultMirror == "mirror-that-was-deleted" {
		t.Errorf("a deleted mirror name was kept as the default")
	}
}

// TestRegistriesCatalogIsServed protects GET /api/registries.
//
// This endpoint is the ONLY place a user learns that ghcr.io, quay.io,
// registry.k8s.io and friends can be pulled from directly — the pull path
// itself accepts any host, so nothing else surfaces them. It is also the only
// thing that tells the UI which registries are searchable by keyword.
func TestRegistriesCatalogIsServed(t *testing.T) {
	ts := newTestServer(t)
	code, env := ts.envelope(http.MethodGet, "/api/registries", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/registries = %d (%v)", code, env)
	}

	raw, ok := env["registries"].([]any)
	if !ok {
		t.Fatalf("registries is %T, want an array", env["registries"])
	}
	if len(raw) < 5 {
		t.Fatalf("catalog has %d entries, want the shipped set", len(raw))
	}

	byHost := map[string]map[string]any{}
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("catalog entry is %T, want an object", item)
		}
		host, _ := entry["host"].(string)
		id, _ := entry["id"].(string)
		if host == "" || id == "" {
			t.Errorf("catalog entry is missing id/host: %v", entry)
		}
		byHost[host] = entry
	}

	// The registries the user explicitly asked for must be there.
	for _, host := range []string{"ghcr.io", "quay.io"} {
		if _, present := byHost[host]; !present {
			t.Errorf("%s is missing from the catalog", host)
		}
	}

	// Docker Hub must be present under its real host, and advertise the
	// default searcher.
	if entry, present := byHost["registry-1.docker.io"]; !present {
		t.Error("registry-1.docker.io is missing from the catalog")
	} else if got, _ := entry["searchId"].(string); got != "dockerhub" {
		t.Errorf("dockerhub searchId = %q, want dockerhub", got)
	}

	// Quay advertises its searcher — and that searcher must actually exist,
	// or the UI would offer a search that always fails.
	if entry, present := byHost["quay.io"]; present {
		if got, _ := entry["searchId"].(string); got != "quay" {
			t.Errorf("quay.io searchId = %q, want quay", got)
		}
	} else {
		t.Error("quay.io is missing from the catalog")
	}

	// A registry with no public search API must say so rather than quietly
	// omitting the field: GHCR is reached by owner/repo.
	if entry, present := byHost["ghcr.io"]; present {
		if searchID, _ := entry["searchId"].(string); searchID != "" {
			t.Errorf("ghcr.io claims searchId %q but GHCR has no search API", searchID)
		}
		if note, _ := entry["note"].(string); note == "" {
			t.Error("ghcr.io should explain how to reach it without a search API")
		}
	}

	// Every advertised searchId must be resolvable through the same path the
	// search handler uses, end to end.
	for _, item := range raw {
		entry := item.(map[string]any)
		searchID, _ := entry["searchId"].(string)
		if searchID == "" {
			continue
		}
		if _, err := registry.PickSearcher(ts.searchers(), searchID); err != nil {
			t.Errorf("catalog advertises searchId %q that cannot be resolved: %v", searchID, err)
		}
	}
}
