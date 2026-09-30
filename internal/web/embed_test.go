package web

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedWebIsServedWithoutRedirects(t *testing.T) {
	s := httptest.NewServer(Handler())
	defer s.Close()
	for _, path := range []string{"/", "/index.html", "/app.js", "/style.css"} {
		r, err := s.Client().Get(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil || r.StatusCode != 200 || len(body) == 0 || r.Request.URL.Path != path {
			t.Fatalf("asset %s: status %d, body %d, err %v", path, r.StatusCode, len(body), err)
		}
		if path == "/" && !strings.Contains(string(body), "NodeBridge") {
			t.Fatal("root did not serve the UI")
		}
	}
	r, err := s.Client().Get(s.URL + "/api/missing")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatal("unknown API returned the UI")
	}
}
