package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// L'onglet doit porter l'icône embarquée, servie par le binaire lui-même (aucune ressource
// extérieure, cf. la CSP de handlePage).
func TestFavicon(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleFavicon(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type %q", ct)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("corps sans signature PNG (%d octets)", rec.Body.Len())
	}

	raw, err := assets.ReadFile("assets/app.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `<link rel="icon" type="image/png" href="/favicon.ico">`) {
		t.Fatal("app.html ne déclare pas le favicon")
	}
}
