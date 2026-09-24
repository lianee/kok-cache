package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Le verrou doit refuser une instance VIVANTE et reprendre une instance MORTE.
//
// Les deux moitiés comptent autant : sans la première, deux instances servent le même dossier ;
// sans la seconde, un simple plantage rendrait le programme définitivement impossible à relancer,
// ce qui serait pire que le mal.
//
// ⚠️ Le PID d'un processus RÉEL est indispensable. Prendre un PID au hasard ne prouverait rien, et
// PID 1 encore moins : `Signal(0)` y répond EPERM pour un utilisateur ordinaire, donc il serait vu
// comme mort.
func TestLockRefusesLiveInstanceAndRecoversDeadOne(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	lockFile := filepath.Join(dir, ".kok-cache.lock")

	// Un vrai processus, qui nous appartient — donc réellement détectable comme vivant.
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("impossible de lancer un processus témoin : %v", err)
	}
	alivePID := child.Process.Pid
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(alivePID)), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Lock(); err == nil {
		t.Fatal("verrou accordé alors qu'une instance vivante le détient")
	} else {
		var locked ErrLocked
		if !errors.As(err, &locked) {
			t.Fatalf("erreur inattendue : %v", err)
		}
		if locked.PID != alivePID {
			t.Fatalf("PID rapporté %d, attendu %d", locked.PID, alivePID)
		}
	}

	// Le processus meurt : le verrou devient reprenable.
	_ = child.Process.Kill()
	_, _ = child.Process.Wait()

	recovered, err := s.Lock()
	if err != nil {
		t.Fatalf("verrou refusé alors que le détenteur est mort : %v", err)
	}
	if !recovered {
		t.Fatal("reprise non signalée — un redémarrage après plantage doit se voir dans le journal")
	}

	// Et le fichier porte bien NOTRE pid.
	raw, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("le verrou porte %q, attendu %d", raw, os.Getpid())
	}
}

// Un verrou libre s'acquiert sans être signalé comme « repris » — sinon l'avertissement de
// redémarrage après plantage s'afficherait à chaque lancement normal et ne voudrait plus rien dire.
func TestLockOnFreeDirIsNotReportedAsRecovered(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if recovered {
		t.Fatal("un dossier vierge ne doit pas être signalé comme verrou repris")
	}
	s.Unlock()
	if _, err := os.Stat(filepath.Join(s.Dir(), ".kok-cache.lock")); !os.IsNotExist(err) {
		t.Fatal("Unlock n'a pas retiré le verrou qui nous appartenait")
	}
}

// LockHolder sert au redémarrage : le parent libère le verrou, lance son successeur, et doit
// savoir si celui-ci l'a réellement PRIS — `cmd.Start()` ne prouvant que l'engendrement.
func TestLockHolder(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := s.LockHolder(); ok {
		t.Fatal("un détenteur est signalé alors qu'aucun verrou n'existe")
	}
	if _, err := s.Lock(); err != nil {
		t.Fatal(err)
	}
	pid, ok := s.LockHolder()
	if !ok {
		t.Fatal("verrou pris mais aucun détenteur lisible")
	}
	if pid != os.Getpid() {
		t.Fatalf("détenteur %d, attendu %d", pid, os.Getpid())
	}

	// Un verrou illisible ne doit pas être pris pour un détenteur valide : c'est ce qui ferait
	// croire au parent que son successeur a démarré alors que non.
	if err := os.WriteFile(filepath.Join(s.Dir(), ".kok-cache.lock"), []byte("pas un pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LockHolder(); ok {
		t.Fatal("un verrou illisible a été accepté comme détenteur")
	}
}
