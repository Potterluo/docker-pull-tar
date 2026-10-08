package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/store"
	"github.com/Potterluo/docker-pull-tar/internal/tasks"
)

// credentialRequest is the body of POST/PUT /api/credentials.
//
// Secret is accepted on the way IN and NEVER returned on the way out: a stored
// password is deliberately not readable back through the API. To change one,
// re-save it.
type credentialRequest struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Secret   string `json:"secret"`
	Kind     string `json:"kind"`
	Note     string `json:"note"`
}

// handleListCredentials returns the stored registry logins, without secrets.
//
//	GET /api/credentials
func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	list, err := s.tasks.ListCredentials(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeOK(w, map[string]any{
		"credentials": list,
		// How secrets are protected on this machine, so the UI can state it
		// rather than imply more protection than exists (DPAPI on Windows, a
		// key file with weaker guarantees elsewhere).
		"protection": s.tasks.ProtectorLabel(),
	})
}

// handleCreateCredential stores a login for a registry host.
//
//	POST /api/credentials
//
// Upserts by host: one login per registry, which is what every registry client
// assumes. Re-posting a host replaces its credentials.
func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var req credentialRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Host) == "" {
		writeError(w, http.StatusBadRequest, "缺少仓库地址（host）")
		return
	}
	c, err := s.tasks.SaveCredential(r.Context(), req.Host, req.Username, req.Secret, req.Kind, req.Note)
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	s.hub.Publish(eventOf(tasks.EventSettingsChanged, map[string]any{
		"credentials": true, "host": c.Host,
	}))
	writeOK(w, map[string]any{"credential": viewOf(c)})
}

// handleUpdateCredential edits the login identified by {id}.
//
//	PUT /api/credentials/{id}
//
// An omitted secret KEEPS the stored one: the browser never held it, so
// requiring it back would mean the client had to keep a copy of the password.
func (s *Server) handleUpdateCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req credentialRequest
	if !readJSON(w, r, &req) {
		return
	}
	saved, err := s.tasks.UpdateCredential(r.Context(), id, tasks.CredentialUpdate{
		Host:      req.Host,
		Username:  req.Username,
		Kind:      req.Kind,
		Note:      req.Note,
		NewSecret: req.Secret,
	})
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	s.hub.Publish(eventOf(tasks.EventSettingsChanged, map[string]any{
		"credentials": true, "host": saved.Host,
	}))
	writeOK(w, map[string]any{"credential": viewOf(saved)})
}

// handleDeleteCredential removes a stored login.
//
//	DELETE /api/credentials/{id}
func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.DeleteCredential(r.Context(), r.PathValue("id")); err != nil {
		storeError(w, err)
		return
	}
	s.hub.Publish(eventOf(tasks.EventSettingsChanged, map[string]any{"credentials": true}))
	writeOK(w, nil)
}

// viewOf converts a stored row into the secret-free view the API returns.
func viewOf(c *store.Credential) tasks.CredentialView {
	return tasks.CredentialView{
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

// writeCredentialError maps the credential layer's errors onto status codes.
//
// "Cannot encrypt on this machine" and "cannot decrypt the stored blob" are
// 500s — nothing the caller did. Everything else that reaches here is an
// input-shape problem (blank secret, missing username, bad kind, host already
// taken), i.e. a 400.
func writeCredentialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tasks.ErrNoProtector):
		writeError(w, http.StatusInternalServerError, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "凭证不存在")
	case strings.Contains(err.Error(), "无法解密"):
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
