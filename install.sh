# Copyright (c) 2026 Ivan Vasilev
# This source code is licensed under the MIT license found in the
# LICENSE file in the root directory of this source tree.
#!/usr/bin/env bash
# sudo bash <(curl -Ls https://amnesia.ma7neko.ru/forgeting/install.sh)

set -euo pipefail

GRN='\033[0;32m'; YLW='\033[1;33m'; RED='\033[0;31m'; BLU='\033[0;34m'
BLD='\033[1m'; NC='\033[0m'


ok()   { echo -e "  ${GRN}✓${NC}  $*"; }
warn() { echo -e "  ${YLW}⚠${NC}  $*"; }
fail() { trap - ERR; echo -e "\n  ${RED}✗${NC}  $*" >&2; exit 1; }
step() { echo -e "\n${BLD}${BLU}── $* ──${NC}"; }

trap 'rc=$?; echo -e "\n  ${RED}✗ НЕОЖИДАННАЯ ОШИБКА${NC}  строка ${LINENO}  код ${rc}\n     команда: ${BASH_COMMAND}\n     полный лог: ${LOGFILE:-<ещё не открыт>}" >&2' ERR

[[ $EUID -ne 0 ]]          && fail "Запусти от root: sudo bash install.sh"
[[ -z "${BASH_VERSION:-}" ]] && fail "Нужен bash: bash install.sh"

VERSION="0.1.3.1"
BASE_URL="https://amnesia.ma7neko.ru/forgeting"   # хост с install.sh и архивами
PROJECT="/opt/awg-control"
AMNEZIA_DIR="/etc/amnezia"
AWG_DIR="$AMNEZIA_DIR/amneziawg"
PRIV_KEY_FILE="$AMNEZIA_DIR/server_private.key"
PUB_KEY_FILE="$AWG_DIR/server_public.key"
AWG_CONF="$AWG_DIR/awg1.conf"
DB_FILE="$AWG_DIR/users.db"
UI_DB_FILE="$AWG_DIR/ui.db"   # своя БД awg-ui (API-ключи); вне PROJECT — переживает переустановку
# Внутренняя авторизация awg-ui → awg-ctrl (Ed25519): приватный → awg-ui, публичный → awg-ctrl.
INTERNAL_AUTH_PRIV="$AWG_DIR/internal_auth_private.key"
INTERNAL_AUTH_PUB="$AWG_DIR/internal_auth_public.key"
IFACE="awg1"
AWG_PORT="47619"
SUBNET="10.9"
MTU="1376"

# Запуск CLI: tsx напрямую из node_modules (не npx) — чтобы PID был реальным
# (та же причина, что у обёртки /usr/local/bin/awg-ctrl). Нужно обоим режимам.
TSX="$PROJECT/cli/node_modules/.bin/tsx"
CLI="$PROJECT/cli/src/index.ts"

echo -e "${BLD}Forgetting Alpha ${VERSION}${NC}"
echo

# Запускается ДО вопросов и любого деструктива (rm -rf): на несовместимой
# машине лучше упасть сразу, а не после ввода данных или удаления каталога.
# Порты (UDP VPN / порт UI) здесь НЕ проверяем — это отдельно (фаервол/облако).
echo -e "${BLD}Проверка совместимости:${NC}"

KERNEL=$(uname -r)
VIRT=$(systemd-detect-virt 2>/dev/null || echo "unknown")
echo "    Ядро: $KERNEL · виртуализация: $VIRT · $(lsb_release -ds 2>/dev/null || echo unknown)"

# Считаем все три пункта, НЕ падая на первом, — чтобы показать полный чеклист
# со статусом по каждому. Если хоть один не прошёл — печатаем причины и выходим
# с кодом 0 (чистый выход, без вида «упало с ошибкой»).
VIRT_OK=1; HDR_OK=1; NET_OK=1
VIRT_WHY=""; HDR_WHY=""; NET_WHY=""

# 1) Виртуализация: kernel-модуль нельзя загрузить там, где ядро общее с хостом.
case "$VIRT" in
    openvz|lxc|lxc-libvirt|docker|podman|wsl)
        VIRT_OK=0
        VIRT_WHY="виртуализация '$VIRT' — ядро общее с хостом, kernel-модуль AmneziaWG не загрузить (нужен userspace amneziawg-go)"
        ;;
esac

# 2) Заголовки ядра: DKMS соберёт модуль только при их наличии под текущее ядро.
#    Уже в системе или есть кандидат в apt — ок. «(none)» — кастомное ядро, не
#    проходит. Пустой apt-индекс не валим: добьёт поздняя проверка на шаге AWG.
HDR_POLICY=$(apt-cache policy "linux-headers-$KERNEL" 2>/dev/null || true)
if [[ -d "/lib/modules/$KERNEL/build" ]]; then
    :
elif echo "$HDR_POLICY" | grep -q 'Candidate: [^(]'; then
    :
elif echo "$HDR_POLICY" | grep -q 'Candidate: (none)'; then
    HDR_OK=0
    HDR_WHY="нет заголовков под ядро $KERNEL в apt (кастомное ядро провайдера). Решение: apt-get install -y linux-generic && reboot, затем запусти install.sh заново в generic-ядре"
fi

# 3) Доступ в интернет: нужен для архива, Node и пакетов AWG. Без -f: любой
#    HTTP-ответ = связь есть; ненулевой код только при сбое соединения/DNS.
if ! curl -sS --connect-timeout 8 -o /dev/null "$BASE_URL/" 2>/dev/null; then
    NET_OK=0
    NET_WHY="нет доступа к $BASE_URL — проверь интернет и DNS"
fi

# Чеклист: ✓ — пройдено, ✗ — нет.
mark() { [[ "$1" == 1 ]] && echo -e "  ${GRN}✓${NC}  $2" || echo -e "  ${RED}✗${NC}  $2"; }
mark "$VIRT_OK" "Виртуализация"
mark "$HDR_OK"  "Заголовки ядра"
mark "$NET_OK"  "Доступ в интернет"

if [[ "$VIRT_OK" == 0 || "$HDR_OK" == 0 || "$NET_OK" == 0 ]]; then
    echo
    warn "Установка невозможна — не пройдены проверки:"
    [[ "$VIRT_OK" == 0 ]] && echo -e "    ${RED}•${NC} $VIRT_WHY"
    [[ "$HDR_OK"  == 0 ]] && echo -e "    ${RED}•${NC} $HDR_WHY"
    [[ "$NET_OK"  == 0 ]] && echo -e "    ${RED}•${NC} $NET_WHY"
    echo
    trap - ERR
    exit 0
fi

# Наличие базы определяем заранее и молча — от него зависит, спросим ли ниже имя
# сервера (если база есть, имя берётся из неё). Без вывода. KEEP_DATA — отдельно.
DB_EXISTS="n"
[[ -f "$DB_FILE" ]] && DB_EXISTS="y"
echo

# TODO: рандомизировать параметры обфускации при установке.
#   awg-ctrl уже читает их из [Interface] awg1.conf (readAwgParams), поэтому
#   достаточно генерировать случайные значения здесь — править awg-ctrl не нужно.
#   Ограничения, которые обязан соблюсти генератор:
#     - Jc: 3–10 (больше — лишний трафик); Jmin < Jmax, оба < MTU
#     - S1, S2: < ~150, в части версий S1 != S2
#     - H1–H4: уникальны между собой, НЕ равны 1/2/3/4 (зарезервированные
#       типы сообщений WireGuard), большие uint32 без пересечений
#     - I1–I5 НЕ трогать: сейчас уходят в vpn:// ключ пустыми плейсхолдерами
#   Параметры фиксируются на весь срок жизни сервера: при KEEP_DATA=y их
#   менять нельзя (иначе все ранее выданные vpn:// ключи станут невалидными).
JC=6; JMIN=10; JMAX=50
S1=90; S2=45; S3=37; S4=14
H1="1224800044-2116730834"
H2="2122053282-2133204808"
H3="2133604274-2140756116"
H4="2143656228-2147444225"

NET_IFACE=$(ip route show default 2>/dev/null | awk '/default/{print $5; exit}')
[[ -z "$NET_IFACE" ]] && fail "Не могу определить сетевой интерфейс"

# Сначала корректно через CLI (по PID-файлам), затем добиваем всё, что ещё
# держит файлы проекта: осиротевшие процессы, ручной запуск или stale PID,
# которые `stop all` не находит. Вызывать ДО `rm -rf $PROJECT` — иначе node
# продолжит работать с уже удалёнными файлами и порт/интерфейс останутся занятыми.
kill_related() {
    local _tsx="$PROJECT/cli/node_modules/.bin/tsx"
    local _cli="$PROJECT/cli/src/index.ts"
    [[ -x "$_tsx" && -f "$_cli" ]] && "$_tsx" "$_cli" stop all 2>/dev/null || true

    # cmdline всех сервисов (tsx awg-ctrl/awg-ui/cli) содержит путь проекта.
    if command -v pkill &>/dev/null; then
        pkill -TERM -f "$PROJECT/" 2>/dev/null || true
        sleep 1
        pkill -KILL -f "$PROJECT/" 2>/dev/null || true
    else
        local _pids
        _pids=$(ps -eo pid=,args= | awk -v p="$PROJECT/" 'index($0,p){print $1}')
        [[ -n "$_pids" ]] && kill -TERM $_pids 2>/dev/null || true
        sleep 1
        _pids=$(ps -eo pid=,args= | awk -v p="$PROJECT/" 'index($0,p){print $1}')
        [[ -n "$_pids" ]] && kill -KILL $_pids 2>/dev/null || true
    fi

    rm -f /tmp/awg-ctrl.pid /tmp/awg-ui.pid 2>/dev/null || true
}

# Логирование: дублируем stdout/stderr в файл лога через tee. LOGFILE — глобал,
# на него ссылается ERR-трап.
start_logging() {
    LOGFILE="/var/log/awg-install-$(date +%Y%m%d-%H%M%S).log"
    exec > >(tee -a "$LOGFILE") 2>&1
    echo -e "  ${BLD}Лог установки:${NC} $LOGFILE"
}

# Скачать архив версии VERSION, распаковать в PROJECT и проверить, что ключевые
# файлы на месте.
download_extract() {
    local url="$BASE_URL/awgcontrol-${VERSION}.tar.gz"
    local tmp="/tmp/awgcontrol-${VERSION}.tar.gz"
    echo "  → версия: ${VERSION}"
    curl -fsSL --connect-timeout 15 "$url" -o "$tmp" \
        || fail "Не удалось скачать архив: $url"
    ok "Архив скачан: $(du -sh "$tmp" | cut -f1)"

    mkdir -p "$PROJECT"
    tar -xzf "$tmp" -C "$PROJECT" --strip-components=1 \
        || fail "Не удалось распаковать архив"
    rm -f "$tmp"
    ok "Распакован → $PROJECT"

    local f
    for f in \
        "$PROJECT/awg-ctrl/index.ts" \
        "$PROJECT/awg-ctrl/package.json" \
        "$PROJECT/awg-ui/public/index.html" \
        "$PROJECT/awg-ui/server.ts" \
        "$PROJECT/awg-ui/package.json" \
        "$PROJECT/cli/src/index.ts" \
        "$PROJECT/cli/package.json"
    do
        [[ -f "$f" ]] || fail "Файл не найден после распаковки: $f"
    done
    ok "Архив проверен"
}

# npm install во всех трёх сервисах.
npm_install_all() {
    local SVC
    for SVC in awg-ctrl awg-ui cli; do
        if [[ -f "$PROJECT/$SVC/package.json" ]]; then
            echo -n "  $SVC ... "
            (cd "$PROJECT/$SVC" && npm install --silent) \
                || fail "npm install в $SVC завершился ошибкой"
            echo -e "${GRN}ok${NC}"
        else
            warn "$PROJECT/$SVC/package.json не найден — пропуск"
        fi
    done
}

# systemd-юнит: автозапуск awg-control на загрузке. Модель CLI — детач-процессы
# с PID-файлами (а не один долгоживущий процесс), поэтому Type=oneshot +
# RemainAfterExit: systemd держит юнит «active», процессами рулит CLI через
# start/stop. awg-ctrl падает, если awg1 не поднят, а на загрузке awg-quick up
# вручную не выполняется — поднимаем интерфейс в ExecStartPre (если ещё не поднят).
SERVICE_UNIT="/etc/systemd/system/awg-control.service"
setup_service() {
    # node может стоять вне дефолтного PATH systemd (nvm и т.п.), а tsx
    # запускается через shebang `#!/usr/bin/env node` — иначе ExecStart падает с
    # кодом 127 «node not found». Прописываем реальный каталог node в PATH юнита;
    # sbin тоже включаем (awg-quick зовёт iptables/ip/sysctl).
    local node_dir
    node_dir=$(dirname "$(command -v node 2>/dev/null || echo /usr/bin/node)")
    cat > "$SERVICE_UNIT" <<UNIT
[Unit]
Description=AWG Control — awg-ctrl + awg-ui (Forgetting)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=PATH=${node_dir}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ExecStartPre=/bin/sh -c 'awg show ${IFACE} >/dev/null 2>&1 || awg-quick up ${IFACE}'
ExecStart=/usr/local/bin/awg-ctrl start all
ExecStop=/usr/local/bin/awg-ctrl stop all

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    systemctl enable awg-control.service >/dev/null 2>&1 \
        || warn "systemctl enable awg-control не удался — автозапуск не настроен"
    ok "systemd-юнит awg-control установлен (автозапуск на загрузке)"
}

# Запуск/перезапуск сервисов + статус. Через systemd, если он есть (тогда
# работает автозапуск на загрузке); иначе — напрямую через CLI, без автозапуска.
start_and_status() {
    if command -v systemctl >/dev/null 2>&1; then
        setup_service
        systemctl restart awg-control.service \
            || fail "systemctl restart awg-control завершился ошибкой"
    else
        warn "systemd не найден — запускаю напрямую через CLI (без автозапуска)"
        "$TSX" "$CLI" start all
    fi
    sleep 2
    echo
    "$TSX" "$CLI" status
}

INSTALL_MODE="fresh"
if [[ -d "$PROJECT" ]]; then
    echo
    warn "Найден каталог $PROJECT"
    echo "  1) Обновить до версии ${VERSION} — сохранить настройки и пользователей"
    echo "  2) Полностью переустановить — удалить каталог и начать заново"
    read -rp "  Выбери [1/2]: " INST_CHOICE
    case "${INST_CHOICE:-1}" in
        1) INSTALL_MODE="update" ;;
        2) INSTALL_MODE="fresh" ;;
        *) fail "Неверный выбор: введи 1 или 2" ;;
    esac
    echo
fi

if [[ "$INSTALL_MODE" == "update" ]]; then
    start_logging
    step "Обновление до версии ${VERSION}"

    echo "  → останавливаем и убиваем все процессы awg-control"
    kill_related

    download_extract
    npm_install_all
    start_and_status

    echo
    echo -e "${BLD}${GRN}✓ Обновление завершено — версия ${VERSION}${NC}"
    echo
    exit 0
fi

# При полной переустановке убиваем все процессы awg-control и очищаем старый
# каталог, чтобы не оставалось ни запущенных процессов, ни старых файлов.
if [[ -d "$PROJECT" ]]; then
    kill_related
    rm -rf "$PROJECT"
fi

step "Конфигурация"

# Server IP определяем автоматически (внешний через ifconfig.me, иначе локальный
# по маршруту) — не спрашиваем. Если определить не удалось — падаем.
SERVER_IP=$(curl -s4 --connect-timeout 5 ifconfig.me 2>/dev/null \
         || ip route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}' \
         || echo "")
[[ -z "$SERVER_IP" ]] && fail "Не удалось определить Server IP автоматически"
ok "Server IP: $SERVER_IP"

# Имя сервера спрашиваем ниже — только если базы ещё нет (DB_EXISTS=n).
# Если база есть, имя берётся из неё.

# Внутренняя авторизация awg-ui ↔ awg-ctrl — асимметричная пара, генерируется
# в шаге 4 (Ключи). Здесь секрет не нужен.
AWGCTRL_PORT=$(( (RANDOM % 22768) + 32768 ))

read -rp "  UI port (Enter — случайный): " UI_PORT
UI_PORT="${UI_PORT:-$(( (RANDOM % 22768) + 32768 ))}"

read -rp "  UI логин [admin]: " UI_USER
UI_USER="${UI_USER:-admin}"

SUGGESTED_PASS=$(tr -dc 'a-zA-Z0-9' < /dev/urandom | head -c 12 2>/dev/null || openssl rand -hex 6)
read -rsp "  UI пароль [$SUGGESTED_PASS]: " UI_PASS; echo
UI_PASS="${UI_PASS:-$SUGGESTED_PASS}"

JWT_SECRET=$(tr -dc 'a-zA-Z0-9' < /dev/urandom | head -c 32 2>/dev/null || openssl rand -hex 16)

# Если найдены база пользователей и ключи сервера — предлагаем их сохранить.
# Важно: vpn:// ключи пользователей привязаны к ключу сервера, поэтому
# сохранять базу имеет смысл только вместе со старым ключом сервера.
KEEP_DATA="n"
if [[ -f "$DB_FILE" || -f "$PRIV_KEY_FILE" ]]; then
    echo
    warn "Найдена существующая установка:"
    [[ -f "$DB_FILE" ]]       && echo "    база пользователей: $DB_FILE"
    [[ -f "$PRIV_KEY_FILE" ]] && echo "    ключ сервера:       $PRIV_KEY_FILE"

    if [[ -f "$DB_FILE" && -f "$PRIV_KEY_FILE" && -f "$PUB_KEY_FILE" ]]; then
        read -rp "  Сохранить пользователей и ключ сервера? [Y/n]: " KD
        [[ "${KD:-y}" =~ ^[Yy]$ || -z "${KD}" ]] && KEEP_DATA="y"
    else
        warn "Для сохранения нужны и база, и оба ключа сервера — часть отсутствует."
        warn "Пользователей не сохранить (vpn:// ключи стали бы невалидными)."
    fi
fi

# Имя сервера: если база уже существует (DB_EXISTS, проверено заранее в
# preflight) — имя берётся из неё, не спрашиваем. Если базы нет — спрашиваем.
if [[ "$DB_EXISTS" == "y" ]]; then
    SERVER_NAME="VPN"
    echo "  Server name: берётся из существующей базы (пропускаем)"
else
    read -rp "  Server name [VPN]: " SERVER_NAME
    SERVER_NAME="${SERVER_NAME:-VPN}"
fi

echo "    Project dir:   $PROJECT"
echo "    Server IP:     $SERVER_IP"
echo "    Server name:   $SERVER_NAME"
echo "    AWG port:      $AWG_PORT  (udp)"
echo "    awgctrl port:  $AWGCTRL_PORT"
echo "    UI port:       $UI_PORT"
echo "    UI логин:      $UI_USER"
echo "    Net interface: $NET_IFACE"
if [[ "$KEEP_DATA" == "y" ]]; then
    echo -e "    Данные:        ${GRN}сохранить существующих пользователей и ключ${NC}"
elif [[ -f "$DB_FILE" ]]; then
    echo -e "    Данные:        ${YLW}новая установка (старая база → бэкап)${NC}"
fi
echo

read -rp "  Продолжить? [Y/n]: " YN
[[ "${YN:-y}" =~ ^[Nn]$ ]] && { echo "  Отменено."; exit 0; }

start_logging

# Совместимость (виртуализация, заголовки ядра, интернет) проверена выше в
# секции «Проверка совместимости»; $KERNEL задан там же.
step "1/7  AmneziaWG"

if command -v awg &>/dev/null && command -v awg-quick &>/dev/null && modinfo amneziawg &>/dev/null; then
    ok "AWG уже установлен (модуль amneziawg: $(modinfo -F version amneziawg 2>/dev/null || echo present))"
else
    echo "  → apt-get update"
    apt-get update || fail "apt-get update упал — проверь /etc/apt/sources.list*"

    echo "  → установка зависимостей сборки + заголовков ядра"
    DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
    apt-get install -y \
        software-properties-common \
        python3-launchpadlib \
        gnupg2 \
        dkms \
        build-essential \
        "linux-headers-$KERNEL" \
        linux-headers-generic \
        || fail "Не удалось установить зависимости или заголовки ядра"

    if [[ ! -d "/lib/modules/$KERNEL/build" ]]; then
        warn "Нет /lib/modules/$KERNEL/build — заголовки под текущее ядро отсутствуют."

        warn "Часто бывает на кастомном ядре провайдера. Решение:"
        warn "    apt-get install -y linux-generic && reboot"
        warn "и после загрузки в generic-ядро запусти install.sh заново."
        fail "Отсутствуют заголовки ядра $KERNEL — DKMS не соберёт модуль"
    fi
    ok "Заголовки ядра на месте: /lib/modules/$KERNEL/build"

    echo "  → add-apt-repository ppa:amnezia/ppa"
    add-apt-repository -y ppa:amnezia/ppa \
        || fail "Не удалось добавить PPA ppa:amnezia/ppa"

    echo "  → apt-get update (после PPA)"
    apt-get update \
        || fail "apt-get update после PPA упал — проверь источники amnezia в /etc/apt"

    echo "  → установка amneziawg (сборка DKMS-модуля, может занять до минуты)"
    if ! DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
         apt-get install -y amneziawg; then
        warn "apt-get install amneziawg завершился с ошибкой."
        MKLOG=$(ls -1t /var/lib/dkms/amneziawg/*/build/make.log 2>/dev/null | head -1 || true)
        if [[ -n "${MKLOG:-}" && -f "$MKLOG" ]]; then
            echo "  ───── $MKLOG (последние 40 строк) ─────"
            tail -n 40 "$MKLOG" | sed 's/^/    /'
            echo "  ───────────────────────────────────────────────"
        else
            warn "make.log не найден — ошибка, вероятно, на этапе apt/репозиториев."
        fi
        fail "Не удалось установить amneziawg (детали выше и в $LOGFILE)"
    fi

    echo "  → проверка собранного модуля"
    dkms status amneziawg 2>/dev/null | sed 's/^/    /' || true
    if ! modprobe amneziawg 2>/dev/null; then
        MKLOG=$(ls -1t /var/lib/dkms/amneziawg/*/build/make.log 2>/dev/null | head -1 || true)
        [[ -n "${MKLOG:-}" && -f "$MKLOG" ]] && { echo "  ── make.log (tail) ──"; tail -n 40 "$MKLOG" | sed 's/^/    /'; }
        fail "Модуль amneziawg не загрузился — DKMS-сборка несовместима с ядром"
    fi
    ok "AWG установлен, модуль amneziawg собран и загружается"
fi

step "2/7  Node.js"

if command -v node &>/dev/null; then
    ok "Node.js уже установлен: $(node --version)"
else
    echo "  → установка Node.js 20.x (nodesource)"
    curl -fsSL https://deb.nodesource.com/setup_20.x | bash -
    apt-get install -y nodejs || fail "Не удалось установить Node.js"
    ok "Node.js установлен: $(node --version)"
fi

step "3/7  Файлы проекта"

download_extract

step "4/7  Ключи и конфиг AWG"

mkdir -p "$AWG_DIR" "$AMNEZIA_DIR"

AWG_BIN=$(which awg || echo /usr/bin/awg)

if [[ "$KEEP_DATA" == "y" ]]; then
    # Переиспользуем существующий ключ сервера — иначе старые vpn:// ключи
    # пользователей в users.db станут невалидными.
    PRIV_KEY=$(cat "$PRIV_KEY_FILE")
    PUB_KEY=$(cat "$PUB_KEY_FILE")
    ok "Используются существующие ключи сервера (база сохранена): $PUB_KEY"
else
    # Новая установка. Существующую базу не удаляем, а отправляем в бэкап,
    # чтобы awg-ctrl создал чистую users.db при старте.
    if [[ -f "$DB_FILE" ]]; then
        DB_BAK="${DB_FILE}.bak-$(date +%Y%m%d-%H%M%S)"
        mv "$DB_FILE" "$DB_BAK"
        warn "Старая база пользователей сохранена: $DB_BAK"
    fi
    # ui.db (API-ключи awg-ui) — тоже в бэкап, чтобы awg-ui создал чистую БД.
    if [[ -f "$UI_DB_FILE" ]]; then
        UI_DB_BAK="${UI_DB_FILE}.bak-$(date +%Y%m%d-%H%M%S)"
        mv "$UI_DB_FILE" "$UI_DB_BAK"
        # WAL-сайдкары удаляем — к новой БД они неприменимы.
        rm -f "${UI_DB_FILE}-wal" "${UI_DB_FILE}-shm"
        warn "Старая база API-ключей сохранена: $UI_DB_BAK"
    fi

    PRIV_KEY=$(umask 077 && awg genkey)
    PUB_KEY=$(printf '%s' "$PRIV_KEY" | awg pubkey)

    umask 077
    printf '%s' "$PRIV_KEY" > "$PRIV_KEY_FILE"
    printf '%s' "$PUB_KEY"  > "$PUB_KEY_FILE"
    chmod 600 "$PRIV_KEY_FILE" "$PUB_KEY_FILE"

    ok "Публичный ключ: $PUB_KEY"
fi

# Внутренняя авторизация awg-ui → awg-ctrl: Ed25519-пара. Приватный → awg-ui,
# публичный → awg-ctrl. Перегенерируется при каждой установке (эфемерна: обе
# стороны переписываются вместе) — даже при KEEP_DATA, на vpn:// ключи не влияет.
( umask 077
  openssl genpkey -algorithm ed25519 -out "$INTERNAL_AUTH_PRIV"
  openssl pkey -in "$INTERNAL_AUTH_PRIV" -pubout -out "$INTERNAL_AUTH_PUB" )
chmod 600 "$INTERNAL_AUTH_PRIV" "$INTERNAL_AUTH_PUB"
ok "Ключи внутренней авторизации awg-ui ↔ awg-ctrl"

grep -qxF 'net.ipv4.ip_forward=1' /etc/sysctl.conf \
    || echo 'net.ipv4.ip_forward=1' >> /etc/sysctl.conf
grep -qxF 'net.ipv6.conf.all.forwarding=1' /etc/sysctl.conf \
    || echo 'net.ipv6.conf.all.forwarding=1' >> /etc/sysctl.conf
sysctl -qp
ok "IP forwarding включён"

cat > "$AWG_CONF" <<CONF
[Interface]
Address = ${SUBNET}.0.1/16
MTU = ${MTU}
PostUp = ${AWG_BIN} set ${IFACE} private-key ${PRIV_KEY_FILE}; iptables -A FORWARD -i ${IFACE} -j ACCEPT; iptables -t nat -A POSTROUTING -o ${NET_IFACE} -j MASQUERADE
PreDown = iptables -D FORWARD -i ${IFACE} -j ACCEPT; iptables -t nat -D POSTROUTING -o ${NET_IFACE} -j MASQUERADE
ListenPort = ${AWG_PORT}
PrivateKey = ${PRIV_KEY}
Jc = ${JC}
Jmin = ${JMIN}
Jmax = ${JMAX}
S1 = ${S1}
S2 = ${S2}
S3 = ${S3}
S4 = ${S4}
H1 = ${H1}
H2 = ${H2}
H3 = ${H3}
H4 = ${H4}
CONF

chmod 600 "$AWG_CONF"
ok "$AWG_CONF"

step "5/7  Запуск AWG"

if awg show "$IFACE" &>/dev/null; then
    warn "Интерфейс $IFACE уже существует — перезапускаем"
    awg-quick down "$IFACE" 2>/dev/null || true
fi

awg-quick up "$IFACE" || fail "awg-quick up $IFACE завершился с ошибкой"

# awg-quick не всегда применяет приватный ключ из [Interface]/PostUp
# (наблюдалось: awg show public-key = none сразу после up). Если ключ не
# совпал — доставляем его явно из файла и проверяем ещё раз.
RUNNING_PUB=$(awg show "$IFACE" public-key 2>/dev/null || echo "")
if [[ "$RUNNING_PUB" != "$PUB_KEY" ]]; then
    awg set "$IFACE" private-key "$PRIV_KEY_FILE" \
        || fail "Не удалось применить приватный ключ к $IFACE"
    RUNNING_PUB=$(awg show "$IFACE" public-key 2>/dev/null || echo "")
fi

if [[ "$RUNNING_PUB" == "$PUB_KEY" ]]; then
    ok "Интерфейс $IFACE запущен, ключ применён"
else
    fail "Ключ на $IFACE не совпал: ${RUNNING_PUB:-none} ≠ $PUB_KEY"
fi

step "6/7  npm install"

npm_install_all

# Глобальный бинарник — используем tsx напрямую из node_modules (не npx)
# чтобы child.pid в cli был реальным PID процесса
cat > /usr/local/bin/awg-ctrl << 'WRAPPER'
#!/usr/bin/env bash
exec /opt/awg-control/cli/node_modules/.bin/tsx \
     /opt/awg-control/cli/src/index.ts "$@"
WRAPPER
chmod +x /usr/local/bin/awg-ctrl
ok "awg-ctrl → /usr/local/bin/awg-ctrl"

step "7/7  Запуск сервисов"

cat > "$PROJECT/cli/cli.env" <<ENV
# Generated by install.sh — $(date -u '+%Y-%m-%d %H:%M UTC')

# ── awgctrl (Ring 0) ──────────────────────────────────────────────────────
AWGCTRL_PORT=${AWGCTRL_PORT}
SERVER_IP=${SERVER_IP}
SERVER_PORT=${AWG_PORT}
SERVER_NAME=${SERVER_NAME}

# ── внутренняя авторизация awg-ui → awg-ctrl (Ed25519) ─────────────────────
INTERNAL_AUTH_KEY_FILE=${INTERNAL_AUTH_PRIV}
INTERNAL_AUTH_PUB_FILE=${INTERNAL_AUTH_PUB}

# ── ui (Ring 4) ───────────────────────────────────────────────────────────
UI_PORT=${UI_PORT}
UI_USER=${UI_USER}
UI_PASS=${UI_PASS}
JWT_SECRET=${JWT_SECRET}
ENV

chmod 600 "$PROJECT/cli/cli.env"
ok "$PROJECT/cli/cli.env"

start_and_status

echo
echo -e "${BLD}${GRN}✓ Установка завершена${NC}"
echo
echo -e "  ${BLD}Сервисы:${NC}"
echo "    awg-ctrl  →  http://localhost:${AWGCTRL_PORT}  (внутренний)"
echo "    awg-ui    →  http://${SERVER_IP}:${UI_PORT}"
echo
echo -e "  ${BLD}${YLW}UI логин:${NC}   ${UI_USER}"
echo -e "  ${BLD}${YLW}UI пароль:${NC}  ${UI_PASS}"
echo
echo -e "  ${BLD}Управление:${NC}"
echo "    awg-ctrl                         — CLI: start/stop/status/credentials"
echo "    systemctl start|stop awg-control — сервис (автозапуск на загрузке включён)"
echo
