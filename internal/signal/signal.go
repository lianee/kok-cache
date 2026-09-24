// Package signal parle au serveur de signalisation dédié `kok-rt`.
//
// Le fil est du Primus BRUT (trames JSON sur un WebSocket), pas la bibliothèque cliente Primus :
// c'est déjà ainsi que le bot seeder s'y connecte, et cela évite d'embarquer un moteur JavaScript.
// Deux détails de forme sont à respecter tels quels :
//
//  1. Le battement de cœur est une CHAÎNE JSON — `"primus::ping::<ts>"` — pas un objet ; il faut
//     répondre `"primus::pong::<ts>"`, sinon le serveur ferme la connexion.
//  2. `{hello: <id>}` arrive AVANT l'authentification : c'est ainsi qu'un pair apprend son PROPRE
//     identifiant, indispensable à l'adressage dirigé (`toPeerId`).
//
// ⛔ Depuis le 2026-08-28, plus rien n'aboutit sans ticket signé. Un ticket refusé donne une panne
// SILENCIEUSE par excellence : on reste connecté mais on ne sert plus un seul bloc, indiscernable
// d'un essaim calme. D'où le renouvellement automatique et un journal bruyant en cas d'échec.
package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lianee/kok-cache/internal/api"
)

const (
	// maxMessageSize borne ce qu'on accepte de lire du fil. Voir `session()` : le défaut de
	// gorilla est « aucune limite ».
	maxMessageSize = 1 << 20

	// readTimeout : délai maximal SANS AUCUN message, battements de cœur compris. Primus émet un
	// `primus::ping::` bien plus souvent ; cette marge est là pour ne jamais couper une connexion
	// saine, tout en détectant en minutes — et non en heures — une connexion semi-ouverte.
	readTimeout = 2 * time.Minute
)

// TurnCreds : credentials de relais de courte durée, distribués par le serveur sur une connexion
// authentifiée. ⚠️ TURN est un RELAIS, pas un outil de connexion (c'est STUN, l'outil de
// connexion) : sur une paire de candidats `relay`, les octets transitent physiquement par le
// serveur de l'opérateur.
type TurnCreds struct {
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	URLs       []string `json:"urls"`
}

// Handler reçoit les événements du fil. Toutes les méthodes sont appelées depuis la goroutine de
// lecture : elles doivent rendre la main vite et déporter tout travail long.
type Handler interface {
	OnConnected(myID string)
	OnDisconnected()
	OnBroadcast(data json.RawMessage)
	OnTurnCreds(creds TurnCreds)
}

type ticketSource interface {
	RTTicket(ctx context.Context, instance string) (api.Ticket, error)
}

type Client struct {
	url      string
	instance string
	tickets  ticketSource
	handler  Handler
	log      *slog.Logger
	retry    time.Duration

	// writeMu sérialise les écritures : gorilla n'autorise qu'un écrivain à la fois, et il est
	// distinct de `mu` pour qu'un envoi ne bloque pas la lecture de l'état.
	writeMu sync.Mutex

	mu     sync.Mutex
	conn   *websocket.Conn
	myID   string
	authed bool
	ticket api.Ticket
	stats  StatsProvider
	// authErr : dernier refus d'obtention de ticket, en clair. Il doit REMONTER à l'interface —
	// un helper qui ne sert rien parce que l'abonnement a expiré ne doit pas ressembler à une panne.
	authErr string
	// retried : un refus n'entraîne qu'UN renouvellement forcé par connexion. Sans ce garde-fou on
	// rejouerait indéfiniment un ticket que le serveur vient de refuser.
	retried bool
}

func New(url, instance string, tickets ticketSource, handler Handler, retry time.Duration, log *slog.Logger) *Client {
	if retry <= 0 {
		retry = 5 * time.Second
	}
	return &Client{url: url, instance: instance, tickets: tickets, handler: handler, log: log, retry: retry}
}

// MyID est l'identifiant de spark, adresse de ce pair dans l'essaim. Vide tant que la connexion
// n'a pas dit `hello`.
func (c *Client) MyID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.myID
}

// AuthError renvoie la raison du dernier refus, vide si tout va bien.
func (c *Client) AuthError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authErr
}

func (c *Client) Authenticated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authed
}

// Run maintient la connexion jusqu'à annulation du contexte.
func (c *Client) Run(ctx context.Context) {
	for {
		if err := c.session(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("signalisation déconnectée", "err", err, "retry", c.retry)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.retry):
		}
	}
}

func (c *Client) session(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	conn, _, err := dialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return err
	}

	// ⛔ Sans SetReadLimit, gorilla ne borne RIEN : sa vérification est `if c.readLimit > 0 && …`,
	// donc le défaut (0) accepte un message de taille arbitraire et l'alloue entièrement. Or
	// `stem_broadcast` relaie des messages VENUS D'AUTRES PAIRS : une charge utile énorme émise par
	// un seul pair était allouée chez tous les destinataires.
	// 1 Mo est très large pour ce protocole — le plus gros message réaliste est une offre SDP
	// chargée de candidats, quelques dizaines de Ko.
	conn.SetReadLimit(maxMessageSize)
	// ⛔ Et un délai de LECTURE, absent jusqu'ici alors que l'écriture en avait un. Sans lui, une
	// connexion semi-ouverte (le serveur cesse d'émettre sans fermer proprement) laisse
	// `ReadMessage` bloqué pour toujours : aucune erreur, donc aucune reconnexion, et kok-cache
	// reste « connecté » sans plus rien servir. C'est la panne muette que tout le reste du projet
	// s'applique à rendre impossible.
	// ⚠️ Le délai est repoussé à CHAQUE message reçu, battements de cœur compris — c'est eux qui
	// garantissent du trafic sur une connexion inactive. La marge est volontairement large : un
	// plafond trop court couperait des connexions saines si le serveur espaçait ses pings.
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

	c.mu.Lock()
	c.conn, c.myID, c.authed, c.retried = conn, "", false, false
	c.mu.Unlock()

	defer func() {
		conn.Close()
		c.mu.Lock()
		c.conn, c.myID, c.authed = nil, "", false
		c.mu.Unlock()
		c.handler.OnDisconnected()
	}()

	// Le contexte annulé doit débloquer la lecture, qui n'en tient pas compte d'elle-même.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		// Tout message reçu — y compris un battement de cœur — prouve que le fil est vivant.
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		c.dispatch(ctx, raw)
	}
}

// envelope regroupe tous les champs que le serveur peut envoyer. Un seul décodage suffit : ces
// messages sont petits et fréquents (des dizaines de candidats ICE par négociation).
type envelope struct {
	Hello string          `json:"hello"`
	Cmd   string          `json:"cmd"`
	Role  string          `json:"role"`
	UID   int             `json:"uid"`
	Data  json.RawMessage `json:"data"`
	// turn_creds arrive à plat dans l'enveloppe, pas dans un sous-objet.
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	URLs       []string `json:"urls"`
}

func (c *Client) dispatch(ctx context.Context, raw []byte) {
	// Battement de cœur : une CHAÎNE JSON, donc `"primus::ping::…"` avec les guillemets.
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && len(s) > 14 && s[:14] == "primus::ping::" {
			c.writeRaw("primus::pong::" + s[14:])
		}
		return
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}

	switch {
	case env.Hello != "":
		c.mu.Lock()
		c.myID = env.Hello
		c.mu.Unlock()
		c.log.Debug("identifiant de pair reçu", "id", env.Hello)
		go c.authenticate(ctx, false)

	case env.Cmd == "auth_ok":
		c.mu.Lock()
		c.authed = true
		c.authErr = ""
		myID := c.myID
		c.mu.Unlock()
		c.log.Info("authentifié auprès de la signalisation", "role", env.Role, "uid", env.UID)
		// Déclaration de CAPACITÉ (« je peux servir des blocs »), pas d'identité — celle-ci vient
		// du ticket. `rt.ident` existe encore côté serveur mais y est ignoré : ne pas l'envoyer.
		c.Send(map[string]any{"rt": map[string]any{"stem_peer": true}})
		c.Send(map[string]any{"cmd": "get_turn_creds"})
		c.sendStats()
		c.handler.OnConnected(myID)

	case env.Cmd == "auth_failed", env.Cmd == "auth_required":
		c.mu.Lock()
		already := c.retried
		c.retried = true
		c.authed = false
		c.mu.Unlock()
		if already {
			c.log.Error("AUTHENTIFICATION REFUSÉE — kok-cache ne sert RIEN ; vérifier la connexion du compte (`kok-cache login`)")
			return
		}
		c.log.Warn("ticket refusé, renouvellement")
		go c.authenticate(ctx, true)

	case env.Cmd == "turn_creds":
		c.handler.OnTurnCreds(TurnCreds{Username: env.Username, Credential: env.Credential, URLs: env.URLs})

	case (env.Cmd == "broadcast" || env.Cmd == "stem_broadcast") && len(env.Data) > 0:
		c.handler.OnBroadcast(env.Data)
	}
}

func (c *Client) authenticate(ctx context.Context, force bool) {
	c.mu.Lock()
	t := c.ticket
	c.mu.Unlock()

	if force || t.Expired() {
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		fresh, err := c.tickets.RTTicket(reqCtx, c.instance)
		cancel()
		if err != nil {
			c.log.Error("impossible d'obtenir un ticket de signalisation", "err", err)
			c.mu.Lock()
			c.authErr = err.Error()
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		c.ticket = fresh
		c.mu.Unlock()
		t = fresh
		c.log.Info("ticket obtenu", "ttl", t.TTL)
	}
	c.Send(map[string]any{"cmd": "auth", "ticket": t.Value})
}

// StatsProvider fournit ce que le helper publie sur lui-même. Facultatif : sans lui, rien n'est
// envoyé et le serveur ne connaîtra que la présence de l'instance.
type StatsProvider func() map[string]any

// SetStats installe le fournisseur d'état publié.
//
// À quoi ça sert : la page « compte » du site k-ok ne peut pas interroger le helper (une page
// HTTPS ne joint pas `127.0.0.1`), mais elle peut demander au serveur de signalisation, qui voit
// les deux connexions sous le même compte. Ces chiffres-là remplacent les anciens indicateurs, en
// mieux — ils décrivent le cache réel, pas le dossier du navigateur.
func (c *Client) SetStats(p StatsProvider) {
	c.mu.Lock()
	c.stats = p
	c.mu.Unlock()
}

func (c *Client) sendStats() {
	c.mu.Lock()
	p := c.stats
	c.mu.Unlock()
	if p == nil {
		return
	}
	c.Send(map[string]any{"rt": map[string]any{"cache_stats": p()}})
}

// PublishStats republie l'état périodiquement, tant que le contexte vit. Le rythme est lent à
// dessein : ces chiffres ne servent qu'à un affichage, ils ne pilotent rien.
func (c *Client) PublishStats(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if c.Authenticated() {
				c.sendStats()
			}
		}
	}
}

// Send envoie un message brut sur le fil.
func (c *Client) Send(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.writeBytes(raw)
}

// Broadcast enveloppe un message P2P. Le serveur route en O(1) vers `toPeerId` s'il est présent —
// c'est le chemin chaud (stem-resp, stem-get, et surtout les candidats ICE) — et diffuse aux seuls
// pairs authentifiés déclarés capables de servir sinon.
func (c *Client) Broadcast(data any) {
	c.Send(map[string]any{"cmd": "stem_broadcast", "data": data})
}

func (c *Client) writeRaw(s string) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	c.writeBytes(raw)
}

func (c *Client) writeBytes(raw []byte) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	// gorilla n'autorise qu'un écrivain à la fois ; le verrou d'écriture est distinct de celui de
	// l'état pour ne pas bloquer les lecteurs de MyID() pendant un envoi.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		c.log.Debug("écriture impossible", "err", err)
	}
}

// String pour les journaux de diagnostic.
func (c *Client) String() string { return fmt.Sprintf("kok-rt(%s)", c.url) }
