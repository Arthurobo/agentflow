package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                  {Data: []byte("<html>home</html>")},
		"404.html":                    {Data: []byte("<html>custom not found</html>")},
		"run/index.html":              {Data: []byte("<html>run page</html>")},
		"run.txt":                     {Data: []byte("run text")},
		"about.html":                  {Data: []byte("<html>about</html>")},
		"emptydir/.keep":              {Data: []byte("")},
		"_next/static/chunks/app.js":  {Data: []byte("console.log(1)")},
		"_next/static/css/styles.css": {Data: []byte("body{}")},
		// A file a naive filepath.Join of a traversal path would land on.
		"etc/passwd": {Data: []byte("root:SECRET-SHOULD-NOT-LEAK")},
		"x":          {Data: []byte("SECRET-X-SHOULD-NOT-LEAK")},
	}
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPathTraversalIsRejected(t *testing.T) {
	h := newHandler(testFS())
	for _, p := range []string{
		"/..%2f..%2fetc/passwd",
		"/_next/../../x",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/run/..%2f..%2fx",
		"/..%5c..%5cx",
	} {
		rec := do(t, h, http.MethodGet, p)
		if rec.Code == http.StatusOK {
			t.Errorf("%s: got 200, want a refusal", p)
		}
		if body := rec.Body.String(); strings.Contains(body, "SECRET") {
			t.Errorf("%s: leaked file content %q", p, body)
		}
	}
}

func TestResolutionOrder(t *testing.T) {
	h := newHandler(testFS())
	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/", 200, "home"},
		{"/index.html", 200, "home"},
		{"/run.txt", 200, "run text"}, // exact file wins
		{"/run", 200, "run page"},     // then <path>/index.html
		{"/run/", 200, "run page"},    // trailing slash too
		{"/missing", 404, "custom not found"},
		{"/emptydir/", 404, "custom not found"}, // a directory is never listed
	}
	for _, c := range cases {
		rec := do(t, h, http.MethodGet, c.path)
		if rec.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.path, rec.Code, c.status)
		}
		if !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: body %q, want it to contain %q", c.path, rec.Body.String(), c.body)
		}
	}
}

func TestPlainNotFoundWithout404Page(t *testing.T) {
	fsys := testFS()
	delete(fsys, "404.html")
	rec := do(t, newHandler(fsys), http.MethodGet, "/missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("unexpected html body %q", rec.Body.String())
	}
}

func TestCacheControl(t *testing.T) {
	h := newHandler(testFS())
	cases := []struct {
		path, want string
	}{
		{"/_next/static/chunks/app.js", "public, max-age=31536000, immutable"},
		{"/_next/static/css/styles.css", "public, max-age=31536000, immutable"},
		{"/", "no-cache"},
		{"/run/", "no-cache"},
		{"/about.html", "no-cache"},
		{"/missing", "no-cache"}, // 404.html is HTML too
	}
	for _, c := range cases {
		rec := do(t, h, http.MethodGet, c.path)
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%s: Cache-Control %q, want %q", c.path, got, c.want)
		}
	}
	if ct := do(t, h, http.MethodGet, "/_next/static/chunks/app.js").Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("js content type %q", ct)
	}
	if ct := do(t, h, http.MethodGet, "/").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("html content type %q", ct)
	}
}

func TestOnlyGetAndHead(t *testing.T) {
	h := newHandler(testFS())
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		rec := do(t, h, m, "/")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, rec.Code)
		}
		if rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: Allow %q", m, rec.Header().Get("Allow"))
		}
	}

	rec := do(t, h, http.MethodHead, "/")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD /: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != "17" {
		t.Fatalf("HEAD /: Content-Length %q", rec.Header().Get("Content-Length"))
	}
	rec = do(t, h, http.MethodHead, "/missing")
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Fatalf("HEAD /missing: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestNeverServesAPI(t *testing.T) {
	fsys := testFS()
	fsys["api/v1/agentd/health"] = &fstest.MapFile{Data: []byte("SECRET-API-FILE")}
	fsys["api/index.html"] = &fstest.MapFile{Data: []byte("SECRET-API-INDEX")}
	for _, h := range []http.Handler{newHandler(fsys), newHandler(fstest.MapFS{})} {
		for _, p := range []string{"/api/v1/agentd/health", "/api/", "/api"} {
			rec := do(t, h, http.MethodGet, p)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: status %d, want 404", p, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "SECRET") || strings.Contains(rec.Body.String(), "<html>") {
				t.Errorf("%s: body %q", p, rec.Body.String())
			}
		}
	}
}

func TestPlaceholderWhenNoIndex(t *testing.T) {
	h := newHandler(fstest.MapFS{".gitkeep": {Data: nil}})
	for _, p := range []string{"/", "/run/", "/pair/", "/anything/else"} {
		rec := do(t, h, http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Web UI not included in this build") {
			t.Errorf("%s: body %q", p, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: content type %q", p, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q", p, cc)
		}
	}
	if rec := do(t, h, http.MethodHead, "/"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD placeholder: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST placeholder: status %d", rec.Code)
	}
}

func TestEmbeddedHandlerAnswers(t *testing.T) {
	// Whatever dist holds in this checkout (a real export or only .gitkeep),
	// the root must answer 200 rather than an error.
	rec := do(t, Handler(), http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / on embedded handler: %d", rec.Code)
	}
}
