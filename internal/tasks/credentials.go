package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/registry"
	"github.com/Potterluo/docker-pull-tar/internal/secrets"
	"github.com/Potterluo/docker-pull-tar/internal/store"
)

// Credential kinds.
const (
	CredentialBasic = "basic"
	CredentialToken = "token"
)

// ValidCredentialKind reports whether a client-supplied kind is one we store.
func ValidCredentialKind(kind string) bool {
	switch kind {
	case CredentialBasic, CredentialToken:
		return true
	}
	return false
}

// CredentialView is a stored login as the API and CLI may see it.
//
// It deliberately has no Secret field: the sealed text never leaves the store
// layer, so no handler can leak it by forgetting to strip a field, and a
// credential can never be read back out to a client in any form.
type CredentialView struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	Kind      string    `json:"kind"`
	Note      string    `json:"note"`
	HasSecret bool      `json:"hasSecret"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ErrNoProtector means credentials cannot be stored or read on this machine
// (the platform provides no usable encryption facility). Pulls with explicit
// -u/-p still work; only the stored-credential feature is unavailable.
var ErrNoProtector = errors.New("本机无法安全地保存凭证")

// protectorOnce guards the lazy construction of the secret protector. The
// Manager owns the ONLY protector in the process, so there is exactly one
// place that derives or loads a key.
var (
	protectorOnce sync.Once
	protectorInst secrets.Protector
	protectorErr  error
)

// protector returns the manager's secret protector, building it on first use.
//
// Lazy because a failure here must not stop the app from booting: a machine
// where DPAPI is unavailable can still pull public images, and reporting that
// at first USE is far more useful than refusing to start.
func (m *Manager) protector() (secrets.Protector, error) {
	protectorOnce.Do(func() {
		protectorInst, protectorErr = secrets.New(m.dataDir)
	})
	if protectorErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoProtector, protectorErr)
	}
	if protectorInst == nil {
		return nil, ErrNoProtector
	}
	return protectorInst, nil
}

// ProtectorLabel describes how secrets are protected, for the settings page.
func (m *Manager) ProtectorLabel() string {
	p, err := m.protector()
	if err != nil {
		return "不可用：" + err.Error()
	}
	return p.Describe()
}

// SaveCredential stores (or replaces) the login for a host.
//
// The host is normalised through registry.NormalizeAuthHost first, so
// "docker.io", "registry-1.docker.io" and "https://index.docker.io/v1/" are one
// entry rather than three that each miss on lookup.
func (m *Manager) SaveCredential(ctx context.Context, host, username, secret, kind, note string) (*store.Credential, error) {
	host = registry.NormalizeAuthHost(host)
	username = strings.TrimSpace(username)
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = CredentialBasic
	}
	if !ValidCredentialKind(kind) {
		return nil, fmt.Errorf("凭证类型必须是 basic 或 token")
	}
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("密码/令牌不能为空")
	}
	// A token login has no username in some registries; everything else needs
	// one, and a silent empty username produces an auth failure that looks
	// like a wrong password.
	if kind == CredentialBasic && username == "" {
		return nil, fmt.Errorf("basic 类型需要用户名")
	}

	p, err := m.protector()
	if err != nil {
		return nil, err
	}
	sealed, err := p.Seal([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("加密凭证失败: %w", err)
	}

	// Replace-by-host: one login per registry, which is what every registry
	// client in the wild assumes. Updating in place keeps the id stable so a
	// GUI holding a row reference does not go stale.
	existing, findErr := m.st.FindCredentialByHost(ctx, host)
	switch {
	case findErr == nil && existing != nil:
		existing.Username = username
		existing.Secret = sealed
		existing.Kind = kind
		existing.Note = note
		if err := m.st.UpdateCredential(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	case errors.Is(findErr, store.ErrNotFound):
		c := &store.Credential{
			Host:     host,
			Username: username,
			Secret:   sealed,
			Kind:     kind,
			Note:     note,
		}
		if err := m.st.CreateCredential(ctx, c); err != nil {
			// A concurrent create is the only realistic cause; surface it
			// rather than silently overwriting someone else's row.
			if errors.Is(err, store.ErrDuplicate) {
				return nil, fmt.Errorf("该仓库 %s 已存在凭证，请刷新后重试", host)
			}
			return nil, err
		}
		return c, nil
	default:
		return nil, findErr
	}
}

// ListCredentials returns every stored login, without secrets.
func (m *Manager) ListCredentials(ctx context.Context) ([]CredentialView, error) {
	rows, err := m.st.ListCredentials(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CredentialView, 0, len(rows))
	for i := range rows {
		out = append(out, credentialView(&rows[i]))
	}
	return out, nil
}

// DeleteCredential removes a stored login by id.
func (m *Manager) DeleteCredential(ctx context.Context, id string) error {
	return m.st.DeleteCredential(ctx, id)
}

// CredentialUpdate describes an edit that may or may not replace the secret.
type CredentialUpdate struct {
	Host     string
	Username string
	Kind     string
	Note     string
	// NewSecret replaces the stored one when non-empty. Empty means "keep the
	// existing sealed value", which is what lets the UI edit a note or rename a
	// host WITHOUT the browser ever holding the password — requiring it to
	// re-send the secret would mean the client had to keep a copy of it.
	NewSecret string
}

// UpdateCredential edits a stored login in place.
//
// Changing the host is supported (fixing a typo, moving a login from
// "localhost:5000" to a real name). It is refused when another entry already
// owns the target host, rather than silently merging two logins into one.
func (m *Manager) UpdateCredential(ctx context.Context, id string, up CredentialUpdate) (*store.Credential, error) {
	row, err := m.st.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}

	host := registry.NormalizeAuthHost(firstNonBlank(up.Host, row.Host))
	if host != row.Host {
		if other, err := m.st.FindCredentialByHost(ctx, host); err == nil && other.ID != id {
			return nil, fmt.Errorf("%s 已存在凭证，请先删除再修改", host)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		row.Host = host
	}

	kind := firstNonBlank(up.Kind, row.Kind)
	if !ValidCredentialKind(kind) {
		return nil, fmt.Errorf("凭证类型必须是 basic 或 token")
	}
	row.Kind = kind
	if strings.TrimSpace(up.Username) != "" {
		row.Username = strings.TrimSpace(up.Username)
	}
	if up.Note != "" {
		row.Note = up.Note
	}
	if kind == CredentialBasic && row.Username == "" {
		return nil, fmt.Errorf("basic 类型需要用户名")
	}

	if strings.TrimSpace(up.NewSecret) != "" {
		p, err := m.protector()
		if err != nil {
			return nil, err
		}
		sealed, err := p.Seal([]byte(up.NewSecret))
		if err != nil {
			return nil, fmt.Errorf("加密凭证失败: %w", err)
		}
		row.Secret = sealed
	} else if row.Secret == "" {
		// Nothing to keep and nothing to set: an entry with no secret cannot
		// authenticate, so refuse rather than save a login that always fails.
		return nil, fmt.Errorf("缺少密码/令牌")
	}

	if err := m.st.UpdateCredential(ctx, row); err != nil {
		return nil, err
	}
	return row, nil
}

// firstNonBlank returns a unless it is blank, in which case b.
func firstNonBlank(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}

// DeleteCredentialByHost removes the login for a (possibly unnormalised) host.
func (m *Manager) DeleteCredentialByHost(ctx context.Context, host string) error {
	row, err := m.st.FindCredentialByHost(ctx, registry.NormalizeAuthHost(host))
	if err != nil {
		return err
	}
	return m.st.DeleteCredential(ctx, row.ID)
}

// CredentialsFor resolves the login to use for a registry host.
//
// Precedence: an explicit per-invocation credential always wins (the user
// typed it, so it must not be second-guessed), then a stored one. The bool
// reports whether anything was found, so the caller can tell "no credentials
// configured" from "credentials that failed to open" — the first is normal for
// public images, the second must be reported.
func (m *Manager) CredentialsFor(ctx context.Context, host string, explicit registry.Credentials) (registry.Credentials, bool, error) {
	if !explicit.Empty() {
		return explicit, true, nil
	}
	row, err := m.st.FindCredentialByHost(ctx, registry.NormalizeAuthHost(host))
	if errors.Is(err, store.ErrNotFound) {
		return registry.Credentials{}, false, nil
	}
	if err != nil {
		return registry.Credentials{}, false, err
	}
	p, err := m.protector()
	if err != nil {
		return registry.Credentials{}, false, err
	}
	plain, err := p.Open(row.Secret)
	if err != nil {
		// The sealed blob is unreadable: a different user account (DPAPI is
		// per-user) or a replaced machine key. Say which host, never the value.
		return registry.Credentials{}, false, fmt.Errorf("无法解密 %s 的凭证（可能是在其它账户下保存的），请重新登录: %w", row.Host, err)
	}
	return registry.Credentials{Username: row.Username, Password: string(plain)}, true, nil
}

// credentialView strips the secret. Kept as one function so there is a single
// place where a Credential becomes externally visible.
func credentialView(c *store.Credential) CredentialView {
	return CredentialView{
		ID:        c.ID,
		Host:      c.Host,
		Username:  c.Username,
		Kind:      c.Kind,
		Note:      c.Note,
		HasSecret: c.Secret != "",
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
}
