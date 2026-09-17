package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWaitSessionSurfacesBackendError covers the client half of the
// silent-failure fix. The wait endpoint answers HTTP 200 for every terminal
// state — success and failure alike — so a caller that only checks the status
// code cannot distinguish a crashed backend from a completed run. That is how
// a codex session whose handshake failed printed "session completed" and
// exited 0 without doing any work.
func TestWaitSessionSurfacesBackendError(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "errored session is a failure",
			body: `{"status":"error","num_turns":0,"timeout":false}`, wantErr: true},
		{name: "idle session is success",
			body: `{"status":"idle","num_turns":1,"timeout":false}`, wantErr: false},
		{name: "stopped session is success",
			body: `{"status":"stopped","num_turns":2,"timeout":false}`, wantErr: false},
		{name: "timed-out-but-running stays success",
			body: `{"status":"running","num_turns":1,"timeout":true}`, wantErr: false},
		{name: "unparseable body stays lenient",
			body: `<!doctype html><html></html>`, wantErr: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			api := &serverAPI{addr: srv.URL, token: "test-token"}
			err := api.waitSession("sess_xyz")
			if tc.wantErr && err == nil {
				t.Fatal("expected an error for an errored session, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
