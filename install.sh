#!/usr/bin/env bash
# Copyright (c) 2026 Ivan Vasilev
# This source code is licensed under the MIT license found in the
# LICENSE file in the root directory of this source tree.
#
# Установщик Forgetting: панель управления VPN на AmneziaWG.
#   bash <(curl -Ls https://raw.githubusercontent.com/maeneko/forgetting/main/install.sh)
# Поверх установки — меню: обновить, переустановить, удалить. Параметры (ROLE, JOIN, AWG_GEN,
# AWG_PORT, AWG_MTU, UI_PREFIX, UNINSTALL…) задаются окружением — таблица в README,
# «Параметры установщика».
#
# Порядок файла: помощники вывода → константы → функции → сценарий (с «Сценарий» ниже).

set -euo pipefail

# Запущен из меню awg-ctrl — с временной копии (internal/cli/installer.go): убираем её сразу,
# bash дочитает скрипт из уже открытого файла.
case "${BASH_SOURCE[0]:-}" in
    */awg-install-[0-9]*.sh) rm -f -- "${BASH_SOURCE[0]}" ;;
esac

# Цвета — настоящие ESC-символы ($'…'), а не '\033' строкой: так они работают и в echo -e,
# и в приглашениях read -p. ACC — фиолетовый акцент панели (design.md). Без терминала
# (перенаправили в файл) или с NO_COLOR — без цветов.
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
    GRN=$'\033[0;32m'; YLW=$'\033[1;33m'; RED=$'\033[0;31m'; ACC=$'\033[38;5;141m'
    BLD=$'\033[1m';    DIM=$'\033[2m';    NC=$'\033[0m'
else
    GRN=""; YLW=""; RED=""; ACC=""; BLD=""; DIM=""; NC=""
fi

# Терминал ли на выводе — запоминаем до перенаправления в лог (после него stdout — уже труба).
# Ширина — чтобы строка спиннера не переносилась.
TTY_OUT=""
[[ -t 1 ]] && TTY_OUT="y"
COLS=$(stty size 2>/dev/null </dev/tty | cut -d' ' -f2 || true)
[[ "$COLS" =~ ^[0-9]+$ && "$COLS" -ge 40 ]] || COLS=80

ok()   { echo -e "  ${GRN}✓${NC}  $*"; }
warn() { echo -e "  ${YLW}⚠${NC}  $*"; }
fail() { trap - ERR; echo -e "\n  ${RED}✗${NC}  $*" >&2; exit 1; }
info() { echo -e "  ${DIM}→  $*${NC}"; }
skip() { echo -e "  ${DIM}–  $*${NC}"; }
# Заголовок шага — полоса прогресса фиксированной ширины, затем название (так названия стоят
# в один столбец): step "4/6  Запуск AWG" → «━━━━━━━━──── Запуск AWG  4/6». Шаг без номера
# (проверки, вопросы) — с пустой полосой той же ширины.
STEP_W=12
step() {
    local n=0 t=1 title="$*" full="" left="" bar i f
    if [[ "$1" =~ ^([0-9]+)/([0-9]+)\ +(.*)$ ]]; then
        n="${BASH_REMATCH[1]}"; t="${BASH_REMATCH[2]}"; title="${BASH_REMATCH[3]}"
    fi
    # Две половины собираем отдельно: срез ${bar:0:f} в C-локали режет байты, а «━» — три байта.
    f=$(( n * STEP_W / t ))
    for (( i = 0; i < STEP_W; i++ )); do
        if (( i < f )); then full+="━"; else left+="─"; fi
    done
    bar="${ACC}${full}${NC}${DIM}${left}${NC}"
    if (( n > 0 )); then
        echo -e "\n  ${bar}  ${BLD}${title}${NC}  ${DIM}${n}/${t}${NC}"
    else
        echo -e "\n  ${bar}  ${BLD}${title}${NC}"
    fi
}
# Карточка — строки с цветной полосой слева (правой рамки нет: ширину кириллицы в C-локали
# bash не посчитает, и рамка бы поехала). card_line ЦВЕТ "текст".
card_line() { echo -e "  ${1}┃${NC} ${2:-}"; }
# Приглашение к вводу — для read -p.
Q="  ${ACC}›${NC} "

# Вывод команд. В лог — всегда полностью, приглушённым блоком «│». На экране (терминал) — одна
# строка: спиннер, последняя строка вывода и время; упала команда — её последние 30 строк.
# Без терминала или с VERBOSE=1 — весь вывод и на экран, как в логе.
#   run cmd …   — выполнить; код возврата — её.
#   … | logblock — то же оформление для готового текста (хвост make.log, dmesg).
# Переменные перед вызовом функции bash передаёт и в неё, и в её команды:
#   DEBIAN_FRONTEND=noninteractive run apt-get install -y …
# Дескрипторы (открываются ниже, вместе с логом): 3 — только в лог, 4 — только на экран.
# Командам 3 закрываем: демон, унаследовавший его, держал бы лог открытым после выхода.
logblock() {
    local line
    while IFS= read -r line || [[ -n "$line" ]]; do
        printf '  %s│ %s%s\n' "$DIM" "$line" "$NC"
    done
}
SPIN=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)
run() {
    if [[ -z "$TTY_OUT" || -n "${VERBOSE:-}" ]] || ! { true >&3; } 2>/dev/null; then
        "$@" 2>&1 3>&- | logblock
        return "${PIPESTATUS[0]}"
    fi
    local out pid rc=0 i=0 t0=$SECONDS last el w name="$1"
    [[ "$name" == */* ]] && name="${name##*/}"
    [[ -n "${2:-}" && "$2" != -* ]] && name+=" $2"
    w=$(( COLS - ${#name} - 20 )); (( w < 10 )) && w=10
    out=$(mktemp)
    printf '  %s$ %s%s\n' "$DIM" "$*" "$NC" >&3
    "$@" > "$out" 2>&1 3>&- &
    pid=$!
    while kill -0 "$pid" 2>/dev/null; do
        el=$(( SECONDS - t0 ))
        last=$(tail -n 1 "$out" 2>/dev/null | tr -d '\r\033\t' | cut -c1-"$w" || true)
        printf '\r\033[K  %s%s%s %s  %s%s  %d:%02d%s' "$ACC" "${SPIN[i++ % 10]}" "$NC" "$name" "$DIM" "$last" $(( el / 60 )) $(( el % 60 )) "$NC" >&4
        sleep 0.12
    done
    wait "$pid" || rc=$?
    printf '\r\033[K' >&4
    logblock < "$out" >&3
    (( rc != 0 )) && printf '  ✗  код %d\n' "$rc" >&3
    if (( rc != 0 )); then
        printf '  %s✗%s  %s — код %d, последние строки (весь вывод — в логе):\n' "$RED" "$NC" "$name" "$rc" >&4
        tail -n 30 "$out" | logblock >&4
    fi
    rm -f "$out"
    return "$rc"
}

# Случайная строка из [a-zA-Z0-9]. Вход конечный: при `tr < /dev/urandom | head` tr получает
# SIGPIPE, под pipefail вся цепочка «падала», и срабатывал запасной openssl — пароль склеивался
# из двух половин. Из 1 КиБ случайных байт букв и цифр выходит ~240 — хватает с запасом.
# LC_ALL=C: tr работает с байтами, а не с символами локали.
rand_alnum() {
    head -c 1024 /dev/urandom | LC_ALL=C tr -dc 'a-zA-Z0-9' | head -c "$1"
}

rand_int() { shuf -i "$1-$2" -n 1; }
url_host() { sed -E 's#^https?://([^/]+).*#\1#' <<< "$1"; }

# Вопрос: ask VAR "текст: " [secret]. Введённое печатает сам терминал — в поток вывода, а значит
# и в лог, оно не попадает. Поэтому после ответа строка «вопрос ответ» печатается ещё раз: на
# экране — поверх себя же (курсор на строку вверх), в логе — целиком. Скрытый ввод — точками.
ask() {
    local __var="$1" __prompt="$2" __secret="${3:-}" __ans __shown
    if [[ -n "$__secret" ]]; then
        read -rsp "${Q}${__prompt}" __ans
    else
        read -rp "${Q}${__prompt}" __ans
    fi
    __shown="$__ans"
    [[ -n "$__secret" && -n "$__ans" ]] && __shown="••••••"
    if [[ -t 0 && -n "$__secret" ]]; then
        printf '\r%s%s%s\n' "$Q" "$__prompt" "$__shown"        # после read -s курсор на той же строке
    elif [[ -t 0 ]]; then
        printf '\033[1A\r%s%s%s\n' "$Q" "$__prompt" "$__shown"
    else
        printf '%s%s%s\n' "$Q" "$__prompt" "$__shown"           # ввод не с терминала — приглашения не было
    fi
    printf -v "$__var" '%s' "$__ans"
}

# ask_def VAR "вопрос" ПО_УМОЛЧАНИЮ [secret] — спросить, только если VAR не задана окружением;
# пустой ответ — значение по умолчанию.
ask_def() {
    local __v="$1"
    [[ -n "${!__v:-}" ]] && return 0
    ask "$__v" "$2" "${4:-}"
    [[ -n "${!__v}" ]] || printf -v "$__v" '%s' "$3"
}

# shellcheck disable=SC2154  # rc присваивается в самой строке trap
trap 'rc=$?; echo -e "\n  ${RED}✗ НЕОЖИДАННАЯ ОШИБКА${NC}  строка ${LINENO}  код ${rc}\n     команда: ${BASH_COMMAND}\n     полный лог: ${LOGFILE:-<ещё не открыт>}" >&2' ERR

[[ $EUID -ne 0 ]]          && fail "Запусти от root: sudo bash install.sh"
[[ -z "${BASH_VERSION:-}" ]] && fail "Нужен bash: bash install.sh"

# Лог установки — с первой строки: всё, что видно на экране (шапка, проверки, вопросы с
# ответами, вывод apt/DKMS), плюс то, что установщик раньше глушил. В файле — без цветов
# и без перерисовок строк (ESC-последовательности и всё до последнего \r вырезаются).
# Права 600: в итоге печатается пароль панели. LOGFILE — глобал, на него ссылается ERR-трап.
LOGFILE="/var/log/awg-install-$(date +%Y%m%d-%H%M%S).log"
( umask 077; : > "$LOGFILE" )
# LC_ALL=C: sed режет байты, а не символы — на битом UTF-8 из чужого вывода (make.log, dmesg) он
# не спотыкается. tee -p: если запись лога вдруг умрёт, tee не падает от SIGPIPE и продолжает
# писать на экран — иначе вслед за ним молча умер бы и сам установщик.
# Три потока: 4 — сам терминал (спиннер run и хвост упавшей команды — только на экран);
# 3 — только лог (полный вывод команд под спиннером); stdout/stderr — и туда, и туда (tee).
exec 4>&1
exec 3> >(LC_ALL=C sed -u -e $'s/\033\\[[0-9;]*[A-Za-z]//g' -e $'s/.*\r//' >> "$LOGFILE")
LOG_SED_PID="${!:-}"
exec > >(tee -p /dev/fd/3) 2>&1
LOG_PID="${!:-}"
# Дождаться, пока tee и sed допишут хвост, — иначе последние строки вылезут уже после приглашения
# шелла. sed ждём не дольше 2 с: если какой-то процесс всё же унаследовал fd 3, выход не повиснет.
trap 'exec >&- 2>&-; wait "$LOG_PID" 2>/dev/null || true; exec 3>&-
      for _ in {1..20}; do kill -0 "$LOG_SED_PID" 2>/dev/null || break; sleep 0.1; done' EXIT

# ═══ Константы ═════════════════════════════════════════════════════════════════
# ⚠️ Дублирует .env из репозитория: установщик собирает URL релиза и печатает
# баннер ДО того, как архив с .env скачан, поэтому взять их оттуда не может.
# При бампе версии правь оба места.
VERSION="0.5.3"
CHANNEL="${CHANNEL:-Beta}"

# Откуда брать архив и как называется продукт. По умолчанию — релизы на GitHub; источник
# переопределяется окружением. Путь релиза — <repo>/releases/download/v<VERSION>/<file>
# (такой же у Gitea, так что подойдёт и своё зеркало).
#   REPO_BASE     — основной источник;
#   REPO_FALLBACK — запасной, если в основном релиза нет (по умолчанию не задан);
#   ARCHIVE_URL   — полный URL архива, перекрывает оба варианта;
#   BRAND         — имя продукта в баннерах, systemd-юните и панели; по
#                   умолчанию берётся из .env в архиве, здесь только запасное
#                   значение для баннера самого установщика.
BRAND_OVERRIDE="${BRAND:-}"
BRAND="${BRAND:-Forgetting}"
REPO_BASE="${REPO_BASE:-https://github.com/maeneko/forgetting}"
REPO_FALLBACK="${REPO_FALLBACK:-$REPO_BASE}"
# Архив — под архитектуру: в нём готовый бинарник awg-control (Go, статический).
case "$(uname -m)" in
    x86_64|amd64)  GOARCH="amd64" ;;
    aarch64|arm64) GOARCH="arm64" ;;
    *)             GOARCH="" ;;
esac
ARCHIVE_NAME="awgcontrol-${VERSION}-linux-${GOARCH:-unknown}.tar.gz"
ARCHIVE_URL="${ARCHIVE_URL:-}"
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
# Подписка sen:// (SenAWG): ключ подписи ответов (Ed25519) и, при SUB_TLS=on, самоподписанный
# сертификат. В отличие от internal_auth они НЕ эфемерны — публичный ключ подписи и отпечаток
# сертификата зашиты во все выданные sen:// ссылки.
SUB_SIGN_PRIV="$AWG_DIR/sub_sign.key"
SUB_TLS_KEY="$AWG_DIR/sub_tls.key"
SUB_TLS_CRT="$AWG_DIR/sub_tls.crt"
# Несколько серверов (README, «Несколько серверов»). Хаб панели: самоподписанный сертификат, его
# отпечаток зашит в строки подключения нод — живёт столько же, сколько панель. Нода: свой
# Ed25519-ключ, которым она представляется панели.
NODE_TLS_KEY="$AWG_DIR/node_tls.key"
NODE_TLS_CRT="$AWG_DIR/node_tls.crt"
NODE_KEY_FILE="$AWG_DIR/node.key"
IFACE="awg1"
AWG_PORT="${AWG_PORT:-}"   # UDP для клиентов; пусто — выбирает choose_awg_params
SUBNET="10.9"
MTU="${AWG_MTU:-}"   # MTU туннеля; пусто — выбирает choose_awg_params

# Единый бинарник: сервисы (ctrl/ui/agent) и CLI (start/stop/status/…). Команда awg-ctrl —
# ссылка на него.
BIN="$PROJECT/awg-control"

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
[[ "$AWG_PIN" =~ ^[yn]$ ]] || fail "AWG_PIN должен быть y или n (задано: $AWG_PIN)"

# Параметры обфускации, UDP-порт и MTU AWG. На новой установке — случайные, у каждого сервера
# свои: известные всем значения по умолчанию — готовая сигнатура для блокировки. При KEEP_DATA=y
# берутся из прежнего awg1.conf (choose_awg_params ниже): они зашиты во все выданные vpn://
# ключи, их смена сделала бы каждый ключ невалидным.
# Ограничения генератора (gen_awg_params):
#   - Jc 4–8; Jmin < Jmax, оба много меньше MTU
#   - S1–S4 ≥ 12: при HeaderProtectionKey (AmneziaWG 3.x) меньшее модуль отвергает с одним
#     «Invalid argument» (причина видна лишь при
#     `echo "module amneziawg +p" > /sys/kernel/debug/dynamic_debug/control`)
#   - размеры пакетов после добавки разные: 148+S1 (init), 92+S2 (response), 64+S3 (cookie) —
#     иначе тип пакета снова узнаётся по длине; отсюда же S1 != S2 и S2 != S1+56
#   - S4 добавляется к каждому пакету данных — держим маленьким, как прежние 14
#   - H1–H4: непересекающиеся диапазоны в [10^8, 2^31−1] — далеко от 1–4 (типы сообщений
#     WireGuard) и не выше прежних значений (некоторые клиенты разбирают их как int32)
#   - I1 — пакет-маскировка, его шлёт инициатор handshake, принимающая сторона просто
#     отбрасывает. Сервер его не сверяет: у каждого ключа может быть свой, поэтому при
#     KEEP_DATA=y без I1 в конфиге его можно сгенерировать заново — старые ключи не сломаются.
#     Формат обязан пройти И kernel-модуль (сервер), И amneziawg-go (клиенты) — см. AWG_I1_RE
#     Клиентам awg-ctrl отдаёт свой I1 из users.db (internal/ctrl/i1.go): на первом старте
#     берёт этот, дальше его меняют в панели («Мастер-ключи») — в awg1.conf он не пишется
#   - I2–I5 НЕ трогать: в vpn:// ключ уходят пустыми плейсхолдерами
#   - MTU 1320–1376: не выше прежнего (запас под S4, заголовки и IPv6 на пути), не ниже —
#     лишние накладные расходы. Расхождение MTU сервера и клиента WireGuard переносит
# Прежние постоянные значения: ими собраны ключи установок до рандомизации — нужны, если
# при KEEP_DATA=y awg1.conf потерялся.
LEGACY_AWG_PORT="47619"
LEGACY_MTU="1376"
LEGACY_JC=6; LEGACY_JMIN=10; LEGACY_JMAX=50
LEGACY_S1=90; LEGACY_S2=45; LEGACY_S3=37; LEGACY_S4=14
LEGACY_H1="1224800044-2116730834"
LEGACY_H2="2122053282-2133204808"
LEGACY_H3="2133604274-2140756116"
LEGACY_H4="2143656228-2147444225"

# ═══ Функции ═══════════════════════════════════════════════════════════════════
# Сначала корректно через CLI (по PID-файлам), затем добиваем всё, что ещё
# держит файлы проекта: осиротевшие процессы, ручной запуск или stale PID,
# которые `stop all` не находит. Вызывать ДО `rm -rf $PROJECT` — иначе сервисы
# продолжат работать с уже удалёнными файлами и порт/интерфейс останутся занятыми.
kill_related() {
    if [[ -x "$BIN" ]]; then
        "$BIN" stop all 2>/dev/null || true
    else
        # Установка до Go-версии: её CLI на tsx.
        local _tsx="$PROJECT/cli/node_modules/.bin/tsx"
        local _cli="$PROJECT/cli/src/index.ts"
        [[ -x "$_tsx" && -f "$_cli" ]] && "$_tsx" "$_cli" stop all 2>/dev/null || true
    fi

    # cmdline всех сервисов (awg-control <роль>, у старых — tsx …) содержит путь проекта.
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

    rm -f /tmp/awg-ctrl.pid /tmp/awg-ui.pid /tmp/awg-agent.pid 2>/dev/null || true
}

# Значение из cli.env прежней установки (PREV_CLI_ENV читается в начале сценария).
prev_val() { grep "^$1=" <<< "$PREV_CLI_ENV" | tail -1 | cut -d= -f2- || true; }
# Прежнее значение — только если данные сохраняются (KEEP_DATA): порт и путь панели зашиты в
# закладки и адрес внешнего API, порты подписки и хаба — в выданные ссылки и строки нод.
keep() { [[ "$KEEP_DATA" == "y" ]] && prev_val "$1" || true; }

# Пункт чеклиста совместимости: ✓ — пройдено, ✗ — нет.
mark() { if [[ "$1" == 1 ]]; then echo -e "  ${GRN}✓${NC}  $2"; else echo -e "  ${RED}✗${NC}  $2"; fi; }

# ── Полное удаление ─────────────────────────────────────────────────────────
# Убирает всё, что ставили установщик и awg-ctrl: сервисы и юнит, интерфейс awg1, nft-таблицы
# шейпинга и блокировки торрентов, открытые порты в UFW/firewalld/iptables, Prometheus из
# `awg-ctrl monitor install`, каталог проекта, /etc/amnezia (пользователи, ключи, базы), sysctl,
# логи установки (в них пароль панели). AmneziaWG (пакеты, модуль, PPA) — по вопросу: он мог
# стоять и до нас. Общие зависимости (dkms, build-essential, iptables, заголовки) не трогаем.
uninstall_all() {
    local role="$PREV_ROLE" conf="$AWG_CONF" net_iface="" p
    local awg_port ports=()
    local unit="/etc/systemd/system/awg-control.service"

    step "Удаление ${BRAND}"
    echo -e "    ${DIM}Будут удалены сервисы, $PROJECT, $AMNEZIA_DIR (пользователи, ключи, базы),${NC}"
    echo -e "    ${DIM}юнит systemd, правила фаервола и логи установки. Выданные ключи перестанут работать.${NC}"
    case "$role" in
        node) echo -e "    ${DIM}Это нода: после удаления убери сервер и в списке серверов панели.${NC}" ;;
        *)    [[ -n "$(prev_val NODE_PORT)" ]] && \
                  echo -e "    ${DIM}Подключённые к этой панели ноды останутся без панели — их VPN продолжит работать.${NC}" ;;
    esac
    echo
    if [[ ! "${UNINSTALL:-}" =~ ^([Yy]|yes|1)$ ]]; then
        ask CONFIRM "Для подтверждения введи «удалить»: "
        [[ "$CONFIRM" == "удалить" ]] || fail "Отменено — ничего не тронуто"
    fi

    # Копия данных — по умолчанию да: без неё выданные ключи не восстановить.
    if [[ -d "$AMNEZIA_DIR" || -f "$PROJECT/cli/cli.env" ]]; then
        ask KEEP_BACKUP "Сохранить копию данных (пользователи, ключи, cli.env) в /root? [Y/n]: "
        if [[ "${KEEP_BACKUP:-y}" =~ ^[Yy]$ ]]; then
            local bak items=()
            bak="/root/forgetting-backup-$(date +%Y%m%d-%H%M%S).tar.gz"
            [[ -d "$AMNEZIA_DIR" ]] && items+=("${AMNEZIA_DIR#/}")
            [[ -f "$PROJECT/cli/cli.env" ]] && items+=("${PROJECT#/}/cli/cli.env")
            ( umask 077; tar -czf "$bak" -C / "${items[@]}" ) \
                && ok "Копия: $bak" \
                || fail "Не удалось сохранить копию — удаление отменено, ничего не тронуто"
        fi
    fi

    # Что открывали наружу — из прежнего конфига (порты случайные, угадать нельзя).
    awg_port=$(sed -n 's/^ListenPort *= *//p' "$conf" 2>/dev/null | head -1 || true)
    [[ -z "$awg_port" ]] && awg_port=$(prev_val SERVER_PORT)
    [[ -n "$awg_port" ]] && ports+=("${awg_port}/udp")
    for p in UI_PORT SUB_PORT NODE_PORT; do
        [[ -n "$(prev_val "$p")" ]] && ports+=("$(prev_val "$p")/tcp")
    done
    net_iface=$(grep -oE 'POSTROUTING -o [^ ]+' "$conf" 2>/dev/null | head -1 | awk '{print $3}' || true)

    info "остановка сервисов"
    if command -v systemctl &>/dev/null && [[ -f "$unit" ]]; then
        run systemctl disable --now awg-control.service || true
    fi
    # Prometheus из `awg-ctrl monitor install` — только наш (метка в конфиге); чужой не трогаем.
    if [[ -x "$BIN" ]] && grep -qs '^# managed by forgetting' /etc/prometheus/prometheus.yml; then
        info "Prometheus, поставленный панелью"
        run "$BIN" monitor remove || warn "Prometheus не удалён — sudo apt purge prometheus"
    fi
    kill_related
    ok "Сервисы остановлены"

    if command -v awg &>/dev/null && awg show "$IFACE" &>/dev/null; then
        # down выполняет PreDown из конфига — снимает FORWARD/MASQUERADE.
        run awg-quick down "$IFACE" || ip link delete dev "$IFACE" 2>/dev/null || true
        ok "Интерфейс $IFACE опущен"
    fi
    if command -v nft &>/dev/null; then
        nft delete table inet awg_shape 2>/dev/null || true
        nft delete table inet awg_torrent 2>/dev/null || true
    fi

    if [[ ${#ports[@]} -gt 0 ]]; then
        if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -qi '^Status: active'; then
            for p in "${ports[@]}"; do run ufw delete allow "$p" || true; done
            [[ -n "$net_iface" ]] && { run ufw route delete allow in on "$IFACE" out on "$net_iface" || true; }
            ok "UFW: закрыты ${ports[*]}"
        elif command -v firewall-cmd &>/dev/null && firewall-cmd --state &>/dev/null; then
            for p in "${ports[@]}"; do run firewall-cmd --permanent --remove-port="$p" || true; done
            run firewall-cmd --reload || true
            ok "firewalld: закрыты ${ports[*]}"
        elif command -v iptables &>/dev/null; then
            local removed=0
            for p in "${ports[@]}"; do
                while iptables -C INPUT -p "${p#*/}" --dport "${p%/*}" -j ACCEPT 2>/dev/null; do
                    iptables -D INPUT -p "${p#*/}" --dport "${p%/*}" -j ACCEPT && removed=1
                done
            done
            if [[ "$removed" == 1 ]]; then
                command -v netfilter-persistent &>/dev/null && { run netfilter-persistent save || true; }
                ok "iptables: закрыты ${ports[*]}"
            fi
        fi
    fi

    if [[ -f /etc/systemd/system/awg-control.service ]]; then
        rm -f /etc/systemd/system/awg-control.service
        command -v systemctl &>/dev/null && { run systemctl daemon-reload || true; }
    fi
    [[ "$(readlink /usr/local/bin/awg-ctrl 2>/dev/null)" == "$BIN" ]] && rm -f /usr/local/bin/awg-ctrl
    # Пути — константы из шапки; :? — страховка от пустой переменной.
    rm -rf "${PROJECT:?}" "${AMNEZIA_DIR:?}"
    rm -f /etc/sysctl.d/99-awg-control.conf
    # Логи установок: в них пароль панели. Текущий (удаления) — без секретов, остаётся.
    find /var/log -maxdepth 1 -name 'awg-install-*.log' ! -path "$LOGFILE" -delete 2>/dev/null || true
    ok "Удалены $PROJECT, $AMNEZIA_DIR, юнит, команда awg-ctrl, логи установки"

    if dpkg -s amneziawg &>/dev/null || dpkg -s amneziawg-tools &>/dev/null; then
        echo
        ask DROP_AWG "Удалить и AmneziaWG (пакеты, модуль ядра, репозиторий)? [y/N]: "
        if [[ "${DROP_AWG:-n}" =~ ^[Yy]$ ]]; then
            run apt-mark unhold amneziawg amneziawg-dkms amneziawg-tools || true
            rmmod amneziawg 2>/dev/null || true
            DEBIAN_FRONTEND=noninteractive run apt-get purge -y amneziawg amneziawg-dkms amneziawg-tools \
                || warn "apt-get purge завершился ошибкой — проверь: dpkg -l | grep amneziawg"
            rm -f /etc/apt/sources.list.d/amnezia-ppa.list /etc/apt/keyrings/amnezia-ppa.gpg
            if command -v add-apt-repository &>/dev/null \
                && grep -rqs 'amnezia/ppa' /etc/apt/sources.list /etc/apt/sources.list.d/; then
                run add-apt-repository -y --remove ppa:amnezia/ppa || true
            fi
            ok "AmneziaWG удалён"
        else
            skip "AmneziaWG оставлен"
        fi
    fi

    echo
    card_line "$GRN"
    card_line "$GRN" "${GRN}${BLD}✓ ${BRAND} удалён${NC}"
    card_line "$GRN"
    card_line "$GRN" "${DIM}IP forwarding (sysctl) выключится после перезагрузки — сейчас: sysctl -w net.ipv4.ip_forward=0,${NC}"
    card_line "$GRN" "${DIM}если он не нужен другим сервисам. Фаервол хостера (панель провайдера) — закрой порты там сам.${NC}"
    card_line "$GRN"
    echo
}

gen_awg_params() {
    JC=$(rand_int 4 8)
    JMIN=$(rand_int 8 40)
    JMAX=$(( JMIN + $(rand_int 20 80) ))
    while :; do
        S1=$(rand_int 15 150); S2=$(rand_int 15 150); S3=$(rand_int 15 60)
        local a=$(( 148 + S1 )) b=$(( 92 + S2 )) c=$(( 64 + S3 ))
        [[ "$S1" != "$S2" && "$a" != "$b" && "$a" != "$c" && "$b" != "$c" ]] && break
    done
    S4=$(rand_int 12 20)
    # 8 разных чисел по возрастанию → 4 непересекающихся диапазона, раздаём H1–H4 вперемешку.
    local n r
    mapfile -t n < <(shuf -i 100000000-2147483647 -n 8 | sort -n)
    mapfile -t r < <(printf '%s\n' "${n[0]}-${n[1]}" "${n[2]}-${n[3]}" "${n[4]}-${n[5]}" "${n[6]}-${n[7]}" | shuf)
    H1=${r[0]}; H2=${r[1]}; H3=${r[2]}; H4=${r[3]}
}

# Синтаксис I1, который принимают обе реализации AmneziaWG:
#   - только теги <b 0xHEX>, <r N>, <rc N>, <rd N>, <t>. <c> не знает amneziawg-go (клиент не
#     применит ключ), <d>/<ds>/<dz> не знает kernel-модуль;
#   - ровно ОДИН пробел между именем тега и аргументом: модуль делит по одиночному пробелу;
#   - у <b> префикс 0x обязателен, hex — чётной длины;
#   - текст вне <…> обе молча выкидывают — не пишем.
# Ошибка формата роняет весь `awg-quick up`, а модуль говорит лишь «Invalid argument».
AWG_I1_RE='^(<b 0x([0-9a-f]{2})+>|<(r|rc|rd) [1-9][0-9]{0,3}>|<t>)+$'

# Домены для I1: популярные, чтобы запрос выглядел обычным.
I1_DOMAINS=(google.com youtube.com yandex.ru vk.com mail.ru ok.ru apple.com icloud.com
            microsoft.com cloudflare.com wikipedia.org github.com)

# I1 — DNS-запрос (его шлёт клиент, так что запрос правдоподобнее ответа): <r 2> — ID, новый
# в каждом пакете; флаги 0100 (стандартный, RD), 1 вопрос; случайный домен, A или AAAA, класс IN.
# 27–36 байт — меньше любого handshake-пакета (самый короткий, cookie, ≥ 79): по длине не путается.
gen_awg_i1() {
    local d label qname="" qtype labels
    d=${I1_DOMAINS[$(rand_int 0 $(( ${#I1_DOMAINS[@]} - 1 )))]}
    [[ $(rand_int 0 1) == 1 ]] && d="www.$d"
    IFS=. read -ra labels <<< "$d"
    for label in "${labels[@]}"; do
        qname+=$(printf '%02x' "${#label}")$(printf '%s' "$label" | od -An -tx1 | tr -d ' \n')
    done
    qtype=$([[ $(rand_int 0 2) == 0 ]] && echo 001c || echo 0001)
    I1="<r 2><b 0x01000001000000000000${qname}00${qtype}0001>"
}

legacy_awg_params() {
    JC=$LEGACY_JC; JMIN=$LEGACY_JMIN; JMAX=$LEGACY_JMAX
    S1=$LEGACY_S1; S2=$LEGACY_S2; S3=$LEGACY_S3; S4=$LEGACY_S4
    H1=$LEGACY_H1; H2=$LEGACY_H2; H3=$LEGACY_H3; H4=$LEGACY_H4
}

# Случайный UDP-порт, который никто на сервере не слушает.
gen_awg_port() {
    local p
    while :; do
        p=$(rand_int 20000 59999)
        [[ -z "$(ss -Hulnp "sport = :$p" 2>/dev/null)" ]] && { echo "$p"; return; }
    done
}

# Порт, MTU и обфускация: при KEEP_DATA=y — из прежнего awg1.conf (все J/S/H разом или ни
# одного: смешивать с новыми нельзя), иначе случайные. AWG_PORT и AWG_MTU из окружения
# важнее всего.
choose_awg_params() {
    local k prev_port=""
    # || true: без файла sed падает, а с pipefail и set -e это молча роняло бы установку.
    conf_val() { sed -n "s/^$1 *= *//p" "$AWG_CONF" 2>/dev/null | head -1 || true; }
    if [[ "$KEEP_DATA" == "y" ]]; then
        JC=$(conf_val Jc); JMIN=$(conf_val Jmin); JMAX=$(conf_val Jmax)
        S1=$(conf_val S1); S2=$(conf_val S2); S3=$(conf_val S3); S4=$(conf_val S4)
        H1=$(conf_val H1); H2=$(conf_val H2); H3=$(conf_val H3); H4=$(conf_val H4)
        for k in "$JC" "$JMIN" "$JMAX" "$S1" "$S2" "$S3" "$S4" "$H1" "$H2" "$H3" "$H4"; do
            if [[ -z "$k" ]]; then
                legacy_awg_params
                warn "Параметры обфускации не прочитаны из $AWG_CONF — взяты прежние постоянные"
                break
            fi
        done
        prev_port=$(conf_val ListenPort)
        [[ -z "$prev_port" ]] && prev_port=$(prev_val SERVER_PORT)
        AWG_PORT="${AWG_PORT:-${prev_port:-$LEGACY_AWG_PORT}}"
        MTU="${MTU:-$(conf_val MTU)}"
        MTU="${MTU:-$LEGACY_MTU}"
        I1=$(conf_val I1)
        if [[ -n "$I1" && ! "$I1" =~ $AWG_I1_RE ]]; then
            warn "I1 из $AWG_CONF не пройдёт парсер AmneziaWG — генерируем новый"
            I1=""
        fi
        # Конфиги до рандомизации I1 не содержат: ключи брали общий из awg-ctrl. Новый
        # безопасен — сервер I1 не сверяет, у старых ключей останется прежний.
        [[ -z "$I1" ]] && gen_awg_i1
    else
        gen_awg_params
        gen_awg_i1
        AWG_PORT="${AWG_PORT:-$(gen_awg_port)}"
        MTU="${MTU:-$(rand_int 1320 1376)}"
    fi
    [[ "$AWG_PORT" =~ ^[0-9]+$ && "$AWG_PORT" -gt 0 && "$AWG_PORT" -lt 65536 ]] \
        || fail "AWG_PORT «${AWG_PORT}» — не номер порта"
    [[ "$MTU" =~ ^[0-9]+$ && "$MTU" -ge 1280 && "$MTU" -le 1500 ]] \
        || fail "MTU «${MTU}» — нужно число 1280–1500"
    [[ "$I1" =~ $AWG_I1_RE ]] || fail "I1 «${I1}» не пройдёт парсер AmneziaWG"
}

valid_port() { [[ "$1" =~ ^[0-9]+$ && "$1" -gt 0 && "$1" -lt 65536 ]]; }

# Случайный свободный TCP-порт 32768–55535, не из уже занятых установкой (аргументы).
gen_tcp_port() {
    local p used=" $* "
    while :; do
        p=$(rand_int 32768 55535)
        [[ "$used" == *" $p "* ]] && continue
        [[ -z "$(ss -Htlnp "sport = :$p" 2>/dev/null)" ]] && { echo "$p"; return; }
    done
}

# Скачать архив версии VERSION, распаковать в PROJECT и проверить, что ключевые
# файлы на месте.
download_extract() {
    local tmp="/tmp/${ARCHIVE_NAME}"
    local keep_tarball=0

    # AWG_LOCAL_TARBALL — поставить сборку, которой ещё нет в релизах (или
    # встать без сети). Архив должен быть той же формы, что делает CI:
    # awg-control (бинарник под эту архитектуру) + awg-ui/dist/ + .env.
    if [[ -n "${AWG_LOCAL_TARBALL:-}" ]]; then
        [[ -f "$AWG_LOCAL_TARBALL" ]] \
            || fail "AWG_LOCAL_TARBALL указан, но файл не найден: $AWG_LOCAL_TARBALL"
        tmp="$AWG_LOCAL_TARBALL"
        keep_tarball=1          # чужой файл — после распаковки не удаляем
        info "локальный архив: $tmp"
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

        info "версия: ${VERSION}"
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
    remove_legacy
    run tar -xzf "$tmp" -C "$PROJECT" --strip-components=1 \
        || fail "Не удалось распаковать архив"
    [[ "$keep_tarball" == 1 ]] || rm -f "$tmp"
    ok "Распакован → $PROJECT"

    local f
    for f in "$BIN" "$PROJECT/awg-ui/dist/index.html" "$PROJECT/.env"; do
        [[ -f "$f" ]] || fail "Файл не найден после распаковки: $f"
    done
    # Рабочие каталоги сервисов: в них живут panel.id, логи awg-ctrl, cli.env.
    mkdir -p "$PROJECT/awg-ctrl" "$PROJECT/awg-ui" "$PROJECT/awg-agent" "$PROJECT/cli" "$PROJECT/logs"
    chmod 755 "$BIN"
    "$BIN" version >/dev/null 2>&1 || fail "Бинарник $BIN не запускается (архив не под эту архитектуру?)"
    ok "Архив проверен"
}

# remove_legacy — при обновлении убрать код прежней версии: TS-исходники, node_modules и старую
# сборку фронтенда. Данные остаются: cli/cli.env, awg-ui/panel.id, логи (базы и ключи — в /etc).
remove_legacy() {
    local d
    for d in awg-ctrl awg-ui awg-agent cli; do
        [[ -d "$PROJECT/$d" ]] || continue
        rm -rf "$PROJECT/$d/node_modules" "$PROJECT/$d/src" "$PROJECT/$d/public" "$PROJECT/$d/dist"
        rm -f "$PROJECT/$d"/*.ts "$PROJECT/$d"/package*.json "$PROJECT/$d"/tsconfig*.json "$PROJECT/$d/index.html"
    done
}

# Команда awg-ctrl — ссылка на бинарник: CLI находит корень установки по её цели.
install_command() {
    ln -sfn "$BIN" /usr/local/bin/awg-ctrl
    ok "awg-ctrl → $BIN"
}

# systemd-юнит: автозапуск awg-control на загрузке. Модель CLI — детач-процессы
# с PID-файлами (а не один долгоживущий процесс), поэтому Type=oneshot +
# RemainAfterExit: systemd держит юнит «active», процессами рулит CLI через
# start/stop. awg-ctrl падает, если awg1 не поднят, а на загрузке awg-quick up
# вручную не выполняется — поднимаем интерфейс в ExecStartPre (если ещё не поднят).
SERVICE_UNIT="/etc/systemd/system/awg-control.service"
setup_service() {
    # sbin в PATH юнита: awg-quick зовёт iptables/ip/sysctl.
    local pre="" what
    # Панели без VPN (core) интерфейс не нужен — ExecStartPre только там, где есть awg-ctrl.
    [[ "$ROLE" != "core" ]] && pre="ExecStartPre=/bin/sh -c 'awg show ${IFACE} >/dev/null 2>&1 || awg-quick up ${IFACE}'"
    case "$ROLE" in
        core) what="awg-ui" ;;
        node) what="awg-ctrl + awg-agent" ;;
        *)    what="awg-ctrl + awg-ui" ;;
    esac
    cat > "$SERVICE_UNIT" <<UNIT
[Unit]
Description=AWG Control — ${what} (${BRAND})
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
${pre}
ExecStart=/usr/local/bin/awg-ctrl start all
ExecStop=/usr/local/bin/awg-ctrl stop all

[Install]
WantedBy=multi-user.target
UNIT
    run systemctl daemon-reload
    run systemctl enable awg-control.service \
        || warn "systemctl enable awg-control не удался — автозапуск не настроен"
    ok "systemd-юнит awg-control установлен (автозапуск на загрузке)"
}

# Запуск/перезапуск сервисов + статус. Через systemd, если он есть (тогда
# работает автозапуск на загрузке); иначе — напрямую через CLI, без автозапуска.
start_and_status() {
    if command -v systemctl >/dev/null 2>&1; then
        setup_service
        run systemctl restart awg-control.service \
            || fail "systemctl restart awg-control завершился ошибкой"
    else
        warn "systemd не найден — запускаю напрямую через CLI (без автозапуска)"
        run "$BIN" start all
    fi
    sleep 2
    "$BIN" status
}

# Нода: к какой панели подключаться. Строку awgjoin://… даёт панель («Добавить сервер»):
# base64url от JSON {v, h, p, pin, n, s}. Разбираем здесь, до установки, чтобы опечатка не
# обнаружилась через десять минут сборки модуля. Значения в строке ограничены
# [A-Za-z0-9.:_-], поэтому JSON без экранирования и sed его разбирает надёжно.
parse_join() {
    local b64="${1#awgjoin://}" json
    [[ "$1" == awgjoin://* && "$b64" =~ ^[A-Za-z0-9_-]+$ ]] || return 1
    b64=$(tr '_-' '/+' <<< "$b64")
    while (( ${#b64} % 4 )); do b64+="="; done
    json=$(base64 -d <<< "$b64" 2>/dev/null) || return 1
    [[ "$json" == *'"v":1,'* ]] || return 1
    CORE_HOST=$(sed -nE 's/.*"h":"([A-Za-z0-9.:_-]{1,253})".*/\1/p'  <<< "$json")
    CORE_PORT=$(sed -nE 's/.*"p":([0-9]{1,5})[,}].*/\1/p'           <<< "$json")
    CORE_PIN=$(sed -nE 's/.*"pin":"([A-Za-z0-9_-]{43})".*/\1/p'     <<< "$json")
    NODE_ID=$(sed -nE 's/.*"n":([0-9]+)[,}].*/\1/p'                  <<< "$json")
    JOIN_SECRET=$(sed -nE 's/.*"s":"([A-Za-z0-9_-]{22})".*/\1/p'    <<< "$json")
    # NODE_ID 0 — сервер, с которого переехала панель (awg-ctrl migrate import).
    [[ -n "$CORE_HOST" && -n "$CORE_PIN" && -n "$JOIN_SECRET" && -n "$NODE_ID" ]] \
        && (( CORE_PORT > 0 && CORE_PORT < 65536 && NODE_ID >= 0 ))
}

# SHA-256 от SPKI сертификата панели, base64url без «=» — в том же виде, что pin в строке.
# Пусто — панель не ответила.
core_pin() {
    local target="$1:$2" pub
    [[ "$1" == *:* ]] && target="[$1]:$2"   # IPv6
    pub=$(timeout 8 openssl s_client -connect "$target" </dev/null 2>/dev/null \
        | openssl x509 -pubkey -noout 2>/dev/null) || true
    # Без этой проверки недоступная панель дала бы хэш пустой строки — «чужой сертификат».
    [[ "$pub" == *"BEGIN PUBLIC KEY"* ]] || return 0
    openssl pkey -pubin -outform der <<< "$pub" 2>/dev/null \
        | openssl dgst -sha256 -binary \
        | base64 | tr '+/' '-_' | tr -d '=\n' || true
}

# ── AmneziaWG: PPA для Debian, проверки модуля и ядра ───────────────────────
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
    info "подключение PPA amnezia (Debian ${OS_CODENAME:-?} → серия $suite)"

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

# Закрывает ли голый iptables входящие: политика DROP или правило DROP/REJECT в INPUT.
iptables_input_closed() {
    command -v iptables &>/dev/null || return 1
    iptables -S INPUT 2>/dev/null | grep -qE '^-P INPUT DROP|^-A INPUT .*-j (DROP|REJECT)'
}

# Открыть OPEN_PORTS на фаерволе самого сервера: UFW, firewalld или голый iptables — что активно.
# Фаервол хостера (панель провайдера, security group) отсюда не виден — о нём напоминаем в итоге.
open_firewall() {
    local P
    if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -qi '^Status: active'; then
        # UFW. PostUp делает `iptables -A FORWARD` — правило ДОПИСЫВАЕТСЯ в конец
        # цепочки, а UFW вставляет свои переходы в начало, поэтому до нашего ACCEPT
        # дело не доходит и трафик клиентов наружу режется: в dmesg сыплется
        # «[UFW BLOCK] IN=awg1 OUT=eth0». Симптом — «VPN подключается, интернета нет».
        # Лечится штатным `ufw route allow`, менять DEFAULT_FORWARD_POLICY не нужно.
        info "UFW активен, открываем $([[ "$ROLE" != "core" ]] && echo "маршрут и ")порты"
        if [[ "$ROLE" != "core" ]]; then
            run ufw route allow in on "$IFACE" out on "$NET_IFACE" \
                || warn "ufw route allow не сработал — трафик клиентов может резаться"
        fi
        for P in "${OPEN_PORTS[@]}"; do
            run ufw allow "$P" || warn "не удалось открыть $P"
        done
        run ufw reload || true
        ok "UFW:$([[ "$ROLE" != "core" ]] && echo " форвардинг $IFACE → $NET_IFACE,") порты ${OPEN_PORTS[*]}"
    elif command -v firewall-cmd &>/dev/null && firewall-cmd --state &>/dev/null; then
        info "firewalld активен, открываем порты"
        for P in "${OPEN_PORTS[@]}"; do
            run firewall-cmd --permanent --add-port="$P" || warn "не удалось открыть $P"
        done
        run firewall-cmd --reload || true
        ok "firewalld: порты ${OPEN_PORTS[*]}"
        if [[ "$ROLE" != "core" ]]; then
            warn "Если VPN подключается, а интернета нет — firewalld режет форвардинг:"
            warn "  firewall-cmd --permanent --zone=trusted --add-interface=${IFACE} && firewall-cmd --reload"
        fi
    elif iptables_input_closed; then
        # Голый iptables без обёрток: вставляем ACCEPT в начало INPUT (повторно не дублируем).
        info "iptables закрывает входящие, открываем порты"
        for P in "${OPEN_PORTS[@]}"; do
            iptables -C INPUT -p "${P#*/}" --dport "${P%/*}" -j ACCEPT 2>/dev/null \
                || run iptables -I INPUT -p "${P#*/}" --dport "${P%/*}" -j ACCEPT \
                || warn "не удалось открыть $P"
        done
        if command -v netfilter-persistent &>/dev/null; then
            run netfilter-persistent save || warn "правила iptables не сохранены — после перезагрузки порты закроются"
            ok "iptables: порты ${OPEN_PORTS[*]} (сохранено)"
        else
            ok "iptables: порты ${OPEN_PORTS[*]}"
            warn "Правила iptables не переживут перезагрузку: apt install iptables-persistent && netfilter-persistent save"
        fi
    else
        info "Фаервол на сервере входящие не закрывает — открывать нечего"
    fi
}

# Адрес панели для итога: с секретным путём.
panel_url() { echo "http://$1:$2/${3:+$3/}"; }

# ═══ Сценарий ══════════════════════════════════════════════════════════════════

echo
card_line "$ACC"
card_line "$ACC" "${ACC}◆${NC} ${BLD}${BRAND}${NC}   ${DIM}${CHANNEL} · ${VERSION}${NC}"
card_line "$ACC" "${DIM}Панель управления VPN на AmneziaWG — установка${NC}"
card_line "$ACC"
card_line "$ACC" "${DIM}лог   $LOGFILE${NC}"
card_line "$ACC"

# Поверх существующей установки: обновить (настройки, пользователи и роль остаются),
# переустановить с нуля или удалить совсем. Спрашиваем первым — от ответа зависит, спрашивать
# ли роль. Без меню: ACTION=update|reinstall|uninstall (так запускают пункты меню awg-ctrl)
# или UNINSTALL=y — удалить без вопроса-подтверждения.
#
# Установку старше MIN_UPGRADE_VERSION не обновить и не перенести: с ней несовместимы и данные,
# и выданные ключи. Для неё — только удаление или полная переустановка со сбросом всех данных
# (WIPE=y): пользователи, мастер-ключи, ключи API, ключи сервера и ноды — всё заново. Старые базы
# уходят в бэкап (*.bak-…), как при обычной переустановке, но перенести их нельзя. Версия — из
# .env прежней установки (он есть с 0.2.0); нет его — версия неизвестна и тоже считается устаревшей.
# ALLOW_048=y — исключение для 0.4.8: её данные 0.5.0 переносит, и её можно обновить как обычно
# (через меню awg-ctrl — ALLOW_048=y awg-ctrl, окружение доходит до установщика).
MIN_UPGRADE_VERSION="0.5.0"
version_lt() { [[ "$1" != "$2" && "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" == "$1" ]]; }
WIPE="n"
OUTDATED="n"
PREV_VERSION=""
if [[ -d "$PROJECT" ]]; then
    PREV_VERSION=$(sed -n 's/^VERSION=//p' "$PROJECT/.env" 2>/dev/null | head -1 | tr -d "\"' " || true)
    if [[ -z "$PREV_VERSION" ]] || version_lt "${PREV_VERSION%%-*}" "$MIN_UPGRADE_VERSION"; then
        OUTDATED="y"
    fi
    if [[ "$OUTDATED" == "y" && "${PREV_VERSION%%-*}" == "0.4.8" && "${ALLOW_048:-}" =~ ^([Yy]|yes|1)$ ]]; then
        OUTDATED="n"
        echo
        warn "Версия 0.4.8 старше ${MIN_UPGRADE_VERSION}, но задан ALLOW_048 — обновление без сброса данных"
    fi
fi
outdated_card() {
    echo
    card_line "$RED"
    card_line "$RED" "${RED}${BLD}✗ Установлена устаревшая версия ${PREV_VERSION:-(неизвестна)}${NC}"
    card_line "$RED"
    card_line "$RED" "Обновить её до ${VERSION} нельзя: версии старше ${MIN_UPGRADE_VERSION} нужна полная переустановка."
    card_line "$RED" "Будут удалены все ключи и данные:"
    card_line "$RED" "  ${DIM}·${NC} пользователи и их vpn:// ключи"
    card_line "$RED" "  ${DIM}·${NC} мастер-ключи и ссылки sen://, ключи API"
    card_line "$RED" "  ${DIM}·${NC} ключи сервера и подключение к панели (ноду придётся добавить заново)"
    card_line "$RED"
    card_line "$RED" "Выданные ключи перестанут работать — клиентам нужно будет выдать новые."
    card_line "$RED" "${DIM}Старые базы останутся в бэкапе (*.bak-…), но перенести их в ${VERSION} нельзя.${NC}"
    if [[ "${PREV_VERSION%%-*}" == "0.4.8" ]]; then
        card_line "$RED"
        card_line "$RED" "Для 0.4.8 есть исключение: ${BLD}ALLOW_048=y bash install.sh${NC} — обновление с сохранением данных."
    fi
    card_line "$RED"
    echo
}
INSTALL_MODE="fresh"
ACTION="${ACTION:-}"
[[ "$ACTION" =~ ^(|update|reinstall|uninstall)$ ]] \
    || fail "ACTION должен быть update, reinstall или uninstall (задано: $ACTION)"
if [[ "${UNINSTALL:-}" =~ ^([Yy]|yes|1)$ ]]; then
    INSTALL_MODE="uninstall"
elif [[ -n "$ACTION" ]]; then
    [[ -d "$PROJECT" || "$ACTION" == "uninstall" ]] || fail "Обновлять нечего: в $PROJECT ничего не установлено"
    case "$ACTION" in
        update)    INSTALL_MODE="update" ;;
        reinstall) INSTALL_MODE="fresh" ;;
        uninstall) INSTALL_MODE="uninstall" ;;
    esac
    if [[ "$OUTDATED" == "y" && "$INSTALL_MODE" != "uninstall" ]]; then
        outdated_card
        ask WIPE_YN "Переустановить с удалением всех ключей? [y/N]: "
        [[ "${WIPE_YN:-n}" =~ ^[Yy]$ ]] || { echo "  Отменено — ничего не изменено."; exit 0; }
        INSTALL_MODE="fresh"; WIPE="y"
    fi
elif [[ -d "$PROJECT" && "$OUTDATED" == "y" ]]; then
    outdated_card
    echo -e "    ${ACC}${BLD}1${NC}  Переустановить   ${DIM}с удалением всех ключей и данных${NC}"
    echo -e "    ${ACC}${BLD}2${NC}  Удалить          ${DIM}полностью: сервисы, данные, правила фаервола, по желанию AmneziaWG${NC}"
    ask INST_CHOICE "Выбери [1/2, Enter — отменить]: "
    case "${INST_CHOICE:-}" in
        1) INSTALL_MODE="fresh"; WIPE="y" ;;
        2) INSTALL_MODE="uninstall" ;;
        *) echo "  Отменено — ничего не изменено."; exit 0 ;;
    esac
    echo
elif [[ -d "$PROJECT" ]]; then
    echo
    warn "Уже установлено в $PROJECT ${DIM}(версия ${PREV_VERSION})${NC}"
    echo -e "    ${ACC}${BLD}1${NC}  Обновить         ${DIM}до ${VERSION}; настройки, пользователи и роль сохранятся${NC}"
    echo -e "    ${ACC}${BLD}2${NC}  Переустановить   ${DIM}с нуля; данные можно будет сохранить${NC}"
    echo -e "    ${ACC}${BLD}3${NC}  Удалить          ${DIM}полностью: сервисы, данные, правила фаервола, по желанию AmneziaWG${NC}"
    ask INST_CHOICE "Выбери [1–3, Enter — 1]: "
    case "${INST_CHOICE:-1}" in
        1) INSTALL_MODE="update" ;;
        2) INSTALL_MODE="fresh" ;;
        3) INSTALL_MODE="uninstall" ;;
        *) fail "Неверный выбор: введи 1, 2 или 3" ;;
    esac
    echo
fi

# cli.env прежней установки читаем сразу: при переустановке каталог проекта удаляется
# раньше, чем до него доходит дело, а из него берутся роль и порты, зашитые в уже
# выданные ссылки (SUB_PORT, NODE_PORT) и в подключение ноды к панели.
PREV_CLI_ENV=""
[[ -f "$PROJECT/cli/cli.env" ]] && PREV_CLI_ENV=$(cat "$PROJECT/cli/cli.env")
PREV_ROLE=""
if [[ -n "$PREV_CLI_ENV" ]]; then
    if   [[ -n "$(prev_val CORE_HOST)" ]];        then PREV_ROLE="node"
    elif [[ "$(prev_val LOCAL_NODE)" == "off" ]]; then PREV_ROLE="core"
    else                                               PREV_ROLE="standalone"
    fi
fi

if [[ "$INSTALL_MODE" == "uninstall" ]]; then
    uninstall_all
    exit 0
fi
# Роль сервера (несколько серверов под одной панелью — README, «Несколько серверов»):
#   standalone — панель + VPN на одной машине, как было всегда; может принимать ноды;
#   core       — только панель, без VPN на этой машине (LOCAL_NODE=off): AmneziaWG, модуль
#                ядра и заголовки не ставятся, VPN-серверы подключаются к ней нодами;
#   node       — VPN без панели: awg-ctrl + awg-agent, который сам подключается к панели
#                по строке awgjoin://… («Добавить сервер» в панели). Входящих портов,
#                кроме UDP для клиентов, нода не открывает.
# Можно задать окружением: ROLE=node JOIN='awgjoin://…' bash install.sh
# При обновлении роль берётся из прежней установки: смена роли — только переустановкой.
ROLE="${ROLE:-}"
if [[ "$INSTALL_MODE" == "update" ]]; then
    if [[ -n "$ROLE" && -n "$PREV_ROLE" && "$ROLE" != "$PREV_ROLE" ]]; then
        fail "Смена роли ($PREV_ROLE → $ROLE) при обновлении невозможна — выбери полную переустановку"
    fi
    ROLE="${PREV_ROLE:-standalone}"
elif [[ -z "$ROLE" ]]; then
    case "$PREV_ROLE" in node) ROLE_DEF=3 ;; core) ROLE_DEF=2 ;; *) ROLE_DEF=1 ;; esac
    echo
    echo -e "  ${BLD}Роль сервера${NC}"
    echo -e "    ${ACC}${BLD}1${NC}  Панель + VPN    ${DIM}самостоятельный сервер; к нему можно подключать ноды${NC}"
    echo -e "    ${ACC}${BLD}2${NC}  Только панель   ${DIM}без VPN на этой машине; серверы подключаются нодами${NC}"
    echo -e "    ${ACC}${BLD}3${NC}  Нода            ${DIM}VPN без панели; подключается к уже установленной${NC}"
    ask ROLE_CHOICE "Выбери [1–3, Enter — ${ROLE_DEF}]: "
    case "${ROLE_CHOICE:-$ROLE_DEF}" in
        1) ROLE="standalone" ;;
        2) ROLE="core" ;;
        3) ROLE="node" ;;
        *) fail "Неверный выбор: введи 1, 2 или 3" ;;
    esac
fi
[[ "$ROLE" =~ ^(standalone|core|node)$ ]] || fail "ROLE должен быть standalone, core или node (задано: $ROLE)"
case "$ROLE" in
    standalone) ROLE_LABEL="панель + VPN" ;;
    core)       ROLE_LABEL="только панель" ;;
    node)       ROLE_LABEL="нода" ;;
esac

# Запускается ДО вопросов конфигурации и любого деструктива (rm -rf): на несовместимой
# машине лучше упасть сразу, а не после ввода данных или удаления каталога.
# Порты (UDP VPN / порт UI) здесь НЕ проверяем — это отдельно (фаервол/облако).
step "Проверка совместимости"

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

echo -e "  ${DIM}${OS_NAME:-$(lsb_release -ds 2>/dev/null || echo unknown)} · ядро $KERNEL · виртуализация $VIRT · роль: $ROLE_LABEL${NC}"

# Считаем все пункты, НЕ падая на первом, — чтобы показать полный чеклист
# со статусом по каждому. Если хоть один не прошёл — печатаем причины и выходим
# с кодом 0 (чистый выход, без вида «упало с ошибкой»).
OS_OK=1; ARCH_OK=1; VIRT_OK=1; HDR_OK=1; NET_OK=1
OS_WHY=""; ARCH_WHY=""; VIRT_WHY=""; HDR_WHY=""; NET_WHY=""

# Архитектура: релиз собирается под amd64 и arm64.
if [[ -z "$GOARCH" ]]; then
    ARCH_OK=0
    ARCH_WHY="архитектура '$(uname -m)' не поддерживается — сборки есть для x86_64 и aarch64"
fi

# 0) Дистрибутив: установщик умеет только apt-семейство Ubuntu/Debian.
if [[ -z "$OS_FAMILY" ]]; then
    OS_OK=0
    OS_WHY="дистрибутив '${OS_NAME:-${OS_ID:-неизвестно}}' не поддерживается — нужен Ubuntu или Debian"
fi

# Виртуализация и заголовки нужны только для модуля ядра — панели без VPN (core) они не важны,
# её можно ставить и в контейнер.
if [[ "$ROLE" != "core" ]]; then
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
fi

# 3) Доступ в интернет: нужен для архива и пакетов AWG. Без -f: любой
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

mark "$OS_OK"   "Дистрибутив"
mark "$ARCH_OK" "Архитектура"
if [[ "$ROLE" != "core" ]]; then
    mark "$VIRT_OK" "Виртуализация"
    mark "$HDR_OK"  "Заголовки ядра"
fi
mark "$NET_OK"  "Доступ в интернет"

if [[ "$OS_OK" == 0 || "$ARCH_OK" == 0 || "$VIRT_OK" == 0 || "$HDR_OK" == 0 || "$NET_OK" == 0 ]]; then
    echo
    warn "Установка невозможна — не пройдены проверки:"
    [[ "$OS_OK"   == 0 ]] && echo -e "    ${RED}•${NC} $OS_WHY"
    [[ "$ARCH_OK" == 0 ]] && echo -e "    ${RED}•${NC} $ARCH_WHY"
    [[ "$VIRT_OK" == 0 ]] && echo -e "    ${RED}•${NC} $VIRT_WHY"
    [[ "$HDR_OK"  == 0 ]] && echo -e "    ${RED}•${NC} $HDR_WHY"
    [[ "$NET_OK"  == 0 ]] && echo -e "    ${RED}•${NC} $NET_WHY"
    echo
    trap - ERR
    exit 0
fi

NET_IFACE=$(ip route show default 2>/dev/null | awk '/default/{print $5; exit}')
[[ -z "$NET_IFACE" ]] && fail "Не могу определить сетевой интерфейс"

# ── Обновление: новая версия поверх, настройки и данные на месте ──────────────
if [[ "$INSTALL_MODE" == "update" ]]; then
    step "Обновление до версии ${VERSION} ($ROLE_LABEL)"

    info "останавливаем и убиваем все процессы awg-control"
    kill_related

    download_extract
    install_command

    # Установке без секретного пути панели — выдаём его (UI_PREFIX=off — оставить без пути):
    # иначе обновлённый сервер по-прежнему узнаётся сканером по странице логина. cli.env
    # обновление не переписывает, поэтому дописываем одну строку.
    UI_PREFIX_NEW="n"
    UI_PREFIX="${UI_PREFIX:-}"
    if [[ "$ROLE" != "node" ]]; then
        CLI_ENV="$PROJECT/cli/cli.env"
        if ! grep -q '^UI_PREFIX=' "$CLI_ENV" 2>/dev/null && [[ "$UI_PREFIX" != "off" ]]; then
            UI_PREFIX="${UI_PREFIX:-$(rand_alnum 16)}"
            [[ "$UI_PREFIX" =~ ^[A-Za-z0-9_-]{8,64}$ ]] \
                || fail "UI_PREFIX «${UI_PREFIX}» — нужно 8–64 символа A–Z a–z 0–9 _ -"
            printf 'UI_PREFIX=%s\n' "$UI_PREFIX" >> "$CLI_ENV"
            UI_PREFIX_NEW="y"
            ok "Панели выдан секретный путь"
        fi
        UI_PREFIX=$(sed -n 's/^UI_PREFIX=//p' "$CLI_ENV" 2>/dev/null | tail -1)
    fi

    start_and_status

    echo
    card_line "$GRN"
    card_line "$GRN" "${GRN}${BLD}✓ Обновлено до ${VERSION}${NC}   ${DIM}${BRAND} · ${ROLE_LABEL}${NC}"
    card_line "$GRN"
    if [[ "$ROLE" != "node" ]]; then
        HOST="$(prev_val SUB_HOST)"; HOST="${HOST:-$(prev_val SERVER_IP)}"
        card_line "$GRN" "${DIM}Панель   ${NC} ${BLD}$(panel_url "${HOST:-IP}" "$(prev_val UI_PORT)" "$UI_PREFIX")${NC}"
        if [[ "$UI_PREFIX_NEW" == "y" ]]; then
            card_line "$GRN" "         ${YLW}путь новый: без него панель отвечает 404 — обнови закладки и адрес${NC}"
            card_line "$GRN" "         ${YLW}внешнего API (/${UI_PREFIX}/api/v1/…). Вернуть как было: UI_PREFIX= в cli.env${NC}"
        fi
        card_line "$GRN"
    fi
    echo
    exit 0
fi

step "Конфигурация"

# Внешний IPv4 — через ifconfig.me, иначе адрес, с которого сервер ходит наружу. Не спрашиваем.
SERVER_IP=$(curl -s4 --connect-timeout 5 ifconfig.me 2>/dev/null || true)
if ! [[ "$SERVER_IP" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
    SERVER_IP=$(ip route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit }}')
fi
[[ -n "$SERVER_IP" ]] || fail "Не удалось определить адрес сервера"
ok "Адрес сервера: $SERVER_IP"

# Что сохранять при переустановке — спрашиваем первым: от ответа зависят значения по умолчанию.
#   С VPN (standalone, node): база пользователей и ключ сервера — только вместе:
#   vpn:// ключи пользователей привязаны к ключу сервера. ui.db следует за ними.
#   Панель без VPN (core): её данные — ui.db (серверы, ключи API, мастер-ключи).
KEEP_DATA="n"
if [[ "$WIPE" == "y" ]]; then
    :   # устаревшая установка: сохранять нечего, сброс уже подтверждён
elif [[ "$ROLE" == "core" ]]; then
    if [[ -f "$UI_DB_FILE" ]]; then
        echo
        warn "Найдены данные панели: $UI_DB_FILE"
        ask KD "Сохранить серверы, ключи API и мастер-ключи? [Y/n]: "
        [[ "${KD:-y}" =~ ^[Yy]$ ]] && KEEP_DATA="y"
    fi
elif [[ -f "$DB_FILE" || -f "$PRIV_KEY_FILE" ]]; then
    echo
    warn "Найдена существующая установка:"
    [[ -f "$DB_FILE" ]]       && echo "    база пользователей: $DB_FILE"
    [[ -f "$PRIV_KEY_FILE" ]] && echo "    ключ сервера:       $PRIV_KEY_FILE"

    if [[ -f "$DB_FILE" && -f "$PRIV_KEY_FILE" && -f "$PUB_KEY_FILE" ]]; then
        ask KD "Сохранить пользователей и ключ сервера? [Y/n]: "
        [[ "${KD:-y}" =~ ^[Yy]$ ]] && KEEP_DATA="y"
    else
        warn "Для сохранения нужны и база, и оба ключа сервера — часть отсутствует."
        warn "Пользователей не сохранить (vpn:// ключи стали бы невалидными)."
    fi
fi

# Панель (awg-ui), подписка sen:// и приём нод есть везде, кроме ноды. Значения из окружения
# (UI_PORT=…, SUB_PORT=…) важнее и не спрашиваются.
UI_PORT="${UI_PORT:-}"; UI_USER="${UI_USER:-}"; UI_PASS="${UI_PASS:-}"
SUB_PORT="${SUB_PORT:-}"; SUB_TLS="${SUB_TLS:-}"; NODE_PORT="${NODE_PORT:-}"
JWT_SECRET=""
if [[ "$ROLE" != "node" ]]; then
    DEF=$(keep UI_PORT); DEF="${DEF:-$(gen_tcp_port)}"
    ask_def UI_PORT "Порт панели [$DEF]: " "$DEF"
    valid_port "$UI_PORT" || fail "Порт панели «$UI_PORT» — не номер порта"

    DEF=$(keep UI_USER); DEF="${DEF:-admin}"
    ask_def UI_USER "Логин панели [$DEF]: " "$DEF"

    DEF=$(keep UI_PASS)
    if [[ -n "$DEF" ]]; then
        ask_def UI_PASS "Пароль панели [Enter — прежний]: " "$DEF" secret
    else
        DEF=$(rand_alnum 12)
        ask_def UI_PASS "Пароль панели [$DEF]: " "$DEF" secret
    fi
    # Прежний секрет — чтобы открытые вкладки панели не разлогинило.
    JWT_SECRET=$(keep JWT_SECRET); JWT_SECRET="${JWT_SECRET:-$(rand_alnum 32)}"

    SUB_PORT="${SUB_PORT:-$(keep SUB_PORT)}"
    if [[ -z "$SUB_PORT" ]]; then
        DEF=$(gen_tcp_port "$UI_PORT")
        ask_def SUB_PORT "Порт подписки sen:// [$DEF]: " "$DEF"
    fi
    valid_port "$SUB_PORT" || fail "Порт подписки «$SUB_PORT» — не номер порта"
    [[ "$SUB_PORT" != "$UI_PORT" ]] || fail "Порт подписки совпадает с портом панели ($UI_PORT)"

    SUB_TLS="${SUB_TLS:-$(keep SUB_TLS)}"
    if [[ -z "$SUB_TLS" ]]; then
        ask ST "Подписка sen:// по HTTPS (самоподписанный сертификат, домен не нужен)? [Y/n]: "
        [[ "${ST:-y}" =~ ^[Nn]$ ]] && SUB_TLS="off" || SUB_TLS="on"
    fi
    [[ "$SUB_TLS" == "on" ]] || SUB_TLS="off"

    # Приём нод: ноды сами подключаются к панели по WSS на этот порт. Сертификат хаба awg-ui
    # создаёт при первом запуске, его отпечаток зашит в строки подключения. Панели без VPN
    # приём нод обязателен (иначе ей нечем управлять), у «панели + VPN» — по желанию.
    NODE_PORT="${NODE_PORT:-$(keep NODE_PORT)}"
    if [[ -z "$NODE_PORT" ]]; then
        if [[ "$ROLE" == "core" ]]; then
            DEF=$(gen_tcp_port "$UI_PORT" "$SUB_PORT")
            ask_def NODE_PORT "Порт для подключения нод [$DEF]: " "$DEF"
        else
            ask NODE_PORT "Порт для подключения нод (Enter — не принимать ноды, «+» — случайный): "
            [[ "$NODE_PORT" == "+" ]] && NODE_PORT=$(gen_tcp_port "$UI_PORT" "$SUB_PORT")
        fi
    fi
    if [[ -n "$NODE_PORT" ]]; then
        valid_port "$NODE_PORT" || fail "Порт для нод «$NODE_PORT» — не номер порта"
        [[ "$NODE_PORT" != "$UI_PORT" && "$NODE_PORT" != "$SUB_PORT" ]] \
            || fail "Порт для нод совпадает с портом панели или подписки"
    fi
fi
# awg-ctrl слушает только loopback; порт внутренний, при каждой установке новый.
AWGCTRL_PORT=$(gen_tcp_port "${UI_PORT:-}" "${SUB_PORT:-}" "${NODE_PORT:-}")

# Порт, MTU и параметры обфускации VPN (choose_awg_params).
[[ "$ROLE" != "core" ]] && choose_awg_params

if [[ "$ROLE" != "node" ]]; then
    # Секретный путь панели: снаружи она отвечает только на http://IP:PORT/<путь>/, на остальное —
    # 404, как пустой сервер. Без него сканер узнавал панель по странице логина одним GET.
    # При переустановке путь прежний; UI_PREFIX=off — без пути, UI_PREFIX=<свой> — свой
    # (8–64 символа A–Z a–z 0–9 _ -).
    UI_PREFIX="${UI_PREFIX:-}"
    UI_PREFIX_NEW="n"
    if [[ "$UI_PREFIX" == "off" ]]; then
        UI_PREFIX=""
    elif [[ -z "$UI_PREFIX" ]]; then
        UI_PREFIX=$(keep UI_PREFIX)
        if [[ -z "$UI_PREFIX" ]]; then
            UI_PREFIX=$(rand_alnum 16)
            [[ -n "$PREV_CLI_ENV" ]] && UI_PREFIX_NEW="y"
        fi
    fi
    [[ -z "$UI_PREFIX" || "$UI_PREFIX" =~ ^[A-Za-z0-9_-]{8,64}$ ]] \
        || fail "UI_PREFIX «${UI_PREFIX}» — нужно 8–64 символа A–Z a–z 0–9 _ -"
    # Путь переехавшей сюда панели (migrate) — её закладкам и API через ретранслятор.
    UI_PREFIX_MOVED=$(keep UI_PREFIX_MOVED)
fi

# Нода: к какой панели подключаться (строка awgjoin://… из «Добавить сервер» в панели).
CORE_HOST=""; CORE_PORT=""; CORE_PIN=""; NODE_ID=""; JOIN_SECRET=""
if [[ "$ROLE" == "node" ]]; then
    JOIN="${JOIN:-}"
    # Переустановка ноды: ключ ноды на месте, панель её уже знает — можно не выпускать
    # новую строку, а оставить прежнее подключение.
    if [[ -z "$JOIN" && "$WIPE" != "y" && "$PREV_ROLE" == "node" && -n "$(prev_val CORE_HOST)" && -f "$NODE_KEY_FILE" ]]; then
        ask KJ "Оставить подключение к панели $(prev_val CORE_HOST):$(prev_val CORE_PORT) (нода #$(prev_val NODE_ID))? [Y/n]: "
        if [[ "${KJ:-y}" =~ ^[Yy]$ ]]; then
            CORE_HOST="$(prev_val CORE_HOST)"; CORE_PORT="$(prev_val CORE_PORT)"
            CORE_PIN="$(prev_val CORE_PIN)";   NODE_ID="$(prev_val NODE_ID)"
            JOIN_SECRET="$(prev_val JOIN_SECRET)"   # пусто, если нода уже подключалась
        fi
    fi
    if [[ -z "$CORE_HOST" ]]; then
        [[ -z "$JOIN" ]] && ask JOIN "Строка подключения к панели (awgjoin://…): "
        parse_join "$JOIN" \
            || fail "Строка подключения повреждена — возьми новую в панели: «Добавить сервер»"
    fi
    ok "Панель: $CORE_HOST:$CORE_PORT · нода #$NODE_ID"

    # Проверяем сразу: отпечаток не совпал — это не та панель (или соединение перехвачено),
    # ставить дальше нет смысла. Не ответила — не страшно: агент подключится, когда будет связь.
    GOT_PIN=$(core_pin "$CORE_HOST" "$CORE_PORT")
    if [[ -z "$GOT_PIN" ]]; then
        warn "Панель $CORE_HOST:$CORE_PORT сейчас не отвечает — нода подключится, когда появится связь."
        warn "Проверь, что порт открыт на стороне панели."
    elif [[ "$GOT_PIN" != "$CORE_PIN" ]]; then
        fail "Сертификат на $CORE_HOST:$CORE_PORT не совпадает с отпечатком из строки подключения — это не та панель"
    else
        ok "Сертификат панели совпал с отпечатком"
    fi
fi

# Имя сервера — только там, где есть VPN. При сохранённой базе имя хранится в ней (его меняют
# в панели) — не спрашиваем; иначе спрашиваем, по умолчанию прежнее.
SERVER_NAME="${SERVER_NAME:-}"
if [[ "$ROLE" != "core" ]]; then
    if [[ "$KEEP_DATA" == "y" ]]; then
        SERVER_NAME="${SERVER_NAME:-$(prev_val SERVER_NAME)}"
        info "Имя сервера — из сохранённой базы"
    else
        DEF=$(prev_val SERVER_NAME); DEF="${DEF:-VPN}"
        ask_def SERVER_NAME "Имя сервера [$DEF]: " "$DEF"
    fi
fi
SERVER_NAME="${SERVER_NAME:-VPN}"

# Сводка. Метки выровнены пробелами прямо в строках: printf в bash считает ширину в байтах,
# и кириллица поехала бы.
echo
card_line "$ACC" "${BLD}Будет установлено${NC}"
card_line "$ACC"
card_line "$ACC" "${DIM}Роль           ${NC} ${BLD}$ROLE_LABEL${NC}"
card_line "$ACC" "${DIM}Адрес          ${NC} $SERVER_IP  ${DIM}· интерфейс $NET_IFACE${NC}"
if [[ "$ROLE" != "core" ]]; then
    card_line "$ACC" "${DIM}Имя сервера    ${NC} $SERVER_NAME"
    card_line "$ACC" "${DIM}VPN            ${NC} ${AWG_PORT}/udp  ${DIM}· MTU ${MTU}${NC}"
fi
if [[ "$ROLE" != "node" ]]; then
    card_line "$ACC" "${DIM}Панель         ${NC} ${UI_PORT}/tcp  ${DIM}· логин $UI_USER · $([[ -n "$UI_PREFIX" ]] && echo "секретный путь" || echo "без секретного пути")${NC}"
    card_line "$ACC" "${DIM}Подписка sen://${NC} ${SUB_PORT}/tcp  ${DIM}· $([[ "$SUB_TLS" == "on" ]] && echo "HTTPS" || echo "HTTP, ответы подписаны")${NC}"
    if [[ -n "$NODE_PORT" ]]; then
        card_line "$ACC" "${DIM}Приём нод      ${NC} ${NODE_PORT}/tcp"
    else
        card_line "$ACC" "${DIM}Приём нод      ${NC} ${DIM}выключен${NC}"
    fi
else
    card_line "$ACC" "${DIM}Панель         ${NC} $CORE_HOST:$CORE_PORT  ${DIM}· нода #$NODE_ID${NC}"
fi
if [[ "$KEEP_DATA" == "y" ]]; then
    if [[ "$ROLE" == "core" ]]; then
        card_line "$ACC" "${DIM}Данные         ${NC} ${GRN}сохраняются: серверы и ключи панели${NC}"
    else
        card_line "$ACC" "${DIM}Данные         ${NC} ${GRN}сохраняются: пользователи и ключ сервера${NC}"
    fi
elif [[ "$WIPE" == "y" ]]; then
    card_line "$ACC" "${DIM}Данные         ${NC} ${RED}все ключи и данные удаляются${NC} ${DIM}(устаревшая версия ${PREV_VERSION:-?}; базы — в бэкап)${NC}"
elif [[ "$ROLE" != "core" && -f "$DB_FILE" ]] || [[ "$ROLE" == "core" && -f "$UI_DB_FILE" ]]; then
    card_line "$ACC" "${DIM}Данные         ${NC} ${YLW}с нуля, старая база уйдёт в бэкап${NC}"
fi
card_line "$ACC" "${DIM}Каталог        ${NC} ${DIM}$PROJECT${NC}"
card_line "$ACC"
echo

ask YN "Продолжить? [Y/n]: "
[[ "${YN:-y}" =~ ^[Nn]$ ]] && { echo "  Отменено."; exit 0; }

# Переустановка: прежние процессы и каталог убираем только после подтверждения — отказ на
# «Продолжить?» не должен оставлять сервер без установки. Данные (/etc/amnezia) на месте,
# cli.env прочитан в начале (PREV_CLI_ENV).
if [[ -d "$PROJECT" ]]; then
    kill_related
    rm -rf "${PROJECT:?}"
fi

# Совместимость (виртуализация, заголовки ядра, интернет) проверена выше в
# секции «Проверка совместимости»; $KERNEL задан там же.
step "1/6  AmneziaWG"

if [[ "$ROLE" == "core" ]]; then
    skip "пропущено — у панели без VPN AmneziaWG не нужен"
else

    if command -v awg &>/dev/null && command -v awg-quick &>/dev/null && modinfo amneziawg &>/dev/null; then
        ok "AWG уже установлен (модуль amneziawg: $(modinfo -F version amneziawg 2>/dev/null || echo present))"
    else
        info "apt-get update"
        run apt-get update || fail "apt-get update упал — проверь /etc/apt/sources.list*"

        # iptables — в обоих списках: его зовёт PostUp в awg1.conf, а в Debian 13
        # (nftables по умолчанию) он в минимальной системе не установлен.
        if [[ "$OS_FAMILY" == "debian" ]]; then
            DEPS=(gnupg ca-certificates curl dkms build-essential iptables)
        else
            DEPS=(software-properties-common python3-launchpadlib gnupg2 dkms build-essential iptables)
        fi
        info "установка зависимостей сборки"
        DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
            run apt-get install -y "${DEPS[@]}" \
            || fail "Не удалось установить зависимости сборки: ${DEPS[*]}"

        # Заголовки — отдельным шагом: их нехватка — самая частая причина сбоя, и на
        # неё нужен точный совет, а не общее «не удалось установить». Ошибку apt
        # здесь не валим: решает наличие /lib/modules/$KERNEL/build ниже.
        HDR_PKGS=("linux-headers-$KERNEL")
        [[ "$OS_FAMILY" == "ubuntu" ]] && HDR_PKGS+=(linux-headers-generic)
        info "установка заголовков ядра: ${HDR_PKGS[*]}"
        DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
            run apt-get install -y "${HDR_PKGS[@]}" \
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
            info "add-apt-repository ppa:amnezia/ppa"
            run add-apt-repository -y ppa:amnezia/ppa \
                || fail "Не удалось добавить PPA ppa:amnezia/ppa"
        fi

        info "apt-get update (после PPA)"
        run apt-get update \
            || fail "apt-get update после PPA упал — проверь источники amnezia в /etc/apt"

        info "установка amneziawg (сборка DKMS-модуля, может занять до минуты)"
        if ! DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
             run apt-get install -y amneziawg; then
            warn "apt-get install amneziawg завершился с ошибкой."
            MKLOG=$(ls -1t /var/lib/dkms/amneziawg/*/build/make.log 2>/dev/null | head -1 || true)
            if [[ -n "${MKLOG:-}" && -f "$MKLOG" ]]; then
                info "$MKLOG — последние 40 строк"
                tail -n 40 "$MKLOG" | logblock
            else
                warn "make.log не найден — ошибка, вероятно, на этапе apt/репозиториев."
            fi
            fail "Не удалось установить amneziawg (детали выше и в $LOGFILE)"
        fi

        info "проверка собранного модуля"
        run dkms status amneziawg || true
        if ! run modprobe amneziawg; then
            MKLOG=$(ls -1t /var/lib/dkms/amneziawg/*/build/make.log 2>/dev/null | head -1 || true)
            [[ -n "${MKLOG:-}" && -f "$MKLOG" ]] && { info "$MKLOG — последние 40 строк"; tail -n 40 "$MKLOG" | logblock; }
            fail "Модуль amneziawg не загрузился — DKMS-сборка несовместима с ядром"
        fi
        ok "AWG установлен, модуль amneziawg собран и загружается"
    fi


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
            run apt-mark unhold amneziawg amneziawg-dkms amneziawg-tools || true
            run apt-get update || true
            DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a \
                run apt-get install -y --only-upgrade amneziawg amneziawg-dkms amneziawg-tools || true
            MOD_VER=$(disk_mod_ver)
        fi

        # Загруженный отстал от дискового — перезагружаем. Интерфейс держит модуль,
        # поэтому сначала опускаем его; awg1 на этом шаге ещё не нужен.
        LOADED_VER=$(loaded_mod_ver)
        if [[ -n "$LOADED_VER" && "$LOADED_VER" != "$MOD_VER" ]]; then
            warn "В ядре загружен модуль $LOADED_VER, на диске $MOD_VER — перезагружаем модуль."
            run awg-quick down "$IFACE" || true
            ip link delete dev "$IFACE" 2>/dev/null || true
            if run modprobe -r amneziawg && run modprobe amneziawg; then
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
fi

step "2/6  Файлы проекта"

download_extract

step "3/6  Ключи и конфиг AWG"

mkdir -p "$AWG_DIR" "$AMNEZIA_DIR"

# Сброс устаревшей установки: ключ ноды — тоже в бэкап, агент создаст новый (ноду добавляют в
# панель заново). Остальное (базы, ключи сервера и подписи) сбрасывается как при KEEP_DATA=n ниже.
if [[ "$WIPE" == "y" && -f "$NODE_KEY_FILE" ]]; then
    mv "$NODE_KEY_FILE" "${NODE_KEY_FILE}.bak-$(date +%Y%m%d-%H%M%S)"
    warn "Старый ключ ноды убран в бэкап — ноду нужно заново добавить в панели"
fi

# ui.db (серверы, API-ключи, мастер-ключи awg-ui) — в бэкап, чтобы awg-ui создал чистую БД.
if [[ "$KEEP_DATA" != "y" && -f "$UI_DB_FILE" ]]; then
    UI_DB_BAK="${UI_DB_FILE}.bak-$(date +%Y%m%d-%H%M%S)"
    mv "$UI_DB_FILE" "$UI_DB_BAK"
    # WAL-сайдкары удаляем — к новой БД они неприменимы.
    rm -f "${UI_DB_FILE}-wal" "${UI_DB_FILE}-shm"
    warn "Старая база панели сохранена: $UI_DB_BAK"
fi

# Ключ сервера и база пользователей — только там, где есть VPN.
if [[ "$ROLE" != "core" ]]; then
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

        PRIV_KEY=$(umask 077 && awg genkey)
        PUB_KEY=$(printf '%s' "$PRIV_KEY" | awg pubkey)

        umask 077
        printf '%s' "$PRIV_KEY" > "$PRIV_KEY_FILE"
        printf '%s' "$PUB_KEY"  > "$PUB_KEY_FILE"
        chmod 600 "$PRIV_KEY_FILE" "$PUB_KEY_FILE"

        ok "Публичный ключ: $PUB_KEY"
    fi
fi

# Внутренняя авторизация awg-ui → awg-ctrl: Ed25519-пара. Приватный → awg-ui,
# публичный → awg-ctrl. Перегенерируется при каждой установке (эфемерна: обе
# стороны переписываются вместе) — даже при KEEP_DATA, на vpn:// ключи не влияет.
( umask 077
  openssl genpkey -algorithm ed25519 -out "$INTERNAL_AUTH_PRIV"
  openssl pkey -in "$INTERNAL_AUTH_PRIV" -pubout -out "$INTERNAL_AUTH_PUB" )
chmod 600 "$INTERNAL_AUTH_PRIV" "$INTERNAL_AUTH_PUB"
ok "Ключи внутренней авторизации awg-ui ↔ awg-ctrl"

# Подписка sen:// живёт в панели — у ноды её нет.
if [[ "$ROLE" != "node" ]]; then
    # Подписка sen://. ⚠️ Существующие файлы при KEEP_DATA=y сохраняем: публичный ключ подписи и
    # отпечаток сертификата зашиты во все выданные ссылки, новый ключ сломал бы каждую. При чистой
    # установке ui.db уходит в бэкап (старые мастер-ключи всё равно недействительны) — ключи новые.
    if [[ "$KEEP_DATA" == "y" && -f "$SUB_SIGN_PRIV" ]]; then
        ok "Ключ подписи sen:// сохранён (существующие ссылки продолжат работать)"
    else
        ( umask 077; openssl genpkey -algorithm ed25519 -out "$SUB_SIGN_PRIV" )
        chmod 600 "$SUB_SIGN_PRIV"
        ok "Ключ подписи sen:// создан"
    fi
    # Имя в сертификате случайное: одинаковое у всех серверов было бы сигнатурой для сканера.
    # Клиент сверяет pin ключа, а не имя.
    if [[ "$SUB_TLS" == "on" ]]; then
        if [[ "$KEEP_DATA" == "y" && -f "$SUB_TLS_CRT" && -f "$SUB_TLS_KEY" ]]; then
            ok "Сертификат подписки sen:// сохранён"
        else
            ( umask 077
              openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
                  -keyout "$SUB_TLS_KEY" -out "$SUB_TLS_CRT" -days 3650 \
                  -subj "/CN=$(rand_alnum 10 | tr 'A-Z' 'a-z').net" 2>/dev/null )
            chmod 600 "$SUB_TLS_KEY" "$SUB_TLS_CRT"
            ok "Самоподписанный сертификат подписки sen:// создан (10 лет)"
        fi
    fi
fi

if [[ "$ROLE" != "core" ]]; then
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
I1 = ${I1}
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
fi

step "4/6  Запуск AWG"

if [[ "$ROLE" == "core" ]]; then
    skip "пропущено — у панели без VPN интерфейса нет"
    if command -v awg &>/dev/null && awg show "$IFACE" &>/dev/null; then
        warn "Интерфейс $IFACE от прошлой установки ещё поднят — панель его не использует."
        warn "Опустить: awg-quick down $IFACE"
    fi
else

    if awg show "$IFACE" &>/dev/null; then
        warn "Интерфейс $IFACE уже существует — перезапускаем"
        run awg-quick down "$IFACE" || true
    fi

    # Подъём интерфейса — единственная настоящая проверка, что ядро приняло
    # 3.1-параметры. Модуль на отказ отвечает лишь «Invalid argument», причину
    # печатает только в dmesg и только при включённом dynamic debug. Поэтому:
    # упали → включаем debug, пробуем ещё раз, показываем причину, и если и это не
    # помогло — снимаем 3.x-ключи и поднимаемся на 2.0, а не валим установку.
    AWG3_KEY_RE='^(HeaderProtectionKey|ContentPaddingAddition|RekeyAfterTime|RekeyTimeout|RejectAfterTime|KeepaliveTimeout|MaxHandshakeAttempts|RandomTrailers|DisableCookies) *='

    info "awg-quick up $IFACE"
    if ! run awg-quick up "$IFACE"; then
        ip link delete dev "$IFACE" 2>/dev/null || true

        if [[ "$AWG3" != "y" ]]; then
            fail "awg-quick up $IFACE завершился с ошибкой"
        fi

        warn "Ядро отвергло конфиг AmneziaWG 3.1. Выясняем причину."
        echo "module amneziawg +p" > /sys/kernel/debug/dynamic_debug/control 2>/dev/null || true

        if run awg-quick up "$IFACE"; then
            ok "Со второй попытки интерфейс поднялся"
        else
            ip link delete dev "$IFACE" 2>/dev/null || true
            info "dmesg — последние 15 строк amneziawg"
            dmesg 2>/dev/null | grep -i amneziawg | tail -n 15 | logblock || true

            cp "$AWG_CONF" "${AWG_CONF}.awg31"
            sed -i -E "/$AWG3_KEY_RE/d" "$AWG_CONF"
            AWG3="n"; NEW_GEN="2.0"
            warn "Откатываемся на AmneziaWG 2.0. Конфиг 3.1 сохранён: ${AWG_CONF}.awg31"
            [[ "$AWG_GEN" == "3.1" ]] && \
                warn "Модуль и ядро подходят под 3.1, но интерфейс поднялся на 2.0 — см. ${AWG_CONF}.awg31 и dmesg выше."
            warn "Разберись с причиной и повтори переход кнопкой «Перейти на AWG 3.1» в панели."

            run awg-quick up "$IFACE" \
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
        info "apt-mark hold amneziawg amneziawg-dkms amneziawg-tools"
        run apt-mark hold amneziawg amneziawg-dkms amneziawg-tools || true
        ok "Версия пакета закреплена (apt-mark hold) — снять: apt-mark unhold amneziawg amneziawg-dkms amneziawg-tools"
    fi

    GEN_LABEL=$([[ "$AWG3" == "y" ]] && echo "3.1" || echo "2.0")
    ok "AmneziaWG $GEN_LABEL · модуль ${MOD_VER:-?} · tools ${TOOLS_VER:-?}$([[ "$AWG_PIN" == "y" ]] && echo " · версия закреплена")"
fi

step "5/6  Фаервол"

# Порты, которые должны быть доступны снаружи. Нода открывает только UDP для клиентов: к панели
# она подключается сама.
OPEN_PORTS=()
[[ "$ROLE" != "core" ]] && OPEN_PORTS+=("${AWG_PORT}/udp")
[[ "$ROLE" != "node" ]] && OPEN_PORTS+=("${UI_PORT}/tcp" "${SUB_PORT}/tcp")
[[ "$ROLE" != "node" && -n "$NODE_PORT" ]] && OPEN_PORTS+=("${NODE_PORT}/tcp")
open_firewall

step "6/6  Запуск сервисов"

install_command
# cli.env по роли. Роль CLI узнаёт отсюда же: CORE_HOST → нода (awg-ctrl + awg-agent),
# LOCAL_NODE=off → панель без VPN (только awg-ui), иначе — awg-ctrl + awg-ui.
{
cat <<ENV
# Generated by install.sh — $(date -u '+%Y-%m-%d %H:%M UTC') · роль: ${ROLE}
ENV

if [[ "$ROLE" != "core" ]]; then
cat <<ENV

# ── awgctrl (Ring 0) ──────────────────────────────────────────────────────
AWGCTRL_PORT=${AWGCTRL_PORT}
SERVER_IP=${SERVER_IP}
SERVER_PORT=${AWG_PORT}
SERVER_NAME=${SERVER_NAME}
ENV
fi

cat <<ENV

# ── внутренняя авторизация awg-ui/awg-agent → awg-ctrl (Ed25519) ──────────
INTERNAL_AUTH_KEY_FILE=${INTERNAL_AUTH_PRIV}
INTERNAL_AUTH_PUB_FILE=${INTERNAL_AUTH_PUB}
ENV

if [[ "$ROLE" != "node" ]]; then
cat <<ENV

# ── ui (Ring 4) ───────────────────────────────────────────────────────────
UI_PORT=${UI_PORT}
UI_PREFIX=${UI_PREFIX}
UI_PREFIX_MOVED=${UI_PREFIX_MOVED}
UI_USER=${UI_USER}
UI_PASS=${UI_PASS}
JWT_SECRET=${JWT_SECRET}

# ── подписка sen:// для SenAWG (мастер-ключи) ──────────────────────────────
SUB_PORT=${SUB_PORT}
SUB_TLS=${SUB_TLS}
SUB_SIGN_KEY_FILE=${SUB_SIGN_PRIV}
SUB_TLS_KEY_FILE=${SUB_TLS_KEY}
SUB_TLS_CERT_FILE=${SUB_TLS_CRT}
SUB_HOST=${SERVER_IP}

# ── приём нод (несколько серверов) ────────────────────────────────────────
NODE_PORT=${NODE_PORT}
NODE_TLS_KEY_FILE=${NODE_TLS_KEY}
NODE_TLS_CERT_FILE=${NODE_TLS_CRT}
LOCAL_NODE=$([[ "$ROLE" == "core" ]] && echo off || echo on)
ENV
fi

# JOIN_SECRET одноразовый: агент сам сотрёт его отсюда после первого подключения.
if [[ "$ROLE" == "node" ]]; then
cat <<ENV

# ── нода: подключение к панели (из строки awgjoin://…) ─────────────────────
CORE_HOST=${CORE_HOST}
CORE_PORT=${CORE_PORT}
CORE_PIN=${CORE_PIN}
NODE_ID=${NODE_ID}
NODE_KEY_FILE=${NODE_KEY_FILE}
JOIN_SECRET=${JOIN_SECRET}
ENV
fi
} > "$PROJECT/cli/cli.env"

# Имя продукта попадает в cli.env, только если оператор задал его явно
# (BRAND=… перед запуском). Иначе оно берётся из .env, приехавшего в архиве, —
# чтобы источник правды оставался один.
[[ -n "$BRAND_OVERRIDE" ]] && echo "BRAND=${BRAND_OVERRIDE}" >> "$PROJECT/cli/cli.env"

chmod 600 "$PROJECT/cli/cli.env"
ok "$PROJECT/cli/cli.env"

AGENT_LOG="$PROJECT/logs/awg-agent.log"
AGENT_LOG_FROM=$(( $(wc -l 2>/dev/null < "$AGENT_LOG" || echo 0) + 1 ))

start_and_status

# Нода: ждём, пока агент подключится к панели — иначе оператор узнает о проблеме,
# только когда не увидит сервер в панели.
if [[ "$ROLE" == "node" ]]; then
    NODE_UP="n"
    for _ in $(seq 1 15); do
        if tail -n "+$AGENT_LOG_FROM" "$AGENT_LOG" 2>/dev/null | grep -q "подключено к core"; then NODE_UP="y"; break; fi
        sleep 1
    done
    if [[ "$NODE_UP" == "y" ]]; then
        ok "Нода подключилась к панели $CORE_HOST:$CORE_PORT"
    else
        warn "Нода пока не подключилась к панели."
        if [[ -s "$AGENT_LOG" ]]; then
            info "$AGENT_LOG — последние строки"
            tail -n "+$AGENT_LOG_FROM" "$AGENT_LOG" 2>/dev/null | tail -n 5 | logblock || true
        else
            info "агент ещё ничего не записал — проверь: awg-ctrl status"
        fi
    fi
fi

echo
card_line "$GRN"
card_line "$GRN" "${GRN}${BLD}✓ Готово${NC}   ${DIM}${BRAND} ${VERSION} · ${ROLE_LABEL}${NC}"
card_line "$GRN"
if [[ "$ROLE" != "node" ]]; then
    card_line "$GRN" "${DIM}Панель   ${NC} ${BLD}$(panel_url "$SERVER_IP" "$UI_PORT" "$UI_PREFIX")${NC}"
    if [[ -n "$UI_PREFIX" ]]; then
        card_line "$GRN" "         ${DIM}секретный путь: без него панель отвечает 404 — сохрани адрес целиком${NC}"
        [[ "$UI_PREFIX_NEW" == "y" ]] && \
            card_line "$GRN" "         ${YLW}путь новый: обнови закладки и адрес внешнего API (/${UI_PREFIX}/api/v1/…)${NC}"
    fi
    card_line "$GRN" "${DIM}Логин    ${NC} ${UI_USER}"
    card_line "$GRN" "${DIM}Пароль   ${NC} ${YLW}${UI_PASS}${NC}  ${DIM}сменить: awg-ctrl credentials${NC}"
    card_line "$GRN"
    card_line "$GRN" "${DIM}sen://   ${NC} $([[ "$SUB_TLS" == "on" ]] && echo https || echo http)://${SERVER_IP}:${SUB_PORT}  ${DIM}подписка SenAWG${NC}"
    [[ -n "$NODE_PORT" ]] && card_line "$GRN" "${DIM}Ноды     ${NC} wss://${SERVER_IP}:${NODE_PORT}  ${DIM}«Добавить сервер» в панели${NC}"
fi
if [[ "$ROLE" != "core" ]]; then
    card_line "$GRN" "${DIM}VPN      ${NC} ${SERVER_IP}:${AWG_PORT}/udp  ${DIM}· awg-ctrl на 127.0.0.1:${AWGCTRL_PORT}${NC}"
fi
if [[ "$ROLE" == "node" ]]; then
    card_line "$GRN" "${DIM}Панель   ${NC} wss://${CORE_HOST}:${CORE_PORT}  ${DIM}исходящее подключение${NC}"
    card_line "$GRN"
    card_line "$GRN" "Сервер «${SERVER_NAME}» появится в списке серверов панели — управляй им оттуда."
fi
card_line "$GRN"
echo
card_line "$YLW" "${YLW}⚠${NC}  Если у хостера свой фаервол (панель провайдера, security group) — открой там:"
card_line "$YLW" "   ${BLD}${OPEN_PORTS[*]}${NC}"
card_line "$YLW" "   ${DIM}иначе клиенты не подключатся, а на сервере всё будет выглядеть исправным${NC}"
echo
echo -e "    ${DIM}awg-ctrl                     ${NC} меню: запуск, статус, логин и пароль, обновление и удаление"
echo -e "    ${DIM}awg-ctrl status              ${NC} состояние сервисов и адрес панели"
echo -e "    ${DIM}systemctl restart awg-control${NC} перезапуск ${DIM}(автозапуск на загрузке включён)${NC}"
echo
