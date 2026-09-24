package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lianee/kok-cache/internal/config"
	"github.com/lianee/kok-cache/internal/ui"
)

// Mise à jour du binaire, sur le modèle de sw3-proxy, avec une différence de principe :
// ici c'est une OPTION, désactivée par défaut (décision du 2026-08-28). Sans elle, le programme
// se contente d'annoncer qu'une version plus récente existe, et l'utilisateur décide : soit il
// clique « Mettre à jour maintenant » dans la page, soit il va chercher la release lui-même.
//
// ⛔ Un mécanisme de mise à jour est un canal d'exécution de code à distance vers chaque machine
// où le programme tourne : qui contrôle l'adresse de téléchargement les contrôle toutes. C'est le
// seul point du projet où « ce n'est pas une application bancaire » ne s'applique pas. D'où :
//
//   - le manifeste (latest.json) est SIGNÉ en Ed25519 avec une clé qui ne vit ni dans le dépôt
//     ni dans la CI, seulement sur la machine de l'éditeur ; la clé publique est compilée ici ;
//   - RIEN n'est écrit sur le disque avant que la signature du manifeste ET l'empreinte SHA-256
//     du binaire téléchargé aient été vérifiées ; TLS et GitHub sont un plus, pas la confiance ;
//   - on ne descend jamais de version (monotonie), et une version dont le successeur n'a pas
//     démarré n'est pas retentée ;
//   - les empreintes du manifeste sont celles de la release attestée par GitHub Actions : un tiers
//     peut vérifier que ce que l'éditeur a signé est bien ce que la CI publique a construit.
//
// Déroulement : manifeste → plus récent ? → binaire de cette plateforme → SHA-256 → écriture à
// côté de l'exécutable → renommage de l'actuel en .old, du nouveau à sa place → lancement du
// successeur → preuve qu'il a pris le verrou d'instance → sortie. Tout échec avant le lancement
// laisse le binaire en cours intact ; un successeur qui ne démarre pas est tué et l'ancien binaire
// est remis en place.

const (
	// DefaultUpdateURL : les releases GitHub, où vit aussi l'attestation de provenance. Les
	// binaires y ont un nom STABLE (kok-cache-<os>-<arch>), c'est ce qui rend « latest » possible.
	// ⛔ En dur, comme les autres adresses (voir config.go) : un jour de changement de domaine,
	// c'est la mise à jour qui corrige le parc, elle ne peut donc pas dépendre d'un fichier local.
	DefaultUpdateURL = "https://github.com/lianee/kok-cache/releases/latest/download"
	// ReleasePageURL est proposée à l'utilisateur quand il préfère télécharger lui-même.
	ReleasePageURL = "https://github.com/lianee/kok-cache/releases/latest"

	updateInterval = 6 * time.Hour
)

// updatePublicHex : clé publique Ed25519 de l'éditeur (32 octets, hex). La clé privée
// correspondante est dans le magasin local de l'éditeur, jamais dans le dépôt ni dans la CI.
// Rotation : publier une release signée par l'ANCIENNE clé qui embarque la NOUVELLE clé
// publique, puis signer les suivantes avec la nouvelle.
//
// Variable et non constante pour qu'un banc d'essai puisse la remplacer à la compilation
// (`-ldflags -X main.updatePublicHex=…`) : c'est ainsi que la chaîne complète est testée avec
// une clé jetable, sans toucher à la vraie. La release, elle, prend la valeur écrite ici.
var updatePublicHex = "5a97614e11be99c6657137b7d5611ff79e37f027669cd4474159b75606d42a69"

// updateURL permet de viser un serveur d'essai, par environnement et jamais par config.json,
// pour la même raison que les autres adresses : ce fichier ne doit pas pouvoir détourner le
// programme vers un autre éditeur. Sans la clé privée, un autre serveur ne peut de toute façon
// rien faire installer.
func updateURL() string { return config.EnvOr("KOK_CACHE_UPDATE_URL", DefaultUpdateURL) }

// updatePublicKey est nil quand aucune clé n'est compilée : alors aucune mise à jour n'est
// possible, ni automatique ni manuelle, et la page le dit. Les tests substituent leur propre clé.
var updatePublicKey = func() ed25519.PublicKey {
	if updatePublicHex == "" {
		return nil
	}
	b, err := hex.DecodeString(updatePublicHex)
	if err != nil || len(b) != ed25519.PublicKeySize {
		panic("clé publique de mise à jour compilée illisible")
	}
	return ed25519.PublicKey(b)
}()

type manifestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Version string `json:"version"`
	Date    string `json:"date"`
	// MinVersion : plancher décidé côté éditeur. En dessous, la page affiche « mise à jour
	// requise » même si l'option automatique est désactivée. Ceinture et bretelles pour le jour où
	// une version en circulation ne doit plus tourner (protocole cassé, faille).
	MinVersion string                  `json:"min_version,omitempty"`
	Files      map[string]manifestFile `json:"files"`
}

// platformKey identifie le binaire de cette machine dans le manifeste : « linux-amd64 »,
// « darwin-arm64 », « windows-amd64 ». Même clé que les noms d'artefacts de build.sh.
func platformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// parsedVersion : « v1.2.3-rc1 » → {1,2,3} + « rc1 ». Ce que produit `git describe` sur un tag.
type parsedVersion struct {
	n   [3]int
	pre string
}

// parseVersion refuse tout ce qui n'est pas une version publiée : « dev », un hash court
// (`git describe --always` sans tag), une chaîne « -dirty ». Un binaire sans version publiée ne
// se met jamais à jour, il ne saurait pas dire s'il régresse.
func parseVersion(v string) (parsedVersion, bool) {
	var out parsedVersion
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, _ := strings.Cut(v, "-")
	if strings.Contains(pre, "dirty") || strings.Contains(pre, "-g") {
		return out, false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out.n[i] = n
	}
	out.pre = pre
	return out, true
}

// versionLess : a < b au sens de semver (une pré-version est inférieure à la version finale de
// même numéro). Faux si l'une des deux n'est pas une version publiée.
func versionLess(a, b string) bool {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return false
	}
	for i := range pa.n {
		if pa.n[i] != pb.n[i] {
			return pa.n[i] < pb.n[i]
		}
	}
	switch {
	case pa.pre == pb.pre:
		return false
	case pb.pre == "":
		return true
	case pa.pre == "":
		return false
	}
	return pa.pre < pb.pre
}

// verifyManifest vérifie la signature détachée puis décode le manifeste. Dans cet ordre : rien
// n'est interprété avant d'être authentifié.
func verifyManifest(pub ed25519.PublicKey, data, sig []byte) (*manifest, error) {
	if pub == nil {
		return nil, errors.New("aucune clé publique compilée dans ce binaire")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, errors.New("signature illisible")
	}
	if !ed25519.Verify(pub, data, raw) {
		return nil, errors.New("signature du manifeste invalide")
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifeste illisible : %v", err)
	}
	if _, ok := parseVersion(m.Version); !ok {
		return nil, fmt.Errorf("manifeste sans version publiée (%q)", m.Version)
	}
	return &m, nil
}

func fetchManifest(ctx context.Context, base string, pub ed25519.PublicKey) (*manifest, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	get := func(u string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s : HTTP %d", u, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	}
	// Anti-cache : un intermédiaire qui resservirait un vieux manifeste cacherait une release.
	bust := "?_=" + strconv.FormatInt(time.Now().Unix(), 10)
	data, err := get(base + "/latest.json" + bust)
	if err != nil {
		return nil, err
	}
	sig, err := get(base + "/latest.json.sig" + bust)
	if err != nil {
		return nil, err
	}
	return verifyManifest(pub, data, sig)
}

// downloadBinary récupère le binaire de cette plateforme et le confronte au manifeste signé.
func downloadBinary(ctx context.Context, base string, m *manifest) ([]byte, error) {
	f, ok := m.Files[platformKey()]
	if !ok || f.Name == "" || f.SHA256 == "" {
		return nil, fmt.Errorf("pas de binaire pour %s dans le manifeste", platformKey())
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/"+f.Name+"?v="+m.Version, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s : HTTP %d", f.Name, resp.StatusCode)
	}
	// Plafond : une valeur venue du réseau ne dicte jamais une allocation sans borne.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), f.SHA256) {
		return nil, fmt.Errorf("empreinte SHA-256 inattendue pour %s", f.Name)
	}
	return data, nil
}

// swapExecutable installe data à la place de exe. Renvoie une fonction qui remet l'ancien
// binaire (utilisée si le successeur ne démarre pas). Le processus en cours continue de tourner
// depuis le fichier renommé : renommer un exécutable en cours d'exécution est permis sur les
// trois systèmes, le supprimer ne l'est pas sous Windows, d'où .old plutôt qu'une suppression.
//
// Sur macOS, exe est `Contents/MacOS/kok-cache-bin` dans le bundle : c'est lui qu'on remplace, le
// petit lanceur `kok-cache` du bundle reste en place. Écrit par le programme et non par un
// navigateur, le nouveau binaire ne porte pas l'attribut de quarantaine.
func swapExecutable(exe string, data []byte) (restore func(), err error) {
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return nil, err
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(old, exe)
		_ = os.Remove(tmp)
		return nil, err
	}
	return func() {
		_ = os.Remove(exe)
		_ = os.Rename(old, exe)
	}, nil
}

// cleanupOldBinary retire la génération précédente laissée par une mise à jour. L'échec est
// ignoré (Windows garde le fichier verrouillé tant que l'ancien processus n'est pas tout à fait
// parti) : le démarrage suivant réessaiera.
func cleanupOldBinary(exe string) { _ = os.Remove(exe + ".old") }

// Updater vérifie le manifeste, et installe si l'option est active ou si l'utilisateur le demande.
type Updater struct {
	exe string
	log *slog.Logger
	pub ed25519.PublicKey
	// handOver lance le successeur et prouve qu'il sert ; en cas de succès, le processus courant
	// s'arrête de lui-même (contexte principal annulé). Fourni par doRun, qui seul tient le verrou.
	handOver func(extraArgs []string) error
	// auto est la copie vivante de `auto_update` : config.Config n'a pas de verrou, l'interface
	// écrit le fichier sous le sien et nous tient au courant par SetAuto.
	auto atomic.Bool

	mu            sync.Mutex
	available     *manifest
	required      bool
	updating      bool
	updatedFrom   string
	failedVersion string
	lastErr       string
}

func newUpdater(auto bool, exe, updatedFrom string, log *slog.Logger, handOver func([]string) error) *Updater {
	u := &Updater{exe: exe, log: log, pub: updatePublicKey, handOver: handOver, updatedFrom: updatedFrom}
	u.auto.Store(auto)
	return u
}

func (u *Updater) State() ui.UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := ui.UpdateState{Enabled: u.pub != nil, Auto: u.auto.Load(), Required: u.required,
		Updating: u.updating, UpdatedFrom: u.updatedFrom, Err: u.lastErr, ReleaseURL: ReleasePageURL}
	if u.available != nil {
		st.Available = u.available.Version
	}
	return st
}

// SetAuto prend acte de l'option (l'interface a déjà écrit config.json). Activer déclenche une
// vérification immédiate : quelqu'un qui coche la case pendant qu'une version est annoncée
// attend qu'il se passe quelque chose.
func (u *Updater) SetAuto(ctx context.Context, on bool) {
	u.auto.Store(on)
	if on && u.pub != nil {
		go u.check(ctx, true)
	}
}

// UpdateNow installe la version annoncée sur demande explicite, que l'option soit active ou non.
// Même chemin, mêmes vérifications : la demande de l'utilisateur ne dispense de rien.
func (u *Updater) UpdateNow(ctx context.Context) error {
	if u.pub == nil {
		return errors.New("ce binaire ne contient pas de clé de vérification : téléchargez la nouvelle version vous-même")
	}
	go u.check(ctx, true)
	return nil
}

// Run vérifie au démarrage puis toutes les 6 h. Sans clé compilée ou sans version publiée
// (build locale), il ne fait rien du tout, et le dit une fois.
func (u *Updater) Run(ctx context.Context) {
	if u.pub == nil {
		u.log.Info("mise à jour : aucune clé publique compilée, vérification désactivée")
		return
	}
	if _, ok := parseVersion(version); !ok {
		u.log.Info("mise à jour : binaire sans version publiée, vérification désactivée", "version", version)
		return
	}
	t := time.NewTicker(updateInterval)
	defer t.Stop()
	for {
		u.check(ctx, u.auto.Load())
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

// check lit le manifeste ; installe seulement si `install` est vrai. Un seul passage à la fois :
// un clic « Mettre à jour » pendant la vérification périodique ne lance pas deux téléchargements.
func (u *Updater) check(ctx context.Context, install bool) {
	u.mu.Lock()
	if u.updating {
		u.mu.Unlock()
		return
	}
	u.updating = install
	u.mu.Unlock()
	done := func(err string) {
		u.mu.Lock()
		u.updating = false
		u.lastErr = err
		u.mu.Unlock()
	}

	m, err := fetchManifest(ctx, updateURL(), u.pub)
	if err != nil {
		u.log.Warn("mise à jour : manifeste indisponible", "err", err)
		done("")
		return
	}
	newer := versionLess(version, m.Version)
	u.mu.Lock()
	u.required = m.MinVersion != "" && versionLess(version, m.MinVersion)
	if newer {
		u.available = m
	} else {
		u.available = nil
	}
	failed := m.Version == u.failedVersion
	u.mu.Unlock()

	if !newer {
		done("")
		return
	}
	if !install {
		u.log.Info("mise à jour disponible (option désactivée, rien n'est installé)", "version", m.Version)
		done("")
		return
	}
	if failed {
		// Déjà installée une fois sans que le successeur démarre : on ne coupe pas le service
		// toutes les 6 h pour la même release cassée. Une plus récente remet les compteurs à zéro.
		done("la version " + m.Version + " n'a pas démarré lors d'un essai précédent")
		return
	}
	u.log.Info("mise à jour : téléchargement", "de", version, "vers", m.Version)
	data, err := downloadBinary(ctx, updateURL(), m)
	if err != nil {
		u.log.Warn("mise à jour : téléchargement refusé", "err", err)
		done(err.Error())
		return
	}
	restore, err := swapExecutable(u.exe, data)
	if err != nil {
		u.log.Warn("mise à jour : échange du binaire impossible", "err", err)
		done(err.Error())
		return
	}
	u.log.Info("mise à jour installée, passage de relais", "version", m.Version)
	if err := u.handOver([]string{"--updated-from=" + version}); err != nil {
		u.log.Error("mise à jour : la nouvelle version n'a pas démarré, retour à l'ancienne", "err", err)
		restore()
		u.mu.Lock()
		u.failedVersion = m.Version
		u.mu.Unlock()
		done(err.Error())
		return
	}
	// Succès : le successeur tient le verrou et le contexte principal est annulé, ce processus
	// s'arrête. `updating` reste vrai jusqu'au bout, la page sait qu'elle va perdre le contact.
}
