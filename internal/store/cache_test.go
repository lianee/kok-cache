package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Le cache d'empreintes doit survivre au processus — sinon chaque démarrage relit tout le dossier
// (42,5 Go mesurés chez le premier utilisateur, plusieurs minutes en silence).
func TestHashCachePersists(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "hashes.json")

	s1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("ciphertext")
	if err := s1.Commit(77, StemVocals, data, hashOf(data)); err != nil {
		t.Fatal(err)
	}
	want := s1.Hash(77, StemVocals) // remplit le cache mémoire
	if err := s1.SaveHashCache(cachePath); err != nil {
		t.Fatal(err)
	}

	// Nouveau Store = nouveau processus : sans cache persistant il devrait tout rehacher.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := s2.UnhashedCount(); n != 1 {
		t.Errorf("avant chargement : %d fichier(s) à hacher, attendu 1", n)
	}
	s2.LoadHashCache(cachePath)
	if n := s2.UnhashedCount(); n != 0 {
		t.Errorf("après chargement : %d fichier(s) à hacher, attendu 0", n)
	}
	if got := s2.Hash(77, StemVocals); got != want {
		t.Errorf("empreinte relue incorrecte : %s", got)
	}
}

// Un cache périmé ne doit JAMAIS produire un mauvais résultat : le fichier remplacé est rehaché.
// C'est ce qui rend cette optimisation sûre — au pire elle coûte une relecture.
func TestHashCacheNeverTrustsStaleEntry(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "hashes.json")

	s1, _ := New(dir)
	v1 := []byte("version 1")
	if err := s1.Commit(9, StemVocals, v1, hashOf(v1)); err != nil {
		t.Fatal(err)
	}
	_ = s1.Hash(9, StemVocals)
	if err := s1.SaveHashCache(cachePath); err != nil {
		t.Fatal(err)
	}

	// Le fichier change hors du programme (re-split, copie manuelle…).
	v2 := []byte("version 2, remplacée hors ligne")
	if err := os.WriteFile(s1.Path(9, StemVocals), v2, 0o644); err != nil {
		t.Fatal(err)
	}

	s2, _ := New(dir)
	s2.LoadHashCache(cachePath)
	if got := s2.Hash(9, StemVocals); got != hashOf(v2) {
		t.Fatalf("empreinte périmée servie depuis le cache disque : %s", got)
	}
}
