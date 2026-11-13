package control

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebStaticHandlerServesAssetsAndSPAWithoutMaskingAPI(t *testing.T) {
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("console-shell"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dist, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "assets", "app.js"), []byte("asset-body"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newWebStaticHandler(dist)
	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{"/", "console-shell", 200},
		{"/projects/prj_demo", "console-shell", 200},
		{"/assets/app.js", "asset-body", 200},
		{"/api/unknown", "404 page not found", 404},
		{"/missing.js", "404 page not found", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%q", w.Code, tc.status, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body %q does not contain %q", w.Body.String(), tc.want)
			}
		})
	}
}
