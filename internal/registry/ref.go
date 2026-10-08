// Package registry implements the Docker Registry HTTP API v2 client:
// image reference parsing, token/basic authentication, manifest and blob
// fetching, architecture resolution, and image search.
//
// It is deliberately free of any knowledge of progress reporting, storage
// or HTTP serving — internal/puller and internal/tasks build on it.
package registry

import (
	"fmt"
	"strings"
)

// DefaultRegistry is where an unqualified image reference resolves.
const DefaultRegistry = "registry-1.docker.io"

// Ref is a fully parsed image reference.
//
// Exactly one of Tag or Digest is normally meaningful: a reference with a
// digest pins an immutable manifest, a reference with a tag is mutable.
// Reference() picks the right one.
type Ref struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	UseHTTP    bool   `json:"useHTTP"`
}

// Reference is what goes into the URL: the digest when pinned, else the tag.
func (r Ref) Reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	if r.Tag != "" {
		return r.Tag
	}
	return "latest"
}

// Host returns the registry host[:port].
func (r Ref) Host() string { return r.Registry }

// Scheme returns "http" for plain-HTTP registries, else "https".
func (r Ref) Scheme() string {
	if r.UseHTTP {
		return "http"
	}
	return "https"
}

// BaseURL is the registry's v2 API root: "<scheme>://<host>/v2".
func (r Ref) BaseURL() string {
	return r.Scheme() + "://" + r.Registry + "/v2"
}

// TagOrDefault is the tag used for display and for `docker load`-visible
// tags; a digest-only reference still needs a printable tag.
func (r Ref) TagOrDefault() string {
	if r.Tag != "" {
		return r.Tag
	}
	return "latest"
}

// String is the canonical "registry/repository:tag" form.
func (r Ref) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return s + "@" + r.Digest
	}
	return s + ":" + r.TagOrDefault()
}

// RepoTag is the tag recorded in the tar's RepoTags field. Docker Hub's
// implicit "library/" namespace is stripped, matching `docker save`:
// library/nginx becomes "nginx", user/app stays "user/app".
func (r Ref) RepoTag() string {
	repo := r.Repository
	if NeedsLibraryPrefix(r.Registry) {
		repo = strings.TrimPrefix(repo, "library/")
	}
	return repo + ":" + r.TagOrDefault()
}

// IsZero reports whether the reference is unset.
func (r Ref) IsZero() bool { return r.Registry == "" || r.Repository == "" }

// WithRegistry returns a copy of r pointing at host. Used by mirror
// selection: the repository and tag are preserved, only the host changes.
func (r Ref) WithRegistry(host string, useHTTP bool) Ref {
	r.Registry = host
	r.UseHTTP = useHTTP
	return r
}

// Parse turns a user-supplied image string into a Ref.
//
//	customRegistry, when non-empty, supplies the registry for references that
//	do not name one themselves — an explicit host in s ALWAYS wins, because
//	the user typed it deliberately. To force a mirror onto a fully qualified
//	reference, resolve the Ref first and call WithRegistry.
//
// Accepted shapes:
//
//	nginx
//	nginx:1.26
//	library/nginx
//	user/app:1.0
//	harbor.abc.com/a/b/nginx:1.26
//	harbor.abc.com:5000/a/b/nginx
//	nginx@sha256:0123...
//	harbor.abc.com/a/b/nginx:1.26@sha256:0123...
func Parse(s, customRegistry string) (Ref, error) {
	ref := Ref{}
	original := strings.TrimSpace(s)
	if original == "" {
		return ref, fmt.Errorf("镜像名称不能为空")
	}

	rest := original

	// A digest is unambiguous: it is everything after the first '@'.
	if at := strings.Index(rest, "@"); at >= 0 {
		digest := strings.TrimSpace(rest[at+1:])
		rest = strings.TrimSpace(rest[:at])
		if digest == "" {
			return ref, fmt.Errorf("镜像 %q 的 digest 为空", original)
		}
		if !strings.Contains(digest, ":") {
			return ref, fmt.Errorf("镜像 %q 的 digest 格式无效（应为 algo:hex，例如 sha256:...）", original)
		}
		ref.Digest = digest
	}
	if rest == "" {
		return ref, fmt.Errorf("镜像名称不能为空")
	}

	// Split the registry off the front. The first path segment is a host
	// when the reference has a '/' AND that segment carries a dot or a
	// port, or is exactly "localhost" (Docker's own heuristic).
	registry := ""
	remainder := rest
	if slash := strings.Index(rest, "/"); slash >= 0 {
		first := rest[:slash]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry = first
			remainder = rest[slash+1:]
		}
	}
	if remainder == "" {
		return ref, fmt.Errorf("镜像 %q 缺少仓库名", original)
	}

	// Tag: the last ':' that appears after the final '/'. This keeps
	// registry ports (already split off) and repo names containing ':'
	// out of the tag.
	tag := ""
	if colon := strings.LastIndex(remainder, ":"); colon >= 0 && !strings.Contains(remainder[colon+1:], "/") {
		tag = remainder[colon+1:]
		remainder = remainder[:colon]
	}
	if remainder == "" {
		return ref, fmt.Errorf("镜像 %q 缺少仓库名", original)
	}
	if strings.ContainsAny(tag, " \t") {
		return ref, fmt.Errorf("镜像 %q 的 tag 含非法字符", original)
	}

	if registry == "" {
		registry = strings.TrimSpace(customRegistry)
		if registry == "" {
			registry = DefaultRegistry
		}
	}
	ref.Registry = strings.TrimSuffix(registry, "/")

	// Docker Hub and its pull-through mirrors use the implicit "library"
	// namespace for single-segment repositories.
	if NeedsLibraryPrefix(ref.Registry) && !strings.Contains(remainder, "/") {
		remainder = "library/" + remainder
	}
	ref.Repository = remainder

	if tag != "" {
		ref.Tag = tag
	} else if ref.Digest == "" {
		ref.Tag = "latest"
	} else {
		// Digest-pinned with no tag: keep a printable tag for docker load.
		ref.Tag = "latest"
	}
	return ref, nil
}

// dockerHubHosts are the spellings that all mean Docker Hub itself.
var dockerHubHosts = map[string]bool{
	"docker.io":            true,
	"index.docker.io":      true,
	"registry-1.docker.io": true,
	"registry.docker.io":   true,
	"hub.docker.com":       true,
}

// IsDockerHub reports whether host is Docker Hub under any of its names.
func IsDockerHub(host string) bool { return dockerHubHosts[strings.ToLower(host)] }

// dockerHubProxies are known pull-through mirrors of Docker Hub. They serve
// the same repository namespace, so they need the same "library/" rule.
var dockerHubProxies = map[string]bool{
	"docker.1ms.run":        true,
	"docker.nju.edu.cn":     true,
	"docker.xuanyuan.me":    true,
	"docker.xuanyuan.cloud": true,
	"docker.m.daocloud.io":  true,
}

// NeedsLibraryPrefix reports whether single-segment repositories on host
// must be expanded to "library/<name>".
func NeedsLibraryPrefix(host string) bool {
	h := strings.ToLower(host)
	return IsDockerHub(h) || dockerHubProxies[h]
}

// Mirror is a named registry host, used both as a pull-through mirror of
// Docker Hub and as an explicit pull source.
type Mirror struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	IsDefault bool   `json:"isDefault"`
}

// BuiltinMirrors is the shipped mirror list. The store seeds these on first
// boot so the CLI and the GUI show the same set.
var BuiltinMirrors = []Mirror{
	{ID: "dockerhub", Name: "Docker Hub 官方", Host: "registry-1.docker.io", IsDefault: true},
	{ID: "1ms", Name: "1ms 加速源", Host: "docker.1ms.run"},
	{ID: "nju", Name: "南大镜像源", Host: "docker.nju.edu.cn"},
	{ID: "xuanyuan", Name: "轩辕镜像", Host: "docker.xuanyuan.me"},
	{ID: "xuanyuan-cloud", Name: "轩辕镜像 (Cloud)", Host: "docker.xuanyuan.cloud"},
	{ID: "daocloud", Name: "DaoCloud Docker Hub", Host: "docker.m.daocloud.io"},
	{ID: "daocloud-k8s", Name: "DaoCloud K8s", Host: "k8s.m.daocloud.io"},
	{ID: "daocloud-ghcr", Name: "DaoCloud GHCR", Host: "ghcr.m.daocloud.io"},
	{ID: "daocloud-quay", Name: "DaoCloud Quay", Host: "quay.m.daocloud.io"},
	{ID: "daocloud-nvcr", Name: "DaoCloud NVCR", Host: "nvcr.m.daocloud.io"},
}

// MirrorByID looks up a built-in mirror; ok is false when id is unknown.
func MirrorByID(id string) (Mirror, bool) {
	for _, m := range BuiltinMirrors {
		if m.ID == id {
			return m, true
		}
	}
	return Mirror{}, false
}

// MirrorByHost looks up a built-in mirror by its host.
func MirrorByHost(host string) (Mirror, bool) {
	for _, m := range BuiltinMirrors {
		if strings.EqualFold(m.Host, host) {
			return m, true
		}
	}
	return Mirror{}, false
}

// DefaultMirror is the mirror a reference with no host resolves to.
func DefaultMirror() Mirror {
	for _, m := range BuiltinMirrors {
		if m.IsDefault {
			return m
		}
	}
	return BuiltinMirrors[0]
}

// Registry is a public registry the tool can pull from DIRECTLY, as opposed
// to BuiltinMirrors, which are pull-through accelerators of Docker Hub.
//
// The distinction matters: a mirror only ever proxies Docker Hub's namespace
// (so "nginx" means "library/nginx" there), while these have their own
// namespaces ("prometheus/prometheus" on quay.io is NOT "library/…"). Pulling
// from them already worked before they were listed — Parse accepts any host —
// but nothing in the UI said so, and the Docker Hub-specific "library/" rule
// must not leak onto them.
type Registry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	// SearchID names the Searcher that can search this registry, or "" when
	// it publishes no search API. GHCR, for instance, has none: its packages
	// are addressed by owner/repo, and DockerPull lists their tags instead
	// (see `dockerpull tags`).
	SearchID string `json:"searchId,omitempty"`
	Note     string `json:"note,omitempty"`
}

// BuiltinRegistries are the widely used public registries offered as pull
// sources out of the box. Every entry was verified reachable and to speak the
// standard v2 token flow; adding more is a one-line change. Users can add
// their own (an internal Harbor, say) through the sources table.
var BuiltinRegistries = []Registry{
	{ID: "dockerhub", Name: "Docker Hub 官方", Host: DefaultRegistry, SearchID: "dockerhub"},
	{ID: "ghcr", Name: "GitHub Container Registry", Host: "ghcr.io",
		Note: "无公开搜索接口，按 owner/repo 直接拉取，可用 tags 查标签"},
	{ID: "quay", Name: "Quay.io (Red Hat)", Host: "quay.io", SearchID: "quay"},
	{ID: "k8s", Name: "Kubernetes registry.k8s.io", Host: "registry.k8s.io",
		Note: "K8s 官方镜像仓库（kube-apiserver 等）"},
	{ID: "mcr", Name: "Microsoft Container Registry", Host: "mcr.microsoft.com",
		SearchID: "mcr",
		Note:     "无关键词搜索 API，但 /v2/_catalog 匿名可读，用它做本地关键词搜索"},
	{ID: "ecr-public", Name: "Amazon ECR Public", Host: "public.ecr.aws",
		Note: "无匿名搜索接口（/v2/_catalog 返回 401），按仓库全称直接拉取"},
	{ID: "nvcr", Name: "NVIDIA NGC", Host: "nvcr.io",
		Note: "无匿名搜索接口（API 需认证），按 owner/repo 直接拉取"},
	{ID: "gitlab", Name: "GitLab Container Registry", Host: "registry.gitlab.com",
		Note: "只能按项目定位镜像，没有全局镜像搜索"},
	{ID: "chainguard", Name: "Chainguard (cgr.dev)", Host: "cgr.dev",
		Note: "无公开搜索接口，按镜像全称直接拉取"},
	{ID: "gcr", Name: "Google Container Registry (旧)", Host: "gcr.io",
		Note: "已被 Artifact Registry 取代，仍可拉取历史镜像"},
}

// RegistryByID looks up a built-in registry by its stable id.
func RegistryByID(id string) (Registry, bool) {
	for _, r := range BuiltinRegistries {
		if r.ID == id {
			return r, true
		}
	}
	return Registry{}, false
}

// RegistryByHost looks up a built-in registry by host, tolerating the Docker
// Hub aliases.
func RegistryByHost(host string) (Registry, bool) {
	want := NormalizeAuthHost(host)
	for _, r := range BuiltinRegistries {
		if NormalizeAuthHost(r.Host) == want {
			return r, true
		}
	}
	return Registry{}, false
}

// PullHost turns whatever the user typed into a registry host.
//
// The `--mirror` flag and the API's `registry` field are one knob, and users
// reasonably reach for any of these spellings:
//
//	"1ms"                 a built-in Docker Hub accelerator id
//	"docker.1ms.run"      …or its host
//	"nju" / "南大镜像源"   another accelerator, by id or name
//	"mcr" / "quay"        an UPSTREAM REGISTRY id  <- this used to fail
//	"quay.io"             …or its host
//	"mcr.microsoft.com"
//	"harbor.internal:5000" anything else that looks like a host
//
// Resolving only mirror ids meant `--mirror mcr` and `--mirror quay` reached
// DNS as a hostname, failing with "lookup mcr: no such host" — a confusing way
// to say "that is a known source, just spelled as an id". ok is false when the
// value is neither a known source nor host-shaped, so a caller can report it
// rather than silently pulling from the default.
func PullHost(idOrHost string) (string, bool) {
	v := strings.TrimSpace(idOrHost)
	if v == "" {
		return "", false
	}
	// A URL is not an id: strip the scheme and any path so a pasted
	// "https://quay.io/…" still resolves to the host.
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	if i := strings.IndexAny(v, "/?#"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return "", false
	}

	if mir, ok := MirrorByID(v); ok {
		return mir.Host, true
	}
	for _, mir := range BuiltinMirrors {
		if strings.EqualFold(mir.Name, v) {
			return mir.Host, true
		}
	}
	if reg, ok := RegistryByID(v); ok {
		return reg.Host, true
	}
	for _, reg := range BuiltinRegistries {
		if strings.EqualFold(reg.Name, v) {
			return reg.Host, true
		}
	}
	// A search source id/name ("1ms 加速源", "MCR (Microsoft)"): only some
	// sources pin a host, so this cannot be the last word.
	if host := PullHostForSearchSource(v); host != "" {
		return host, true
	}
	// Finally: does it look like a host at all? Docker's own heuristic — a dot,
	// a port, or localhost — so a bare typo is refused instead of dialled.
	if strings.ContainsAny(v, ".:") || strings.EqualFold(v, "localhost") {
		return v, true
	}
	return "", false
}

// SearcherServesRegistry reports whether a searcher's tag list describes the SAME
// namespace as the registry being inspected, so using it as a fallback is honest.
//
// It is not: the searcher's Docker Hub API answering for a ghcr.io pull. The
// names look alike and the tags do NOT — ghcr.io/linuxcontainers/alpine has 38
// tags while Docker Hub's linuxcontainers/alpine has 42, so a fallback silently
// offers tags the download cannot have. The two are only interchangeable within
// one namespace family: Docker Hub and its accelerators, or the registry that
// owns the searcher (quay.io for the Quay searcher).
func SearcherServesRegistry(host, searcherID string) bool {
	host = strings.TrimSpace(host)
	searcherID = strings.TrimSpace(searcherID)
	if host == "" || searcherID == "" {
		return false
	}
	pinned := PullHostForSearchSource(searcherID)
	if pinned == "" {
		// The searcher pins no host, i.e. it is Docker Hub-family (Hub's own
		// search API, or a Hub-compatible accelerator). Its namespace is Docker
		// Hub's, so the target must be Docker Hub's too.
		return isDockerHubFamily(host)
	}
	if NormalizeAuthHost(pinned) == NormalizeAuthHost(host) {
		return true
	}
	// Different hosts can still be the same namespace: an accelerator serves
	// Docker Hub's, so 1ms's tag list describes registry-1.docker.io correctly.
	return isDockerHubFamily(pinned) && isDockerHubFamily(host)
}

// isDockerHubFamily reports whether a host serves Docker Hub's namespace: Hub
// itself (under any alias) or one of its accelerators.
func isDockerHubFamily(host string) bool {
	if reg, ok := RegistryByHost(host); ok {
		return reg.ID == "dockerhub"
	}
	if _, ok := MirrorByHost(host); ok {
		return true
	}
	return NormalizeAuthHost(host) == "docker.io"
}

// PullHostForSearchSource returns the registry host that can SERVE a search
// source's results, or "" when the caller should fall back to its configured
// default mirror.
//
// This exists because a search result is a NAME, not a location. Searching
// "nginx" on 1ms returns "library/nginx" — a Docker Hub repository name — so a
// pull that merely parses that name reaches registry-1.docker.io, which is
// exactly the host the user was trying to avoid by choosing 1ms. The search
// source has to travel with the result into the pull.
//
// The rules, in order:
//
//  1. A public registry that owns its own namespace (quay.io) serves its own
//     search results, so its host is returned. Result names there
//     ("prometheus/prometheus") are meaningless on a Docker Hub mirror.
//  2. A built-in Docker Hub accelerator (1ms → docker.1ms.run) serves the
//     results of the source with the same id or name.
//  3. The Docker Hub official entry returns "" — NOT its hard-coded
//     registry-1.docker.io. Docker Hub's own search says nothing about which
//     accelerator the operator prefers, so the caller's configured default
//     mirror is the better answer.
//  4. Anything else (a user-added search source) returns "", i.e. the default.
func PullHostForSearchSource(sourceID string) string {
	id := strings.ToLower(strings.TrimSpace(sourceID))
	if id == "" {
		return ""
	}
	// A caller may hold the SEARCH SOURCE's identity rather than a registry's:
	// the store seeds rows from BuiltinSearchSources, whose names differ from the
	// matching registry's ("MCR (Microsoft)" vs "Microsoft Container Registry").
	// Fold those names onto their ids first, so either spelling resolves.
	for _, src := range BuiltinSearchSources {
		if strings.EqualFold(src.ID, id) || strings.EqualFold(src.Name, id) {
			id = strings.ToLower(src.ID)
			break
		}
	}
	// 1. Own-namespace registries. Matched by SearchID (what the CLI passes)
	// or by Name — the store seeds built-ins by NAME, so a caller holding a row
	// rather than a built-in id still resolves.
	for _, reg := range BuiltinRegistries {
		if reg.ID == "dockerhub" || reg.SearchID == "" {
			continue
		}
		if strings.EqualFold(reg.SearchID, id) || strings.EqualFold(reg.Name, id) {
			return reg.Host
		}
	}
	// 2. Docker Hub accelerators. The default entry is skipped on purpose
	// (rule 3): returning it would defeat the operator's default-mirror setting.
	for _, mir := range BuiltinMirrors {
		if mir.IsDefault {
			continue
		}
		if strings.EqualFold(mir.ID, id) || strings.EqualFold(mir.Name, id) {
			return mir.Host
		}
	}
	// 3 & 4. Defer to the caller's configured default mirror.
	return ""
}

// dockerHubAliases are the hostnames that all mean "Docker Hub". A credential
// saved against one of them must be found when a reference names another:
// users type `docker.io`, the daemon talks to `registry-1.docker.io`, and
// `docker login` writes `https://index.docker.io/v1/`.
var dockerHubAliases = map[string]bool{
	"docker.io":                   true,
	"index.docker.io":             true,
	"registry-1.docker.io":        true,
	"registry.docker.io":          true,
	"https://index.docker.io/v1/": true,
}

// NormalizeAuthHost reduces a registry host to one canonical form, so a
// stored credential is found regardless of the spelling the user (or the
// reference) used.
//
// It lower-cases, strips a scheme and any path, and folds Docker Hub's
// aliases onto "docker.io". An empty host means Docker Hub, because that is
// what a bare `nginx` reference resolves to.
func NormalizeAuthHost(host string) string {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "" {
		return "docker.io"
	}
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	// Trailing slash and any path are not part of the host. A registry host
	// may legitimately carry a port, so only "/" is a separator here.
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[:i]
	}
	h = strings.TrimSuffix(h, "/")
	if dockerHubAliases[h] {
		return "docker.io"
	}
	return h
}
