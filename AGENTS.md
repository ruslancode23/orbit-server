# AGENTS.md — orbit-server

Инструкции для AI-агентов (Claude Code, Codex, Cursor и др.) и для людей.

Orbit — открытое приложение с постоянными голосовыми комнатами для команд и сообществ.
Этот репозиторий — API-сервер. Web-клиент — отдельный репозиторий `orbit-web`.
Источник требований — PRD 3.1 (Orbit — PRD 3.0 в Claude Docs). Текущий этап — spike: голос и экран в браузере.

## Команды

```bash
cp .env.example .env
make up           # PostgreSQL (localhost:5433), SeaweedFS (S3), LiveKit в Docker
make migrate
make run          # API на :8080
make check        # golangci-lint + go test -race — запускай перед тем, как считать задачу готовой
make fmt          # gofumpt + goimports
make api          # после изменения api/openapi.yaml
make sqlc         # после изменения db/queries или миграций
make migrate-new name=create_users
make help         # все цели
```

Инструменты (sqlc, goose, golangci-lint) закреплены в `tools/go.mod` и вызываются через `go tool`.
Не ставь их глобально и не добавляй в основной `go.mod`.

## Стек (PRD 11.2)

Go 1.26, `net/http` + chi, OpenAPI + oapi-codegen (strict server) с валидацией kin-openapi,
`coder/websocket`, PostgreSQL через pgx + sqlc, миграции goose, slog,
LiveKit (SFU) для голоса и экрана, S3 (SeaweedFS локально, ADR-0003). Позже: river для фоновых задач.

Не добавляй: ORM (gorm, ent), Redis, брокеры сообщений, микросервисы, logrus/zap, testify/mock-фреймворки
без необходимости. Решение уровня стека фиксируй в `docs/adr/`.

## Структура — модульный монолит

```
cmd/orbit/            сборка зависимостей, запуск, graceful shutdown — без бизнес-логики
internal/config/      конфиг только из переменных окружения
internal/api/         сгенерировано из api/openapi.yaml (make api), не править
internal/platform/    инфраструктура: httpserver, postgres, logger
internal/<module>/    доменные модули: auth, servers, messages, voice, files, moderation
migrations/           SQL-миграции goose (только вперёд; Down обязателен)
db/queries/           SQL для sqlc → internal/platform/postgres/db (сгенерированный код не правь руками)
```

- Модуль владеет своими таблицами. Другой модуль обращается к ним через Go-интерфейс модуля, а не SQL.
- Зависимости передаются явно через конструкторы (`NewHandler(db)`), без глобального состояния и `init()`.
- Интерфейсы объявляет потребитель, а не реализация (`health.Pinger`).

## Обязательные правила

- **Права проверяются на сервере** в каждом обработчике и каждой подписке на события. Скрытая кнопка на
  клиенте — не защита. Изоляция данных между серверами (workspace) покрывается тестами.
- Секреты — только из переменных окружения. Новая переменная → `internal/config` + `.env.example` + README.
- Пароли — argon2id (`internal/auth`). Сессии — непрозрачные токены: клиенту уходит токен, в БД лежит только его
  SHA-256. Токен живёт в cookie `orbit_session` (HttpOnly, SameSite=Lax, Secure вне разработки) — её же браузер
  пришлёт при апгрейде WebSocket, чего заголовком не сделать. `auth.Middleware` кладёт сессию в контекст и никого
  не отклоняет; хендлер без сессии отвечает 401 сам, вне генерируемых хендлеров — `auth.RequireSession`.
- Не доверяй `X-Forwarded-For` без списка доверенных прокси (`chi/middleware.RealIP` не используем).
- SQL только параметризованный, через sqlc.
- Вложения (`internal/files`): загрузка идёт через сервер, тип определяется по содержимому
  (`http.DetectContentType`), html/svg/js не принимаем; скачивание — подписанной ссылкой из
  объектного хранилища (`internal/platform/objectstore`), не через сервер.
- Живые события — через `internal/events` (шина в процессе) и `internal/gateway` (WebSocket,
  `/api/v1/ws`, вне OpenAPI). Модуль публикует событие через свой узкий интерфейс и не знает о
  подписчиках; права проверяются при каждой подписке, а бан и кик закрывают поток сразу
  (`Hub.Revoke`). Медленный клиент отключается с кодом `behind` и догоняет историю по курсору.

## Код

- Ошибки оборачивай с контекстом: `fmt.Errorf("create server: %w", err)`. Сравнение — `errors.Is/As`.
- Первый аргумент функций с I/O — `ctx context.Context`. HTTP-клиенты и запросы — с контекстом.
- Логи — `slog` со структурными полями, без `fmt.Println`. Не логируй токены, пароли, тела сообщений.
- API описывается в `api/openapi.yaml` до кода: правишь спецификацию → `make api` → реализуешь метод
  `api.StrictServerInterface` в своём модуле. Не пиши роуты и JSON-декодирование вручную, не правь `*.gen.go`.
- Ошибки — в едином формате `{"error":{"code","message"}}` (`httpx`, схема `Error`). Валидация входа описывается
  в схеме; в домене остаются только правила, которые схема выразить не может.

## Тесты

- Юнит-тесты рядом с кодом, table-driven, стандартный `testing`.
- Интеграционные — с настоящим PostgreSQL в контейнере, не мокай базу.
- `go test -race ./...` должен проходить. Тесты не зависят от `.env` разработчика.

## Git

- Задачи — в Jira, проект SCRUM (эпик SCRUM-5). Ветки: `feat/…`, `fix/…`, `chore/…`, ключ задачи Jira в имени: `feat/SCRUM-7-voice-token`.
- Сообщения коммитов — на английском, в повелительном наклонении: `Add LiveKit token endpoint`.
- Заголовок и описание PR — на русском: что сделано, зачем, как проверено, что не покрыто.
- Не коммить `.env` и секреты. Не делай push и не создавай PR без просьбы.
