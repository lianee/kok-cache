// Package peer implémente le pair WebRTC de kok-cache : il sert les stems qu'il détient et
// récupère ceux qui lui manquent, exactement dans le protocole des deux implémentations
// existantes (bot seeder Node et copie inlinée dans global.js).
//
// ⚠️ CE PROTOCOLE EXISTE DÉSORMAIS EN TROIS IMPLÉMENTATIONS. Toute correction de logique
// (créneaux, plancher de débit, blocs v2, délais) doit être reportée dans les trois :
//
//	seeder/seeder.js        · bot Node de la seedbox
//	web/extracted/global.js · copie inlinée, tout navigateur est un pair
//	CE paquet               · kok-cache
//
// Coût réel d'un oubli, mesuré en août 2026 : une fuite de créneaux corrigée d'un seul côté a
// laissé les pairs navigateur muets après chaque rafale, et il a fallu ~43 h d'observation et
// trois hypothèses réfutées pour revenir à un bug déjà connu.
//
// ⛔ INVARIANT AVEUGLE : ce paquet ne manipule que du ciphertext. Il ne demande jamais de clé, n'en
// stocke aucune et ne déchiffre rien. C'est ce qui rend le binaire inoffensif s'il est décompilé
// et son dossier copié — et c'est aussi ce qui empêche kok-cache de devenir l'outil de moissonnage
// idéal.
package peer

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/lianee/kok-cache/internal/signal"
	"github.com/lianee/kok-cache/internal/store"
)

// Constantes reprises telles quelles du seeder Node — chacune a été payée par un bug réel, les
// commentaires disent lequel.
const (
	// 16 Ko : taille historiquement sûre pour un message DataChannel. 64 Ko fonctionnait sur
	// Chrome mais déclenchait un bug d'interop SCTP avec Firefox — transfert qui « connecte » puis
	// ne remonte jamais côté client alors que l'émetteur envoie tout avec succès.
	chunkSize = 16384

	// Seuil de contrôle de flux du DataChannel.
	bufferedAmountLow = 262144

	// Garde-fou : longueur maximale d'une demande de plage. ⛔ Une demande plus longue est ignorée
	// EN SILENCE par le pair qui sert — pas d'erreur, pas de réponse.
	maxRangeReq = 8 << 20

	// rangeChunk : taille des plages demandées, moitié du plafond ci-dessus.
	//
	// ⚠️ La marge n'est pas de la coquetterie : demander exactement `maxRangeReq` marcherait, mais
	// la moindre évolution du plafond chez un pair rendrait tous les gros stems inaccessibles, en
	// silence et sans erreur. Le navigateur, lui, demande 512 Ko — c'est pour cela qu'il n'a
	// jamais rencontré ce défaut.
	rangeChunk = 4 << 20

	// Pas de PROGRÈS depuis ce délai → abandon. C'est un timeout d'absence de progrès, pas une
	// durée totale : un timer fixe tuait des transferts encore activement en cours, ralentis par
	// le partage de charge, toujours au même point.
	xferTimeout = 30 * time.Second

	// Filet ultime : un transfert qui progresse juste assez pour repousser le timer sans jamais
	// aboutir tiendrait sinon indéfiniment (un test de stress est resté bloqué 42 min).
	maxXferDuration = 15 * time.Minute

	// Plancher de débit — 30 kbps. En dessous c'est du slow-loris, pas un mobile lent. L'ancien
	// plafond en horloge murale faisait exactement l'inverse : il coupait les connexions honnêtes
	// et lentes (un stem pèse ~8,5 Mo, 180 s de plafond exigeaient 380 kbps SOUTENUS) tout en
	// laissant survivre un octet toutes les 29 secondes.
	minXferRate      = 3750 // octets/s
	xferRateGrace    = 60 * time.Second
	chunkPacingDelay = 2 * time.Millisecond // bug Firefox SCTP : plusieurs DATA chunks dans un
	// même paquet SCTP sont ignorés silencieusement (pas de SACK).
	// cf. bugzilla.mozilla.org/show_bug.cgi?id=1230965

	// Répartition de charge : un pair qui sert plusieurs transferts EN MÊME TEMPS échoue
	// massivement (~65 % d'échec à 5+ envois concurrents), alors que plusieurs pairs servant
	// chacun un transfert réussissent à 100 %. Plutôt que de faire attendre un client déjà engagé,
	// on RETARDE la réponse proportionnellement à la charge : un pair moins chargé répond plus
	// vite et gagne la sélection (premier arrivé, premier choisi côté demandeur).
	responseDelayPerLoad = 600 * time.Millisecond
	// ⚠️ Borne indispensable : le demandeur n'attend un `stem-resp` que 6 000 ms. Sans plafond,
	// 11 envois actifs donneraient 6 600 ms de délai — on répondrait APRÈS son abandon, donc on
	// deviendrait invisible exactement quand on est le plus sollicité.
	maxResponseDelay = 2500 * time.Millisecond

	maxLoadToRespond   = 12 // au-delà, on ne répond plus du tout à stem-avail
	maxConcurrentSends = 12 // filet de sécurité au moment du send réel

	// ⛔ Plafond de la taille ANNONCÉE par un pair dans `stem-resp`.
	//
	// Sans lui, la valeur d'un tiers pilotait directement une allocation et une boucle d'émission :
	// `size` énorme → soit des milliards de demandes de plage, soit `make([]byte, 0, size)` qui
	// PANIQUE dans un callback WebRTC, et une panique non rattrapée tue le processus. Un pair
	// pouvait donc éteindre à distance n'importe quelle instance de l'essaim, avec un message.
	//
	// L'asymétrie qui l'avait laissé passer : on bornait ce qu'on nous DEMANDE (`maxRangeReq`,
	// seed.go), pas ce qu'on nous ANNONCE.
	//
	// 64 Mo est délibérément très large — un stem pèse 6 à 11 Mo — parce que ce plafond n'est pas
	// un réglage de performance : il ne sert qu'à rendre l'abus inoffensif. Le vrai contrôle de ce
	// qu'on écrit reste le hash du manifeste.
	maxStemSize = 64 << 20

	// Plafonds de CARDINALITÉ. Les chiens de garde bornaient la durée d'une session (~30 s), jamais
	// leur nombre — or les clés de `seeding` et `inboxes` contiennent `peerId`, une valeur fournie
	// par le pair et jamais vérifiée. Chaque identifiant inédit créait donc une PeerConnection
	// (agent ICE, sockets) ou une goroutine de plus, sans limite.
	// ⚠️ `maxConcurrentSends` ne protégeait pas : il n'est acquis qu'à l'envoi réel, bien après
	// l'allocation.
	maxSeedSessions = 32
	maxInboxes      = 64

	// Côté demandeur : délai d'attente d'un `stem-resp` avant d'abandonner la recherche.
	availTimeout = 15 * time.Second
)

// Oracle est l'autorité sur les hashes : le manifeste du serveur, jamais un pair.
type Oracle interface {
	// ExpectedHash renvoie le hash de référence d'un stem, ou "" si le catalogue ne le connaît
	// pas. Un stem sans hash de référence n'est jamais écrit sur le disque.
	ExpectedHash(songID int, stem string) string
	// Suspect signale qu'un pair a demandé ce stem avec un hash différent du nôtre. C'est un
	// INDICE, jamais un ordre : il ne peut que déclencher une consultation anticipée de
	// l'autorité, jamais une suppression.
	Suspect(songID int)
	// Contended signale qu'un AUTRE miroir vient de demander (`sync`) un stem que nous sommes en
	// train de télécharger : les deux balayages sont en verrou sur la même région. Le miroir en
	// tire la décision de repartir d'ailleurs à la fin de la chanson en cours.
	Contended(songID int)
}

type Manager struct {
	store  *store.Store
	sig    *signal.Client
	oracle Oracle
	log    *slog.Logger

	// sendSem borne les envois simultanés. Le compteur `activeSends` en est le miroir observable,
	// utilisé pour le délai de réponse et le seuil de silence.
	sendSem     chan struct{}
	activeSends atomic.Int64

	// droppedOffers compte les offres SDP arrivées sans session préparée. Compteur de diagnostic,
	// pas de statistique : il doit rester à zéro, et le harnais l'affiche quand un transfert
	// échoue.
	droppedOffers atomic.Int64

	iceMu sync.RWMutex
	ice   []webrtc.ICEServer

	mu       sync.Mutex
	seeding  map[string]*seedSession  // clé "<peerId>-<songId>-<stem>"
	leeching map[string]*leechSession // clé "<songId>-<stem>"
	inboxes  map[string]chan wireMsg  // une file ordonnée par pair émetteur

	ctx context.Context
}

func NewManager(ctx context.Context, st *store.Store, oracle Oracle, log *slog.Logger) *Manager {
	return &Manager{
		store:    st,
		oracle:   oracle,
		log:      log,
		sendSem:  make(chan struct{}, maxConcurrentSends),
		seeding:  make(map[string]*seedSession),
		leeching: make(map[string]*leechSession),
		inboxes:  make(map[string]chan wireMsg),
		ctx:      ctx,
		ice: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
}

// Attach relie le manager au client de signalisation. Séparé du constructeur parce que les deux
// se référencent mutuellement.
func (m *Manager) Attach(sig *signal.Client) { m.sig = sig }

// Ready dit si ce pair peut réellement demander quelque chose : socket authentifié et identité
// connue. Sans ce garde, une coupure de réseau ne ralentit pas la synchronisation — elle la fait
// ÉCHOUER À PLEINE VITESSE, chaque demande rendant la main aussitôt.
func (m *Manager) Ready() bool {
	return m.sig != nil && m.sig.Authenticated() && m.sig.MyID() != ""
}

// SetOracle installe l'autorité des hashes. Même raison : le manifeste a besoin du manager pour
// récupérer, le manager a besoin du manifeste pour savoir quoi accepter.
//
// ⚠️ Doit être appelé avant que la signalisation ne démarre : sans oracle, aucun stem n'a de hash
// de référence, donc rien ne serait jamais écrit — une panne silencieuse de plus.
func (m *Manager) SetOracle(o Oracle) { m.oracle = o }

// ── signal.Handler ────────────────────────────────────────────────────────────────────────────

func (m *Manager) OnConnected(myID string) {
	m.log.Info("pair en ligne", "peerId", myID)
}

// OnDisconnected ne tue QUE les sessions pas encore établies.
//
// ⛔ Ne pas généraliser : une fois le datachannel ouvert (ICE et DTLS négociés), un transfert n'a
// plus besoin de la signalisation pour continuer. Le seeder Node tuait systématiquement tout
// transfert en cours à chaque cycle de reconnexion (~5 s en période instable) — un transfert de
// plus de cinq secondes ne survivait alors que par chance.
func (m *Manager) OnDisconnected() {
	m.mu.Lock()
	seeds := make([]*seedSession, 0, len(m.seeding))
	for _, s := range m.seeding {
		seeds = append(seeds, s)
	}
	leeches := make([]*leechSession, 0, len(m.leeching))
	for _, l := range m.leeching {
		leeches = append(leeches, l)
	}
	m.mu.Unlock()

	for _, s := range seeds {
		if !isConnected(s.pc) {
			s.close("signalisation perdue avant établissement")
		}
	}
	for _, l := range leeches {
		if !isConnected(l.pc) {
			l.close("signalisation perdue avant établissement")
		}
	}
}

func (m *Manager) OnTurnCreds(creds signal.TurnCreds) {
	if len(creds.URLs) == 0 {
		return
	}
	m.iceMu.Lock()
	m.ice = []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
		{URLs: creds.URLs, Username: creds.Username, Credential: creds.Credential,
			CredentialType: webrtc.ICECredentialTypePassword},
	}
	m.iceMu.Unlock()
	m.log.Info("credentials TURN reçus", "urls", len(creds.URLs))
}

func (m *Manager) iceConfig() webrtc.Configuration {
	m.iceMu.RLock()
	defer m.iceMu.RUnlock()
	servers := make([]webrtc.ICEServer, len(m.ice))
	copy(servers, m.ice)
	return webrtc.Configuration{ICEServers: servers}
}

// OnBroadcast route un message du maillage. Appelé depuis la goroutine de lecture : tout travail
// long part en goroutine.
func (m *Manager) OnBroadcast(raw json.RawMessage) {
	var msg wireMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	myID := m.sig.MyID()
	if myID == "" || msg.PeerID == "" || msg.PeerID == myID {
		return
	}
	if msg.SongID.int() <= 0 {
		return
	}

	switch msg.Type {
	case msgAvail:
		// Seul cas traité hors file : la réponse est volontairement RETARDÉE selon la charge
		// (jusqu'à 2,5 s), ce qui bloquerait tout le reste du trafic de ce pair.
		if store.ValidStem(msg.Stem) {
			go m.onAvail(msg, myID)
		}
	case msgResp, msgGet, msgSDP, msgICE:
		if msg.ToPeerID == myID {
			m.enqueue(msg, myID)
		}
	case msgDelete:
		// ⛔ VOLONTAIREMENT IGNORÉ. Le bot seeder supprime bien les fichiers d'une chanson sur ce
		// message, mais il tourne sur une machine de l'opérateur. Ici, l'obéir donnerait à
		// n'importe quel pair une primitive d'effacement à distance sur les disques d'inconnus.
		// Une chanson retirée du catalogue disparaît du manifeste ; c'est `kok-cache prune`, une
		// action de l'utilisateur, qui décide alors de libérer la place.
		m.log.Debug("stem-delete ignoré (l'autorité est le manifeste, pas un pair)", "songId", msg.SongID.int())
	}
}

// ── état observable ───────────────────────────────────────────────────────────────────────────

// Activity décrit un transfert en cours, pour l'interface. Instantané : rien n'est conservé, une
// session terminée disparaît de la liste.
type Activity struct {
	Direction string    `json:"direction"` // "envoi" ou "réception"
	SongID    int       `json:"songId"`
	Stem      string    `json:"stem"`
	Peer      string    `json:"peer"`
	Bytes     int64     `json:"bytes"`
	Total     int64     `json:"total"`
	Since     time.Time `json:"since"`
}

// Snapshot liste les transferts en cours, envois d'abord.
func (m *Manager) Snapshot() []Activity {
	m.mu.Lock()
	seeds := make([]*seedSession, 0, len(m.seeding))
	for _, s := range m.seeding {
		seeds = append(seeds, s)
	}
	leeches := make([]*leechSession, 0, len(m.leeching))
	for _, l := range m.leeching {
		leeches = append(leeches, l)
	}
	m.mu.Unlock()

	out := make([]Activity, 0, len(seeds)+len(leeches))
	for _, s := range seeds {
		out = append(out, Activity{
			Direction: "envoi", SongID: s.songID, Stem: s.stem, Peer: s.peerID,
			Bytes: s.chunksSent.Load() * chunkSize,
			Total: m.store.Size(s.songID, s.stem),
			Since: s.startedAt,
		})
	}
	for _, l := range leeches {
		l.mu.Lock()
		total := l.size
		l.mu.Unlock()
		out = append(out, Activity{
			Direction: "réception", SongID: l.songID, Stem: l.stem, Peer: l.fromPeerID,
			Bytes: l.received.Load(), Total: total, Since: l.startedAt,
		})
	}
	return out
}

// ── ordre des messages ────────────────────────────────────────────────────────────────────────

// inboxIdle : une file inactive est démontée, sinon chaque pair croisé laisserait une goroutine
// derrière lui.
const inboxIdle = 2 * time.Minute

// enqueue met le message dans la file de SON pair émetteur.
//
// ⚠️ COURSE RÉELLE, prouvée par le harnais avec un compteur (`droppedOffers`) et non par
// relecture : `stem-get` et l'offre SDP qui le suit immédiatement étaient traités dans deux
// goroutines concurrentes. Une fois sur deux environ, l'offre passait devant et ne trouvait
// aucune session préparée : elle était jetée, plus rien n'arrivait, et le transfert mourait
// 30 secondes plus tard sur « aucun progrès » — un symptôme qui ne désigne pas du tout sa cause.
//
// Les deux implémentations existantes n'ont pas ce problème parce que JavaScript traite un
// message à la fois : l'ordre du fil y est gratuit. Ici il faut le rétablir explicitement. Une
// file PAR PAIR conserve le parallélisme entre pairs, qui est celui qui compte.
func (m *Manager) enqueue(msg wireMsg, myID string) {
	m.mu.Lock()
	q, ok := m.inboxes[msg.PeerID]
	if !ok {
		// ⛔ Plafond de cardinalité : une file et une goroutine par expéditeur, et `peerId` est une
		// valeur fournie par le pair. Sans ce garde, une rafale d'identifiants inédits en créait
		// autant. Le nettoyage par inactivité existait, mais il n'agit qu'APRÈS coup.
		if len(m.inboxes) >= maxInboxes {
			m.mu.Unlock()
			m.log.Warn("trop de pairs actifs, message ignoré", "pair", msg.PeerID, "type", msg.Type)
			return
		}
		q = make(chan wireMsg, 64)
		m.inboxes[msg.PeerID] = q
		go m.drain(msg.PeerID, q)
	}
	m.mu.Unlock()

	select {
	case q <- msg:
	default:
		// File saturée : on jette plutôt que de bloquer la goroutine de lecture du fil, qui sert
		// TOUS les pairs. Le demandeur réessaiera.
		m.log.Warn("file d'un pair saturée, message ignoré", "pair", msg.PeerID, "type", msg.Type)
	}
}

func (m *Manager) drain(peerID string, q chan wireMsg) {
	idle := time.NewTimer(inboxIdle)
	defer idle.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return

		case msg := <-q:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(inboxIdle)

			// Relu à chaque message : une reconnexion de la signalisation change notre
			// identifiant, et une file survit à la reconnexion.
			myID := m.sig.MyID()
			if myID == "" {
				continue
			}

			switch msg.Type {
			case msgResp:
				m.onResp(msg, myID)
			case msgGet:
				m.onGet(msg, myID)
			case msgSDP:
				if msg.SDP != nil {
					m.onSDP(msg)
				}
			case msgICE:
				if msg.Candidate != nil {
					m.onICE(msg)
				}
			}

		case <-idle.C:
			m.mu.Lock()
			// Re-vérification sous verrou : un message a pu arriver entre l'expiration et ici.
			if len(q) == 0 && m.inboxes[peerID] == q {
				delete(m.inboxes, peerID)
				m.mu.Unlock()
				return
			}
			m.mu.Unlock()
			idle.Reset(inboxIdle)
		}
	}
}

// ── stem-avail : quelqu'un cherche un stem ────────────────────────────────────────────────────

func (m *Manager) onAvail(msg wireMsg, myID string) {
	songID := msg.SongID.int()

	if m.store.Has(songID, msg.Stem) {
		// Désaccord de hash → SILENCE. Jamais de suppression : le hash attendu vient du
		// DEMANDEUR, et un désaccord n'implique même pas que notre fichier soit périmé (un
		// navigateur au catalogue daté demande avec l'ancien hash — c'est LUI qui a tort).
		if !m.store.Serves(songID, msg.Stem, msg.Hash) {
			m.log.Debug("hash différent, on se tait", "songId", songID, "stem", msg.Stem)
			// L'indice, lui, est exploitable : il fait avancer la consultation de l'AUTORITÉ.
			// Sans ça, un fichier périmé serait du poids mort permanent — jamais servi, jamais
			// remplacé, jusqu'à la prochaine synchronisation complète.
			m.oracle.Suspect(songID)
			return
		}

		load := m.activeSends.Load()
		if load >= maxLoadToRespond {
			m.log.Debug("charge maximale, on ne répond pas", "actifs", load)
			return
		}
		delay := time.Duration(load) * responseDelayPerLoad
		if delay > maxResponseDelay {
			delay = maxResponseDelay
		}
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(delay):
		}
		size := m.store.Size(songID, msg.Stem)
		if size <= 0 {
			return
		}
		m.sig.Broadcast(wireMsg{
			Type: msgResp, SongID: msg.SongID, Stem: msg.Stem,
			PeerID: myID, ToPeerID: msg.PeerID,
			V: stemProtocolVersion, Size: size,
		})
		m.log.Debug("stem-resp envoyé", "songId", songID, "stem", msg.Stem, "vers", msg.PeerID, "délai", delay)
		return
	}

	// Demande de BALAYAGE d'un autre miroir : on ne la poursuit pas. Il l'obtiendra du seeder, et
	// notre propre balayage la trouvera chez lui le moment venu. La poursuivre, c'était télécharger
	// chaque chanson de l'autre au même instant que lui — deux fois depuis le seeder, jamais de pair
	// à pair (constaté le 2026-09-21 avec deux miroirs). Si nous la téléchargeons déjà nous-mêmes,
	// c'est que nos deux curseurs sont au même endroit : on le dit au miroir, qui décrochera.
	if msg.Sync {
		key := store.Key(songID, msg.Stem)
		m.mu.Lock()
		_, leeching := m.leeching[key]
		m.mu.Unlock()
		if leeching {
			m.oracle.Contended(songID)
		}
		return
	}

	// On ne l'a pas : on le cherche à notre tour, pour que le prochain demandeur le trouve ici.
	// ⚠️ Le hash de référence vient du MANIFESTE, jamais du message : accepter le hash d'un pair
	// reviendrait à laisser n'importe qui décider du contenu du cache de l'utilisateur.
	want := m.oracle.ExpectedHash(songID, msg.Stem)
	if want == "" {
		return
	}
	m.Request(songID, msg.Stem, want)
}
