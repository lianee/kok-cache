#!/usr/bin/env bash
# Signe le manifeste de mise à jour d'une release publiée, et l'attache à cette release.
#
# ────────────────────────────────────────────────────────────────────────────────────────────────
# Publier une release et la POUSSER vers les copies installées sont deux décisions distinctes.
#
#   · GitHub Actions construit, atteste et publie les binaires (release.yml). Il ne détient
#     aucune clé de mise à jour : un dépôt public compromis, un workflow modifié, une action
#     tierce détournée, rien de tout cela ne peut faire installer quoi que ce soit chez qui que
#     ce soit.
#   · Ce script, lancé par l'éditeur sur sa machine, produit `latest.json` + `latest.json.sig`
#     et les ajoute aux artefacts de la release. C'est ce fichier signé, et lui seul, que les
#     copies installées acceptent (cmd/kok-cache/update.go).
#
# Avant de signer, le script télécharge chaque binaire de la release, VÉRIFIE SON ATTESTATION de
# provenance (`gh attestation verify`) et calcule lui-même les empreintes : le manifeste signé
# décrit donc des octets dont on a vérifié qu'ils sortent bien du workflow public, pas ce qu'un
# fichier SHA256SUMS prétend. Un tiers peut refaire la même vérification : les empreintes du
# manifeste doivent être celles de SHA256SUMS, et celles-ci celles des artefacts attestés.
#
# La clé privée : un seed Ed25519 de 32 octets en hex, dans le magasin local de l'éditeur.
#   première fois :  go run ./tools/sign -gen -out ~/.sw3/secrets/kok-cache/signing-key
#                    (la clé PUBLIQUE s'affiche : à recopier dans update.go, `updatePublicHex`)
#   ensuite :        KOK_CACHE_SIGNING_KEY="$(sw3secret kok-cache/signing-key)" scripts/sign-release.sh v1.2.0
# ⛔ La valeur de la clé ne passe jamais par un argument (visible dans `ps`) ni par un fichier du
#    dépôt (.gitignore exclut *.key, mais le réflexe reste : ne rien écrire ici).
#
# Usage :
#   scripts/sign-release.sh <tag> [--min-version vX.Y.Z] [--dry-run]
#     --min-version : plancher en dessous duquel les copies installées affichent « mise à jour
#                     requise » même sans l'option automatique. À n'utiliser que pour une version
#                     qui ne doit plus tourner (protocole cassé, faille).
#     --dry-run     : tout faire sauf téléverser.
# ────────────────────────────────────────────────────────────────────────────────────────────────
set -euo pipefail
cd "$(dirname "$0")/.."

REPO="lianee/kok-cache"
TAG="${1:-}"
shift || true
MIN_VERSION=""
DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --min-version) MIN_VERSION="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "⛔ argument inconnu : $1" >&2; exit 1 ;;
  esac
done

die() { echo "⛔ $*" >&2; exit 1; }
ok()  { echo "   ✓ $*"; }

[ -n "$TAG" ] || die "usage : scripts/sign-release.sh <tag> [--min-version vX.Y.Z] [--dry-run]"
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "tag « $TAG » : forme attendue vX.Y.Z"
[ -n "${KOK_CACHE_SIGNING_KEY:-}" ] || die "KOK_CACHE_SIGNING_KEY absente (voir l'en-tête du script)"
command -v gh >/dev/null || die "gh (GitHub CLI) introuvable"

# Une pré-version n'est jamais « latest » sur GitHub, donc jamais vue par les copies installées :
# signer son manifeste ne servirait à rien, et le dire évite de croire l'avoir poussée.
case "$TAG" in *-*) die "« $TAG » est une pré-version : elle n'est pas servie comme « latest », rien à signer" ;; esac

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "── Téléchargement des artefacts de $TAG ──────────────────────────────────"
gh release download "$TAG" --repo "$REPO" --dir "$WORK" --pattern 'kok-cache-*' --pattern 'SHA256SUMS'
ls -1 "$WORK" | sed 's/^/   /'

echo
echo "── Attestation de provenance ──────────────────────────────────────────────"
# Chaque binaire doit avoir été produit par le workflow de CE dépôt. Le zip macOS n'est pas
# attesté à part (c'est un emballage du binaire darwin, qui l'est), on le laisse de côté.
for f in "$WORK"/kok-cache-*; do
  case "$f" in *.zip) continue ;; esac
  gh attestation verify "$f" --repo "$REPO" >/dev/null || die "attestation invalide : $(basename "$f")"
  ok "$(basename "$f") : attesté par $REPO"
done

echo
echo "── Empreintes ────────────────────────────────────────────────────────────"
sha() { sha256sum "$WORK/$1" | cut -d' ' -f1; }
for f in kok-cache-linux-amd64 kok-cache-linux-arm64 kok-cache-darwin-amd64 kok-cache-darwin-arm64 kok-cache-windows-amd64.exe; do
  [ -f "$WORK/$f" ] || die "artefact manquant dans la release : $f"
  # L'empreinte calculée ici doit être celle que la CI a publiée : sinon quelqu'un a remplacé un
  # artefact après coup, et l'attestation seule ne le dirait pas.
  grep -q "$(sha "$f")  ./$f" "$WORK/SHA256SUMS" || die "$f : empreinte différente de SHA256SUMS"
  ok "$f  $(sha "$f" | cut -c1-16)…"
done

echo
echo "── Manifeste ─────────────────────────────────────────────────────────────"
# Même forme que sw3-proxy. Les clés sont GOOS-GOARCH, ce que le binaire calcule pour se
# reconnaître (platformKey), et les noms sont les artefacts STABLES de la release.
cat > "$WORK/latest.json" <<JSON
{"version":"${TAG}","date":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","min_version":"${MIN_VERSION}","files":{
"linux-amd64":{"name":"kok-cache-linux-amd64","sha256":"$(sha kok-cache-linux-amd64)"},
"linux-arm64":{"name":"kok-cache-linux-arm64","sha256":"$(sha kok-cache-linux-arm64)"},
"darwin-amd64":{"name":"kok-cache-darwin-amd64","sha256":"$(sha kok-cache-darwin-amd64)"},
"darwin-arm64":{"name":"kok-cache-darwin-arm64","sha256":"$(sha kok-cache-darwin-arm64)"},
"windows-amd64":{"name":"kok-cache-windows-amd64.exe","sha256":"$(sha kok-cache-windows-amd64.exe)"}}}
JSON
go run ./tools/sign -in "$WORK/latest.json"
cat "$WORK/latest.json"

if [ "$DRY_RUN" = "1" ]; then
  echo
  echo "── --dry-run : manifeste signé dans $WORK, rien n'a été téléversé ────────"
  trap - EXIT
  exit 0
fi

echo
printf "Attacher ce manifeste signé à la release %s, donc le proposer à TOUTES les copies installées ? [tapez le tag] : " "$TAG"
read -r ANSWER
[ "$ANSWER" = "$TAG" ] || die "abandon"

# --clobber : re-signer une release (rotation de clé, plancher ajouté) remplace le manifeste.
gh release upload "$TAG" --repo "$REPO" --clobber "$WORK/latest.json" "$WORK/latest.json.sig"
ok "latest.json + latest.json.sig attachés à $TAG"
echo
echo "   Les copies installées vérifient toutes les 6 h : avec l'option automatique, elles"
echo "   passeront à $TAG dans la journée ; sans elle, leur page l'annonce et propose le bouton."
