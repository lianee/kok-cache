// Package store gère les fichiers de stems sur le disque local.
//
// Trois exigences y sont concentrées, spécifiées avant la première ligne de code :
//
//  1. PRÉDICAT DE SYNCHRONISATION = « absent OU hash différent ».
//     « manquant » est un prédicat FAUX, et silencieusement : un re-split change le sel et
//     remplace intégralement le ciphertext, donc le fichier n'est pas absent, il est PÉRIMÉ. Un
//     miroir qui ne cherche que les absents ne le rafraîchit jamais — et comme il revérifie son
//     hash avant de répondre, il se tait définitivement sur cette chanson. Perte de couverture
//     invisible.
//
//  2. ÉCRITURE TEMPORAIRE + RENOMMAGE ATOMIQUE APRÈS VÉRIFICATION DU HASH.
//     Un fichier en cours de téléchargement ne doit JAMAIS être annoncé. Sans ça, un cache en
//     pleine synchro répond « oui j'ai ce stem », sert un fichier tronqué, et le receveur le
//     rejette au hash : coût réseau pur, et un pair qui a l'air défaillant.
//
//  3. SILENCE — JAMAIS SUPPRESSION — SUR DÉSACCORD DE HASH.
//     Le hash attendu vient du DEMANDEUR. Supprimer sur sa parole donnerait à n'importe quel pair
//     une primitive d'effacement à distance : diffuser des `stem-avail` aux hashes bidons sur tout
//     le catalogue viderait le cache de tout l'essaim, à coût quasi nul, contre des machines
//     d'inconnus. De plus un désaccord n'implique pas que le fichier local est périmé : un
//     navigateur au catalogue daté demande avec l'ancien hash, et c'est LUI qui a tort.
//     L'autorité sur la péremption, c'est le manifeste du serveur — jamais un pair.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Les deux seuls stems d'une chanson. L'ordre est celui du manifeste : [accompaniment, vocals].
const (
	StemAccompaniment = "accompaniment"
	StemVocals        = "vocals"
)

// Stems est l'ordre canonique, identique à celui des paires du manifeste.
var Stems = [2]string{StemAccompaniment, StemVocals}

// ValidStem évite qu'un nom venu du réseau ne se transforme en chemin de fichier arbitraire.
func ValidStem(stem string) bool {
	return stem == StemAccompaniment || stem == StemVocals
}

// ErrHashMismatch : les octets reçus ne correspondent pas au hash attendu. Aucun fichier n'a été
// écrit, aucun fichier existant n'a été touché.
var ErrHashMismatch = errors.New("hash mismatch")

type cacheEntry struct {
	hash    string
	modTime int64
	size    int64
}

// Store est l'accès concurrent au dossier de stems.
type Store struct {
	dir string

	mu    sync.Mutex
	cache map[string]cacheEntry // clé = "<songId>-<stem>"

	// hashed : nombre d'empreintes réellement CALCULÉES (pas servies par le cache) depuis le
	// démarrage. Alimente l'affichage de progression.
	hashed atomic.Int64
}

func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: dossier vide")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{dir: dir, cache: make(map[string]cacheEntry)}, nil
}

func (s *Store) Dir() string { return s.dir }

// Key est l'identifiant interne d'un stem. Le fil P2P, lui, identifie toujours un stem par
// songId + stem, jamais par un nom de fichier : le nommage reste un détail de stockage local.
func Key(songID int, stem string) string {
	return strconv.Itoa(songID) + "-" + stem
}

// Path renvoie le chemin du fichier, ou "" si le stem n'est pas l'un des deux valides.
//
// `.kok` = ciphertext AES-256-CTR, pas de l'audio jouable — l'ancienne extension `.m4a` était
// trompeuse pour des stems chiffrés au repos.
//
// ⛔ LA VALIDATION EST ICI, au point de passage obligé, et pas seulement chez les appelants.
// `filepath.Join` NETTOIE les `..` : un stem de la forme `x/../../../etc/foo` sortait du dossier de
// cache. Les trois entrées réseau appelaient bien `ValidStem` avant, donc rien n'était exploitable
// — mais la sûreté reposait sur trois appelants qui devaient y penser chacun, sans garde commun.
// C'est la forme exacte du défaut qui a déjà mordu ce projet (« mon balayage ne couvrait que
// 3 fichiers »). Un quatrième appelant, un jour, n'aurait rien eu pour le retenir.
//
// Renvoyer "" est sûr pour tous les appelants : `os.Stat("")`, `os.Open("")` et `os.ReadFile("")`
// échouent tous, donc `Has` répond false, `Hash` renvoie "", `Size` renvoie 0 et `ReadAll` renvoie
// une erreur — c'est-à-dire exactement « ce stem n'existe pas ».
func (s *Store) Path(songID int, stem string) string {
	if !ValidStem(stem) {
		return ""
	}
	return filepath.Join(s.dir, Key(songID, stem)+".kok")
}

// Has ne dit que « le fichier existe », jamais « il est à jour » — cf. le prédicat de
// synchronisation, qui est la seule question qui compte pour un miroir.
func (s *Store) Has(songID int, stem string) bool {
	st, err := os.Stat(s.Path(songID, stem))
	return err == nil && st.Mode().IsRegular()
}

// Size renvoie la taille du fichier, ou 0 s'il est absent.
func (s *Store) Size(songID int, stem string) int64 {
	st, err := os.Stat(s.Path(songID, stem))
	if err != nil {
		return 0
	}
	return st.Size()
}

// Hash calcule le SHA-256 du fichier, avec un cache indexé sur (mtime, taille).
// Renvoie "" si le fichier est absent ou illisible.
//
// Le cache est indispensable : `stem-avail` arrive plusieurs fois par seconde pendant une partie
// et relire 8,5 Mo à chaque question rendrait le cache plus lent que l'essaim. Il est invalidé par
// mtime ET taille, pas par mtime seul : un remplacement peut préserver l'horodatage à la
// granularité du système de fichiers.
func (s *Store) Hash(songID int, stem string) string {
	path := s.Path(songID, stem)
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	key := Key(songID, stem)
	mod, size := st.ModTime().UnixNano(), st.Size()

	s.mu.Lock()
	if c, ok := s.cache[key]; ok && c.modTime == mod && c.size == size {
		s.mu.Unlock()
		return c.hash
	}
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.hashed.Add(1)

	s.mu.Lock()
	s.cache[key] = cacheEntry{hash: sum, modTime: mod, size: size}
	s.mu.Unlock()
	return sum
}

// NeedsSync est LE prédicat de synchronisation : « absent OU hash différent ».
//
// wantHash vient du MANIFESTE (l'autorité), jamais d'un pair. Un wantHash vide veut dire « le
// serveur ne publie pas de hash pour ce stem » : on ne peut alors ni vérifier ni décider, donc on
// ne synchronise pas — le manifeste exclut déjà les chansons incomplètes (« une chanson, c'est
// deux stems ou rien »).
func (s *Store) NeedsSync(songID int, stem, wantHash string) bool {
	if wantHash == "" {
		return false
	}
	if !s.Has(songID, stem) {
		return true
	}
	return !strings.EqualFold(s.Hash(songID, stem), wantHash)
}

// Serves répond à « puis-je servir ce stem à un pair qui l'annonce avec ce hash ? ».
//
// ⛔ Un désaccord renvoie false et RIEN D'AUTRE : pas de suppression, pas de mise en quarantaine,
// pas de marquage. Le silence est le comportement correct (cf. l'en-tête du paquet). Un
// wantHash vide (pair qui ne précise rien) est accepté : c'est le comportement historique du
// seeder, et le receveur vérifie de toute façon ce qu'il reçoit.
func (s *Store) Serves(songID int, stem, wantHash string) bool {
	if !s.Has(songID, stem) {
		return false
	}
	if wantHash == "" {
		return true
	}
	return strings.EqualFold(s.Hash(songID, stem), wantHash)
}

// ReadAll charge le fichier entier en mémoire (~8,5 Mo). C'est ce que fait déjà le seeder Node :
// une session sert typiquement plusieurs plages du même fichier, et le relire par morceaux
// coûterait plus cher que de le garder le temps du transfert.
func (s *Store) ReadAll(songID int, stem string) ([]byte, error) {
	return os.ReadFile(s.Path(songID, stem))
}

// Commit écrit les octets reçus SI et SEULEMENT SI leur hash correspond à `wantHash`, via un
// fichier temporaire du même dossier puis un renommage atomique.
//
// Ordre imposé, et il est la moitié de l'exigence : le hash est vérifié AVANT que quoi que ce soit
// n'apparaisse sous le nom définitif. Un fichier visible est donc toujours un fichier vérifié —
// c'est ce qui permet de répondre à `stem-avail` pendant une synchronisation sans jamais annoncer
// un fichier tronqué.
//
// Le temporaire vit dans le dossier de destination (un renommage n'est atomique qu'à l'intérieur
// d'un même système de fichiers) et porte un préfixe `.` + `.part` : il est ignoré par le
// balayage, et reconnaissable si un plantage en laisse traîner.
func (s *Store) Commit(songID int, stem string, data []byte, wantHash string) error {
	if !ValidStem(stem) {
		return fmt.Errorf("store: stem inconnu %q", stem)
	}
	if wantHash == "" {
		// Refus délibéré : écrire sans hash de référence, c'est accepter n'importe quels octets
		// d'un pair quelconque comme contenu du cache de l'utilisateur.
		return errors.New("store: écriture refusée sans hash attendu")
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, wantHash) {
		return fmt.Errorf("%w: attendu %s, obtenu %s", ErrHashMismatch, short(wantHash), short(got))
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+Key(songID, stem)+".*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op après un renommage réussi

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// fsync avant le renommage : sans lui, un arrêt brutal peut laisser un fichier au bon NOM avec
	// un contenu incomplet — exactement l'état que tout ce mécanisme existe pour rendre impossible.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.Path(songID, stem)); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.cache, Key(songID, stem))
	s.mu.Unlock()
	return nil
}

// Remove efface un stem du disque et oublie son empreinte. Réservé au miroir, qui ne l'appelle
// que sur la parole du MANIFESTE pour un fichier périmé que rien ne remplacera (mode à la demande) ;
// jamais sur l'indice d'un pair. Un fichier absent n'est pas une erreur.
func (s *Store) Remove(songID int, stem string) error {
	if !ValidStem(stem) {
		return fmt.Errorf("store: stem inconnu %q", stem)
	}
	if err := os.Remove(s.Path(songID, stem)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	delete(s.cache, Key(songID, stem))
	s.mu.Unlock()
	return nil
}

// Inventory est l'état du disque : clé = songId, valeur = paire [accompaniment, vocals] de hashes
// (chaîne vide = fichier absent).
type Inventory map[int][2]string

// Scan lit le dossier et calcule les hashes des fichiers présents. Coûteux au premier appel
// (relit tout le dossier), quasi gratuit ensuite grâce au cache.
func (s *Store) Scan() (Inventory, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Inventory{}, nil
		}
		return nil, err
	}
	inv := Inventory{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		songID, stem, ok := parseName(e.Name())
		if !ok {
			continue
		}
		pair := inv[songID]
		idx := 0
		if stem == StemVocals {
			idx = 1
		}
		pair[idx] = s.Hash(songID, stem)
		inv[songID] = pair
	}
	return inv, nil
}

// parseName reconnaît `<songId>-<stem>.kok` et ignore tout le reste — dont les temporaires
// `.<clé>.*.part`, qui ne doivent jamais être vus comme du contenu disponible.
func parseName(name string) (int, string, bool) {
	if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".kok") {
		return 0, "", false
	}
	base := strings.TrimSuffix(name, ".kok")
	dash := strings.IndexByte(base, '-')
	if dash <= 0 {
		return 0, "", false
	}
	songID, err := strconv.Atoi(base[:dash])
	if err != nil || songID <= 0 {
		return 0, "", false
	}
	stem := base[dash+1:]
	if !ValidStem(stem) {
		return 0, "", false
	}
	return songID, stem, true
}

// SweepTemp retire les temporaires abandonnés par un arrêt brutal. Ils sont inertes (jamais
// annoncés, jamais servis), mais occupent le disque.
func (s *Store) SweepTemp() int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if os.Remove(filepath.Join(s.dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// DiskUsage renvoie le nombre de fichiers et le total d'octets.
func (s *Store) DiskUsage() (files int, bytes int64) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, _, ok := parseName(e.Name()); !ok {
			continue
		}
		if info, err := e.Info(); err == nil {
			files++
			bytes += info.Size()
		}
	}
	return files, bytes
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}
