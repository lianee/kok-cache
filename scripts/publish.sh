#!/usr/bin/env bash
# Publie un état VALIDÉ de `main` vers le dépôt public GitHub.
#
# ────────────────────────────────────────────────────────────────────────────────────────────────
# Le principe : DEUX historiques, pas un.
#
#   Forgejo (origin)  — `main`   : l'historique de travail, fin, privé, avec ses tâtonnements.
#   GitHub  (github)  — `public` : une suite d'états validés, un commit par version publiée.
#
# Chaque commit public a EXACTEMENT l'arbre de `main` au moment de la publication, mais aucun de
# ses commits. Deux conséquences, et ce sont les deux raisons d'être de ce script :
#
#   1. Une erreur commise sur Forgejo — un secret glissé puis retiré, typiquement — ne peut JAMAIS
#      atteindre le dépôt public. Un `git rm` n'efface rien d'un historique ; ici l'historique
#      n'est simplement jamais transmis. La propriété tient à CHAQUE publication, pas seulement
#      à la première comme le ferait un squash initial.
#   2. Ce qui est publié est un état qu'on a décidé de publier, pas un instantané accidentel.
#
# ⛔ NE JAMAIS ajouter le remote github à `main`. Deux remotes sur une même branche, et un
#    `git push --all` distrait publie tout l'historique. Les branches séparées rendent l'accident
#    structurellement impossible — c'est le seul garde-fou qui ne repose pas sur l'attention.
#
# ⚠️ LA CHAÎNE DE VERSION EST UNE ENTRÉE DU BUILD. `scripts/build.sh` fait
#    `-X main.version=$(git describe --tags …)`. Un binaire bâti depuis Forgejo (`cacc517`) n'est
#    donc PAS celui bâti depuis le tag public (`v1.0.0`) : empreintes différentes, et un tiers en
#    conclut que le binaire publié ne correspond pas aux sources — soit l'effondrement de
#    l'argument central du projet, sur un détail invisible.
#    → Les artefacts de release doivent être bâtis PAR GITHUB ACTIONS, au tag, depuis le dépôt
#      public. Les builds locaux ne servent qu'à tester. C'est pour ça que ce script pose le tag
#      sur le commit PUBLIC.
# ────────────────────────────────────────────────────────────────────────────────────────────────
#
# Usage :
#   scripts/publish.sh v1.0.0 ["message de version"]
#   scripts/publish.sh v1.0.0 --dry-run     # montre tout, ne modifie et ne pousse RIEN
#
# Le script ne pousse qu'après confirmation explicite.

set -euo pipefail
cd "$(dirname "$0")/.."

PUBLIC_BRANCH="public"
GITHUB_REMOTE="github"
SRC_BRANCH="main"

TAG="${1:-}"
shift || true
DRY_RUN=0
MSG=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    *) MSG="$arg" ;;
  esac
done

die() { echo "⛔ $*" >&2; exit 1; }
ok()  { echo "   ✓ $*"; }

[ -n "$TAG" ] || die "usage : scripts/publish.sh <tag> [\"message\"] [--dry-run]"
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
  || die "tag « $TAG » : forme attendue vX.Y.Z (éventuellement suffixé), pour que « git describe » soit lisible"
[ -n "$MSG" ] || MSG="$TAG"

echo "── Contrôles ──────────────────────────────────────────────────────────────"

# 1. Arbre propre. Sinon on publierait un état qui n'est pas celui qu'on a testé, et le
#    « -dirty » de git describe trahirait de toute façon l'incohérence.
[ -z "$(git status --porcelain)" ] || die "l'arbre de travail n'est pas propre — commiter ou remiser d'abord"
ok "arbre de travail propre"

# 2. La source existe.
git rev-parse -q --verify "refs/heads/$SRC_BRANCH" >/dev/null \
  || die "branche « $SRC_BRANCH » introuvable"
ok "branche source « $SRC_BRANCH » présente"

# 3. Le tag ne doit pas déjà exister : republier un tag change ce que vérifie un tiers qui a
#    déjà téléchargé. Une version publiée est immuable.
! git rev-parse -q --verify "refs/tags/$TAG" >/dev/null \
  || die "le tag « $TAG » existe déjà — une version publiée ne se réécrit pas, en prendre un nouveau"
ok "tag « $TAG » disponible"

# 4. Le remote GitHub doit être configuré.
if ! git remote get-url "$GITHUB_REMOTE" >/dev/null 2>&1; then
  MISSING_REMOTE="remote « $GITHUB_REMOTE » absent. Le configurer une fois :
     git remote add $GITHUB_REMOTE git@github.com:<compte>/kok-cache.git
   (⛔ ne l'ajoutez PAS comme second remote de « $SRC_BRANCH »)"
  # En --dry-run on continue : pouvoir inspecter ce qui SERAIT publié avant même d'avoir créé le
  # dépôt distant est précisément l'usage utile de --dry-run.
  [ "$DRY_RUN" = "1" ] || die "$MISSING_REMOTE"
  echo "   ⚠️  $MISSING_REMOTE"
else
  ok "remote « $GITHUB_REMOTE » : $(git remote get-url "$GITHUB_REMOTE")"
fi

# 5. Rien d'ignoré ne doit traîner dans l'arbre publié. dist/ contient les binaires ; s'il était
#    suivi par erreur, on publierait ~55 Mo à chaque version.
if git ls-tree -r --name-only "$SRC_BRANCH" | grep -qE '^dist/'; then
  die "dist/ est suivi par git — il ne doit pas l'être (vérifier .gitignore)"
fi
ok "dist/ n'est pas suivi"

echo
echo "── Ce qui serait publié ───────────────────────────────────────────────────"

TREE="$(git rev-parse "$SRC_BRANCH^{tree}")"
PARENT="$(git rev-parse -q --verify "refs/heads/$PUBLIC_BRANCH" || true)"

if [ -z "$PARENT" ]; then
  echo "   PREMIÈRE PUBLICATION — le dépôt public partira d'un commit initial unique,"
  echo "   sans aucun historique Forgejo. C'est la propriété recherchée."
  echo
  echo "   Fichiers ($(git ls-tree -r --name-only "$SRC_BRANCH" | wc -l)) :"
  git ls-tree -r --name-only "$SRC_BRANCH" | sed 's/^/     /'
else
  echo "   Publication précédente : $(git log -1 --format='%h %s (%ad)' --date=short "$PARENT")"
  echo
  if git diff --quiet "$PARENT^{tree}" "$TREE"; then
    # Contenu identique : c'est la promotion d'une pré-version en version finale, le cas normal
    # après une répétition réussie. Fabriquer un commit vide serait faux — deux commits distincts
    # pour un contenu identique. La sémantique juste est UN commit, DEUX tags.
    SAME_TREE=1
    echo "   Contenu IDENTIQUE à la publication précédente."
    echo "   → aucun nouveau commit : le tag « $TAG » sera posé sur le commit existant."
    echo "     (c'est le cas d'une pré-version promue en version finale)"
  else
    SAME_TREE=0
    echo "   Différences d'arbre depuis la dernière publication :"
    git diff --stat "$PARENT^{tree}" "$TREE" | sed 's/^/     /'
  fi
fi
: "${SAME_TREE:=0}"

echo
if [ "$SAME_TREE" = "1" ]; then
  echo "   Commit public         : $PARENT (inchangé)"
else
  echo "   Nouveau commit public : arbre $TREE"
fi
echo "   Tag                   : $TAG"
echo "   Message               : $MSG"

# Garde-fou de dernière minute : chercher des motifs de secret dans l'arbre à publier.
# ⚠️ Ce n'est PAS une garantie — c'est un filet grossier. La vraie protection est de ne jamais
#    mettre de secret dans le dépôt (cf. le bloc « zéro secret » de TECHNIQUE.md).
echo
echo "── Recherche grossière de secrets (filet, pas garantie) ───────────────────"
SUSPECT=$(git grep -nIE '(secret|token|passwd|password|api[_-]?key)[[:space:]]*[:=][[:space:]]*["'"'"'][A-Za-z0-9/+_-]{16,}' "$SRC_BRANCH" -- . 2>/dev/null || true)
if [ -n "$SUSPECT" ]; then
  echo "$SUSPECT" | sed 's/^/   ⚠️  /'
  echo
  echo "   ⛔ Vérifiez ces lignes avant de continuer."
else
  ok "aucun motif évident"
fi

if [ "$DRY_RUN" = "1" ]; then
  echo
  echo "── --dry-run : rien n'a été créé ni poussé ────────────────────────────────"
  exit 0
fi

echo
printf "Publier « %s » sur %s ? [tapez le tag pour confirmer] : " "$TAG" "$(git remote get-url "$GITHUB_REMOTE")"
read -r ANSWER
[ "$ANSWER" = "$TAG" ] || die "abandon (confirmation non concordante)"

echo
echo "── Publication ────────────────────────────────────────────────────────────"

# Le commit est fabriqué en plomberie : on prend l'ARBRE de main tel quel, avec la publication
# précédente pour parent. Aucun checkout, donc aucun risque de fichier oublié, supprimé ou
# laissé derrière — l'identité de l'arbre est garantie par construction, pas par une recopie.
if [ "$SAME_TREE" = "1" ]; then
  COMMIT="$PARENT"
  ok "contenu inchangé — pas de nouveau commit, on tague $COMMIT"
elif [ -n "$PARENT" ]; then
  COMMIT="$(git commit-tree "$TREE" -p "$PARENT" -m "$MSG")"
  git update-ref "refs/heads/$PUBLIC_BRANCH" "$COMMIT"
  ok "commit public $COMMIT sur « $PUBLIC_BRANCH »"
else
  COMMIT="$(git commit-tree "$TREE" -m "$MSG")"
  git update-ref "refs/heads/$PUBLIC_BRANCH" "$COMMIT"
  ok "commit public $COMMIT sur « $PUBLIC_BRANCH »"
fi

git tag -a "$TAG" -m "$MSG" "$COMMIT"
ok "tag $TAG posé sur le commit public"

# Vérification : l'arbre publié doit être RIGOUREUSEMENT celui de main. Si ça diffère, c'est que
# la plomberie a mal tourné, et il vaut mieux le savoir avant de pousser qu'après.
git diff --quiet "$SRC_BRANCH^{tree}" "$COMMIT^{tree}" \
  || die "INCOHÉRENCE : l'arbre publié diffère de $SRC_BRANCH — ne pas pousser, examiner"
ok "arbre publié identique à « $SRC_BRANCH » (vérifié)"

git push "$GITHUB_REMOTE" "$PUBLIC_BRANCH:main"
git push "$GITHUB_REMOTE" "refs/tags/$TAG"
ok "poussé vers $GITHUB_REMOTE (branche main + tag $TAG)"

echo
echo "── Fait ───────────────────────────────────────────────────────────────────"
echo "   ⚠️ Les artefacts de release doivent être bâtis PAR GITHUB ACTIONS, au tag $TAG."
echo "      Un build local n'aurait pas la même chaîne de version, donc pas la même empreinte."
echo
echo "   Pour taguer aussi le point correspondant côté Forgejo (facultatif, repère de travail) :"
echo "      git tag -a $TAG-src -m \"source de $TAG\" $SRC_BRANCH && git push origin $TAG-src"
echo "      (nom distinct exprès : même nom sur deux commits différents prête à confusion)"
