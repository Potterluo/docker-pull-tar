package registry

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientOptions configures the HTTP client used for every registry call.
type ClientOptions struct {
	// Credentials are used for both bearer token exchange and Basic auth.
	Credentials Credentials

	// ProxyURL: "" honours HTTP(S)_PROXY from the environment, "-" forces a
	// direct connection, anything else is an explicit proxy URL.
	ProxyURL string

	// InsecureTLS skips TLS certificate verification. Default false —
	// verification is ON unless explicitly disabled. (The legacy tool
	// hard-disabled it on every request with no way to turn it back on.)
	InsecureTLS bool

	// Timeout is the per-request timeout for metadata calls. Streaming
	// blob bodies use a longer, separate deadline. 0 means the defaults.
	Timeout time.Duration

	// UserAgent overrides the outgoing User-Agent.
	UserAgent string
}

// Default timeouts.
const (
	defaultMetaTimeout = 60 * time.Second
	defaultUserAgent   = "dockerpull/1.0 (+https://github.com/Potterluo/docker-pull-tar)"
)

// Client talks to registries. It is safe for concurrent use.
type Client struct {
	httpClient  *http.Client
	opts        ClientOptions
	userAgent   string
	metaTimeout time.Duration

	mu    sync.Mutex
	cache map[string]cachedAuth
}

type cachedAuth struct {
	header    string
	expiresAt time.Time
}

// NewClient builds a client. The returned client owns its own
// http.Client (with its own transport) so proxy/TLS settings are applied
// per task, never process-wide.
func NewClient(opts ClientOptions) *Client {
	proxyFn := http.ProxyFromEnvironment
	switch {
	case opts.ProxyURL == "-":
		proxyFn = nil
	case strings.TrimSpace(opts.ProxyURL) != "":
		if u, err := url.Parse(opts.ProxyURL); err == nil {
			proxyFn = http.ProxyURL(u)
		}
	}

	transport := &http.Transport{
		Proxy:                 proxyFn,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
		// Registries are frequently fronted by caches that reject
		// compressed blob responses; gzip on metadata is fine.
		DisableCompression: false,
	}
	if opts.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit user opt-in
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultMetaTimeout
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}

	return &Client{
		// No client-level Timeout: blob downloads stream for minutes and a
		// fixed client timeout would kill them. Per-request deadlines come
		// from the context, which callers control.
		httpClient: &http.Client{
			Transport: transport,
			// Follow redirects to the blob CDN. Go drops the
			// Authorization header on a cross-host redirect, which is
			// exactly what S3 presigned URLs require.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("重定向次数过多")
				}
				return nil
			},
		},
		opts:        opts,
		userAgent:   ua,
		metaTimeout: timeout,
		cache:       make(map[string]cachedAuth),
	}
}

// Credentials returns the client's configured credentials.
func (c *Client) Credentials() Credentials { return c.opts.Credentials }

func (c *Client) cacheKey(ref Ref) string {
	return ref.Registry + "|" + ref.Repository + "|" + c.opts.Credentials.Username
}

// AuthHeader returns the Authorization header value for ref, performing the
// ping/token dance on first use and caching the result. An anonymous
// registry yields "".
func (c *Client) AuthHeader(ctx context.Context, ref Ref) (string, error) {
	key := c.cacheKey(ref)
	c.mu.Lock()
	if ca, ok := c.cache[key]; ok && (ca.expiresAt.IsZero() || time.Now().Before(ca.expiresAt)) {
		c.mu.Unlock()
		return ca.header, nil
	}
	c.mu.Unlock()

	header, ttl, err := c.negotiateAuth(ctx, ref)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.cache[key] = cachedAuth{header: header, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
	return header, nil
}

// InvalidateAuth drops the cached credential for ref so the next call
// renegotiates. Called after a 401 on a route that was previously fine.
func (c *Client) InvalidateAuth(ref Ref) {
	c.mu.Lock()
	delete(c.cache, c.cacheKey(ref))
	c.mu.Unlock()
}

// negotiateAuth performs GET /v2/ and resolves the challenge. It returns
// the header value plus how long it may be cached.
func (c *Client) negotiateAuth(ctx context.Context, ref Ref) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, c.metaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.BaseURL()+"/", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	// Deliberately NO credentials on this probe.
	//
	// A registry that requires Basic auth answers 200 *because* it
	// authenticated us — it does not echo a challenge on a request that
	// already carried credentials. Sending them here therefore made the
	// client conclude "public registry" and cache an empty header, so every
	// later request went out anonymously and 401'd. Sending none forces the
	// 401 + WWW-Authenticate branch below, which is where the credential
	// decision belongs.

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("连接仓库 %s 失败: %w", ref.Registry, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK && !c.opts.Credentials.Empty():
		// A 200 to an anonymous probe means the registry is public. But when
		// the caller supplied credentials, use them anyway: some registries
		// serve anonymous reads while requiring auth for pulls, and a private
		// registry behind a proxy may not challenge at all. Carrying the
		// credentials costs nothing and avoids a mysterious later 401.
		return c.opts.Credentials.BasicHeader(), 12 * time.Hour, nil

	case resp.StatusCode == http.StatusOK:
		// Public registry: no Authorization needed at all.
		return "", 12 * time.Hour, nil

	case resp.StatusCode == http.StatusUnauthorized:
		ch := ParseWWWAuthenticate(resp.Header.Get("WWW-Authenticate"))
		switch ch.Scheme {
		case "bearer":
			token, err := c.fetchToken(&ch, ref.Repository, c.opts.Credentials)
			if err != nil {
				return "", 0, err
			}
			// Docker Hub tokens last ~5 minutes; use a conservative TTL
			// so a long download does not start failing mid-flight.
			return "Bearer " + token, 4 * time.Minute, nil
		case "basic":
			if c.opts.Credentials.Empty() {
				return "", 0, fmt.Errorf("仓库 %s 需要 Basic 认证，请通过 --username / --password 提供用户名和密码", ref.Registry)
			}
			return c.opts.Credentials.BasicHeader(), 12 * time.Hour, nil
		case "none":
			return "", 0, fmt.Errorf("仓库 %s 需要认证但未返回认证信息", ref.Registry)
		default:
			return "", 0, fmt.Errorf("仓库 %s 返回了不支持的认证方式: %s", ref.Registry, ch.Realm)
		}

	default:
		return "", 0, fmt.Errorf("仓库 %s 健康检查返回 %d", ref.Registry, resp.StatusCode)
	}
}

// doAuthed issues req with the negotiated Authorization header, refreshing
// once on 401 (a stale cached token is the common cause).
func (c *Client) doAuthed(ctx context.Context, ref Ref, build func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			c.InvalidateAuth(ref)
		}
		auth, err := c.AuthHeader(ctx, ref)
		if err != nil {
			return nil, err
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", c.userAgent)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("仓库 %s 认证失败", ref.Registry)
}

// FetchManifest fetches the manifest for reference (a tag or a digest).
func (c *Client) FetchManifest(ctx context.Context, ref Ref, reference string) (*Manifest, error) {
	if reference == "" {
		reference = ref.Reference()
	}
	ctx, cancel := context.WithTimeout(ctx, c.metaTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/%s/manifests/%s", ref.BaseURL(), ref.Repository, reference)
	resp, err := c.doAuthed(ctx, ref, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", AcceptManifestTypes)
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("获取清单失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取清单失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取清单 %s 失败 (%d): %s", reference, resp.StatusCode, truncate(string(body), 200))
	}

	m, err := parseManifest(body, resp.Header.Get("Docker-Content-Digest"))
	if err != nil {
		return nil, err
	}
	if m.MediaType == "" {
		m.MediaType = resp.Header.Get("Content-Type")
	}
	return m, nil
}

// archNotFoundError explains a failed architecture match.
//
// The message must be actionable, because the fix depends on WHY it failed and
// the user cannot see the index:
//
//   - the index lists other platforms  → name them, so the user can pick one
//     (`--arch windows/amd64`).
//   - it lists NONE for that OS        → say so explicitly. Otherwise the
//     message ends in "可用架构:" followed by nothing, which is what a
//     windows-only image used to produce and reads like a bug in the tool
//     rather than a fact about the image.
func archNotFoundError(want Platform, available []Platform) error {
	names := PlatformStrings(available)
	if len(names) == 0 {
		return fmt.Errorf("在清单中找不到架构 %s：该清单没有声明任何平台信息", want.String())
	}
	// Does the index offer anything at all for the requested OS?
	sameOS := false
	for _, p := range available {
		if strings.EqualFold(p.OS, want.OS) {
			sameOS = true
			break
		}
	}
	if !sameOS {
		return fmt.Errorf("在清单中找不到架构 %s：该镜像没有 %s 变体，可用架构: %s（请显式指定，例如 -a %s）",
			want.String(), want.OS, strings.Join(names, ", "), names[0])
	}
	return fmt.Errorf("在清单中找不到架构 %s，可用架构: %s", want.String(), strings.Join(names, ", "))
}

// ResolveImage resolves ref to a concrete image manifest for platform.
//
// When ref points at an index, the matching sub-manifest is fetched by
// digest and its platform is returned (which may differ in spelling from
// the request, e.g. "arm64v8" for "arm64"). When ref already points at an
// image manifest, the platform's *config blob* is read to report what the
// image actually is — an index-less single-arch image must not be
// silently mislabelled, and the legacy tool fetched the manifest twice to
// get this.
func (c *Client) ResolveImage(ctx context.Context, ref Ref, platform Platform) (*Manifest, Platform, error) {
	root, err := c.FetchManifest(ctx, ref, "")
	if err != nil {
		return nil, Platform{}, err
	}

	if !root.IsIndex {
		cfg, err := c.FetchConfig(ctx, ref, root.Config.Digest)
		if err != nil {
			// The manifest is usable even if the config blob is not
			// readable; report the requested platform.
			return root, platform, nil
		}
		actual := cfg.Platform()
		if actual.IsZero() {
			actual = platform
		}
		return root, actual, nil
	}

	entry, ok := root.EntryFor(platform)
	if !ok {
		return nil, Platform{}, archNotFoundError(platform, root.Platforms())
	}

	sub, err := c.FetchManifest(ctx, ref, entry.Digest)
	if err != nil {
		return nil, Platform{}, err
	}
	if sub.IsIndex {
		return nil, Platform{}, fmt.Errorf("清单索引 %s 指向了另一个索引，无法继续解析", entry.Digest)
	}
	return sub, entry.PlatformName(), nil
}

// ScanPlatforms returns the selectable platforms for ref: the index's
// entries for a multi-arch image, or the single platform from the config
// blob for a single-arch one.
func (c *Client) ScanPlatforms(ctx context.Context, ref Ref) ([]Platform, *Manifest, error) {
	root, err := c.FetchManifest(ctx, ref, "")
	if err != nil {
		return nil, nil, err
	}
	if root.IsIndex {
		return root.Platforms(), root, nil
	}
	cfg, err := c.FetchConfig(ctx, ref, root.Config.Digest)
	if err != nil {
		return nil, root, err
	}
	return DedupePlatforms([]Platform{cfg.Platform()}), root, nil
}

// FetchConfig downloads and decodes the image config blob.
func (c *Client) FetchConfig(ctx context.Context, ref Ref, digest string) (*ImageConfig, error) {
	raw, err := c.FetchBlobBytes(ctx, ref, digest, 4<<20)
	if err != nil {
		return nil, err
	}
	return ParseImageConfig(raw)
}

// FetchBlobBytes downloads a small blob fully into memory (config blobs
// only — layers go through OpenBlob).
func (c *Client) FetchBlobBytes(ctx context.Context, ref Ref, digest string, limit int64) ([]byte, error) {
	br, err := c.OpenBlob(ctx, ref, digest, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = br.Body.Close() }()
	if br.StatusCode != http.StatusOK && br.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("下载 %s 失败 (%d)", shortDigest(digest), br.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(br.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", shortDigest(digest), err)
	}
	return body, nil
}

// BlobReader is an open blob stream. The caller must Close it.
//
// StatusCode is significant:
//
//	206 — the Range was honoured; the body starts at the requested offset.
//	200 — the registry ignored the Range; the body is the WHOLE blob and
//	      the caller must truncate its partial file rather than append.
//	416 — the requested offset is already at/past the end; the partial
//	      file is complete.
type BlobReader struct {
	Body          io.ReadCloser
	StatusCode    int
	ContentLength int64
	TotalSize     int64
}

// OpenBlob opens the blob for digest, optionally from an offset. A zero
// offset fetches the whole blob.
func (c *Client) OpenBlob(ctx context.Context, ref Ref, digest string, offset int64) (*BlobReader, error) {
	if offset <= 0 {
		return c.OpenBlobRange(ctx, ref, digest, 0, -1)
	}
	return c.OpenBlobRange(ctx, ref, digest, offset, -1)
}

// OpenBlobRange opens the blob for digest over the inclusive byte range
// [start, end]. end < 0 means "to the end of the blob". This is the
// primitive the puller's chunked downloader builds on.
func (c *Client) OpenBlobRange(ctx context.Context, ref Ref, digest string, start, end int64) (*BlobReader, error) {
	// Blob bodies stream for minutes; do not wrap them in the metadata
	// timeout. Cancellation still works through the caller's context.
	url := fmt.Sprintf("%s/%s/blobs/%s", ref.BaseURL(), ref.Repository, digest)
	resp, err := c.doAuthed(ctx, ref, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/octet-stream")
		switch {
		case start > 0 && end >= start:
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		case start > 0:
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		case end >= 0:
			req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", end))
		}
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", shortDigest(digest), err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("下载 %s 失败 (%d): %s", shortDigest(digest), resp.StatusCode, truncate(string(body), 200))
	}

	br := &BlobReader{
		Body:          resp.Body,
		StatusCode:    resp.StatusCode,
		ContentLength: resp.ContentLength,
	}
	if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
		br.TotalSize = total
	} else if resp.StatusCode == http.StatusOK {
		br.TotalSize = resp.ContentLength
	}
	return br, nil
}

// ListTags returns the tags the REGISTRY being talked to reports for a
// repository (GET /v2/<repo>/tags/list).
//
// This is the authoritative tag list for the host a pull will actually use,
// which is why the inspect handler prefers it. A searcher is frequently a
// different registry entirely — Docker Hub's API for a ghcr.io or quay.io pull —
// so asking it for the tags of a same-looking `owner/repo` name can offer a tag
// from another publisher's image, and the download then fails on a tag the user
// believed existed.
//
// A registry that does not implement tags/list (or answers 404) is reported as
// an error, so the caller can fall back to a searcher rather than claiming the
// repository has no tags.
func (c *Client) ListTags(ctx context.Context, ref Ref) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.metaTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/%s/tags/list?n=200", ref.BaseURL(), ref.Repository)
	resp, err := c.doAuthed(ctx, ref, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("获取标签列表失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取标签列表失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取标签列表失败 (%d): %s", resp.StatusCode, truncate(string(body), 200))
	}
	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析标签列表失败: %w", err)
	}
	return c.ensureDefaultTags(ctx, ref, payload.Tags), nil
}

// orderTags makes the registry's tag list usable as a picker.
//
// The distribution spec does not define the order of tags/list, and in practice
// it is lexicographic. Everything keeps the registry's own order — a fact we
// have no better information about than the registry does — while
// ensureDefaultTags lifts the two tags a user is most likely to want to the
// front.
func orderTags(tags []string) []string {
	return tags
}

// ensureDefaultTags guarantees that the tag the caller asked for and `latest`
// are present and first, probing for them when the page did not include them.
//
// tags/list is lexicographic AND paginated, so a single page can contain
// neither: `library/nginx` has thousands of tags, and no 200-entry window
// starting at "1" reaches "latest". The picker then opened on "1" and did not
// offer latest at all — for the most commonly pulled tag there is. Each missing
// candidate costs one HEAD.
func (c *Client) ensureDefaultTags(ctx context.Context, ref Ref, tags []string) []string {
	candidates := make([]string, 0, 2)
	if t := strings.TrimSpace(ref.Tag); t != "" && t != "latest" {
		candidates = append(candidates, t)
	}
	candidates = append(candidates, "latest")

	seen := make(map[string]bool, len(tags)+len(candidates))
	out := make([]string, 0, len(tags)+len(candidates))
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, want := range candidates {
		if want == "" {
			continue
		}
		if containsString(tags, want) || c.tagExists(ctx, ref, want) {
			add(want)
		}
	}
	for _, t := range tags {
		add(t)
	}
	return out
}

// tagExists reports whether the repository really has that tag.
func (c *Client) tagExists(ctx context.Context, ref Ref, tag string) bool {
	ctx, cancel := context.WithTimeout(ctx, c.metaTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/%s/manifests/%s", ref.BaseURL(), ref.Repository, tag)
	resp, err := c.doAuthed(ctx, ref, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", AcceptManifestTypes)
		return req, nil
	})
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// OpenBlobURLRange opens a blob from an absolute URL (a foreign layer's
// declared location) over the inclusive byte range [start, end], where end < 0
// means "to the end" and start < 0 means "the whole blob".
//
// It is deliberately NOT authenticated. A foreign layer lives on a third-party
// CDN (go.microsoft.com for a Windows base image), and sending the registry's
// bearer token there would leak a credential to an unrelated host — as well as
// breaking pre-signed URLs, which reject an unexpected Authorization header.
//
// The URLs are tried in order, because a manifest may list several mirrors of
// the same blob. A non-2xx from the last one is returned as an error; the
// caller still verifies the digest, so a CDN serving the wrong bytes is caught
// downstream rather than trusted here.
func (c *Client) OpenBlobURLRange(ctx context.Context, urls []string, digest string, start, end int64) (*BlobReader, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("层 %s 没有可用的下载地址", shortDigest(digest))
	}
	var lastErr error
	for _, raw := range urls {
		br, err := c.openURLRange(ctx, raw, digest, start, end)
		if err == nil {
			return br, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Client) openURLRange(ctx context.Context, raw, digest string, start, end int64) (*BlobReader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("无效的下载地址 %q: %w", raw, err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/octet-stream")
	switch {
	case start > 0 && end >= start:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	case start > 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	case end >= 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", end))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", hostOf(raw), err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent &&
		resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("下载 %s 失败 (%d): %s", shortDigest(digest), resp.StatusCode, truncate(string(body), 200))
	}

	br := &BlobReader{
		Body:          resp.Body,
		StatusCode:    resp.StatusCode,
		ContentLength: resp.ContentLength,
	}
	if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
		br.TotalSize = total
	} else if resp.StatusCode == http.StatusOK {
		br.TotalSize = resp.ContentLength
	}
	return br, nil
}

// hostOf renders just the host of a URL for an error message, so a failure
// names the CDN the user cannot reach instead of a 200-character link.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// HeadBlob reports a blob's size, or 0 when the registry does not answer
// HEAD with a length. It never fails hard: an unknown size is recoverable,
// a failed download is not.
func (c *Client) HeadBlob(ctx context.Context, ref Ref, digest string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/%s/blobs/%s", ref.BaseURL(), ref.Repository, digest)
	resp, err := c.doAuthed(ctx, ref, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	})
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	if resp.StatusCode != http.StatusOK {
		return 0, nil
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength, nil
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, nil
}

// parseContentRangeTotal extracts the total from
// "bytes 400-999/1000".
func parseContentRangeTotal(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	slash := strings.LastIndex(v, "/")
	if slash < 0 {
		return 0
	}
	total := strings.TrimSpace(v[slash+1:])
	if total == "*" || total == "" {
		return 0
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// shortDigest renders a digest for logs.
func shortDigest(digest string) string {
	s := strings.TrimPrefix(digest, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "blob"
	}
	return s
}
