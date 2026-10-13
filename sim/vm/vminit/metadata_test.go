package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMetadataServesDocumentsAndListsDirectories(t *testing.T) {
	docs := metadataDocs{
		"/metadata/v1.json":                              `{"droplet_id":7}`,
		"/metadata/v1/hostname":                          "web-1",
		"/metadata/v1/interfaces/private/0/type":         "private",
		"/metadata/v1/interfaces/public/0/type":          "public",
		"/metadata/v1/tags/elprovo-preview":              "elprovo-preview",
		"/metadata/v1/interfaces/private/0/ipv4/address": "10.0.0.2",
	}
	tests := []struct {
		name, method, path string
		wantStatus         int
		wantBody, wantType string
	}{
		{"document", http.MethodGet, "/metadata/v1/hostname", http.StatusOK, "web-1", "text/plain; charset=utf-8"},
		{"JSON document", http.MethodGet, "/metadata/v1.json", http.StatusOK, `{"droplet_id":7}`, "application/json"},
		{"directory", http.MethodGet, "/metadata/v1/", http.StatusOK, "hostname\ninterfaces/\ntags/\n", "text/plain; charset=utf-8"},
		{"nested directory", http.MethodGet, "/metadata/v1/interfaces/", http.StatusOK, "private/\npublic/\n", "text/plain; charset=utf-8"},
		{"unknown path", http.MethodGet, "/metadata/v1/region", http.StatusNotFound, "404 page not found\n", "text/plain; charset=utf-8"},
		{"directory without its slash", http.MethodGet, "/metadata/v1/tags", http.StatusNotFound, "404 page not found\n", "text/plain; charset=utf-8"},
		{"change", http.MethodPut, "/metadata/v1/hostname", http.StatusMethodNotAllowed, "method not allowed\n", "text/plain; charset=utf-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()

			docs.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus || rec.Body.String() != tt.wantBody || rec.Header().Get("Content-Type") != tt.wantType {
				t.Fatalf("%s %s = %d %q (%s), want %d %q (%s)", tt.method, tt.path, rec.Code, rec.Body, rec.Header().Get("Content-Type"), tt.wantStatus, tt.wantBody, tt.wantType)
			}
		})
	}
}
