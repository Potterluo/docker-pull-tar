package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Potterluo/docker-pull-tar/internal/store"
)

// isNotFound reports whether err means "no such row". Handlers use it to
// answer 404 instead of 500 for an unknown id.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// isDuplicate reports whether err means "already exists" (409).
func isDuplicate(err error) bool {
	return errors.Is(err, store.ErrDuplicate)
}

// parseBoolQuery reads a tolerant boolean query parameter: "1", "true",
// "yes", "on" are true; everything else is false.
func parseBoolQuery(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// trimPathParam returns a path value with surrounding slashes and spaces
// removed, so "/api/artifacts//x/" and "x" behave the same.
func trimPathParam(v string) string {
	return strings.Trim(strings.TrimSpace(v), "/")
}

// notFound writes the standard 404 for an unknown row.
func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not found")
}
