package peer

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/lianee/kok-cache/internal/store"
)

// Ces tests couvrent ce qu'un pair MALVEILLANT peut tenter, par opposition au reste du harnais qui
// vérifie qu'un échange honnête aboutit. Ils sont nés d'un audit du 2026-08-30 : les défauts
// trouvés étaient tous des valeurs venues du réseau qui pilotaient une allocation ou une création
// de ressource, sans plafond.

// leechSessionFor rend la session de récupération en cours pour ce stem, ou nil.
func leechSessionFor(m *Manager, songID int, stem string) *leechSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leeching[store.Key(songID, stem)]
}

func startedPC(l *leechSession) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pc != nil
}

// Un `stem-resp` annonçant une taille absurde doit être IGNORÉ, sans allocation ni connexion.
//
// Avant le correctif, `l.size` était repris tel quel puis servait à `make([]byte, 0, size)` et à
// une boucle d'émission de plages : un seul message suffisait à faire paniquer — donc mourir — le
// processus d'un inconnu. Le côté envoi était borné (`maxRangeReq`), pas le côté réception.
func TestAnnouncedSizeAbsurdIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	defer rt.srv.Close()

	oracle := &fakeOracle{hashes: map[string]string{}}
	victim := newNode(t, ctx, rt, "victime", oracle)
	waitAuth(t, victim)

	const songID = 4242
	_, hash := randomStem(t, 1024)
	oracle.hashes[store.Key(songID, store.StemVocals)] = hash

	cases := []struct {
		name string
		size int64
	}{
		{"maxint64", math.MaxInt64},
		{"4 Go", 4 << 30},
		{"juste au-dessus du plafond", maxStemSize + 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			victim.mgr.Request(songID, store.StemVocals, hash)
			l := leechSessionFor(victim.mgr, songID, store.StemVocals)
			if l == nil {
				t.Fatal("la session de recherche n'a pas été créée")
			}

			victim.mgr.onResp(wireMsg{
				Type: msgResp, SongID: flexInt(songID), Stem: store.StemVocals,
				PeerID: "attaquant", ToPeerID: victim.sig.MyID(),
				V: stemProtocolVersion, Size: tc.size,
			}, victim.sig.MyID())

			if startedPC(l) {
				t.Fatalf("taille %d acceptée : une PeerConnection a été ouverte", tc.size)
			}
			l.finish(nil) // libère la clé pour le cas suivant
		})
	}
}

// CONTRÔLE NÉGATIF — sans lui, le test ci-dessus passerait même si `onResp` ne faisait plus rien
// du tout. Une taille plausible DOIT être acceptée et ouvrir une connexion.
func TestAnnouncedSizePlausibleIsAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	defer rt.srv.Close()

	oracle := &fakeOracle{hashes: map[string]string{}}
	victim := newNode(t, ctx, rt, "victime", oracle)
	waitAuth(t, victim)

	const songID = 4243
	_, hash := randomStem(t, 1024)
	oracle.hashes[store.Key(songID, store.StemVocals)] = hash

	victim.mgr.Request(songID, store.StemVocals, hash)
	l := leechSessionFor(victim.mgr, songID, store.StemVocals)
	if l == nil {
		t.Fatal("la session de recherche n'a pas été créée")
	}
	victim.mgr.onResp(wireMsg{
		Type: msgResp, SongID: flexInt(songID), Stem: store.StemVocals,
		PeerID: "pair-honnête", ToPeerID: victim.sig.MyID(),
		V: stemProtocolVersion, Size: 8 << 20, // 8 Mo, taille réaliste d'un stem
	}, victim.sig.MyID())

	if !startedPC(l) {
		t.Fatal("une taille plausible a été refusée — le plafond est trop bas, ou onResp est cassé")
	}
	l.finish(nil)
}

// Une rafale de `stem-get` aux identifiants de pair inédits ne doit pas créer une session (et donc
// une PeerConnection) par identifiant. `peerId` est une valeur fournie par le pair : sans plafond,
// elle pilotait directement l'allocation de ressources système.
func TestSeedSessionsAreCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	defer rt.srv.Close()

	oracle := &fakeOracle{hashes: map[string]string{}}
	n := newNode(t, ctx, rt, "cible", oracle)
	waitAuth(t, n)

	// La cible doit détenir le stem, sinon `onGet` refuse pour une autre raison et le test ne
	// prouverait rien du plafond.
	const songID = 77
	data, hash := randomStem(t, 4096)
	oracle.hashes[store.Key(songID, store.StemVocals)] = hash
	if err := n.st.Commit(songID, store.StemVocals, data, hash); err != nil {
		t.Fatal(err)
	}

	myID := n.sig.MyID()
	for i := 0; i < maxSeedSessions*3; i++ {
		n.mgr.onGet(wireMsg{
			Type: msgGet, SongID: flexInt(songID), Stem: store.StemVocals,
			PeerID: "attaquant-" + itoa(i), ToPeerID: myID,
			V: stemProtocolVersion,
		}, myID)
	}

	n.mgr.mu.Lock()
	got := len(n.mgr.seeding)
	n.mgr.mu.Unlock()
	if got > maxSeedSessions {
		t.Fatalf("%d sessions d'envoi créées, plafond %d", got, maxSeedSessions)
	}
	if got == 0 {
		t.Fatal("aucune session créée — le test ne prouve rien, vérifier que le stem est bien détenu")
	}
}
