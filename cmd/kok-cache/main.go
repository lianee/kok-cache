// Commande kok-cache — cache local des stems k-ok, et pair de l'essaim 24 h/24.
//
// Ce qu'il fait :
//   - il garde sur le disque les stems déjà écoutés, pour que la lecture soit instantanée ;
//   - il les partage avec les autres joueurs, comme le fait déjà un onglet ouvert.
//
// Ce qu'il ne fait PAS, et c'est un invariant, pas une promesse :
//   - il ne détient AUCUNE clé de déchiffrement et ne lit aucun son ; les fichiers sont du
//     ciphertext, le déchiffrement n'a lieu que dans le navigateur, à la lecture ;
//   - il n'embarque aucun secret partagé : sa seule identité est le jeton OAuth de son
//     utilisateur, révocable à tout moment depuis son compte ;
//   - il ne supprime jamais un fichier sur la demande d'un tiers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	ossignal "os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lianee/kok-cache/internal/api"
	"github.com/lianee/kok-cache/internal/auth"
	"github.com/lianee/kok-cache/internal/config"
	"github.com/lianee/kok-cache/internal/mirror"
	"github.com/lianee/kok-cache/internal/peer"
	"github.com/lianee/kok-cache/internal/signal"
	"github.com/lianee/kok-cache/internal/store"
	"github.com/lianee/kok-cache/internal/ui"
)

// version est injectée à la compilation par scripts/build.sh (-ldflags). « dev » signale un
// binaire construit à la main, hors du chemin de publication.
var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", "", "chemin du fichier de configuration")
		verbose = flag.Bool("v", false, "journal détaillé")
	)
	flag.Usage = usage
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	// Journal vers la console ET un fichier. Le fichier n'est pas un luxe : le binaire Windows est
	// construit sans console (sinon un double-clic ouvre une fenêtre noire que l'utilisateur doit
	// laisser ouverte), et sur macOS le bundle .app n'en a pas non plus. Sans fichier, un problème
	// chez quelqu'un serait totalement muet.
	log, closeLog := newLogger(level, logFilePath())
	defer closeLog.Close()

	// Sans argument, on lance le cache : c'est ce qu'attend quelqu'un qui double-clique sur le
	// programme. `kok-cache -h` reste là pour qui cherche les autres commandes.
	//
	// ⭐ L'ABSENCE d'argument distingue un lancement MANUEL d'un démarrage de session : le
	// raccourci d'autostart passe toujours `run` explicitement (fichier .desktop, clé de registre,
	// LaunchAgent). C'est ce qui permet d'ouvrir la page quand un humain vient de double-cliquer —
	// et de ne rien ouvrir à l'ouverture de session.
	cmd := flag.Arg(0)
	manual := cmd == ""
	if manual {
		cmd = "run"
	}

	// Options relayées au successeur lors d'un redémarrage ou d'une mise à jour. ⚠️ DÉFAUT RÉEL
	// (mesuré le 2026-09-14) : le successeur était lancé avec `run` nu, donc SANS `-config`. Il
	// partait sur la configuration par défaut, un autre dossier, un autre verrou, et le
	// redémarrage était abandonné après 10 s.
	var relay []string
	if *cfgPath != "" {
		relay = append(relay, "-config", *cfgPath)
	}
	if *verbose {
		relay = append(relay, "-v")
	}

	succ := parseSuccessor(flag.Args())

	if err := run(cmd, *cfgPath, manual, relay, succ, log); err != nil {
		fmt.Fprintln(os.Stderr, "kok-cache: "+err.Error())
		os.Exit(1)
	}
}

// successor décrit comment ce processus a été lancé, quand c'est par un autre kok-cache.
type successor struct {
	of          bool
	updatedFrom string
}

// parseSuccessor lit les marqueurs posés APRÈS la commande par le processus qui nous lance
// (voir handOver) :
//
//	--successor              nous remplaçons une instance qui tient encore le port ;
//	--updated-from=<version> et c'est une mise à jour, depuis cette version.
//
// ⚠️ args peut être VIDE : c'est le lancement manuel (double-clic, sans argument). `args[1:]` sur
// une liste vide paniquait avant même l'ouverture du journal — aucun lancement sans argument ne
// fonctionnait plus depuis la mise à jour signée (2026-09-14), et personne ne l'a vu pendant une
// semaine parce que l'autostart passe toujours `run` (trouvé le 2026-09-21).
func parseSuccessor(args []string) successor {
	var succ successor
	if len(args) < 2 {
		return succ
	}
	for _, a := range args[1:] {
		switch {
		case a == "--successor":
			succ.of = true
		case strings.HasPrefix(a, "--updated-from="):
			succ.of = true
			succ.updatedFrom = strings.TrimPrefix(a, "--updated-from=")
		}
	}
	return succ
}

func run(cmd, cfgPath string, manual bool, relay []string, succ successor, log *slog.Logger) error {
	if cmd == "version" {
		fmt.Println("kok-cache " + version)
		return nil
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	tok, err := config.LoadToken()
	if err != nil {
		return err
	}
	authMgr := auth.NewManager(config.OAuthBase(), cfg.Instance, tok)

	ctx, stop := signalContext()
	defer stop()

	switch cmd {
	case "login":
		return doLogin(ctx, authMgr, cfg.OAuthPort)
	case "logout":
		if err := authMgr.Logout(ctx); err != nil {
			return err
		}
		fmt.Println("Déconnecté. Le jeton a été révoqué côté serveur et effacé localement.")
		return nil
	case "status":
		return doStatus(ctx, cfg, tok, authMgr, log)
	case "prune":
		return doPrune(ctx, cfg, authMgr, log)
	case "run":
		return doRun(ctx, cfg, tok, authMgr, manual, relay, succ, log)
	case "open":
		return doOpen()
	default:
		usage()
		return fmt.Errorf("commande inconnue : %s", cmd)
	}
}

func doLogin(ctx context.Context, authMgr *auth.Manager, port int) error {
	fmt.Println("Connexion à votre compte k-ok.")
	err := authMgr.Login(ctx, port, func(u string) {
		// L'URL est TOUJOURS affichée : ouvrir le navigateur est un confort, pas une garantie
		// (poste sans navigateur par défaut, session distante).
		fmt.Println("\nOuvrez cette adresse dans votre navigateur si elle ne s'ouvre pas seule :")
		fmt.Println("\n  " + u + "\n")
	})
	if err != nil {
		return err
	}
	if mail := authMgr.Email(); mail != "" {
		fmt.Println("Connecté en tant que " + mail + ".")
	} else {
		fmt.Println("Connecté.")
	}
	// Divulgation, à l'endroit et au moment où elle a du sens : au moment d'activer le partage.
	fmt.Println("\nkok-cache garde les extraits que vous écoutez et les partage avec les autres")
	fmt.Println("joueurs. Ces fichiers sont chiffrés : ce programme ne détient aucune clé et ne")
	fmt.Println("peut pas les lire. Vous pouvez révoquer son accès à tout moment (`kok-cache logout`).")
	return nil
}

func doStatus(ctx context.Context, cfg *config.Config, tok *config.Token, authMgr *auth.Manager, log *slog.Logger) error {
	st, err := store.New(cfg.CacheDir)
	if err != nil {
		return err
	}
	files, bytes := st.DiskUsage()

	fmt.Println("kok-cache " + version)
	fmt.Println("Configuration : " + cfg.Path())
	fmt.Println("Dossier       : " + cfg.CacheDir)
	fmt.Printf("Sur le disque : %d fichiers, %.1f Go\n", files, float64(bytes)/(1<<30))
	fmt.Println("Instance      : " + cfg.Instance)
	fmt.Printf("Mode miroir   : %v\n", cfg.Mirror)
	if cfg.AutoUpdate {
		fmt.Println("Mise à jour   : automatique")
	} else {
		fmt.Println("Mise à jour   : sur demande (auto_update: false)")
	}

	switch {
	case !authMgr.Configured():
		fmt.Println("Compte        : non connecté (lancer `kok-cache login`)")
		return nil
	case authMgr.Email() != "":
		fmt.Println("Compte        : " + authMgr.Email())
	default:
		fmt.Println("Compte        : connecté")
	}

	st.LoadHashCache(hashCachePath())

	apiClient := api.New(config.APIURL(), config.Referer(), authMgr)
	mir := mirror.New(apiClient, st, nil, mustConfigDir(), time.Duration(cfg.MirrorIntervalMinutes)*time.Minute, cfg.Mirror, log)
	if err := mir.Refresh(ctx); err != nil {
		fmt.Println("Catalogue     : indisponible (" + err.Error() + ")")
		return nil
	}
	// Le prédicat exige l'empreinte de chaque fichier. Tout ce qui n'est pas déjà connu doit être
	// relu depuis le disque — dire combien évite de croire le programme figé.
	if todo := st.UnhashedCount(); todo > 0 {
		fmt.Printf("Empreintes    : %d fichier(s) à relire, patientez…\n", todo)
	}
	pending := mir.Pending()
	stale := 0
	for _, it := range pending {
		if it.Stale {
			stale++
		}
	}
	fmt.Printf("Catalogue     : %d chansons\n", mir.Count())
	fmt.Printf("À synchroniser: %d stems — %d absents, %d périmés\n",
		len(pending), len(pending)-stale, stale)
	_ = st.SaveHashCache(hashCachePath())
	return nil
}

// hashCachePath : à côté de la configuration, pas dans le dossier des stems — celui-ci doit ne
// contenir que des stems, et l'utilisateur peut le vider sans rien casser (il l'a fait).
func hashCachePath() string {
	return filepath.Join(mustConfigDir(), "hashes.json")
}

// doPrune liste — et ne supprime QUE sur demande explicite — les fichiers que le catalogue ne
// connaît plus.
//
// ⛔ Aucune suppression automatique nulle part ailleurs : ni sur un `stem-delete` reçu d'un pair
// (primitive d'effacement à distance), ni sur un désaccord de hash (le demandeur peut avoir tort).
// Un stem retiré du manifeste est le seul cas légitime, et il reste soumis à l'utilisateur.
func doPrune(ctx context.Context, cfg *config.Config, authMgr *auth.Manager, log *slog.Logger) error {
	st, err := store.New(cfg.CacheDir)
	if err != nil {
		return err
	}
	apiClient := api.New(config.APIURL(), config.Referer(), authMgr)
	mir := mirror.New(apiClient, st, nil, mustConfigDir(), time.Minute, false, log)
	if err := mir.Refresh(ctx); err != nil {
		return fmt.Errorf("le catalogue doit être joignable pour décider quoi retirer : %w", err)
	}

	inv, err := st.Scan()
	if err != nil {
		return err
	}
	var orphans []int
	for id := range inv {
		if mir.ExpectedHash(id, store.StemAccompaniment) == "" && mir.ExpectedHash(id, store.StemVocals) == "" {
			orphans = append(orphans, id)
		}
	}
	sort.Ints(orphans)

	if len(orphans) == 0 {
		fmt.Println("Rien à retirer : tout ce qui est stocké figure au catalogue.")
		return nil
	}
	fmt.Printf("%d chanson(s) ne figurent plus au catalogue :\n", len(orphans))
	for _, id := range orphans {
		fmt.Printf("  %d\n", id)
	}
	// ⛔ PLANCHER DE VRAISEMBLANCE. `prune` est la seule commande qui efface des chansons SORTIES du
	// catalogue (le miroir, lui, n'efface hors mode miroir que des fichiers périmés de chansons
	// toujours présentes, cf. mirror.dropStale), et elle décide sur la foi du manifeste. Un manifeste PARTIEL — requête serveur cassée qui ne rend que 10 chansons
	// sur 2790 — ferait passer presque tout le cache pour orphelin, et la confirmation aurait été
	// donnée par quelqu'un qui s'attendait à en retirer trois.
	// Le manifeste vide est déjà refusé en amont (mirror.Refresh) ; ceci couvre le cas partiel.
	if len(orphans) > len(inv)/2 {
		fmt.Printf("\n⛔ %d chansons sur %d seraient retirées — c'est trop pour être normal.\n",
			len(orphans), len(inv))
		fmt.Println("Le catalogue du serveur est probablement incomplet. Rien n'a été supprimé.")
		fmt.Println("Réessayez plus tard ; si cela persiste, signalez-le plutôt que de forcer.")
		return nil
	}
	if os.Getenv("KOK_CACHE_PRUNE_YES") == "" {
		fmt.Println("\nRien n'a été supprimé. Pour confirmer : KOK_CACHE_PRUNE_YES=1 kok-cache prune")
		return nil
	}
	removed := 0
	for _, id := range orphans {
		for _, stem := range store.Stems {
			if os.Remove(st.Path(id, stem)) == nil {
				removed++
			}
		}
	}
	fmt.Printf("%d fichier(s) supprimé(s).\n", removed)
	return nil
}

// doOpen rouvre la page de contrôle d'une instance déjà lancée. L'adresse contient le secret qui
// autorise à la piloter, d'où le fichier en 0600 écrit par l'interface.
func doOpen() error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ui-url"))
	if err != nil {
		return errors.New("aucune instance de kok-cache ne semble tourner (lancer `kok-cache run`)")
	}
	url := strings.TrimSpace(string(raw))
	fmt.Println(url)
	openBrowser(url)
	return nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func doRun(ctx context.Context, cfg *config.Config, tok *config.Token, authMgr *auth.Manager,
	manual bool, relay []string, succ successor, log *slog.Logger) error {
	st, err := store.New(cfg.CacheDir)
	if err != nil {
		return err
	}
	// Résolu UNE FOIS : sous Linux, /proc/self/exe suit le fichier, et après une mise à jour il
	// désignerait le binaire renommé en .old.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if succ.updatedFrom == "" {
		cleanupOldBinary(exe)
	} else {
		// Nous sommes le successeur d'une mise à jour : notre prédécesseur a encore besoin de .old
		// pour revenir en arrière si nous mourons avant d'avoir pris le verrou. On nettoie une fois
		// clairement en vie.
		time.AfterFunc(60*time.Second, func() { cleanupOldBinary(exe) })
	}
	// Instance unique, AVANT tout le reste. Deux instances sur le même dossier, c'est deux pairs
	// qui annoncent et servent les mêmes fichiers avec le même compte — et avec le démarrage
	// automatique plus le lancement par double-clic, le cas est probable, pas théorique.
	recovered, err := st.Lock()
	if err != nil {
		var locked store.ErrLocked
		if errors.As(err, &locked) {
			// ⚠️ Sans console (binaire Windows, bundle macOS), un échec ici est TOTALEMENT muet :
			// on double-clique, rien ne se passe, et rien n'explique pourquoi. Or ce double-clic
			// veut dire « montre-moi kok-cache » — alors on ouvre la page de l'instance qui tourne
			// déjà, et on s'arrête sans erreur.
			if manual {
				log.Info("kok-cache tourne déjà — ouverture de sa page")
				_ = doOpen()
				return nil
			}
			return fmt.Errorf("%w.\nPour ouvrir sa fenêtre : kok-cache open", err)
		}
		return err
	}
	defer st.Unlock()
	if recovered {
		log.Warn("l'instance précédente ne s'est pas arrêtée proprement — verrou repris")
	}

	if n := st.SweepTemp(); n > 0 {
		log.Info("temporaires abandonnés retirés", "n", n)
	}
	st.LoadHashCache(hashCachePath())
	defer func() {
		if err := st.SaveHashCache(hashCachePath()); err != nil {
			log.Warn("cache d'empreintes non enregistré", "err", err)
		}
	}()

	apiClient := api.New(config.APIURL(), config.Referer(), authMgr)

	pm := peer.NewManager(ctx, st, nil, log)
	mir := mirror.New(apiClient, st, pm, mustConfigDir(),
		time.Duration(cfg.MirrorIntervalMinutes)*time.Minute, cfg.Mirror, log)
	pm.SetOracle(mir)

	sig := signal.New(config.SignalURL(), cfg.Instance, apiClient, pm,
		time.Duration(cfg.ReconnectDelayMs)*time.Millisecond, log)
	pm.Attach(sig)

	// Ce que le site pourra afficher à distance, sans jamais joindre cette machine : la page
	// « compte » interroge le serveur de signalisation, qui voit le helper et le navigateur sous le
	// même compte.
	sig.SetStats(func() map[string]any {
		files, bytes := st.DiskUsage()
		return map[string]any{
			"files": files, "bytes": bytes,
			"mirror": mir.Active(), "version": version,
		}
	})

	files, bytes := st.DiskUsage()
	log.Info("kok-cache démarre", "version", version, "dossier", cfg.CacheDir,
		"fichiers", files, "Go", fmt.Sprintf("%.1f", float64(bytes)/(1<<30)), "miroir", cfg.Mirror)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	// handOver lance le successeur et PROUVE qu'il sert avant de s'effacer. Partagé par le bouton
	// « Redémarrer » et par la mise à jour (qui ajoute `--updated-from=`).
	//
	// ⚠️ ORDRE IMPÉRATIF, et c'est tout le bug : libérer le verrou AVANT de lancer le
	// successeur. Auparavant le parent le gardait jusqu'à sa propre sortie, donc l'enfant
	// se voyait refuser le dossier et mourait, puis le parent s'arrêtait. « Redémarrer »
	// ne faisait qu'éteindre.
	handOver := func(extra []string) error {
		_ = st.SaveHashCache(hashCachePath())
		st.Unlock()

		args := append(append(append([]string{}, relay...), "run", "--successor"), extra...)
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			// Échec du lancement : on reprend le verrou, mieux vaut continuer que tout perdre.
			_, _ = st.Lock()
			return err
		}
		// ⛔ `Start()` prouve que le processus a été ENGENDRÉ, pas qu'il VIT. Un successeur qui
		// meurt aussitôt (disque plein, configuration illisible, dossier devenu inaccessible)
		// laissait le parent s'arrêter quand même : plus rien ne tournait, pendant que la page
		// annonçait « kok-cache redémarre » puis se rechargeait dans le vide.
		// C'est la même classe que le défaut « Redémarrer ÉTEIGNAIT », en plus étroit.
		//
		// On attend donc qu'il ait réellement pris le verrou d'instance : c'est la preuve qu'il
		// est allé jusqu'au bout de son démarrage, pas seulement qu'il a été créé.
		if !awaitSuccessor(st, cmd.Process.Pid, 15*time.Second) {
			_ = cmd.Process.Kill() // il n'a pas démarré : ne pas laisser un demi-processus traîner
			_ = cmd.Wait()         // et le moissonner, sinon il reste <defunct> tant qu'on tourne
			_, _ = st.Lock()
			log.Error("redémarrage abandonné : la nouvelle instance n'a pas démarré, l'ancienne continue")
			return errors.New("la nouvelle instance n'a pas démarré, kok-cache continue de tourner")
		}
		log.Info("passage de relais : nouvelle instance lancée", "pid", cmd.Process.Pid)
		stop()
		return nil
	}
	if succ.updatedFrom != "" {
		log.Info("mise à jour effectuée", "de", succ.updatedFrom, "vers", version)
	}
	upd := newUpdater(cfg.AutoUpdate, exe, succ.updatedFrom, log, handOver)

	// Interface locale : c'est elle que voit l'utilisateur, la ligne de commande n'étant plus que
	// le mode expert. Elle démarre AVANT toute connexion, sinon quelqu'un qui n'a pas encore de
	// compte n'aurait aucun moyen de s'en créer un.
	srv, err := ui.Start(ui.Deps{
		Version: version, Cfg: cfg, Token: tok, Store: st, Peer: pm,
		Mirror: mir, Auth: authMgr, Signal: sig, Log: log, Quit: stop,
		Restart:     func() error { return handOver(nil) },
		Update:      upd,
		WaitForPort: succ.of,
	})
	if err != nil {
		return err
	}
	log.Info("interface disponible", "url", srv.URL())

	// ⛔ On n'ouvre PAS le navigateur au lancement : l'adresse est fixe, le site k-ok y renvoie par
	// un lien, et avec le démarrage automatique cela ferait surgir un onglet à chaque ouverture de
	// session — exactement le genre de programme qu'on désinstalle.
	//
	// Seule exception : tant qu'aucun compte n'est connecté, le programme ne peut RIEN faire sans
	// une action de l'utilisateur. Quelqu'un qui vient de double-cliquer sur un binaire sans
	// console ne verrait alors strictement rien se passer.
	// Un lancement manuel doit produire quelque chose de visible : sans console ni fenêtre, rien
	// ne distinguait sinon un démarrage réussi d'un programme qui n'a pas démarré. Au démarrage de
	// session, en revanche, on n'ouvre rien.
	if manual || !authMgr.Configured() {
		srv.Open()
	}

	// ⛔ SURVEILLANCE CONTINUE, pas une décision figée au démarrage.
	//
	// ⚠️ Refonte du 2026-08-29 après un retour d'usage accablant : la présence d'un compte était
	// évaluée UNE FOIS, au lancement. Résultat, se connecter depuis l'interface ne démarrait rien
	// — il fallait redémarrer le programme à la main pour « rejoindre le partage », et se
	// déconnecter n'arrêtait rien non plus. Un utilisateur devait deviner tout cela.
	// Désormais : connexion → le partage démarre seul ; déconnexion → il s'arrête seul.
	go superviseServices(runCtx, authMgr, sig, mir, log)

	// Mise à jour : vérification au démarrage puis toutes les 6 h ; installation seulement si
	// l'option est active ou sur demande depuis la page.
	go upd.Run(runCtx)

	// Le cache d'empreintes est aussi écrit périodiquement, pas seulement à l'arrêt : un `kill -9`
	// ou une coupure de courant le perdrait, et le démarrage suivant relirait tout le dossier —
	// plusieurs minutes de disque pour rien. Écriture atomique, donc une coupure en plein
	// enregistrement ne laisse pas de cache tronqué.
	go func() {
		tick := time.NewTicker(10 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				if err := st.SaveHashCache(hashCachePath()); err != nil {
					log.Debug("cache d'empreintes non enregistré", "err", err)
				}
			}
		}
	}()
	<-runCtx.Done() // l'arrêt vient de l'interface, d'un signal, ou d'un redémarrage demandé

	log.Info("arrêt")
	return nil
}

// awaitSuccessor attend que le processus `pid` ait pris le verrou d'instance, preuve qu'il a
// réellement démarré. Renvoie false s'il meurt ou ne le prend pas dans le délai.
//
// 📌 On interroge le VERROU plutôt que la simple existence du processus : un processus vivant qui
// n'a pas pris le verrou n'a pas fini de démarrer, et c'est le verrou qui décide qui sert.
func awaitSuccessor(st *store.Store, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if holder, ok := st.LockHolder(); ok && holder == pid {
			return true
		}
	}
	return false
}

// superviseServices démarre et arrête le partage selon l'état du compte, en continu.
//
// Le sondage est volontairement bête : deux secondes, une comparaison. Un mécanisme d'abonnement
// serait plus élégant, mais c'est exactement le genre d'état qu'on oublie de notifier depuis un
// chemin — et l'oubli produit ici la pire des pannes : un programme qui tourne sans rien faire.
func superviseServices(ctx context.Context, authMgr *auth.Manager, sig *signal.Client,
	mir *mirror.Mirror, log *slog.Logger) {

	// L'arrêt passe par un canal plutôt que par une fonction d'annulation gardée en variable :
	// `go vet` exige qu'un `cancel` soit appelé sur tous les chemins de SA fonction, ce qu'une
	// boucle de supervision ne peut pas satisfaire lisiblement. Ici la goroutine qui détient le
	// `cancel` l'appelle toujours.
	var stopCh chan struct{}
	var wg sync.WaitGroup
	running := false

	// ⛔ `stopAll` ATTEND la fin des services, il ne se contente pas de demander l'arrêt.
	//
	// Sans cette attente, une déconnexion suivie d'une reconnexion rapide — deux secondes suffisent,
	// c'est la période du sondage — relançait `sig.Run` et `mir.Run` pendant que les précédents
	// finissaient peut-être encore. Deux `sig.Run` sur le MÊME client se disputent la connexion :
	// l'un ferme ce que l'autre vient d'ouvrir. Cela « marchait » parce que deux secondes suffisent
	// en général — une espérance, pas une garantie, et le genre de défaut qui ne se manifeste que
	// chez quelqu'un d'autre.
	stopAll := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
		wg.Wait()
		running = false
	}
	defer stopAll()

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		switch want := authMgr.Configured(); { // ⛔ via authMgr : lecture SOUS VERROU
		case want && !running:
			sctx, cancel := context.WithCancel(ctx)
			stopCh = make(chan struct{})
			go func(ch chan struct{}) {
				<-ch
				cancel()
			}(stopCh)
			wg.Add(3)
			go func() { defer wg.Done(); sig.Run(sctx) }()
			go func() { defer wg.Done(); mir.Run(sctx) }()
			go func() { defer wg.Done(); sig.PublishStats(sctx, 5*time.Minute) }()
			running = true
			log.Info("compte connecté — partage et cache démarrés")
		case !want && running:
			stopAll()
			log.Info("compte déconnecté — partage et cache arrêtés")
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// openLogFile ouvre le journal en ajout, en le faisant tourner à 2 Mo. Renvoie nil si le dossier
// n'est pas accessible : ne pas pouvoir journaliser ne doit jamais empêcher de démarrer.
// logFilePath renvoie le chemin du journal, ou "" si le dossier de configuration est
// introuvable. ⚠️ L'ouverture et la ROTATION sont faites par `rotatingWriter` (log.go) : la
// version précédente ne testait la taille qu'ici, donc une seule fois au démarrage — et une
// instance qui tourne des semaines n'était jamais bornée.
func logFilePath() string {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kok-cache.log")
}

func mustConfigDir() string {
	dir, err := config.Dir()
	if err != nil {
		return "."
	}
	return dir
}

// signalContext annule le contexte sur Ctrl-C ou SIGTERM — un arrêt de service doit fermer les
// sessions proprement, pas laisser des temporaires derrière lui.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	ossignal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
		// ⚠️ Installer un gestionnaire DÉSARME l'interruption par défaut : sans cette ligne, un
		// Ctrl-C pendant une phase qui ne consulte pas le contexte (le hachage du dossier, par
		// exemple) ne fait plus RIEN — il ne reste qu'à fermer la fenêtre. Rendre la main au
		// comportement standard garantit qu'un second Ctrl-C tue le processus.
		ossignal.Stop(ch)
		fmt.Fprintln(os.Stderr, "\nArrêt en cours… (Ctrl-C à nouveau pour forcer)")
	}()
	return ctx, cancel
}

func usage() {
	fmt.Fprint(os.Stderr, `kok-cache `+version+` — cache local des stems k-ok

  kok-cache login     connecter un compte k-ok (ouvre le navigateur)
  kok-cache run       lancer le cache et le partage
  kok-cache status    état du dossier, du compte et du catalogue
  kok-cache prune     lister ce qui ne figure plus au catalogue
  kok-cache logout    révoquer le jeton et l'effacer
  kok-cache version

Options :
  -config <chemin>    fichier de configuration
  -v                  journal détaillé
`)
}
