# Forgetting
Панель управления и API для VPN на базе [AmneziaWG](https://github.com/amnezia-vpn/amneziawg-linux-kernel-module) 
> [!WARNING]
> Данный проект предназначен только для использования в законных целях.
## Архитектура

Три компонента, устанавливаются в `/opt/awg-control/`:

| Компонент | Роль |
|---|---|
| **awg-ctrl** | Привилегированный REST-бэкенд. Управляет пирами через `awg`/`awg-quick`, ведёт SQLite-базу, генерирует `vpn://` ключи. Слушает только loopback. |
| **awg-ui** | Express + React. Веб-панель, аутентификация (JWT), внешний API (`/api/v1`), прокси к awg-ctrl. Единственный сервис, доступный по сети. |
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
sudo bash <(curl -Ls https://raw.githubusercontent.com/maeneko/forgetting/main/install.sh)
```
## Управление

```bash
awg-ctrl 
```

Через systemd (автозапуск на загрузке):

```bash
systemctl start|stop|restart awg-control
```

## 1. Публичный API (`/api/v1`)

Авторизация по API-ключу. Ответы содержат только публичные поля — приватные
ключи и PSK наружу не отдаются.

### Авторизация

Каждый запрос обязан содержать заголовок:

```
X-Api-Key: awgk_<...>
```

Ключ создаётся в панели (вкладка «API-ключи») или через `POST /ui/apikeys`
(раздел 3). Открытое значение ключа показывается **один раз** при создании —
сохрани его сразу, дальше доступен только хэш.

Ответы при проблемах с ключом:

| Код | Когда |
|---|---|
| `401 {"error":"API key required"}` | заголовок отсутствует или не начинается с `awgk_` |
| `401 {"error":"Invalid API key"}` | ключ не найден (удалён/неверный) |

> Ключ привязан к серверу (`server_id`). Сейчас сервер один — поле всегда `0`

### `POST /api/v1/users` — создать пользователя

Создаёт VPN-пира и возвращает готовый `vpn://` ключ.

**Тело:** `{ "name": "<имя>" }`
Имя: `a–z A–Z 0–9 _ -`, 1–32 символа.

**`201 Created`:**
```json
{
  "name": "alice",
  "ip": "10.9.0.2",
  "vpn_key": "vpn://AAAI..."
}
```
| Код ошибки | Причина |
|---|---|
| `400` | имя не прошло валидацию |
| `409` | пользователь с таким именем уже есть |
```bash
curl -X POST http://HOST:PORT/api/v1/users \
  -H "X-Api-Key: awgk_xxx" \
  -H "Content-Type: application/json" \
  -d '{"name":"alice"}'
```
### `GET /api/v1/users` — список пользователей
Пиры с трафиком и статусом онлайн.
**`200 OK`:**
```json
{
  "users": [
    { "name": "alice", "ip": "10.9.0.2", "online": true,  "lastHandshake": 1718800000, "rx": 142400000, "tx": 9800000 },
    { "name": "bob",   "ip": "10.9.0.3", "online": false, "lastHandshake": 0,          "rx": 0,         "tx": 0 }
  ]
}
```
- `lastHandshake` — unix-секунды последнего рукопожатия (`0` — не было).
- `rx`/`tx` — байты принято/отдано.

```bash
curl http://HOST:PORT/api/v1/users -H "X-Api-Key: awgk_xxx"
```

### `GET /api/v1/users/:name` — получить ключ пользователя

**`200 OK`:**
```json
{ "name": "alice", "ip": "10.9.0.2", "vpn_key": "vpn://AAAI..." }
```

| Код ошибки | Причина |
|---|---|
| `404` | пользователь не найден |

```bash
curl http://HOST:PORT/api/v1/users/alice -H "X-Api-Key: awgk_xxx"
```

> Имея `vpn_key`, конфиг `.conf` можно получить локально: убрать префикс
> `vpn://`, base64url-декодировать, отбросить первые 4 байта (длина), распаковать
> `zlib inflate` → JSON; текст конфига лежит в `last_config.config`.

### `DELETE /api/v1/users/:name` — удалить пользователя

**`200 OK`:**
```json
{ "success": true, "name": "alice" }
```

```bash
curl -X DELETE http://HOST:PORT/api/v1/users/alice -H "X-Api-Key: awgk_xxx"
```
## Лицензия
[MIT](LICENSE) © 2026 Ivan Vasilev
