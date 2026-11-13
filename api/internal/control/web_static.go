package control

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// newWebStaticHandler serves immutable Console assets from the API image when
// the standalone nginx Web deployment is intentionally disabled. Existing
// assets are served directly; unknown client-side routes fall back to index.html.
// API and other service namespaces never fall back to the Console document.
func newWebStaticHandler(dist string) http.Handler {
	root := filepath.Clean(dist)
	index := filepath.Join(root, "index.html")
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		requestPath := filepath.Clean(filepath.FromSlash("/" + strings.TrimPrefix(r.URL.Path, "/")))
		candidate := filepath.Join(root, strings.TrimPrefix(requestPath, string(filepath.Separator)))
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		// Do not mask missing API/service endpoints or missing static assets.
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" ||
			strings.HasPrefix(r.URL.Path, "/ai/") || strings.HasPrefix(r.URL.Path, "/storage/") ||
			filepath.Ext(r.URL.Path) != "" {
			http.NotFound(w, r)
			return
		}
		if _, err := os.Stat(index); err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, index)
	})
}
