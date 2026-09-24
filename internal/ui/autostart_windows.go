package ui

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// Démarrage de session sur Windows : une valeur sous HKEY_CURRENT_USER\…\Run.
//
// Pourquoi la base de registre et pas un raccourci dans le dossier Démarrage : un .lnk est un
// format binaire qui s'écrit via COM, donc cgo ou une bibliothèque tierce. `x/sys/windows` est du
// Go pur, déjà présent par pion, et la valeur reste visible et supprimable par l'utilisateur
// (Gestionnaire des tâches → Démarrage).

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "kok-cache"

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValueName)
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if !on {
		err := k.DeleteValue(runValueName)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Guillemets : sans eux, un chemin contenant une espace (« Program Files », un nom
	// d'utilisateur avec une espace) est coupé au premier blanc et le démarrage échoue en silence.
	return k.SetStringValue(runValueName, `"`+exe+`" run`)
}
