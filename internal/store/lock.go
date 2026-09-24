package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Verrou d'instance unique.
//
// ⚠️ Leçon payée sur le bot seeder Node : des copies de test lancées pour un essai de charge sont
// restées actives des JOURS, oubliées, avec du VIEUX code — répondant en parallèle du seeder
// officiel aux mêmes demandes et servant aléatoirement des utilisateurs avec du code cassé.
// Le risque est ici plus grand, pas moindre : `kok-cache` se lance par double-clic, peut démarrer
// tout seul à l'ouverture de session, et son utilisateur n'a aucune raison d'aller regarder la
// liste des processus.
//
// Le verrou vit dans le DOSSIER DE CACHE, pas à côté du binaire : c'est la ressource réellement
// partagée. Deux copies lancées depuis deux endroits différents se voient donc l'une l'autre.

// ErrLocked : une autre instance utilise déjà ce dossier.
type ErrLocked struct{ PID int }

func (e ErrLocked) Error() string {
	return fmt.Sprintf("une autre instance de kok-cache utilise déjà ce dossier (processus %d)", e.PID)
}

func (s *Store) lockPath() string { return filepath.Join(s.dir, ".kok-cache.lock") }

// Lock prend le verrou, ou renvoie ErrLocked. Un verrou dont le processus n'existe plus (arrêt
// brutal, coupure de courant) est repris : sans ça, un plantage rendrait le programme
// définitivement impossible à relancer, ce qui serait pire que le mal.
// Le booléen renvoyé signale un verrou REPRIS : l'instance précédente ne s'est pas arrêtée
// proprement (kill -9, coupure de courant). Ça doit se voir dans le journal — un redémarrage après
// crash n'est pas un démarrage ordinaire, et le taire priverait d'un signal gratuit.
// ⛔ L'ACQUISITION EST ATOMIQUE (`O_CREATE|O_EXCL`), et ce n'est pas un détail de style.
// La version précédente lisait le fichier puis l'écrivait : deux lancements simultanés lisaient
// tous deux « libre » et écrivaient tous deux leur PID — le dernier gagnait le fichier, mais LES
// DEUX instances continuaient sur le même dossier. Exactement ce que ce verrou existe pour
// empêcher, et le scénario est réaliste : double-clic répété, ou démarrage automatique doublé d'un
// lancement manuel à l'ouverture de session.
//
// 📌 Limite connue et assumée : l'identité repose sur le seul PID, donc un PID RECYCLÉ par un
// processus sans rapport fait croire à une instance vivante et bloque le démarrage. Le message
// nomme le processus, ce qui laisse à l'utilisateur de quoi comprendre et agir.
func (s *Store) Lock() (bool, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return false, err
	}
	path := s.lockPath()
	recovered := false

	// Deux tours au plus : le second ne sert qu'après avoir retiré un verrou mort.
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := f.WriteString(strconv.Itoa(os.Getpid()))
			cerr := f.Close()
			if werr != nil {
				return false, werr
			}
			return recovered, cerr
		}
		if !errors.Is(err, fs.ErrExist) {
			return false, err
		}

		raw, rerr := os.ReadFile(path)
		if errors.Is(rerr, fs.ErrNotExist) {
			continue // quelqu'un vient de le retirer : on retente la création atomique
		}
		if rerr != nil {
			return false, rerr
		}
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 &&
			pid != os.Getpid() && processAlive(pid) {
			return false, ErrLocked{PID: pid}
		}
		// Verrou mort, illisible, ou le nôtre : on le retire et on retente une fois.
		recovered = true
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, ErrLocked{}
}

// LockHolder renvoie le PID inscrit dans le verrou, et s'il a pu être lu.
//
// Sert au redémarrage : le parent libère le verrou puis lance son successeur, et doit savoir si
// celui-ci l'a réellement PRIS. Un processus vivant qui n'a pas pris le verrou n'a pas fini de
// démarrer — et c'est le verrou, pas l'existence du processus, qui décide qui sert le dossier.
func (s *Store) LockHolder() (int, bool) {
	raw, err := os.ReadFile(s.lockPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// Unlock ne retire le verrou que s'il nous appartient encore — sinon on effacerait celui d'une
// instance qui a légitimement repris la main après notre plantage.
func (s *Store) Unlock() {
	path := s.lockPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(raw)) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(path)
	}
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// Sur Windows, FindProcess échoue déjà si le processus n'existe pas ; le signal 0 n'y est
		// pas géré.
		return true
	}
	// Signal 0 : ne fait rien, mais échoue si le processus a disparu.
	return p.Signal(syscall.Signal(0)) == nil
}
