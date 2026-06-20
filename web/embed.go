// Package webui embeds the built React single-page app and serves it. The UI is
// compiled into the apiserver binary so Quetzal ships as a single artifact.
//
// `web/dist` is produced by `npm run build`. A committed `dist/.gitkeep` keeps
// `go build` working even when the UI hasn't been built (the handler then serves
// a small placeholder instead of the SPA).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

const placeholder = `<!doctype html><html><head><meta charset="utf-8"><title>Quetzal</title></head>
<body style="font-family:system-ui;background:#0f1115;color:#e6e9ef;padding:40px">
<h1>Quetzal</h1><p>The API is running, but the web UI was not built into this binary.</p>
<p>Run <code>npm --prefix web run build</code> and rebuild, or use the Vite dev server.</p>
</body></html>`

// Handler serves the embedded SPA, falling back to index.html for client-side
// routes. When the UI is absent it serves a placeholder page.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return placeholderHandler()
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return placeholderHandler() // not built (only .gitkeep present)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, index)
			return
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// Unknown path: let the SPA router handle it.
			serveIndex(w, index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}

func placeholderHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(placeholder))
	})
}
