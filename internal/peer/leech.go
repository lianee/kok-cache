package peer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/lianee/kok-cache/internal/store"
)

// leechSession récupère UN stem depuis UN pair. Clé : "<songId>-<stem>".
type leechSession struct {
	m        *Manager
	key      string
	songID   int
	stem     string
	wantHash string

	fromPeerID string
	size       int64

	pc  *webrtc.PeerConnection
	ctx context.Context

	cancel context.CancelFunc
	once   sync.Once

	mu        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit
	buf       []byte

	received  atomic.Int64
	lastMove  atomic.Int64
	startedAt time.Time

	done func(error)
}

// ErrNoPeer : personne n'a répondu dans le délai. Ce n'est pas une panne — l'essaim peut être
// calme, ou personne ne détient encore ce stem.
var ErrNoPeer = errors.New("aucun pair n'a répondu")

// Request cherche un stem dans l'essaim et l'écrit s'il est conforme.
//
// `wantHash` DOIT venir du manifeste. C'est le hash contre lequel les octets reçus sont vérifiés
// avant tout renommage : sans référence de confiance, on écrirait sur le disque de l'utilisateur
// n'importe quels octets qu'un pair veut bien appeler « 1384-vocals ».
func (m *Manager) Request(songID int, stem, wantHash string) {
	m.request(songID, stem, wantHash, false, nil)
}

// RequestWithCallback : demande opportuniste ou ponctuelle, avec compte rendu.
func (m *Manager) RequestWithCallback(songID int, stem, wantHash string, done func(error)) {
	m.request(songID, stem, wantHash, false, done)
}

// RequestSync : demande émise par le BALAYAGE du miroir. Le `stem-avail` porte `sync`, pour que
// les autres miroirs ne la poursuivent pas (voir onAvail).
func (m *Manager) RequestSync(songID int, stem, wantHash string, done func(error)) {
	m.request(songID, stem, wantHash, true, done)
}

func (m *Manager) request(songID int, stem, wantHash string, sync bool, done func(error)) {
	if wantHash == "" || !store.ValidStem(stem) {
		if done != nil {
			done(errors.New("hash de référence inconnu"))
		}
		return
	}
	myID := m.sig.MyID()
	if myID == "" {
		if done != nil {
			done(errors.New("pas encore connecté à la signalisation"))
		}
		return
	}
	key := store.Key(songID, stem)

	m.mu.Lock()
	if _, busy := m.leeching[key]; busy {
		m.mu.Unlock()
		if done != nil {
			done(errors.New("déjà en cours"))
		}
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	l := &leechSession{
		m: m, key: key, songID: songID, stem: stem, wantHash: wantHash,
		ctx: ctx, cancel: cancel, startedAt: time.Now(), done: done,
	}
	m.leeching[key] = l
	m.mu.Unlock()

	l.touch()
	m.sig.Broadcast(wireMsg{
		Type: msgAvail, SongID: flexInt(songID), Stem: stem,
		PeerID: myID, Hash: wantHash, V: stemProtocolVersion, Sync: sync,
	})
	m.log.Debug("stem recherché", "songId", songID, "stem", stem)

	go l.watchdog()
}

// onResp : un pair annonce qu'il détient le stem. Le premier qui répond gagne — c'est ce qui fait
// fonctionner l'ordonnancement par charge de l'autre côté (un pair chargé répond plus tard).
func (m *Manager) onResp(msg wireMsg, myID string) {
	songID := msg.SongID.int()
	key := store.Key(songID, msg.Stem)

	m.mu.Lock()
	l := m.leeching[key]
	m.mu.Unlock()
	if l == nil {
		return
	}

	// Protocole v2 uniquement : sans v/size on ne sait ni demander une plage ni détecter la fin.
	if msg.V < 2 || msg.Size <= 0 {
		return
	}
	// ⛔ La taille vient d'un TIERS et pilote une allocation (`make([]byte, 0, size)`) ainsi qu'une
	// boucle d'émission. Non bornée, elle donnait à n'importe quel pair de l'essaim le moyen
	// d'éteindre cette instance avec un seul message — une panique dans un callback WebRTC n'est
	// rattrapée nulle part. On refuse, et on le DIT : se taire ici ferait ressembler une attaque à
	// un essaim calme.
	if msg.Size > maxStemSize {
		m.log.Warn("taille annoncée hors norme, pair ignoré",
			"songId", songID, "stem", msg.Stem, "de", msg.PeerID,
			"annoncé", msg.Size, "plafond", int64(maxStemSize))
		return
	}

	l.mu.Lock()
	if l.pc != nil { // un autre pair a déjà été retenu
		l.mu.Unlock()
		return
	}
	l.fromPeerID = msg.PeerID
	l.size = msg.Size
	l.mu.Unlock()

	pc, err := m.api.NewPeerConnection(m.iceConfig())
	if err != nil {
		l.finish(err)
		return
	}
	l.mu.Lock()
	l.pc = pc
	l.mu.Unlock()
	l.touch()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		m.sig.Broadcast(wireMsg{
			Type: msgICE, SongID: msg.SongID, Stem: msg.Stem,
			PeerID: myID, ToPeerID: l.fromPeerID, Candidate: &init,
		})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed {
			l.finish(errors.New("connexion échouée"))
		}
	})

	dc, err := pc.CreateDataChannel("stem", nil)
	if err != nil {
		l.finish(err)
		return
	}
	dc.OnOpen(func() {
		// ⛔ NE JAMAIS demander le fichier entier en une seule plage. Le pair qui sert IGNORE
		// SILENCIEUSEMENT toute demande dont la longueur dépasse `MAX_RANGE_REQ` (8 Mo) — aucune
		// erreur, aucune réponse — puis ferme au bout de 30 s sur son propre délai d'absence de
		// progrès. Le symptôme observé est un « abort » inexplicable, à 30 s pile.
		//
		// ⚠️ Bug RÉEL, trouvé en production le 2026-08-28 : les stems de plus de 8 Mo (chansons
		// longues : 1, 4, 8, 9 …) échouaient TOUJOURS, les autres passaient TOUJOURS. Ce
		// déterminisme est ce qui a orienté le diagnostic — une course aurait été aléatoire.
		// Les navigateurs n'ont jamais vu ce défaut parce qu'ils demandent des blocs de 512 Ko.
		for off := int64(0); off < l.size; off += rangeChunk {
			n := int64(rangeChunk)
			if off+n > l.size {
				n = l.size - off
			}
			req, _ := json.Marshal(rangeReq{O: off, L: n})
			if err := dc.SendText(string(req)); err != nil {
				l.finish(err)
				return
			}
		}
	})
	dc.OnMessage(func(raw webrtc.DataChannelMessage) {
		if raw.IsString {
			return
		}
		l.mu.Lock()
		if l.buf == nil {
			l.buf = make([]byte, 0, l.size)
		}
		l.buf = append(l.buf, raw.Data...)
		total := int64(len(l.buf))
		l.mu.Unlock()

		l.received.Store(total)
		l.touch()
		if total >= l.size {
			l.commit()
		}
	})
	dc.OnError(func(err error) { l.finish(err) })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		l.finish(err)
		return
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		l.finish(err)
		return
	}
	// Deux messages, dans cet ordre : `stem-get` fait préparer la connexion en face, puis l'offre.
	m.sig.Broadcast(wireMsg{
		Type: msgGet, SongID: msg.SongID, Stem: msg.Stem,
		PeerID: myID, ToPeerID: l.fromPeerID, V: stemProtocolVersion,
	})
	m.sig.Broadcast(wireMsg{
		Type: msgSDP, SongID: msg.SongID, Stem: msg.Stem,
		PeerID: myID, ToPeerID: l.fromPeerID, SDP: pc.LocalDescription(),
	})
	m.log.Debug("offre envoyée", "songId", songID, "stem", msg.Stem, "vers", l.fromPeerID,
		"taille", msg.Size)
}

func (l *leechSession) acceptAnswer(sdp webrtc.SessionDescription) {
	l.mu.Lock()
	pc := l.pc
	l.mu.Unlock()
	if pc == nil {
		return
	}
	if err := pc.SetRemoteDescription(sdp); err != nil {
		l.finish(err)
		return
	}
	l.mu.Lock()
	l.remoteSet = true
	pending := l.pending
	l.pending = nil
	l.mu.Unlock()
	for _, c := range pending {
		_ = pc.AddICECandidate(c)
	}
	l.touch()
}

func (l *leechSession) addICE(c webrtc.ICECandidateInit) {
	l.mu.Lock()
	if !l.remoteSet || l.pc == nil {
		l.pending = append(l.pending, c)
		l.mu.Unlock()
		return
	}
	pc := l.pc
	l.mu.Unlock()
	_ = pc.AddICECandidate(c)
}

// commit vérifie puis écrit — dans cet ordre, et c'est tout l'intérêt.
//
// `store.Commit` refait le hash lui-même et n'expose le nom définitif qu'après vérification, via
// un renommage atomique. Un fichier visible est donc toujours un fichier vérifié : on peut
// répondre à `stem-avail` pendant une synchronisation sans jamais annoncer un fichier tronqué.
func (l *leechSession) commit() {
	l.mu.Lock()
	buf := l.buf
	l.buf = nil
	l.mu.Unlock()
	if buf == nil {
		return
	}

	err := l.m.store.Commit(l.songID, l.stem, buf, l.wantHash)
	switch {
	case err == nil:
		l.m.log.Info("stem enregistré", "songId", l.songID, "stem", l.stem,
			"Ko", len(buf)/1024, "de", l.fromPeerID)
	case errors.Is(err, store.ErrHashMismatch):
		// Le pair a servi autre chose que ce que le manifeste annonce. Rien n'a été écrit, aucun
		// fichier existant n'a été touché. On ne supprime rien et on n'accuse personne : le pair
		// peut simplement détenir une version périmée.
		l.m.log.Warn("contenu reçu non conforme au manifeste, rejeté",
			"songId", l.songID, "stem", l.stem, "de", l.fromPeerID)
	default:
		l.m.log.Error("écriture impossible", "songId", l.songID, "stem", l.stem, "err", err)
	}
	l.finish(err)
}

func (l *leechSession) touch() { l.lastMove.Store(time.Now().UnixNano()) }

// watchdog borne la recherche puis le transfert.
//
// ⚠️ Écart ASSUMÉ avec le bot seeder Node : là-bas un unique délai de 30 s couvre la recherche ET
// le transfert entier, donc un stem de 8,5 Mo sur une liaison lente est coupé en pleine
// progression. Chez lui c'est marginal (il ne récupère presque jamais) ; ici la récupération est
// le mode principal. On garde donc la même philosophie que côté envoi : un délai d'ABSENCE DE
// PROGRÈS, plus un plafond dur.
func (l *leechSession) watchdog() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-tick.C:
			l.mu.Lock()
			started := l.pc != nil
			l.mu.Unlock()

			now := time.Now()
			if !started {
				if now.Sub(l.startedAt) > availTimeout {
					l.finish(ErrNoPeer)
					return
				}
				continue
			}
			if now.Sub(l.startedAt) > maxXferDuration {
				l.finish(errors.New("plafond de durée atteint"))
				return
			}
			if last := l.lastMove.Load(); last > 0 && now.Sub(time.Unix(0, last)) > xferTimeout {
				l.finish(errors.New("aucun progrès"))
				return
			}
		}
	}
}

func (l *leechSession) close(reason string) {
	l.finish(errors.New(reason))
}

func (l *leechSession) finish(err error) {
	l.once.Do(func() {
		l.cancel()
		l.m.mu.Lock()
		delete(l.m.leeching, l.key)
		l.m.mu.Unlock()

		l.mu.Lock()
		pc := l.pc
		l.buf = nil // libère jusqu'à ~8,5 Mo dès la fin de session
		l.mu.Unlock()
		if pc != nil {
			_ = pc.Close()
		}
		if l.done != nil {
			l.done(err)
		}
	})
}
