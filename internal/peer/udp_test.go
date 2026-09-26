package peer

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/lianee/kok-cache/internal/store"
)

// Le port partagé doit être ouvert dès ListenUDP, avant toute connexion : c'est ce qui fait poser
// au pare-feu Windows sa question au lancement plutôt qu'au premier échange.
func TestUDPPortOpenBeforeAnyConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(ctx, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(m.UDPAddrs()) != 0 {
		t.Fatal("aucun port ne doit être ouvert avant ListenUDP")
	}
	if err := m.ListenUDP(0); err != nil {
		t.Fatal(err)
	}
	addrs := m.UDPAddrs()
	if len(addrs) == 0 {
		t.Fatal("ListenUDP n'a ouvert aucun port")
	}
	for _, a := range addrs {
		ua, ok := a.(*net.UDPAddr)
		if !ok || ua.Port == 0 {
			t.Fatalf("adresse inattendue %v", a)
		}
		// Contrôle positif : l'adresse est réellement tenue, un second bind doit échouer.
		if c, err := net.ListenUDP("udp", ua); err == nil {
			c.Close()
			t.Fatalf("%v n'est pas tenu par le port partagé", ua)
		}
	}
}
