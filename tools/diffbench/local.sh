#!/bin/sh
# local.sh <ts|go> <имя-прогона> — тот же прогон, что run.sh, но без Docker: на одноразовой
# машине (CI-раннер, облачная песочница). Заглушки кладутся в свой каталог и попадают только
# в PATH этого прогона; ВНИМАНИЕ: setup.sh пересоздаёт /etc/amnezia — не запускать на сервере.
# Нужны node 20+, sqlite3, openssl, go; npm install в awg-ctrl/ делается при первом запуске.
# Окружение то же: GEN, WITH_I1, S4, STUB_MODULE. Результат — tools/diffbench/out/<имя>.json.
set -e
IMPL="$1"; NAME="$2"
B=$(cd "$(dirname "$0")" && pwd); ROOT=$(cd "$B/../.." && pwd)
mkdir -p "$B/bin/stubs" "$B/out"
for c in awg awg-quick ip tc nft modinfo; do
  printf '#!/bin/sh\nexec node %s %s "$@"\n' "$B/stubs/stub.mjs" "$c" > "$B/bin/stubs/$c"; chmod +x "$B/bin/stubs/$c"
done
export PATH="$B/bin/stubs:$PATH"
if [ "$IMPL" = "go" ]; then (cd "$ROOT" && go build -o "$B/bin/awg-control" ./cmd/awg-control)
elif [ ! -d "$ROOT/awg-ctrl/node_modules" ]; then (cd "$ROOT/awg-ctrl" && npm install --no-audit --no-fund --loglevel=error --no-package-lock); fi
sh "$B/setup.sh"
export PORT=3005 SERVER_IP=203.0.113.7 SERVER_PORT=47619 SERVER_NAME=VPN \
       INTERNAL_AUTH_PUB_FILE=/etc/amnezia/amneziawg/internal_auth_public.key \
       SHAPE_SAMPLE_S=3600 AWG_CLI=/nonexistent/awg-ctrl
cd "$ROOT/awg-ctrl"
if [ "$IMPL" = "ts" ]; then node node_modules/.bin/tsx index.ts > /run/bench/ctrl.log 2>&1 &
else "$B/bin/awg-control" ctrl > /run/bench/ctrl.log 2>&1 &
fi
PID=$!
i=0; until curl -s -o /dev/null http://127.0.0.1:3005/health; do i=$((i+1)); [ $i -gt 150 ] && { cat /run/bench/ctrl.log; kill $PID; exit 1; }; sleep 0.1; done
sleep 1
cd "$B"
node scenario-ctrl.mjs "$B/out/$NAME.json"
sleep 3
kill $PID 2>/dev/null || true; wait $PID 2>/dev/null || true
node collect.mjs "$B/out/$NAME.json"
cp /run/bench/ctrl.log "$B/out/$NAME.log"
