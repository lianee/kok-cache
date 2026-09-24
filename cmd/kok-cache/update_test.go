package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Chaque test de refus est doublé d'un contrôle positif : la même chaîne, sans l'altération,
// doit passer. Sans cela, un vérificateur qui refuse TOUT passerait les tests de refus.

func TestVersionOrder(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.1", "v1.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.9.0", "v1.10.0", true},
		{"1.0.0", "v2.0.0", true},
		{"v1.0.0-rc1", "v1.0.0", true},  // la pré-version est sous la finale
		{"v1.0.0", "v1.0.0-rc1", false}, // et jamais l'inverse : pas de retour en arrière
		{"v1.0.0-rc1", "v1.0.0-rc2", true},
		{"dev", "v1.0.0", false},               // un binaire sans version ne bouge pas
		{"cbc0436", "v1.0.0", false},           // git describe --always sans tag
		{"v1.0.0-3-gcbc0436", "v1.0.1", false}, // commit après le tag : pas une version publiée
		{"v1.0.0-dirty", "v1.0.1", false},
		{"v1.0.0", "garbage", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.less {
			t.Errorf("versionLess(%q, %q) = %v, attendu %v", c.a, c.b, got, c.less)
		}
	}
}

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signed(t *testing.T, priv ed25519.PrivateKey, m manifest) (data, sig []byte) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)) + "\n")
}

func TestVerifyManifest(t *testing.T) {
	pub, priv := testKey(t)
	data, sig := signed(t, priv, manifest{Version: "v1.2.3", Files: map[string]manifestFile{}})

	m, err := verifyManifest(pub, data, sig)
	if err != nil || m.Version != "v1.2.3" {
		t.Fatalf("manifeste valide refusé : %v", err)
	}

	tampered := []byte(strings.Replace(string(data), "v1.2.3", "v9.9.9", 1))
	if _, err := verifyManifest(pub, tampered, sig); err == nil {
		t.Fatal("manifeste altéré accepté")
	}
	other, _ := testKey(t)
	if _, err := verifyManifest(other, data, sig); err == nil {
		t.Fatal("manifeste signé par une autre clé accepté")
	}
	if _, err := verifyManifest(nil, data, sig); err == nil {
		t.Fatal("aucune clé compilée, et pourtant accepté")
	}
	if _, err := verifyManifest(pub, data, []byte("pas du base64")); err == nil {
		t.Fatal("signature illisible acceptée")
	}
	// Un manifeste signé mais sans version publiée ne doit rien déclencher non plus.
	data2, sig2 := signed(t, priv, manifest{Version: "dev"})
	if _, err := verifyManifest(pub, data2, sig2); err == nil {
		t.Fatal("manifeste sans version publiée accepté")
	}
}

// fakeRelease sert un manifeste signé et un binaire, en comptant les téléchargements.
type fakeRelease struct {
	srv       *httptest.Server
	binary    []byte
	downloads atomic.Int32
	m         manifest
	data, sig []byte
}

func newFakeRelease(t *testing.T, priv ed25519.PrivateKey, ver string, binary []byte, corruptSHA bool) *fakeRelease {
	t.Helper()
	sum := sha256.Sum256(binary)
	sha := hex.EncodeToString(sum[:])
	if corruptSHA {
		sha = strings.Repeat("0", 64)
	}
	f := &fakeRelease{binary: binary}
	f.m = manifest{Version: ver, Files: map[string]manifestFile{
		platformKey(): {Name: "kok-cache-" + platformKey(), SHA256: sha},
	}}
	f.data, f.sig = signed(t, priv, f.m)
	mux := http.NewServeMux()
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(f.data) })
	mux.HandleFunc("/latest.json.sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(f.sig) })
	mux.HandleFunc("/kok-cache-"+platformKey(), func(w http.ResponseWriter, _ *http.Request) {
		f.downloads.Add(1)
		w.Write(f.binary)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestDownloadBinaryChecksSHA(t *testing.T) {
	pub, priv := testKey(t)
	bin := []byte("#!/bin/sh\necho new\n")

	good := newFakeRelease(t, priv, "v1.0.0", bin, false)
	m, err := fetchManifest(context.Background(), good.srv.URL, pub)
	if err != nil {
		t.Fatal(err)
	}
	data, err := downloadBinary(context.Background(), good.srv.URL, m)
	if err != nil || string(data) != string(bin) {
		t.Fatalf("binaire conforme refusé : %v", err)
	}

	bad := newFakeRelease(t, priv, "v1.0.0", bin, true)
	m, err = fetchManifest(context.Background(), bad.srv.URL, pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := downloadBinary(context.Background(), bad.srv.URL, m); err == nil {
		t.Fatal("binaire à l'empreinte inattendue accepté")
	}
	delete(m.Files, platformKey())
	if _, err := downloadBinary(context.Background(), bad.srv.URL, m); err == nil {
		t.Fatal("plateforme absente du manifeste, et pourtant un binaire accepté")
	}
}

func TestSwapExecutableAndRestore(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "kok-cache")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	restore, err := swapExecutable(exe, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("après échange, exe = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old" {
		t.Fatalf("après échange, .old = %q", b)
	}
	restore()
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("après retour arrière, exe = %q", b)
	}
	if _, err := os.Stat(exe + ".old"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(".old encore là après retour arrière")
	}
	cleanupOldBinary(exe) // sans .old : ne doit pas planter
}

// withVersion fait passer ce binaire de test pour une version publiée, le temps d'un test.
func withVersion(t *testing.T, v string, pub ed25519.PublicKey) {
	t.Helper()
	oldV, oldK := version, updatePublicKey
	version, updatePublicKey = v, pub
	t.Cleanup(func() { version, updatePublicKey = oldV, oldK })
}

func newTestUpdater(t *testing.T, auto bool, handOver func([]string) error) (*Updater, string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "kok-cache")
	if err := os.WriteFile(exe, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newUpdater(auto, exe, "", log, handOver), exe
}

// Option désactivée : la version est ANNONCÉE, rien n'est téléchargé ni écrit. C'est le
// comportement par défaut, et la promesse du README.
func TestCheckWithoutOptionOnlyNotifies(t *testing.T) {
	pub, priv := testKey(t)
	withVersion(t, "v1.0.0", pub)
	rel := newFakeRelease(t, priv, "v1.1.0", []byte("newer"), false)
	t.Setenv("KOK_CACHE_UPDATE_URL", rel.srv.URL)

	called := false
	u, exe := newTestUpdater(t, false, func([]string) error { called = true; return nil })
	u.check(context.Background(), u.auto.Load())

	st := u.State()
	if st.Available != "v1.1.0" || st.Updating || st.Err != "" {
		t.Fatalf("état inattendu : %+v", st)
	}
	if rel.downloads.Load() != 0 || called {
		t.Fatal("option désactivée, et pourtant un téléchargement ou un passage de relais")
	}
	if b, _ := os.ReadFile(exe); string(b) != "current" {
		t.Fatal("le binaire a été touché")
	}
}

// Option active : téléchargement, échange, passage de relais avec `--updated-from=<ancienne>`.
func TestCheckWithOptionInstallsAndHandsOver(t *testing.T) {
	pub, priv := testKey(t)
	withVersion(t, "v1.0.0", pub)
	rel := newFakeRelease(t, priv, "v1.1.0", []byte("newer"), false)
	t.Setenv("KOK_CACHE_UPDATE_URL", rel.srv.URL)

	var gotArgs []string
	u, exe := newTestUpdater(t, true, func(a []string) error { gotArgs = a; return nil })
	u.check(context.Background(), u.auto.Load())

	if b, _ := os.ReadFile(exe); string(b) != "newer" {
		t.Fatalf("binaire non remplacé : %q", b)
	}
	if len(gotArgs) != 1 || gotArgs[0] != "--updated-from=v1.0.0" {
		t.Fatalf("arguments du successeur : %v", gotArgs)
	}
	if st := u.State(); !st.Updating {
		t.Fatal("après un passage de relais réussi, l'état doit rester « en cours » jusqu'à la sortie")
	}
}

// Même version ou plus ancienne : rien, même avec l'option. C'est la monotonie.
func TestCheckNeverDowngrades(t *testing.T) {
	pub, priv := testKey(t)
	withVersion(t, "v1.1.0", pub)
	for _, v := range []string{"v1.1.0", "v1.0.9", "v1.1.0-rc1"} {
		rel := newFakeRelease(t, priv, v, []byte("older"), false)
		t.Setenv("KOK_CACHE_UPDATE_URL", rel.srv.URL)
		u, exe := newTestUpdater(t, true, func([]string) error { t.Fatal("passage de relais"); return nil })
		u.check(context.Background(), true)
		if b, _ := os.ReadFile(exe); string(b) != "current" || rel.downloads.Load() != 0 {
			t.Fatalf("%s : le binaire a été touché ou téléchargé", v)
		}
		if st := u.State(); st.Available != "" {
			t.Fatalf("%s annoncée comme disponible", v)
		}
	}
}

// Successeur qui ne démarre pas : l'ancien binaire est remis en place, et la même version n'est
// pas réessayée au passage suivant (sinon le service serait coupé toutes les 6 h).
func TestFailedSuccessorRollsBackAndIsNotRetried(t *testing.T) {
	pub, priv := testKey(t)
	withVersion(t, "v1.0.0", pub)
	rel := newFakeRelease(t, priv, "v1.1.0", []byte("newer"), false)
	t.Setenv("KOK_CACHE_UPDATE_URL", rel.srv.URL)

	calls := 0
	u, exe := newTestUpdater(t, true, func([]string) error { calls++; return errors.New("mort-né") })
	u.check(context.Background(), true)
	if b, _ := os.ReadFile(exe); string(b) != "current" {
		t.Fatalf("pas de retour arrière : %q", b)
	}
	st := u.State()
	if st.Updating || st.Err == "" || st.Available != "v1.1.0" {
		t.Fatalf("état après échec : %+v", st)
	}
	u.check(context.Background(), true)
	if calls != 1 || rel.downloads.Load() != 1 {
		t.Fatalf("la version en échec a été retentée (relais=%d, téléchargements=%d)", calls, rel.downloads.Load())
	}
	// Une version PLUS RÉCENTE remet les compteurs à zéro.
	rel2 := newFakeRelease(t, priv, "v1.2.0", []byte("fixed"), false)
	t.Setenv("KOK_CACHE_UPDATE_URL", rel2.srv.URL)
	u.handOver = func([]string) error { calls++; return nil }
	u.check(context.Background(), true)
	if b, _ := os.ReadFile(exe); string(b) != "fixed" || calls != 2 {
		t.Fatalf("v1.2.0 non installée après l'échec de v1.1.0 (relais=%d, exe=%q)", calls, b)
	}
}

// Manifeste signé par une autre clé : rien n'est téléchargé, rien n'est écrit, aucune erreur
// n'est montrée à l'utilisateur (ce n'est pas lui qui peut y faire quelque chose), mais rien
// n'est annoncé non plus.
func TestForeignManifestIsIgnored(t *testing.T) {
	pub, _ := testKey(t)
	_, otherPriv := testKey(t)
	withVersion(t, "v1.0.0", pub)
	rel := newFakeRelease(t, otherPriv, "v1.1.0", []byte("evil"), false)
	t.Setenv("KOK_CACHE_UPDATE_URL", rel.srv.URL)

	u, exe := newTestUpdater(t, true, func([]string) error { t.Fatal("passage de relais"); return nil })
	u.check(context.Background(), true)
	if b, _ := os.ReadFile(exe); string(b) != "current" || rel.downloads.Load() != 0 {
		t.Fatal("manifeste étranger : le binaire a été touché ou téléchargé")
	}
	if st := u.State(); st.Available != "" || st.Updating {
		t.Fatalf("manifeste étranger annoncé : %+v", st)
	}
}

func TestMinVersionFlagsRequired(t *testing.T) {
	pub, priv := testKey(t)
	withVersion(t, "v1.0.0", pub)
	rel := newFakeRelease(t, priv, "v1.1.0", []byte("newer"), false)
	rel.m.MinVersion = "v1.0.5"
	rel.data, rel.sig = signed(t, priv, rel.m)
	t.Setenv("KOK_CACHE_UPDATE_URL", rel.srv.URL)

	u, _ := newTestUpdater(t, false, nil)
	u.check(context.Background(), false)
	if st := u.State(); !st.Required || st.Available != "v1.1.0" {
		t.Fatalf("plancher non signalé : %+v", st)
	}
}
