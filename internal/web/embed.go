// Package web embeds the admin UI static files into the binary.
//
// It deliberately imports nothing from internal/: the handlers below are
// returned to the caller, which passes them to httpapi.NewRouter. Registering
// them by reaching into the router package would invert the dependency and turn
// the wiring order into a convention nothing enforces.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFiles embed.FS

// fileServer serves the embedded static tree.
func fileServer() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("web: embed sub failed: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}

// AdminHandler serves the admin SPA.
//
// Requests keep their /admin/ prefix so that /admin/index.html resolves to
// static/admin/index.html inside the embedded FS. The same applies to the
// page's own assets (/admin/admin.css, /admin/js/*.js).
func AdminHandler() http.Handler { return fileServer() }

// PlayerHandler serves the public standalone player:
// /player/{channelID} → static/player.html.
// No auth required — the player only accesses public /v1/channels/* endpoints.
func PlayerHandler() http.Handler {
	files := fileServer()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/player.html"
		files.ServeHTTP(w, r)
	})
}

// PlayerAssetsHandler serves the player's own assets under /player-assets/*.
func PlayerAssetsHandler() http.Handler {
	files := fileServer()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/player-assets")
		if path == "" || path == "/" {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(path, "/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		r.URL.Path = path
		files.ServeHTTP(w, r)
	})
}
