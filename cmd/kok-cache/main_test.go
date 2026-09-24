package main

import "testing"

// Sans argument (double-clic), la liste des arguments est vide : ce cas paniquait (`args[1:]` sur
// une liste vide) et rendait tout lancement manuel impossible. Contrôle négatif effectué : avec
// l'ancien code, ce test panique.
func TestParseSuccessorTolerateEmptyArgs(t *testing.T) {
	if s := parseSuccessor(nil); s.of || s.updatedFrom != "" {
		t.Errorf("vide : %+v", s)
	}
	if s := parseSuccessor([]string{"run"}); s.of {
		t.Errorf("run seul : %+v", s)
	}
	if s := parseSuccessor([]string{"run", "--successor"}); !s.of || s.updatedFrom != "" {
		t.Errorf("successeur : %+v", s)
	}
	if s := parseSuccessor([]string{"run", "--updated-from=v1.0.0"}); !s.of || s.updatedFrom != "v1.0.0" {
		t.Errorf("mise à jour : %+v", s)
	}
}
