package fileserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustNew(t *testing.T) http.Handler {
	t.Helper()
	h, err := New(newTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestBrowserServesFile(t *testing.T) {
	h := mustNew(t)
	r := httptest.NewRequest(http.MethodGet, "/hello.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if body, _ := io.ReadAll(w.Body); string(body) != "hi\n" {
		t.Fatalf("body: %q", body)
	}
}

func TestBrowserRejectsWriteMethod(t *testing.T) {
	h := mustNew(t)
	r := httptest.NewRequest(http.MethodPost, "/hello.txt", strings.NewReader("nope"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

func TestWebDAVOptions(t *testing.T) {
	h := mustNew(t)
	r := httptest.NewRequest(http.MethodOptions, "/dav/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	// WebDAV servers advertise DAV capability via the DAV header.
	if dav := w.Header().Get("DAV"); dav == "" {
		t.Fatalf("expected DAV header, got none")
	}
}

func TestWebDAVReadsFile(t *testing.T) {
	h := mustNew(t)
	r := httptest.NewRequest(http.MethodGet, "/dav/hello.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if body, _ := io.ReadAll(w.Body); string(body) != "hi\n" {
		t.Fatalf("body: %q", body)
	}
}

func TestWebDAVRejectsPut(t *testing.T) {
	h := mustNew(t)
	r := httptest.NewRequest(http.MethodPut, "/dav/new.txt", strings.NewReader("nope"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

func TestWebDAVRejectsDelete(t *testing.T) {
	h := mustNew(t)
	r := httptest.NewRequest(http.MethodDelete, "/dav/hello.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

func TestWebDAVPropfindLists(t *testing.T) {
	h := mustNew(t)
	r := httptest.NewRequest("PROPFIND", "/dav/", nil)
	r.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("got %d, want 207", w.Code)
	}
	if body, _ := io.ReadAll(w.Body); !strings.Contains(string(body), "hello.txt") {
		t.Fatalf("PROPFIND body missing hello.txt: %s", body)
	}
}
