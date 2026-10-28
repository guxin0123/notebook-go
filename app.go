package main

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

//go:embed templates/index.html static/*
var embeddedFiles embed.FS

const assetPrefix = "/assets/"

type app struct {
	store     *noteStore
	template  *template.Template
	staticFS  fs.FS
	basePath  string // "" when served at the root, otherwise "/sub/dir"
	assetBase string
}

type pageData struct {
	ID          string
	Content     string
	AssetBase   string
	DownloadURL string
	NewNoteURL  string
}

func newApp(store *noteStore, rawBasePath string) (*app, error) {
	basePath, err := normalizeBasePath(rawBasePath)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.ParseFS(embeddedFiles, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	staticFS, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded static files: %w", err)
	}
	return &app{
		store:     store,
		template:  tmpl,
		staticFS:  staticFS,
		basePath:  basePath,
		assetBase: basePath + assetPrefix,
	}, nil
}

// normalizeBasePath turns a deployment subdirectory into a canonical URL
// prefix: "" when the app is served at the root, otherwise a leading slash and
// no trailing slash (for example "/note" or "/apps/note"). The prefix may be
// passed with or without surrounding slashes.
func normalizeBasePath(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" || prefix == "/" {
		return "", nil
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	if strings.ContainsAny(prefix, "\\?#% \t\r\n") {
		return "", fmt.Errorf("base path %q contains unsupported characters", raw)
	}
	for _, segment := range strings.Split(prefix[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("base path %q contains an invalid segment", raw)
		}
	}
	return prefix, nil
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")

	urlPath := r.URL.Path
	if a.basePath != "" {
		switch {
		case urlPath == a.basePath:
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, a.basePath+"/", http.StatusMovedPermanently)
				return
			}
			urlPath = "/"
		case strings.HasPrefix(urlPath, a.basePath+"/"):
			urlPath = urlPath[len(a.basePath):]
		}
	}

	switch {
	case strings.HasPrefix(urlPath, assetPrefix):
		a.handleAsset(w, r, urlPath)
		return
	case urlPath == "/favicon.ico":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		http.Redirect(w, r, a.assetBase+"favicon.svg", http.StatusMovedPermanently)
		return
	case urlPath == "/":
		a.handleNew(w, r)
		return
	}

	id, action, ok := parseNotePath(urlPath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "download":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		a.handleDownload(w, r, id)
	default:
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			a.handleEditor(w, r, id)
		case http.MethodPost, http.MethodPut:
			a.handleSave(w, r, id)
		default:
			methodNotAllowed(w, "GET, HEAD, PUT, POST")
		}
	}
}

func parseNotePath(urlPath string) (id, action string, ok bool) {
	if !strings.HasPrefix(urlPath, "/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(urlPath, "/")
	if rest == "" {
		return "", "", false
	}

	if !strings.Contains(rest, "/") {
		if !validNoteID(rest) {
			return "", "", false
		}
		return rest, "", true
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "download" || !validNoteID(parts[0]) {
		return "", "", false
	}
	return parts[0], "download", true
}

func (a *app) handleNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}

	for attempt := 0; attempt < 32; attempt++ {
		id, err := randomNoteID(8)
		if err != nil {
			log.Printf("generate note id: %v", err)
			http.Error(w, "无法生成文档地址", http.StatusInternalServerError)
			return
		}
		exists, err := a.store.exists(id)
		if err != nil {
			log.Printf("check note id: %v", err)
			http.Error(w, "读取文档失败", http.StatusInternalServerError)
			return
		}
		if exists {
			continue
		}

		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, a.basePath+"/"+id, http.StatusFound)
		return
	}
	http.Error(w, "无法生成文档地址", http.StatusInternalServerError)
}

func (a *app) handleEditor(w http.ResponseWriter, r *http.Request, id string) {
	data, _, err := a.store.load(id)
	if err != nil {
		switch {
		case errors.Is(err, errNoteTooLarge):
			http.Error(w, fmt.Sprintf("文档超过服务器限制（%s）", humanBytes(a.store.maxBytes)), http.StatusRequestEntityTooLarge)
		default:
			log.Printf("load note: %v", err)
			http.Error(w, "读取文档失败", http.StatusInternalServerError)
		}
		return
	}

	var buf bytes.Buffer
	if err := a.template.ExecuteTemplate(&buf, "index.html", pageData{
		ID:          id,
		Content:     string(data),
		AssetBase:   a.assetBase,
		DownloadURL: a.basePath + "/" + id + "/download",
		NewNoteURL:  a.basePath + "/",
	}); err != nil {
		log.Printf("render note: %v", err)
		http.Error(w, "渲染页面失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

func (a *app) handleSave(w http.ResponseWriter, r *http.Request, id string) {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || !strings.EqualFold(mediaType, "text/plain") {
			http.Error(w, "请求内容必须是 text/plain", http.StatusUnsupportedMediaType)
			return
		}
	}

	if r.ContentLength > a.store.maxBytes {
		http.Error(w, fmt.Sprintf("文档超过服务器限制（%s）", humanBytes(a.store.maxBytes)), http.StatusRequestEntityTooLarge)
		return
	}

	body := http.MaxBytesReader(w, r.Body, a.store.maxBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("文档超过服务器限制（%s）", humanBytes(a.store.maxBytes)), http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("read note body: %v", err)
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}
	if !utf8.Valid(data) {
		http.Error(w, "文档必须是 UTF-8 文本", http.StatusBadRequest)
		return
	}

	if err := a.store.save(id, data); err != nil {
		if errors.Is(err, errNoteTooLarge) {
			http.Error(w, fmt.Sprintf("文档超过服务器限制（%s）", humanBytes(a.store.maxBytes)), http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("save note: %v", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleDownload(w http.ResponseWriter, r *http.Request, id string) {
	f, err := os.Open(a.store.notePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "文档不存在", http.StatusNotFound)
			return
		}
		log.Printf("open note for download: %v", err)
		http.Error(w, "读取文档失败", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("stat note for download: %v", err)
		http.Error(w, "读取文档失败", http.StatusInternalServerError)
		return
	}
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	disposition := mime.FormatMediaType("attachment", map[string]string{
		"filename": id + ".txt",
	})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, id+".txt", info.ModTime(), f)
}

func (a *app) handleAsset(w http.ResponseWriter, r *http.Request, urlPath string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}

	name := strings.TrimPrefix(urlPath, assetPrefix)
	if name == "" || name != path.Base(name) || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		http.NotFound(w, r)
		return
	}

	data, err := fs.ReadFile(a.staticFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	contentType := "application/octet-stream"
	switch path.Ext(name) {
	case ".css":
		contentType = "text/css; charset=utf-8"
	case ".js":
		contentType = "text/javascript; charset=utf-8"
	case ".svg":
		contentType = "image/svg+xml"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "请求方法不支持", http.StatusMethodNotAllowed)
}

func humanBytes(n int64) string {
	if n >= 1024*1024 && n%(1024*1024) == 0 {
		return fmt.Sprintf("%d MiB", n/(1024*1024))
	}
	if n >= 1024 && n%1024 == 0 {
		return fmt.Sprintf("%d KiB", n/1024)
	}
	return fmt.Sprintf("%d 字节", n)
}
