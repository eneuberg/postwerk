// Package httpapi serves Postwerk's HTTP endpoints (H1). In the foundation
// phase these are only the health checks; the query API and UI come later.
package httpapi

import (
	"context"
	"net/http"
	"time"
)

// Ready reports whether Postwerk can do its work. A nil error means ready.
type Ready func(ctx context.Context) error

// New returns an HTTP server with /healthz and /readyz and nothing else.
//
//   - /healthz answers 200 while the process is alive.
//   - /readyz answers 200 if ready returns nil within two seconds, else 503.
func New(addr string, ready Ready) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			// The reason stays in the logs of the caller, not in the reply.
			write(w, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		write(w, http.StatusOK, "ready\n")
	})
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
}

func write(w http.ResponseWriter, code int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}
