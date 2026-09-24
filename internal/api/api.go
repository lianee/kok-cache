// Package api parle à kok.php — les deux seules commandes dont kok-cache a besoin.
//
//	cmd=rt-ticket     → un ticket signé de rôle `cache`, valable pour la signalisation
//	cmd=stem-manifest → la liste (id, [hash acc, hash voc]) de tout le catalogue audio
//
// Les deux passent par `kok_rt_client_auth()` côté serveur, qui reconnaît le jeton OAuth en
// `Authorization: Bearer` et attribue le rôle `cache`. Aucun secret partagé n'est en jeu : c'est
// le jeton personnel de l'utilisateur, révocable individuellement sans toucher à son compte.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSource fournit un jeton d'accès valide, en le rafraîchissant si besoin.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

type Client struct {
	apiURL  string
	referer string
	tokens  TokenSource
	http    *http.Client
}

func New(apiURL, referer string, tokens TokenSource) *Client {
	return &Client{
		apiURL:  apiURL,
		referer: referer,
		tokens:  tokens,
		// Le manifeste complet fait ~400 Ko et peut demander 1,25 s à cache froid côté serveur.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// ErrMembershipRequired : le compte n'a pas d'abonnement actif. Le cache local reste réservé aux
// abonnés — ce n'est pas une panne, et le message doit le dire clairement.
var ErrMembershipRequired = errors.New(
	"votre compte k-ok n'a pas d'abonnement actif : le cache local y est réservé")

// Ticket est la réponse de cmd=rt-ticket.
type Ticket struct {
	Value    string
	TTL      time.Duration
	IssuedAt time.Time
}

// Expired dit si le ticket doit être renouvelé. La marge (un quart du TTL) évite de présenter au
// serveur un ticket qui expire pendant la poignée de main.
func (t Ticket) Expired() bool {
	if t.Value == "" {
		return true
	}
	return time.Since(t.IssuedAt) > t.TTL-t.TTL/4
}

type ticketResponse struct {
	Success bool   `json:"success"`
	Ticket  string `json:"ticket"`
	TTL     int64  `json:"ttl"`
	Error   string `json:"error"`
}

// RTTicket échange le jeton OAuth contre un ticket de signalisation de rôle `cache`.
//
// ⛔ Le ticket n'est JAMAIS écrit sur disque. Un identifiant de longue durée que rien ne renouvelle
// est une panne programmée, et ici une panne SILENCIEUSE : sans ticket valide on reste connecté
// mais on ne sert plus un seul bloc, indiscernable d'un essaim calme.
func (c *Client) RTTicket(ctx context.Context, instance string) (Ticket, error) {
	form := url.Values{}
	form.Set("cmd", "rt-ticket")
	if instance != "" {
		form.Set("inst", instance)
	}
	var out ticketResponse
	if err := c.post(ctx, form, &out); err != nil {
		return Ticket{}, err
	}
	if !out.Success || out.Ticket == "" {
		if out.Error == "membership_required" {
			// Erreur NOMMÉE par le serveur, traduite ici : sans ça l'utilisateur lirait
			// « authentification refusée » et chercherait une panne technique là où il n'y a qu'un
			// abonnement expiré.
			return Ticket{}, ErrMembershipRequired
		}
		return Ticket{}, fmt.Errorf("rt-ticket refusé : %s", errText(out.Error))
	}
	ttl := time.Duration(out.TTL) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return Ticket{Value: out.Ticket, TTL: ttl, IssuedAt: time.Now()}, nil
}

// Manifest est le catalogue tel que le serveur en fait autorité.
type Manifest struct {
	// Sig identifie la version du manifeste. La renvoyer au prochain appel ramène une
	// vérification inchangée à 87 octets et 50 ms — c'est ce qui rend une vérification
	// fréquente acceptable.
	Sig string
	// Count est le nombre de chansons annoncé par le serveur, même quand rien n'est transmis.
	Count int
	// Stems : songId → [hash accompaniment, hash vocals]. Nil quand Unchanged est vrai.
	Stems map[int][2]string
	// Unchanged : la signature fournie était déjà la bonne, rien n'a été transmis.
	Unchanged bool
}

type manifestResponse struct {
	Success   bool                 `json:"success"`
	Sig       string               `json:"sig"`
	Count     int                  `json:"count"`
	Unchanged bool                 `json:"unchanged"`
	Stems     map[string][2]string `json:"stems"`
	Error     string               `json:"error"`
}

// StemManifest récupère le manifeste. `knownSig` est la signature déjà détenue (vide au premier
// appel) : si elle est à jour, le serveur ne renvoie pas les ~400 Ko.
//
// ⭐ C'est l'AUTORITÉ sur la péremption d'un stem. Un désaccord de hash signalé par un pair n'est
// qu'un indice ; c'est cette liste-ci qui décide qu'un fichier est périmé.
func (c *Client) StemManifest(ctx context.Context, knownSig string) (*Manifest, error) {
	form := url.Values{}
	form.Set("cmd", "stem-manifest")
	if knownSig != "" {
		form.Set("sig", knownSig)
	}
	var out manifestResponse
	if err := c.post(ctx, form, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, fmt.Errorf("stem-manifest refusé : %s", errText(out.Error))
	}
	m := &Manifest{Sig: out.Sig, Count: out.Count, Unchanged: out.Unchanged}
	if out.Unchanged {
		return m, nil
	}
	m.Stems = make(map[int][2]string, len(out.Stems))
	for k, pair := range out.Stems {
		id, err := parseID(k)
		if err != nil {
			continue
		}
		// Une entrée sans les DEUX hashes est incomplète — le serveur les exclut déjà (« une
		// chanson, c'est deux stems ou rien »), on ne rattrape pas ce qui aurait glissé.
		if pair[0] == "" || pair[1] == "" {
			continue
		}
		m.Stems[id] = pair
	}
	return m, nil
}

func (c *Client) post(ctx context.Context, form url.Values, out any) error {
	token, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return fmt.Errorf("jeton indisponible : %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	// ⚠️ kok.php refuse toute commande sans Referer du même hôte, AVANT tout traitement.
	// Ce n'est pas ce qui nous authentifie, c'est un garde-fou hérité à satisfaire.
	req.Header.Set("Referer", c.referer)
	req.Header.Set("User-Agent", "kok-cache")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Borne de lecture : le manifeste complet fait ~400 Ko, 8 Mo laissent une marge très large
	// sans exposer le processus à une réponse aberrante.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kok.php a répondu %d", resp.StatusCode)
	}
	if len(body) == 0 {
		// Symptôme connu : réponse vide = Referer manquant ou refusé en amont du traitement.
		return errors.New("réponse vide de kok.php (Referer refusé ?)")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("réponse illisible : %w", err)
	}
	return nil
}

func parseID(s string) (int, error) {
	var id int
	if _, err := fmt.Sscanf(s, "%d", &id); err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, errors.New("id invalide")
	}
	return id, nil
}

func errText(s string) string {
	if s == "" {
		return "réponse inattendue"
	}
	return s
}
