package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Cache d'empreintes PERSISTANT.
//
// ⚠️ Sans lui, le prédicat de synchronisation impose de relire et hacher TOUT le dossier à chaque
// démarrage — 42,5 Go mesurés chez le premier utilisateur, soit plusieurs minutes de disque, en
// silence. Le cache mémoire seul ne survivait pas au processus.
//
// La validité reste jugée sur (mtime, taille), comme en mémoire : un fichier remplacé est
// rehaché, jamais cru sur parole. Le fichier de cache n'est donc qu'une accélération — le perdre
// ou le corrompre ne peut pas produire un mauvais résultat, seulement un démarrage plus lent.
type persistedEntry struct {
	Hash string `json:"h"`
	Mod  int64  `json:"m"`
	Size int64  `json:"s"`
}

// LoadHashCache charge les empreintes mémorisées. Toute erreur est ignorée : au pire on rehache.
func (s *Store) LoadHashCache(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var saved map[string]persistedEntry
	if json.Unmarshal(raw, &saved) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range saved {
		if v.Hash == "" {
			continue
		}
		s.cache[k] = cacheEntry{hash: v.Hash, modTime: v.Mod, size: v.Size}
	}
}

// SaveHashCache écrit les empreintes connues, par un temporaire puis un renommage : un cache
// tronqué par une coupure serait relu au démarrage suivant.
func (s *Store) SaveHashCache(path string) error {
	s.mu.Lock()
	out := make(map[string]persistedEntry, len(s.cache))
	for k, v := range s.cache {
		out[k] = persistedEntry{Hash: v.hash, Mod: v.modTime, Size: v.size}
	}
	s.mu.Unlock()

	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Hashed compte les empreintes calculées depuis le démarrage — sert à montrer une progression
// pendant la vérification initiale d'un gros dossier (42,5 Go relus après une copie manuelle :
// plusieurs minutes, pendant lesquelles il ne doit rien se passer de silencieux).
func (s *Store) Hashed() int64 { return s.hashed.Load() }

// UnhashedCount compte les fichiers dont l'empreinte n'est pas déjà connue — donc ce qu'il faudra
// relire depuis le disque. Sert à prévenir l'utilisateur AVANT une attente de plusieurs minutes,
// plutôt que de le laisser croire à un blocage.
func (s *Store) UnhashedCount() int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		songID, stem, ok := parseName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		s.mu.Lock()
		c, known := s.cache[Key(songID, stem)]
		s.mu.Unlock()
		if !known || c.modTime != info.ModTime().UnixNano() || c.size != info.Size() {
			n++
		}
	}
	return n
}
