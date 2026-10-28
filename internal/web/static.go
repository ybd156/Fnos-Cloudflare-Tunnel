package web

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// mime maps file extensions to content types. Only the types the payload
// actually ships are listed; anything else falls back to
// application/octet-stream.
var mime = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".mjs":   "application/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// serveStatic serves the browser UI out of the payload's www directory.
//
// base is the gateway prefix the page was requested through ("" on a bare
// port); it replaces the %%CF_BASE%% placeholder so the same index.html works
// behind the fnOS gateway and on a loopback port.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, urlPath, base string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Anchor the path before cleaning: Clean("/../..") is "/", so ".." can
	// never climb out of the www directory. A plain prefix comparison used to
	// accept a sibling directory whose name merely started with the same
	// characters (…/www-evil).
	rel := filepath.FromSlash(path.Clean("/" + strings.TrimPrefix(urlPath, "/")))
	if rel == string(filepath.Separator) || rel == "" || rel == "." {
		rel = "index.html"
	}
	target := filepath.Join(s.P.WWW, rel)
	if within, err := filepath.Rel(s.P.WWW, target); err != nil || strings.HasPrefix(within, "..") || filepath.IsAbs(within) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	st, err := os.Stat(target)
	if err != nil || !st.Mode().IsRegular() {
		// Single page app: an unknown extension-less path falls back to the
		// shell. A missing asset is a real 404.
		if path.Ext(rel) != "" || rel == "index.html" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		s.serveStatic(w, r, "/index.html", base)
		return
	}

	ext := strings.ToLower(filepath.Ext(target))
	contentType := mime[ext]
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if ext == ".html" {
		html, readErr := os.ReadFile(target)
		if readErr != nil {
			http.Error(w, "Internal Error", http.StatusInternalServerError)
			return
		}
		body := strings.ReplaceAll(string(html), "%%CF_BASE%%", base)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write([]byte(body))
		return
	}

	f, openErr := os.Open(target)
	if openErr != nil {
		http.Error(w, "Internal Error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	// ServeContent handles Range, If-Modified-Since and HEAD for us.
	http.ServeContent(w, r, filepath.Base(target), st.ModTime(), f)
}
