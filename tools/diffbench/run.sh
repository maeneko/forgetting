#!/bin/sh
# run.sh <ts|go> <имя-прогона>; окружение: GEN, WITH_I1, STUB_MODULE
set -e
IMPL="$1"; NAME="$2"
./setup.sh
export PORT=3005 SERVER_IP=203.0.113.7 SERVER_PORT=47619 SERVER_NAME=VPN \
       INTERNAL_AUTH_PUB_FILE=/etc/amnezia/amneziawg/internal_auth_public.key \
       SHAPE_SAMPLE_S=3600 AWG_CLI=/nonexistent/awg-ctrl
cd /ts/awg-ctrl
if [ "$IMPL" = "ts" ]; then
  node node_modules/.bin/tsx index.ts > /run/bench/ctrl.log 2>&1 &
else
  /go/awg-control ctrl > /run/bench/ctrl.log 2>&1 &
fi
PID=$!
i=0; until curl -s -o /dev/null http://127.0.0.1:3005/health; do i=$((i+1)); [ $i -gt 100 ] && { cat /run/bench/ctrl.log; exit 1; }; sleep 0.1; done
sleep 1   # стартовый apply шейпинга асинхронный — даём ему отработать до сценария
cd /bench
node scenario-ctrl.mjs "/out/$NAME.json"
kill $PID 2>/dev/null || true; wait $PID 2>/dev/null || true
node collect.mjs "/out/$NAME.json"
cp /run/bench/ctrl.log "/out/$NAME.log"
