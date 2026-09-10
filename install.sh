# Copyright (c) 2026 Ivan Vasilev
# This source code is licensed under the MIT license found in the
# LICENSE file in the root directory of this source tree.
#!/usr/bin/env bash

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

# ⚠️ Дублирует .env из репозитория: установщик собирает URL релиза и печатает
# баннер ДО того, как архив с .env скачан, поэтому взять их оттуда не может.
# При бампе версии правь оба места.
VERSION="0.2.1"
CHANNEL="${CHANNEL:-Beta}"

# Откуда брать архив и как называется продукт — задаётся окружением, в коде
# ничего не зашито. У Gitea и GitHub путь релиза одинаковый
# (<repo>/releases/download/v<VERSION>/<file>), поэтому одна схема покрывает оба.
#   REPO_BASE     — основной источник (своё зеркало);
#   REPO_FALLBACK — запасной, если в основном релиза ещё нет;
#   ARCHIVE_URL   — полный URL архива, перекрывает оба варианта;
#   BRAND         — имя продукта в баннерах, systemd-юните и панели; по
#                   умолчанию берётся из .env в архиве, здесь только запасное
#                   значение для баннера самого установщика.
BRAND_OVERRIDE="${BRAND:-}"
BRAND="${BRAND:-Forgetting}"
REPO_BASE="${REPO_BASE:-https://git.ma7neko.ru/maeneko/forgetting}"
REPO_FALLBACK="${REPO_FALLBACK:-https://github.com/maeneko/forgetting}"
ARCHIVE_NAME="awgcontrol-${VERSION}.tar.gz"
ARCHIVE_URL="${ARCHIVE_URL:-}"
url_host() { sed -E 's#^https?://([^/]+).*#\1#' <<< "$1"; }
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

echo -e "${BLD}${BRAND} ${CHANNEL} ${VERSION}${NC}"
echo

# Запускается ДО вопросов и любого деструктива (rm -rf): на несовместимой
# машине лучше упасть сразу, а не после ввода данных или удаления каталога.
# Порты (UDP VPN / порт UI) здесь НЕ проверяем — это отдельно (фаервол/облако).
echo -e "${BLD}Проверка совместимости:${NC}"

KERNEL=$(uname -r)
VIRT=$(systemd-detect-virt 2>/dev/null || echo "unknown")

# Семейство дистрибутива решает, как ставить AmneziaWG:
#   ubuntu — add-apt-repository ppa:amnezia/ppa, заголовки linux-headers-generic;
#   debian — ни software-properties-common (в Debian 13 его нет), ни
#            linux-headers-generic там не существует: PPA подключаем вручную
#            (ключ + signed-by), заголовки — только под текущее ядро.
# Производные (Mint, Pop!_OS, Kali…) определяются через ID_LIKE.
OS_ID=""; OS_LIKE=""; OS_CODENAME=""; OS_NAME=""
if [[ -r /etc/os-release ]]; then
    OS_ID=$(. /etc/os-release; echo "${ID:-}")
    OS_LIKE=$(. /etc/os-release; echo "${ID_LIKE:-}")
    OS_CODENAME=$(. /etc/os-release; echo "${VERSION_CODENAME:-}")
    OS_NAME=$(. /etc/os-release; echo "${PRETTY_NAME:-}")
fi
if [[ "$OS_ID" == "ubuntu" || " $OS_LIKE " == *" ubuntu "* ]]; then
    OS_FAMILY="ubuntu"
elif [[ "$OS_ID" == "debian" || " $OS_LIKE " == *" debian "* ]]; then
    OS_FAMILY="debian"
else
    OS_FAMILY=""
fi
# Вариант ядра без версии: 6.12.85+deb13-cloud-amd64 → cloud-amd64,
# 6.1.0-18-cloud-amd64 → cloud-amd64. Нужен для мета-пакетов Debian
# (linux-image-<вариант> / linux-headers-<вариант>).
KFLAVOUR=$(sed -E 's/^[0-9][^-]*-([0-9]+-)?//' <<< "$KERNEL")
# Что советовать, если заголовков под текущее ядро нет.
if [[ "$OS_FAMILY" == "debian" ]]; then
    HDR_FIX="apt-get update && apt-get install -y linux-image-$KFLAVOUR linux-headers-$KFLAVOUR && reboot"
else
    HDR_FIX="apt-get install -y linux-generic && reboot"
fi

echo "    Ядро: $KERNEL · виртуализация: $VIRT · ${OS_NAME:-$(lsb_release -ds 2>/dev/null || echo unknown)}"

# Считаем все пункты, НЕ падая на первом, — чтобы показать полный чеклист
# со статусом по каждому. Если хоть один не прошёл — печатаем причины и выходим
# с кодом 0 (чистый выход, без вида «упало с ошибкой»).
OS_OK=1; VIRT_OK=1; HDR_OK=1; NET_OK=1
OS_WHY=""; VIRT_WHY=""; HDR_WHY=""; NET_WHY=""

# 0) Дистрибутив: установщик умеет только apt-семейство Ubuntu/Debian.
if [[ -z "$OS_FAMILY" ]]; then
    OS_OK=0
    OS_WHY="дистрибутив '${OS_NAME:-${OS_ID:-неизвестно}}' не поддерживается — нужен Ubuntu или Debian"
fi

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
    HDR_WHY="нет заголовков под ядро $KERNEL в apt. Решение: $HDR_FIX, затем запусти install.sh заново"
fi

# 3) Доступ в интернет: нужен для архива, Node и пакетов AWG. Без -f: любой
#    HTTP-ответ = связь есть; ненулевой код только при сбое соединения/DNS.
#    Проверяем хосты, с которых реально качаем: хватает любого из двух —
#    download_extract() умеет откатываться на запасной.
NET_HOSTS=("$(url_host "$REPO_BASE")")
[[ -n "$ARCHIVE_URL" ]] && NET_HOSTS=("$(url_host "$ARCHIVE_URL")")
[[ -z "$ARCHIVE_URL" && "$(url_host "$REPO_FALLBACK")" != "${NET_HOSTS[0]}" ]] \
    && NET_HOSTS+=("$(url_host "$REPO_FALLBACK")")
NET_OK=0
for h in "${NET_HOSTS[@]}"; do
    if curl -sS --connect-timeout 8 -o /dev/null "https://$h" 2>/dev/null; then NET_OK=1; break; fi
done
[[ "$NET_OK" == 0 ]] && NET_WHY="нет доступа к ${NET_HOSTS[*]} — проверь интернет и DNS"

# Чеклист: ✓ — пройдено, ✗ — нет.
mark() { [[ "$1" == 1 ]] && echo -e "  ${GRN}✓${NC}  $2" || echo -e "  ${RED}✗${NC}  $2"; }
mark "$OS_OK"   "Дистрибутив"
mark "$VIRT_OK" "Виртуализация"
mark "$HDR_OK"  "Заголовки ядра"
mark "$NET_OK"  "Доступ в интернет"

if [[ "$OS_OK" == 0 || "$VIRT_OK" == 0 || "$HDR_OK" == 0 || "$NET_OK" == 0 ]]; then
    echo
    warn "Установка невозможна — не пройдены проверки:"
    [[ "$OS_OK"   == 0 ]] && echo -e "    ${RED}•${NC} $OS_WHY"
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
#     - S1–S4: при заданном HeaderProtectionKey (AmneziaWG 3.x) НИ ОДНО из них
#       не может быть меньше 12 (HEADER_PROTECTION_NONCE_SIZE). Модуль на
#       нарушение отвечает только «Invalid argument», причина видна лишь при
#       `echo "module amneziawg +p" > /sys/kernel/debug/dynamic_debug/control`
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

# Поколение протокола и закрепление версии пакета — управляются переменными
# окружения, задаются ДО запуска: `AWG_GEN=auto bash install.sh`.
#   AWG_GEN=3.1  (по умолчанию) — основное поколение: если модуль в ядре не 3.x
#     или ядро старше 5.5, install.sh падает с диагностикой. Тихий откат на 2.0
#     здесь нежелателен: он молча выдал бы сервер со слабой обфускацией.
#   AWG_GEN=auto — старое поведение: 3.1, если условия выполняются, иначе 2.0
#     без вопросов. Для установки на заведомо старое ядро/модуль.
#   AWG_GEN=2.0  — не трогать модуль (ни апгрейд, ни перезагрузка), сразу 2.0.
# Сервер, установленный на 2.0, переводится на 3.1 позже из панели
# (POST /awg/upgrade) — переустановка для этого не нужна.
AWG_GEN="${AWG_GEN:-3.1}"
[[ "$AWG_GEN" =~ ^(auto|2\.0|3\.1)$ ]] || fail "AWG_GEN должен быть auto, 2.0 или 3.1 (задано: $AWG_GEN)"
# AWG_PIN=y (по умолчанию) — после успешной установки держит amneziawg{,-dkms,-tools}
# через `apt-mark hold`: unattended-upgrades не должен подменять модуль под живым
# сервером (issue #215 — именно так словили регрессию между dkms-сборками).
AWG_PIN="${AWG_PIN:-y}"

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
    local tmp="/tmp/${ARCHIVE_NAME}"
    local keep_tarball=0

    # AWG_LOCAL_TARBALL — поставить сборку, которой ещё нет в релизах (или
    # встать без сети). Архив должен быть той же формы, что делает CI:
    # awg-ctrl/ + awg-ui/ (с готовым dist/) + cli/ + .env, без node_modules.
    if [[ -n "${AWG_LOCAL_TARBALL:-}" ]]; then
        [[ -f "$AWG_LOCAL_TARBALL" ]] \
            || fail "AWG_LOCAL_TARBALL указан, но файл не найден: $AWG_LOCAL_TARBALL"
        tmp="$AWG_LOCAL_TARBALL"
        keep_tarball=1          # чужой файл — после распаковки не удаляем
        echo "  → локальный архив: $tmp"
        ok "Архив взят локально: $(du -sh "$tmp" | cut -f1)"
    else
        local urls=()
        if [[ -n "$ARCHIVE_URL" ]]; then
            urls=("$ARCHIVE_URL")
        else
            urls=("$REPO_BASE/releases/download/v${VERSION}/${ARCHIVE_NAME}")
            [[ "$REPO_FALLBACK" != "$REPO_BASE" ]] \
                && urls+=("$REPO_FALLBACK/releases/download/v${VERSION}/${ARCHIVE_NAME}")
        fi

        echo "  → версия: ${VERSION}"
        local url got=""
        for url in "${urls[@]}"; do
            if curl -fsSL --connect-timeout 15 "$url" -o "$tmp"; then got="$url"; break; fi
            warn "Архив недоступен на $(url_host "$url") — пробуем следующий источник"
        done
        [[ -n "$got" ]] || fail "Не удалось скачать архив ни с одного источника:
$(printf '    %s\n' "${urls[@]}")
    Укажи свой: ARCHIVE_URL=https://…/${ARCHIVE_NAME} bash install.sh"
        ok "Архив скачан с $(url_host "$got"): $(du -sh "$tmp" | cut -f1)"
    fi

    mkdir -p "$PROJECT"
    tar -xzf "$tmp" -C "$PROJECT" --strip-components=1 \
        || fail "Не удалось распаковать архив"
    [[ "$keep_tarball" == 1 ]] || rm -f "$tmp"
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
Description=AWG Control — awg-ctrl + awg-ui (${BRAND})
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

# Debian: add-apt-repository нет (software-properties-common из Debian 13 убран),
# а PPA под Debian-релизы не собирается — подключаем его вручную на Ubuntu-серию
# с glibc не новее дебиановской. Модуль приезжает DKMS-исходниками и от серии
# не зависит; серия важна только для бинарника amneziawg-tools.
#   AMNEZIA_PPA_SUITE — задать серию вручную (focal/jammy/noble).
AMNEZIA_PPA_URL="https://ppa.launchpadcontent.net/amnezia/ppa/ubuntu"
AMNEZIA_PPA_FPR="75C9DD72C799870E310542E24166F2C257290828"   # Launchpad PPA for Iurii Egorov
AMNEZIA_KEYRING="/etc/apt/keyrings/amnezia-ppa.gpg"
add_amnezia_ppa_debian() {
    local suite="${AMNEZIA_PPA_SUITE:-}"
    if [[ -z "$suite" ]]; then
        case "$OS_CODENAME" in
            bullseye) suite="focal" ;;
            bookworm) suite="jammy" ;;
            trixie)   suite="noble" ;;
            *)        suite="noble"
                      warn "Debian '${OS_CODENAME:-неизвестно}' не сопоставлен с серией PPA — берём noble (задать вручную: AMNEZIA_PPA_SUITE=…)" ;;
        esac
    fi
    echo "  → подключение PPA amnezia (Debian ${OS_CODENAME:-?} → серия $suite)"

    # Ключ берём по полному отпечатку и сверяем отпечаток после загрузки —
    # apt-key в Debian 12+ уже нет, а доверять ответу keyserver вслепую нельзя.
    local tmp gh fprs=""
    tmp=$(mktemp); gh=$(mktemp -d)
    if curl -fsSL --connect-timeout 15 \
            "https://keyserver.ubuntu.com/pks/lookup?op=get&options=mr&search=0x${AMNEZIA_PPA_FPR}" \
            | gpg --dearmor > "$tmp" 2>/dev/null; then
        fprs=$(GNUPGHOME="$gh" gpg --batch --show-keys --with-colons "$tmp" 2>/dev/null || true)
    fi
    if ! grep -q "^fpr:::::::::${AMNEZIA_PPA_FPR}:" <<< "$fprs"; then
        rm -rf "$tmp" "$gh"
        fail "Не удалось получить ключ PPA amnezia ($AMNEZIA_PPA_FPR) с keyserver.ubuntu.com"
    fi
    install -d -m 0755 /etc/apt/keyrings
    install -m 0644 "$tmp" "$AMNEZIA_KEYRING"
    rm -rf "$tmp" "$gh"

    echo "deb [signed-by=$AMNEZIA_KEYRING] $AMNEZIA_PPA_URL $suite main" \
        > /etc/apt/sources.list.d/amnezia-ppa.list
    ok "PPA amnezia подключён: $suite, ключ $AMNEZIA_KEYRING"
}

if command -v awg &>/dev/null && command -v awg-quick &>/dev/null && modinfo amneziawg &>/dev/null; then
    ok "AWG уже установлен (модуль amneziawg: $(modinfo -F version amneziawg 2>/dev/null || echo present))"
else
    echo "  → apt-get update"
    apt-get update || fail "apt-get update упал — проверь /etc/apt/sources.list*"

    # iptables — в обоих списках: его зовёт PostUp в awg1.conf, а в Debian 13
    # (nftables по умолчанию) он в минимальной системе не установлен.
    if [[ "$OS_FAMILY" == "debian" ]]; then
        DEPS=(gnupg ca-certificates curl dkms build-essential iptables)
    else
        DEPS=(software-properties-common python3-launchpadlib gnupg2 dkms build-essential iptables)
    fi
    echo "  → установка зависимостей сборки"
    DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
        apt-get install -y "${DEPS[@]}" \
        || fail "Не удалось установить зависимости сборки: ${DEPS[*]}"

    # Заголовки — отдельным шагом: их нехватка — самая частая причина сбоя, и на
    # неё нужен точный совет, а не общее «не удалось установить». Ошибку apt
    # здесь не валим: решает наличие /lib/modules/$KERNEL/build ниже.
    HDR_PKGS=("linux-headers-$KERNEL")
    [[ "$OS_FAMILY" == "ubuntu" ]] && HDR_PKGS+=(linux-headers-generic)
    echo "  → установка заголовков ядра: ${HDR_PKGS[*]}"
    DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
        apt-get install -y "${HDR_PKGS[@]}" \
        || warn "apt не смог поставить ${HDR_PKGS[*]}"

    if [[ ! -d "/lib/modules/$KERNEL/build" ]]; then
        warn "Нет /lib/modules/$KERNEL/build — заголовки под текущее ядро отсутствуют."
        if [[ "$OS_FAMILY" == "debian" ]]; then
            # Debian держит в архиве только последнюю сборку ядра: если сервер
            # давно не перезагружали, пакета под запущенное ядро там уже нет.
            warn "Debian хранит в архиве только свежую сборку ядра — запущенное $KERNEL"
            warn "уже устарело. Поставь актуальное ядро вместе с заголовками:"
        else
            warn "Часто бывает на кастомном ядре провайдера. Решение:"
        fi
        warn "    $HDR_FIX"
        warn "и после перезагрузки запусти install.sh заново."
        fail "Отсутствуют заголовки ядра $KERNEL — DKMS не соберёт модуль"
    fi
    ok "Заголовки ядра на месте: /lib/modules/$KERNEL/build"

    # Если репозиторий amnezia уже настроен (например, ключ и источник заведены
    # руками — add-apt-repository ходит за ключом в Launchpad, а тот периодически
    # отвечает GPGKeyTemporarilyNotFoundError), второй раз его добавлять не нужно.
    # Проверяем именно файлы источников, а не `apt-cache policy`: сразу после
    # apt-get install кэш apt бывает в переходном состоянии и отдаёт пустоту.
    if grep -rqs 'amnezia/ppa' /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null; then
        ok "Репозиторий amnezia уже настроен — add-apt-repository пропущен"
    elif [[ "$OS_FAMILY" == "debian" ]]; then
        add_amnezia_ppa_debian
    else
        echo "  → add-apt-repository ppa:amnezia/ppa"
        add-apt-repository -y ppa:amnezia/ppa \
            || fail "Не удалось добавить PPA ppa:amnezia/ppa"
    fi

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

# Поколение протокола: 3.1 требует модуль 3.x и ядро ≥ 5.5.
#   - модуль: PPA с 30.07.2026 отдаёт 3.x; на старых установках он может быть 2.0,
#     поэтому сначала пробуем обновиться.
#   - ядро: header protection использует библиотечный chacha-API
#     (chacha_init/chacha20_crypt), которого нет до 5.5 — модуль там не соберётся
#     (upstream issue #210). Проблема с nla_put_uint на ядрах < 6.7 уже исправлена.
# При AWG_GEN=3.1 (по умолчанию) недоступность — fail с диагностикой: тихий
# откат подсунул бы более слабую обфускацию незаметно для оператора.
# При AWG_GEN=auto не падаем, а пишем 2.0-конфиг: awg-ctrl определяет поколение
# по наличию HeaderProtectionKey в конфиге, так что всё продолжит работать ровно
# как раньше, а перейти на 3.1 можно потом из панели.

kernel_lt_5_5() {
    local maj min
    maj=${KERNEL%%.*}
    min=${KERNEL#*.}; min=${min%%.*}
    [[ "$maj" -lt 5 || ( "$maj" -eq 5 && "$min" -lt 5 ) ]]
}

# 🛑 Версий у модуля две, и путать их нельзя:
#   modinfo                       — что лежит на диске (после apt/DKMS);
#   /sys/module/amneziawg/version — что реально загружено в ядро.
# `apt --only-upgrade` пересобирает модуль, но НЕ переставляет уже загруженный.
# Ориентироваться на modinfo — значит написать 3.1-конфиг, который старый
# загруженный модуль отвергнет: `awg setconf` вернёт «Unable to modify
# interface: Invalid argument», и awg-quick up упадёт. Решает перезагрузка модуля.
disk_mod_ver()   { modinfo -F version amneziawg 2>/dev/null || echo ""; }
loaded_mod_ver() { cat /sys/module/amneziawg/version 2>/dev/null || echo ""; }

if [[ "$AWG_GEN" == "2.0" ]]; then
    AWG3="n"
    MOD_VER=$(disk_mod_ver)
    ok "Поколение задано вручную (AWG_GEN=2.0) — модуль не трогаем, ставим 2.0."
else
    AWG3="y"

    MOD_VER=$(disk_mod_ver)
    if [[ "${MOD_VER%%.*}" != "3" ]]; then
        warn "Модуль amneziawg версии '${MOD_VER:-неизвестно}' — для AmneziaWG 3.1 нужна 3.x. Пробуем обновить."
        # Прошлая установка могла закрепить пакет (apt-mark hold, см. ниже) —
        # снимаем, иначе --only-upgrade молча не тронет held-версию.
        apt-mark unhold amneziawg amneziawg-dkms amneziawg-tools >/dev/null 2>&1 || true
        apt-get update >/dev/null 2>&1 || true
        DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
            apt-get install -y --only-upgrade amneziawg amneziawg-dkms amneziawg-tools >/dev/null 2>&1 || true
        MOD_VER=$(disk_mod_ver)
    fi

    # Загруженный отстал от дискового — перезагружаем. Интерфейс держит модуль,
    # поэтому сначала опускаем его; awg1 на этом шаге ещё не нужен.
    LOADED_VER=$(loaded_mod_ver)
    if [[ -n "$LOADED_VER" && "$LOADED_VER" != "$MOD_VER" ]]; then
        warn "В ядре загружен модуль $LOADED_VER, на диске $MOD_VER — перезагружаем модуль."
        awg-quick down "$IFACE" 2>/dev/null || true
        ip link delete dev "$IFACE" 2>/dev/null || true
        if modprobe -r amneziawg 2>/dev/null && modprobe amneziawg 2>/dev/null; then
            LOADED_VER=$(loaded_mod_ver)
            ok "Модуль перезагружен: в ядре теперь $LOADED_VER"
        else
            warn "Не удалось перезагрузить модуль (занят?). Нужна перезагрузка сервера."
        fi
    fi

    # Дальше решает ТОЛЬКО загруженная версия — именно она обслуживает интерфейс.
    [[ -n "$LOADED_VER" ]] && MOD_VER="$LOADED_VER"

    if [[ "${MOD_VER%%.*}" != "3" ]]; then
        AWG3="n"
        if [[ "$AWG_GEN" == "3.1" ]]; then
            fail "Нужен модуль amneziawg 3.x, а в ядре версия '${MOD_VER:-неизвестно}' (на диске $(disk_mod_ver)).
    Перезагрузи сервер и запусти установку снова — или поставь на 2.0: AWG_GEN=auto bash install.sh"
        fi
        warn "В ядре модуль версии '${MOD_VER:-неизвестно}' — ставим сервер на AmneziaWG 2.0."
        [[ "$(disk_mod_ver)" == 3.* ]] && \
            warn "На диске уже 3.x — после перезагрузки сервера можно переустановить и получить 3.1."
    elif kernel_lt_5_5; then
        AWG3="n"
        if [[ "$AWG_GEN" == "3.1" ]]; then
            fail "Ядро $KERNEL старше 5.5 — header protection не соберётся (upstream issue #210).
    Обнови ядро — или поставь на 2.0: AWG_GEN=auto bash install.sh"
        fi
        warn "Ядро $KERNEL старше 5.5 — header protection не соберётся (upstream issue #210)."
        warn "Ставим сервер на AmneziaWG 2.0."
    fi
fi

if [[ "$AWG3" == "y" ]]; then
    ok "AmneziaWG 3.1 доступен (модуль $MOD_VER, ядро $KERNEL)"
else
    warn "Сервер будет работать на AmneziaWG 2.0. Обнови ядро/модуль — и переходи на 3.1"
    warn "кнопкой «Перейти на AWG 3.1» в панели, переустановка не нужна."
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

# Свой файл в /etc/sysctl.d, а не /etc/sysctl.conf: в Debian 13 systemd-sysctl
# /etc/sysctl.conf при загрузке больше не читает — forwarding включился бы
# только до первой перезагрузки, и клиенты остались бы без интернета.
SYSCTL_FILE="/etc/sysctl.d/99-awg-control.conf"
cat > "$SYSCTL_FILE" <<'SYSCTL'
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=1
SYSCTL
sysctl -q -p "$SYSCTL_FILE"
ok "IP forwarding включён ($SYSCTL_FILE)"

# Параметры AmneziaWG 3.1. Значения-диапазоны взяты из дефолтов клиента
# AmneziaVPN (protocolConstants.h), чтобы сервер и клиент не расходились.
# RandomTrailers/DisableCookies (фичи 3.1 от 12.08.2026) пока не включаем.
#   🛑 HeaderProtectionKey фиксируется на весь срок жизни сервера наравне с
#   J/S/H: он обязан совпадать на обоих концах, и его смена делает невалидными
#   все ранее выданные vpn:// ключи. При KEEP_DATA берём существующий.
# Поколение предыдущей установки — нужно, чтобы предупредить о перевыпуске
# ключей при KEEP_DATA. Считать обязательно ДО перезаписи конфига.
PREV_GEN="none"
if [[ -f "$AWG_CONF" ]]; then
    if grep -q '^HeaderProtectionKey *=' "$AWG_CONF"; then PREV_GEN="3.1"; else PREV_GEN="2.0"; fi
fi

AWG3_LINES=""
if [[ "$AWG3" == "y" ]]; then
    HEADER_PROTECTION_KEY=""
    if [[ "$KEEP_DATA" == "y" && -f "$AWG_CONF" ]]; then
        # sed, а не awk -F'=': base64-ключ сам содержит '=' в паддинге.
        HEADER_PROTECTION_KEY=$(sed -n 's/^HeaderProtectionKey *= *//p' "$AWG_CONF" | head -1)
        [[ -n "$HEADER_PROTECTION_KEY" ]] && ok "HeaderProtectionKey взят из существующего конфига"
    fi
    [[ -z "$HEADER_PROTECTION_KEY" ]] && HEADER_PROTECTION_KEY=$(awg genpsk)

    AWG3_LINES="HeaderProtectionKey = ${HEADER_PROTECTION_KEY}
ContentPaddingAddition = 10-100
RekeyAfterTime = 100-120
RekeyTimeout = 3-7
RejectAfterTime = 150-180
KeepaliveTimeout = 5-15
MaxHandshakeAttempts = 15-20"
fi

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

[[ -n "$AWG3_LINES" ]] && printf '%s\n' "$AWG3_LINES" >> "$AWG_CONF"

chmod 600 "$AWG_CONF"
NEW_GEN=$([[ "$AWG3" == "y" ]] && echo 3.1 || echo 2.0)
ok "$AWG_CONF (AmneziaWG $NEW_GEN)"

if [[ "$KEEP_DATA" == "y" && "$PREV_GEN" != "none" && "$PREV_GEN" != "$NEW_GEN" ]]; then
    warn "Поколение протокола изменилось: $PREV_GEN → $NEW_GEN."
    warn "Пользователи сохранены, но их vpn:// ключи собраны на старых параметрах"
    warn "и работать перестанут. После запуска открой панель и нажми"
    warn "«Перевыпустить ключи», затем раздай пользователям новые ключи."
fi

step "5/7  Запуск AWG"

if awg show "$IFACE" &>/dev/null; then
    warn "Интерфейс $IFACE уже существует — перезапускаем"
    awg-quick down "$IFACE" 2>/dev/null || true
fi

# Подъём интерфейса — единственная настоящая проверка, что ядро приняло
# 3.1-параметры. Модуль на отказ отвечает лишь «Invalid argument», причину
# печатает только в dmesg и только при включённом dynamic debug. Поэтому:
# упали → включаем debug, пробуем ещё раз, показываем причину, и если и это не
# помогло — снимаем 3.x-ключи и поднимаемся на 2.0, а не валим установку.
AWG3_KEY_RE='^(HeaderProtectionKey|ContentPaddingAddition|RekeyAfterTime|RekeyTimeout|RejectAfterTime|KeepaliveTimeout|MaxHandshakeAttempts|RandomTrailers|DisableCookies) *='

if ! awg-quick up "$IFACE"; then
    ip link delete dev "$IFACE" 2>/dev/null || true

    if [[ "$AWG3" != "y" ]]; then
        fail "awg-quick up $IFACE завершился с ошибкой"
    fi

    warn "Ядро отвергло конфиг AmneziaWG 3.1. Выясняем причину."
    echo "module amneziawg +p" > /sys/kernel/debug/dynamic_debug/control 2>/dev/null || true

    if awg-quick up "$IFACE"; then
        ok "Со второй попытки интерфейс поднялся"
    else
        ip link delete dev "$IFACE" 2>/dev/null || true
        echo "  ── dmesg (последние 15 строк) ─────────────────"
        dmesg 2>/dev/null | grep -i amneziawg | tail -n 15 | sed 's/^/    /' || true
        echo "  ───────────────────────────────────────────────"

        cp "$AWG_CONF" "${AWG_CONF}.awg31"
        sed -i -E "/$AWG3_KEY_RE/d" "$AWG_CONF"
        AWG3="n"; NEW_GEN="2.0"
        warn "Откатываемся на AmneziaWG 2.0. Конфиг 3.1 сохранён: ${AWG_CONF}.awg31"
        [[ "$AWG_GEN" == "3.1" ]] && \
            warn "Модуль и ядро подходят под 3.1, но интерфейс поднялся на 2.0 — см. ${AWG_CONF}.awg31 и dmesg выше."
        warn "Разберись с причиной и повтори переход кнопкой «Перейти на AWG 3.1» в панели."

        awg-quick up "$IFACE" \
            || fail "awg-quick up $IFACE не работает даже на 2.0 — смотри dmesg выше"
        ok "Интерфейс поднят на AmneziaWG 2.0"
    fi
fi

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

TOOLS_VER=$(awg --version 2>/dev/null | awk '{print $2; exit}')

# Закрепляем версию пакета, чтобы unattended-upgrades не подменил модуль под
# живым сервером (issue #215: между двумя dkms-сборками словили регрессию —
# хендшейк проходит, данные не идут). Unhold перед апгрейдом уже сделан выше;
# hold ставим только теперь, когда версия проверена и интерфейс реально поднялся.
if [[ "$AWG_PIN" == "y" ]]; then
    apt-mark hold amneziawg amneziawg-dkms amneziawg-tools >/dev/null 2>&1 || true
    ok "Версия пакета закреплена (apt-mark hold) — снять: apt-mark unhold amneziawg amneziawg-dkms amneziawg-tools"
fi

GEN_LABEL=$([[ "$AWG3" == "y" ]] && echo "3.1" || echo "2.0")
ok "AmneziaWG $GEN_LABEL · модуль ${MOD_VER:-?} · tools ${TOOLS_VER:-?}$([[ "$AWG_PIN" == "y" ]] && echo " · версия закреплена")"

# UFW. PostUp делает `iptables -A FORWARD` — правило ДОПИСЫВАЕТСЯ в конец
# цепочки, а UFW вставляет свои переходы в начало, поэтому до нашего ACCEPT
# дело не доходит и трафик клиентов наружу режется: в dmesg сыплется
# «[UFW BLOCK] IN=awg1 OUT=eth0». Симптом — «VPN подключается, интернета нет».
# Лечится штатным `ufw route allow`, менять DEFAULT_FORWARD_POLICY не нужно.
if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -qi '^Status: active'; then
    echo "  → UFW активен, открываем маршрут и порты"
    ufw route allow in on "$IFACE" out on "$NET_IFACE" >/dev/null 2>&1 \
        || warn "ufw route allow не сработал — трафик клиентов может резаться"
    ufw allow "${AWG_PORT}/udp" >/dev/null 2>&1 || warn "не удалось открыть ${AWG_PORT}/udp"
    ufw allow "${UI_PORT}/tcp"  >/dev/null 2>&1 || warn "не удалось открыть ${UI_PORT}/tcp"
    ufw reload >/dev/null 2>&1 || true
    ok "UFW: форвардинг $IFACE → $NET_IFACE, порты ${AWG_PORT}/udp и ${UI_PORT}/tcp"
else
    echo "  → UFW не активен, правила фаервола не трогаем"
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

# Имя продукта попадает в cli.env, только если оператор задал его явно
# (BRAND=… перед запуском). Иначе оно берётся из .env, приехавшего в архиве, —
# чтобы источник правды оставался один.
[[ -n "$BRAND_OVERRIDE" ]] && echo "BRAND=${BRAND_OVERRIDE}" >> "$PROJECT/cli/cli.env"

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
