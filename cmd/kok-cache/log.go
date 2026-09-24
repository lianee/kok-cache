package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Journal : une rotation qui agit VRAIMENT, et un format compact.
//
// ⛔ DÉFAUT CORRIGÉ LE 2026-08-30 : la rotation existait, mais elle n'était évaluée qu'au
// LANCEMENT. Or le cas normal est une instance qui démarre à l'ouverture de session et tourne des
// semaines : entre deux démarrages, plus rien ne bornait le fichier. Le plafond de 2 Mo donnait
// donc une fausse impression de sûreté. Mesuré sur une instance réelle : ~400 Ko par jour en
// période de synchronisation, soit une trentaine de Mo sur trois mois sans redémarrage.
//
// Choix assumés :
//   - Bornage par TAILLE, pas par durée. « Un mois » ne borne rien — un mois de synchronisation
//     miroir est gros, un mois calme est vide. Ce qu'on protège, c'est le disque.
//   - Pas de compression : à ce plafond, gzip économiserait moins de 2 Mo. Elle ne deviendrait
//     intéressante qu'en gardant des journaux que personne ne lira.
//   - Pas de dépendance (lumberjack ferait tout ça) : dans un projet dont l'argument est qu'on peut
//     auditer ce qu'on exécute, une trentaine de lignes valent mieux qu'un paquet de plus.

const (
	logMaxBytes = 2 << 20 // par génération
	logKeepOld  = ".1"    // une seule génération conservée
)

// rotatingWriter bascule le fichier dès qu'il dépasse le plafond, PENDANT l'exécution.
type rotatingWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func newRotatingWriter(path string, max int64) *rotatingWriter {
	w := &rotatingWriter{path: path, max: max}
	w.open()
	return w
}

func (w *rotatingWriter) open() {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		w.f = nil
		return
	}
	w.f = f
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		// Journal indisponible : on ne fait pas échouer le programme pour autant. Il écrit de toute
		// façon aussi sur la sortie d'erreur.
		return len(p), nil
	}
	if w.size+int64(len(p)) > w.max {
		w.rotateLocked()
		if w.f == nil {
			return len(p), nil
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotateLocked() {
	_ = w.f.Close()
	// Le renommage écrase la génération précédente : deux fichiers au plus, donc 4 Mo au total.
	_ = os.Rename(w.path, w.path+logKeepOld)
	w.size = 0
	w.open()
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// ── format compact ────────────────────────────────────────────────────────────────────────────
//
// `slog.TextHandler` écrit `time=2026-08-30T19:03:38.207+02:00 level=INFO msg="…"`. La clé `time=`
// n'apprend rien, les nanosecondes et le fuseau non plus, et le message est inutilement mis entre
// guillemets. Sur une ligne moyenne de 128 octets, une trentaine sont du décor.
//
// ⚠️ MAIS LA DATE RESTE. Le journal du bot seeder n'horodate rien, et dater un événement y oblige à
// se repérer sur des marqueurs en comptant les lignes — leçon déjà payée. On raccourcit le format,
// on ne retire pas le jour.
//
//	08-30 19:03:38 INFO  authentifié auprès de la signalisation role=cache uid=1
type compactHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Leveler
	attrs []slog.Attr
	group string
}

func newCompactHandler(w io.Writer, level slog.Leveler) *compactHandler {
	return &compactHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *compactHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *compactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &n
}

func (h *compactHandler) WithGroup(name string) slog.Handler {
	n := *h
	if name != "" {
		n.group = strings.TrimPrefix(h.group+"."+name, ".")
	}
	return &n
}

func (h *compactHandler) Handle(_ context.Context, r slog.Record) error {
	var b []byte
	// ⛔ LE DÉCALAGE EST INDISPENSABLE, pas décoratif. Le harnais `npm run remote` horodate en
	// UTC (`toISOString()`), kok-cache et le bot seeder en heure locale. Sans le décalage, deux
	// journaux qu'on croit comparables sont décalés de deux heures en été — piège tombé dans
	// le 2026-08-31 : conclusion « le seeder n'a rien vu » sur une fenêtre décalée, alors
	// qu'il avait servi 130 transferts.
	b = r.Time.AppendFormat(b, "01-02 15:04:05-07")
	b = append(b, ' ')
	b = append(b, levelLabel(r.Level)...)
	b = append(b, ' ')
	// Le message précède les attributs, donc ses espaces ne gênent personne : pas de guillemets.
	b = append(b, strings.ReplaceAll(r.Message, "\n", " ")...)
	for _, a := range h.attrs {
		b = appendAttr(b, h.group, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		b = appendAttr(b, h.group, a)
		return true
	})
	b = append(b, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(b)
	return err
}

func levelLabel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO "
	case l < slog.LevelError:
		return "WARN "
	default:
		return "ERROR"
	}
}

func appendAttr(b []byte, group string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return b
	}
	if a.Value.Kind() == slog.KindGroup {
		g := a.Key
		if group != "" && g != "" {
			g = group + "." + g
		} else if g == "" {
			g = group
		}
		for _, sub := range a.Value.Group() {
			b = appendAttr(b, g, sub)
		}
		return b
	}
	b = append(b, ' ')
	if group != "" {
		b = append(b, group...)
		b = append(b, '.')
	}
	b = append(b, a.Key...)
	b = append(b, '=')
	return appendValue(b, a.Value.String())
}

// appendValue met des guillemets seulement quand il le faut : une valeur vide, ou contenant une
// espace, un guillemet ou un signe égal, serait sinon illisible à la relecture.
func appendValue(b []byte, v string) []byte {
	if v == "" || strings.ContainsAny(v, " \"=\n") {
		return append(b, strconv.Quote(strings.ReplaceAll(v, "\n", " "))...)
	}
	return append(b, v...)
}

// newLogger assemble le tout : sortie d'erreur + fichier tournant, format compact.
func newLogger(level slog.Leveler, path string) (*slog.Logger, io.Closer) {
	if path == "" {
		return slog.New(newCompactHandler(os.Stderr, level)), io.NopCloser(nil)
	}
	w := newRotatingWriter(path, logMaxBytes)
	return slog.New(newCompactHandler(&fileThenConsole{file: w, console: os.Stderr}, level)), w
}

// fileThenConsole écrit dans le fichier D'ABORD, puis sur la console au mieux. ⚠️ Pas un
// io.MultiWriter : celui-ci s'arrête à la PREMIÈRE erreur, et sous Windows le binaire n'a pas de
// console (-H=windowsgui), donc os.Stderr est un handle invalide dont chaque écriture échoue. Le
// fichier restait vide — précisément dans le cas où il est le seul témoin (2026-09-21).
type fileThenConsole struct {
	file    io.Writer
	console io.Writer
}

func (t *fileThenConsole) Write(p []byte) (int, error) {
	n, err := t.file.Write(p)
	_, _ = t.console.Write(p) // best effort : une console absente n'est pas une erreur
	return n, err
}

var _ = time.Now // (le format d'horodatage vit dans Handle)
