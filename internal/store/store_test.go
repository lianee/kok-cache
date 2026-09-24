package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// EXIGENCE 1 — le prédicat de synchronisation est « absent OU hash différent ».
//
// Le cas qui compte est le troisième : un re-split change le sel et remplace intégralement le
// ciphertext. Le fichier n'est pas absent, il est PÉRIMÉ. Un prédicat « manquant » ne le voit
// jamais, et comme on se tait sur un hash qu'on ne reconnaît pas, on disparaît silencieusement de
// l'essaim pour cette chanson.
func TestNeedsSync(t *testing.T) {
	s := newStore(t)
	data := []byte("ciphertext v1")
	want := hashOf(data)

	if !s.NeedsSync(1384, StemVocals, want) {
		t.Error("fichier absent : doit être à synchroniser")
	}
	if err := s.Commit(1384, StemVocals, data, want); err != nil {
		t.Fatal(err)
	}
	if s.NeedsSync(1384, StemVocals, want) {
		t.Error("fichier présent et conforme : ne doit pas être resynchronisé")
	}

	// Le cœur de l'exigence : même nom, même présence, contenu périmé.
	newHash := hashOf([]byte("ciphertext v2 après re-split"))
	if !s.NeedsSync(1384, StemVocals, newHash) {
		t.Error("fichier PÉRIMÉ (présent, hash différent) : doit être à synchroniser")
	}

	// Sans hash de référence on ne peut ni vérifier ni décider : on ne synchronise pas.
	if s.NeedsSync(1384, StemVocals, "") {
		t.Error("sans hash de référence, aucune synchronisation ne doit être déclenchée")
	}
}

// EXIGENCE 2 — écriture temporaire, vérification du hash, PUIS renommage atomique.
//
// Ce que le test prouve : rien n'apparaît sous le nom définitif tant que le contenu n'est pas
// vérifié. C'est ce qui permet de répondre à `stem-avail` pendant une synchronisation sans jamais
// annoncer un fichier tronqué.
func TestCommitVerifiesBeforeRename(t *testing.T) {
	s := newStore(t)
	good := []byte("le bon ciphertext")

	// a) contenu conforme → fichier créé, contenu exact
	if err := s.Commit(42, StemAccompaniment, good, hashOf(good)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(s.Path(42, StemAccompaniment))
	if err != nil || string(got) != string(good) {
		t.Fatalf("contenu écrit incorrect : %v / %q", err, got)
	}

	// b) contenu non conforme → refus, et AUCUN fichier n'apparaît
	bad := []byte("des octets quelconques servis par un pair")
	err = s.Commit(43, StemVocals, bad, hashOf(good))
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("attendu ErrHashMismatch, obtenu %v", err)
	}
	if _, err := os.Stat(s.Path(43, StemVocals)); !os.IsNotExist(err) {
		t.Error("un fichier a été créé alors que le hash ne correspondait pas")
	}

	// c) un refus ne doit pas abîmer un fichier déjà en place
	if err := s.Commit(42, StemAccompaniment, bad, hashOf(good)); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("attendu ErrHashMismatch, obtenu %v", err)
	}
	got, _ = os.ReadFile(s.Path(42, StemAccompaniment))
	if string(got) != string(good) {
		t.Error("le fichier existant a été altéré par une écriture refusée")
	}

	// d) aucun temporaire ne doit survivre à un refus
	entries, _ := os.ReadDir(s.Dir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("temporaire laissé derrière : %s", e.Name())
		}
	}

	// e) écrire sans hash de référence est refusé — sinon n'importe quels octets d'un pair
	// quelconque deviendraient le contenu du cache.
	if err := s.Commit(44, StemVocals, good, ""); err == nil {
		t.Error("une écriture sans hash attendu doit être refusée")
	}
}

// EXIGENCE 3 — sur désaccord de hash : silence, JAMAIS suppression.
//
// Le hash attendu vient du DEMANDEUR. Obéir donnerait à n'importe quel pair une primitive
// d'effacement à distance : diffuser des `stem-avail` aux hashes bidons sur tout le catalogue
// viderait le cache de tout l'essaim. Et un désaccord n'implique même pas qu'on ait tort : un
// navigateur au catalogue daté demande avec l'ancien hash.
func TestServesStaysSilentAndKeepsFile(t *testing.T) {
	s := newStore(t)
	data := []byte("ciphertext")
	if err := s.Commit(7, StemVocals, data, hashOf(data)); err != nil {
		t.Fatal(err)
	}

	if !s.Serves(7, StemVocals, hashOf(data)) {
		t.Error("un hash conforme doit être servi")
	}
	if s.Serves(7, StemVocals, hashOf([]byte("autre chose"))) {
		t.Error("un hash non conforme ne doit PAS être servi")
	}
	// LE point : le fichier est toujours là.
	if !s.Has(7, StemVocals) {
		t.Fatal("le fichier a été supprimé sur un désaccord de hash — primitive d'effacement à distance")
	}
	// Et un pair qui ne précise rien reste servi (comportement historique du seeder ; le receveur
	// vérifie de toute façon ce qu'il reçoit).
	if !s.Serves(7, StemVocals, "") {
		t.Error("sans hash annoncé, le stem doit rester servi")
	}
}

// Le cache de hash est invalidé par (mtime, taille) — pas par mtime seul, dont la granularité
// peut masquer un remplacement.
func TestHashCacheInvalidation(t *testing.T) {
	s := newStore(t)
	v1 := []byte("version 1")
	if err := s.Commit(9, StemVocals, v1, hashOf(v1)); err != nil {
		t.Fatal(err)
	}
	if got := s.Hash(9, StemVocals); got != hashOf(v1) {
		t.Fatalf("hash initial faux : %s", got)
	}

	v2 := []byte("version 2 — même longueur ?")
	if err := s.Commit(9, StemVocals, v2, hashOf(v2)); err != nil {
		t.Fatal(err)
	}
	if got := s.Hash(9, StemVocals); got != hashOf(v2) {
		t.Errorf("hash périmé servi depuis le cache : %s", got)
	}
}

// Un fichier en cours de téléchargement ne doit jamais être vu comme du contenu disponible.
func TestScanIgnoresTemporaries(t *testing.T) {
	s := newStore(t)
	data := []byte("x")
	if err := s.Commit(11, StemAccompaniment, data, hashOf(data)); err != nil {
		t.Fatal(err)
	}
	// Temporaire abandonné par un plantage, portant le nom d'un stem plausible.
	tmp := filepath.Join(s.Dir(), ".12-vocals.123.part")
	if err := os.WriteFile(tmp, []byte("moitié de fichier"), 0o644); err != nil {
		t.Fatal(err)
	}

	inv, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if _, seen := inv[12]; seen {
		t.Error("un temporaire a été inventorié comme contenu disponible")
	}
	if len(inv) != 1 || inv[11][0] != hashOf(data) {
		t.Errorf("inventaire inattendu : %v", inv)
	}
	if n := s.SweepTemp(); n != 1 {
		t.Errorf("SweepTemp devait retirer 1 temporaire, en a retiré %d", n)
	}
}

func TestParseNameRejectsJunk(t *testing.T) {
	cases := []string{
		"1384-vocals.m4a",   // ancienne extension
		"1384-drums.kok",    // stem inexistant
		"abc-vocals.kok",    // id non numérique
		"-vocals.kok",       // id vide
		".1384-vocals.part", // temporaire
		"1384.kok",          // pas de stem
		"0-vocals.kok",      // id nul
	}
	for _, name := range cases {
		if _, _, ok := parseName(name); ok {
			t.Errorf("%q aurait dû être rejeté", name)
		}
	}
	if id, stem, ok := parseName("1384-accompaniment.kok"); !ok || id != 1384 || stem != StemAccompaniment {
		t.Errorf("nom valide rejeté : %d %s %v", id, stem, ok)
	}
}

// DiskUsage ne doit compter que du contenu réel — un temporaire de 8 Mo ne doit pas être présenté
// comme du cache utile.
func TestDiskUsageExcludesTemporaries(t *testing.T) {
	s := newStore(t)
	data := make([]byte, 1024)
	if err := s.Commit(3, StemVocals, data, hashOf(data)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), ".3-vocals.abc.part"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	files, bytes := s.DiskUsage()
	if files != 1 || bytes != 1024 {
		t.Errorf("attendu 1 fichier / 1024 o, obtenu %d / %d", files, bytes)
	}
}

// Un nom de stem qui n'est pas l'un des deux valides ne doit produire AUCUN chemin exploitable.
//
// `Path` construisait le chemin sans valider, et `filepath.Join` NETTOIE les `..` : un stem de la
// forme `x/../../../etc/foo` sortait donc du dossier de cache. Les trois entrées réseau appelaient
// bien `ValidStem` avant, donc rien n'était exploitable — mais la sûreté reposait sur trois
// appelants devant y penser chacun, sans garde au point de passage obligé. Ce test verrouille le
// garde lui-même, pas la discipline des appelants.
func TestPathRefusesStemsHorsListe(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	hostiles := []string{
		"../../../../etc/passwd",
		"x/../../../../etc/passwd",
		"vocals/../../../evasion",
		"..",
		"",
		"VOCALS",         // la casse compte : ce n'est pas l'un des deux noms
		"accompaniment ", // espace final
	}
	for _, stem := range hostiles {
		if p := s.Path(1, stem); p != "" {
			t.Errorf("Path(1, %q) = %q, attendu \"\"", stem, p)
		}
		// Et les accès dérivés doivent se comporter comme « ce stem n'existe pas ».
		if s.Has(1, stem) {
			t.Errorf("Has(1, %q) = true", stem)
		}
		if got := s.Size(1, stem); got != 0 {
			t.Errorf("Size(1, %q) = %d, attendu 0", stem, got)
		}
		if got := s.Hash(1, stem); got != "" {
			t.Errorf("Hash(1, %q) = %q, attendu \"\"", stem, got)
		}
		if _, err := s.ReadAll(1, stem); err == nil {
			t.Errorf("ReadAll(1, %q) a réussi, une erreur était attendue", stem)
		}
		if s.Serves(1, stem, "") {
			t.Errorf("Serves(1, %q, \"\") = true", stem)
		}
	}

	// CONTRÔLE NÉGATIF : les deux stems légitimes doivent, eux, donner un chemin dans le dossier.
	for _, stem := range Stems {
		p := s.Path(42, stem)
		if p == "" {
			t.Fatalf("Path(42, %q) est vide — le garde refuse un stem valide", stem)
		}
		if filepath.Dir(p) != dir {
			t.Fatalf("Path(42, %q) = %q, hors du dossier %q", stem, p, dir)
		}
	}
}
