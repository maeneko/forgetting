# Дифференциальный стенд: TS- и Go-версии awg-ctrl против одних и тех же заглушек.
FROM node:20-bookworm
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends openssl sqlite3 >/dev/null && rm -rf /var/lib/apt/lists/*
COPY tools/diffbench/stubs/stub.mjs /opt/stub/stub.mjs
RUN for c in awg awg-quick ip tc nft modinfo; do printf '#!/bin/sh\nexec node /opt/stub/stub.mjs %s "$@"\n' "$c" > /usr/local/bin/$c; chmod +x /usr/local/bin/$c; done
# TS-эталон: тот же код, что в репозитории, со своими node_modules под linux
COPY awg-ctrl/package.json /ts/awg-ctrl/package.json
RUN cd /ts/awg-ctrl && npm install --no-audit --no-fund --loglevel=error
COPY awg-ctrl/index.ts awg-ctrl/shaping.ts awg-ctrl/tsconfig.json /ts/awg-ctrl/
COPY tools/diffbench/setup.sh tools/diffbench/run.sh tools/diffbench/scenario-ctrl.mjs tools/diffbench/collect.mjs /bench/
WORKDIR /bench
