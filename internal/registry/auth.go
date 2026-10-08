package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// AuthChallenge is a parsed WWW-Authenticate header.
type AuthChallenge struct {
	Scheme  string `json:"scheme"` // "bearer" | "basic" | "none" | "unknown"
	Realm   string `json:"realm"`
	Service string `json:"service"`
	Scope   string `json:"scope"`
}

// Credentials are registry credentials. Both fields must be set to be used.
type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Empty reports whether no usable credentials were supplied.
func (c Credentials) Empty() bool { return c.Username == "" || c.Password == "" }

// BasicHeader is the HTTP Basic value for these credentials.
func (c Credentials) BasicHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password))
}

// Redacted renders credentials safely for logs.
func (c Credentials) Redacted() string {
	if c.Empty() {
		return "(匿名)"
	}
	return c.Username + ":***"
}

// Regexes for the challenge parameters. Parameter values are quoted by
// every real registry; the unquoted form is tolerated by making the quotes
// optional, within the same token (a naive split('"') breaks on Basic
// challenges with no parameters at all — the legacy bug this replaces).
var (
	realmRe   = regexp.MustCompile(`(?i)\brealm\s*=\s*"([^"]*)"|(?i)\brealm\s*=\s*([^\s,]+)`)
	serviceRe = regexp.MustCompile(`(?i)\bservice\s*=\s*"([^"]*)"|(?i)\bservice\s*=\s*([^\s,]+)`)
	scopeRe   = regexp.MustCompile(`(?i)\bscope\s*=\s*"([^"]*)"|(?i)\bscope\s*=\s*([^\s,]+)`)
)

func firstGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	for i := 1; i < len(m); i++ {
		if m[i] != "" {
			return m[i]
		}
	}
	return ""
}

// ParseWWWAuthenticate parses a WWW-Authenticate header.
//
// A missing header yields Scheme "none"; an unrecognised scheme yields
// "unknown". Neither case panics, and Basic-with-no-parameters is handled
// (it is the shape that used to IndexError).
func ParseWWWAuthenticate(header string) AuthChallenge {
	header = strings.TrimSpace(header)
	if header == "" {
		return AuthChallenge{Scheme: "none"}
	}
	lower := strings.ToLower(header)
	switch {
	case strings.HasPrefix(lower, "bearer"):
		return AuthChallenge{
			Scheme:  "bearer",
			Realm:   firstGroup(realmRe, header),
			Service: firstGroup(serviceRe, header),
			Scope:   firstGroup(scopeRe, header),
		}
	case strings.HasPrefix(lower, "basic"):
		return AuthChallenge{Scheme: "basic", Realm: firstGroup(realmRe, header)}
	default:
		scheme := header
		if i := strings.IndexAny(scheme, " \t"); i >= 0 {
			scheme = scheme[:i]
		}
		return AuthChallenge{Scheme: "unknown", Realm: scheme}
	}
}

// tokenResponse covers the token endpoint's response shape. Docker Hub
// returns `token`; GitLab's registry returns `access_token` for the same
// thing, so both are accepted.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func (t tokenResponse) bearer() string {
	if t.Token != "" {
		return t.Token
	}
	return t.AccessToken
}

// buildTokenURL assembles the token endpoint URL, preserving a query string
// already present in the realm (the legacy code concatenated "?service="
// unconditionally and broke on realms that carry parameters).
func buildTokenURL(realm, service, scope string) (string, error) {
	if realm == "" {
		return "", fmt.Errorf("认证响应缺少 realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("认证 realm 无效 (%s): %w", realm, err)
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// tokenScope decides which scope to ask the token service for.
//
// The repository we are about to fetch always wins. A challenge MAY carry a
// scope, but a challenge is also issued for the plain /v2/ ping — and there it
// is a TEMPLATE: ghcr.io answers the ping with
// `scope="repository:user/image:pull"`, and asking for that verbatim requests a
// token to a repository that does not exist. GitHub reports it as 403 DENIED
// rather than 401, so it does not even read as an auth problem. Docker Hub and
// Quay send no scope on the ping, which is why only GHCR broke.
//
// The challenge's scope is used only when there is no repository to name (a
// bare probe).
func tokenScope(challengeScope, repository string) string {
	if repository = strings.TrimSpace(repository); repository != "" {
		return "repository:" + repository + ":pull"
	}
	return challengeScope
}

// fetchToken performs the token exchange for a bearer challenge.
func (c *Client) fetchToken(ch *AuthChallenge, repoScope string, creds Credentials) (string, error) {
	// The scope must name the repository we are about to fetch.
	//
	// A challenge MAY carry one, but a challenge is also issued for the plain
	// /v2/ ping — and there it is a TEMPLATE. ghcr.io answers the ping with
	// `scope="repository:user/image:pull"`, and using that verbatim asks for a
	// token to a repository that does not exist, which GitHub reports as
	// 403 DENIED rather than 401 (so it does not even look like an auth
	// problem). Docker Hub and Quay send no scope on the ping, which is why this
	// only ever broke GHCR. Derive the scope from the reference; fall back to the
	// challenge's only when we have no repository to name (a bare probe).
	scope := tokenScope(ch.Scope, repoScope)
	tokenURL, err := buildTokenURL(ch.Realm, ch.Service, scope)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if !creds.Empty() {
		req.Header.Set("Authorization", creds.BasicHeader())
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求认证服务失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取认证响应失败: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("仓库认证失败（401）：请检查用户名和密码")
	}
	if resp.StatusCode != http.StatusOK {
		// Name the endpoint: "the auth service refused" is unactionable on its
		// own, and the URL is what tells you whether the challenge was parsed
		// into the right scope (ghcr.io answers 403 DENIED rather than 401 when
		// the requested scope is not something anonymous access covers).
		return "", fmt.Errorf("认证服务返回 %d (%s): %s",
			resp.StatusCode, tokenURL, truncate(string(body), 200))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("解析认证响应失败: %w", err)
	}
	tok := tr.bearer()
	if tok == "" {
		return "", fmt.Errorf("认证响应中未包含 token 字段")
	}
	return tok, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
