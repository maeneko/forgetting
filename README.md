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

### Свой источник и своё имя

В коде не зашиты ни репозиторий, ни бренд — всё задаётся окружением до запуска:

| Переменная | По умолчанию | Зачем |
|---|---|---|
| `REPO_BASE` | `https://git.ma7neko.ru/maeneko/forgetting` | основной источник архива релиза |
| `REPO_FALLBACK` | `https://github.com/maeneko/forgetting` | запасной, если в основном релиза ещё нет |
| `ARCHIVE_URL` | — | полный URL архива, перекрывает оба варианта |
| `BRAND` | из `.env` | имя продукта в баннерах, systemd-юните и панели |

Путь релиза у Gitea и GitHub одинаковый (`<repo>/releases/download/v<версия>/awgcontrol-<версия>.tar.gz`),
так что подходит любой из них. Если архив не найден в основном источнике,
установщик предупреждает и берёт его из запасного.

Архив собирается по тегу `v*`: на GitHub — `.github/workflows/release.yml`, в
Gitea — `.gitea/workflows/release.yml` (сборка та же, публикация через API
Gitea). Для запуска в Gitea репозиторий не должен быть pull-зеркалом, Actions
включены, а runner зарегистрирован с меткой `ubuntu-latest`.

```bash
REPO_BASE=https://git.example.org/me/vpn BRAND="My VPN" \
  bash <(curl -Ls https://git.example.org/me/vpn/raw/branch/main/install.sh)
```

Имя панели, канал и версия лежат в `.env` в корне проекта (едет в архиве
релиза, на сервере — `/opt/awg-control/.env`):

```env
BRAND=Forgetting
CHANNEL=Beta
VERSION=0.2.0
```

Оттуда их читают CLI (баннер меню) и панель (вордмарк, `GET /ui/brand`).
`BRAND=` при установке перекрывает значение из файла.

Сама панель и её API к домену не привязаны: работают по адресу того сервера,
куда установлены, внешних сервисов не требуют.
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
