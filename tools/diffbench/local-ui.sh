#!/bin/sh
# local-ui.sh <ts|go> <имя-прогона> — дифференциальный прогон awg-ui без Docker: awg-ctrl (Go,
# на заглушках, сверен с TS отдельно) + панель нужной реализации + нода из Go-агента на тот же
# awg-ctrl. Как и local.sh — только для одноразовой машины: пересоздаёт /etc/amnezia.
# Нужны node 20+, sqlite3, openssl, go и `npm install` в awg-ui/ (и собранный awg-ui/dist).
set -e
IMPL="$1"; NAME="$2"
B=$(cd "$(dirname "$0")" && pwd); ROOT=$(cd "$B/../.." && pwd)
BENCH=/var/lib/uibench
mkdir -p "$B/bin/stubs" "$B/out" "$BENCH"
for c in awg awg-quick ip tc nft modinfo; do
  printf '#!/bin/sh\nexec node %s %s "$@"\n' "$B/stubs/stub.mjs" "$c" > "$B/bin/stubs/$c"; chmod +x "$B/bin/stubs/$c"
done
export PATH="$B/bin/stubs:$PATH"
(cd "$ROOT" && go build ${RACE:+-race} -o "$B/bin/awg-control" ./cmd/awg-control)  # RACE=1 — с детектором гонок
# Ключи стенда — одни на все прогоны: подпись sen://, сертификат хаба нод.
[ -f "$BENCH/sub_sign.key" ] || openssl genpkey -algorithm ed25519 -out "$BENCH/sub_sign.key" 2>/dev/null
[ -f "$BENCH/node_tls.key" ] || openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$BENCH/node_tls.key" -out "$BENCH/node_tls.crt" -days 3650 -subj /CN=awg-node 2>/dev/null
sh "$B/setup.sh"

PORT=3005 SERVER_IP=203.0.113.7 SERVER_PORT=47619 SERVER_NAME=VPN \
  INTERNAL_AUTH_PUB_FILE=/etc/amnezia/amneziawg/internal_auth_public.key SHAPE_SAMPLE_S=3600 AWG_CLI=/nonexistent/awg-ctrl \
  sh -c 'cd /tmp && exec "$0" ctrl' "$B/bin/awg-control" > /run/bench/ctrl.log 2>&1 &
CTRL=$!
i=0; until curl -s -o /dev/null http://127.0.0.1:3005/health; do i=$((i+1)); [ $i -gt 150 ] && { cat /run/bench/ctrl.log; kill $CTRL; exit 1; }; sleep 0.1; done

export PORT=8080 UI_USER=admin UI_PASS=benchpass JWT_SECRET=benchsecret UI_DB_FILE=/run/bench/ui.db AWGCTRL_PORT=3005 \
  INTERNAL_AUTH_KEY_FILE=/etc/amnezia/amneziawg/internal_auth_private.key \
  SUB_PORT=8443 SUB_TLS=off SUB_SIGN_KEY_FILE="$BENCH/sub_sign.key" SUB_HOST=sub.example.com \
  NODE_PORT=8444 NODE_TLS_KEY_FILE="$BENCH/node_tls.key" NODE_TLS_CERT_FILE="$BENCH/node_tls.crt" \
  BRAND=Bench CHANNEL=Beta VERSION=0.0.0-bench MONITOR_HASH_FILE="$BENCH/none" PROM_URL=http://127.0.0.1:9 \
  METRICS_PEERS=on AUDIT=on LOCAL_NODE=on
cd "$ROOT/awg-ui"
if [ "$IMPL" = "ts" ]; then node node_modules/.bin/tsx server.ts > /run/bench/ui.log 2>&1 &
else "$B/bin/awg-control" ui > /run/bench/ui.log 2>&1 &
fi
UIPID=$!
i=0; until curl -s -o /dev/null http://127.0.0.1:8080/ui/brand; do i=$((i+1)); [ $i -gt 200 ] && { cat /run/bench/ui.log; kill $UIPID $CTRL; exit 1; }; sleep 0.1; done
sleep 1

AGENT_BIN="$B/bin/awg-control" BENCH_DIR="$BENCH" "$ROOT/awg-ui/node_modules/.bin/tsx" "$B/scenario-ui.ts" "$B/out/$NAME.json" || true
sleep 2
kill $UIPID $CTRL 2>/dev/null || true; wait $UIPID $CTRL 2>/dev/null || true
node "$B/collect-ui.mjs" "$B/out/$NAME.json"
cp /run/bench/ui.log "$B/out/$NAME.ui.log"; cp /run/bench/ctrl.log "$B/out/$NAME.ctrl.log"
