package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Potterluo/docker-pull-tar/internal/events"
)

// events.go: server-sent events — the realtime channel for the web UI, and
// the mechanism behind F4's live progress.
//
// One GET /api/events stream per browser tab; the hub fans every task,
// artifact and settings event out to all of them. Every frame is built by
// the JSON encoder from a whole Go value, so a multi-byte UTF-8 character
// can never be split across a write (see AGENTS.md's UTF-8 rule).
//
// Why SSE over WebSocket: it is plain HTTP (survives proxies, needs no
// upgrade), auto-reconnects in the browser for free, and the only thing the
// browser cannot do on the stream is send — which is what the POST routes
// are for.

// handleEvents holds an SSE stream open, forwarding hub events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// The statusRecorder in middleware.go forwards Flush, so the assertion
	// above holds through the logging wrapper.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies between the server and the browser must not buffer this
	// response (nginx: proxy_buffering off).
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := s.hub.Subscribe("")
	defer s.hub.Unsubscribe(sub)

	writeSSE := func(evt events.Event) bool {
		b, err := json.Marshal(evt)
		if err != nil {
			return true // skip this event; keep the stream alive
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false // client gone
		}
		flusher.Flush()
		return true
	}

	// A hello frame lets the client confirm the stream is live instead of
	// waiting on an event that may never come.
	if !writeSSE(events.Event{Type: "hello"}) {
		return
	}

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-sub.Ch:
			if !ok {
				return
			}
			if !writeSSE(evt) {
				return
			}
		case <-ping.C:
			if !writeSSE(events.Event{Type: "ping"}) {
				return
			}
		}
	}
}
