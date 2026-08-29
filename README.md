# Forgetting
Панель управления и API для VPN на базе [AmneziaWG](https://github.com/amnezia-vpn/amneziawg-linux-kernel-module) 
> [!WARNING]
> Данный проект предназначен только для использования в законных целях.
## Архитектура

Три компонента, устанавливаются в `/opt/awg-control/`:

| Компонент | Роль |
|---|---|
| **awg-ctrl** | Привилегированный REST-бэкенд. Управляет пирами через `awg`/`awg-quick`, ведёт SQLite-базу, генерирует `vpn://` ключи. Слушает только loopback. |
| **awg-ui** | Express + React. Веб-панель, аутентификация (JWT), прокси к awg-ctrl. Единственный сервис, доступный по сети. |
| **cli** | Менеджер процессов: `start`/`stop`/`restart`/`status`, смена логина/пароля панели, интерактивное TUI-меню. |

Внутренняя связь awg-ui → awg-ctrl подписывается асимметрично (Ed25519): приватный ключ только у awg-ui, awg-ctrl проверяет подпись публичным.

## Требования
- **ОС**: Ubuntu 24.04+ (Тесты проходили на Ubuntu 24.04.4 LTS)
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

Отдельного публичного API с ключами нет: наружу смотрит только панель, и всё,
что она умеет, доступно по HTTP. Схема одна — логин отдаёт JWT на 24 часа,
дальше с ним дёргаются те же роуты, что и из браузера. Базовый URL — адрес
сервера и порт, выбранный при установке.

> [!WARNING]
> Панель отдаёт приватные поля пиров (`vpn_key`, `psk_key`) — это не публичный
> контракт. Не выставляй её порт в интернет без TLS и ограничения по адресам.

### Авторизация

```bash
TOKEN=$(curl -s -X POST http://HOST:PORT/login \
  -H "Content-Type: application/json" \
  -d '{"user":"admin","pass":"***"}' | jq -r .token)
```

| Метод | Ответ и ошибки |
|---|---|
| `POST /login` | `{ "token": "<JWT>" }`, срок 24 часа. `401` — неверная пара; `429` — больше 5 неудачных попыток с одного IP за 15 минут |
| `POST /logout` | `{ "ok": true }`, токен отзывается немедленно |

Дальше в каждый запрос: `Authorization: Bearer $TOKEN`.

### Состояние

| Метод | Ответ |
|---|---|
| `GET /ui/brand` | `{ "brand": "Forgetting", "channel": "Beta", "version": "0.2.0" }` — **без авторизации**, панель берёт отсюда вордмарк на экране логина |
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
