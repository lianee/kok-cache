package peer

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/lianee/kok-cache/internal/store"
)

// seedSession sert UN stem à UN pair. Clé : "<peerId>-<songId>-<stem>".
type seedSession struct {
	m      *Manager
	key    string
	peerID string
	songID int
	stem   string

	pc  *webrtc.PeerConnection
	ctx context.Context

	// ready est fermé quand `pc` est utilisable.
	//
	// ⚠️ COURSE RÉELLE, trouvée par le harnais bout-en-bout (pas par relecture) : la session est
	// publiée dans la map AVANT que la PeerConnection n'existe — il le faut, sinon l'offre SDP qui
	// suit immédiatement le `stem-get` ne trouverait aucune session et serait perdue sans
	// retransmission possible. Mais l'offre arrive alors parfois entre les deux, et l'ancienne
	// version déréférençait un `pc` nil (panique, connexion morte). Publier après création
	// déplacerait simplement le problème dans l'autre sens. Le rendez-vous règle les deux.
	ready chan struct{}

	cancel context.CancelFunc
	once   sync.Once

	mu        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit

	chunksSent atomic.Int64
	lastMove   atomic.Int64 // UnixNano du dernier progrès
	startedAt  time.Time

	reqs chan rangeReq
}

func seedKey(peerID string, songID int, stem string) string {
	return peerID + "-" + strconv.Itoa(songID) + "-" + stem
}

// onGet : un pair demande à recevoir un stem. C'est nous qui préparons la connexion ; l'offre SDP
// arrive juste après, sur le même canal, donc dans l'ordre.
func (m *Manager) onGet(msg wireMsg, myID string) {
	songID := msg.SongID.int()
	// Protocole legacy retiré : un `stem-get` sans v>=2 ne peut venir que d'un onglet resté ouvert
	// sur l'ancien code. Ignoré, aucune connexion créée.
	if msg.V < 2 || !store.ValidStem(msg.Stem) || !m.store.Has(songID, msg.Stem) {
		return
	}
	key := seedKey(msg.PeerID, songID, msg.Stem)

	m.mu.Lock()
	if _, exists := m.seeding[key]; exists {
		m.mu.Unlock()
		return
	}
	// ⛔ Plafond de cardinalité. La clé contient `peerId`, valeur fournie par le pair : sans ce
	// garde, une rafale de `stem-get` aux identifiants inédits créait autant de PeerConnections
	// (agents ICE, sockets, goroutines). Les chiens de garde bornaient la durée, jamais le nombre.
	if len(m.seeding) >= maxSeedSessions {
		m.mu.Unlock()
		m.log.Warn("trop de sessions d'envoi en cours, demande refusée",
			"actives", maxSeedSessions, "de", msg.PeerID)
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	s := &seedSession{
		m: m, key: key, peerID: msg.PeerID, songID: songID, stem: msg.Stem,
		ctx: ctx, cancel: cancel, startedAt: time.Now(),
		reqs:  make(chan rangeReq, 8),
		ready: make(chan struct{}),
	}
	m.seeding[key] = s
	m.mu.Unlock()

	pc, err := webrtc.NewPeerConnection(m.iceConfig())
	if err != nil {
		m.log.Warn("PeerConnection impossible", "err", err)
		s.close("création impossible")
		return
	}
	s.pc = pc
	s.touch()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		m.sig.Broadcast(wireMsg{
			Type: msgICE, SongID: msg.SongID, Stem: msg.Stem,
			PeerID: myID, ToPeerID: s.peerID, Candidate: &init,
		})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.close("connexion " + st.String())
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnClose(func() { s.close("datachannel fermé") })
		dc.OnError(func(err error) { s.close("datachannel en erreur : " + err.Error()) })
		dc.OnMessage(func(raw webrtc.DataChannelMessage) {
			// Le requester v2 n'attend PAS de push spontané : il demande des plages, en texte.
			if !raw.IsString {
				return
			}
			var r rangeReq
			if json.Unmarshal(raw.Data, &r) != nil {
				return
			}
			if r.O < 0 || r.L <= 0 || r.L > maxRangeReq {
				return
			}
			select {
			case s.reqs <- r:
			case <-s.ctx.Done():
			}
		})
		dc.OnOpen(func() { go s.serve(dc) })
	})

	// Dernière étape : débloquer le traitement de l'offre, qui a pu arriver entre-temps.
	close(s.ready)

	m.log.Debug("stem-get accepté", "songId", songID, "stem", msg.Stem, "de", msg.PeerID)
	go s.watchdog()
}

// serve tient le créneau d'envoi pour toute la durée de la session et sert les plages
// séquentiellement.
//
// ⚠️ FUITE DE CRÉNEAU, bug réel : en JS, une acquisition partie en file d'attente pouvait être
// réveillée pour une session déjà nettoyée, incrémentant le compteur d'un envoi que plus personne
// ne libérerait — un créneau perdu à chaque fois, jusqu'à saturation définitive et refus de tout
// service. Ici le `select` sur le contexte rend le cas impossible : une session morte n'acquiert
// rien, et ce qui est acquis est libéré par le `defer`.
func (s *seedSession) serve(dc *webrtc.DataChannel) {
	select {
	case s.m.sendSem <- struct{}{}:
	case <-s.ctx.Done():
		s.m.log.Debug("créneau annulé, session terminée avant acquisition", "key", s.key)
		return
	}
	s.m.activeSends.Add(1)
	defer func() {
		<-s.m.sendSem
		s.m.activeSends.Add(-1)
	}()

	s.m.log.Info("envoi", "songId", s.songID, "stem", s.stem, "vers", s.peerID,
		"actifs", s.m.activeSends.Load())

	// Lu une fois, à la première demande : une session sert typiquement plusieurs plages du même
	// fichier (~8,5 Mo), et le relire par morceaux coûterait plus cher que de le garder.
	var file []byte
	dc.SetBufferedAmountLowThreshold(bufferedAmountLow)
	drained := make(chan struct{}, 1)
	dc.OnBufferedAmountLow(func() {
		select {
		case drained <- struct{}{}:
		default:
		}
	})

	for {
		select {
		case <-s.ctx.Done():
			return
		case r := <-s.reqs:
			if file == nil {
				b, err := s.m.store.ReadAll(s.songID, s.stem)
				if err != nil {
					s.close("lecture impossible : " + err.Error())
					return
				}
				file = b
			}
			if !s.sendRange(dc, file, r, drained) {
				return
			}
		}
	}
}

func (s *seedSession) sendRange(dc *webrtc.DataChannel, file []byte, r rangeReq, drained chan struct{}) bool {
	end := r.O + r.L
	if end > int64(len(file)) {
		end = int64(len(file))
	}
	for off := r.O; off < end; off += chunkSize {
		if s.ctx.Err() != nil {
			return false
		}
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			s.close("datachannel plus ouvert")
			return false
		}
		if dc.BufferedAmount() > bufferedAmountLow {
			// `bufferedamountlow` n'est pas toujours fiable selon les implémentations : filet de
			// sécurité à 3 s, sans lequel la boucle d'envoi peut bloquer indéfiniment.
			select {
			case <-drained:
			case <-time.After(3 * time.Second):
			case <-s.ctx.Done():
				return false
			}
		}
		stop := off + chunkSize
		if stop > end {
			stop = end
		}
		if err := dc.Send(file[off:stop]); err != nil {
			s.close("envoi impossible : " + err.Error())
			return false
		}
		s.chunksSent.Add(1)
		s.touch()
		// Petit délai forcé entre chaque bloc — bug Firefox connu : plusieurs DATA chunks
		// regroupés dans un même paquet SCTP sont ignorés silencieusement (pas de SACK).
		select {
		case <-time.After(chunkPacingDelay):
		case <-s.ctx.Done():
			return false
		}
	}
	return true
}

func (s *seedSession) touch() { s.lastMove.Store(time.Now().UnixNano()) }

// watchdog applique les trois bornes : absence de progrès, plancher de débit, plafond dur.
func (s *seedSession) watchdog() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			now := time.Now()
			if now.Sub(s.startedAt) > maxXferDuration {
				s.close("plafond de durée atteint")
				return
			}
			if last := s.lastMove.Load(); last > 0 && now.Sub(time.Unix(0, last)) > xferTimeout {
				s.close("aucun progrès")
				return
			}
			// Plancher de débit : on ne juge un transfert QUE sur ce qu'il produit réellement,
			// après une période de grâce (ICE, montée en régime). Un mobile à 150 kbps passe ; un
			// slow-loris à un octet toutes les 29 s est éjecté.
			if elapsed := now.Sub(s.startedAt); elapsed > xferRateGrace {
				rate := float64(s.chunksSent.Load()*chunkSize) / elapsed.Seconds()
				if rate < minXferRate {
					s.close("débit sous le plancher")
					return
				}
			}
		}
	}
}

func (s *seedSession) close(reason string) {
	s.once.Do(func() {
		s.cancel()
		s.m.mu.Lock()
		delete(s.m.seeding, s.key)
		s.m.mu.Unlock()
		if s.pc != nil {
			_ = s.pc.Close()
		}
		s.m.log.Debug("session d'envoi terminée", "key", s.key, "raison", reason,
			"blocs", s.chunksSent.Load())
	})
}

// onSDP route une description : offre (nous servons) ou réponse (nous recevons).
func (m *Manager) onSDP(msg wireMsg) {
	songID := msg.SongID.int()
	switch msg.SDP.Type {
	case webrtc.SDPTypeOffer:
		m.mu.Lock()
		s := m.seeding[seedKey(msg.PeerID, songID, msg.Stem)]
		m.mu.Unlock()
		if s == nil {
			// Offre sans `stem-get` préalable : on n'a rien préparé, on ignore.
			m.droppedOffers.Add(1)
			return
		}
		s.answer(*msg.SDP, msg)
	case webrtc.SDPTypeAnswer:
		m.mu.Lock()
		l := m.leeching[store.Key(songID, msg.Stem)]
		m.mu.Unlock()
		if l == nil {
			return
		}
		l.acceptAnswer(*msg.SDP)
	}
}

func (s *seedSession) answer(offer webrtc.SessionDescription, msg wireMsg) {
	select {
	case <-s.ready:
	case <-s.ctx.Done():
		return
	case <-time.After(15 * time.Second):
		s.close("PeerConnection jamais prête")
		return
	}
	if err := s.pc.SetRemoteDescription(offer); err != nil {
		s.close("offre refusée : " + err.Error())
		return
	}
	s.mu.Lock()
	s.remoteSet = true
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, c := range pending {
		_ = s.pc.AddICECandidate(c)
	}

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		s.close("réponse impossible : " + err.Error())
		return
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		s.close("réponse non appliquée : " + err.Error())
		return
	}
	local := s.pc.LocalDescription()
	s.m.sig.Broadcast(wireMsg{
		Type: msgSDP, SongID: msg.SongID, Stem: s.stem,
		PeerID: s.m.sig.MyID(), ToPeerID: s.peerID, SDP: local,
	})
	s.touch()
}

// addICE bufferise les candidats arrivés avant la description distante : pion les refuse tant
// qu'elle n'est pas posée, et ils arrivent régulièrement en avance (trickle ICE).
func (s *seedSession) addICE(c webrtc.ICECandidateInit) {
	s.mu.Lock()
	if !s.remoteSet {
		s.pending = append(s.pending, c)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	_ = s.pc.AddICECandidate(c)
}

func (m *Manager) onICE(msg wireMsg) {
	songID := msg.SongID.int()

	m.mu.Lock()
	s := m.seeding[seedKey(msg.PeerID, songID, msg.Stem)]
	l := m.leeching[store.Key(songID, msg.Stem)]
	m.mu.Unlock()

	if s != nil {
		s.addICE(*msg.Candidate)
		return
	}
	if l != nil && l.fromPeerID == msg.PeerID {
		l.addICE(*msg.Candidate)
	}
}

func isConnected(pc *webrtc.PeerConnection) bool {
	return pc != nil && pc.ConnectionState() == webrtc.PeerConnectionStateConnected
}
