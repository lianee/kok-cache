package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lianee/kok-cache/internal/api"
	"github.com/lianee/kok-cache/internal/store"
)

type fakeAPI struct {
	manifest map[int][2]string
	sig      string
	calls    int
	failWith error
}

func (f *fakeAPI) StemManifest(_ context.Context, knownSig string) (*api.Manifest, error) {
	f.calls++
	if f.failWith != nil {
		return nil, f.failWith
	}
	if knownSig != "" && knownSig == f.sig {
		// C'est le chemin qui rend une vérification fréquente acceptable : 87 octets au lieu de
		// ~400 Ko.
		return &api.Manifest{Sig: f.sig, Count: len(f.manifest), Unchanged: true}, nil
	}
	return &api.Manifest{Sig: f.sig, Count: len(f.manifest), Stems: f.manifest}, nil
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func newMirror(t *testing.T, st *store.Store, apiClient manifestClient) *Mirror {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(apiClient, st, nil, t.TempDir(), time.Minute, true, log)
}

// Pending applique le prédicat de synchronisation à tout le catalogue : absent OU hash différent.
// Le cas central est le stem PÉRIMÉ, qu'un prédicat « manquant » ne verrait jamais.
func TestPendingUsesAbsentOrDifferentHash(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	aJour := []byte("stem à jour")
	perime := []byte("stem tel qu'il était avant le re-split")

	// 1 : les deux stems à jour · 2 : accompagnement périmé · 3 : rien sur le disque
	f := &fakeAPI{sig: "sig1", manifest: map[int][2]string{
		1: {hashOf(aJour), hashOf(aJour)},
		2: {hashOf(aJour), hashOf(aJour)},
		3: {hashOf(aJour), hashOf(aJour)},
	}}
	for _, id := range []int{1, 2} {
		for _, stem := range store.Stems {
			content := aJour
			if id == 2 && stem == store.StemAccompaniment {
				content = perime
			}
			if err := st.Commit(id, stem, content, hashOf(content)); err != nil {
				t.Fatal(err)
			}
		}
	}

	m := newMirror(t, st, f)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	pending := m.Pending()
	want := map[string]bool{
		store.Key(2, store.StemAccompaniment): true, // présent mais PÉRIMÉ
		store.Key(3, store.StemAccompaniment): true, // absent
		store.Key(3, store.StemVocals):        true, // absent
	}
	if len(pending) != len(want) {
		t.Fatalf("attendu %d stems à synchroniser, obtenu %d : %+v", len(want), len(pending), pending)
	}
	for _, it := range pending {
		if !want[store.Key(it.SongID, it.Stem)] {
			t.Errorf("stem inattendu dans la liste : %d-%s", it.SongID, it.Stem)
		}
	}
}

// Une signature connue évite le transfert du manifeste complet, et l'état conservé reste
// utilisable.
func TestRefreshUnchangedKeepsCatalog(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAPI{sig: "sig1", manifest: map[int][2]string{7: {"aaa", "bbb"}}}
	m := newMirror(t, st, f)

	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Count() != 1 || m.ExpectedHash(7, store.StemVocals) != "bbb" {
		t.Fatalf("catalogue mal chargé : %d entrées", m.Count())
	}
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Count() != 1 || m.ExpectedHash(7, store.StemAccompaniment) != "aaa" {
		t.Error("une réponse « inchangé » a vidé le catalogue")
	}
	if f.calls != 2 {
		t.Errorf("attendu 2 appels, obtenu %d", f.calls)
	}
}

// Le manifeste survit à un redémarrage : sans lui, une instance hors ligne n'aurait aucun hash de
// référence, donc ne pourrait ni servir en confiance ni écrire quoi que ce soit.
func TestManifestPersistsAcrossRestart(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	f := &fakeAPI{sig: "sig1", manifest: map[int][2]string{42: {"aaa", "bbb"}}}
	m1 := New(f, st, nil, cacheDir, time.Minute, true, log)
	if err := m1.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Redémarrage, serveur injoignable.
	offline := &fakeAPI{failWith: errors.New("hors ligne")}
	m2 := New(offline, st, nil, cacheDir, time.Minute, true, log)
	if m2.ExpectedHash(42, store.StemVocals) != "bbb" {
		t.Error("le manifeste n'a pas été rechargé après redémarrage")
	}
	if m2.Sig() != "sig1" {
		t.Error("la signature n'a pas été conservée")
	}
}

// ExpectedHash ne doit rien inventer : un stem inconnu du catalogue n'a pas de référence, donc
// n'est jamais écrit sur le disque.
func TestExpectedHashUnknownIsEmpty(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := newMirror(t, st, &fakeAPI{sig: "s", manifest: map[int][2]string{1: {"a", "b"}}})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.ExpectedHash(999, store.StemVocals) != "" {
		t.Error("un stem hors catalogue ne doit avoir aucun hash de référence")
	}
	if m.ExpectedHash(1, "drums") != "" {
		t.Error("un stem inexistant ne doit avoir aucun hash de référence")
	}
}

// Un manifeste VIDE doit être traité comme une panne de l'autorité, pas comme un catalogue vide.
//
// Sans ce garde, une réponse `success:true` sans aucune entrée — bogue serveur, requête cassée,
// base en maintenance — écrasait la liste EN MÉMOIRE et SUR DISQUE. Le cache cessait alors de
// récupérer quoi que ce soit (le prédicat exige un hash de référence), l'état survivait au
// redémarrage, et `prune` voyait tout le contenu comme orphelin.
func TestRefreshRejectsEmptyManifest(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	good := map[int][2]string{
		1: {hashOf([]byte("1a")), hashOf([]byte("1v"))},
		2: {hashOf([]byte("2a")), hashOf([]byte("2v"))},
	}
	f := &fakeAPI{manifest: good, sig: "v1"}
	m := newMirror(t, st, f)

	if err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("premier manifeste refusé : %v", err)
	}
	if m.Count() != 2 {
		t.Fatalf("catalogue à %d chansons, attendu 2", m.Count())
	}

	// Le serveur se met à répondre « aucune chanson », avec une signature neuve pour forcer la
	// prise en compte.
	f.manifest = map[int][2]string{}
	f.sig = "v2"

	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("un manifeste vide a été accepté — il doit être signalé comme une panne")
	}
	// CE QUI COMPTE VRAIMENT : l'ancien catalogue survit.
	if m.Count() != 2 {
		t.Fatalf("catalogue écrasé : %d chansons, attendu 2", m.Count())
	}
	if m.ExpectedHash(1, store.StemVocals) != good[1][1] {
		t.Fatal("les hashes de référence ont été perdus — plus rien ne serait récupérable")
	}
	if m.Sig() == "v2" {
		t.Fatal("la signature vide a été retenue : le prochain appel croirait le catalogue à jour")
	}
}

// fakeFetcher enregistre l'ordre des demandes et les fait toutes échouer aussitôt : seul l'ORDRE
// compte ici, pas le transfert.
type fakeFetcher struct {
	order     []int
	onRequest func(songID int)
}

func (f *fakeFetcher) RequestSync(songID int, _ string, _ string, done func(error)) {
	f.order = append(f.order, songID)
	if f.onRequest != nil {
		f.onRequest(songID)
	}
	done(errors.New("personne"))
}
func (f *fakeFetcher) Ready() bool { return true }

// Un passage démarre à un décalage dans la liste triée, puis boucle : deux instances qui partent
// en même temps ne travaillent pas la même région, donc l'une sert l'autre au lieu que toutes deux
// tirent du seeder. Contrôle négatif effectué : sans la rotation dans syncPass, l'ordre obtenu est
// 1 1 2 2 3 3 4 4 5 5 et le test échoue.
func TestSyncPassStartsAtOffsetAndWrapsAround(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hashOf([]byte("x"))
	f := &fakeAPI{sig: "sig1", manifest: map[int][2]string{1: {h, h}, 2: {h, h}, 3: {h, h}, 4: {h, h}, 5: {h, h}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ff := &fakeFetcher{}
	m := New(f, st, ff, t.TempDir(), time.Minute, true, log)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.startOffset = func(n int) int {
		if n != 10 {
			t.Errorf("décalage demandé sur %d éléments, attendu 10", n)
		}
		return 7 // second stem de la chanson 4 : le départ n'est pas aligné sur une chanson
	}
	m.syncPass(context.Background())

	want := []int{4, 5, 5, 1, 1, 2, 2, 3, 3, 4}
	if len(ff.order) != len(want) {
		t.Fatalf("ordre %v, attendu %v", ff.order, want)
	}
	for i := range want {
		if ff.order[i] != want[i] {
			t.Fatalf("ordre %v, attendu %v", ff.order, want)
		}
	}
}

// Un décalage nul ou hors bornes laisse la liste intacte : un choix de départ ne fait jamais
// perdre un stem.
func TestRotatedKeepsEveryItem(t *testing.T) {
	items := []Item{{SongID: 1}, {SongID: 2}, {SongID: 3}}
	for _, k := range []int{-1, 0, 3, 7} {
		if got := rotated(items, k); len(got) != 3 || got[0].SongID != 1 {
			t.Errorf("k=%d : %v", k, got)
		}
	}
	if got := rotated(items, 2); got[0].SongID != 3 || got[1].SongID != 1 || got[2].SongID != 2 {
		t.Errorf("k=2 : %v", got)
	}
	if got := rotated(nil, 0); len(got) != 0 {
		t.Errorf("vide : %v", got)
	}
}

// stockage de test : 1 à jour · 2 accompagnement périmé · 3 absent · 9 hors catalogue.
func staleFixture(t *testing.T) (*store.Store, *fakeAPI) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	aJour, perime := []byte("à jour"), []byte("ancienne version")
	f := &fakeAPI{sig: "sig1", manifest: map[int][2]string{
		1: {hashOf(aJour), hashOf(aJour)},
		2: {hashOf(aJour), hashOf(aJour)},
		3: {hashOf(aJour), hashOf(aJour)},
	}}
	must := func(id int, stem string, b []byte) {
		if err := st.Commit(id, stem, b, hashOf(b)); err != nil {
			t.Fatal(err)
		}
	}
	must(1, store.StemAccompaniment, aJour)
	must(1, store.StemVocals, aJour)
	must(2, store.StemAccompaniment, perime)
	must(2, store.StemVocals, aJour)
	must(9, store.StemAccompaniment, perime)
	must(9, store.StemVocals, perime)
	return st, f
}

// Hors mode miroir, un fichier périmé est du poids mort définitif : dropStale ne retire QUE lui.
// Une chanson sortie du catalogue (9) reste du ressort de `prune`. Contrôle négatif effectué : avec
// l'appel à Remove retiré de dropStale, 2-accompaniment subsiste et le test échoue.
func TestDropStaleRemovesOnlyStaleFilesOfCataloguedSongs(t *testing.T) {
	st, f := staleFixture(t)
	m := newMirror(t, st, f)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.dropStale()

	if st.Has(2, store.StemAccompaniment) {
		t.Error("2-accompaniment (périmé) aurait dû être supprimé")
	}
	for _, k := range [][2]any{{1, store.StemAccompaniment}, {1, store.StemVocals}, {2, store.StemVocals}, {9, store.StemAccompaniment}, {9, store.StemVocals}} {
		if !st.Has(k[0].(int), k[1].(string)) {
			t.Errorf("%v-%v aurait dû rester", k[0], k[1])
		}
	}
	if len(m.Pending()) != 3 { // 2-acc et 3-acc/voc : absents, à récupérer si le miroir s'active
		t.Errorf("pending %v", m.Pending())
	}
}

// Le câblage dans Run : le rafraîchissement périodique supprime le périmé quand le miroir est
// inactif, et le laisse en place (remplacement à venir) quand il est actif.
func TestRunDropsStaleOnlyWhenMirrorInactive(t *testing.T) {
	for _, active := range []bool{false, true} {
		st, f := staleFixture(t)
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		m := New(f, st, &fakeFetcher{}, t.TempDir(), 20*time.Millisecond, active, log)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { m.Run(ctx); close(done) }()
		// Run attend 5 s avant sa première vérification : on la force par le canal wake.
		m.wake <- struct{}{}
		deadline := time.Now().Add(2 * time.Second)
		for st.Has(2, store.StemAccompaniment) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done
		if has := st.Has(2, store.StemAccompaniment); has == !active {
			t.Errorf("miroir actif=%v : 2-accompaniment présent=%v", active, has)
		}
	}
}

// Verrou détecté pendant la chanson 2 : le passage finit ses deux stems, puis repart d'un point
// aléatoire du RESTE (ici décalage 2 dans [3 3 4 4 5 5] → 4 4 5 5 3 3), sans rien oublier ni
// refaire. Contrôle négatif effectué : sans le décrochage dans syncPass, l'ordre reste 1 1 2 2 3 3
// 4 4 5 5 et le test échoue.
func TestSyncPassJumpsAwayWhenContended(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hashOf([]byte("x"))
	f := &fakeAPI{sig: "sig1", manifest: map[int][2]string{1: {h, h}, 2: {h, h}, 3: {h, h}, 4: {h, h}, 5: {h, h}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var m *Mirror
	ff := &fakeFetcher{onRequest: func(songID int) {
		if songID == 2 {
			m.Contended(songID) // l'autre miroir demande 2 pendant que nous le téléchargeons
		}
	}}
	m = New(f, st, ff, t.TempDir(), time.Minute, true, log)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.startOffset = func(n int) int {
		calls++
		if calls == 1 {
			return 0 // départ en tête de liste
		}
		return 2 // reprise : deux éléments plus loin dans le reste
	}
	m.syncPass(context.Background())

	want := []int{1, 1, 2, 2, 4, 4, 5, 5, 3, 3}
	if len(ff.order) != len(want) {
		t.Fatalf("ordre %v, attendu %v", ff.order, want)
	}
	for i := range want {
		if ff.order[i] != want[i] {
			t.Fatalf("ordre %v, attendu %v", ff.order, want)
		}
	}
	if calls != 2 {
		t.Errorf("startOffset appelé %d fois, attendu 2 (départ + un décrochage)", calls)
	}
}
