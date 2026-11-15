package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestEndpoints(t *testing.T) {
	var readyErr error
	srv := New(":0", func(context.Context) error { return readyErr })

	tests := []struct {
		method, path string
		ready        error
		want         int
	}{
		{"GET", "/healthz", nil, 200},
		{"GET", "/healthz", errors.New("down"), 200},
		{"HEAD", "/healthz", nil, 200},
		{"GET", "/readyz", nil, 200},
		{"GET", "/readyz", errors.New("store closed"), 503},
		{"POST", "/healthz", nil, 405},
		{"GET", "/", nil, 404},
		{"GET", "/v1/mails", nil, 404},
		{"GET", "/debug/pprof/", nil, 404},
	}
	for _, tt := range tests {
		readyErr = tt.ready
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.want {
			t.Errorf("%s %s (ready err %v) = %d, want %d", tt.method, tt.path, tt.ready, rec.Code, tt.want)
		}
		if rec.Code == 503 && rec.Body.String() != "not ready\n" {
			t.Errorf("503 body leaks detail: %q", rec.Body.String())
		}
	}
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Error("timeouts not set")
	}
}
