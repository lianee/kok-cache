# Confidentialité

Cette page décrit, sans exception, ce que **kok-cache** envoie hors de votre ordinateur, à qui, et
pourquoi. Elle concerne le programme kok-cache, pas le site k-ok.fr lui-même.

## En bref

kok-cache ne contient aucune mesure d'audience, aucune publicité, aucun rapport de plantage, et
ne revend rien. Il ne parle qu'à quatre interlocuteurs : le serveur k-ok, les autres joueurs
connectés, un serveur Google qui lui indique son adresse publique, et GitHub pour les mises à jour.

## Ce qui reste sur votre ordinateur

- Les extraits musicaux, dans le dossier que vous avez choisi. Ils sont chiffrés, et kok-cache
  lui-même ne peut pas les lire.
- Ses réglages, son journal de fonctionnement et le jeton qui le relie à votre compte, dans un
  petit dossier de réglages (voir la [page technique](TECHNIQUE.md#configuration)).

Le journal n'est jamais envoyé nulle part. kok-cache ne lit aucun autre dossier que celui que
vous lui avez confié.

## Ce qui est envoyé, et à qui

### Au serveur k-ok (www.k-ok.fr)

- **L'identité de votre compte k-ok**, par le jeton de connexion obtenu quand vous l'avez relié
  à votre compte. Votre mot de passe ne passe jamais par kok-cache.
- **Le nom de cet ordinateur**, tel que votre système le déclare. Il sert à distinguer vos
  différents ordinateurs dans « Mon compte ». Vous pouvez le remplacer par le nom de votre choix
  (réglage `instance`, voir la page technique).
- **Ce que votre copie détient** : la liste des extraits disponibles, leur nombre, la place
  occupée, la version du programme et l'état du mode miroir. C'est ce qui permet au site de vous
  afficher son état, et aux autres joueurs de savoir à qui demander un extrait.
- **Votre adresse IP**, comme pour toute connexion à Internet.

Le serveur n'enregistre rien de tout cela en base de données : l'état de votre copie n'est gardé
en mémoire que tant qu'elle est connectée. Ses journaux techniques (adresse IP, date, requête)
sont effacés automatiquement : au bout d'un an pour le serveur web, de douze semaines pour le
journal de sécurité et de trente jours pour le serveur de signalisation.

### Aux autres joueurs connectés

kok-cache échange des extraits directement avec les navigateurs et les copies de kok-cache des
autres abonnés, comme le fait un onglet ouvert sur le site. Pour établir ces liaisons directes,
les deux côtés se communiquent leurs **adresses IP**, publique et sur le réseau local. Les autres
joueurs voient aussi **quels extraits votre copie peut fournir**. Les extraits échangés restent
chiffrés d'un bout à l'autre.

Quand une liaison directe est impossible, les échanges transitent par un relais du serveur k-ok,
toujours chiffrés.

### À Google (stun.l.google.com)

Pour connaître son adresse publique, kok-cache interroge un serveur STUN de Google. Google reçoit
alors votre adresse IP, et rien d'autre.

### À GitHub (github.com)

Au démarrage puis toutes les six heures, kok-cache vérifie sur GitHub si une nouvelle version
existe, même quand la mise à jour automatique est désactivée (il vous la signale alors sans
l'installer). GitHub reçoit votre adresse IP, selon sa propre
[déclaration de confidentialité](https://docs.github.com/fr/site-policy/privacy-policies/github-general-privacy-statement).

## Vos droits

Vous pouvez à tout moment couper le lien entre kok-cache et votre compte depuis sa page, et
supprimer le programme et ses dossiers (voir le [mode d'emploi](README.md#au-quotidien)).

Pour exercer vos droits d'accès, de rectification ou d'effacement sur les données conservées par
le serveur k-ok, écrivez à [webmaster@k-ok.fr](mailto:webmaster@k-ok.fr). Vous pouvez aussi adresser une
réclamation à la [CNIL](https://www.cnil.fr).

## Modifications

Toute modification de cette page est visible dans l'historique du dépôt, et accompagne la
version du programme qui la rend nécessaire.
