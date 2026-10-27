package main

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newTestApp(t *testing.T, maxBytes int64) (*app, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := newNoteStore(dir, maxBytes)
	if err != nil {
		t.Fatalf("newNoteStore: %v", err)
	}
	handler, err := newApp(store)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	return handler, dir
}

func request(t *testing.T, handler http.Handler, method, target, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRandomNoteID(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id, err := randomNoteID(generatedNoteIDLength)
		if err != nil {
			t.Fatalf("randomNoteID: %v", err)
		}
		if len(id) != generatedNoteIDLength {
			t.Fatalf("length = %d, want %d", len(id), generatedNoteIDLength)
		}
		if !validNoteID(id) {
			t.Fatalf("generated invalid id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 190 {
		t.Fatalf("too many duplicate ids: %d unique of 200", len(seen))
	}

	if _, err := randomNoteID(0); err == nil {
		t.Fatal("randomNoteID(0) should fail")
	}
}

func TestParseNotePath(t *testing.T) {
	tests := []struct {
		path   string
		id     string
		action string
		ok     bool
	}{
		{"/abc123", "abc123", "", true},
		{"/a-b_c", "a-b_c", "", true},
		{"/abc123/download", "abc123", "download", true},
		{"/assets", "assets", "", true},
		{"/", "", "", false},
		{"/abc/", "", "", false},
		{"/abc/other", "", "", false},
		{"/abc/download/extra", "", "", false},
		{"/../etc/passwd", "", "", false},
		{"/bad.id", "", "", false},
		{"/" + strings.Repeat("a", 65), "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			id, action, ok := parseNotePath(tt.path)
			if id != tt.id || action != tt.action || ok != tt.ok {
				t.Fatalf("parseNotePath(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.path, id, action, ok, tt.id, tt.action, tt.ok)
			}
		})
	}
}

func TestNewNoteRedirect(t *testing.T) {
	handler, dir := newTestApp(t, defaultMaxNoteBytes)

	rec := request(t, handler, http.MethodGet, "/", "", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	location := rec.Header().Get("Location")
	if !regexp.MustCompile(`^/[234579abcdefghjkmnpqrstwxyz]{8}$`).MatchString(location) {
		t.Fatalf("unexpected redirect location %q", location)
	}
	if cache := rec.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cache)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("missing content security policy: %q", csp)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("opening a new note should not create files, found %d", len(entries))
	}
}

func TestSaveEditorAndDownload(t *testing.T) {
	handler, dir := newTestApp(t, defaultMaxNoteBytes)
	content := "第一行\n</textarea><script>alert(\"x\")</script>\n& last\r\n"

	rec := request(t, handler, http.MethodPut, "/testDoc_1", content, "text/plain; charset=utf-8")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("save status = %d, body = %q", rec.Code, rec.Body.String())
	}

	path := filepath.Join(dir, "testDoc_1.txt")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(saved) != content {
		t.Fatalf("saved content mismatch:\n got %q\nwant %q", saved, content)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("file mode = %o, want 600", mode)
		}
	}

	editor := request(t, handler, http.MethodGet, "/testDoc_1", "", "")
	if editor.Code != http.StatusOK {
		t.Fatalf("editor status = %d", editor.Code)
	}
	if got := editor.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("editor content type = %q", got)
	}
	if cache := editor.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Fatalf("editor Cache-Control = %q", cache)
	}
	body := editor.Body.String()
	if strings.Contains(body, "</textarea><script>") {
		t.Fatal("note content was not escaped inside the textarea")
	}
	if !strings.Contains(body, "&lt;/textarea&gt;") {
		t.Fatalf("escaped content missing from editor page: %q", body)
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("raw script content found in editor page")
	}

	download := request(t, handler, http.MethodGet, "/testDoc_1/download", "", "")
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d", download.Code)
	}
	if got := download.Body.String(); got != content {
		t.Fatalf("download content mismatch:\n got %q\nwant %q", got, content)
	}
	if got := download.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("download content type = %q", got)
	}
	mediaType, params, err := mime.ParseMediaType(download.Header().Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("parse Content-Disposition: %v", err)
	}
	if mediaType != "attachment" || params["filename"] != "testDoc_1.txt" {
		t.Fatalf("unexpected Content-Disposition: %q", download.Header().Get("Content-Disposition"))
	}
	if got := download.Header().Get("Content-Length"); got != fmt.Sprint(len(content)) {
		t.Fatalf("download Content-Length = %q, want %d", got, len(content))
	}

	head := request(t, handler, http.MethodHead, "/testDoc_1/download", "", "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD download status=%d body=%d", head.Code, head.Body.Len())
	}
}

func TestEmptySaveAndDownload(t *testing.T) {
	handler, _ := newTestApp(t, defaultMaxNoteBytes)

	rec := request(t, handler, http.MethodPost, "/empty", "", "text/plain")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("empty save status = %d", rec.Code)
	}
	download := request(t, handler, http.MethodGet, "/empty/download", "", "")
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d", download.Code)
	}
	if download.Body.Len() != 0 {
		t.Fatalf("download body = %q, want empty", download.Body.String())
	}
}

func TestMissingDownload(t *testing.T) {
	handler, _ := newTestApp(t, defaultMaxNoteBytes)
	rec := request(t, handler, http.MethodGet, "/missing/download", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestInvalidRequests(t *testing.T) {
	handler, dir := newTestApp(t, defaultMaxNoteBytes)

	tests := []struct {
		name        string
		method      string
		target      string
		body        string
		contentType string
		want        int
	}{
		{"invalid id", http.MethodGet, "/bad.id", "", "", http.StatusNotFound},
		{"traversal", http.MethodGet, "/..%2Fsecret", "", "", http.StatusNotFound},
		{"encoded dot", http.MethodGet, "/%2e%2e", "", "", http.StatusNotFound},
		{"save with form encoding", http.MethodPost, "/note", "text=hello", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"delete", http.MethodDelete, "/note", "", "", http.StatusMethodNotAllowed},
		{"post to download", http.MethodPost, "/note/download", "", "text/plain", http.StatusMethodNotAllowed},
		{"asset traversal", http.MethodGet, "/assets/../templates/index.html", "", "", http.StatusNotFound},
		{"asset directory", http.MethodGet, "/assets/", "", "", http.StatusNotFound},
		{"new note post", http.MethodPost, "/", "", "", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := request(t, handler, tt.method, tt.target, tt.body, tt.contentType)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusMethodNotAllowed && rec.Header().Get("Allow") == "" {
				t.Fatal("405 response is missing an Allow header")
			}
		})
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid requests created %d files", len(entries))
	}
}

func TestSaveTooLargeKeepsPreviousVersion(t *testing.T) {
	maxBytes := int64(32)
	handler, dir := newTestApp(t, maxBytes)

	first := request(t, handler, http.MethodPut, "/limit", "short", "text/plain")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first save status = %d", first.Code)
	}

	tooLarge := strings.Repeat("x", int(maxBytes)+1)
	rec := request(t, handler, http.MethodPut, "/limit", tooLarge, "text/plain")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized save status = %d, want 413", rec.Code)
	}

	data, err := os.ReadFile(filepath.Join(dir, "limit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "short" {
		t.Fatalf("previous version was damaged: %q", data)
	}
}

func TestInvalidUTF8IsRejected(t *testing.T) {
	handler, dir := newTestApp(t, defaultMaxNoteBytes)
	body := string([]byte{0xff, 0xfe, 0xfd})
	rec := request(t, handler, http.MethodPut, "/binary", body, "text/plain")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "binary.txt")); !os.IsNotExist(err) {
		t.Fatalf("invalid content should not be written, err = %v", err)
	}
}

func TestOversizedExistingFileIsRejectedOnRead(t *testing.T) {
	handler, dir := newTestApp(t, 16)
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), bytes.Repeat([]byte("a"), 17), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := request(t, handler, http.MethodGet, "/large", "", "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestAssets(t *testing.T) {
	handler, _ := newTestApp(t, defaultMaxNoteBytes)

	rec := request(t, handler, http.MethodGet, "/assets/app.js", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Fatalf("content type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "saveNow") {
		t.Fatal("app.js content missing")
	}

	css := request(t, handler, http.MethodGet, "/assets/style.css", "", "")
	if css.Code != http.StatusOK || !strings.Contains(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("css status=%d type=%q", css.Code, css.Header().Get("Content-Type"))
	}

	favicon := request(t, handler, http.MethodGet, "/assets/favicon.svg", "", "")
	if favicon.Code != http.StatusOK || !strings.Contains(favicon.Header().Get("Content-Type"), "image/svg+xml") {
		t.Fatalf("favicon status=%d type=%q", favicon.Code, favicon.Header().Get("Content-Type"))
	}

	missing := request(t, handler, http.MethodGet, "/assets/nope.js", "", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d", missing.Code)
	}
}

func TestConcurrentSavesRemainWhole(t *testing.T) {
	handler, dir := newTestApp(t, defaultMaxNoteBytes)
	const writers = 24
	values := make([]string, writers)
	for i := range values {
		values[i] = fmt.Sprintf("writer-%02d\n%s\n", i, strings.Repeat(string(rune('a'+i)), 4096))
	}

	var wg sync.WaitGroup
	statuses := make([]int, writers)
	for i := range values {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPut, "/race", strings.NewReader(values[i]))
			req.Header.Set("Content-Type", "text/plain")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			statuses[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusNoContent {
			t.Fatalf("writer %d status = %d", i, status)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "race.txt"))
	if err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, value := range values {
		if string(data) == value {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("concurrent save produced a torn file of %d bytes", len(data))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %d entries", len(entries))
	}
}

func TestContentSecurityAndRobotsHeaders(t *testing.T) {
	handler, _ := newTestApp(t, defaultMaxNoteBytes)
	rec := request(t, handler, http.MethodGet, "/header-test", "", "")
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"X-Robots-Tag":           "noindex, nofollow",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestURLPathWithEscapedSlash(t *testing.T) {
	handler, dir := newTestApp(t, defaultMaxNoteBytes)
	// Build the request manually so the escaped slash survives into URL.Path.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL = &url.URL{Path: "/../secret"}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal file exists: %v", err)
	}
}
