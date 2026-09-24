package ui

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// pickDirectory ouvre la boîte de dialogue de choix de dossier du système.
//
// Sans cgo, donc sans lier la moindre bibliothèque graphique : on lance l'utilitaire que le
// système fournit déjà. Renvoie "" si l'utilisateur annule — une annulation n'est pas une erreur.
func pickDirectory(current string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		script := `POSIX path of (choose folder with prompt "Dossier des extraits k-ok")`
		return runPicker(exec.Command("osascript", "-e", script))

	case "windows":
		// -sta est OBLIGATOIRE : FolderBrowserDialog est un composant COM à cloisonnement
		// monothread, et PowerShell démarre en MTA. Sans ce drapeau la fenêtre ne s'ouvre pas.
		ps := `Add-Type -AssemblyName System.Windows.Forms; ` +
			`$d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
			`$d.Description = 'Dossier des extraits k-ok'; ` +
			`if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath }`
		return runPicker(exec.Command("powershell", "-NoProfile", "-sta", "-Command", ps))

	default:
		if path, err := exec.LookPath("zenity"); err == nil {
			args := []string{"--file-selection", "--directory", "--title=Dossier des extraits k-ok"}
			if current != "" {
				args = append(args, "--filename="+strings.TrimRight(current, "/")+"/")
			}
			return runPicker(exec.Command(path, args...))
		}
		if path, err := exec.LookPath("kdialog"); err == nil {
			return runPicker(exec.Command(path, "--getexistingdirectory", current))
		}
		return "", errors.New(
			"aucune boîte de dialogue disponible (installez zenity ou kdialog), " +
				"ou modifiez « cache_dir » dans le fichier de configuration")
	}
}

func runPicker(cmd *exec.Cmd) (string, error) {
	out, err := cmd.Output()
	if err != nil {
		// Un code de retour non nul, ici, veut dire « annulé » dans les trois outils. Le
		// distinguer d'une vraie panne n'apporterait rien à l'utilisateur : dans les deux cas,
		// rien ne change.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}
