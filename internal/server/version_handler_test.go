package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airdropia/pgw/internal/version"
)

// TestVersionEndpointReportsLocalBuild pins the GET /version contract the
// dashboard's versionStore parses. The upstream beacon behaviour (visit
// cookies, outbound manifest requests, install ids) was removed with the
// versioncheck package in plan §16 Stage 3; the endpoint now answers
// purely from build metadata and never touches the network.
func TestVersionEndpointReportsLocalBuild(t *testing.T) {
	srv := New(&mockProvider{}, &Config{})
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var status versionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode version response: %v", err)
	}
	if status.App == "" {
		t.Error("app is empty, want the distribution name")
	}
	if status.Version != version.Version {
		t.Errorf("version = %q, want %q", status.Version, version.Version)
	}
	if status.Enabled {
		t.Error("enabled = true, want false (beacon removed in Stage 3)")
	}
	if status.Latest != "" || status.UpdateAvailable {
		t.Error("update fields should be empty until Stage 9/10 lands the lightweight release check")
	}
}

// TestVersionEndpointSkipsAuthentication mirrors the upstream contract: the
// endpoint must answer without an admin credential because the dashboard
// polls it on every page load.
func TestVersionEndpointSkipsAuthentication(t *testing.T) {
	srv := New(&mockProvider{}, &Config{})
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"app"`) {
		t.Errorf("response missing app field: %s", rec.Body.String())
	}
}
