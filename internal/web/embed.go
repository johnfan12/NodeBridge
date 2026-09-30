package web

import (
	"bytes"
	"embed"
	"net/http"
	"strings"
	"time"
)

//go:embed index.html admin.html app.js style.css
var files embed.FS

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", 405)
			return
		}
		name := r.URL.Path
		if name == "/" {
			name = "/index.html"
		}
		if name == "/admin" || name == "/admin/" {
			name = "/admin.html"
		}
		if name != "/admin.html" && name != "/index.html" && name != "/app.js" && name != "/style.css" {
			http.NotFound(w, r)
			return
		}
		body, err := files.ReadFile(strings.TrimPrefix(name, "/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	})
}
