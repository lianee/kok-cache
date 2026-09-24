//go:build linux || darwin

package ui

import (
	"errors"
	"io/fs"
	"os"
)

var errNoHome = errors.New("dossier personnel introuvable")

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
