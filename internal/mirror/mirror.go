// Package mirror tient le manifeste du catalogue et, en mode miroir, comble ce qui manque.
//
// Le manifeste est l'AUTORITÉ sur la péremption d'un stem. Il ne peut pas venir de l'essaim :
// `stem-avail` est une question sur UNE chanson connue (« as-tu 1384-vocals, hash X ? ») et il
// n'existe aucun « liste-moi tout » dans le protocole — c'est très bien ainsi. Un cache qui
// démarre ne saurait donc ni quoi demander, ni quoi vérifier. Le navigateur, lui, connaît le
// catalogue parce qu'il en charge les fichiers de données ; kok-cache est AVEUGLE par conception,
// et ces fichiers sont chiffrés par une clé qui exige une session.
//
// ⭐ SAVOIR CE QU'IL FAUT CHERCHER ET SAVOIR OÙ LE PRENDRE SONT DEUX PROBLÈMES DISTINCTS : le
// manifeste répond au premier, l'essaim au second.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lianee/kok-cache/internal/api"
	"github.com/lianee/kok-cache/internal/store"
)

type manifestClient interface {
	StemManifest(ctx context.Context, knownSig string) (*api.Manifest, error)
}

type fetcher interface {
	// RequestSync : demande de balayage, marquée `sync` sur le fil (les autres miroirs ne la
	// poursuivent pas, cf. peer.onAvail).
	RequestSync(songID int, stem, wantHash string, done func(error))
	// Ready : le pair est-il en mesure de demander ? Une synchronisation qui l'ignore transforme
	// une coupure réseau de deux minutes en des milliers d'échecs instantanés.
	Ready() bool
}

// syncNetWait : durée maximale d'attente du retour du réseau au sein d'un passage. Au-delà, on
// abandonne le passage — le suivant reprendra là où le prédicat le dira.
const syncNetWait = 15 * time.Minute

// suspectCooldown borne l'effet d'un indice : un pair — ou plusieurs de mèche — ne doit pas
// pouvoir déclencher une rafale de requêtes vers le serveur en diffusant des hashes bidons.
const suspectCooldown = 5 * time.Minute

type Mirror struct {
	api    manifestClient
	store  *store.Store
	fetch  fetcher
	log    *slog.Logger
	path   string // cache disque du manifeste
	period time.Duration
	active bool // mode miroir : synchroniser tout le catalogue

	mu    sync.RWMutex
	sig   string
	stems map[int][2]string

	suspects    chan int
	lastSuspect time.Time

	// wake réveille la boucle sans attendre la prochaine échéance. ⚠️ Sans lui, activer le mode
	// miroir depuis l'interface ne produisait RIEN pendant six heures : le drapeau changeait, la
	// boucle dormait. Vu de l'utilisateur, la case à cocher ne faisait simplement rien.
	wake chan struct{}

	// failures compte les échecs par clé pour espacer les tentatives sur un stem que personne ne
	// détient — sans ça, une chanson introuvable serait redemandée en boucle à chaque passage.
	failures map[string]int

	// startOffset choisit où un passage commence dans la liste TRIÉE des stems à récupérer
	// (aléatoire par défaut, fixé par les tests). Voir syncPass pour la raison.
	startOffset func(n int) int

	// contended : un autre miroir a demandé un stem que nous téléchargions (peer.Oracle). Le
	// passage en cours repart d'un autre point à la fin de la chanson. Voir syncPass.
	contended atomic.Bool
}

func New(apiClient manifestClient, st *store.Store, f fetcher, cacheDir string, period time.Duration, active bool, log *slog.Logger) *Mirror {
	m := &Mirror{
		api: apiClient, store: st, fetch: f, log: log,
		path:        filepath.Join(cacheDir, "manifest.json"),
		period:      period,
		active:      active,
		stems:       map[int][2]string{},
		suspects:    make(chan int, 64),
		wake:        make(chan struct{}, 1),
		failures:    map[string]int{},
		startOffset: rand.IntN,
	}
	m.load()
	return m
}

// ── peer.Oracle ───────────────────────────────────────────────────────────────────────────────

// ExpectedHash renvoie le hash de référence, ou "" si le catalogue ne connaît pas ce stem.
func (m *Mirror) ExpectedHash(songID int, stem string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pair, ok := m.stems[songID]
	if !ok {
		return ""
	}
	switch stem {
	case store.StemAccompaniment:
		return pair[0]
	case store.StemVocals:
		return pair[1]
	}
	return ""
}

// Suspect enregistre un INDICE de péremption : un pair a demandé un stem avec un hash différent
// du nôtre.
//
// ⛔ Ce n'est jamais un ordre. Il ne déclenche qu'une consultation anticipée de l'autorité — au
// plus une fois par période de refroidissement. La décision de remplacer (ou, hors mode miroir,
// de supprimer, cf. dropStale) un fichier reste prise sur la foi du manifeste, jamais sur la parole
// d'un pair. Sans ce mécanisme, un fichier périmé attendrait le prochain rafraîchissement
// périodique : jamais servi (on se tait), pas encore remplacé.
func (m *Mirror) Suspect(songID int) {
	select {
	case m.suspects <- songID:
	default: // file pleine : l'indice est perdu, la synchronisation périodique le rattrapera
	}
}

// Contended : un autre miroir vient de demander (`sync`) un stem que nous téléchargeons. Nos deux
// balayages sont donc au même endroit, et vont y rester (voir syncPass) : on note, et le passage
// décrochera à la fin de la chanson en cours.
func (m *Mirror) Contended(songID int) {
	if !m.contended.Swap(true) {
		m.log.Debug("verrou avec un autre miroir", "songId", songID)
	}
}

// ── boucle ────────────────────────────────────────────────────────────────────────────────────

// Run rafraîchit le manifeste puis, en mode miroir, comble les manques.
func (m *Mirror) Run(ctx context.Context) {
	// Première vérification décalée : la signalisation doit d'abord être authentifiée, sinon les
	// requêtes partiraient dans le vide.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-m.wake:
			m.log.Info("mode miroir activé — synchronisation immédiate")
			if err := m.Refresh(ctx); err != nil {
				m.log.Warn("manifeste indisponible", "err", err)
			} else if m.Active() {
				m.syncPass(ctx)
			} else {
				m.dropStale()
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(m.period)

		case <-timer.C:
			if err := m.Refresh(ctx); err != nil {
				m.log.Warn("manifeste indisponible", "err", err)
			} else if m.Active() { // relu à chaque passage : l'interface peut le changer à chaud
				m.syncPass(ctx)
			} else {
				m.dropStale()
			}
			m.log.Info("prochaine vérification du catalogue", "dans", m.period)
			timer.Reset(m.period)

		case songID := <-m.suspects:
			m.mu.RLock()
			recent := time.Since(m.lastSuspect) < suspectCooldown
			m.mu.RUnlock()
			if recent {
				continue
			}
			m.mu.Lock()
			m.lastSuspect = time.Now()
			m.mu.Unlock()

			m.log.Debug("indice de péremption, consultation de l'autorité", "songId", songID)
			if err := m.Refresh(ctx); err != nil {
				m.log.Warn("manifeste indisponible", "err", err)
				continue
			}
			// On ne resynchronise que la chanson signalée : un indice ne justifie pas un balayage
			// complet, et l'autorité vient de trancher sur son cas.
			m.syncSong(ctx, songID)
		}
	}
}

// Refresh interroge le serveur. La signature connue est renvoyée : inchangée, la réponse fait
// 87 octets au lieu de ~400 Ko — c'est ce qui rend une vérification fréquente acceptable.
func (m *Mirror) Refresh(ctx context.Context) error {
	m.mu.RLock()
	known := m.sig
	m.mu.RUnlock()

	reqCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	mf, err := m.api.StemManifest(reqCtx, known)
	if err != nil {
		return err
	}
	if mf.Unchanged {
		m.log.Debug("manifeste inchangé", "chansons", mf.Count)
		return nil
	}
	// ⛔ UN MANIFESTE VIDE EST UNE PANNE, PAS UN CATALOGUE VIDE.
	//
	// Sans ce garde, une réponse `success:true` sans aucune entrée — bogue serveur, requête SQL
	// cassée, base en maintenance — écrasait la liste en mémoire ET le fichier local. Trois
	// conséquences, de la plus discrète à la pire :
	//   · `ExpectedHash` ne rend plus rien, donc plus aucun stem n'est récupérable (le prédicat
	//     exige un hash de référence) — le cache s'arrête, en silence ;
	//   · la sauvegarde rend l'état PERSISTANT : même un redémarrage ne le répare pas ;
	//   · `prune` voit alors TOUT le contenu comme orphelin et, confirmation donnée, efface le
	//     cache entier — des dizaines de Go.
	// L'autorité qui ne répond rien doit être traitée comme INDISPONIBLE : on garde ce qu'on avait.
	if len(mf.Stems) == 0 {
		m.log.Error("manifeste VIDE reçu du serveur — ignoré, l'ancien catalogue est conservé",
			"sig", mf.Sig, "count annoncé", mf.Count)
		return errors.New("manifeste vide : le serveur n'a annoncé aucune chanson")
	}

	m.mu.Lock()
	m.sig = mf.Sig
	m.stems = mf.Stems
	m.mu.Unlock()
	m.log.Info("manifeste à jour", "chansons", len(mf.Stems), "sig", mf.Sig)
	return m.save()
}

// Pending liste ce qui doit être synchronisé, dans l'ordre des identifiants pour que deux passages
// successifs reprennent au même endroit.
func (m *Mirror) Pending() []Item {
	m.mu.RLock()
	ids := make([]int, 0, len(m.stems))
	for id := range m.stems {
		ids = append(ids, id)
	}
	pairs := make(map[int][2]string, len(m.stems))
	for k, v := range m.stems {
		pairs[k] = v
	}
	m.mu.RUnlock()

	sort.Ints(ids)
	var out []Item
	for _, id := range ids {
		pair := pairs[id]
		for i, stem := range store.Stems {
			// LE prédicat : absent OU hash différent. « Manquant » seul ne verrait jamais un
			// fichier périmé par un re-split, et un cache qui se tait sur un stem périmé disparaît
			// silencieusement de l'essaim pour cette chanson.
			if m.store.NeedsSync(id, stem, pair[i]) {
				out = append(out, Item{
					SongID: id, Stem: stem, Hash: pair[i],
					Stale: m.store.Has(id, stem), // présent mais non conforme = périmé
				})
			}
		}
	}
	return out
}

// Item est un stem à récupérer.
type Item struct {
	SongID int
	Stem   string
	Hash   string
	// Stale distingue les deux moitiés du prédicat : le fichier est PRÉSENT mais son hash ne
	// correspond plus (re-split), par opposition à simplement absent.
	//
	// La distinction n'est pas cosmétique : elle dit si un cache subit réellement de la péremption,
	// ce qui est la seule justification du prédicat « absent OU hash différent ». Sans ce compteur,
	// on ne peut qu'en faire l'hypothèse.
	Stale bool
}

// syncPass récupère ce qui manque, en série : la synchronisation initiale (des dizaines de Go) doit être
// étalée et reprenable, pas menée au maximum de ce que la liaison encaisse. Un cache est un
// invité sur la machine de quelqu'un.
func (m *Mirror) syncPass(ctx context.Context) {
	pending := m.Pending()
	if len(pending) == 0 {
		m.log.Info("catalogue complet", "chansons", m.Count())
		return
	}
	stale := 0
	for _, it := range pending {
		if it.Stale {
			stale++
		}
	}
	m.log.Info("synchronisation", "à récupérer", len(pending),
		"absents", len(pending)-stale, "périmés", stale)

	// ⚠️ Le parcours part d'un point ALÉATOIRE de la liste triée, et boucle. Deux instances qui
	// démarraient en même temps (rotation du master des stems, 2026-09-21) traitaient les mêmes
	// chansons au même instant, dans le même ordre : le seul détenteur d'un fichier neuf étant le
	// seeder, chacune tirait TOUT le catalogue de la seedbox et aucune ne servait l'autre. Avec un
	// départ décalé, chaque instance détient vite ce qui manque à l'autre, et un pair répond avant le
	// seeder. Rotation et non mélange : le rapport d'échecs se lit encore par plages d'ids contigus
	// (une coupure réseau), ce qu'un ordre aléatoire rendrait illisible.
	//
	// ⚠️ Le décalage ne suffit pas : le verrou se REFORME. L'instance qui entre dans la région déjà
	// faite par l'autre y est servie de pair à pair, donc vite, et rattrape le curseur de l'autre ;
	// elle demande alors la chanson que l'autre télécharge (pas encore annoncée), le seeder répond
	// aux deux, à vitesses égales, écart inférieur à une chanson : état stable jusqu'à la fin. D'où
	// le décrochage : quand l'autre miroir demande ce que nous téléchargeons (Contended), on finit
	// la chanson puis on repart d'un point aléatoire du RESTE de la liste. Une collision coûte une
	// chanson en double, pas le passage.
	queue := rotated(pending, m.startOffset(len(pending)))
	m.contended.Store(false)

	done, okCount := 0, 0
	// La LISTE des échecs, pas seulement leur nombre : sans elle, un « échoués=128 » ne mène nulle
	// part — on ne sait ni lesquels, ni pourquoi, et il faut fouiller les journaux d'un pair.
	var failures []string
	for i := 0; i < len(queue); i++ {
		it := queue[i]
		if ctx.Err() != nil {
			return
		}
		// Décrochage entre deux chansons seulement : les deux stems d'une chanson restent ensemble.
		if i > 0 && it.SongID != queue[i-1].SongID && m.contended.Swap(false) {
			rest := rotated(queue[i:], m.startOffset(len(queue[i:])))
			m.log.Info("verrou avec un autre miroir : reprise à un autre point de la liste",
				"quitté", it.SongID, "repris", rest[0].SongID, "restants", len(rest))
			queue, i, it = rest, 0, rest[0]
		}
		// ⚠️ Attendre le réseau AVANT de demander. Sinon une coupure — les brèves coupures de
		// ligne nocturnes existent — fait échouer d'un coup tout le reste de la liste, en
		// quelques secondes, sans que rien ne soit tenté.
		if !m.waitReady(ctx) {
			m.log.Warn("synchronisation interrompue : réseau indisponible",
				"traités", done, "sur", len(pending))
			return
		}
		// ⛔ Le drapeau est relu À CHAQUE STEM, pas seulement entre deux passages. Un passage
		// complet dure des heures (≈ 7 h 45 sur tout le catalogue) : sans cette relecture, décocher
		// « miroir » ne produisait rien de perceptible, la synchronisation continuant jusqu'au bout.
		// ⭐ C'est le défaut SYMÉTRIQUE de celui déjà corrigé par le canal `wake` — où cocher la case
		// ne faisait rien pendant six heures. Une case à cocher doit agir dans les deux sens.
		if !m.Active() {
			m.log.Info("mode miroir désactivé — synchronisation arrêtée",
				"traités", done, "sur", len(pending))
			return
		}
		if m.skip(it) {
			continue
		}
		if done == 0 {
			// Le tout premier : sans cette ligne, l'utilisateur ne sait pas si la boucle est
			// partie ou figée — et la réponse peut demander 15 s.
			m.log.Info("première récupération lancée", "songId", it.SongID, "stem", it.Stem)
		}
		if err := m.fetchOne(ctx, it); err != nil {
			failures = append(failures, store.Key(it.SongID, it.Stem)+" ("+err.Error()+")")
			// ⚠️ Les premiers échecs remontent en AVERTISSEMENT, pas en Debug : un essaim qui ne
			// répond jamais doit se voir tout de suite. Ensuite on se tait pour ne pas noyer le
			// journal sur 5580 stems — le compte final le dira.
			if len(failures) <= 3 {
				m.log.Warn("récupération échouée", "songId", it.SongID, "stem", it.Stem, "err", err)
			}
		} else {
			okCount++
		}
		done++
		// Une synchronisation initiale dure des heures : sans jalons, elle est indiscernable d'un
		// programme figé.
		if done%25 == 0 {
			m.log.Info("synchronisation en cours", "traités", done, "sur", len(pending),
				"réussis", okCount, "échoués", len(failures))
		}
	}
	m.log.Info("passage de synchronisation terminé",
		"traités", done, "réussis", okCount, "échoués", len(failures))
	if len(failures) > 0 {
		// Borné : 5580 échecs rempliraient le journal et le rendraient inutilisable. Trente
		// suffisent à voir s'il s'agit d'ids contigus (une coupure) ou dispersés (autre chose).
		shown := failures
		if len(shown) > 30 {
			shown = shown[:30]
		}
		m.log.Warn("stems non récupérés (repris au prochain passage)",
			"total", len(failures), "liste", strings.Join(shown, ", "))
	}
}

// dropStale : hors mode miroir, un fichier PÉRIMÉ (présent, chanson toujours au catalogue, hash
// différent) est du poids mort définitif. Rien ne le remplacera : la boucle ne synchronise pas, et
// l'indice d'un pair ne vient que si quelqu'un redemande cette chanson. Après une rotation du
// master des stems (2026-09-21), c'est TOUT le cache d'un auditeur à la demande qui tombait dans
// ce cas, sans issue. On le supprime, sur la parole du manifeste : c'est l'autorité qui commande
// déjà les retéléchargements du mode miroir, et le périmètre est étroit — une chanson SORTIE du
// catalogue n'est pas concernée (`kok-cache prune`, manuel, reste seul juge), et en mode miroir
// le fichier est remplacé en place, jamais supprimé (il sert de pont aux pairs qui demandent encore
// l'ancien hash). Un manifeste PARTIEL n'a aucun effet ici : une chanson absente du manifeste n'est
// pas périmée, elle est hors sujet.
func (m *Mirror) dropStale() {
	n, bytes := 0, int64(0)
	for _, it := range m.Pending() {
		if !it.Stale {
			continue
		}
		size := m.store.Size(it.SongID, it.Stem)
		if err := m.store.Remove(it.SongID, it.Stem); err != nil {
			// Windows refuse d'effacer un fichier ouvert (envoi en cours) : on réessaiera au
			// prochain rafraîchissement, il n'y a rien d'autre à faire.
			m.log.Debug("suppression impossible", "songId", it.SongID, "stem", it.Stem, "err", err)
			continue
		}
		n++
		bytes += size
	}
	if n > 0 {
		m.log.Info("fichiers périmés supprimés (mode à la demande : rien ne les aurait remplacés)",
			"fichiers", n, "Mo", bytes>>20)
	}
}

// rotated renvoie la liste commencée à l'indice k, puis reprise du début. k hors bornes = liste
// intacte : un choix de départ ne doit jamais faire perdre un stem.
func rotated(items []Item, k int) []Item {
	if k <= 0 || k >= len(items) {
		return items
	}
	out := make([]Item, 0, len(items))
	out = append(out, items[k:]...)
	return append(out, items[:k]...)
}

// waitReady patiente tant que le pair ne peut pas émettre. Renvoie false si le contexte est
// annulé ou si l'attente dépasse la limite.
func (m *Mirror) waitReady(ctx context.Context) bool {
	if m.fetch == nil || m.fetch.Ready() {
		return true
	}
	m.log.Warn("réseau indisponible — synchronisation en pause")
	deadline := time.Now().Add(syncNetWait)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
		if m.fetch.Ready() {
			m.log.Info("réseau revenu — reprise de la synchronisation")
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func (m *Mirror) syncSong(ctx context.Context, songID int) {
	m.mu.RLock()
	pair, ok := m.stems[songID]
	m.mu.RUnlock()
	if !ok {
		// La chanson a disparu du catalogue. ⛔ On ne supprime rien : le fichier local reste, et
		// c'est `kok-cache prune`, lancé par l'utilisateur, qui décide de libérer la place.
		return
	}
	for i, stem := range store.Stems {
		if m.store.NeedsSync(songID, stem, pair[i]) {
			if err := m.fetchOne(ctx, Item{SongID: songID, Stem: stem, Hash: pair[i]}); err != nil {
				m.log.Debug("remplacement impossible", "songId", songID, "stem", stem, "err", err)
			}
		}
	}
}

func (m *Mirror) fetchOne(ctx context.Context, it Item) error {
	done := make(chan error, 1)
	m.fetch.RequestSync(it.SongID, it.Stem, it.Hash, func(err error) { done <- err })

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		key := store.Key(it.SongID, it.Stem)
		m.mu.Lock()
		if err != nil {
			m.failures[key]++
		} else {
			delete(m.failures, key)
		}
		m.mu.Unlock()
		return err
	case <-time.After(20 * time.Minute):
		// Filet : la session a ses propres bornes, mais la boucle ne doit jamais rester suspendue
		// à un callback qui n'arrive pas.
		m.log.Warn("récupération sans réponse", "songId", it.SongID, "stem", it.Stem)
		return errors.New("aucune réponse")
	}
}

// skip espace les tentatives sur un stem que personne ne détient : après 3 échecs, on ne le
// redemande qu'un passage sur quatre.
func (m *Mirror) skip(it Item) bool {
	key := store.Key(it.SongID, it.Stem)
	m.mu.RLock()
	n := m.failures[key]
	m.mu.RUnlock()
	return n >= 3 && n%4 != 0
}

// SetActive active ou coupe le mode miroir SANS redémarrage : la boucle relit le drapeau à chaque
// passage. Activer déclenche une synchronisation au prochain réveil, couper laisse simplement le
// cache se remplir de ce qui est écouté.
func (m *Mirror) SetActive(on bool) {
	m.mu.Lock()
	changed := m.active != on
	m.active = on
	m.mu.Unlock()

	// Activation = action explicite de l'utilisateur : elle doit se voir tout de suite.
	if on && changed {
		select {
		case m.wake <- struct{}{}:
		default: // un réveil est déjà en attente, inutile d'en empiler un second
		}
	}
}

// Active dit si le mode miroir est en cours.
func (m *Mirror) Active() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// Count est le nombre de chansons connues du manifeste.
func (m *Mirror) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.stems)
}

// Sig est la signature du manifeste détenu.
func (m *Mirror) Sig() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sig
}

// ── persistance ───────────────────────────────────────────────────────────────────────────────

type manifestFile struct {
	Sig   string               `json:"sig"`
	Stems map[string][2]string `json:"stems"`
}

// load relit le manifeste du dernier lancement. Sans lui, un démarrage hors ligne ne connaîtrait
// aucun hash de référence : on ne pourrait ni servir en confiance, ni écrire quoi que ce soit.
func (m *Mirror) load() {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var f manifestFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	stems := make(map[int][2]string, len(f.Stems))
	for k, v := range f.Stems {
		id, err := strconv.Atoi(k)
		if err != nil || id <= 0 {
			continue
		}
		stems[id] = v
	}
	m.mu.Lock()
	m.sig, m.stems = f.Sig, stems
	m.mu.Unlock()
	m.log.Debug("manifeste local chargé", "chansons", len(stems))
}

func (m *Mirror) save() error {
	m.mu.RLock()
	f := manifestFile{Sig: m.sig, Stems: make(map[string][2]string, len(m.stems))}
	for k, v := range m.stems {
		f.Stems[strconv.Itoa(k)] = v
	}
	m.mu.RUnlock()

	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
