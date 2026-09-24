package ui

import "testing"

// La clé `Exec` d'un fichier .desktop sépare les arguments par des espaces. Sans guillemets, un
// exécutable rangé dans un dossier au nom composé n'est pas lancé — et rien ne le signale : le
// fichier existe, donc l'interface annonce le démarrage automatique activé.
func TestQuoteDesktopExec(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/usr/bin/kok-cache", `"/usr/bin/kok-cache"`},
		// Le cas qui motivait le correctif.
		{"/home/moi/Mes Applis/kok-cache", `"/home/moi/Mes Applis/kok-cache"`},
		// Caractères réservés à l'intérieur des guillemets, selon la spécification.
		{`/home/a"b/kok-cache`, `"/home/a\"b/kok-cache"`},
		{"/home/a`b/kok-cache", "\"/home/a\\`b/kok-cache\""},
		{`/home/a$b/kok-cache`, `"/home/a\$b/kok-cache"`},
		{`/home/a\b/kok-cache`, `"/home/a\\b/kok-cache"`},
		// Accents : rien à échapper, et surtout rien à casser.
		{"/home/josé/Téléchargements/kok-cache", `"/home/josé/Téléchargements/kok-cache"`},
	}
	for _, c := range cases {
		if got := quoteDesktopExec(c.in); got != c.want {
			t.Errorf("quoteDesktopExec(%q)\n  obtenu : %s\n  attendu : %s", c.in, got, c.want)
		}
	}
}
