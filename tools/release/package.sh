#!/bin/sh
# package.sh <версия> — архивы релиза: по одному на архитектуру, awgcontrol-<версия>-linux-<arch>.tar.gz.
# Внутри: awg-control (статический Go-бинарник: сервисы и CLI), awg-ui/dist (фронтенд), .env.
# Фронтенд должен быть собран заранее (npm run build в awg-ui). Общий для GitHub и Gitea.
set -eu
VERSION="$1"
[ -f awg-ui/dist/index.html ] || { echo "нет awg-ui/dist — сначала npm run build в awg-ui"; exit 1; }
for ARCH in amd64 arm64; do
  D="awgcontrol-${VERSION}"
  rm -rf "$D" && mkdir -p "$D/awg-ui"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$D/awg-control" ./cmd/awg-control
  cp -r awg-ui/dist "$D/awg-ui/dist"
  cp .env "$D/"
  tar -czf "awgcontrol-${VERSION}-linux-${ARCH}.tar.gz" "$D/"
  rm -rf "$D"
  echo "awgcontrol-${VERSION}-linux-${ARCH}.tar.gz"
done
