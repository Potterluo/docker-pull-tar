# CONTRACT — frozen interfaces for the DockerPull rewrite

This file is the **frozen interface contract** every parallel workstream codes
against. If you need to change something here, change it *here first*, then in
the code — never the other way round.

Module path: `github.com/Potterluo/docker-pull-tar`
Runtime: Go 1.25 (CGO-free) + Next.js 16 static export, one binary.
Auth: **none** — this is a single-user local tool (no onboard/login/API keys).

## Revision log (changes made during implementation)

The interfaces below were adjusted while building; these are the differences
from the first draft, all of them driven by bugs the test suites found:

| Area | First draft | Built | Why |
|---|---|---|---|
| `puller.Options` TLS | `VerifyTLS bool` ("default false") | `InsecureTLS bool` | A positive flag with a safe-sounding name whose zero value *disabled* verification. `InsecureTLS` makes the zero value secure. |
| `puller.Options.MaxRetries` | `0` meant "use the default" | `MaxRetriesUnset = -1` means default; `0` means no retries | Zero conflated "unset" with "none", so a configured 0 silently became 10 retries with exponential backoff. |
| `puller` on-disk layout | everything directly under `WorkDir` | `blobs/`, `chunks/`, `stage/` subdirectories | The tar root must contain only tar members; `progress.json` and chunk temp files had to move out of it. |
| `store.Task` | `VerifyTLS` + `UseHTTP` | `VerifyTLS` + `Insecure` (transport) | `UseHTTP` duplicated what `Insecure` already meant and was never written. One column, documented. |
| `tasks.StartSpec` TLS | `VerifyTLS bool` + `UseStoredTLS bool` | `SkipVerifyTLS bool` | The stored setting is the baseline and the flag can only turn verification *off*, so a zero-valued spec can never weaken TLS. This was a real bug: `--no-verify-tls` and the GUI checkbox were silently ignored. |
| `tasks.StartSpec` proxy | `UseStoredProxy bool` | none (empty `ProxyURL` = stored) | The flag was redundant with the empty-string case. |
| `tasks.Manager.Retry` | refused succeeded tasks (shared guard with `Resume`) | only `Resume` refuses them | The guard made `Retry` answer "use 重试" — the /tasks 重试 button was dead after a successful download. |

A later adversarial review (a second agent, with this document as ground
truth) added these, all of which were real defects:

| Area | Was | Built | Why |
|---|---|---|---|
| `task.updated` payload | a 5-field map | the full `Task` | The client **replaces** the row on `task.updated`, so a partial payload blanked out 镜像/架构/创建时间 on every layer transition until the next poll. Regression test: `TestTaskUpdatedCarriesTheWholeTask`. |
| Boot | nothing reconciled leftover rows | `Manager.ReconcileTasks` | A hard kill left a row `running` with no live job: 下载中 forever, the 2-second refetch loop hot forever, 进行中 never cleared. |
| `Manager.Shutdown` | returned nothing | returns `drained bool` | The caller can tell that a worker is still writing instead of closing the store underneath it. |
| `Manager.Delete` | removed files right after `cancel()` | waits for the worker's `done` | The puller re-created its directories after the delete, leaving an orphaned tree nothing collected. |
| `.ok` seal fast path | `size > 0` | `size > 0 && (expected <= 0 \|\| size == expected)` | A truncated blob with a valid marker was reported `skipped`; for an uncompressed layer nothing downstream noticed. |
| Unknown blob size | `Content-Range` ignored | falls back to `BlobReader.TotalSize` | Progress showed 未知 forever even though the server sent the length. |
| 设为默认 (mirrors) | only built-in ids accepted | an id **or** a host | Marking a user-added mirror as default was stored and silently ignored. |
| 设为默认 (search) | only the settings key consulted | `is_default` flag consulted first | Same, for user-added search sources. |
| `fetchChunk` on 416 | claimed the chunk complete | a clear error | It recorded bytes that were never written; the failure resurfaced as a confusing merge error. |
| `POST /api/tasks` errors | classified by substring | `tasks.ErrInvalidImage` sentinel | Every unrecognised parse error became a 502 instead of a 400. |
| CLI `--help`/`--version` | fell through to `serve` → exit 2 | handled before dispatch | `--help` printed Go's raw `Usage of serve:` with a usage exit code. |
| CLI task id not found | exit 2 (usage) | exit 1 (failure) | Told scripts the wrong thing. |
| `recordArtifact` resize | delete + re-insert | `UpdateArtifact` | A failed insert after the delete lost the row entirely. |
| `repositories` file key | `library/nginx` while `RepoTags` said `nginx` | the same string as `RepoTags` | A legacy (pre-manifest) loader would have tagged the image differently. |

Round three — from a field report of the desktop app not starting at all:

| Area | Was | Built | Why |
|---|---|---|---|
| Desktop data directory | `os.UserConfigDir()` unconditionally (== `%APPDATA%`) | probed chain: `APP_DATA_DIR` → `%APPDATA%` → `%LOCALAPPDATA%` → `<exe dir>\data` | The override ignored `APP_DATA_DIR`, so a redirected / read-only / policy-locked roaming profile left the user with **no way** to redirect the app. And a directory that can be *created* is not necessarily *writable* — that is the `readonly database`/CANTOPEN failure — so every candidate is probed by creating and writing a real file. |
| WebView2 profile | Wails' default `%APPDATA%\<exe>` | `<dataDir>\webview2` via `windows.Options.WebviewUserDataPath` | A **second, independent** `%APPDATA%` requirement; fixing only the database path merely moves the failure to WebView2 init (800700aa). |
| Desktop fatal errors | `slog.Error` + `os.Exit(1)` | log file + native `MessageBoxW` (stderr off-Windows) | The binary is linked `-H windowsgui`, so it has no console: the reason went to a dead stdout handle and the user's only symptom was "double-click does nothing". The dialog and the log name the failure, the data directory, the log path, and every rejected candidate with its reason. |

Verified by running the desktop binary in an environment where **both**
`%APPDATA%` and `%LOCALAPPDATA%` were denied: it used to exit 1 silently, it now
falls back to `<exe dir>\data`, opens its window, starts WebView2 and serves the
UI. `APP_DATA_DIR` was verified to override the whole chain, and a forced store
failure was verified to produce the dialog plus the log line.

Round four — public registries and private-registry credentials:

| Area | Was | Built | Why |
|---|---|---|---|
| Public registries | only Docker Hub accelerators were listed | `registry.BuiltinRegistries` (ghcr.io, quay.io, registry.k8s.io, mcr, public.ecr.aws, nvcr.io, registry.gitlab.com, cgr.dev, gcr.io) served by `GET /api/registries` | Pulling `ghcr.io/owner/repo` always worked — `Parse` accepts any host — but nothing surfaced it, and a mirror is NOT the same thing: a mirror proxies Docker Hub's namespace, these have their own. |
| Keyword search | Docker Hub + 1ms | + `QuaySearcher` (`quay.io` is the only one of these with a public search API) | GHCR has no search API at all, so it is reached by owner/repo plus a tag listing; the catalog carries a `note` saying so rather than offering a search that would always fail. |
| `?source=` on /search | ignored | honoured, but only for an ENABLED source | The catalog's 「搜索」 button deep-links to a backend; ignoring the parameter would have silently searched the default one instead. |
| `registry.NormalizeAuthHost` | — | new | `docker.io`, `registry-1.docker.io`, `index.docker.io`, `https://index.docker.io/v1/` must be ONE credential, not four that each miss. |
| Credential storage | — | `internal/secrets` (DPAPI on Windows, AES-256-GCM + `secret.key` elsewhere) + a `credentials` table | A private image otherwise needs `-u/-p` on every invocation. |
| `Credential` through the API | — | write-only: `CredentialView` has no `Secret` field at all | A password must not be readable back through any endpoint, log or `--json` output. Returning `store.Credential` and remembering to strip a field is a rule someone eventually forgets; a type with no such field cannot leak it. |
| Pull path | used only the per-invocation credential | explicit → stored (normalised host) | Makes `login ghcr.io` then `pull ghcr.io/private/x` work with no flags. Resume/retry re-resolve, because the task row deliberately carries no secret. |
| `scripts/smoke.ps1` | the spawned server inherited the caller's stdout | its output goes to `<dataDir>/server.*.log`, shown when startup fails | An inherited stdout is a PIPE when a caller captures output; the server held it open after the script exited, so the caller blocked to its own timeout. |

---

## 1. Scope (what the product does)

Five features, one code path shared by CLI and GUI:

| # | Feature | Core package | UI |
|---|---|---|---|
| F1 | 镜像搜索 (keyword search across sources) | `internal/registry` (Searcher) | `/search` |
| F2 | 镜像下载 (multi-arch, multi-source, auth, tar export) | `internal/puller` | `/search` → dialog |
| F3 | 断点续传 (resume across process restarts) | `internal/puller` | `/tasks` |
| F4 | 进度管理 (task lifecycle + live progress) | `internal/tasks` | `/tasks` |
| F5 | 本地镜像包管理 (list/download/delete tars) | `internal/store` + server | `/artifacts` |
| F6 | 凭证管理 (stored registry logins for private images) | `internal/secrets` + `internal/tasks` | `/credentials` |

Plus settings (proxy / TLS / concurrency / sources) at `/settings`, a dashboard
at `/`, and a CLI (`pull`, `search`, `tags`, `tasks`, `serve`, `version`).

---

## 2. `internal/registry` — references, auth, manifests, search

```go
package registry

// ---------- F1/F2: image reference ----------

// Ref is a fully parsed image reference.
type Ref struct {
	Registry   string // host[:port]; default "registry-1.docker.io"
	Repository string // "library/nginx"
	Tag        string // "" when Digest is set
	Digest     string // "sha256:..." ; "" when Tag is set
	UseHTTP    bool   // plain-HTTP registry
}

func Parse(s, customRegistry string) (Ref, error) // default tag "latest"
func (r Ref) Reference() string                   // Digest if set, else Tag
func (r Ref) Host() string                        // Registry
func (r Ref) RepoTag() string                     // "nginx:latest" — "library/" stripped on Docker Hub
func (r Ref) String() string                      // "registry/repository:tag"

// Docker Hub hosts that mean the same registry.
func IsDockerHub(host string) bool

// ---------- F2: architectures ----------

type Platform struct{ OS, Architecture, Variant string }

func (p Platform) String() string // "linux/amd64", "linux/arm/v7"
func (p Platform) IsZero() bool

// ArchAliases maps a canonical name to its accepted spellings.
var ArchAliases = map[string][]string{
	"amd64": {"amd64", "x86_64", "x64"},
	"arm64": {"arm64", "arm64v8", "aarch64"},
	"arm":   {"arm", "arm32", "arm32v7", "arm32v6", "arm32v5", "armhf", "armv7"},
	"386":   {"386", "i386", "x86"},
	"ppc64le": {"ppc64le"},
	"riscv64": {"riscv64"},
	"s390x":   {"s390x"},
}

// ResolveArch finds the best platform in available for the user's target
// string. Matching order: exact "os/arch[/variant]" → exact arch → canonical
// alias group → bidirectional substring → variant match on arm.
func ResolveArch(target string, available []Platform) (Platform, bool)

// ---------- F2: registry auth ----------

type AuthChallenge struct {
	Scheme  string // "bearer" | "basic" | "none" | "unknown"
	Realm   string
	Service string
	Scope   string
}

func ParseWWWAuthenticate(header string) AuthChallenge

type Credentials struct{ Username, Password string }

func (c Credentials) Empty() bool

// ClientOptions configures the HTTP client used for every registry call.
type ClientOptions struct {
	Credentials Credentials
	ProxyURL    string        // "" = honour env; "-" = direct; else explicit proxy URL
	InsecureTLS bool          // skip TLS certificate verification
	HTTPRegistry bool         // (per-Ref; this is the default for new Refs)
	Timeout     time.Duration // per-request; 0 = 60s
	UserAgent   string        // default "dockerpull/<version>"
}

type Client struct{ /* unexported */ }

func NewClient(opts ClientOptions) *Client

// AuthHeader returns the Authorization header value to use for repo.
// Anonymous registries return "".
func (c *Client) AuthHeader(ctx context.Context, ref Ref) (string, error)

// FetchManifest fetches reference (tag or digest). It resolves nothing and
// returns the raw parsed manifest.
func (c *Client) FetchManifest(ctx context.Context, ref Ref, reference string) (*Manifest, error)

// FetchManifestChain fetches reference and, when it is an index, returns the
// sub-manifest for platform. Returns (image manifest, digest actually used).
func (c *Client) ResolveImage(ctx context.Context, ref Ref, platform Platform) (*Manifest, Platform, error)

// Manifest is either an image index or an image manifest.
type Manifest struct {
	MediaType string
	Digest    string // digest as reported by the registry (docker-content-digest or computed)
	IsIndex   bool
	Config    Descriptor
	Layers    []Descriptor
	Manifests []Descriptor // index entries only

	Raw []byte
}

func (m *Manifest) Platforms() []Platform // os=="linux" only, deduped, order preserved

type Descriptor struct {
	MediaType   string
	Digest      string
	Size        int64
	Platform    *Platform
	Annotations map[string]string
}

// BlobReader opens a blob. offset > 0 issues a Range request; the caller must
// check resp.StatusCode: 206 = resumed, 200 = server ignored the Range (start
// over), 416 = already complete.
type BlobReader struct {
	Body          io.ReadCloser
	StatusCode    int
	ContentLength int64
	TotalSize     int64 // from Content-Range; 0 when unknown
}

func (c *Client) OpenBlob(ctx context.Context, ref Ref, digest string, offset int64) (*BlobReader, error)

// HeadBlob returns the blob size, or 0 when the registry does not answer HEAD.
func (c *Client) HeadBlob(ctx context.Context, ref Ref, digest string) (int64, error)

// ---------- F1: search ----------

type SearchResult struct {
	Source      string `json:"source"`
	Name        string `json:"name"`        // "library/nginx"
	Repository  string `json:"repository"`  // same as Name, kept explicit
	Image       string `json:"image"`       // "nginx"
	Description string `json:"description"`
	Stars       int64  `json:"stars"`
	Pulls       int64  `json:"pulls"`
	Updated     string `json:"updated"`     // "YYYY-MM-DD"
	Official    bool   `json:"official"`
}

type SearchPage struct {
	Results  []SearchResult `json:"results"`
	Total    int            `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"pageSize"`
}

type TagInfo struct {
	Name     string     `json:"name"`
	Platforms []Platform `json:"platforms,omitempty"`
	Size     int64      `json:"size,omitempty"`
	Updated  string     `json:"updated,omitempty"`
}

// Searcher is one search backend.
type Searcher interface {
	// ID is the stable source id ("dockerhub", "1ms").
	ID() string
	// Name is the human label ("Docker Hub 官方", "1ms 加速源").
	Name() string
	Search(ctx context.Context, keyword string, page, pageSize int) (SearchPage, error)
	Tags(ctx context.Context, repository string) ([]TagInfo, error)
}

func NewDockerHubSearcher(client *http.Client, baseURL string) Searcher
func NewOneMSSearcher(client *http.Client, baseURL string) Searcher

// SearchSources returns the registry of built-in searchers, keyed by id.
// Unknown/empty id selects the default ("dockerhub").
func SearchSources(client *http.Client) map[string]Searcher
func PickSearcher(sources map[string]Searcher, id string) (Searcher, error)
```

### Built-in data (mirrors & sources)

```go
// Mirror is a registry mirror — a plain host substitution for Docker Hub.
type Mirror struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	IsDefault bool  `json:"isDefault"`
}

var BuiltinMirrors = []Mirror{
	{"dockerhub", "Docker Hub 官方", "registry-1.docker.io", true},
	{"1ms", "1ms 加速源", "docker.1ms.run", false},
	{"nju", "南大镜像源", "docker.nju.edu.cn", false},
	{"xuanyuan", "轩辕镜像", "docker.xuanyuan.me", false},
	{"daocloud", "DaoCloud 镜像", "docker.m.daocloud.io", false},
	{"daocloud-k8s", "DaoCloud K8s", "k8s.m.daocloud.io", false},
	{"daocloud-ghcr", "DaoCloud GHCR", "ghcr.m.daocloud.io", false},
	{"daocloud-quay", "DaoCloud Quay", "quay.m.daocloud.io", false},
}

var BuiltinSearchSources = []struct{ ID, Name, URL string }{
	{"dockerhub", "Docker Hub 官方", "https://hub.docker.com"},
	{"1ms", "1ms 加速源", "https://1ms.run/api/v1/registry"},
}
```

---

## 3. `internal/puller` — resumable download + tar assembly

```go
package puller

// Options is one pull job. Every field is resolved by the caller (server,
// CLI, or tasks.Manager) — the puller never prompts.
type Options struct {
	Ref       registry.Ref
	Platform  registry.Platform

	// WorkDir holds partial downloads + progress.json. One per task.
	// Resuming a task means pointing a new job at the same WorkDir.
	WorkDir   string
	// OutputDir is where the finished .tar is written.
	OutputDir string

	Workers        int   // concurrent layers; default 4
	ChunkSize      int64 // default 10 MiB
	ChunkThreshold int64 // blobs above this use chunked mode; default 50 MiB

	// MaxRetries is the per-blob retry budget. MaxRetriesUnset (-1) asks
	// for DefaultMaxRetries; 0 is a real value meaning "do not retry".
	// (Zero could not double as "unset" — that made a configured 0 silently
	// become 10 retries with exponential backoff.)
	MaxRetries int

	// InsecureTLS skips certificate verification. Negative on purpose: the
	// zero value means "verify", so a caller that forgets cannot silently
	// disable TLS. (The legacy tool hard-disabled it everywhere.)
	InsecureTLS bool

	// Sink receives progress. May be nil. Implementations must be
	// non-blocking-ish: the puller calls them from worker goroutines.
	Sink Sink
}

// Sink receives progress callbacks. Implementations must be safe for
// concurrent use.
type Sink interface {
	// OnState is called when a layer/task state changes materially
	// (status transition, size discovery, completion) — low frequency.
	OnState(st State)
	// OnProgress is called frequently with byte counters only.
	OnProgress(digest string, downloaded int64)
}

// State is the persisted job state (WorkDir/progress.json) and the payload
// sent to Sink.OnState and over SSE.
type State struct {
	Ref      string     `json:"ref"`      // registry/repository:tag
	Platform string     `json:"platform"` // linux/amd64
	Status   string     `json:"status"`   // pending|running|completed|failed|canceled
	Error    string     `json:"error,omitempty"`

	TotalBytes      int64 `json:"totalBytes"`
	DownloadedBytes int64 `json:"downloadedBytes"`

	Layers []LayerState `json:"layers"`

	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type LayerState struct {
	Digest     string `json:"digest"`
	Kind       string `json:"kind"`   // "layer" | "config"
	Name       string `json:"name"`   // short display name, e.g. "a3f1c2b9d0e4"
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status"` // pending|downloading|completed|failed|skipped
	Error      string `json:"error,omitempty"`

	// Chunks is the durable resume ledger for chunked downloads; empty for
	// streaming downloads (those resume from the partial file size).
	Chunks []ChunkState `json:"chunks,omitempty"`
}

type ChunkState struct {
	Index  int   `json:"index"`
	Start  int64 `json:"start"`
	End    int64 `json:"end"` // inclusive
	Done   bool  `json:"done"`
}

// Result is a finished job.
type Result struct {
	TarPath   string        `json:"tarPath"`
	Size      int64         `json:"size"`
	Bytes     int64         `json:"bytes"`     // total downloaded payload
	Duration  time.Duration `json:"duration"`
	Layers    int           `json:"layers"`
	Completed int           `json:"completed"` // layers copied from a previous run
}

// Pull runs one job to completion. It is resumable: calling Pull again with
// the same WorkDir continues where the previous call stopped, including after
// a process restart. Cancel via ctx (progress is saved before returning).
func Pull(ctx context.Context, opts Options) (*Result, error)

// LoadState reads WorkDir/progress.json. A missing or unreadable file yields
// (nil, nil) — a fresh job. A state whose Ref/Platform does not match opts
// yields (nil, nil) as well (stale state is never reused).
func LoadState(workDir string, ref registry.Ref, platform registry.Platform) (*State, error)

// SaveState writes WorkDir/progress.json atomically (tmp + rename).
func SaveState(workDir string, st *State) error

// ScanPlatforms resolves the platforms an image publishes (for the arch
// picker). Thin wrapper so callers do not import registry for this.
func ScanPlatforms(ctx context.Context, client *registry.Client, ref registry.Ref) ([]registry.Platform, error)

// TarName is the artifact file name: "<safe_repo>_<tag>_<safe_arch>.tar".
func TarName(ref registry.Ref, platform registry.Platform) string

// DefaultOutputDir is "<base>/<safe_repo>_<tag>_<safe_arch>".
func DefaultOutputDir(base string, ref registry.Ref, platform registry.Platform) string
```

### On-disk layout (must stay `docker load`-compatible)

```
<WorkDir>/progress.json                        job state (resume ledger)
<WorkDir>/blobs/<sha256_hex>                   downloaded blob (partial or complete)
<WorkDir>/blobs/<sha256_hex>.ok                seal marker: digest verified
<WorkDir>/chunks/<sha256_hex>/chunk_%05d       chunked-mode pieces, renamed into
                                               place only once whole
<WorkDir>/stage/<fakeid>/layer.tar             gunzipped layer (the tar root)
<WorkDir>/stage/<fakeid>/json                  {"id": fakeid, "parent": parentid|null}
<WorkDir>/stage/<confighex>.json               image config blob
<WorkDir>/stage/manifest.json                  [{"Config","RepoTags","Layers"}]
<WorkDir>/stage/repositories                   {repo_key: {tag: lastLayerID}}

<OutputDir>/<TarName>                          final artifact
```

The three subdirectories keep non-tar files out of the archive root:
`tar -C <WorkDir>/stage -cf out.tar .` is exactly the member set, so
`progress.json`, the blob cache and any chunk temp files cannot leak into the
artifact. `stage/` is deleted after a successful pack; `blobs/` is kept so a
re-export or re-pull of the same image needs no network.

- `fakeid = sha256hex(parentID + "\n" + layerDigest + "\n")`, `parentID` is the
  previous layer's fakeid (`""` for the first).
- The tar is written with **root-relative member names** (equivalent to
  `tar -C <WorkDir> -cf out.tar .`) — no `layers/` prefix. `manifest.json`
  lists `"<fakeid>/layer.tar"`.
- `RepoTags` is `["nginx:latest"]` — `library/` is stripped for Docker Hub.

### Resume guarantees (F3)

1. Streaming path: resuming sends `Range: bytes=<size(partial)>-`.
   - `206` → append from the offset.
   - `200` → server ignored the Range: **truncate and restart** (never append).
   - `416` → the partial file is already complete; verify and finish.
2. Chunked path (blob > `ChunkThreshold`): each chunk is an independent Range
   GET writing `chunk_%05d`; `ChunkState.Done` is persisted in `progress.json`
   so a restart skips finished chunks.
3. Every completed blob is verified against its digest. On mismatch the file is
   deleted, the chunk ledger reset, and the blob re-downloaded (counted against
   `MaxRetries`).
4. Layers already `completed` are reported as `skipped` and re-verified cheaply
   (size check; full hash only when the size matches but the digest file is
   absent).

---

## 4. `internal/store` — persistence

Added to the scaffold's `Store` interface (see `internal/store/store.go`).
All timestamps UTC. `?` placeholders + `d.rebind(...)`.

```go
// Task is one download job. Config columns are the resolved pull options.
type Task struct {
	ID         string
	Ref        string // "registry/repository:tag"
	Registry   string
	Repository string
	Tag        string
	Digest     string
	Platform   string // "linux/amd64"

	Status     string // pending|running|paused|succeeded|failed|canceled
	Error      string

	TotalBytes      int64
	DownloadedBytes int64
	Speed           float64 // bytes/sec, last sampled

	WorkDir   string
	TarPath   string
	TarSize   int64

	Workers int `json:"workers"`
	// Insecure is the registry's TRANSPORT: true reaches it over plain HTTP
	// (the legacy `--insecure`, needed for a local registry). It is
	// per-task state so a resumed job reaches the same registry the same
	// way.
	Insecure bool `json:"insecure"`
	// VerifyTLS is the unrelated CERTIFICATE switch; it defaults to true.
	VerifyTLS bool `json:"verifyTls"`

	CreatedAt  time.Time
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
}

// TaskLayer is one blob of a task (one layer, or the config blob).
type TaskLayer struct {
	ID         string
	TaskID     string
	Digest     string
	Kind       string // "layer" | "config"
	Name       string
	Position   int
	Size       int64
	Downloaded int64
	Status     string // pending|downloading|completed|failed|skipped
	Error      string
}

// Source is a user-configurable search source or registry mirror.
type Source struct {
	ID        string
	Kind      string // "search" | "mirror"
	Name      string
	URL       string // search sources
	Host      string // mirror sources
	Enabled   bool
	Priority  int
	IsDefault bool
	CreatedAt time.Time
}

// Artifact is a finished .tar on disk.
type Artifact struct {
	ID         string
	Name       string // file name
	Path       string // absolute path
	Repository string
	Tag        string
	Platform   string
	Size       int64
	TaskID     string
	CreatedAt  time.Time
}

// Setting is a key/value app preference row.
type Setting struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

type TaskStore interface {
	CreateTask(ctx context.Context, t *Task) error
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasks(ctx context.Context, limit int) ([]Task, error)
	UpdateTask(ctx context.Context, t *Task) error
	DeleteTask(ctx context.Context, id string) error

	ReplaceTaskLayers(ctx context.Context, taskID string, layers []TaskLayer) error
	ListTaskLayers(ctx context.Context, taskID string) ([]TaskLayer, error)
	UpdateTaskLayer(ctx context.Context, l *TaskLayer) error

	CreateSource(ctx context.Context, s *Source) error
	GetSource(ctx context.Context, id string) (*Source, error)
	ListSources(ctx context.Context, kind string) ([]Source, error) // kind "" = all
	UpdateSource(ctx context.Context, s *Source) error
	DeleteSource(ctx context.Context, id string) error

	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	ListSettings(ctx context.Context) ([]Setting, error)

	CreateArtifact(ctx context.Context, a *Artifact) error
	GetArtifact(ctx context.Context, id string) (*Artifact, error)
	ListArtifacts(ctx context.Context) ([]Artifact, error)
	DeleteArtifact(ctx context.Context, id string) error
}
```

Tables: `tasks`, `task_layers`, `sources`, `settings`, `artifacts` — added to
`migrationSQL()` in `internal/store/db.go`. No `user_id` (no auth).

**Setting keys** (all optional; these are the defaults):

| Key | Default | Meaning |
|---|---|---|
| `output_dir` | `<dataDir>/downloads` | where `.tar` files land |
| `workers` | `4` | concurrent layers |
| `verify_tls` | `true` | verify TLS certificates |
| `proxy_mode` | `system` | `system` \| `none` \| `custom` |
| `proxy_url` | `""` | used when `proxy_mode=custom` |
| `default_mirror` | `dockerhub` | mirror id used when a ref names no registry |
| `default_search_source` | `dockerhub` | search backend id |
| `chunk_threshold_mb` | `50` | chunked-download threshold |
| `max_retries` | `10` | per-blob retry budget |

---

## 5. `internal/tasks` — lifecycle + progress fan-out

```go
package tasks

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Task is the wire-facing view: a store.Task plus its layers.
type Task struct {
	store.Task
	Layers []store.TaskLayer `json:"layers"`
}

// StartSpec is a resolved request to start a pull.
type StartSpec struct {
	Image    string // raw user string, e.g. "nginx:1.26"
	Registry string // optional explicit host or mirror id
	Platform string // "amd64" | "linux/arm64"
	Workers  int

	// Insecure is the TRANSPORT: reach the registry over plain HTTP.
	Insecure bool
	// SkipVerifyTLS turns certificate verification OFF for this pull. The
	// stored `verify_tls` setting is always the baseline, so the zero value
	// means "keep the configured behaviour" — a zero-valued spec can never
	// weaken TLS.
	SkipVerifyTLS bool

	OutputDir string
	// ProxyURL overrides the stored proxy. Empty means "use the
	// proxy_mode/proxy_url settings".
	ProxyURL string

	Credentials registry.Credentials
}

type Manager struct{ /* unexported */ }

func NewManager(st store.Store, hub *events.Hub, dataDir string, version string) *Manager

// Start resolves the spec, creates the task row, launches the job and
// returns immediately.
func (m *Manager) Start(ctx context.Context, spec StartSpec) (*Task, error)

func (m *Manager) Get(ctx context.Context, id string) (*Task, error)
func (m *Manager) List(ctx context.Context, limit int) ([]Task, error)

// Pause cancels the running job but keeps its WorkDir + progress.json, so
// Resume continues from the same byte offsets.
func (m *Manager) Pause(ctx context.Context, id string) error
// Resume restarts a paused/failed/canceled task against the same WorkDir.
func (m *Manager) Resume(ctx context.Context, id string) error
// Cancel stops the job and marks it canceled.
func (m *Manager) Cancel(ctx context.Context, id string) error
// Retry clears the resume ledger and starts over from byte 0.
func (m *Manager) Retry(ctx context.Context, id string) error
// Delete removes the task; removeFiles also deletes WorkDir + artifact.
func (m *Manager) Delete(ctx context.Context, id string, removeFiles bool) error

// Shutdown stops every running job, leaving resumable state on disk. It
// reports whether every worker finished its bookkeeping before the deadline:
// false means the caller must not treat the store as safe to close yet.
func (m *Manager) Shutdown(ctx context.Context) bool

// ReconcileTasks parks tasks a previous process left mid-flight (running or
// pending with no live job) and demotes their "downloading" layers. Call it
// once at boot, before serving — without it a hard-killed download shows as
// 下载中 forever.
func (m *Manager) ReconcileTasks(ctx context.Context) (int, error)

// ScanArtifacts reconciles the artifacts table with OutputDir on disk.
func (m *Manager) ScanArtifacts(ctx context.Context) ([]store.Artifact, error)
```

### SSE events (F4)

Published on the shared `events.Hub` (never a second hub):

| Type | Data |
|---|---|
| `task.created` | full `Task` |
| `task.updated` | full `Task` — NEVER a partial map; the client replaces the row with it (status/byte changes; coalesced to ≤ ~5/s per task) |
| `task.progress` | `{id, downloadedBytes, totalBytes, speed, layers:[{digest,downloaded,status}]}` |
| `task.finished` | `{id, status, tarPath, error}` |
| `artifact.created` | full `store.Artifact` |
| `artifact.deleted` | `{id}` |
| `source.changed` | `{id}` |
| `settings.changed` | `{}` |

---

## 6. HTTP API (envelope: `{"ok":true,...}` / `{"ok":false,"error":"..."}`)

No auth. Any mutating route may be called by the local browser.

```
GET    /healthz /livez /readyz
GET    /api/status
GET    /api/search?q=&source=&page=&pageSize=
GET    /api/tags?repository=&source=
POST   /api/images/inspect        {image, registry?, platform?}   -> InspectResult
POST   /api/images/inspect                                      {image, registry?, platform?,
                                                                  source?, insecure?}
                                                                -> {ref, mediaType, isIndex,
                                                                    platforms[], actual?, tags[],
                                                                    tagsFrom, totalBytes, layerCount}
                                                                `insecure` mirrors taskRequest: plain HTTP,
                                                                and it must be sent to BOTH inspect and the
                                                                task or the picker describes a different
                                                                connection than the pull.
                                                                `tags` comes from the REGISTRY being
                                                                inspected (its own tags/list; `latest` and
                                                                the requested tag are probed and put
                                                                first, because the page is lexicographic and
                                                                paginated); a searcher is only the fallback
                                                                for a registry without that endpoint, and
                                                                `tagsFrom` records which one answered.
`registry` (and the CLI's `--mirror`) accepts anything registry.PullHost resolves: a
built-in accelerator id/name ("1ms", "南大镜像源"), an upstream-registry id/name ("mcr",
"quay", "Quay.io (Red Hat)"), a search-source name, or a host ("registry.example.com:5000").
A value that is none of those and not host-shaped is refused (400 / exit 2) instead of
being dialled as a hostname. The settings validator (`default_mirror`) uses the same
function, so the three spellings can never disagree.

GET    /api/sources?kind=                                       [{...source, pullHost?}]
                                                                pullHost is set on SEARCH sources only: the host
                                                                their results must be pulled from. A search result
                                                                is a repository NAME, not a location, so without it
                                                                a 1ms search downloads from the default mirror.
                                                                Empty = use defaultPullHost. Mirrors carry no
                                                                pullHost: their own `host` is the target.
POST   /api/sources               {kind,name,url?,host?,enabled,priority,isDefault}
PUT    /api/sources/{id}
DELETE /api/sources/{id}
GET    /api/registries                                          {registries:[{id,name,host,searchId?,note?}], defaultPullHost}
                                                                read-only. defaultPullHost is the host a reference
                                                                with NO host resolves to today (`default_mirror`,
                                                                which may be a built-in accelerator id, a user-added
                                                                mirror host, or nothing) — never assumed to be docker.io.
GET    /api/credentials                                         (stored logins; NEVER returns a secret)
POST   /api/credentials             {host,username?,secret,kind?,note?}   (upsert by host)
PUT    /api/credentials/{id}        same, secret omitted = keep the stored one
DELETE /api/credentials/{id}
GET    /api/settings
PUT    /api/settings              {key:value,...}   (bulk upsert)
GET    /api/tasks
POST   /api/tasks                 {image, registry?, platform?, workers?, insecure?, useHTTP?, verifyTls?, outputDir?} -> Task
GET    /api/tasks/{id}
POST   /api/tasks/{id}/pause
POST   /api/tasks/{id}/resume
POST   /api/tasks/{id}/cancel
POST   /api/tasks/{id}/retry
DELETE /api/tasks/{id}?files=true
GET    /api/artifacts
GET    /api/artifacts/{id}
POST   /api/artifacts/{id}/reveal                              opens the artifact's folder in the OS file
                                                                manager (server-side, because a browser
                                                                cannot open a local folder). The path comes
                                                                from the store row and goes through the SAME
                                                                validation as download/delete — the client
                                                                sends an id, never a path.        -> application/x-tar download
DELETE /api/artifacts/{id}
GET    /api/stats
GET    /api/events                -> SSE
```

### DTOs (JSON field names are camelCase, matching `store` structs)

`POST /api/tasks` carries two TLS-ish knobs that are NOT the same thing:

| Field | Meaning |
|---|---|
| `useHTTP` | Talk to the registry over **plain HTTP**, no TLS at all. The CLI's legacy `--insecure` maps here. |
| `insecure` | Skip TLS **certificate verification** (the UI's 跳过 TLS 校验 checkbox). |
| `verifyTls` | Explicit positive form of `insecure`; when present it wins. |

Neither can turn verification back on against a stored `verify_tls: false` —
that is by design (`SkipVerifyTLS` semantics).

```go
// InspectResult
type InspectResult struct {
	Ref         registry.Ref        `json:"ref"`         // {registry,repository,tag,digest,useHTTP}
	MediaType   string              `json:"mediaType"`
	IsIndex     bool                `json:"isIndex"`
	Platforms   []registry.Platform `json:"platforms"`   // empty for a single manifest
	Actual      *registry.Platform  `json:"actual,omitempty"`
	Tags        []string            `json:"tags"`        // best-effort, may be empty
	TotalBytes  int64               `json:"totalBytes"`  // sum of layer sizes when known
	LayerCount  int                 `json:"layerCount"`
}

// Stats (dashboard)
type Stats struct {
	TasksTotal     int   `json:"tasksTotal"`
	TasksRunning   int   `json:"tasksRunning"`
	TasksSucceeded int   `json:"tasksSucceeded"`
	TasksFailed    int   `json:"tasksFailed"`
	BytesTotal     int64 `json:"bytesTotal"`
	ArtifactsTotal int   `json:"artifactsTotal"`
	ArtifactsBytes int64 `json:"artifactsBytes"`
}
```

`/api/status` (replaces the scaffold's auth-driven probe):

```json
{"ok":true,"version":"v2.0.0","configured":true,"dataDir":"...","outputDir":"...","platform":"windows/amd64","auth":false}
```

---

## 7. Frontend pages (`web/src/app/`)

Static export, all client components. Trailing-slash hrefs.

| Route | File | Content |
|---|---|---|
| `/` | `page.tsx` | 概览 — stat cards, recent tasks, artifact count, live SSE feed |
| `/search` | `search/page.tsx` | 镜像搜索 + tag/arch picker dialog + 开始下载 |
| `/tasks` | `tasks/page.tsx` | 进度管理 — task table, per-layer bars, pause/resume/cancel/retry/delete |
| `/artifacts` | `artifacts/page.tsx` | 本地镜像包 — list, `docker load` hint, browser download, delete |
| `/search` step order | `search/page.tsx` | 1 搜索 → **2 选择下载源** → 3 选择版本与架构 → 下载, plus a direct full-reference box. The download source comes BEFORE tag/arch because both are read from the chosen registry: a tag on quay.io need not exist on docker.io. Changing the source re-resolves them. |
| `/settings` | `settings/page.tsx` | 设置 — 网络/代理, 下载选项, 数据源管理, 外观 |

Removed (scaffold examples / auth): `/chat`, `/items`, `/files`, `/analytics`,
`/onboard`, `/admin/users`, `/settings/apikeys`.

Components removed: `auth-guard.tsx`, `login-screen.tsx`, `user-context.tsx`,
`markdown*.tsx`.

### `web/src/lib/api.ts` surface

```ts
export interface Envelope { ok: boolean; error?: string }

export interface Ref { registry: string; repository: string; tag: string; digest: string; useHTTP: boolean }
export interface Platform { os: string; architecture: string; variant?: string }
export interface SearchResult { source: string; name: string; repository: string; image: string; description: string; stars: number; pulls: number; updated: string; official: boolean }
export interface SearchPage { results: SearchResult[]; total: number; page: number; pageSize: number }
export interface TagInfo { name: string; platforms?: Platform[]; size?: number; updated?: string }
export interface InspectResult { ref: Ref; mediaType: string; isIndex: boolean; platforms: Platform[]; actual?: Platform; tags: string[]; totalBytes: number; layerCount: number }
export interface TaskLayer { id: string; taskId: string; digest: string; kind: string; name: string; position: number; size: number; downloaded: number; status: string; error: string }
export interface Task { id: string; ref: string; registry: string; repository: string; tag: string; digest: string; platform: string; status: string; error: string; totalBytes: number; downloadedBytes: number; speed: number; workDir: string; tarPath: string; tarSize: number; workers: number; insecure: boolean; verifyTls: boolean; createdAt: string; startedAt: string; updatedAt: string; finishedAt: string; layers: TaskLayer[] }
export interface Artifact { id: string; name: string; path: string; repository: string; tag: string; platform: string; size: number; taskId: string; createdAt: string }
export interface Source { id: string; kind: string; name: string; url: string; host: string; enabled: boolean; priority: number; isDefault: boolean; createdAt: string }
export interface Settings { [key: string]: string }
export interface Stats { tasksTotal: number; tasksRunning: number; tasksSucceeded: number; tasksFailed: number; bytesTotal: number; artifactsTotal: number; artifactsBytes: number }
export interface Status { version: string; configured: boolean; dataDir: string; outputDir: string; platform: string; auth: boolean }

// apiFetch(url, init?) — same-origin, no auth header, no actAs mirroring.
export async function apiJSON<T extends Envelope>(url: string, init?: RequestInit): Promise<T>
export async function apiFetch(url: string, init?: RequestInit): Promise<Response>

export function getStatus(): Promise<Status & Envelope>
export function getStats(): Promise<Stats & Envelope>
export function searchImages(q: string, source: string, page: number, pageSize?: number): Promise<SearchPage & Envelope>
export function getTags(repository: string, source: string): Promise<{ tags: TagInfo[] } & Envelope>
export function inspectImage(image: string, registry?: string, platform?: string): Promise<InspectResult & Envelope>
export function listTasks(): Promise<{ tasks: Task[] } & Envelope>
export function getTask(id: string): Promise<{ task: Task } & Envelope>
export function createTask(body: { image: string; registry?: string; platform?: string; workers?: number; insecure?: boolean; useHTTP?: boolean; verifyTls?: boolean; outputDir?: string }): Promise<{ task: Task } & Envelope>
export function pauseTask(id: string): Promise<Envelope>
export function resumeTask(id: string): Promise<Envelope>
export function cancelTask(id: string): Promise<Envelope>
export function retryTask(id: string): Promise<Envelope>
export function deleteTask(id: string, files?: boolean): Promise<Envelope>
export function listArtifacts(): Promise<{ artifacts: Artifact[] } & Envelope>
export function deleteArtifact(id: string): Promise<Envelope>
export function artifactURL(id: string): string
export function listSources(kind?: string): Promise<{ sources: Source[] } & Envelope>
export function createSource(body: Partial<Source>): Promise<{ source: Source } & Envelope>
export function updateSource(id: string, body: Partial<Source>): Promise<{ source: Source } & Envelope>
export function deleteSource(id: string): Promise<Envelope>
export function getSettings(): Promise<{ settings: Settings } & Envelope>
export function updateSettings(patch: Settings): Promise<Envelope>
export function subscribeEvents(handlers: { onTask?: (t: Task) => void; onProgress?: (p: TaskProgress) => void; onArtifact?: (a: Artifact) => void; onAny?: (type: string, data: unknown) => void; onStatus?: (s: "open" | "closed") => void }): () => void
```

`subscribeEvents` wraps `EventSource("/api/events")` and dispatches on
`event.type`; the scaffold's existing helper shape must be preserved
(`TextDecoder`-free; SSE here is server→client only).

---

## 8. CLI (`cmd/server`, binary `dockerpull`)

Subcommand dispatch before flag parsing. Default (no subcommand) = `serve`, so
`./dockerpull` behaves like the scaffold's single binary.

```
dockerpull [serve] [-port N] [-data-dir PATH] [-dev-proxy URL] [-version]
dockerpull pull   -i <image> [-a amd64] [-o DIR] [--workers N] [--insecure]
                  [-r REGISTRY] [-u USER] [-p PASS] [--mirror HOST|ID]
                  [--proxy URL] [--no-proxy] [--no-verify-tls]
                  [--ci] [--quiet] [--list-arch] [--debug]
dockerpull search -k <keyword> [--source ID] [--page N] [--page-size N] [--json] [--no-download] [--tag T] [-a ARCH] [-o DIR]
dockerpull tags   -i <image> [--source ID] [--json]
dockerpull tasks  [list | show <id> | pause <id> | resume <id> | cancel <id> | retry <id> | rm <id> [--files]]
dockerpull version
```

Exit codes: `0` success, `1` failure, `2` usage error. **Never** a silent 0.

Original Chinese user-facing strings are kept for the CLI's interactive mode
(`🚀 Docker 镜像拉取工具`, `📋 当前可用架构：...`, `📎 ... 检测到已下载 ...，尝试断点续传...`,
`✅ 镜像 ... 下载完成！`, `💡 导入命令: docker load -i ...`, `⏱️ 总耗时: ...`).

---

## 9. Ground rules for every workstream

1. **Definition of done**: `go build ./...`, `go vet ./...`, `go test ./...`,
   and `cd web && pnpm build` all pass.
2. One response envelope; `writeOK`/`writeError`/`readJSON` only.
3. Store: one interface, `?` placeholders + `d.rebind(...)`, `ErrNotFound` for
   missing rows. New tables go in `migrationSQL()`.
4. Package `internal/server/dist` is build output — never edit by hand.
5. Never delete or reorder a `--- gen:* ---` marker.
6. The repo root stays package-less (no `.go` files in the root directory).
7. Streaming/SSE must ride real TCP (the Wails desktop shell depends on it).
8. Do not add a second `events.Hub`; `internal/app.Boot` creates the one hub.
9. Mirror/source defaults come from `registry.BuiltinMirrors` /
   `registry.BuiltinSearchSources` — the store seeds them on first boot, so
   both CLI and GUI see the same list.
10. No `user_id` columns, no ownership checks, no `?actAs=`, no auth
    middleware — authentication is out of scope by decision.

