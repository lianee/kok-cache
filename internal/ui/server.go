// Package ui sert la page de contrôle de kok-cache sur http://127.0.0.1.
//
// Pourquoi une page dans le navigateur plutôt qu'une fenêtre native : **toute** bibliothèque de
// GUI native en Go exige cgo (Fyne via OpenGL, Wails via webview, la plupart des barres de tâches
// sous Linux et macOS). Or `CGO_ENABLED=0` est ce qui rend les builds reproductibles, donc
// vérifiables par un tiers — c'est le socle de la confiance annoncée dans TECHNIQUE.md, et la
// deuxième raison forte d'avoir choisi pion. Échanger la vérifiabilité contre de l'apparence
// serait un mauvais marché.
//
// ⚠️ À ne PAS confondre avec le HTTPS local écarté le 2026-08-28 : celui-là devait être atteint
// depuis une page `https://www.k-ok.fr`, d'où le mixed-content, un nom DNS public pointant sur
// 127.0.0.1 et une clé privée distribuée à tout le monde. Ici la page est servie ET consultée en
// HTTP, sur une IP littérale, par le processus lui-même : aucun de ces problèmes n'existe.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lianee/kok-cache/internal/auth"
	"github.com/lianee/kok-cache/internal/config"
	"github.com/lianee/kok-cache/internal/mirror"
	"github.com/lianee/kok-cache/internal/peer"
	"github.com/lianee/kok-cache/internal/signal"
	"github.com/lianee/kok-cache/internal/store"
)

//go:embed assets/*
var assets embed.FS

// Deps rassemble ce que la page doit pouvoir lire et piloter.
type Deps struct {
	Version string
	Cfg     *config.Config
	Token   *config.Token
	Store   *store.Store
	Peer    *peer.Manager
	Mirror  *mirror.Mirror
	Auth    *auth.Manager
	Signal  *signal.Client
	Log     *slog.Logger
	// Quit arrête proprement le programme (annulation du contexte principal).
	Quit func()
	// Restart relance le programme. ⚠️ Il DOIT être fourni par le programme principal, car lui
	// seul peut libérer le verrou d'instance avant de lancer le successeur : sinon le nouveau
	// processus se voit refuser l'accès au dossier, meurt aussitôt, et l'ancien s'arrête quand
	// même — « Redémarrer » ne faisait donc que quitter (défaut réel, corrigé le 2026-08-29).
	Restart func() error
	// Update expose la mise à jour du binaire (cmd/kok-cache/update.go). Nil : rien n'est affiché.
	Update UpdateControl
	// WaitForPort : ce processus succède à un autre (redémarrage, mise à jour) qui tient ENCORE le
	// port de contrôle et ne le lâchera qu'en nous voyant prendre le verrou. Attendre ce port
	// quelques secondes plutôt que de glisser sur le suivant : la page qui se recharge, le lien du
	// site et `kok-cache open` visent tous l'adresse fixe.
	WaitForPort bool
}

// UpdateState est ce que la page affiche de la mise à jour.
type UpdateState struct {
	// Enabled : une clé publique de vérification est compilée, une mise à jour est donc possible.
	Enabled bool
	// Auto : l'option de l'utilisateur (`auto_update` dans config.json).
	Auto bool
	// Available : version plus récente connue, "" sinon. Required : la version en cours est sous
	// le plancher fixé par l'éditeur.
	Available string
	Required  bool
	// Updating : téléchargement ou passage de relais en cours ; la page va perdre le contact.
	Updating bool
	// UpdatedFrom : ce processus a été lancé par une mise à jour, depuis cette version.
	UpdatedFrom string
	// Err : dernier échec, pour que la page ne promette pas une mise à jour qui ne vient pas.
	Err string
	// ReleaseURL : où télécharger la version soi-même.
	ReleaseURL string
}

// UpdateControl est ce que la page peut lire et déclencher.
type UpdateControl interface {
	State() UpdateState
	// SetAuto prend acte de l'option ; config.json est écrit par l'interface, sous son verrou.
	SetAuto(ctx context.Context, on bool)
	// UpdateNow installe la version annoncée, option ou pas. Mêmes vérifications.
	UpdateNow(ctx context.Context) error
}

type Server struct {
	deps Deps
	// secret autorise les appels d'API. Il est transmis dans l'URL ouverte, puis renvoyé par la
	// page dans un en-tête.
	secret string
	url    string
	ln     net.Listener

	mu        sync.Mutex
	loginURL  string
	loginBusy bool
	loginErr  string
	notice    string

	// Instantané du reste à synchroniser, calculé EN ARRIÈRE-PLAN.
	//
	// ⚠️ DÉFAUT RÉEL : ce calcul se faisait dans la requête `/api/state`. Il exige l'empreinte de
	// chaque fichier, donc après une copie manuelle du dossier (42,5 Go, aucune empreinte connue)
	// la requête mettait plusieurs MINUTES à répondre — la page attendait, et n'affichait que son
	// en-tête. Une requête d'état ne doit jamais faire de travail long.
	pending  int
	stale    int
	scanned  bool
	scanning bool
}

// Start ouvre le serveur sur un port FIXE de la boucle locale.
//
// ⚠️ Le port était éphémère au départ, et c'était une erreur d'ergonomie : il changeait à chaque
// lancement, donc le site k-ok ne pouvait proposer aucun lien « ouvrir kok-cache », et refermer
// l'onglet coûtait un passage par le terminal. Pour un gain de sécurité nul : un port n'a jamais
// rien protégé, il se balaie en une seconde.
//
// Si le port est déjà pris (une autre application, une instance en cours d'arrêt), on essaie les
// neuf suivants avant de se rabattre sur un port quelconque — un démarrage qui échoue serait bien
// pire qu'un lien qui, ce jour-là, ne tombe pas juste.
func Start(deps Deps) (*Server, error) {
	base := deps.Cfg.UIPort
	var ln net.Listener
	var err error
	// ⚠️ DÉFAUT RÉEL (mesuré le 2026-09-14) : après « Redémarrer », le successeur démarrait
	// pendant que son prédécesseur tenait encore le port, prenait donc le SUIVANT (8384, qui est
	// le port OAuth), et la page rechargée sur 8383 tombait dans le vide. Le prédécesseur ne lâche
	// son port qu'après nous avoir vus prendre le verrou, ce qui prend au plus quelques secondes.
	if deps.WaitForPort {
		deadline := time.Now().Add(10 * time.Second)
		for {
			ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base))
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			ln = nil
		}
	}
	for p := base; ln == nil && p <= base+9; p++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			break
		}
		ln = nil
	}
	if ln == nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, fmt.Errorf("impossible d'ouvrir l'interface locale : %w", err)
		}
	}
	secret, err := loadOrCreateSecret()
	if err != nil {
		return nil, err
	}
	s := &Server{
		deps:   deps,
		secret: secret,
		ln:     ln,
	}
	s.url = fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/api/state", s.guard(s.handleState))
	mux.HandleFunc("/api/login", s.guard(s.handleLogin))
	mux.HandleFunc("/api/logout", s.guard(s.handleLogout))
	mux.HandleFunc("/api/dir", s.guard(s.handleDir))
	mux.HandleFunc("/api/mirror", s.guard(s.handleMirror))
	mux.HandleFunc("/api/autostart", s.guard(s.handleAutostart))
	mux.HandleFunc("/api/restart", s.guard(s.handleRestart))
	mux.HandleFunc("/api/autoupdate", s.guard(s.handleAutoUpdate))
	mux.HandleFunc("/api/update", s.guard(s.handleUpdate))
	mux.HandleFunc("/api/quit", s.guard(s.handleQuit))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // la fermeture passe par le contexte principal

	go s.scanLoop()

	s.writeStateFile()
	return s, nil
}

// loadOrCreateSecret garde le secret de l'interface D'UN LANCEMENT À L'AUTRE.
//
// ⚠️ DÉFAUT RÉEL corrigé le 2026-08-29 : il était tiré à chaque démarrage, si bien qu'une page
// ouverte avant un redémarrage gardait l'ancien. Toutes ses requêtes recevaient 403 — donc plus
// aucun bouton ne répondait, « Quitter » compris — et comme le script avalait l'erreur, il ne
// restait à l'écran que l'en-tête. Le bouton « Redémarrer » de l'interface se cassait lui-même
// par construction.
//
// Le secret vit à côté du jeton OAuth, en 0600. Il n'ouvre que l'interface locale, sur la boucle
// locale : quiconque peut lire ce fichier peut de toute façon déjà lire le jeton du compte.
func loadOrCreateSecret() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ui-secret")
	if raw, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(raw)); len(v) >= 32 {
			return v, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	// ⛔ Écriture ATOMIQUE (temporaire + renommage), comme pour config.json et token.json.
	// Un `os.WriteFile` interrompu — coupure de courant, disque plein — laisse un secret TRONQUÉ.
	// Au démarrage suivant il est jugé trop court, donc régénéré : toutes les pages déjà ouvertes
	// reçoivent alors 403 sur chaque appel, y compris « Quitter ». C'est exactement le défaut
	// corrigé le 2026-08-29 (secret non persistant), atteignable par un autre chemin.
	tmp, err := os.CreateTemp(dir, ".ui-secret-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // sans effet après un renommage réussi
	if _, err := tmp.WriteString(secret + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return secret, nil
}

func (s *Server) URL() string { return s.url }

// Open lance le navigateur par défaut sur la page. L'URL est toujours affichée par ailleurs :
// ouvrir le navigateur est un confort, pas une dépendance (poste sans navigateur par défaut,
// session distante).
func (s *Server) Open() {
	// ⚠️ Garde-fou ajouté après un incident réel : des instances de TEST, lancées sans compte,
	// ouvraient la page de connexion dans le navigateur de l'utilisateur puis étaient tuées —
	// donc des fenêtres qui surgissent et se referment aussitôt pendant qu'il travaille.
	// Sert aussi à qui veut lancer kok-cache sans que rien ne s'ouvre.
	if os.Getenv("KOK_CACHE_NO_BROWSER") != "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", s.url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", s.url)
	default:
		cmd = exec.Command("xdg-open", s.url)
	}
	_ = cmd.Start()
}

// writeStateFile mémorise l'adresse pour `kok-cache open` — utile surtout quand le port par défaut
// était pris et que l'adresse n'est donc pas celle attendue.
func (s *Server) writeStateFile() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	_ = os.WriteFile(dir+"/ui-url", []byte(s.url+"\n"), 0o600)
}

// ── protection des appels ─────────────────────────────────────────────────────────────────────

// guard protège les APPELS D'API. Le port étant désormais fixe et la page librement accessible,
// c'est ici que tout se joue — trois barrières qui tiennent chacune seule :
//
//  1. Un secret tiré à chaque lancement, **jamais dans l'URL** : il n'est écrit que dans le HTML
//     servi par ce serveur. Une page tierce ne peut pas le lire — la politique de même origine lui
//     interdit de voir la réponse de `127.0.0.1`, y compris dans une iframe.
//  2. Un en-tête PERSONNALISÉ, que seul un `fetch` peut poser. Un formulaire soumis en travers des
//     origines — la façon classique de forger une requête — en est incapable, et un `fetch`
//     déclenche une vérification préalable CORS à laquelle nous ne répondons jamais favorablement,
//     puisque nous n'émettons aucun en-tête d'autorisation d'origine.
//  3. L'origine, refusée si elle existe et n'est pas la nôtre.
//
// 📌 Ce que le port ne protégeait PAS, et qu'il est donc inutile de rendre imprévisible : il se
// balaie en une seconde. Le cacher n'était pas une barrière, seulement une gêne pour l'utilisateur.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(origin) {
			http.Error(w, "origine refusée", http.StatusForbidden)
			return
		}
		given := r.Header.Get("X-KOK-Token")
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.secret)) != 1 {
			http.Error(w, "jeton d'interface invalide", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) sameOrigin(origin string) bool {
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return host == s.host()
}

func (s *Server) host() string {
	if addr, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return fmt.Sprintf("127.0.0.1:%d", addr.Port)
	}
	return ""
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// ⛔ Jamais dans une iframe : une page tierce ne pourrait pas en lire le contenu, mais elle
	// pourrait le superposer à ses propres boutons pour faire cliquer l'utilisateur à son insu.
	w.Header().Set("X-Frame-Options", "DENY")
	raw, err := assets.ReadFile("assets/app.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := strings.Replace(string(raw), "__KOK_TOKEN__", s.secret, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Jamais en cache : une page resservie depuis le cache du navigateur porterait un secret
	// périmé, et l'utilisateur serait bloqué sans message.
	w.Header().Set("Cache-Control", "no-store")
	// La page ne charge rien de l'extérieur : tout est en ligne dans le fichier.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'")
	_, _ = w.Write([]byte(page))
}

// handleFavicon sert l'icône embarquée (la même que l'entrée de menu) : la page reste ainsi
// sans aucune ressource extérieure, et l'onglet garde son icône même hors ligne.
func (s *Server) handleFavicon(w http.ResponseWriter, _ *http.Request) {
	raw, err := assets.ReadFile("assets/icon.png")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(raw)
}

// scanLoop tient à jour le reste à synchroniser, hors de toute requête.
//
// Le premier passage peut durer plusieurs minutes (vérification d'un gros dossier) : pendant ce
// temps la page reste vivante et l'annonce, au lieu de se figer sans un mot.
func (s *Server) scanLoop() {
	for {
		if s.deps.Mirror.Count() > 0 {
			s.mu.Lock()
			s.scanning = true
			s.mu.Unlock()

			pending := s.deps.Mirror.Pending()
			stale := 0
			for _, it := range pending {
				if it.Stale {
					stale++
				}
			}

			s.mu.Lock()
			s.pending, s.stale, s.scanned, s.scanning = len(pending), stale, true, false
			s.mu.Unlock()
		}
		time.Sleep(15 * time.Second)
	}
}

// ── état ──────────────────────────────────────────────────────────────────────────────────────

type stateResponse struct {
	Version   string `json:"version"`
	Connected bool   `json:"connected"`
	Account   string `json:"account"`
	LoggedIn  bool   `json:"loggedIn"`
	LoginBusy bool   `json:"loginBusy"`
	LoginURL  string `json:"loginUrl"`
	LoginErr  string `json:"loginErr"`
	AuthErr   string `json:"authErr"`
	Notice    string `json:"notice"`
	Dir       string `json:"dir"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	Catalog   int    `json:"catalog"`
	// CatalogBytes : volume ESTIMÉ du catalogue complet, extrapolé de la taille moyenne réellement
	// observée sur ce disque. Vaut 0 tant qu'on n'a rien pour l'estimer.
	CatalogBytes int64 `json:"catalogBytes"`
	Pending      int   `json:"pending"`
	Stale        int   `json:"stale"`
	// Scanning : une vérification des fichiers est en cours ; Scanned : au moins une a abouti.
	// ToVerify/Hashed donnent la progression — indispensables après une copie manuelle du dossier,
	// où tout est à revérifier et où la page semblerait figée sans eux.
	Scanning  bool            `json:"scanning"`
	Scanned   bool            `json:"scanned"`
	ToVerify  int             `json:"toVerify"`
	Hashed    int64           `json:"hashed"`
	Mirror    bool            `json:"mirror"`
	Autostart bool            `json:"autostart"`
	Activity  []peer.Activity `json:"activity"`
	// Update : absent quand le programme n'a pas de mécanisme de mise à jour (tests).
	Update *updateResponse `json:"update,omitempty"`
}

type updateResponse struct {
	Enabled     bool   `json:"enabled"`
	Auto        bool   `json:"auto"`
	Available   string `json:"available"`
	Required    bool   `json:"required"`
	Updating    bool   `json:"updating"`
	UpdatedFrom string `json:"updatedFrom"`
	Err         string `json:"err"`
	ReleaseURL  string `json:"releaseUrl"`
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	// ⛔ TOUT CE QUI SORT DE CE PAQUET EST LU AVANT DE PRENDRE `s.mu`.
	//
	// La version précédente tenait `s.mu` pendant ces appels. Or `auth.Manager.AccessToken()` garde
	// SON mutex pendant une requête HTTP de rafraîchissement — 30 s de délai client. Un
	// rafraîchissement en cours bloquait donc `Auth.Email()`, donc `handleState`, qui tenait `s.mu`,
	// donc `handleLogin`, `handleDir`, `handleLogout` ET `scanLoop`. Résultat : la page entière
	// figée jusqu'à 30 s, environ une fois par heure — et bien plus si le réseau est mauvais.
	// C'est le symptôme « page figée » déjà rencontré deux fois, par un troisième chemin.
	files, bytes := s.deps.Store.DiskUsage()
	connected := s.deps.Signal.Authenticated()
	account := s.deps.Auth.Email()
	loggedIn := s.deps.Auth.Configured() // via Auth : Token n'a pas de verrou (course du 2026-08-31)
	authErr := s.deps.Signal.AuthError()
	catalog := s.deps.Mirror.Count()
	mirrorOn := s.deps.Mirror.Active()
	autoOn := autostartEnabled()
	activity := s.deps.Peer.Snapshot()
	hashed := s.deps.Store.Hashed()
	var upd *updateResponse
	if s.deps.Update != nil {
		u := s.deps.Update.State()
		upd = &updateResponse{Enabled: u.Enabled, Auto: u.Auto, Available: u.Available,
			Required: u.Required, Updating: u.Updating, UpdatedFrom: u.UpdatedFrom, Err: u.Err,
			ReleaseURL: u.ReleaseURL}
	}

	s.mu.Lock()
	st := stateResponse{
		Version:   s.deps.Version,
		Connected: connected,
		Account:   account,
		LoggedIn:  loggedIn,
		LoginBusy: s.loginBusy,
		LoginURL:  s.loginURL,
		LoginErr:  s.loginErr,
		AuthErr:   authErr,
		Notice:    s.notice,
		// ⛔ `Cfg` est lu SOUS `s.mu` : `handleDir`, `handleMirror` et `handleAutostart` écrivent ces
		// champs, et les handlers HTTP tournent chacun dans leur goroutine pendant que la page sonde
		// toutes les 1,5 s. `config.Config` n'a aucun verrou — c'était une course de données
		// (audit 2026-08-31), déclenchée par un simple changement de dossier en cours de sondage.
		Dir:       s.deps.Cfg.CacheDir,
		Files:     files,
		Bytes:     bytes,
		Catalog:   catalog,
		Mirror:    mirrorOn,
		Autostart: autoOn,
		Activity:  activity,
		Update:    upd,
	}
	// ⛔ AUCUN calcul long ici : on sert le dernier instantané produit par scanLoop.
	st.Pending, st.Stale, st.Scanning, st.Scanned = s.pending, s.stale, s.scanning, s.scanned
	s.mu.Unlock()
	st.Hashed = hashed
	if st.Scanning {
		// Coût négligeable (un `stat` par fichier) : donne le dénominateur de la progression.
		st.ToVerify = s.deps.Store.UnhashedCount()
	}

	if st.Catalog > 0 {

		// Volume du catalogue : ESTIMÉ, jamais écrit en dur.
		//
		// ⚠️ La page annonçait « environ 44 Go » dans son texte. Ce chiffre vieillit tout seul —
		// le catalogue grossit d'une à deux chansons par semaine — et le corriger imposerait de
		// republier un binaire chez tout le monde pour une phrase.
		// Extrapolation à partir de ce que ce disque contient déjà : deux stems par chanson,
		// taille moyenne mesurée. Le cache complet, l'estimation devient la mesure exacte.
		if files > 0 {
			st.CatalogBytes = (bytes / int64(files)) * int64(st.Catalog) * 2
		}
	}
	writeJSON(w, st)
}

// ── actions ───────────────────────────────────────────────────────────────────────────────────

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.loginBusy {
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	s.loginBusy, s.loginErr, s.loginURL = true, "", ""
	s.mu.Unlock()

	go func() {
		// Contexte propre : la connexion doit survivre à la requête HTTP qui l'a déclenchée.
		ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
		defer cancel()

		err := s.deps.Auth.Login(ctx, s.deps.Cfg.OAuthPort, func(u string) {
			s.mu.Lock()
			s.loginURL = u
			s.mu.Unlock()
		})

		s.mu.Lock()
		s.loginBusy = false
		if err != nil {
			s.loginErr = err.Error()
		} else {
			s.notice = "Compte connecté. Le partage démarre."
		}
		s.mu.Unlock()
	}()

	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.deps.Auth.Logout(ctx); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.mu.Lock()
	s.notice = "Déconnecté. Le jeton a été révoqué et effacé, le partage est arrêté."
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// handleDir ouvre la boîte de dialogue NATIVE du système.
//
// Une page web ne peut pas fournir un chemin disque : `<input webkitdirectory>` ne donne que des
// noms relatifs, jamais l'emplacement réel. Le sélecteur natif est donc la seule voie — et il est
// atteint en lançant l'utilitaire du système, sans cgo ni dépendance.
func (s *Server) handleDir(w http.ResponseWriter, _ *http.Request) {
	dir, err := pickDirectory(s.deps.Cfg.CacheDir)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if dir == "" { // l'utilisateur a annulé
		writeJSON(w, map[string]any{"ok": true, "changed": false})
		return
	}
	s.mu.Lock() // même verrou que la lecture dans handleState : Config n'a pas le sien
	s.deps.Cfg.CacheDir = dir
	err = s.deps.Cfg.Save()
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.mu.Lock()
	// Honnêteté d'interface : le dossier n'est pas déplacé à chaud, et le dire vaut mieux que de
	// laisser croire que c'est fait.
	s.notice = "Nouveau dossier enregistré. Il sera utilisé après un redémarrage de kok-cache " +
		"(les extraits déjà stockés restent où ils sont)."
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "changed": true, "dir": dir})
}

func (s *Server) handleMirror(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	// Borne de cohérence : il faut déjà le secret pour arriver ici, donc un appelant hostile
	// serait un processus local du même utilisateur — qui a de toute façon tous les droits. On
	// borne quand même, par la même discipline qu'ailleurs : rien de ce qui vient d'un corps
	// de requête ne doit pouvoir dicter une allocation.
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "requête illisible", http.StatusBadRequest)
		return
	}
	s.deps.Mirror.SetActive(body.On) // pris en compte immédiatement, sans redémarrage
	s.mu.Lock()                      // même verrou que la lecture dans handleState : Config n'a pas le sien
	s.deps.Cfg.Mirror = body.On
	err := s.deps.Cfg.Save()
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	// Borne de cohérence : il faut déjà le secret pour arriver ici, donc un appelant hostile
	// serait un processus local du même utilisateur — qui a de toute façon tous les droits. On
	// borne quand même, par la même discipline qu'ailleurs : rien de ce qui vient d'un corps
	// de requête ne doit pouvoir dicter une allocation.
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "requête illisible", http.StatusBadRequest)
		return
	}
	if err := setAutostart(body.On); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.mu.Lock() // même verrou que la lecture dans handleState : Config n'a pas le sien
	s.deps.Cfg.Autostart = body.On
	_ = s.deps.Cfg.Save()
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// handleAutoUpdate enregistre l'option de mise à jour automatique (config.json) et en informe
// l'updater, qui vérifie aussitôt si on vient de l'activer.
func (s *Server) handleAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "requête illisible", http.StatusBadRequest)
		return
	}
	if s.deps.Update == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "mise à jour indisponible"})
		return
	}
	s.mu.Lock() // même verrou que la lecture dans handleState : Config n'a pas le sien
	s.deps.Cfg.AutoUpdate = body.On
	err := s.deps.Cfg.Save()
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.deps.Update.SetAuto(context.Background(), body.On)
	writeJSON(w, map[string]any{"ok": true})
}

// handleUpdate installe la version annoncée sur demande de l'utilisateur. La réponse part avant
// que quoi que ce soit ne commence : le passage de relais coupe ce serveur.
func (s *Server) handleUpdate(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Update == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "mise à jour indisponible"})
		return
	}
	if err := s.deps.Update.UpdateNow(context.Background()); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleRestart relance le programme. Utilisé après un changement de dossier — plus simple et plus
// sûr que de déplacer un store en cours d'utilisation, avec des transferts en vol.
func (s *Server) handleRestart(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Restart == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "redémarrage indisponible"})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(300 * time.Millisecond) // laisse la réponse partir
		if err := s.deps.Restart(); err != nil {
			s.deps.Log.Error("redémarrage impossible", "err", err)
		}
	}()
}

func (s *Server) handleQuit(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.deps.Quit()
	}()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
