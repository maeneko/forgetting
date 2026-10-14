#!/bin/sh
# Чистое состояние «сервера»: awg1.conf нужного поколения, ключи, пустые базы, заглушки.
# GEN=2|3.1, WITH_I1=1 — добавить I1 в conf (install.sh его не пишет).
set -e
GEN="${GEN:-2}"
D=/etc/amnezia/amneziawg
rm -rf /etc/amnezia /var/lib/stub /run/bench
mkdir -p "$D" /var/lib/stub /run/bench
node -e 'const c=require("crypto");process.stdout.write(c.createHash("sha256").update("server").digest("base64"))' > /etc/amnezia/server_private.key
chmod 600 /etc/amnezia/server_private.key
node -e 'const c=require("crypto");const p=require("fs").readFileSync("/etc/amnezia/server_private.key","utf8").trim();process.stdout.write(c.createHash("sha256").update("pub:"+p).digest("base64")+"\n")' > "$D/server_public.key"
openssl genpkey -algorithm ed25519 -out "$D/internal_auth_private.key" 2>/dev/null
openssl pkey -in "$D/internal_auth_private.key" -pubout -out "$D/internal_auth_public.key"
PRIV=$(cat /etc/amnezia/server_private.key)
cat > "$D/awg1.conf" <<CONF
[Interface]
Address = 10.9.0.1/16
MTU = 1376
PostUp = awg set awg1 private-key /etc/amnezia/server_private.key; iptables -A FORWARD -i awg1 -j ACCEPT; iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
PreDown = iptables -D FORWARD -i awg1 -j ACCEPT; iptables -t nat -D POSTROUTING -o eth0 -j MASQUERADE
ListenPort = 47619
PrivateKey = ${PRIV}
Jc = 6
Jmin = 10
Jmax = 50
S1 = 90
S2 = 45
S3 = 37
S4 = ${S4:-14}
H1 = 1224800044-2116730834
H2 = 2122053282-2133204808
H3 = 2133604274-2140756116
H4 = 2143656228-2147444225
CONF
if [ "${WITH_I1:-0}" = "1" ]; then
  echo 'I1 = <r 2><b 0x858000010001000000000669636c6f756403636f6d0000010001c00c000100010000105a00044d583737>' >> "$D/awg1.conf"
fi
if [ "$GEN" = "3.1" ]; then
cat >> "$D/awg1.conf" <<CONF
HeaderProtectionKey = AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=
ContentPaddingAddition = 10-100
RekeyAfterTime = 100-120
RekeyTimeout = 3-7
RejectAfterTime = 150-180
KeepaliveTimeout = 5-15
MaxHandshakeAttempts = 15-20
CONF
fi
chmod 600 "$D/awg1.conf"
echo '{"up":false,"n":0,"peers":{}}' > /var/lib/stub/state.json
: > /var/lib/stub/log.jsonl
awg-quick up awg1
: > /var/lib/stub/log.jsonl
