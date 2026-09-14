// Package webui serves the embedded Next.js static export from the same
// binary as the daemon.
//
// The build pipeline (Makefile) renders web/out into internal/webui/dist/.
// This package embeds that directory and serves it under the root URL:
//
//   - Only GET and HEAD are answered; anything else is 405.
//   - /api/ belongs to the daemon's API and is never served from here.
//   - A request path resolves to the exact file, then to <path>/index.html,
//     then to 404.html with status 404. Directories are never listed.
//   - /_next/static/* is content-hashed by the build, so it is cached
//     forever; every HTML response is no-cache so a new release is picked
//     up on the next load.
//
// Security headers and the HTML Content-Security-Policy are added by the
// root handler in internal/httpserve, not here, so every response from the
// listener gets them in one place.
package webui

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Handler returns the http.Handler that serves the embedded static export.
// It is safe to use without a web build: when dist has no index.html (a
// `go install` binary) it serves a short page explaining how to get the UI.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// dist is committed with a .gitkeep, so this only happens when the
		// embed directive itself is broken. Still answer something useful.
		return placeholderHandler()
	}
	return newHandler(sub)
}

// newHandler builds the static handler over fsys. Tests inject an
// fstest.MapFS here.
func newHandler(fsys fs.FS) http.Handler {
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		return placeholderHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r) {
			return
		}
		if isAPIPath(r.URL.Path) || hasDotDotSegment(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		if serveFile(w, r, fsys, name, http.StatusOK) {
			return
		}
		if serveFile(w, r, fsys, path.Join(name, "index.html"), http.StatusOK) {
			return
		}
		if serveFile(w, r, fsys, "404.html", http.StatusNotFound) {
			return
		}
		http.NotFound(w, r)
	})
}

// allowMethod answers 405 for anything but GET/HEAD and reports whether the
// request may continue.
func allowMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// hasDotDotSegment rejects traversal attempts outright. r.URL.Path is
// already percent-decoded, so "%2e%2e" and "..%2f" arrive here as "..".
// fs.FS cannot escape its root anyway, but a request that tries is not one
// we want to quietly resolve to some other file.
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(c rune) bool { return c == '/' || c == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// serveFile writes name from fsys with the given status and reports whether
// it existed as a regular file.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string, status int) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return false
	}

	h := w.Header()
	h.Set("Content-Type", contentTypeFor(name))
	switch {
	case strings.HasPrefix(name, "_next/static/"):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case strings.HasSuffix(name, ".html"):
		h.Set("Cache-Control", "no-cache")
	}

	if status == http.StatusOK {
		if rs, ok := f.(io.ReadSeeker); ok {
			http.ServeContent(w, r, name, stat.ModTime(), rs)
			return true
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return false
		}
		http.ServeContent(w, r, name, stat.ModTime(), bytes.NewReader(data))
		return true
	}

	// Non-200 (the 404 page): ServeContent would force 200 and honor
	// conditional/range headers that make no sense for an error page.
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}

func contentTypeFor(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".webmanifest", ".manifest":
		return "application/manifest+json"
	default:
		return "application/octet-stream"
	}
}

const placeholderPage = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>agentflow</title></head>
<body style="font-family:system-ui;max-width:36rem;margin:4rem auto;padding:1rem;color:#222">
<h1>agentflow</h1>
<p>Web UI not included in this build. Install a release binary or run <code>make build</code>.</p>
<p>The daemon and its API are running normally.</p>
</body></html>`

// placeholderHandler serves the explanation page for binaries built without
// the web export. It answers 200: the daemon is healthy, only the UI is
// missing, and a 5xx would read as an outage.
func placeholderHandler() http.Handler {
	body := []byte(placeholderPage)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r) {
			return
		}
		if isAPIPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(body))
	})
}
