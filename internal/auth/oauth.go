// Package auth implémente l'identification de kok-cache : OAuth 2.0 Authorization Code + PKCE,
// avec redirection en boucle locale (RFC 8252), puis rafraîchissement silencieux.
//
// Pourquoi OAuth et pas un mot de passe ni un jeton partagé :
//   - l'application stocke un JETON, jamais un mot de passe ;
//   - le jeton est révocable individuellement — couper un helper ne touche pas au compte ;
//   - aucun secret partagé n'est distribué, donc `strings` sur le binaire ne rend rien.
//
// ⚠️ PIÈGE SERVEUR, vérifié en production le 2026-08-28 : `register.php` DEVINE le `user_type`, et
// une `redirect_uri` en boucle locale — exactement celle qu'impose la RFC 8252 pour une app native
// — est inférée **`admin`**, donc authentifiée contre la table `admins`. Le paramètre explicite
// `user_type:"user"` a la priorité sur toute inférence : il n'est pas optionnel ici.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lianee/kok-cache/internal/config"
)

// Scope : `openid email` route l'enregistrement vers la table `users` et donne un `sub` numérique,
// que le pont rt-ticket exige (un `sub` en `a:` est un admin, et il est refusé).
const scope = "openid email"

// refreshSkew : on renouvelle avant l'échéance réelle. Le jeton d'accès vit 1 h ; se réveiller à
// la dernière seconde exposerait chaque requête à un refus pour un simple décalage d'horloge.
const refreshSkew = 5 * time.Minute

type Manager struct {
	base string
	// instance nomme la machine. Il finit dans le `client_name`, donc sur l'écran d'autorisation :
	// « kok-cache sur portable-salon » dit à l'utilisateur QUI demande, ce que « kok-cache » tout
	// court ne disait pas.
	instance string
	http     *http.Client

	mu  sync.Mutex
	tok *config.Token
}

func NewManager(oauthBase, instance string, tok *config.Token) *Manager {
	return &Manager{
		base:     strings.TrimRight(oauthBase, "/"),
		instance: instance,
		http:     &http.Client{Timeout: 30 * time.Second},
		tok:      tok,
	}
}

// ErrNotLoggedIn : aucun identifiant stocké. L'appelant doit inviter à lancer `kok-cache login`,
// jamais tenter de deviner quoi que ce soit.
var ErrNotLoggedIn = errors.New("aucun compte connecté (lancer `kok-cache login`)")

// AccessToken renvoie un jeton d'accès valide, en le rafraîchissant si nécessaire.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.tok == nil || (m.tok.AccessToken == "" && m.tok.RefreshToken == "") {
		return "", ErrNotLoggedIn
	}
	if m.tok.AccessToken != "" && time.Until(time.Unix(m.tok.ExpiresAt, 0)) > refreshSkew {
		return m.tok.AccessToken, nil
	}
	if m.tok.RefreshToken == "" {
		return "", ErrNotLoggedIn
	}
	if err := m.refreshLocked(ctx); err != nil {
		return "", err
	}
	return m.tok.AccessToken, nil
}

// Configured dit si un compte est connecté.
//
// ⛔ À UTILISER PARTOUT plutôt que `Token.Configured()` en direct. `Token` n'a pas de verrou : ses
// champs sont écrits ici sous `m.mu` (connexion, rafraîchissement, oubli), et deux lecteurs
// tournent en permanence dans d'autres goroutines — le superviseur de services (toutes les 2 s) et
// `handleState` de l'interface (à chaque sondage de la page, 1,5 s). Lire les champs sans passer
// par ce verrou est une COURSE DE DONNÉES au sens strict de Go : lecture non synchronisée
// concurrente d'une écriture. Trouvée à l'audit du 2026-08-31.
func (m *Manager) Configured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tok != nil && m.tok.Configured()
}

// Forget efface les identifiants locaux, sous verrou.
//
// ⚠️ `Logout` appelait `tok.Forget()` APRÈS avoir relâché `m.mu` : l'effacement des champs se
// faisait donc hors protection, en pleine concurrence avec les lecteurs ci-dessus.
func (m *Manager) Forget() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok == nil {
		return nil
	}
	return m.tok.Forget()
}

// Email renvoie l'adresse associée au compte, si elle a été obtenue à la connexion.
func (m *Manager) Email() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok == nil {
		return ""
	}
	return m.tok.Email
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (m *Manager) refreshLocked(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", m.tok.ClientID)
	form.Set("refresh_token", m.tok.RefreshToken)
	form.Set("scope", scope)

	var out tokenResponse
	if err := m.postForm(ctx, m.base+"/token", form, &out); err != nil {
		return err
	}
	if out.AccessToken == "" {
		// Un refus ici veut dire que l'utilisateur a révoqué l'app, ou que le refresh a expiré
		// (30 j). C'est un état normal, pas une panne : il faut se reconnecter.
		return fmt.Errorf("%w : rafraîchissement refusé (%s)", ErrNotLoggedIn, errText(out))
	}
	m.applyLocked(out)
	return m.tok.Save()
}

func (m *Manager) applyLocked(out tokenResponse) {
	m.tok.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		m.tok.RefreshToken = out.RefreshToken
	}
	ttl := out.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	m.tok.ExpiresAt = time.Now().Add(time.Duration(ttl) * time.Second).Unix()
}

// Login exécute le flux interactif complet. `prompt` reçoit l'URL à ouvrir : le programme tente de
// lancer le navigateur, mais l'URL doit TOUJOURS être affichée — un poste sans navigateur par
// défaut, un serveur distant ou une session en SSH sont des cas ordinaires.
func (m *Manager) Login(ctx context.Context, port int, prompt func(url string)) error {
	// Port FIXE, et c'est ce qui permet de réutiliser l'application déjà enregistrée.
	//
	// ⚠️ DÉFAUT CORRIGÉ (constaté à l'usage) : avec un port tiré au hasard, la `redirect_uri`
	// changeait à chaque connexion, donc il fallait ENREGISTRER UNE NOUVELLE APPLICATION à chaque
	// fois. Conséquences visibles : le serveur redemandait l'autorisation même à quelqu'un qui
	// venait de l'accorder, et la table des clients se remplissait d'une entrée par connexion.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Repli : mieux vaut une connexion qui aboutit avec un client neuf qu'un échec sec.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("impossible d'ouvrir le port local : %w", err)
		}
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	// Application déjà enregistrée avec CETTE adresse de retour : on la réutilise. Le serveur
	// compare la `redirect_uri` à l'octet près, donc tout changement impose un réenregistrement.
	m.mu.Lock()
	clientID := ""
	if m.tok.ClientID != "" && m.tok.RedirectURI == redirectURI {
		clientID = m.tok.ClientID
	}
	m.mu.Unlock()

	if clientID == "" {
		clientID, err = m.register(ctx, redirectURI)
		if err != nil {
			return err
		}
	}

	verifier, err := randomString(64)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state, err := randomString(24)
	if err != nil {
		return err
	}

	authURL := m.base + "/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {scope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				writePage(w, "Autorisation refusée", "Vous pouvez fermer cette page et revenir à kok-cache.")
				if e == "access_denied" {
					// Message écrit pour quelqu'un qui vient de cliquer « Refuser » : ni code
					// d'erreur, ni impasse.
					done <- result{err: errors.New(
						"vous avez refusé l'autorisation. Sans elle, kok-cache ne peut pas rejoindre " +
							"le partage — vous pouvez réessayer quand vous voulez")}
					return
				}
				done <- result{err: fmt.Errorf("autorisation refusée : %s", e)}
				return
			}
			// `state` protège contre une requête forgée arrivant sur notre port local — le
			// navigateur de la machine peut recevoir n'importe quelle URL.
			if q.Get("state") != state {
				writePage(w, "Requête inattendue", "Vous pouvez fermer cette page.")
				done <- result{err: errors.New("state invalide")}
				return
			}
			code := q.Get("code")
			if code == "" {
				writePage(w, "Requête incomplète", "Vous pouvez fermer cette page.")
				done <- result{err: errors.New("code absent")}
				return
			}
			writePage(w, "kok-cache est connecté",
				"Vous pouvez fermer cette page : kok-cache démarre le partage tout seul.")
			done <- result{code: code}
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(ln) //nolint:errcheck // l'arrêt est piloté par le contexte ci-dessous
	defer srv.Close()

	prompt(authURL)
	openBrowser(authURL)

	// Délai large : l'utilisateur doit pouvoir saisir un mot de passe, voire créer son compte.
	loginCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var code string
	select {
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		code = r.code
	case <-loginCtx.Done():
		return errors.New("délai dépassé — aucune réponse du navigateur")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	form.Set("code", code)

	var out tokenResponse
	if err := m.postForm(ctx, m.base+"/token", form, &out); err != nil {
		return err
	}
	if out.AccessToken == "" {
		return fmt.Errorf("échange du code refusé (%s)", errText(out))
	}

	m.mu.Lock()
	m.tok.ClientID = clientID
	m.tok.RedirectURI = redirectURI // pour réutiliser l'application à la prochaine connexion
	m.applyLocked(out)
	m.mu.Unlock()

	// L'adresse e-mail n'est lue que pour être AFFICHÉE (« connecté en tant que … »). Elle n'est
	// jamais envoyée ailleurs et n'entre dans aucune décision.
	if mail := m.fetchEmail(ctx, out.AccessToken); mail != "" {
		m.mu.Lock()
		m.tok.Email = mail
		m.mu.Unlock()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tok.Save()
}

// Logout révoque le jeton côté serveur puis efface le fichier local. La révocation compte : le
// pont rt-ticket vérifie `oauth_access_tokens.revoked`, donc elle coupe réellement le helper.
func (m *Manager) Logout(ctx context.Context) error {
	m.mu.Lock()
	tok := m.tok
	m.mu.Unlock()
	if tok == nil || !m.Configured() {
		return nil
	}
	for _, pair := range [][2]string{
		{tok.AccessToken, "access_token"},
		{tok.RefreshToken, "refresh_token"},
	} {
		if pair[0] == "" {
			continue
		}
		form := url.Values{}
		form.Set("token", pair[0])
		form.Set("token_type_hint", pair[1])
		form.Set("client_id", tok.ClientID)
		// Une révocation qui échoue ne doit pas empêcher l'effacement local : l'utilisateur a
		// demandé à se déconnecter, le jeton expirera de lui-même.
		_ = m.postForm(ctx, m.base+"/revoke", form, &struct{}{})
	}
	// ⛔ Par `m.Forget()`, donc SOUS VERROU. La version précédente appelait `tok.Forget()` ici,
	// hors de `m.mu` : l'effacement des champs courait contre le superviseur de services et
	// `handleState`, qui les lisent en continu depuis d'autres goroutines.
	return m.Forget()
}

type registerResponse struct {
	ClientID string `json:"client_id"`
	UserType string `json:"user_type"`
	Error    string `json:"error"`
}

func (m *Manager) register(ctx context.Context, redirectURI string) (string, error) {
	// Le nom est le SEUL texte que le client contrôle sur l'écran d'autorisation. « kok-cache »
	// tout court n'apprend rien à personne ; y mettre la machine répond au moins à « qui
	// demande ? ».
	name := "kok-cache"
	if m.instance != "" {
		name += " sur " + m.instance
	}
	body, err := json.Marshal(map[string]any{
		"client_name":   name,
		"redirect_uris": []string{redirectURI},
		"scope":         scope,
		// ⛔ NE PAS RETIRER — cf. le bandeau du paquet : sans ce champ, une redirection en boucle
		// locale est inférée `admin` et l'utilisateur ne pourrait pas se connecter avec son compte.
		"user_type": "user",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/register", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out registerResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("enregistrement illisible (HTTP %d)", resp.StatusCode)
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("enregistrement refusé : %s", errText(tokenResponse{Error: out.Error}))
	}
	if out.UserType != "" && out.UserType != "user" {
		// Détection explicite du piège plutôt qu'un échec obscur trois étapes plus loin.
		return "", fmt.Errorf("le serveur a enregistré l'application comme %q au lieu de \"user\"", out.UserType)
	}
	return out.ClientID, nil
}

func (m *Manager) fetchEmail(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/userinfo", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := m.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Email string `json:"email"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &out)
	return out.Email
}

func (m *Manager) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func writePage(w http.ResponseWriter, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title>"+
		"<body style=\"font-family:system-ui;margin:4rem auto;max-width:30rem;text-align:center\">"+
		"<h1>%s</h1><p>%s</p>", title, title, msg)
}

// openBrowser est un confort, jamais une dépendance : l'URL est de toute façon affichée.
func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func errText(out tokenResponse) string {
	switch {
	case out.ErrorDesc != "":
		return out.ErrorDesc
	case out.Error != "":
		return out.Error
	default:
		return "réponse inattendue"
	}
}
