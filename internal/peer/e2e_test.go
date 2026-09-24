package peer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/lianee/kok-cache/internal/api"
	"github.com/lianee/kok-cache/internal/signal"
	"github.com/lianee/kok-cache/internal/store"
)

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// Ce fichier fait tourner DEUX instances de kok-cache face à face, reliées par un faux `kok-rt`,
// et leur fait transférer un stem pour de vrai : offre/réponse SDP, candidats ICE, DataChannel,
// demande de plage, blocs de 16 Ko, vérification du hash, renommage atomique.
//
// Pourquoi ce niveau d'effort plutôt que des tests unitaires du dispatch : le piège le plus
// coûteux de la migration de la signalisation (`pathname` ignoré par Primus) a été trouvé par un
// harnais et PAS par la lecture du code — un grep avait même « confirmé » l'option. Un protocole
// ne se vérifie qu'en le parlant.
// ═══════════════════════════════════════════════════════════════════════════════════════════════

// ── faux serveur de signalisation ─────────────────────────────────────────────────────────────

type fakeRT struct {
	srv *httptest.Server

	mu    sync.Mutex
	conns map[string]*fakeConn
	seq   int
}

type fakeConn struct {
	id string
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *fakeConn) send(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.TextMessage, raw)
}

func (c *fakeConn) sendRaw(raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.TextMessage, raw)
}

func newFakeRT(t *testing.T) *fakeRT {
	t.Helper()
	f := &fakeRT{conns: map[string]*fakeConn{}}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.seq++
		c := &fakeConn{id: "spark-" + itoa(f.seq), ws: ws}
		f.conns[c.id] = c
		f.mu.Unlock()

		// `hello` AVANT l'authentification : c'est ainsi qu'un pair apprend son propre id, dont il
		// a besoin pour l'adressage dirigé.
		c.send(map[string]any{"hello": c.id})

		go func() {
			defer func() {
				f.mu.Lock()
				delete(f.conns, c.id)
				f.mu.Unlock()
				ws.Close()
			}()
			for {
				_, raw, err := ws.ReadMessage()
				if err != nil {
					return
				}
				f.route(c, raw)
			}
		}()
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRT) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeRT) route(from *fakeConn, raw []byte) {
	var env struct {
		Cmd  string          `json:"cmd"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return
	}
	switch env.Cmd {
	case "auth":
		from.send(map[string]any{"cmd": "auth_ok", "role": "cache", "uid": 1})
	case "get_turn_creds":
		// Aucun relais dans ce test : les deux pairs sont sur la même machine, les candidats hôtes
		// suffisent. Répondre sans URL laisse la configuration ICE inchangée.
	case "stem_broadcast":
		var d struct {
			ToPeerID string `json:"toPeerId"`
		}
		_ = json.Unmarshal(env.Data, &d)
		f.mu.Lock()
		defer f.mu.Unlock()
		if d.ToPeerID != "" {
			if target, ok := f.conns[d.ToPeerID]; ok {
				target.sendRaw(raw)
			}
			return
		}
		for id, c := range f.conns {
			if id != from.id {
				c.sendRaw(raw)
			}
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ── doublures ─────────────────────────────────────────────────────────────────────────────────

type fakeTickets struct{}

func (fakeTickets) RTTicket(context.Context, string) (api.Ticket, error) {
	return api.Ticket{Value: "v1.test.test", TTL: time.Hour, IssuedAt: time.Now()}, nil
}

// fakeOracle joue le rôle du manifeste : l'AUTORITÉ sur les hashes.
type fakeOracle struct {
	mu        sync.Mutex
	hashes    map[string]string
	suspects  []int
	contended []int
}

func (o *fakeOracle) ExpectedHash(songID int, stem string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hashes[store.Key(songID, stem)]
}

func (o *fakeOracle) Suspect(songID int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.suspects = append(o.suspects, songID)
}

func (o *fakeOracle) Contended(songID int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.contended = append(o.contended, songID)
}

func (o *fakeOracle) suspected() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.suspects...)
}

// ── montage ───────────────────────────────────────────────────────────────────────────────────

type node struct {
	st     *store.Store
	mgr    *Manager
	sig    *signal.Client
	oracle *fakeOracle
}

func newNode(t *testing.T, ctx context.Context, rt *fakeRT, name string, oracle *fakeOracle) *node {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// KOK_TEST_LOG=1 rend le déroulé du protocole visible — c'est par là qu'on diagnostique, pas
	// en relisant le code.
	out := io.Discard
	if os.Getenv("KOK_TEST_LOG") != "" {
		out = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})).With("node", name)

	mgr := NewManager(ctx, st, oracle, log)
	// Pas de STUN : les deux pairs sont sur la même machine et le test ne doit dépendre d'aucun
	// service extérieur.
	mgr.ice = []webrtc.ICEServer{}

	sig := signal.New(rt.url(), name, fakeTickets{}, mgr, 200*time.Millisecond, log)
	mgr.Attach(sig)
	go sig.Run(ctx)

	return &node{st: st, mgr: mgr, sig: sig, oracle: oracle}
}

func waitAuth(t *testing.T, nodes ...*node) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, n := range nodes {
		for !n.sig.Authenticated() || n.sig.MyID() == "" {
			if time.Now().After(deadline) {
				t.Fatal("authentification jamais obtenue auprès du faux kok-rt")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func randomStem(t *testing.T, size int) ([]byte, string) {
	t.Helper()
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf)
	return buf, hex.EncodeToString(sum[:])
}

// ── le test principal ─────────────────────────────────────────────────────────────────────────

// Un transfert complet, de bout en bout : B ne détient rien, A détient le stem, B le demande et
// finit avec un fichier vérifié sur son disque.
func TestTransferBetweenTwoCaches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	oracleA := &fakeOracle{hashes: map[string]string{}}
	oracleB := &fakeOracle{hashes: map[string]string{}}

	a := newNode(t, ctx, rt, "seed", oracleA)
	b := newNode(t, ctx, rt, "cache", oracleB)
	waitAuth(t, a, b)

	// 1 Mo : assez pour 64 blocs de 16 Ko, donc pour exercer réellement la boucle d'envoi et le
	// contrôle de flux, sans allonger le test.
	const songID = 1384
	data, hash := randomStem(t, 1<<20)
	if err := a.st.Commit(songID, store.StemVocals, data, hash); err != nil {
		t.Fatal(err)
	}
	oracleA.hashes[store.Key(songID, store.StemVocals)] = hash
	oracleB.hashes[store.Key(songID, store.StemVocals)] = hash

	done := make(chan error, 1)
	b.mgr.RequestWithCallback(songID, store.StemVocals, hash, func(err error) { done <- err })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("transfert échoué : %v (offres perdues côté serveur : %d)",
				err, a.mgr.droppedOffers.Load())
		}
	case <-ctx.Done():
		t.Fatal("transfert jamais terminé")
	}

	if !b.st.Has(songID, store.StemVocals) {
		t.Fatal("le fichier n'est pas arrivé sur le disque du demandeur")
	}
	if got := b.st.Hash(songID, store.StemVocals); got != hash {
		t.Fatalf("contenu reçu incorrect : %s au lieu de %s", got, hash)
	}
	// Le créneau d'envoi doit être rendu : sans ça, un pair finit par refuser tout service en
	// boucle — c'est le bug de fuite de créneau observé en production côté Node.
	waitFor(t, 5*time.Second, func() bool { return a.mgr.activeSends.Load() == 0 }, "créneau d'envoi jamais libéré")
}

// Un stem de PLUS DE 8 Mo doit se transférer normalement.
//
// ⛔ RÉGRESSION RÉELLE, observée en production le 2026-08-28 : le client demandait le fichier
// entier en une seule plage, or un pair IGNORE SILENCIEUSEMENT toute demande dépassant
// `maxRangeReq` (8 Mo) — pas d'erreur, pas de réponse — puis ferme au bout de 30 s sur son propre
// délai d'absence de progrès. Résultat : les chansons longues échouaient TOUJOURS (1, 4, 8, 9…),
// les courtes passaient TOUJOURS, et le symptôme visible était un « abort » incompréhensible.
//
// Ce test échoue avec l'ancien code et passe avec le découpage en plages de 4 Mo. Les tests
// précédents utilisaient 1 Mo — donc aucun ne pouvait voir le défaut.
func TestTransferLargerThanMaxRange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	oracleA := &fakeOracle{hashes: map[string]string{}}
	oracleB := &fakeOracle{hashes: map[string]string{}}

	a := newNode(t, ctx, rt, "seed", oracleA)
	b := newNode(t, ctx, rt, "cache", oracleB)
	waitAuth(t, a, b)

	const songID = 1
	// 9 Mo : au-delà du plafond de 8 Mo, comme les stems des chansons longues.
	data, hash := randomStem(t, 9<<20)
	if err := a.st.Commit(songID, store.StemAccompaniment, data, hash); err != nil {
		t.Fatal(err)
	}
	oracleA.hashes[store.Key(songID, store.StemAccompaniment)] = hash
	oracleB.hashes[store.Key(songID, store.StemAccompaniment)] = hash

	done := make(chan error, 1)
	b.mgr.RequestWithCallback(songID, store.StemAccompaniment, hash, func(err error) { done <- err })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("transfert d'un stem de 9 Mo échoué : %v", err)
		}
	case <-ctx.Done():
		t.Fatal("transfert jamais terminé — demande de plage probablement ignorée pour dépassement")
	}
	if got := b.st.Hash(songID, store.StemAccompaniment); got != hash {
		t.Fatalf("contenu reçu incorrect : %s au lieu de %s", got, hash)
	}
}

// Un pair qui annonce un hash différent du nôtre ne doit RIEN obtenir — ni le fichier, ni sa
// suppression. Le désaccord ne produit qu'un indice, transmis à l'autorité.
func TestHashMismatchStaysSilentAndKeepsFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	oracleA := &fakeOracle{hashes: map[string]string{}}
	oracleB := &fakeOracle{hashes: map[string]string{}}

	a := newNode(t, ctx, rt, "seed", oracleA)
	b := newNode(t, ctx, rt, "cache", oracleB)
	waitAuth(t, a, b)

	const songID = 999
	data, hash := randomStem(t, 64<<10)
	if err := a.st.Commit(songID, store.StemVocals, data, hash); err != nil {
		t.Fatal(err)
	}
	oracleA.hashes[store.Key(songID, store.StemVocals)] = hash

	// B demande le MÊME stem avec un hash périmé (le cas réel : catalogue daté côté demandeur).
	_, staleHash := randomStem(t, 8)
	oracleB.hashes[store.Key(songID, store.StemVocals)] = staleHash

	done := make(chan error, 1)
	b.mgr.RequestWithCallback(songID, store.StemVocals, staleHash, func(err error) { done <- err })

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("un transfert a abouti alors que le hash demandé ne correspond à rien")
		}
	case <-ctx.Done():
		t.Fatal("la recherche n'a jamais rendu la main")
	}

	// A a toujours son fichier : le désaccord n'a rien effacé.
	if !a.st.Has(songID, store.StemVocals) {
		t.Fatal("le fichier a été supprimé sur désaccord de hash — primitive d'effacement à distance")
	}
	// B n'a rien écrit.
	if b.st.Has(songID, store.StemVocals) {
		t.Fatal("un fichier a été écrit alors qu'aucun contenu conforme n'a été reçu")
	}
	// Et A a bien remonté l'INDICE à son autorité, sans agir de lui-même.
	waitFor(t, 5*time.Second, func() bool { return len(oracleA.suspected()) > 0 },
		"le désaccord n'a pas été signalé à l'autorité")
}

// Un `stem-delete` venu de l'essaim ne doit avoir AUCUN effet : obéir donnerait à n'importe quel
// pair une primitive d'effacement à distance sur les disques d'inconnus.
func TestStemDeleteFromPeerIsIgnored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	oracleA := &fakeOracle{hashes: map[string]string{}}
	oracleB := &fakeOracle{hashes: map[string]string{}}

	a := newNode(t, ctx, rt, "seed", oracleA)
	b := newNode(t, ctx, rt, "attacker", oracleB)
	waitAuth(t, a, b)

	const songID = 4242
	data, hash := randomStem(t, 4096)
	if err := a.st.Commit(songID, store.StemVocals, data, hash); err != nil {
		t.Fatal(err)
	}

	b.sig.Broadcast(wireMsg{Type: msgDelete, SongID: songID, PeerID: b.sig.MyID()})
	time.Sleep(1500 * time.Millisecond)

	if !a.st.Has(songID, store.StemVocals) {
		t.Fatal("un pair a réussi à faire supprimer un fichier à distance")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *Manager) isLeeching(songID int, stem string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.leeching[store.Key(songID, stem)]
	return ok
}

func (o *fakeOracle) contendedIDs() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.contended...)
}

// Une demande de BALAYAGE (`sync`) n'est pas poursuivie par l'autre miroir, alors qu'une demande
// ordinaire l'est (récupération opportuniste) ; et si l'autre miroir téléchargeait déjà ce stem,
// il en est averti (Contended). Contrôle négatif effectué : sans le `if msg.Sync` de onAvail, A se
// met à télécharger 777 sur la demande sync de B et le test échoue.
func TestSyncRequestIsNotChasedButFlagsContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	rt := newFakeRT(t)
	_, hash := randomStem(t, 8)
	keys := map[string]string{store.Key(777, store.StemVocals): hash, store.Key(778, store.StemVocals): hash, store.Key(779, store.StemVocals): hash}
	oracleA := &fakeOracle{hashes: keys}
	oracleB := &fakeOracle{hashes: keys}
	a := newNode(t, ctx, rt, "A", oracleA)
	b := newNode(t, ctx, rt, "B", oracleB)
	waitAuth(t, a, b)

	// (1) demande sync de B : A ne la poursuit pas.
	b.mgr.RequestSync(777, store.StemVocals, hash, nil)
	time.Sleep(1500 * time.Millisecond)
	if a.mgr.isLeeching(777, store.StemVocals) {
		t.Fatal("A poursuit une demande de balayage : les deux miroirs retéléchargent la même chanson")
	}
	if len(oracleA.contendedIDs()) != 0 {
		t.Fatalf("A n'était pas sur 777, aucun verrou attendu, obtenu %v", oracleA.contendedIDs())
	}

	// (2) témoin : une demande ORDINAIRE de B (un auditeur) est bien poursuivie par A.
	b.mgr.RequestWithCallback(778, store.StemVocals, hash, nil)
	waitFor(t, 5*time.Second, func() bool { return a.mgr.isLeeching(778, store.StemVocals) },
		"la récupération opportuniste d'une demande ordinaire ne fonctionne plus")

	// (3) A télécharge déjà 779 (personne ne répond, la session reste ouverte) ; la demande sync de B
	// sur le même stem doit lui signaler le verrou. ⚠️ Pas 777 : la demande de B en (1) est encore
	// ouverte, une seconde demande de B serait « déjà en cours » et ne diffuserait rien.
	a.mgr.RequestSync(779, store.StemVocals, hash, nil)
	waitFor(t, 5*time.Second, func() bool { return a.mgr.isLeeching(779, store.StemVocals) }, "A ne cherche pas 779")
	b.mgr.RequestSync(779, store.StemVocals, hash, nil)
	waitFor(t, 5*time.Second, func() bool { ids := oracleA.contendedIDs(); return len(ids) == 1 && ids[0] == 779 },
		"le verrou sur 779 n'a pas été signalé à A")
}
