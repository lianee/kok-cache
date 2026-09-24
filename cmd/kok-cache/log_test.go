package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Le binaire Windows n'a pas de console (-H=windowsgui) : os.Stderr y est un handle invalide et
// chaque écriture échoue. Le journal doit atteindre le FICHIER malgré tout — c'est même sa raison
// d'être. Reproduit ici avec un descripteur invalide à la place de stderr. Avec l'ancien
// io.MultiWriter(os.Stderr, fichier), qui s'arrête à la première erreur, le fichier restait vide
// (constaté sous Windows le 2026-09-21 : kok-cache.log à 0 octet, instance en marche).
func TestLogReachesFileWhenStderrIsBroken(t *testing.T) {
	saved := os.Stderr
	os.Stderr = os.NewFile(^uintptr(0), "stderr-invalide")
	defer func() { os.Stderr = saved }()

	path := filepath.Join(t.TempDir(), "kok-cache.log")
	log, closer := newLogger(slog.LevelInfo, path)
	log.Info("kok-cache démarre", "version", "test")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("journal vide : l'échec de stderr a bloqué l'écriture dans le fichier")
	}
}
