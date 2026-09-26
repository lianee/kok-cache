# kok-cache : page technique

Cette page rassemble ce qui n'a pas sa place sur la [page d'accueil](README.md) : les propriétés
de sécurité du programme et ce qui les garantit, la ligne de commande, la configuration, la
vérification du binaire, le développement.

## Ce que ce programme ne fait pas

Ce ne sont pas des promesses commerciales, ce sont des propriétés du code. Chacune est vérifiable
dans les sources, et testée.

- **Il ne peut pas lire la musique qu'il stocke.** Les fichiers sont chiffrés au repos.
  `kok-cache` ne demande aucune clé de déchiffrement, n'en stocke aucune et n'en manipule aucune :
  le déchiffrement n'a lieu que dans votre navigateur, au moment de la lecture. Copier son dossier
  ne donne rien d'écoutable.
- **Il ne contient aucun mot de passe ni secret partagé.** Sa seule identité est un jeton OAuth
  délivré à *votre* compte, que vous pouvez révoquer à tout moment (`kok-cache logout`). Un secret
  compilé dans un binaire distribué n'est pas un secret : `strings` suffirait à l'extraire.
- **Il ne supprime jamais un fichier sur la demande d'un tiers.** Aucun message reçu du réseau ne
  peut effacer quoi que ce soit sur votre disque. Deux cas seulement effacent : `prune`, pour les
  chansons sorties du catalogue, qui par défaut se contente de lister (il faut la relancer avec
  `KOK_CACHE_PRUNE_YES=1` pour qu'elle efface quoi que ce soit) ; et, hors mode miroir, le
  remplacement d'un fichier périmé par du vide : quand le catalogue officiel annonce une nouvelle
  version d'une chanson que vous aviez en cache, l'ancienne est inutilisable (personne ne la demande
  plus) et rien ne viendrait la remplacer, elle est donc supprimée. En mode miroir elle est
  remplacée par la nouvelle version, pas supprimée.
- **Il n'expose aucun service sur le réseau.** Sa page de contrôle et le retour de la connexion
  OAuth sont servis sur `127.0.0.1`, jamais sur une interface réseau : ni l'une ni l'autre n'est
  joignable depuis l'extérieur de votre machine. Le partage entre pairs passe par WebRTC, qui ouvre
  bien des ports UDP le temps d'un échange, mais une session ne s'établit qu'après négociation par
  le serveur de signalisation, où rien n'aboutit sans ticket authentifié.
- **Il n'installe rien au démarrage de votre session sans votre accord explicite.**
- **Il ne remplace pas son propre binaire sans votre accord**, et jamais sans preuve. L'option
  `auto_update` est fausse par défaut : sans elle, la page se borne à annoncer une version plus
  récente et à proposer un bouton. Avec ou sans, ce qui est installé a été vérifié deux fois
  avant qu'un octet ne soit écrit : la signature Ed25519 du manifeste, par une clé que seul
  l'éditeur détient, puis l'empreinte SHA-256 du binaire, contre ce manifeste. Le détail est dans
  la section [Mises à jour](#mises-à-jour).

## Page de contrôle

C'est une page servie par le programme lui-même, pas un site distant : rien n'en sort et elle
n'est accessible que depuis votre machine. Son adresse est
[http://127.0.0.1:8383](http://127.0.0.1:8383). Elle se change dans `config.json` (`ui_port`), et
si le port est déjà pris le programme essaie les suivants et écrit l'adresse retenue dans son
journal.

Relancer le programme quand il tourne déjà n'en démarre pas un second : il rouvre sa page.

Le journal est écrit dans `kok-cache.log`, à côté de la configuration.

## En ligne de commande

Tout reste pilotable sans la page, pour qui préfère :

```sh
kok-cache login     # connecte le compte (ouvre le navigateur)
kok-cache run       # lance le cache et le partage
kok-cache status    # ce qui est stocké, le catalogue, le reste à récupérer
kok-cache prune     # liste ce qui ne figure plus au catalogue ; efface avec KOK_CACHE_PRUNE_YES=1
kok-cache logout    # révoque le jeton et l'efface
```

Une seule instance peut utiliser un dossier à la fois. Lancé en ligne de commande, un second
exemplaire refuse de démarrer et vous renvoie vers le premier ; lancé d'un double-clic, il ouvre
simplement la page de celui qui tourne. Ce garde-fou existe parce que des copies oubliées d'un
programme équivalent ont tourné pendant des jours avec du vieux code, en servant de vrais
utilisateurs.

## Configuration

`config.json`, dans le dossier de configuration de votre système
(`~/.config/kok-cache/` sur Linux, `~/Library/Application Support/kok-cache/` sur macOS,
`%AppData%\kok-cache\` sur Windows). Le fichier est créé au premier lancement avec les valeurs par
défaut, il est fait pour être lu et modifié.

| Clé | Rôle |
|---|---|
| `cache_dir` | dossier des fichiers `<id>-<stem>.kok` |
| `mirror` | `true` pour synchroniser tout le catalogue, pas seulement ce que vous écoutez |
| `mirror_interval_minutes` | période de vérification du catalogue (6 h par défaut) |
| `instance` | étiquette de cette machine dans les journaux du serveur |
| `auto_update` | `true` pour installer seul les nouvelles versions (`false` par défaut : annonce seulement) |

Le jeton d'accès vit dans `token.json` (mode 0600), à part et jamais affiché.

## Mises à jour

Toutes les six heures, et au démarrage, le programme lit `latest.json` et sa signature
`latest.json.sig` parmi les fichiers de la
[dernière release](https://github.com/lianee/kok-cache/releases/latest). Ce qu'il en fait dépend
de l'option `auto_update` :

- `false` (défaut) : si la version annoncée est plus récente, la page l'affiche, avec un bouton
  « Mettre à jour maintenant » et un lien vers la release. Rien n'est téléchargé.
- `true` : il télécharge le binaire de sa plateforme, le vérifie, l'installe à sa propre place et
  se relance. La page affiche ensuite « mis à jour depuis la version X ».

Le bouton suit exactement le même chemin que l'option : la demande de l'utilisateur ne dispense
d'aucune vérification.

Ce qui est vérifié, dans cet ordre, avant qu'un octet ne soit écrit sur le disque :

1. **La signature du manifeste.** `latest.json` est signé en Ed25519 ; la clé publique est
   compilée dans le binaire (`updatePublicHex`, dans `cmd/kok-cache/update.go`). Un manifeste
   altéré, signé par une autre clé, ou dont la signature est illisible, est ignoré sans bruit :
   rien n'est annoncé, rien n'est installé. TLS et GitHub ne sont pas ce qui fonde la confiance.
2. **La monotonie.** Seule une version strictement plus récente (au sens de semver, une
   pré-version comptant comme inférieure à sa finale) est prise en compte. Un binaire sans version
   publiée (`dev`, un hash de commit, un `-dirty`) ne se met jamais à jour.
3. **L'empreinte du binaire.** Le fichier téléchargé doit avoir le SHA-256 inscrit dans le
   manifeste signé. Sinon, il est jeté.

Puis, une fois le binaire en place, **la preuve que la nouvelle version démarre** : l'ancien
processus lance le nouveau et attend qu'il ait pris le verrou d'instance. S'il ne le prend pas,
il est tué, l'ancien binaire est remis en place (il avait été renommé, pas effacé), l'ancien
processus continue, la page dit pourquoi, et cette version n'est pas retentée.

**Où vit la clé privée, et pourquoi ce n'est pas la CI.** GitHub Actions construit et atteste les
binaires ; il ne détient aucune clé de mise à jour. C'est l'éditeur qui, depuis sa machine, lance
`scripts/sign-release.sh <tag>` : le script télécharge les artefacts de la release, **vérifie leur
attestation de provenance**, calcule lui-même les empreintes, signe le manifeste et l'attache à
la release. Publier une release et la proposer aux copies installées sont donc deux décisions
distinctes, et un dépôt ou un workflow compromis ne peut rien faire installer chez personne.

Un tiers peut vérifier la chaîne : les empreintes de `latest.json` doivent être celles de
`SHA256SUMS`, elles-mêmes celles des artefacts attestés, eux-mêmes reproductibles depuis les
sources (section suivante).

Le manifeste peut porter un plancher (`min_version`). En dessous, la page affiche « mise à jour
requise » même sans l'option ; c'est réservé à une version qui ne doit plus tourner.

Pour un banc d'essai, `KOK_CACHE_UPDATE_URL` (variable d'environnement, jamais `config.json`)
désigne un autre serveur. Sans la clé privée, ce serveur ne peut rien faire installer : c'est ce
qui permet de laisser cette variable exister.

## Pare-feu Windows

La page de contrôle et la connexion au compte n'écoutent que sur `127.0.0.1`. Les échanges avec
les autres joueurs, en revanche, passent par WebRTC, qui a besoin de ports UDP sur la carte
réseau. kok-cache les ouvre dès le démarrage, partagés par toutes les connexions : le pare-feu
Windows Defender pose donc sa question au lancement. Les ouvrir à la demande la faisait surgir au
premier échange direct, n'importe quand, et elle était presque toujours fermée sans être lue.

Fermer cette fenêtre (Échap, « Annuler ») crée des règles qui bloquent les connexions ENTRANTES.
kok-cache continue de fonctionner, mais les pairs qui veulent le joindre, y compris vos autres
ordinateurs sur le même réseau, passent alors par le relais du serveur au lieu d'une liaison
directe. Pour rattraper : Sécurité Windows → Pare-feu et protection du réseau → « Autoriser une
application via le pare-feu » → « Modifier les paramètres », puis cochez kok-cache pour le profil
de votre réseau (Privé, ou Public si Windows l'a classé ainsi : Paramètres → Réseau et Internet →
propriétés de la connexion → Type de profil réseau).

Les règles du pare-feu suivent le chemin du programme, et une mise à jour remplace le fichier au
même endroit : la question ne devrait donc pas revenir à chaque version.

## Vérifier le binaire que vous avez téléchargé

Les *releases* sont construites par une CI publique, avec une attestation de provenance, et les
builds sont **reproductibles**. Vous pouvez donc reconstruire vous-même l'artefact publié et
comparer :

```sh
git checkout <tag>
KOK_CACHE_VERSION=<tag> ./scripts/build.sh
sha256sum -c dist/SHA256SUMS
```

`KOK_CACHE_VERSION` n'est pas une commodité : la version est inscrite **dans** le binaire, elle
fait donc partie de l'entrée du build. La laisser deviner par `git describe` ne marche pas quand
deux tags désignent le même commit (le cas d'une pré-version promue en version finale). La CI la
fixe au tag, fixez la même. Chaque release rappelle la commande exacte.

Cela suppose aussi la version de Go épinglée dans `go.mod`. Le script vous prévient si la vôtre
diffère.

## Développement

```sh
go test ./...          # dont un transfert WebRTC réel entre deux instances
go test -race ./...
./scripts/build.sh
```

Les tests ne dépendent d'aucun service extérieur : un faux serveur de signalisation est monté en
mémoire, et deux instances complètes s'échangent réellement un fichier (offre/réponse SDP,
candidats ICE, DataChannel, blocs de 16 Ko, vérification du hash).

## Trois invariants, et pourquoi ils comptent

1. **Prédicat de synchronisation : « absent OU hash différent ».** Un ré-encodage côté serveur
   remplace intégralement le contenu d'un extrait : le fichier n'est pas absent, il est périmé. Un
   cache qui ne cherche que les absents ne le rafraîchit jamais, se tait sur ce hash inconnu, et
   disparaît silencieusement du partage pour ce morceau.
2. **Vérification du hash AVANT le renommage atomique.** Un fichier en cours de téléchargement
   n'est jamais annoncé ; un fichier visible est toujours un fichier vérifié. Sans cela, un cache
   en pleine synchronisation annoncerait des fichiers tronqués.
3. **Silence, jamais suppression, sur désaccord de hash.** Le hash attendu vient de celui qui
   demande. Obéir donnerait à n'importe qui une primitive d'effacement à distance sur les disques
   des autres. L'autorité sur ce qui est périmé, c'est le catalogue signé du serveur.

## Licence

[MIT](LICENSE), © 2026 Liane.

Cette licence autorise explicitement la reconstruction du binaire à partir des sources, ce dont
dépend la vérification décrite plus haut : sans licence, le droit d'auteur l'interdirait par
défaut. Les dépendances sont toutes permissives (pion en MIT, gorilla/websocket en BSD-2,
`golang.org/x/sys` en BSD-3).
