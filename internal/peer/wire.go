package peer

import (
	"encoding/json"
	"strconv"

	"github.com/pion/webrtc/v4"
)

// STEM_V — protocole v2 « à la torrent » : on annonce { v, size } dans `stem-resp`, le `stem-get`
// porte v>=2, et on sert ensuite des plages d'octets à la demande. Le protocole legacy (fichier
// entier poussé spontanément) a été RETIRÉ le 2026-07-19 : un `stem-get` sans v est ignoré.
const stemProtocolVersion = 2

// Types de messages échangés dans l'enveloppe `stem_broadcast`.
const (
	msgAvail  = "stem-avail"
	msgResp   = "stem-resp"
	msgGet    = "stem-get"
	msgSDP    = "stem-sdp"
	msgICE    = "stem-ice"
	msgDelete = "stem-delete"
)

// wireMsg est la forme exacte des messages du maillage. Les champs optionnels sont omis à
// l'émission pour rester octet pour octet comparable à ce qu'envoient les deux implémentations
// existantes (bot seeder Node et global.js).
type wireMsg struct {
	Type     string  `json:"type"`
	SongID   flexInt `json:"songId"`
	Stem     string  `json:"stem"`
	PeerID   string  `json:"peerId"`
	ToPeerID string  `json:"toPeerId,omitempty"`
	Hash     string  `json:"hash,omitempty"`
	V        int     `json:"v,omitempty"`
	Size     int64   `json:"size,omitempty"`
	// Sync marque un `stem-avail` émis par le BALAYAGE d'un miroir (pas par un auditeur). Champ
	// additif, ignoré par le seeder Node et les navigateurs (comme `role` dans stem-resp). Un pair
	// qui le voit ne poursuit pas la demande opportunément : deux miroirs se téléchargeraient sinon
	// mutuellement chaque chanson au même instant, tous deux depuis le seeder (2026-09-21).
	Sync bool `json:"sync,omitempty"`

	SDP       *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
}

// rangeReq est la demande de plage envoyée EN TEXTE sur le datachannel : {"o":offset,"l":len}.
type rangeReq struct {
	O int64 `json:"o"`
	L int64 `json:"l"`
}

// flexInt accepte un identifiant de chanson en nombre OU en chaîne.
//
// Ce n'est pas de la complaisance : le navigateur construit ses clés par interpolation
// (`${songId}-${stem}`), donc un `"1384"` et un `1384` y sont indiscernables et les deux formes
// circulent sans que rien ne le signale. Un décodage strict échouerait en silence sur la moitié
// de l'essaim. À l'émission on écrit toujours un nombre.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

func (f flexInt) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Itoa(int(f))), nil
}

func (f flexInt) int() int { return int(f) }
