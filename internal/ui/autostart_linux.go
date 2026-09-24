package ui

import (
	"os"
	"path/filepath"
	"strings"
)

// Démarrage à l'ouverture de session, spécification XDG : un fichier .desktop dans
// ~/.config/autostart. Aucun privilège requis, visible et supprimable à la main — ce qui est le
// but : l'utilisateur doit toujours pouvoir défaire ce qu'il a activé.

func autostartPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "autostart", "kok-cache.desktop")
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
		return removeIfExists(p)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	icon := installIcon() // vide si l'installation échoue : un raccourci sans icône reste valide
	entry := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=kok-cache\n" +
		"Comment=Cache local des extraits k-ok\n" +
		"Exec=" + quoteDesktopExec(exe) + " run\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
	if icon != "" {
		entry += "Icon=" + icon + "\n"
	}
	return os.WriteFile(p, []byte(entry), 0o644)
}

// quoteDesktopExec rend un chemin utilisable dans la clé `Exec` d'un fichier .desktop.
//
// ⛔ La spécification Desktop Entry SÉPARE LES ARGUMENTS PAR DES ESPACES. Un exécutable rangé dans
// « /home/moi/Mes Applis/kok-cache » était donc lu comme la commande « /home/moi/Mes » suivie de
// deux arguments : le démarrage automatique échouait, en silence, alors que le fichier existe et
// que l'interface l'annonce activé.
//
// ⭐ Le défaut était CONNU : le commentaire de la version Windows explique précisément pourquoi il
// faut des guillemets autour du chemin. Il n'avait été traité que là, sur trois plateformes.
//
// La spécification demande des guillemets doubles, et à l'intérieur un antislash devant `"`,
// '`', `$` et `\` lui-même.
func quoteDesktopExec(path string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range path {
		switch r {
		case '"', '`', '$', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// installIcon dépose l'icône dans le thème de l'utilisateur et renvoie le nom à utiliser.
//
// Sous Linux, un exécutable ne porte pas d'icône : elle vient du fichier .desktop, qui désigne un
// nom résolu dans les thèmes installés. On écrit donc le PNG embarqué dans le dossier utilisateur —
// aucun privilège requis, et rien en dehors de son propre profil.
func installIcon() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := assets.ReadFile("assets/icon.png")
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".local", "share", "icons", "hicolor", "256x256", "apps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	if err := os.WriteFile(filepath.Join(dir, "kok-cache.png"), raw, 0o644); err != nil {
		return ""
	}
	return "kok-cache"
}
