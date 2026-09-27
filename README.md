# Forgetting
Панель управления и API для VPN на базе [AmneziaWG](https://github.com/amnezia-vpn/amneziawg-linux-kernel-module) 
> [!WARNING]
> Данный проект предназначен только для использования в законных целях.
## Архитектура

Три компонента, устанавливаются в `/opt/awg-control/`:

| Компонент | Роль |
|---|---|
| **awg-ctrl** | Привилегированный REST-бэкенд. Управляет пирами через `awg`/`awg-quick`, ведёт SQLite-базу, генерирует `vpn://` ключи. Слушает только loopback. |
| **awg-ui** | Express + React. Веб-панель, аутентификация (JWT), прокси к awg-ctrl, публичный API `/api/v1` и своя база его ключей. Единственный сервис, доступный по сети. |
| **cli** | Менеджер процессов: `start`/`stop`/`restart`/`status`, смена логина/пароля панели, интерактивное TUI-меню. |

Внутренняя связь awg-ui → awg-ctrl подписывается асимметрично (Ed25519): приватный ключ только у awg-ui, awg-ctrl проверяет подпись публичным.

## Требования
- **ОС**: Ubuntu 24.04+ (Тесты проходили на Ubuntu 24.04.4 LTS) или Debian 11–13 (поддержка экспериментальная: PPA amnezia подключается вручную, серия подбирается по релизу Debian и переопределяется через `AMNEZIA_PPA_SUITE`)
- **Права**: root.
- **Ядро**: kvm (не OpenVZ/LXC/Docker/WSL — нужен загружаемый kernel-модуль AmneziaWG).
- **Заголовки ядра** под текущее ядро (для сборки DKMS-модуля).
- **Node.js 20.x** (ставится установщиком автоматически).

Совместимость установщик проверяет сам перед любыми изменениями.

## Установка

На сервере, от root:

```bash
bash <(curl -Ls https://git.ma7neko.ru/maeneko/forgetting/raw/branch/main/install.sh)
```

Установщик по умолчанию ставит сервер на AmneziaWG 3.1. Если модуль в ядре
старше 3.x или ядро старше 5.5, установка остановится с диагностикой — поставить
на 2.0 можно так:

```bash
AWG_GEN=auto bash <(curl -Ls https://git.ma7neko.ru/maeneko/forgetting/raw/branch/main/install.sh)
```

Перевести такой сервер на 3.1 позже можно кнопкой «Перейти на AWG 3.1» в панели —
переустановка не нужна, пользователи сохраняются, ключи перевыпускаются
автоматически.

Источник архива и имя панели не зашиты в код — задаются переменными
`REPO_BASE`, `REPO_FALLBACK`, `ARCHIVE_URL`, `BRAND` перед запуском установщика
(подробности — в комментариях `install.sh` и в `.env`).

## API

Наружу смотрят две независимые поверхности. **Публичный API** `/api/v1` —
стабильный контракт для сторонних программ, авторизация по API-ключу.
**Роуты панели** — всё, что умеет веб-интерфейс, авторизация по JWT. Базовый
URL в обоих случаях — адрес сервера и порт, выбранный при установке.

> [!WARNING]
> Роуты панели отдают приватные поля пиров (`vpn_key`, `psk_key`) — это не
> публичный контракт, в отличие от `/api/v1`. Не выставляй её порт в интернет
> без TLS и ограничения по адресам.

### Публичный API (`/api/v1`)

Ключ создаётся во вкладке «API-ключи» и уходит в заголовке `X-Api-Key`.
Открытое значение показывается один раз при создании — на сервере лежит только
SHA-256 и префикс для показа в списке. Наружу отдаются только публичные поля:
`psk_key` и `pub_key` не покидают сервер.

```bash
curl -s -X POST http://HOST:PORT/api/v1/users \
  -H "X-Api-Key: awgk_xxx" -H "Content-Type: application/json" \
  -d '{"name":"alice"}' | jq -r .vpn_key
```

| Метод | Что делает |
|---|---|
| `POST /api/v1/users` | Создаёт пира. Тело `{ "name": "alice" }`. `201` с `{ name, ip, gen, vpn_key }`; `400` — имя не прошло валидацию, `409` — уже есть |
| `GET /api/v1/users` | Список со счётчиками: `{ users: [{ name, ip, key_gen, online, lastHandshake, rx, tx }] }` |
| `GET /api/v1/users/:name` | `{ name, ip, gen, vpn_key }`; `404` — не найден |
| `DELETE /api/v1/users/:name` | `{ "success": true, "name": "alice" }` |

`gen` — поколение AmneziaWG, на параметрах которого собран ключ (`"2"` или
`"3.1"`). Операции только CRD: у пира нет изменяемых полей, «перевыпустить»
ключ = удалить и создать заново.

У ключа — **набор серверов**, которые он видит (панель → «API-ключи» → шестерёнка;
`0` — сервер самой панели, остальные — подключённые ноды). Пользователи `vpn://`
заводятся на сервере ключа по умолчанию; другой сервер из набора выбирается
заголовком `X-Server-Id: <id>`, сервер вне набора → `404`.

#### Мастер-ключи `sen://`

Мастер-ключ — ссылка `sen://…` для приложения SenAWG с лимитом устройств: каждое
устройство само регистрируется по ссылке и получает пира на всех серверах ключа,
его приватный ключ сервер не видит. API-ключ видит и меняет мастер-ключ, только
если **все** его серверы входят в набор API-ключа; остальные для него не
существуют (`404`).

```bash
curl -s -X POST http://HOST:PORT/api/v1/masterkeys \
  -H "X-Api-Key: awgk_xxx" -H "Content-Type: application/json" \
  -d '{"label":"Семья","device_limit":3}' | jq -r .link
```

| Метод | Что делает |
|---|---|
| `POST /api/v1/masterkeys` | Создаёт ключ. Тело `{ label, device_limit?, servers? }` (лимит 1–100, по умолчанию 3; без `servers` — весь набор API-ключа). `201` с `{ id, uuid, label, device_limit, devices, servers, created_at, link, tls }`; `403` — сервер вне набора, `503` — подписка не настроена (ключ не создаётся) |
| `GET /api/v1/masterkeys` | Список: `{ keys: [{ id, uuid, label, device_limit, devices, servers, created_at, deleting? }] }`. `uuid` — постоянный идентификатор ключа: не меняется ни при перевыпуске ссылки, ни при правке метки. Везде, где в пути `:id`, можно передать и `uuid` |
| `GET /api/v1/masterkeys/:id` | Ключ, ссылка и устройства одним запросом: те же поля + `link`, `tls`, `device_list` (`devices` — счётчик) |
| `PATCH /api/v1/masterkeys/:id` | `{ label?, device_limit?, servers? }` — смена серверов сразу добавляет/снимает пиров устройств |
| `POST /api/v1/masterkeys/:id/rotate` | Новая ссылка `{ id, link, tls }`; старая не принимает новые устройства, подключённые работают дальше |
| `POST /api/v1/masterkeys/:id/rekey` | Все устройства сменят ключи WireGuard при следующем опросе (IP и PSK сохраняются) |
| `DELETE /api/v1/masterkeys/:id` | `{ success, id, pending }` — ключ и устройства отключаются сразу; `pending` — серверы (ноды не в сети), которые ещё не сняли пиров, до тех пор ключ в списке с `deleting` |
| `GET /api/v1/masterkeys/:id/devices` | `{ devices: [{ id, device_name, platform, version, online, lastHandshake, rx, tx, last_seen, servers_ok, servers_total, … }] }` — статистика по всем серверам ключа |
| `DELETE /api/v1/masterkeys/:id/devices/:device` | Отвязывает устройство, место освобождается |
| `POST /api/v1/masterkeys/:id/devices/:device/rekey` | Устройство сменит ключ при следующем опросе |
| `POST /api/v1/masterkeys/:id/devices/:device/psk` | Новый PSK: `{ id, pending }` — `pending` серверов ждут ноду |

| Код | Когда |
|---|---|
| `401 {"error":"API key required"}` | заголовка нет или он не начинается с `awgk_` |
| `401 {"error":"Invalid API key"}` | ключ не найден — удалён или неверен |
| `502 {"error":"awg-ctrl недоступен"}` | awg-ui посредничает, а awg-ctrl не отвечает |

Ключи лежат в собственной БД панели (`/etc/amnezia/amneziawg/ui.db`) — вне
каталога установки, поэтому переустановка их не трогает. Сама awg-ctrl про
внешние ключи ничего не знает: awg-ui ходит к ней своей внутренней авторизацией.
Управлять ключами можно и без панели, по JWT: `GET/POST /ui/apikeys` (с полем
`servers`), `PATCH /ui/apikeys/:id` `{ "servers": [0, 2] }` — сменить набор, не
перевыпуская ключ, и `DELETE /ui/apikeys/:id`. Подробно — `awg-ui/API.md`.

### Авторизация панели

```bash
TOKEN=$(curl -s -X POST http://HOST:PORT/login \
  -H "Content-Type: application/json" \
  -d '{"user":"admin","pass":"***"}' | jq -r .token)
```

| Метод | Ответ и ошибки |
|---|---|
| `POST /login` | `{ "token": "<JWT>" }`, срок 24 часа. `401` — неверная пара; `429` — больше 5 неудачных попыток с одного IP за 15 минут |
| `POST /logout` | `{ "ok": true }`, токен отзывается немедленно |

Дальше в каждый запрос: `Authorization: Bearer $TOKEN`. Ниже — роуты, которые
дёргает сама панель; в отличие от `/api/v1` их форма может меняться.

### Состояние

| Метод | Ответ |
|---|---|
| `GET /ui/brand` | `{ "brand": "Forgetting", "channel": "Beta", "version": "0.3.3" }` — **без авторизации**, панель берёт отсюда вордмарк на экране логина |
| `GET /health` | `{ "status", "server", "ip", "gen", "awg": { "status", "peers", "module", "tools" } }`. `200`, если интерфейс поднят, иначе `503` и `status: "degraded"` |
| `GET /awg/status` | `{ "up": true, "peers": 3, "publicKey": "…" }` |

`gen` — поколение AmneziaWG, на параметрах которого сейчас работает интерфейс:
`"2"` или `"3.1"`. Оно выводится из `awg1.conf`, а не хранится отдельно.

### Пользователи

| Метод | Что делает |
|---|---|
| `POST /api/users` | Создаёт пира. Тело `{ "name": "alice" }`, имя — `a–z A–Z 0–9 _ -`, до 32 символов. `201` с `{ name, ip, pub_key, psk_key, vpn_key, key_gen, vpn_key_prev }`; `400` — имя не прошло валидацию, `409` — уже есть |
| `GET /api/users` | Список: `{ users: [{ name, ip, pub_key, vpn_key, key_gen, online, lastHandshake }] }` |
| `GET /api/users/stats` | То же без ключей, но со счётчиками: `{ users: [{ name, ip, key_gen, online, lastHandshake, rx, tx }] }` |
| `POST /api/users/:name` | Возвращает пользователя целиком, включая `vpn_key`. Метод именно `POST`, чтобы ключ не оседал в логах и истории как параметр `GET`. `404` — не найден |
| `DELETE /api/users/:name` | `{ "success": true, "name": "alice" }` |
| `POST /api/users/reissue` | Пересобирает `vpn://` всем на текущих параметрах: `{ total, reissued, regenerated: [], backup }`. IP и ключевая пара сохраняются, клиентам нужно заново импортировать ключ; `backup` — путь к снимку базы, снятому перед проходом |

```bash
curl -s -X POST http://HOST:PORT/api/users \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"alice"}' | jq -r .vpn_key
```

Имея `vpn_key`, конфиг `.conf` можно получить локально: убрать префикс `vpn://`,
base64url-декодировать, отбросить первые 4 байта (длина), распаковать
`zlib inflate` → JSON; текст конфига лежит в `last_config.config`.

### Интерфейс AWG

| Метод | Что делает |
|---|---|
| `POST /awg/start` | Поднимает интерфейс, если он лежит; возвращает его статус |
| `POST /awg/restart` | `awg-quick down/up` + ресинк пиров, `{ "success": true }`. Соединения клиентов кратковременно рвутся |
| `POST /awg/upgrade` | Переводит сервер с 2.0 на 3.1: правит `awg1.conf`, перезапускает интерфейс и перевыпускает все ключи. `{ gen, backup, bumpedS: [], reissue: { total, reissued, … } }` |

`POST /awg/upgrade` отвечает `409`, если сервер уже на 3.1, модуль в ядре не
3.x или ядро старше 5.5, и `500` с текстом причины, если ядро не приняло
3.1-конфиг — в этом случае прежний конфиг возвращается из копии, а интерфейс
поднимается обратно на 2.0.

### Мастер-ключи

Те же операции, что в `/api/v1/masterkeys`, но без ограничения набором серверов.

| Метод | Что делает |
|---|---|
| `GET /ui/masterkeys` | `{ enabled, tls, keys: [{ id, uuid, label, device_limit, devices, servers, created_at, deleting? }] }`; `:id` в путях — id или `uuid` |
| `POST /ui/masterkeys` | `{ label, device_limit?, servers? }` → `201` |
| `PATCH /ui/masterkeys/:id` | `{ label?, device_limit?, servers? }` |
| `GET /ui/masterkeys/:id/link` | `{ link, tls }` — ссылка `sen://…` |
| `GET /ui/masterkeys/:id/devices` | Устройства со статусом и трафиком |
| `POST /ui/masterkeys/:id/rotate`, `/rekey` | Новая ссылка / смена ключей у всех устройств |
| `DELETE /ui/masterkeys/:id` | `{ success, id, pending }` |
| `DELETE /ui/devices/:id` | Отвязать устройство |
| `POST /ui/devices/:id/rekey`, `/psk` | Смена ключа / PSK у устройства |

## Управление

```bash
awg-ctrl 
```

Через systemd (автозапуск на загрузке):

```bash
systemctl start|stop|restart awg-control
```

## Лицензия
[MIT](LICENSE) © 2026 Ivan Vasilev
