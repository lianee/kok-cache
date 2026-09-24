// Package config lit et écrit la configuration de kok-cache.
//
// Deux fichiers distincts, et c'est délibéré :
//   - config.json : réglages, lisible, éditable à la main, sauvegardable ;
//   - token.json  : identifiants OAuth, mode 0600, jamais affiché, jamais journalisé.
//
// ⛔ Aucun secret partagé n'est compilé dans le binaire. C'est l'invariant central du projet :
// un secret présent dans un binaire distribué à des inconnus n'est pas un secret (`strings` suffit).
// La seule identité de kok-cache est le jeton OAuth de SON utilisateur, obtenu en ligne.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Adresses du service. ⛔ EN DUR, JAMAIS dans config.json — décidé le 2026-08-30, après la
// bascule vers www.k-ok.fr, et pour deux raisons distinctes :
//
//  1. Sinon un changement de domaine est IRRATTRAPABLE sur le parc installé. `Load()` écrit le
//     fichier complet dès le premier lancement, donc l'ancienne adresse s'y grave et le programme
//     la relit indéfiniment — même après mise à jour du binaire. Une adresse en dur, au contraire,
//     suit la mise à jour : c'est le seul canal de correction dont on dispose réellement.
//  2. `oauth_base` dans un fichier éditable est une SURFACE D'ATTAQUE. Qui peut écrire ce fichier
//     détourne le parcours OAuth vers un serveur de hameçonnage et récupère l'autorisation. Pour
//     un programme qui tourne chez des inconnus et détient un jeton, l'adresse du service n'est
//     pas une préférence : elle fait partie de ce qu'EST le programme.
//
// Restent dans config.json les vrais réglages : dossier, ports, miroir, démarrage auto, étiquette.
const (
	DefaultSignalURL = "wss://www.k-ok.fr/kok-rt"
	DefaultAPIURL    = "https://www.k-ok.fr/data/plugins/kok.php"
	DefaultOAuthBase = "https://www.k-ok.fr/oauth"
	// kok.php refuse toute commande dont le Referer n'est pas du même hôte (checkReferer).
	// Ce n'est PAS ce qui nous authentifie — c'est le jeton — mais son absence fait refuser la
	// requête avant tout traitement.
	DefaultReferer = "https://www.k-ok.fr/"

	// DefaultUIPort : port de la page de contrôle. Fixe, pour que le site k-ok puisse pointer
	// dessus et que l'utilisateur retrouve sa page en tapant simplement l'adresse.
	DefaultUIPort = 8383

	// DefaultOAuthPort : port de retour de l'autorisation. Distinct de l'interface pour qu'une
	// connexion reste possible même si le port de l'interface a dû être décalé.
	DefaultOAuthPort = 8384
)

// Adresses effectives du service.
//
// Une variable d'environnement permet de viser un serveur de recette. C'est DÉLIBÉRÉMENT un
// environnement et non un champ de config.json : la valeur est explicite, propre au lancement, et
// n'est jamais écrite sur disque — donc elle ne peut pas se figer en silence ni être posée à
// l'insu de l'utilisateur par quiconque sait écrire dans son dossier de configuration.
func SignalURL() string { return EnvOr("KOK_CACHE_SIGNAL_URL", DefaultSignalURL) }
func APIURL() string    { return EnvOr("KOK_CACHE_API_URL", DefaultAPIURL) }
func OAuthBase() string { return EnvOr("KOK_CACHE_OAUTH_BASE", DefaultOAuthBase) }
func Referer() string   { return EnvOr("KOK_CACHE_REFERER", DefaultReferer) }

// EnvOr est aussi utilisée par la commande pour l'adresse des mises à jour (même règle).
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Config est le contenu de config.json.
type Config struct {
	// CacheDir contient les fichiers `<songId>-<stem>.kok`. C'est le dossier que l'utilisateur
	// désigne ; il peut être celui qu'un navigateur Chromium remplissait déjà via
	// showDirectoryPicker, les noms sont identiques.
	CacheDir string `json:"cache_dir"`

	// ⛔ Pas de signal_url / api_url / oauth_base / referer ici : voir le bloc de constantes en
	// tête de fichier. Les clés présentes dans un ancien config.json sont simplement IGNORÉES à la
	// lecture (Go ignore les champs JSON inconnus) puis effacées à la réécriture — le retrait de
	// ces champs EST donc la migration du parc installé, sans toucher à aucun fichier.

	// Instance : étiquette reprise dans le journal du serveur de signalisation. Sans elle, deux
	// machines du même utilisateur y sont indiscernables. ⚠️ C'est un LIBELLÉ, pas une preuve.
	Instance string `json:"instance"`

	// UIPort : port de la page de contrôle locale.
	//
	// ⚠️ FIXE ET PRÉVISIBLE À DESSEIN. Un port éphémère change à chaque lancement : le site k-ok
	// ne peut alors pas proposer de lien « ouvrir kok-cache », et un utilisateur qui ferme
	// l'onglet n'a plus aucun moyen simple d'y revenir. C'est un frein direct à l'adoption, pour
	// un gain de sécurité nul — le port n'a jamais rien protégé (cf. `internal/ui`).
	UIPort int `json:"ui_port"`

	// OAuthPort : port sur lequel revient l'autorisation OAuth.
	//
	// ⚠️ FIXE, et pour une raison de fond : la `redirect_uri` contient le port, et le serveur la
	// compare telle quelle. Avec un port tiré au hasard, chaque connexion devait ENREGISTRER UNE
	// NOUVELLE APPLICATION — donc le serveur voyait un logiciel inconnu à chaque fois, redemandait
	// l'autorisation, et la table des clients se remplissait d'un client par connexion.
	OAuthPort int `json:"oauth_port"`

	// Mirror : synchroniser tout le catalogue au lieu de ne garder que ce qui a été écouté.
	Mirror bool `json:"mirror"`

	// MirrorIntervalMinutes : période de la vérification du manifeste en mode miroir.
	MirrorIntervalMinutes int `json:"mirror_interval_minutes"`

	ReconnectDelayMs int `json:"reconnect_delay_ms"`

	// Autostart mémorise le choix d'installation au démarrage de session — opt-in, réversible.
	Autostart bool `json:"autostart"`

	// AutoUpdate : remplacer le binaire tout seul quand une version plus récente est publiée.
	// ⛔ FAUX PAR DÉFAUT (décision du 2026-08-28) : sans ce choix explicite, le programme se borne
	// à ANNONCER la nouvelle version. Voir cmd/kok-cache/update.go pour ce qui est vérifié avant
	// qu'un octet ne soit écrit, option ou pas.
	AutoUpdate bool `json:"auto_update"`

	path string
}

// Token est le contenu de token.json (mode 0600).
type Token struct {
	ClientID     string `json:"client_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt : epoch secondes. Une marge est appliquée par le rafraîchisseur, on ne fait pas
	// confiance à l'horloge à la seconde près.
	ExpiresAt int64  `json:"expires_at"`
	Email     string `json:"email,omitempty"`
	// RedirectURI : celle avec laquelle ClientID a été enregistré. Le client n'est réutilisable
	// que si elle n'a pas changé — le serveur compare cette adresse à l'octet près.
	RedirectURI string `json:"redirect_uri,omitempty"`

	path string
}

// Dir renvoie le dossier de configuration, créé si besoin.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "kok-cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return "kok-stems"
	}
	return filepath.Join(base, "kok-cache", "stems")
}

// Load lit config.json, en créant un fichier par défaut s'il n'existe pas.
// `override` permet à l'appelant de forcer un chemin (option --config).
func Load(override string) (*Config, error) {
	path := override
	if path == "" {
		dir, err := Dir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "config.json")
	}

	cfg := &Config{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("config illisible (%s) : %w", path, err)
		}
		cfg.path = path
	case errors.Is(err, fs.ErrNotExist):
		// Premier lancement : on écrit un fichier complet plutôt que de garder les défauts en
		// mémoire — l'utilisateur doit pouvoir VOIR et modifier ce que fait le programme.
	default:
		return nil, err
	}

	cfg.applyDefaults()
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.CacheDir == "" {
		c.CacheDir = defaultCacheDir()
	}
	if c.Instance == "" {
		if h, err := os.Hostname(); err == nil {
			c.Instance = h
		}
	}
	if c.ReconnectDelayMs <= 0 {
		c.ReconnectDelayMs = 5000
	}
	if c.OAuthPort <= 0 || c.OAuthPort > 65535 {
		c.OAuthPort = DefaultOAuthPort
	}
	if c.UIPort <= 0 || c.UIPort > 65535 {
		// Hors de la plage éphémère du système (32768+), donc jamais pris au hasard par un autre
		// programme, et assez rare pour ne heurter aucun service courant.
		c.UIPort = DefaultUIPort
	}
	if c.MirrorIntervalMinutes <= 0 {
		// Une vérification quotidienne inchangée coûte 87 octets grâce à la signature du
		// manifeste ; toutes les 6 h reste négligeable et rattrape plus vite une nouveauté.
		c.MirrorIntervalMinutes = 360
	}
}

// Path renvoie le chemin du fichier de configuration effectivement utilisé.
func (c *Config) Path() string { return c.path }

func (c *Config) Save() error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeFilePrivate(c.path, raw, 0o644)
}

// LoadToken lit token.json. Un fichier absent n'est PAS une erreur : c'est l'état « pas encore
// connecté », que l'appelant traite en lançant `kok-cache login`.
func LoadToken() (*Token, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "token.json")
	tok := &Token{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return tok, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, tok); err != nil {
		return nil, fmt.Errorf("token illisible (%s) : %w", path, err)
	}
	tok.path = path
	return tok, nil
}

func (t *Token) Save() error {
	if t.path == "" {
		dir, err := Dir()
		if err != nil {
			return err
		}
		t.path = filepath.Join(dir, "token.json")
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// 0600 : le jeton vaut l'accès au compte de l'utilisateur.
	return writeFilePrivate(t.path, raw, 0o600)
}

// Forget efface les identifiants — sur le disque ET en mémoire.
//
// ⚠️ DÉFAUT RÉEL : seul le fichier était supprimé. L'objet gardait ses champs, donc
// `Configured()` répondait encore « oui » et l'interface continuait d'afficher l'écran connecté :
// le bouton « Se déconnecter » restait là sans plus rien faire, et se reconnecter imposait de
// quitter puis relancer le programme.
func (t *Token) Forget() error {
	t.AccessToken = ""
	t.RefreshToken = ""
	t.ClientID = ""
	t.RedirectURI = ""
	t.Email = ""
	t.ExpiresAt = 0
	if t.path == "" {
		return nil
	}
	err := os.Remove(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (t *Token) Configured() bool { return t.RefreshToken != "" || t.AccessToken != "" }

// writeFilePrivate écrit via un fichier temporaire puis un renommage atomique : une coupure de
// courant ne doit jamais laisser un config.json ou un token.json tronqué.
func writeFilePrivate(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op si le renommage a réussi

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
