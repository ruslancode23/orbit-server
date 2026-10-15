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

## Локальная инфраструктура

| Сервис | Адрес | Доступ |
| --- | --- | --- |
| PostgreSQL | localhost:5433 (`ORBIT_POSTGRES_PORT`) | orbit / orbit |
| SeaweedFS S3 / master UI | localhost:8333 / localhost:9333 | orbit / orbit-dev-secret ([ADR-0003](docs/adr/0003-seaweedfs-instead-of-minio.md)) |
| LiveKit | ws://localhost:7880 | devkey / secret (dev mode) |

Все значения — только для локальной разработки.
