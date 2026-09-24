package auth

import (
	"sync"
	"testing"

	"github.com/lianee/kok-cache/internal/config"
)

// COURSE DE DONNÉES trouvée à l'audit du 2026-08-31, à lancer avec `-race`.
//
// `config.Token` n'a AUCUN verrou. Ses champs sont écrits par la connexion, le rafraîchissement et
// l'oubli, pendant que DEUX lecteurs tournent en permanence dans d'autres goroutines :
//   - `superviseServices` (cmd/kok-cache) toutes les 2 s,
//   - `handleState` de l'interface, à chaque sondage de la page (1,5 s).
//
// Tous deux appelaient `Token.Configured()` en direct, sans passer par le mutex de `auth.Manager`
// qui protège pourtant les écritures. Et `Logout` appelait `tok.Forget()` APRÈS avoir relâché ce
// mutex : l'effacement lui-même se faisait hors protection.
//
// ⚠️ CE TEST NE PROUVE RIEN SANS `-race`. Une course sur un en-tête de chaîne ne se manifeste
// presque jamais en exécution ordinaire — c'est justement ce qui la rend dangereuse. Le détecteur,
// lui, la voit dès le premier accès concurrent.
//
// CONTRÔLE NÉGATIF, pour vérifier que ce test sait échouer : remplacer le corps de
// `Manager.Configured()` par un accès direct sans verrou —
//
//	func (m *Manager) Configured() bool { return m.tok != nil && m.tok.Configured() }
//
// puis relancer avec `-race` : le détecteur signale « WARNING: DATA RACE » entre cette lecture et
// l'écriture de `Forget`. Vérifié le 2026-08-31.
func TestConfiguredAndForgetAreRaceFree(t *testing.T) {
	tok := &config.Token{AccessToken: "a", RefreshToken: "r"}
	m := NewManager("https://example.invalid/oauth", "test", tok)

	const rounds = 200
	var wg sync.WaitGroup

	// Écrivain : c'est ce que fait `Logout` en fin de parcours.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = m.Forget()
		}
	}()

	// Deux lecteurs, à l'image du superviseur de services et de `handleState`.
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				_ = m.Configured()
			}
		}()
	}

	wg.Wait()

	// Après tous ces oublis, l'état doit être cohérent : aucun compte connecté.
	if m.Configured() {
		t.Fatal("un compte est encore signalé comme connecté après Forget()")
	}
	if m.Email() != "" {
		t.Fatal("l'adresse e-mail a survécu à Forget()")
	}
}

// Email() est lu par `doStatus` et par l'interface ; il touche les mêmes champs.
func TestEmailIsRaceFree(t *testing.T) {
	m := NewManager("https://example.invalid/oauth", "test",
		&config.Token{AccessToken: "a", Email: "qui@example.invalid"})

	var wg sync.WaitGroup
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = m.Email()
				_ = m.Configured()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = m.Forget()
		}
	}()
	wg.Wait()
}
