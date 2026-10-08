package tasks

// manager_test.go drives the real Manager against a real migrated SQLite
// store and the in-process fake registry (registrytest). Everything stays
// hermetic: the only network is a httptest server on 127.0.0.1, and every
// file lives under a t.TempDir().
//
// Why this package earns this much attention: internal/tasks is the one
// place both front ends go through — the CLI's `pull` and the GUI's
// POST /api/tasks both call Manager.Start and both read these rows back. A
// bug here is invisible from the engine packages (registry, puller and
// store are all fine) yet shows up as "the GUI says succeeded but there is
// no file", "pause lost my progress" or "the artifact list is empty".
//
// Run the suite with -race: the manager runs real goroutines and publishes
// onto a real hub.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/events"
	"github.com/Potterluo/docker-pull-tar/internal/puller"
	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/registry/registrytest"
	"github.com/Potterluo/docker-pull-tar/internal/store"
)

const (
	// testPollInterval is how often the tests re-read the store while
	// waiting for a background goroutine to reach an observable state.
	// Short enough to catch a real race, long enough not to spin a core.
	testPollInterval = 5 * time.Millisecond
	// testTimeout bounds every wait. A healthy run reaches its target in
	// milliseconds; the bound exists so a broken manager fails the test
	// instead of hanging the suite.
	testTimeout = 30 * time.Second
)

// newTestManager opens a migrated SQLite store in its own temp data dir, a
// hub, and a manager with the output directory pinned to a SECOND temp dir
// so no test can write an artifact into the repository.
//
// Cleanup order matters: cleanups run LIFO, so the store close is
// registered first and the manager shutdown second — the manager must stop
// writing before the store it writes through goes away.
func newTestManager(t *testing.T) (*Manager, store.Store, *events.Hub, string, string) {
	t.Helper()

	dataDir := t.TempDir()
	outputDir := t.TempDir()

	st, err := store.New(&store.StorageConfig{Type: "sqlite", AutoMigrate: true}, dataDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hub := events.New()
	mgr := NewManager(st, hub, dataDir, "test")
	if err := mgr.EnsureConfig(t.Context()); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	if _, err := mgr.SaveConfig(t.Context(), map[string]string{KeyOutputDir: outputDir}); err != nil {
		t.Fatalf("SaveConfig(output_dir): %v", err)
	}
	t.Cleanup(func() {
		// Shutdown takes a context because it waits (briefly) for running
		// jobs to park. The background context keeps it independent of the
		// test's own context, which is already cancelled when cleanups run.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mgr.Shutdown(ctx)
	})
	return mgr, st, hub, dataDir, outputDir
}

// isTerminalStatus reports whether a status is one a task stays in: the
// pollers stop there.
func isTerminalStatus(s string) bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusPaused:
		return true
	default:
		return false
	}
}

// waitFor polls cond until it is true, failing the test at the deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", testTimeout, what)
		}
		time.Sleep(testPollInterval)
	}
}

// getTask reads one task, failing the test if the row is gone.
func getTask(t *testing.T, mgr *Manager, id string) *Task {
	t.Helper()
	task, err := mgr.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return task
}

// waitTask polls until the task reaches a terminal status and returns that
// snapshot. The failure message carries the task's Error so a broken pull
// reports why it broke instead of only that it did.
func waitTask(t *testing.T, mgr *Manager, id string) *Task {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		task, err := mgr.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if isTerminalStatus(task.Status) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s is still %q after %s (downloaded %d/%d, error %q)",
				id, task.Status, testTimeout, task.DownloadedBytes, task.TotalBytes, task.Error)
		}
		time.Sleep(testPollInterval)
	}
}

// getConfig resolves the stored configuration.
func getConfig(t *testing.T, mgr *Manager) Config {
	t.Helper()
	cfg, err := mgr.Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	return cfg
}

// setMaxRetries lowers the retry budget. The puller's default is 10 with
// exponential backoff (2s, 4s, 8s … capped at 60s), so a test that provokes
// a download failure and leaves the default in place would take minutes.
func setMaxRetries(t *testing.T, mgr *Manager, n int) {
	t.Helper()
	if _, err := mgr.SaveConfig(t.Context(), map[string]string{KeyMaxRetries: strconv.Itoa(n)}); err != nil {
		t.Fatalf("SaveConfig(max_retries=%d): %v", n, err)
	}
}

// startMockPull starts one pull against the fake registry over plain HTTP.
//
// Insecure is the TRANSPORT switch (`--insecure` / useHTTP): the mock
// speaks HTTP, so without it the registry client would try TLS and fail.
// Certificate verification is left alone, so the stored `verify_tls`
// setting applies and the task records what was actually in force.
func startMockPull(t *testing.T, mgr *Manager, host, repo, tag string, workers int) *Task {
	t.Helper()
	task, err := mgr.Start(t.Context(), StartSpec{
		Image:    host + "/" + repo + ":" + tag,
		Platform: "linux/amd64",
		Workers:  workers,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("Start(%s/%s:%s): %v", host, repo, tag, err)
	}
	return task
}

// registerWithoutLayer publishes an image's manifest and config blob but NOT
// its layer blob. The pull therefore reaches the layer download and then
// waits (retrying) or fails on that one blob. This is how the tests below
// obtain a pull that is unambiguously still running, or unambiguously
// failing on the layer — with no sleeping and no timing luck involved.
func registerWithoutLayer(reg *registrytest.Registry, img *registrytest.Image) {
	reg.AddManifest(img.Repo, img.Tag, registrytest.MediaTypeManifest, img.ManifestBytes)
	reg.AddBlob(img.ConfigDigest, img.ConfigBytes)
}

// waitArtifact waits for the artifact row for path to appear. It is not
// optional politeness: execute marks the task succeeded BEFORE it records
// the artifact, so reading the table the instant the status flips would race
// the manager and be flaky.
func waitArtifact(t *testing.T, st store.Store, path string) store.Artifact {
	t.Helper()
	var found store.Artifact
	waitFor(t, "the artifact row for "+path, func() bool {
		arts, err := st.ListArtifacts(context.Background())
		if err != nil {
			return false
		}
		for _, a := range arts {
			if filepath.Clean(a.Path) == filepath.Clean(path) {
				found = a
				return true
			}
		}
		return false
	})
	return found
}

// statsInt reads one integer counter out of Manager.Stats.
func statsInt(t *testing.T, stats map[string]any, key string) int {
	t.Helper()
	v, ok := stats[key]
	if !ok {
		t.Fatalf("Stats() has no %q: %v", key, stats)
	}
	n, ok := v.(int)
	if !ok {
		t.Fatalf("Stats()[%q] = %#v (%T), want int", key, v, v)
	}
	return n
}

// artifactNames lists the artifact rows for a failure message.
func artifactNames(arts []store.Artifact) []string {
	out := make([]string, 0, len(arts))
	for _, a := range arts {
		out = append(out, a.Name)
	}
	return out
}

// TestEnsureConfigSeedsEverything protects first-boot state. A fresh install
// starts with an EMPTY store; the Settings page and the mirror picker are
// built from what EnsureConfig writes, so a missing default is a blank field
// in the GUI. EnsureConfig runs on every boot, so it must also be a no-op the
// second time — the (kind, name) uniqueness rule is what keeps the seeded
// sources from multiplying.
func TestEnsureConfigSeedsEverything(t *testing.T) {
	mgr, st, _, dataDir, outputDir := newTestManager(t)
	ctx := t.Context()

	rows, err := st.ListSettings(ctx)
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	have := make(map[string]string, len(rows))
	for _, r := range rows {
		have[r.Key] = r.Value
	}

	// Every setting the package knows must have a value after a boot.
	for _, key := range SettingKeys() {
		if _, ok := have[key]; !ok {
			t.Errorf("EnsureConfig left %q unset: the settings page would render an empty field", key)
		}
	}
	// ...and it must be the shipped default, not something invented.
	for key, want := range ConfigDefaults(dataDir) {
		if key == KeyOutputDir {
			// newTestManager pins output_dir after EnsureConfig wrote the
			// data-dir default; assert the pinned value instead.
			if have[key] != outputDir {
				t.Errorf("stored %s = %q, want the pinned %q", key, have[key], outputDir)
			}
			continue
		}
		if have[key] != want {
			t.Errorf("stored %s = %q, want the default %q", key, have[key], want)
		}
	}

	search, err := st.ListSources(ctx, KindSearch)
	if err != nil {
		t.Fatalf("ListSources(search): %v", err)
	}
	mirrors, err := st.ListSources(ctx, KindMirror)
	if err != nil {
		t.Fatalf("ListSources(mirror): %v", err)
	}
	if len(search) < 2 {
		t.Errorf("seeded %d search sources, want at least 2", len(search))
	}
	if len(mirrors) < 5 {
		t.Errorf("seeded %d mirrors, want at least 5", len(mirrors))
	}

	var searchDefaults []store.Source
	for _, s := range search {
		if s.Kind != KindSearch {
			t.Errorf("a search source has kind %q, want %q", s.Kind, KindSearch)
		}
		if s.Name == "" || s.URL == "" {
			t.Errorf("search source %q is missing a name or url: %+v", s.ID, s)
		}
		if !s.Enabled {
			t.Errorf("seeded search source %q is disabled: the picker would filter it out", s.Name)
		}
		if s.IsDefault {
			searchDefaults = append(searchDefaults, s)
		}
	}
	if len(searchDefaults) != 1 {
		t.Errorf("%d search sources are flagged as the default, want exactly 1", len(searchDefaults))
	} else {
		wantName, wantURL := "", ""
		for _, b := range registry.BuiltinSearchSources {
			if b.ID == registry.DefaultSearchSourceID {
				wantName, wantURL = b.Name, b.URL
			}
		}
		if searchDefaults[0].Name != wantName || searchDefaults[0].URL != wantURL {
			t.Errorf("default search source = %+v, want name %q url %q", searchDefaults[0], wantName, wantURL)
		}
	}

	var mirrorDefaults []store.Source
	for _, m := range mirrors {
		if m.Kind != KindMirror {
			t.Errorf("a mirror source has kind %q, want %q", m.Kind, KindMirror)
		}
		if m.Host == "" {
			t.Errorf("seeded mirror %q has no host: a pull through it would fail", m.Name)
		}
		if !m.Enabled {
			t.Errorf("seeded mirror %q is disabled", m.Name)
		}
		if m.IsDefault {
			mirrorDefaults = append(mirrorDefaults, m)
		}
	}
	if len(mirrorDefaults) != 1 {
		t.Errorf("%d mirrors are flagged as the default, want exactly 1", len(mirrorDefaults))
	} else {
		want := registry.DefaultMirror()
		if mirrorDefaults[0].Host != want.Host || mirrorDefaults[0].Name != want.Name {
			t.Errorf("default mirror = %+v, want host %q name %q", mirrorDefaults[0], want.Host, want.Name)
		}
	}

	// Second boot: identical counts, no duplicate names.
	if err := mgr.EnsureConfig(ctx); err != nil {
		t.Fatalf("EnsureConfig (second call): %v", err)
	}
	search2, err := st.ListSources(ctx, KindSearch)
	if err != nil {
		t.Fatalf("ListSources(search) after reseeding: %v", err)
	}
	mirrors2, err := st.ListSources(ctx, KindMirror)
	if err != nil {
		t.Fatalf("ListSources(mirror) after reseeding: %v", err)
	}
	if len(search2) != len(search) {
		t.Errorf("the second EnsureConfig changed the search source count: %d → %d", len(search), len(search2))
	}
	if len(mirrors2) != len(mirrors) {
		t.Errorf("the second EnsureConfig changed the mirror count: %d → %d", len(mirrors), len(mirrors2))
	}
	for _, set := range [][]store.Source{search2, mirrors2} {
		seen := make(map[string]bool, len(set))
		for _, s := range set {
			if seen[s.Name] {
				t.Errorf("duplicate %s source %q after re-seeding", s.Kind, s.Name)
			}
			seen[s.Name] = true
		}
	}
	rows2, err := st.ListSettings(ctx)
	if err != nil {
		t.Fatalf("ListSettings after reseeding: %v", err)
	}
	if len(rows2) != len(rows) {
		t.Errorf("the second EnsureConfig changed the setting count: %d → %d", len(rows), len(rows2))
	}
}

// TestResolveConfigFromStore walks the store → resolver path end to end:
// what SaveConfig writes is what the next pull reads. Every key is
// exercised, because a key that round-trips through ResolveConfig but is
// never read back from the store is a setting the GUI can save and the
// manager quietly ignores.
func TestResolveConfigFromStore(t *testing.T) {
	mgr, _, _, dataDir, outputDir := newTestManager(t)
	ctx := t.Context()

	want := distinctSettingValues(dataDir)
	want[KeyOutputDir] = outputDir // newTestManager pinned it already

	if _, err := mgr.SaveConfig(ctx, want); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	cfg := getConfig(t, mgr)
	for _, key := range SettingKeys() {
		if got := settingValue(cfg, key); got != want[key] {
			t.Errorf("stored %s read back as %q, want %q", key, got, want[key])
		}
	}
}

// TestConfigRoundTrip protects the write path's two contracts: an unknown
// key is rejected (so a typo surfaces immediately instead of being stored
// where nothing reads it), and a rejected patch is not partially applied.
// The blank-value case is the GUI's "clear this field" gesture.
func TestConfigRoundTrip(t *testing.T) {
	mgr, st, _, _, outputDir := newTestManager(t)
	ctx := t.Context()

	t.Run("an unknown key is rejected and nothing is written", func(t *testing.T) {
		for _, key := range []string{"workerss", "output-dir", "Workers", "default_search_sources"} {
			if _, err := mgr.SaveConfig(ctx, map[string]string{key: "1"}); err == nil {
				t.Errorf("SaveConfig({%q: 1}) = nil error, want a rejection", key)
			} else if !strings.Contains(err.Error(), key) {
				t.Errorf("SaveConfig({%q: 1}) error = %q, want it to name the key", key, err)
			}
		}
		// A patch is validated as a whole before any of it is stored.
		if _, err := mgr.SaveConfig(ctx, map[string]string{KeyWorkers: "9", "not_a_setting": "x"}); err == nil {
			t.Error("SaveConfig with one bad key = nil error, want a rejection")
		}
		rows, err := st.ListSettings(ctx)
		if err != nil {
			t.Fatalf("ListSettings: %v", err)
		}
		for _, r := range rows {
			if r.Key == "not_a_setting" || r.Key == "workerss" {
				t.Errorf("a rejected key was stored anyway: %s = %q", r.Key, r.Value)
			}
			if r.Key == KeyWorkers && r.Value == "9" {
				t.Error("a rejected patch was partially applied: workers was written")
			}
		}
	})

	t.Run("a stored value reaches Config", func(t *testing.T) {
		cfg, err := mgr.SaveConfig(ctx, map[string]string{KeyWorkers: "8"})
		if err != nil {
			t.Fatalf("SaveConfig(workers=8): %v", err)
		}
		if cfg.Workers != 8 {
			t.Errorf("SaveConfig returned workers = %d, want 8", cfg.Workers)
		}
		if got := getConfig(t, mgr); got.Workers != 8 {
			t.Errorf("Config().Workers = %d, want 8", got.Workers)
		}
		if got := getConfig(t, mgr); got.OutputDir != outputDir {
			t.Errorf("Config().OutputDir = %q, want the pinned %q", got.OutputDir, outputDir)
		}
	})

	t.Run("a blank value means use the default", func(t *testing.T) {
		if _, err := mgr.SaveConfig(ctx, map[string]string{KeyWorkers: "   ", KeyVerifyTLS: ""}); err != nil {
			t.Fatalf("SaveConfig(blank): %v", err)
		}
		cfg := getConfig(t, mgr)
		if cfg.Workers != 4 {
			t.Errorf("blank workers resolved to %d, want the default 4 (not a clamp to 1)", cfg.Workers)
		}
		if !cfg.VerifyTLS {
			t.Error("blank verify_tls resolved to false, want the default true")
		}
	})
}

// TestStartRejectsBadImage protects the first thing every pull does: parse
// the user's string. A rejected request must leave the store exactly as it
// was — a task row (and a work directory) for an image that was never even
// understood would show up on the /tasks page as a permanently broken job.
func TestStartRejectsBadImage(t *testing.T) {
	cases := []struct {
		name  string
		image string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"digest marker with no digest", "nginx@"},
		{"digest without an algorithm", "nginx@sha256"},
		{"host with no repository", "harbor.abc.com/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, st, _, dataDir, _ := newTestManager(t)

			if _, err := mgr.Start(t.Context(), StartSpec{Image: tc.image, Insecure: true}); err == nil {
				t.Fatalf("Start(%q) = nil error, want a rejection", tc.image)
			}

			rows, err := st.ListTasks(t.Context(), 0)
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("Start(%q) left %d task rows behind: %+v", tc.image, len(rows), rows)
			}
			tasks, err := mgr.List(t.Context(), 0)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(tasks) != 0 {
				t.Errorf("List() reports %d tasks after a rejected Start(%q)", len(tasks), tc.image)
			}
			// The work directory is the other durable side effect: it must
			// not be created for a request that never became a job.
			if entries, err := os.ReadDir(filepath.Join(dataDir, "work")); err == nil && len(entries) != 0 {
				t.Errorf("a rejected Start left %d work directories behind", len(entries))
			}
		})
	}
}

// TestSuccessfulPullRecordsEverything is the end-to-end happy path, and the
// only test that proves all five store slices agree after a real download:
// the task row, its layer rows, the tar on disk, the artifact index and the
// dashboard counters. Every one of those is rendered by a different GUI
// page, so a mismatch here is a page that lies.
func TestSuccessfulPullRecordsEverything(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "hello from the mock registry")
	reg.RegisterImage(img)

	mgr, st, _, _, outputDir := newTestManager(t)
	task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)

	final := waitTask(t, mgr, task.ID)
	if final.Status != StatusSucceeded {
		t.Fatalf("task ended %q (error %q), want %q", final.Status, final.Error, StatusSucceeded)
	}
	if final.Error != "" {
		t.Errorf("succeeded task carries error %q, want none", final.Error)
	}

	// The resolved reference is persisted, because resume/retry re-parse it
	// instead of trusting the user's original string a second time.
	if want := reg.Host() + "/library/nginx:latest"; final.Ref != want {
		t.Errorf("Ref = %q, want %q", final.Ref, want)
	}
	if final.Registry != reg.Host() {
		t.Errorf("Registry = %q, want the mock host %q", final.Registry, reg.Host())
	}
	if final.Repository != "library/nginx" {
		t.Errorf("Repository = %q, want %q", final.Repository, "library/nginx")
	}
	if final.Tag != "latest" {
		t.Errorf("Tag = %q, want %q", final.Tag, "latest")
	}
	if final.Platform != "linux/amd64" {
		t.Errorf("Platform = %q, want %q", final.Platform, "linux/amd64")
	}
	if !final.Insecure {
		t.Error("Insecure = false: the plain-HTTP transport choice must survive on the row for a resume")
	}
	if !final.VerifyTLS {
		t.Error("VerifyTLS = false: certificates must stay verified (the default) on this pull")
	}
	if final.StartedAt.IsZero() || final.FinishedAt.IsZero() {
		t.Errorf("timestamps not recorded: started %v finished %v", final.StartedAt, final.FinishedAt)
	}
	if final.FinishedAt.Before(final.StartedAt) {
		t.Errorf("FinishedAt %v is before StartedAt %v", final.FinishedAt, final.StartedAt)
	}

	// The tar exists and the recorded size is the file's real size.
	if final.TarPath == "" {
		t.Fatal("TarPath is empty on a succeeded task: the artifact is unreachable")
	}
	if filepath.Dir(final.TarPath) != outputDir {
		t.Errorf("TarPath %q is not inside the pinned output dir %q", final.TarPath, outputDir)
	}
	info, err := os.Stat(final.TarPath)
	if err != nil {
		t.Fatalf("stat %s: %v", final.TarPath, err)
	}
	if info.Size() != final.TarSize || final.TarSize <= 0 {
		t.Errorf("TarSize = %d, file is %d bytes (want a positive, matching size)", final.TarSize, info.Size())
	}
	if final.DownloadedBytes != final.TotalBytes || final.DownloadedBytes <= 0 {
		t.Errorf("succeeded task has downloaded=%d total=%d, want both equal and positive",
			final.DownloadedBytes, final.TotalBytes)
	}

	// One config blob and one layer, both settled with a known size, in
	// manifest order — that order is what the UI stacks.
	configs, layers := 0, 0
	for i, l := range final.Layers {
		if l.Position != i {
			t.Errorf("layer %d (%s) has Position %d, want %d", i, l.Digest, l.Position, i)
		}
		if l.Size <= 0 {
			t.Errorf("layer %s has Size %d, want the size from the manifest", l.Digest, l.Size)
		}
		if l.Status != puller.LayerCompleted && l.Status != puller.LayerSkipped {
			t.Errorf("layer %s ended %q, want completed or skipped", l.Digest, l.Status)
		}
		if l.Downloaded != l.Size {
			t.Errorf("layer %s recorded %d of %d bytes", l.Digest, l.Downloaded, l.Size)
		}
		switch l.Kind {
		case puller.KindConfig:
			configs++
		case puller.KindLayer:
			layers++
		default:
			t.Errorf("layer %s has an unknown kind %q", l.Digest, l.Kind)
		}
	}
	if configs != 1 || layers != 1 {
		t.Errorf("task has %d config blobs and %d layers, want 1 and 1", configs, layers)
	}

	// The artifact index is what the /artifacts page and the download
	// endpoint read; its metadata comes from the task row, not from parsing
	// the file name.
	art := waitArtifact(t, st, final.TarPath)
	if art.ID == "" {
		t.Fatal("artifact row has no id")
	}
	if art.Size != final.TarSize || art.Size <= 0 {
		t.Errorf("artifact size = %d, want the tar size %d", art.Size, final.TarSize)
	}
	if art.Repository != "library/nginx" || art.Tag != "latest" || art.Platform != "linux/amd64" {
		t.Errorf("artifact metadata = %s:%s (%s), want library/nginx:latest (linux/amd64)",
			art.Repository, art.Tag, art.Platform)
	}
	if art.TaskID != task.ID {
		t.Errorf("artifact task id = %q, want %q", art.TaskID, task.ID)
	}

	stats, err := mgr.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got := statsInt(t, stats, "tasksTotal"); got != 1 {
		t.Errorf("tasksTotal = %d, want 1", got)
	}
	if got := statsInt(t, stats, "tasksSucceeded"); got != 1 {
		t.Errorf("tasksSucceeded = %d, want 1", got)
	}
	if got := statsInt(t, stats, "tasksFailed"); got != 0 {
		t.Errorf("tasksFailed = %d, want 0", got)
	}
	if got := statsInt(t, stats, "artifactsTotal"); got != 1 {
		t.Errorf("artifactsTotal = %d, want 1", got)
	}

	// The path guard the download/delete endpoints run before touching a
	// file must accept the artifact this pull just produced.
	if err := ValidateArtifactPath(art.Path, outputDir); err != nil {
		t.Errorf("ValidateArtifactPath(%q, %q) = %v, want nil", art.Path, outputDir, err)
	}
}

// TestProgressIsPersistedDuringDownload protects the "live progress" claim:
// while a pull runs, the store row (not just the event stream) must show the
// running status, the per-layer state and the byte counters — the GUI's
// /tasks page reads the store, and a browser that connects halfway through a
// download has no events to replay.
//
// Two subtests, because the two halves need different evidence:
//
//   - a running download is observed deterministically by withholding one
//     blob, so the task cannot finish before the test samples it;
//   - the byte counters are checked on a real, successful pull. Such a pull
//     over loopback can finish between two samples, in which case the only
//     honest assertion is that the FINAL counters advanced (which the
//     manager writes from the puller's result) — the subtest logs which
//     branch it took instead of pretending the timing never happens.
func TestProgressIsPersistedDuringDownload(t *testing.T) {
	t.Run("a running download is visible in the store", func(t *testing.T) {
		reg := registrytest.New(t, registrytest.Options{})
		img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "hello")
		registerWithoutLayer(reg, img)

		mgr, _, _, _, _ := newTestManager(t)
		// The retry window is what keeps the task running while the test
		// samples it; three retries is ~14s of backoff, and the test acts
		// within milliseconds.
		setMaxRetries(t, mgr, 3)
		task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)

		var sampled *Task
		waitFor(t, "the store to report a running task with a downloading layer", func() bool {
			got, err := mgr.Get(context.Background(), task.ID)
			if err != nil {
				return false
			}
			sampled = got
			if got.Status != StatusRunning {
				return false
			}
			for _, l := range got.Layers {
				if l.Status == puller.LayerDownloading {
					return true
				}
			}
			return false
		})

		// The layer rows were written by the store sink while the pull was
		// still in flight, in manifest order with the config blob first.
		if len(sampled.Layers) < 2 {
			t.Fatalf("store shows %d layers mid-pull, want the config blob and the layer", len(sampled.Layers))
		}
		if sampled.Layers[0].Kind != puller.KindConfig {
			t.Errorf("first layer kind = %q, want %q (the puller's work list puts the config blob first)",
				sampled.Layers[0].Kind, puller.KindConfig)
		}
		for i := range sampled.Layers {
			if sampled.Layers[i].Position != i {
				t.Errorf("layer %d (%s) has Position %d, want %d — the UI stacks layers in Position order",
					i, sampled.Layers[i].Digest, sampled.Layers[i].Position, i)
			}
		}
		if sampled.Layers[0].Size <= 0 {
			t.Errorf("mid-pull layer %s has Size %d, want the manifest size", sampled.Layers[0].Digest, sampled.Layers[0].Size)
		}
		if sampled.UpdatedAt.IsZero() {
			t.Error("mid-pull task row has no UpdatedAt: the UI cannot tell how fresh a progress bar is")
		}

		// Park it so the test does not sit through the retry window.
		if err := mgr.Pause(context.Background(), task.ID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if final := waitTask(t, mgr, task.ID); final.Status != StatusPaused {
			t.Errorf("paused task ended %q, want %q", final.Status, StatusPaused)
		}
	})

	t.Run("the byte counters advance during a successful pull", func(t *testing.T) {
		reg := registrytest.New(t, registrytest.Options{})
		img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "hello again")
		reg.RegisterImage(img)

		mgr, _, _, _, _ := newTestManager(t)
		task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)

		var (
			maxSeen        int64
			sawDownloading bool
		)
		var final *Task
		waitFor(t, "the pull to finish", func() bool {
			got, err := mgr.Get(context.Background(), task.ID)
			if err != nil {
				return false
			}
			if got.DownloadedBytes > maxSeen {
				maxSeen = got.DownloadedBytes
			}
			for _, l := range got.Layers {
				if l.Status == puller.LayerDownloading {
					sawDownloading = true
				}
			}
			final = got
			return isTerminalStatus(got.Status)
		})

		if final.Status != StatusSucceeded {
			t.Fatalf("task ended %q (error %q), want %q", final.Status, final.Error, StatusSucceeded)
		}
		if maxSeen == 0 && !sawDownloading {
			// Branch taken: the pull finished between two samples. Over
			// loopback a two-blob image takes a few milliseconds, so this is
			// expected sometimes; the deterministic proof that the sink
			// writes mid-pull is the subtest above.
			t.Logf("the pull finished between samples (never observed a live state); asserting on the final counters only")
		} else {
			t.Logf("observed live progress: max downloaded %d bytes, downloading layer seen: %v", maxSeen, sawDownloading)
		}
		if final.DownloadedBytes <= 0 {
			t.Error("final DownloadedBytes = 0 after a successful pull: the completion is not reflected in the counters")
		}
		if final.DownloadedBytes != final.TotalBytes {
			t.Errorf("final counters are inconsistent: downloaded %d, total %d", final.DownloadedBytes, final.TotalBytes)
		}
		var layerBytes int64
		for _, l := range final.Layers {
			layerBytes += l.Size
		}
		if layerBytes <= 0 {
			t.Error("no layer rows carry a size after a successful pull")
		}
	})
}

// TestEventsArePublished protects the SSE contract the GUI is built on: the
// pages refresh because these events arrive, not because they poll. It also
// exercises the manager's locking, since publish is called from the puller's
// sink goroutines and from the finalizer — run this with -race.
func TestEventsArePublished(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "event payload")
	reg.RegisterImage(img)

	mgr, _, hub, _, _ := newTestManager(t)

	// Two subscribers, both drained continuously. The hub's per-subscriber
	// buffer is 16 and a small pull publishes more than a dozen events; a
	// reader that lags would silently drop them, and a test that then fails
	// on "artifact.created never arrived" would be testing the scheduler.
	// Ordered delivery per subscriber means one of the two always has it.
	var (
		mu   sync.Mutex
		seen []string
	)
	subs := []*events.Subscriber{hub.Subscribe("test"), hub.Subscribe("test")}
	for _, sub := range subs {
		sub := sub
		t.Cleanup(func() { hub.Unsubscribe(sub) })
		go func() {
			for evt := range sub.Ch {
				mu.Lock()
				seen = append(seen, evt.Type)
				mu.Unlock()
			}
		}()
	}

	hasEvent := func(eventType string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range seen {
			if e == eventType {
				return true
			}
		}
		return false
	}

	task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)

	final := waitTask(t, mgr, task.ID)
	if final.Status != StatusSucceeded {
		t.Fatalf("task ended %q (error %q), want %q", final.Status, final.Error, StatusSucceeded)
	}
	// task.finished is published last, so waiting for it means everything
	// published for this task has been delivered to the subscribers.
	waitFor(t, EventTaskFinished+" to reach the subscriber", func() bool { return hasEvent(EventTaskFinished) })

	if !hasEvent(EventTaskCreated) {
		t.Errorf("no %s event: the /tasks page would not learn about a CLI-started download", EventTaskCreated)
	}
	if !hasEvent(EventTaskUpdated) && !hasEvent(EventTaskProgress) {
		t.Errorf("neither %s nor %s arrived: a task's progress would never refresh", EventTaskUpdated, EventTaskProgress)
	}
	if !hasEvent(EventArtifactCreated) {
		t.Errorf("no %s event: the /artifacts page would not show the new tar", EventArtifactCreated)
	}

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	t.Logf("published %d events: %v", len(got), got)

	// A mutation the manager makes must never reach another task's events:
	// the payload of task.created is the task itself.
	if hasEvent(EventArtifactDeleted) {
		t.Error("an artifact.deleted event was published for a successful pull")
	}
}

// TestTaskUpdatedCarriesTheWholeTask is the regression test for a bug the
// adversarial review found, and the one most likely to reach a user.
//
// storeSink.OnState used to publish task.updated as a five-field map
// ({id,status,downloadedBytes,totalBytes,layers}). CONTRACT §5 and api.ts both
// declare it as a complete Task, and the /tasks page REPLACES the row when it
// arrives — so every layer transition blanked out the running task's
// 镜像 / 架构 / 创建时间 columns until the next 2-second listTasks poll
// restored them.
//
// Any partial payload fails this: the assertion is that the fields the page
// renders from the event are present and correct.
func TestTaskUpdatedCarriesTheWholeTask(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "full payload")
	reg.RegisterImage(img)

	mgr, _, hub, _, _ := newTestManager(t)

	type frame struct {
		typ  string
		data any
	}
	var (
		mu     sync.Mutex
		frames []frame
	)
	sub := hub.Subscribe("test")
	t.Cleanup(func() { hub.Unsubscribe(sub) })
	go func() {
		for evt := range sub.Ch {
			mu.Lock()
			frames = append(frames, frame{typ: evt.Type, data: evt.Data})
			mu.Unlock()
		}
	}()

	task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)
	final := waitTask(t, mgr, task.ID)
	if final.Status != StatusSucceeded {
		t.Fatalf("task ended %q (error %q)", final.Status, final.Error)
	}
	waitFor(t, EventTaskUpdated+" to arrive", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range frames {
			if f.typ == EventTaskUpdated {
				return true
			}
		}
		return false
	})

	mu.Lock()
	defer mu.Unlock()
	var (
		checked    int
		withLayers int
	)
	for _, f := range frames {
		if f.typ != EventTaskUpdated {
			continue
		}
		got, ok := f.data.(*Task)
		if !ok {
			t.Fatalf("task.updated payload is %T, want *Task — the client replaces the row with it, "+
				"so a partial payload blanks out 镜像/架构/创建时间", f.data)
		}
		// The identity fields the /tasks row renders from the event. Every
		// frame must carry them, including the first one, which is published
		// as soon as the task flips to running.
		if got.Ref == "" || got.Platform == "" || got.CreatedAt.IsZero() {
			t.Errorf("task.updated payload is missing identity fields: ref=%q platform=%q createdAt=%v",
				got.Ref, got.Platform, got.CreatedAt)
		}
		if len(got.Layers) > 0 {
			withLayers++
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no task.updated frame was captured")
	}
	// At least one frame must describe the layers, or the per-layer bars
	// would only ever come from the 2-second polling fallback. (The frame
	// published the instant the task starts legitimately has none yet: the
	// manifest has not been resolved, so the image's layers are unknown.)
	if withLayers == 0 {
		t.Errorf("none of the %d task.updated frames carried layers: the per-layer progress bars would stay empty", checked)
	}
}

// TestFailedPullIsRecorded protects the failure path: the row must say
// FAILED, carry the reason (the GUI shows it), keep no artifact, and leave a
// layer row that explains which blob broke. Without this the /tasks page
// would show a pull that silently never produces a file.
func TestFailedPullIsRecorded(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "never arrives")
	// The manifest and the config blob are served; the LAYER blob is not.
	// That makes the failure unambiguous: with a DenyBlobs registry the
	// config blob fails too, and which error the pool reports first is a
	// race between two workers.
	registerWithoutLayer(reg, img)

	mgr, st, _, _, _ := newTestManager(t)
	// One retry = one 2s backoff. The puller's default of 10 would take
	// minutes of exponential backoff.
	setMaxRetries(t, mgr, 1)

	task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)
	final := waitTask(t, mgr, task.ID)

	if final.Status != StatusFailed {
		t.Fatalf("task ended %q (error %q), want %q", final.Status, final.Error, StatusFailed)
	}
	if final.Error == "" {
		t.Fatal("a failed task has an empty Error: the GUI would show a silent failure")
	}
	// The message must name the blob, not just say "download failed".
	short := strings.TrimPrefix(img.LayerDigest, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	if !strings.Contains(final.Error, short) {
		t.Errorf("task error %q does not name the failing layer %s", final.Error, short)
	}
	if final.TarPath != "" {
		t.Errorf("failed task recorded a TarPath %q, want none", final.TarPath)
	}
	if final.FinishedAt.IsZero() {
		t.Error("failed task has no FinishedAt")
	}

	// The layer row keeps the reason a single blob broke.
	found := false
	for _, l := range final.Layers {
		if l.Kind != puller.KindLayer {
			continue
		}
		found = true
		if l.Status != puller.LayerFailed {
			t.Errorf("the failed layer's status = %q, want %q", l.Status, puller.LayerFailed)
		}
		if l.Error == "" {
			t.Error("the failed layer row has an empty Error: the per-layer detail is lost")
		}
	}
	if !found {
		t.Fatal("the failed task has no layer row: the UI cannot show what failed")
	}

	arts, err := st.ListArtifacts(t.Context())
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 0 {
		t.Errorf("a failed pull created %d artifact rows: %v", len(arts), artifactNames(arts))
	}
	stats, err := mgr.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got := statsInt(t, stats, "tasksFailed"); got != 1 {
		t.Errorf("tasksFailed = %d, want 1", got)
	}
	if got := statsInt(t, stats, "artifactsTotal"); got != 0 {
		t.Errorf("artifactsTotal = %d, want 0 after a failed pull", got)
	}
}

// TestPauseResumeContinuesFromDisk protects 断点续传 end to end, which is the
// feature the whole tool is built around: Pause must leave the working
// directory and its resume ledger in place, and Resume must finish the job
// from that same directory (not restart it somewhere else).
//
// The running state is made deterministic instead of racy: the layer blob is
// withheld, so the pull sits in its retry backoff until the test pauses it.
// After the pause the blob is published and the resume must succeed.
func TestPauseResumeContinuesFromDisk(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "resume payload")
	registerWithoutLayer(reg, img)

	mgr, _, _, _, outputDir := newTestManager(t)
	// A generous retry budget: it only has to outlast the few milliseconds
	// between Start and Pause.
	setMaxRetries(t, mgr, 5)

	task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)
	waitFor(t, "the pull to be running", func() bool {
		got, err := mgr.Get(context.Background(), task.ID)
		return err == nil && got.Status == StatusRunning
	})

	if err := mgr.Pause(context.Background(), task.ID); err != nil {
		t.Fatalf("Pause on a running task: %v", err)
	}
	paused := waitTask(t, mgr, task.ID)
	if paused.Status != StatusPaused {
		t.Fatalf("after Pause the task is %q (error %q), want %q: a pause must not cancel or fail the job",
			paused.Status, paused.Error, StatusPaused)
	}
	if paused.FinishedAt.IsZero() {
		t.Error("paused task has no FinishedAt: the UI cannot tell when it stopped")
	}

	// The resume ledger is the ONLY thing a resume needs, so it must
	// survive a pause.
	if paused.WorkDir == "" {
		t.Fatal("the paused task has no WorkDir: there is nothing to resume from")
	}
	if !dirExists(paused.WorkDir) {
		t.Fatalf("Pause removed the work directory %s: the downloaded bytes are gone", paused.WorkDir)
	}
	ledger := filepath.Join(paused.WorkDir, puller.ProgressFileName)
	if _, err := os.Stat(ledger); err != nil {
		t.Fatalf("the resume ledger %s did not survive the pause: %v", ledger, err)
	}

	// Now let the layer exist: Resume must continue, not start over.
	reg.AddBlob(img.LayerDigest, img.LayerGz)
	if err := mgr.Resume(context.Background(), task.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	final := waitTask(t, mgr, task.ID)
	if final.Status != StatusSucceeded {
		t.Fatalf("after Resume the task is %q (error %q), want %q", final.Status, final.Error, StatusSucceeded)
	}
	if final.WorkDir != paused.WorkDir {
		t.Errorf("Resume moved the task to a different work dir: %q → %q", paused.WorkDir, final.WorkDir)
	}
	info, err := os.Stat(final.TarPath)
	if err != nil {
		t.Fatalf("stat %s: %v", final.TarPath, err)
	}
	if info.Size() != final.TarSize || final.TarSize <= 0 {
		t.Errorf("TarSize = %d, file is %d bytes", final.TarSize, info.Size())
	}
	if filepath.Dir(final.TarPath) != outputDir {
		t.Errorf("TarPath %q is not inside the pinned output dir %q", final.TarPath, outputDir)
	}
	for _, l := range final.Layers {
		if l.Status != puller.LayerCompleted && l.Status != puller.LayerSkipped {
			t.Errorf("layer %s ended %q after the resume, want completed or skipped", l.Digest, l.Status)
		}
	}
}

// TestResumeRejectsSucceededTask protects the deliberate status gate: Resume
// on a succeeded task is refused (the message points the user at 重试),
// because re-running the same download silently would be a surprise. Retry
// is the allowed second attempt — the whole reason the refusal message names
// it — and it must reuse the artifact row for the same path instead of
// adding a duplicate to the /artifacts page.
func TestResumeRejectsSucceededTask(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "retry payload")
	reg.RegisterImage(img)

	mgr, st, _, _, _ := newTestManager(t)
	task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)

	first := waitTask(t, mgr, task.ID)
	if first.Status != StatusSucceeded {
		t.Fatalf("first run ended %q (error %q), want %q", first.Status, first.Error, StatusSucceeded)
	}
	art := waitArtifact(t, st, first.TarPath)
	if art.Path != filepath.Clean(first.TarPath) {
		t.Fatalf("artifact path = %q, want %q", art.Path, filepath.Clean(first.TarPath))
	}

	t.Run("Resume is refused and points at 重试", func(t *testing.T) {
		if err := mgr.Resume(t.Context(), task.ID); err == nil {
			t.Fatal("Resume on a succeeded task = nil, want a refusal")
		} else if !strings.Contains(err.Error(), "重试") {
			t.Errorf("Resume error = %q, want it to point at 重试", err)
		}
		if after := getTask(t, mgr, task.ID); after.Status != StatusSucceeded {
			t.Errorf("a refused Resume changed the status to %q, want %q", after.Status, StatusSucceeded)
		}
	})

	// KNOWN BUG — this subtest is expected to FAIL until
	// manager.go:644-646 is fixed. `restart` refuses a succeeded task for
	// BOTH callers, so Retry (fromScratch=true) is rejected with the very
	// message that tells the user to use Retry, and a succeeded task can
	// never be re-downloaded. The guard needs to be
	// `if !fromScratch && t.Status == StatusSucceeded`.
	t.Run("Retry re-runs a succeeded task without duplicating the artifact", func(t *testing.T) {
		// Retry discards the resume ledger and downloads again from byte zero.
		if err := mgr.Retry(t.Context(), task.ID); err != nil {
			t.Fatalf("Retry on a succeeded task = %v, want it allowed (the refusal message itself says to use 重试)", err)
		}
		second := waitTask(t, mgr, task.ID)
		if second.Status != StatusSucceeded {
			t.Fatalf("the retry ended %q (error %q), want %q", second.Status, second.Error, StatusSucceeded)
		}
		if second.TarPath != first.TarPath {
			t.Errorf("the retry wrote a different path: %q → %q", first.TarPath, second.TarPath)
		}
		arts, err := st.ListArtifacts(t.Context())
		if err != nil {
			t.Fatalf("ListArtifacts: %v", err)
		}
		if len(arts) != 1 {
			t.Errorf("after a retry there are %d artifact rows, want 1: %v", len(arts), artifactNames(arts))
		} else if filepath.Clean(arts[0].Path) != filepath.Clean(second.TarPath) {
			t.Errorf("artifact path = %q, want %q", arts[0].Path, second.TarPath)
		}
		if info, err := os.Stat(second.TarPath); err != nil {
			t.Errorf("the retry's tar is missing: %v", err)
		} else if info.Size() != second.TarSize {
			t.Errorf("TarSize = %d, file is %d bytes", second.TarSize, info.Size())
		}
	})
}

// TestDeleteRemovesRowsAndFiles protects both halves of the delete contract,
// which the GUI exposes as one checkbox: removeFiles=true must take the task
// row, its layers, the tar and the work directory with it, leaving nothing
// for a later scan to resurrect; removeFiles=false must delete the job but
// keep the tar and its index row, because the artifact is the thing the user
// came for.
func TestDeleteRemovesRowsAndFiles(t *testing.T) {
	t.Run("removing files takes the row, the tar and the work dir", func(t *testing.T) {
		reg := registrytest.New(t, registrytest.Options{})
		img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "delete me")
		reg.RegisterImage(img)

		mgr, st, _, _, _ := newTestManager(t)
		task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)
		final := waitTask(t, mgr, task.ID)
		if final.Status != StatusSucceeded {
			t.Fatalf("setup pull ended %q (error %q)", final.Status, final.Error)
		}
		waitArtifact(t, st, final.TarPath)
		tarPath, workDir := final.TarPath, final.WorkDir

		if err := mgr.Delete(t.Context(), task.ID, true); err != nil {
			t.Fatalf("Delete(removeFiles=true): %v", err)
		}

		if _, err := mgr.Get(context.Background(), task.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get after Delete = %v, want store.ErrNotFound", err)
		}
		if _, err := os.Stat(tarPath); !os.IsNotExist(err) {
			t.Errorf("the tar %s is still on disk after Delete(true): err = %v", tarPath, err)
		}
		if _, err := os.Stat(workDir); !os.IsNotExist(err) {
			t.Errorf("the work directory %s survived Delete(true): err = %v", workDir, err)
		}
		arts, err := st.ListArtifacts(context.Background())
		if err != nil {
			t.Fatalf("ListArtifacts: %v", err)
		}
		if len(arts) != 0 {
			t.Errorf("%d artifact rows survived Delete(true): %v", len(arts), artifactNames(arts))
		}
		layers, err := st.ListTaskLayers(context.Background(), task.ID)
		if err != nil {
			t.Fatalf("ListTaskLayers: %v", err)
		}
		if len(layers) != 0 {
			t.Errorf("%d orphan layer rows survived Delete(true): the tables have no cascade", len(layers))
		}
	})

	t.Run("keeping files keeps the tar and its index row", func(t *testing.T) {
		reg := registrytest.New(t, registrytest.Options{})
		img := registrytest.MustBuildImage("library/busybox", "latest", "amd64", "hello.txt", "keep me")
		reg.RegisterImage(img)

		mgr, st, _, _, _ := newTestManager(t)
		task := startMockPull(t, mgr, reg.Host(), "library/busybox", "latest", 1)
		final := waitTask(t, mgr, task.ID)
		if final.Status != StatusSucceeded {
			t.Fatalf("setup pull ended %q (error %q)", final.Status, final.Error)
		}
		art := waitArtifact(t, st, final.TarPath)

		if err := mgr.Delete(t.Context(), task.ID, false); err != nil {
			t.Fatalf("Delete(removeFiles=false): %v", err)
		}
		if _, err := mgr.Get(context.Background(), task.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get after Delete = %v, want store.ErrNotFound", err)
		}
		if _, err := os.Stat(final.TarPath); err != nil {
			t.Errorf("Delete(false) removed the tar: %v", err)
		}
		// The artifact row deliberately stays: the file is still there and
		// still listed on the /artifacts page. Only the job row goes away,
		// which is why its task_id now points at nothing.
		arts, err := st.ListArtifacts(context.Background())
		if err != nil {
			t.Fatalf("ListArtifacts: %v", err)
		}
		if len(arts) != 1 {
			t.Fatalf("%d artifact rows after Delete(false), want 1: %v", len(arts), artifactNames(arts))
		}
		if filepath.Clean(arts[0].Path) != filepath.Clean(art.Path) {
			t.Errorf("artifact path = %q, want %q", arts[0].Path, art.Path)
		}
	})
}

// TestScanArtifactsReconciles protects the artifact index against drift from
// the filesystem: a .tar dropped into the output directory (by an older run,
// a copy, or a manual export) must appear, and a row whose file was deleted
// behind the app's back must not linger on the page. Directories and
// non-.tar files must be ignored.
func TestScanArtifactsReconciles(t *testing.T) {
	mgr, _, _, _, outputDir := newTestManager(t)
	ctx := t.Context()

	sizes := map[string]int{
		"library_nginx_latest_amd64.tar": 4096,
		"a_b_c.tar":                      17,
	}
	for name, size := range sizes {
		if err := os.WriteFile(filepath.Join(outputDir, name), []byte(strings.Repeat("x", size)), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(outputDir, "not-a-tar.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatalf("write not-a-tar.txt: %v", err)
	}
	if err := os.Mkdir(filepath.Join(outputDir, "a-directory.tar"), 0o755); err != nil {
		t.Fatalf("mkdir a-directory.tar: %v", err)
	}

	arts, err := mgr.ScanArtifactsIn(ctx, outputDir)
	if err != nil {
		t.Fatalf("ScanArtifactsIn: %v", err)
	}
	byName := make(map[string]store.Artifact, len(arts))
	for _, a := range arts {
		byName[a.Name] = a
	}
	if len(arts) != 2 {
		t.Fatalf("the scan recorded %d artifacts (%v), want the 2 .tar files", len(arts), artifactNames(arts))
	}
	if _, ok := byName["not-a-tar.txt"]; ok {
		t.Error("a non-.tar file was indexed as an artifact")
	}
	if _, ok := byName["a-directory.tar"]; ok {
		t.Error("a DIRECTORY named *.tar was indexed as an artifact")
	}
	for name, size := range sizes {
		a, ok := byName[name]
		if !ok {
			t.Errorf("%s was not indexed", name)
			continue
		}
		if a.Size != int64(size) {
			t.Errorf("%s size = %d, want %d", name, a.Size, size)
		}
		if want := filepath.Join(outputDir, name); filepath.Clean(a.Path) != filepath.Clean(want) {
			t.Errorf("%s path = %q, want %q", name, a.Path, want)
		}
	}
	// A scanned row has no task behind it, so its metadata comes from the
	// file name. The split is documented as best-effort (repo, tag and arch
	// may all contain "_"), and it takes the LAST two fields as tag+arch:
	// so "library_nginx_latest_amd64.tar" yields repo "library_nginx",
	// tag "latest", platform "amd64" — NOT "library/nginx" and not
	// "linux/amd64". That is the real behaviour; the task-derived columns
	// set by recordArtifact are the authority for pulls made by this app.
	nginx := byName["library_nginx_latest_amd64.tar"]
	if nginx.Tag != "latest" {
		t.Errorf("parsed tag = %q, want %q", nginx.Tag, "latest")
	}
	if nginx.Platform != "amd64" {
		t.Errorf("parsed platform = %q, want %q", nginx.Platform, "amd64")
	}
	if nginx.Repository != "library_nginx" {
		t.Errorf("parsed repository = %q, want the best-effort split %q", nginx.Repository, "library_nginx")
	}
	abc := byName["a_b_c.tar"]
	if abc.Repository != "a" || abc.Tag != "b" || abc.Platform != "c" {
		t.Errorf("a_b_c.tar parsed as %q/%q/%q, want a/b/c", abc.Repository, abc.Tag, abc.Platform)
	}

	// Re-scanning is idempotent: path is the artifact's filesystem identity.
	again, err := mgr.ScanArtifactsIn(ctx, outputDir)
	if err != nil {
		t.Fatalf("second ScanArtifactsIn: %v", err)
	}
	if len(again) != 2 {
		t.Errorf("the second scan produced %d rows, want the same 2: %v", len(again), artifactNames(again))
	}

	// A file deleted behind the app's back loses its row.
	if err := os.Remove(filepath.Join(outputDir, "a_b_c.tar")); err != nil {
		t.Fatalf("remove a_b_c.tar: %v", err)
	}
	after, err := mgr.ScanArtifactsIn(ctx, outputDir)
	if err != nil {
		t.Fatalf("third ScanArtifactsIn: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after deleting one file the scan found %d rows, want 1: %v", len(after), artifactNames(after))
	}
	if after[0].Name != "library_nginx_latest_amd64.tar" {
		t.Errorf("the surviving artifact is %q, want the file that still exists", after[0].Name)
	}
}

// TestValidateArtifactPath protects the guard the server runs before it
// deletes or streams a file: the path is re-validated against the configured
// directories, so a doctored row cannot reach outside them. The
// sibling-directory case is the classic prefix-matching bug — an allowed
// "…\out" must not bless "…\outside\f" — and the empty-entry case protects
// the "no directory configured" path from accidentally allowing everything.
func TestValidateArtifactPath(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "out")
	other := filepath.Join(root, "elsewhere")
	for _, dir := range []string{allowed, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	cases := []struct {
		name    string
		path    string
		allowed []string
		wantErr bool
	}{
		{"a file inside the allowed dir", filepath.Join(allowed, "image.tar"), []string{allowed}, false},
		{"a file in a nested subdir", filepath.Join(allowed, "nested", "deep.tar"), []string{allowed}, false},
		{"the allowed dir itself is accepted", allowed, []string{allowed}, false},
		{"a file outside every allowed dir", filepath.Join(other, "image.tar"), []string{allowed}, true},
		{"a sibling whose name shares a prefix", filepath.Join(root, "outside", "image.tar"), []string{allowed}, true},
		{"a .. escape out of the allowed dir", filepath.Join(allowed, "..", "escape.tar"), []string{allowed}, true},
		{"an empty path", "", []string{allowed}, true},
		{"a dot path", ".", []string{allowed}, true},
		{"a .. path", "..", []string{allowed}, true},
		{"no allowed dirs at all", filepath.Join(allowed, "image.tar"), nil, true},
		{"blank allowed entries are skipped", filepath.Join(allowed, "image.tar"), []string{"", "   ", allowed}, false},
		{"a blank allowed entry grants nothing", filepath.Join(allowed, "image.tar"), []string{""}, true},
		{"the second allowed dir matches", filepath.Join(other, "image.tar"), []string{allowed, other}, false},
		{"a relative path is not inside an absolute dir", "image.tar", []string{allowed}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateArtifactPath(tc.path, tc.allowed...)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateArtifactPath(%q, %q) = nil, want an error", tc.path, tc.allowed)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateArtifactPath(%q, %q) = %v, want nil", tc.path, tc.allowed, err)
			}
		})
	}
}

// TestShutdownParksRunningTasks protects the app's exit path. A download in
// flight when the process shuts down must be PARKED (paused), not canceled:
// a canceled task reads as a user decision, and the whole point of 断点续传
// is that the user can resume what an exit interrupted. Shutdown must also
// come back promptly rather than block the exit.
func TestShutdownParksRunningTasks(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "shutdown payload")
	registerWithoutLayer(reg, img)

	mgr, _, _, _, _ := newTestManager(t)
	// ~14s of retry backoff: the task is guaranteed to still be running
	// when Shutdown is called, milliseconds from now.
	setMaxRetries(t, mgr, 3)

	task := startMockPull(t, mgr, reg.Host(), "library/nginx", "latest", 1)
	waitFor(t, "the pull to be running", func() bool {
		got, err := mgr.Get(context.Background(), task.ID)
		return err == nil && got.Status == StatusRunning
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	mgr.Shutdown(ctx)
	elapsed := time.Since(started)
	if elapsed >= 3*time.Second {
		t.Errorf("Shutdown took %s, want it back before its 3s timeout", elapsed)
	}

	// Shutdown waits for the jobs it stopped, so the row is already final.
	final := getTask(t, mgr, task.ID)
	if final.Status == StatusCanceled {
		t.Errorf("Shutdown left the task %q: an exit must park a download, not cancel it", final.Status)
	}
	if final.Status != StatusPaused {
		t.Errorf("Shutdown left the task %q, want %q (it was running when Shutdown was called)", final.Status, StatusPaused)
	}
	if final.Error != "" {
		t.Errorf("a parked task carries error %q, want none", final.Error)
	}
	// The ledger is what makes the parked task resumable after a restart.
	if _, err := os.Stat(filepath.Join(final.WorkDir, puller.ProgressFileName)); err != nil {
		t.Errorf("the resume ledger did not survive Shutdown: %v", err)
	}
}

// TestStartHonoursSkipVerifyTLS protects the fix for a bug this suite found:
// the certificate switch ignored an explicit "do not verify".
//
// Both callers express that request — cmd/server/cli.go (`--no-verify-tls`)
// and internal/server/handlers_tasks.go (the GUI's "跳过 TLS 校验" checkbox)
// — by setting StartSpec.SkipVerifyTLS. The stored `verify_tls` setting is
// the baseline, and this flag can only turn verification off, never on.
//
// The regression: StartSpec used to carry a positive `VerifyTLS bool` plus a
// `UseStoredTLS bool`, and the resolver's default branch read the STORED
// value, so both switches were silently ignored and certificates kept being
// verified. The field is now negative precisely so the zero value cannot
// weaken TLS.
func TestStartHonoursSkipVerifyTLS(t *testing.T) {
	reg := registrytest.New(t, registrytest.Options{})
	img := registrytest.MustBuildImage("library/nginx", "latest", "amd64", "hello.txt", "tls payload")
	reg.RegisterImage(img)

	mgr, st, _, _, _ := newTestManager(t)
	if cfg := getConfig(t, mgr); !cfg.VerifyTLS {
		t.Fatalf("precondition: stored verify_tls = false, want the shipped default true")
	}

	task, err := mgr.Start(t.Context(), StartSpec{
		Image:    reg.Host() + "/library/nginx:latest",
		Platform: "linux/amd64",
		// The transport flag (plain HTTP), unrelated to certificates.
		Insecure: true,
		// What --no-verify-tls and the GUI's insecure checkbox send.
		SkipVerifyTLS: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	row, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.VerifyTLS {
		t.Errorf("StartSpec.SkipVerifyTLS=true stored VerifyTLS=true: an explicit \"do not verify certificates\" was silently replaced by the stored setting, so `--no-verify-tls` and the GUI's insecure checkbox do nothing")
	}
}

// TestMirrorHostResolvesIDsAndHosts protects the registry picker: MirrorHost
// accepts a mirror id, an arbitrary host the user typed, or nothing at all
// (meaning "the configured default"), and the CLI hands whatever was selected
// straight through. An id that is no longer in the shipped list must be
// treated as a raw host rather than being dropped.
func TestMirrorHostResolvesIDsAndHosts(t *testing.T) {
	mgr, _, _, _, _ := newTestManager(t)
	ctx := t.Context()

	if got, want := mgr.MirrorHost(ctx, ""), registry.DefaultMirror().Host; got != want {
		t.Errorf("MirrorHost(\"\") = %q, want the default mirror host %q", got, want)
	}
	nju, ok := registry.MirrorByID("nju")
	if !ok {
		t.Fatal("the shipped mirror list no longer contains the nju id")
	}
	if got := mgr.MirrorHost(ctx, "  nju  "); got != nju.Host {
		t.Errorf("MirrorHost(\"  nju  \") = %q, want %q", got, nju.Host)
	}
	if got := mgr.MirrorHost(ctx, "my.registry.local:5000"); got != "my.registry.local:5000" {
		t.Errorf("MirrorHost(raw host) = %q, want it back unchanged", got)
	}
	if got := mgr.MirrorHost(ctx, "   "); got != registry.DefaultMirror().Host {
		t.Errorf("MirrorHost(blank) = %q, want the default mirror host %q", got, registry.DefaultMirror().Host)
	}

	// An empty id follows the STORED default, so changing the setting
	// changes what an unqualified pull uses.
	if _, err := mgr.SaveConfig(ctx, map[string]string{KeyDefaultMirror: "1ms"}); err != nil {
		t.Fatalf("SaveConfig(default_mirror): %v", err)
	}
	oneMS, ok := registry.MirrorByID("1ms")
	if !ok {
		t.Fatal("the shipped mirror list no longer contains the 1ms id")
	}
	if got := mgr.MirrorHost(ctx, ""); got != oneMS.Host {
		t.Errorf("MirrorHost(\"\") after changing the default = %q, want %q", got, oneMS.Host)
	}
}

// TestReconcileTasksParksInterruptedRows protects the boot-time fixup.
//
// A hard kill (taskkill, closing the console, power loss) never runs
// Shutdown, so the row stays "running" with no live job. Before this, the
// /tasks page showed 下载中 forever, the client's 2-second refetch loop stayed
// hot forever, and the dashboard's 进行中 counter was permanently inflated —
// the only escape was clicking 暂停.
func TestReconcileTasksParksInterruptedRows(t *testing.T) {
	mgr, st, _, _, _ := newTestManager(t)
	ctx := context.Background()

	// Two rows in the states a previous process could leave behind, plus one
	// terminal row that must NOT be touched.
	rows := []*store.Task{
		{ID: "mid_running", Ref: "r", Status: StatusRunning, Workers: 4, VerifyTLS: true},
		{ID: "mid_pending", Ref: "r", Status: StatusPending, Workers: 4, VerifyTLS: true},
		{ID: "already_done", Ref: "r", Status: StatusSucceeded, Workers: 4, VerifyTLS: true},
	}
	for _, row := range rows {
		if err := st.CreateTask(ctx, row); err != nil {
			t.Fatalf("CreateTask(%s): %v", row.ID, err)
		}
	}
	// A layer the dead process left mid-flight.
	if err := st.ReplaceTaskLayers(ctx, "mid_running", []store.TaskLayer{
		{ID: "l1", TaskID: "mid_running", Digest: "sha256:a", Kind: "layer", Name: "a", Position: 0, Status: "downloading"},
	}); err != nil {
		t.Fatalf("ReplaceTaskLayers: %v", err)
	}

	parked, err := mgr.ReconcileTasks(ctx)
	if err != nil {
		t.Fatalf("ReconcileTasks: %v", err)
	}
	if parked != 2 {
		t.Errorf("parked %d rows, want 2 (running + pending)", parked)
	}

	for _, tc := range []struct {
		id   string
		want string
	}{
		{"mid_running", StatusPaused},
		{"mid_pending", StatusPaused},
		{"already_done", StatusSucceeded},
	} {
		got, err := st.GetTask(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", tc.id, err)
		}
		if got.Status != tc.want {
			t.Errorf("%s status = %q, want %q", tc.id, got.Status, tc.want)
		}
		if tc.want == StatusPaused && got.Speed != 0 {
			t.Errorf("%s kept a stale speed %v after being parked", tc.id, got.Speed)
		}
	}

	// The dead process's "downloading" layer must not survive either, or the
	// UI would show a bar that never moves.
	layers, err := st.ListTaskLayers(ctx, "mid_running")
	if err != nil {
		t.Fatalf("ListTaskLayers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(layers))
	}
	if layers[0].Status != "pending" {
		t.Errorf("layer status = %q, want pending after reconciliation", layers[0].Status)
	}

	// A second boot must be a no-op: parked rows are not re-parked.
	again, err := mgr.ReconcileTasks(ctx)
	if err != nil {
		t.Fatalf("second ReconcileTasks: %v", err)
	}
	if again != 0 {
		t.Errorf("second reconcile parked %d rows, want 0", again)
	}
}
