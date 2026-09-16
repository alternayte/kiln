package gateway

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/alternayte/kiln/web"
)

// apiPrefixes are the paths the API owns. A client-side route must never
// shadow one, so the index fallback refuses them and the browser gets the
// API's own 404 instead of an HTML page.
var apiPrefixes = []string{
	"/v1/", "/api/", "/mcp", "/healthz",
	"/openapi.json", "/llms.txt", "/llms-full.txt", "/.well-known/",
}

// uiHandler serves the built SPA. A request for a file that exists answers
// with that file. Anything else answers with the index, so a deep link such
// as /sandboxes/abc reaches the client router on a fresh load.
func uiHandler() (http.Handler, error) {
	dist, err := fs.Sub(web.Assets, "dist")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(dist))
	index, indexErr := fs.ReadFile(dist, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range apiPrefixes {
			if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix) {
				http.NotFound(w, r)
				return
			}
		}
		if indexErr != nil {
			// The binary was built without a bundle. Saying so beats a blank
			// page that looks like a broken deployment.
			http.Error(w, "the web UI is not built into this binary; run `just web` and rebuild", http.StatusServiceUnavailable)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "." {
			if f, err := dist.Open(name); err == nil {
				f.Close()
				// A hashed asset never changes under its name, so it is
				// cached hard. index.html is not, or a deploy never lands.
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(index)
	}), nil
}
