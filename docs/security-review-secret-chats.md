# Security-ревью секретных чатов

Дата: 2026-10-20 · Задача: E2E-6 (SCRUM-82) · Модель угроз:
[threat-model-secret-chats.md](threat-model-secret-chats.md), решение о протоколе:
[ADR-0005](adr/0005-secret-chats-olm.md).

## Что проверено

| Проверка | Где закреплено | Чем подтверждено |
| --- | --- | --- |
| Сервер хранит только публичные ключи, привязку устройств и шифротекст конвертов | `migrations/00012_devices_and_secret_chats.sql`, `internal/secrets` | `internal/secrets/service_test.go`, схема БД |
| Секретный канал отвергает текстовые сообщения и курсоры прочтения | `internal/messages/service.go` (`KindSecret` → 404), `internal/reads/service.go` | `TestSecretChannelRefusesMessages`, тесты reads |
| One-time key выдаётся ровно один раз, атомарно | `ClaimOneTimeKey` (`DELETE … RETURNING`) | `TestOneTimeKeyIsHandedOutOnce` |
| Регистрация устройства требует подписи ed25519 владельца ключа | `internal/secrets/service.go` (`registrationMessage`, `ed25519.Verify`) | `TestRegisterDeviceChecksTheSignature` |
| Чужое устройство и чужой получатель конверта не проходят | `requireOwnedDevice`, `SendEnvelope` | `TestStartSecretChatRefusesSomebodyElsesDevice`, `TestEnvelopeValidation` |
| Конверт живёт не дольше недели, подтверждение удаляет его сразу | `MaxEnvelopeTTL`, `AcknowledgeEnvelope`, `secrets.Sweeper` | `TestExpiredEnvelopesAreSwept`, e2e |
| Файл секретного чата — шифротекст с дедлайном, недоступен постороннему | `internal/files` (`channel_id`, `expires_at`, `Link`) | `TestSecretChatFileIsSharedWithThePeerAndExpires` |
| На проводе нет открытого текста | клиент шифрует до отправки | `session.test.ts` (тело конверта и blob не содержат plaintext), `e2e/secret-chat.spec.ts` (перехват POST `/envelopes`) |
| В `localStorage` нет ключей и сообщений | `PlatformAdapter.secretStore` (IndexedDB), pickles под AES-GCM | `session.test.ts` (`localStorage.length === 0`) |
| Смена ключа сбрасывает сверку и блокирует отправку | `SecretSession.loadState`, `SecretChatPage` | `session.test.ts`, `SecretChatPage.test.tsx` |
| Уведомление не содержит текста | `useSecretNotifications` | `useSecretNotifications.test.tsx` |
| CSP и SRI включены в сборке | `scripts/security-html.ts`, `vite.config.ts` | `tests/security-html.test.ts` |

## Найденные ограничения (заведены как известные, не блокеры релиза)

- Любой аутентифицированный аккаунт может вычерпать one-time keys чужого устройства запросами
  `GET /devices/{id}/key-bundle`. Нужен rate limiting/anti-abuse — отдельная задача.
- Серверный TTL конверта и вложения — до 7 дней, даже если таймер сообщения 5 секунд. Это шифротекст,
  но метаданные живут дольше; в UI и ADR это описано честно.
- Ключ шифрования pickles лежит в том же IndexedDB, что и pickles. Он неэкспортируемый `CryptoKey` и
  защищает от случайного чтения JSON, но не от скрипта в origin. Tauri закроет это keychain'ом.
- Ответы и правка в секретном чате не реализованы в этой версии; реакции — вне MVP. Это указано в ADR
  и в задаче E2E-4 как не покрытое.
- CSP в dev-режиме не включается (Vite HMR требует inline-скриптов); проверяйте `pnpm build` и
  `pnpm test:e2e:pwa`.

## Решение по внешнему аудиту

Внешний аудит в рамках эпика не заказывался: бюджет и сроки эпика его не включали. Проведено внутреннее
ревью по чек-листу выше и тестами. Рекомендация: до публичного запуска секретных чатов заказать внешний
аудит Olm-интеграции и модели устройств; до этого держать функцию за флагом или пометить как
экспериментальную. Решение зафиксировано комментарием в SCRUM-82.
