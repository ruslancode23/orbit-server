# orbit-server

API-сервер Orbit: модульный монолит на Go. Данные — PostgreSQL, файлы — S3 (SeaweedFS), голос и экран — LiveKit.

## Запуск

Нужны Go 1.26+ и Docker.

```bash
cp .env.example .env
make up        # PostgreSQL, SeaweedFS (S3), LiveKit
make migrate
make run       # API на http://localhost:8080
```

Проверка: `curl localhost:8080/readyz` → `{"database":"up","status":"ok"}`.

## Контракт API

[`api/openapi.yaml`](api/openapi.yaml) — источник правды. Из него `make api` генерирует сервер
(`internal/api/api.gen.go`, oapi-codegen strict server), а orbit-web — клиент, типы и функции для TanStack Query.
Каждый запрос проверяется по схеме до хендлера: неверное тело, лишние поля и неизвестные пути получают ответ
в едином формате ошибки. Порядок изменения API: правишь `openapi.yaml` → `make api` → реализуешь метод
сгенерированного интерфейса. CI падает, если сгенерированный код не совпадает со спецификацией.

## API для разработки

В `ORBIT_ENV=development` включён `POST /api/v1/voice/token` — токен LiveKit без авторизации, для spike:

```bash
curl -X POST localhost:8080/api/v1/voice/token -H 'Content-Type: application/json' \
  -d '{"room":"dev-room","displayName":"Anna"}'
# {"token":"eyJ…","url":"ws://localhost:7880","identity":"guest-…","expiresAt":"…"}
```

Токен даёт вход в одну комнату на 10 минут. В production маршрута нет (404). Ошибки API — в едином
формате `{"error":{"code":"invalid_request","message":"…"}}`.

## Команды

`make help` выводит все цели. Основные: `make test`, `make lint`, `make fmt`, `make sqlc`,
`make migrate-new name=create_users`, `make check` (то же, что CI).

Инструменты закреплены в `tools/go.mod` и запускаются через `go tool` — ставить их отдельно не нужно ([ADR-0002](docs/adr/0002-dev-tools-module.md)).

## Структура

```
cmd/orbit/              точка входа: конфиг, зависимости, graceful shutdown
internal/
  config/               конфигурация из переменных окружения
  health/               /healthz и /readyz
  platform/             инфраструктура: httpserver, postgres, logger
  <модуль>/             доменные модули: auth, servers, messages, voice, files, moderation
migrations/             SQL-миграции goose
db/queries/             SQL-запросы для sqlc → internal/platform/postgres/db
deploy/                 docker-compose для локальной разработки
docs/adr/               архитектурные решения
```

Правила (PRD 18.2): права проверяются на сервере в каждом обработчике и подписке; без Redis, брокеров
и микросервисов до появления нагрузки; секреты только через переменные окружения.

Аккаунты и сессии — `internal/auth`: пароли argon2id, в БД лежит только SHA-256 токена сессии, сам токен —
в cookie `orbit_session` (HttpOnly, Secure вне разработки). Время жизни сессии — `ORBIT_SESSION_TTL`
(по умолчанию 720h). Полный список переменных — в `.env.example`.

## Файлы

Вложение загружается через сервер (`POST /api/v1/channels/{id}/attachments`, multipart, поле `file`):
он проверяет размер и определяет тип по первым байтам, а не по имени. Скачивание — по подписанной
ссылке прямо из хранилища (`GET /api/v1/attachments/{id}`), она живёт `ORBIT_ATTACHMENT_LINK_TTL`.
Файл, чьё сообщение так и не пришло, удаляет фоновая уборка через `ORBIT_ORPHAN_ATTACHMENT_AGE`.
Лимит размера — `ORBIT_MAX_UPLOAD_BYTES`.

## Живой поток событий

`GET /api/v1/ws` — WebSocket, вне OpenAPI (спецификация описывает запросы и ответы, а не поток).
Сессия берётся из cookie `orbit_session`, без неё — 401. Кадры — JSON.

От клиента: `{"type":"subscribe","workspaceId":"…","cursors":{"<channelId>":<seq>}}` (курсоры
необязательны — это то, на чём клиент остановился), `{"type":"unsubscribe"}`, `{"type":"ping"}`.

От сервера: `subscribed`, `unsubscribed`, `pong`, события `message.created`, `message.updated`,
`message.deleted`, `message.reaction`, `member.changed` (поля `workspaceId`, `channelId`, `seq`,
`payload`) и `error` с кодом. После `subscribe` сервер сначала досылает пропущенное по курсорам,
потом включает живой поток и не повторяет то, что уже дослал.

Коды ошибок потока: `not_found` — нет доступа к серверу; `forbidden` — доступ пропал уже во время
подключения (бан или кик), поток закрыт; `behind` — клиент не успевал читать, нужно переподключиться
и догнать по курсору; `invalid_request` — непонятный кадр.

## Локальная инфраструктура

| Сервис | Адрес | Доступ |
| --- | --- | --- |
| PostgreSQL | localhost:5433 (`ORBIT_POSTGRES_PORT`) | orbit / orbit |
| SeaweedFS S3 / master UI | localhost:8333 / localhost:9333 | orbit / orbit-dev-secret ([ADR-0003](docs/adr/0003-seaweedfs-instead-of-minio.md)) |
| LiveKit | ws://localhost:7880 | devkey / secret (dev mode), настройки поверх — `deploy/livekit.yaml` |

Все значения — только для локальной разработки.
