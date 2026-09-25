# kok-cache

**kok-cache** garde sur votre ordinateur les extraits que vous écoutez sur [k-ok](https://www.k-ok.fr),
pour qu'ils démarrent instantanément la fois suivante. Il en fait aussi profiter les autres joueurs,
comme le ferait un onglet resté ouvert sur le site, sans que vous ayez à le laisser ouvert.

C'est un petit programme, sans installation, qui se pilote depuis une page dans votre navigateur.
Il est réservé aux comptes k-ok avec abonnement actif.

## Installation, en trois gestes

### 1. Téléchargez le fichier pour votre ordinateur

Sur la [page de téléchargement](https://github.com/lianee/kok-cache/releases/latest), prenez le
fichier qui correspond à votre machine :

| Vous avez… | Prenez… |
|---|---|
| Windows | `kok-cache-windows-amd64.exe` |
| Mac récent (puce Apple M1, M2, M3…) | `kok-cache-macos-apple-silicon.app.zip` |
| Mac plus ancien (processeur Intel) | `kok-cache-macos-intel.app.zip` |
| Linux | `kok-cache-linux-amd64` |

Sur Mac, ouvrez le `.zip` : il contient l'application, que vous pouvez glisser où vous voulez.

### 2. Double-cliquez dessus

Rien à installer, aucune fenêtre noire, rien à laisser ouvert. Le programme démarre, discret.

Il se peut que votre ordinateur vous demande confirmation la première fois, parce que le programme
ne vient pas d'une boutique d'applications. Sur Windows, cliquez « Informations complémentaires »
puis « Exécuter quand même ». Sur Mac, faites un clic droit sur l'application puis « Ouvrir ».

### 3. Suivez la page qui s'ouvre

Une page apparaît dans votre navigateur. Elle sert à tout : connecter votre compte k-ok, choisir
le dossier où seront gardés les extraits, voir ce qui est stocké et ce qui est partagé.

C'est terminé. À partir de là, kok-cache travaille tout seul.

## Au quotidien

**Pour revenir à sa page**, allez dans **Mon compte** sur k-ok.fr et cliquez
**« Ouvrir kok-cache sur cet ordinateur »**. Ce panneau vous montre aussi si kok-cache est en
ligne, sur chacun de vos ordinateurs.

(Relancer le programme fonctionne aussi : s'il tourne déjà, il n'en démarre pas un second, il
rouvre simplement sa page.)

**Pour l'arrêter**, utilisez le bouton « Quitter » de sa page.

**Pour vous en séparer**, quittez-le, puis supprimez le fichier téléchargé et le dossier que vous
lui aviez confié. Il ne reste alors qu'un petit dossier de réglages, que vous pouvez aussi jeter
(son emplacement est indiqué dans la [page technique](TECHNIQUE.md#configuration)).

## Ce qu'il fait, et ne fait pas, avec vos données

- **Il ne peut pas lire la musique qu'il stocke.** Les fichiers gardés sur votre disque sont
  illisibles en dehors du site : copier le dossier ne donne rien d'écoutable.
- **Il ne connaît aucun mot de passe.** Il est simplement relié à *votre* compte k-ok, et vous
  pouvez couper ce lien à tout moment.
- **Personne ne peut lui ordonner d'effacer quoi que ce soit à distance.** La seule chose qu'il
  retire de lui-même, c'est une ancienne version d'une chanson que le site a remplacée, devenue
  inutile.
- **Il n'est joignable que depuis votre ordinateur**, et il ne s'installe pas au démarrage de
  votre session sans votre accord.
- **Il ne se met pas à jour tout seul**, sauf si vous cochez l'option. Sans elle, sa page vous
  signale qu'une nouvelle version existe et vous laisse choisir. Avec ou sans, il n'installe
  jamais qu'un fichier signé par l'éditeur et identique à celui publié.

Ce ne sont pas des promesses en l'air : chacune est une propriété du code, vérifiable et testée.
Le détail, pour qui veut le lire, est dans la [page technique](TECHNIQUE.md).

Ce qu'il envoie hors de votre ordinateur, et à qui, est détaillé dans la
[page confidentialité](PRIVACY.md).

## Pour aller plus loin

La [page technique](TECHNIQUE.md) couvre le reste : l'utilisation en ligne de commande, les réglages,
comment vérifier que le fichier téléchargé est bien celui qui a été publié, et comment contribuer.

## Licence

[MIT](LICENSE), © 2026 Liane.

Signature du programme pour Windows : [politique de signature](CODE_SIGNING.md) (en anglais).
