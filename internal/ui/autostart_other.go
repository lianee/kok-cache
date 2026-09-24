//go:build !linux && !darwin && !windows

package ui

import "errors"

// Systèmes hors des cibles publiées : on ne prétend pas savoir installer un démarrage de session.
// Refuser explicitement vaut mieux qu'une case à cocher qui ne fait rien.

func autostartEnabled() bool { return false }

func setAutostart(bool) error {
	return errors.New("le démarrage automatique n'est pas géré sur ce système")
}
