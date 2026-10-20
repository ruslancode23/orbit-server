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

## Голосовые каналы

Канал с `kind: voice` — постоянная комната LiveKit: имя комнаты — id канала, а identity
участника — id аккаунта, поэтому вторая вкладка того же человека заменяет первую.
`POST /api/v1/channels/{channelId}/voice-token` выдаёт токен на 10 минут по сессии:

```bash
curl -b cookies.txt -X POST localhost:8080/api/v1/channels/$CHANNEL_ID/voice-token
# {"token":"eyJ…","url":"ws://localhost:7880","identity":"<account id>","expiresAt":"…"}
```

Войти может любой участник сервера. Право публиковать микрофон и экран (`canPublish`) следует
из роли: в обычном голосовом канале говорят все, в read-only — только админы и владелец;
остальные получают токен с `canPublish: false` и могут слушать. Токен на текстовый канал,
чужой канал или канал сервера, где вы не состоите, — 404. Ошибки API — в едином формате
`{"error":{"code":"invalid_request","message":"…"}}`.

Кто сидит в голосовых каналах, сервер держит в памяти по вебхукам LiveKit
(`POST /api/v1/livekit/webhook`, подпись проверяется тем же API-секретом). Список отдаётся
в `GET /api/v1/workspaces/{id}` полем `voice`, а изменения приходят событием `voice.presence`
по WebSocket. После перезапуска состояние восстанавливается из `ListParticipants`, когда
сервер впервые спрашивают; вебхуки только ускоряют обновление. Подпись и формат событий —
`deploy/livekit.yaml` и `internal/voice/webhook.go`.

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

Иконка сервера — `PUT /api/v1/workspaces/{id}/icon` (multipart, поле `file`; только PNG, JPEG или WebP
по содержимому), удаление — `DELETE` там же; меняют admin и owner. Участники видят её по подписанной
ссылке `iconUrl` в ответах о сервере, она живёт `ORBIT_ICON_LINK_TTL`. Лимит — `ORBIT_MAX_ICON_BYTES`.

## Права участников

Сервер сам решает, что можно роли member (`memberPermissions` в схеме `Workspace`): `canInvite` —
создавать ссылки-приглашения, `canCreateChannels` — создавать каналы (не read-only), `canAttachFiles` —
прикреплять файлы. По умолчанию `false`, `false`, `true` — как было до флагов. Owner и admin могут всё
всегда. Флаги задаются при создании сервера и через `PATCH /api/v1/workspaces/{id}` (admin или owner);
права читаются на каждый запрос, поэтому смена флага действует сразу, в том числе для уже подключённых
клиентов, а событие `workspace.updated` подсказывает им перерисовать кнопки.

## Живой поток событий

`GET /api/v1/ws` — WebSocket, вне OpenAPI (спецификация описывает запросы и ответы, а не поток).
Сессия берётся из cookie `orbit_session`, без неё — 401. Кадры — JSON.

От клиента: `{"type":"subscribe","workspaceId":"…","cursors":{"<channelId>":<seq>}}` (курсоры
необязательны — это то, на чём клиент остановился), `{"type":"resume","conversations":{"<conversationId>":<seq>}}`
для личных бесед (у них нет сервера), `{"type":"unsubscribe"}`, `{"type":"ping"}`.

У соединения два потока: поток выбранного сервера и личный поток аккаунта. Второй открывается сразу
после авторизации и приносит `conversation.created`, события сообщений и прочтения в беседах, а также
`secret.envelope` — непрозрачный конверт секретного чата. Права на событие беседы проверяются по её
участникам, поэтому вышедший из группы перестаёт получать их сразу.

От сервера: `subscribed`, `unsubscribed`, `pong`, события `message.created`, `message.updated`,
`message.deleted`, `message.reaction`, `member.changed`, `workspace.updated` (поля `workspaceId`, `channelId`, `seq`,
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

## Секретные чаты

Секретный чат — это `channels.kind = 'secret'` с двумя участниками и привязкой к конкретным устройствам
(`secret_chat_devices`). Сервер хранит публичные ключи устройств (`devices`), одноразовые pre-keys
(`device_one_time_keys`, выдача — `DELETE … RETURNING`) и шифротекст конвертов (`secret_envelopes`).
Текста, ключей и имён файлов у сервера нет; обычные эндпоинты сообщений секретный канал отвергают 404.

Устройство регистрируется подписью ed25519 над `orbit-device-v1|<userId>|<identityKey>|<signingKey>`,
дальше секретные операции требуют заголовок `X-Orbit-Device`. Конверт живёт до недели и удаляется
подтверждением получателя; вложения секретного чата — шифротекст с `expiresAt`, который убирает sweeper.

Подробности: [ADR-0005](docs/adr/0005-secret-chats-olm.md) и
[модель угроз](docs/threat-model-secret-chats.md). Security-ревью —
[docs/security-review-secret-chats.md](docs/security-review-secret-chats.md).
