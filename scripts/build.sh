#!/usr/bin/env bash
# Chemin de build UNIQUE de kok-cache — celui de la CI comme celui d'un poste de développement.
#
# Pourquoi un script et pas un `go build` à la main : on distribue du code exécutable à des
# inconnus, qui démarre avec leur ordinateur. Publier les sources ne rend PAS le binaire
# vérifiable — les gens téléchargent un zip. Ce qui ferme l'écart, c'est qu'un tiers puisse
# reconstruire OCTET POUR OCTET l'artefact publié et comparer son SHA-256. Cela impose :
#
#   · CGO_ENABLED=0  — aucune dépendance à un compilateur C ni aux bibliothèques de la machine.
#                      C'est aussi la raison de fond du choix de pion plutôt qu'un binding
#                      libdatachannel : toute dépendance cgo casserait la reproductibilité.
#   · -trimpath      — sans lui, les chemins absolus de la machine de build entrent dans le binaire.
#   · -buildid=      — l'identifiant de build varie sinon d'une machine à l'autre.
#   · toolchain Go ÉPINGLÉE dans go.mod — une version différente produit un binaire différent.
#
# Vérification par un tiers :
#   git checkout <tag> && ./scripts/build.sh && sha256sum -c dist/SHA256SUMS
set -euo pipefail

cd "$(dirname "$0")/.."

# Version : le tag git s'il y en a un, sinon la description courte. JAMAIS un horodatage — deux
# builds de la même source doivent donner le même octet.
VERSION="${KOK_CACHE_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

# Toolchain attendue, lue dans go.mod : la version fait partie de l'entrée du build.
WANT_GO="$(awk '/^go /{print $2}' go.mod)"
HAVE_GO="$(go env GOVERSION | sed 's/^go//')"
if [ "$HAVE_GO" != "$WANT_GO" ]; then
  echo "⚠️  Go $HAVE_GO utilisé alors que go.mod épingle $WANT_GO." >&2
  echo "    Le binaire ne sera PAS reproductible bit à bit. GOTOOLCHAIN=go${WANT_GO} corrige." >&2
fi

# -s -w : pas de table de symboles ni de DWARF. Le débogage se fait sur une build locale ; ici on
# réduit la taille de ce qui est téléchargé.
LDFLAGS="-s -w -buildid= -X main.version=${VERSION}"

TARGETS="${KOK_CACHE_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64}"

rm -rf dist
mkdir -p dist

for target in $TARGETS; do
  GOOS="${target%%/*}"
  GOARCH="${target##*/}"
  # ⚠️ Nom SANS version — décidé le 2026-08-29, avant toute publication.
  # La page « compte » du site doit pointer vers un lien STABLE : mettre la version dans le nom
  # obligerait à republier la page à chaque release. Même patron que sw3-proxy, qui sert
  # `sw3-proxy-windows-amd64.zip?v=<version>` : le nom identifie la PLATEFORME, la version se lit
  # dans le binaire (`kok-cache version`), dans le tag et dans SHA256SUMS.
  name="kok-cache-${GOOS}-${GOARCH}"
  out="dist/${name}"
  [ "$GOOS" = "windows" ] && out="${out}.exe"

  # Windows : -H=windowsgui supprime la console. Sans lui, un double-clic ouvre une fenêtre noire
  # que l'utilisateur doit laisser ouverte — le programme s'arrête s'il la ferme. Le journal part
  # alors dans un fichier (cf. internal/… le journal est écrit dans le dossier de configuration).
  ld="$LDFLAGS"
  [ "$GOOS" = "windows" ] && ld="$ld -H=windowsgui"

  echo "→ ${GOOS}/${GOARCH}"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" GOFLAGS=-mod=readonly \
    go build -trimpath -ldflags "$ld" -o "$out" ./cmd/kok-cache

  # macOS : un binaire Unix double-cliqué ouvre le Terminal. Un bundle .app se lance sans, et
  # c'est un simple dossier — aucune signature ni outil Apple nécessaire pour le produire.
  if [ "$GOOS" = "darwin" ]; then
    # Nom lisible par un humain plutôt que l'architecture Go — même convention que sw3-proxy,
    # qui sert « macos-intel » et « macos-apple-silicon ». Personne ne sait ce qu'est « arm64 ».
    case "$GOARCH" in
      amd64) MACLABEL="macos-intel" ;;
      arm64) MACLABEL="macos-apple-silicon" ;;
      *)     MACLABEL="macos-${GOARCH}" ;;
    esac
    app="dist/kok-cache-${MACLABEL}.app"
    mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
    cp "$out" "$app/Contents/MacOS/kok-cache"
    cp assets/icon.icns "$app/Contents/Resources/kok-cache.icns"
    cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>kok-cache</string>
  <key>CFBundleIdentifier</key><string>fr.k-ok.kok-cache</string>
  <key>CFBundleExecutable</key><string>kok-cache</string>
  <key>CFBundleShortVersionString</key><string>${VERSION}</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleIconFile</key><string>kok-cache</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST
    # LSUIElement : pas d'icône dans le Dock ni de fenêtre — le programme vit dans le navigateur.
    # ⚠️ Le binaire du bundle doit être lancé avec l'argument `run` : c'est le rôle du .app, dont
    # l'exécutable est appelé sans argument. D'où le petit lanceur ci-dessous.
    mv "$app/Contents/MacOS/kok-cache" "$app/Contents/MacOS/kok-cache-bin"
    printf '#!/bin/sh\nexec "$(dirname "$0")/kok-cache-bin" run\n' > "$app/Contents/MacOS/kok-cache"
    chmod +x "$app/Contents/MacOS/kok-cache"

    # ⚠️ Un .app est un DOSSIER : tel quel il n'est pas téléchargeable. On le livre zippé, et on
    # retire le dossier de `dist` pour qu'il ne reste que des artefacts publiables.
    # `-y` conserve les liens symboliques, `-X` supprime les métadonnées propres à la machine de
    # build — sans quoi l'archive ne serait pas reproductible.
    # ⚠️ REPRODUCTIBILITÉ : un zip enregistre la date de chaque fichier. Sans horodatage fixe,
    # deux builds de la même source donneraient des archives différentes — et l'argument
    # « reconstruisez et comparez le SHA-256 » tomberait pour macOS.
    find "$app" -exec touch -t 200001010000 {} +
    ( cd dist && zip -qry -X "kok-cache-${MACLABEL}.app.zip" "kok-cache-${MACLABEL}.app" )
    rm -rf "$app"
  fi
done

# Fichiers seulement : les bundles .app sont des dossiers.
# La version est consignée ICI, puisqu'elle ne figure plus dans les noms de fichiers.
( cd dist && { echo "# kok-cache ${VERSION}"; find . -maxdepth 1 -type f ! -name SHA256SUMS -exec sha256sum {} + ; } > SHA256SUMS )

echo
echo "kok-cache ${VERSION} — empreintes :"
cat dist/SHA256SUMS
