package ui

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
)

// Démarrage de session sur macOS : un LaunchAgent dans ~/Library/LaunchAgents. Agent utilisateur,
// pas démon système — aucun privilège d'administration, et l'utilisateur peut le retirer seul.

const launchAgentLabel = "fr.k-ok.kok-cache"

func autostartPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func autostartEnabled() bool {
	p := autostartPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func setAutostart(on bool) error {
	p := autostartPath()
	if p == "" {
		return errNoHome
	}
	if !on {
		// Déchargé d'abord : sans ça, l'agent reste actif jusqu'à la prochaine ouverture de
		// session alors que l'interface annonce le contraire.
		_ = exec.Command("launchctl", "unload", p).Run()
		return removeIfExists(p)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// ⛔ Le chemin est ÉCHAPPÉ. Un plist est du XML : un exécutable rangé dans un dossier nommé
	// « Photos & Vidéos » — un & suffit — produisait un document malformé, que launchd refuse.
	// Panne parfaitement muette : l'interface annonce le démarrage automatique activé, le fichier
	// existe (donc `autostartEnabled()` répond oui), et rien ne se lance à l'ouverture de session.
	// D'autant plus invisible que macOS est la seule plateforme jamais exécutée.
	var quoted bytes.Buffer
	if err := xml.EscapeText(&quoted, []byte(exe)); err != nil {
		return err
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchAgentLabel + `</string>
  <key>ProgramArguments</key>
  <array><string>` + quoted.String() + `</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
`
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "load", p).Run()
	return nil
}
