# php-bot — модератор и ассистент TG-чата «PHP-сообщество Воронеж»

Отдельный Go-проект (никак не связан с другими репозиториями). Один бинарник /
Docker-контейнер, long-polling Telegram Bot API, Postgres+pgvector для истории
и семантического поиска, LLM через VseLLM (OpenAI-compatible).

## Возможности

- **Модерация новичков**: при входе задаёт открытый вопрос, LLM-судья выносит
  вердикт (bot/human/unclear), бот постит рекомендацию с inline-кнопками
  `[Кикнуть] / [Оставить] / [Спросить ещё]`. **Кик — только после подтверждения
  админом**, автоматически бот не банит.
- **PHP-ассистент**: отвечает на `/ask`, @упоминание бота или reply на его
  сообщение. Контекст = RAG top-K по pgvector + последние N сообщений чата.
- **Темы для оживления**: cron 2 раза в день проверяет активность, если чат
  тихий — генерирует и постит тему. `/topic now` — вручную.
- **Поиск по истории**: `/search <запрос>` — top-K семантически близких сообщений.
- **Дайджест недели**: каждый понедельник 09:00, или `/digest week`.
- **Команды**: `/help`, `/stats`, `/check @user`, `/kick @user`.

## Стек

- Go 1.23, один бинарник
- `github.com/go-telegram/bot` — TG Bot API (long-polling, inline keyboards)
- PostgreSQL 16 + `pgvector` (HNSW, cosine) — образ `pgvector/pgvector:pg16`
- `github.com/robfig/cron/v3` — шедулер тем/дайджеста
- LLM: VseLLM (`google/gemini-2.5-flash`) + embeddings (`text-embedding-3-small`, 1536d)

---

# Инструкция: от нуля до работающего бота

## Шаг 1. Создать бота в Telegram

1. Открой в Telegram **@BotFather**.
2. Отправь `/newbot`.
3. Введи **display name** (любой, напр. «PHP Воронеж Модератор») — это видно в списке чатов.
4. Введи **@username** — уникальный ник бота, оканчивающийся на `bot`
   (напр. `php_voronezh_bot`). **Ник пригодится дальше** — им будут @упоминать.
5. BotFather пришлёт **HTTP API token** вида `1234567890:ABC-Def...`. Это и есть
   `PHPBOT_TG_TOKEN`. **Не публикуй его.**

Дополнительно (опционально, через `/setdescription`, `/setuserpic`, `/setabouttext`)
— оформи профиль бота, чтобы выглядел солидно в чате.

> @username бота **не нужно** прописывать в `.env` — бот сам узнаёт его через
> `getMe` при старте. Если позже сменишь ник — ничего перенастраивать не надо.

## Шаг 2. Узнать свой user_id и id чата

Эти id нужны для конфига. Узнать их проще всего через **@userinfobot** —
перешли ему любое сообщение, он ответит id.

- **Свой user_id** (и id других админов): перешли своё сообщение в @userinfobot →
  запиши число. Так для каждого админа. Итог — список через запятую в `PHPBOT_ADMIN_IDS`.
- **ID целевого чата**: добавь @userinfobot в чат → он сразу пришлёт id (с минусом,
  напр. `-1001234567890`). После — kick'ни его из чата. ID → `PHPBOT_CHAT_ID`.
  Если чатов несколько — через запятую.

## Шаг 3. Получить ключ VseLLM

1. Зарегистрируйся на `api.vsellm.ru`.
2. В личном кабинете создай API-ключ → `PHPBOT_LLM_API_KEY`.

## Шаг 4. Подготовить файл `.env`

```bash
cp .env.example .env
```

Заполни (минимум — первые 4 переменные, остальное можно оставить по умолчанию):

```dotenv
# --- обязательно ---
PHPBOT_TG_TOKEN=1234567890:ABC-Def_gHiJkLmNoPqRsTuVwXyZ
PHPBOT_ADMIN_IDS=11111111,22222222
PHPBOT_CHAT_ID=-1001234567890
PHPBOT_LLM_API_KEY=vsellm_xxx...

# --- LLM (дефолты подходят) ---
PHPBOT_LLM_URL=https://api.vsellm.ru/v1
PHPBOT_LLM_MODEL=google/gemini-2.5-flash
PHPBOT_EMBED_MODEL=text-embedding-3-small
PHPBOT_EMBED_DIM=1536

# --- поведение (дефолты подходят) ---
PHPBOT_QUIET_THRESHOLD=20
PHPBOT_TOPIC_CRON=0 12,20 * * *
PHPBOT_NEWCOMER_TIMEOUT=5m

# --- Docker-only (для docker-compose) ---
POSTGRES_USER=phpbot
POSTGRES_PASSWORD=смените_на_длинный_пароль
POSTGRES_DB=php_bot
TZ=Europe/Moscow
```

> ⚠️ `.env` уже в `.gitignore` — не попадёт в git. Никогда не коммить токены/пароли.

## Шаг 5. Деплой (два варианта)

### Вариант A — Docker Compose (рекомендуется, для сервера)

Требуется установленный Docker + Docker Compose plugin.

```bash
git clone <your-repo-url> php-bot
cd php-bot
cp .env.example .env
nano .env                # заполни по шагам 1–4 выше

docker compose build
docker compose up -d     # поднимет БД (pgvector) и бота
docker compose logs -f bot
```

Готово. Бот:
- сам создаст таблицы (embedded-миграции)
- сам подключится к TG через long-polling
- логи в `./logs/bot.log` и в `docker compose logs`

Обновление:

```bash
git pull
docker compose build
docker compose up -d
```

Резервная копия БД:

```bash
docker compose exec db pg_dump -U php_bot php_bot > backup.sql
```

### Вариант B — binary + systemd (если Docker не подходит)

Требуется локальный PostgreSQL 16 + расширение `pgvector`.

```sql
CREATE DATABASE php_bot;
\c php_bot
CREATE EXTENSION IF NOT EXISTS vector;
```

```bash
go build -o php-bot ./cmd/bot
```

`.env` можно не использовать — переменные подкладываются через systemd:

`/etc/systemd/system/php-bot.service`:

```ini
[Unit]
Description=PHP-Community Telegram Bot
After=network.target postgresql.service

[Service]
Type=simple
User=phpbot
WorkingDirectory=/srv/php-bot
EnvironmentFile=/srv/php-bot/.env
ExecStart=/srv/php-bot/php-bot
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now php-bot
sudo journalctl -u php-bot -f
```

## Шаг 6. Добавить бота в чат и сделать админом

**Это критично.** Без прав админа бот:
- не получит `new_chat_members` (модерация не сработает)
- не сможет кикать (кнопка «Кикнуть» вернёт ошибку)

1. В целевом чате: **«Добавить участника» → найди @username бота → добавь.**
2. Открой настройки чата → **«Управление группой» → «Администраторы» → «Добавить администратора»**.
3. Выбери бота. Дай права:
   - ✅ **Ban users** (обязательно — для кика)
   - ✅ **Delete messages** (опционально — для будущей анти-спам очистки)
   - остальные — по желанию
4. Сохрани.

> Ник → не нужен в конфиге. Бот зовёт `getMe` при старте и знает свой @username.

## Шаг 7. Smoke-тест

В чате, где бот админ:

| Что сделать | Что должно произойти |
|---|---|
| Написать `/help` | Бот пришлёт список команд |
| Написать `/ask что такое PSR-4?` | Ответ PHP-ассистента с контекстом |
| @упомянуть бота с вопросом | Ответ ассистента |
| Reply на сообщение бота | Ответ ассистента |
| Написать `/stats` | Статистика чата |
| Написать `/search di` | Top-K релевантных сообщений |
| Пригласить второй (тестовый) аккаунт | Бот поздоровается и задаст вопрос |
| Ответить тестовым аккаунтом | Бот вынесет вердикт + кнопки |
| (админом) `/topic now` | Сгенерирует и запостит тему |
| (админом) `/digest week` | Дайджест за неделю |
| (админом) `/check @username` | Вердикт судьи по последнему сообщению |

Смотреть логи в реальном времени: `docker compose logs -f bot` или
`sudo journalctl -u php-bot -f`.

---

# Команды бота

| Команда | Кто | Что |
|---|---|---|
| `/ask <текст>` | все | ответ на PHP/IT-вопрос |
| `/search <запрос>` | все | поиск по истории |
| `/stats` | все | статистика чата |
| `/help` | все | список команд |
| `/topic now` | admin | сгенерировать и запостить тему |
| `/digest week` | admin | дайджест за неделю |
| `/check @user` | admin | ручной прогон judge |
| `/kick @user` | admin | кик (обход авто-флоу) |

Также бот отвечает на **@упоминание** своего @username и на **reply** на своё
сообщение.

---

# Конфигурация

Все переменные — в `.env.example`. Ключевые:

| Переменная | По умолчанию | Описание |
|---|---|---|
| `PHPBOT_TG_TOKEN` | — | от @BotFather, обязательно |
| `PHPBOT_ADMIN_IDS` | — | tg user id админов через запятую |
| `PHPBOT_CHAT_ID` | — | id целевого чата(ов) через запятую |
| `PHPBOT_LLM_API_KEY` | — | ключ VseLLM, обязательно |
| `PHPBOT_DB_URL` | (docker) | `postgres://...`; в docker compose подставляется автоматически |
| `PHPBOT_LLM_MODEL` | `google/gemini-2.5-flash` | модель чата |
| `PHPBOT_EMBED_MODEL` | `text-embedding-3-small` | модель эмбеддингов |
| `PHPBOT_EMBED_DIM` | `1536` | **должно совпадать** с `vector(N)` в миграции |
| `PHPBOT_QUIET_THRESHOLD` | `20` | мин. сообщений за 24ч, иначе «тихо» |
| `PHPBOT_TOPIC_CRON` | `0 12,20 * * *` | cron проверки тишины |
| `PHPBOT_NEWCOMER_TIMEOUT` | `5m` | сколько ждать ответ новичка |
| `POSTGRES_USER/PASSWORD/DB` | `phpbot` | креды БД (только для docker compose) |
| `TZ` | `Europe/Moscow` | таймзона для cron и логов |

**Смена embedding-модели** (напр. на 768d): поменять `PHPBOT_EMBED_DIM`,
в `internal/db/migrations/000002_vector.up.sql` — `vector(768)`, дропнуть
таблицу `embeddings` и переиндексировать.

---

# Промпты

Системные промпты — `internal/prompts/*.txt`, встраиваются в бинарник через
`//go:embed`. Safety-секция — первой строкой в каждом промпте, неотключаемая.

| Файл | Назначение |
|---|---|
| `judge.txt` | судья модерации: критерии, few-shot, JSON-вывод |
| `chat.txt` | PHP-ассистент (русскоязычный, технологичный) |
| `topic.txt` | генерация тем для чата |
| `digest.txt` | суммаризация недели |
| `safety.txt` | общий safety-блок (вкладывается в другие) |

Правишь промпт → пересобираешь (`docker compose build && up -d`).

---

# Архитектура

```
cmd/bot/main.go              — точка входа, wiring, slog, graceful shutdown
internal/
  config/                    — env (PHPBOT_*)
  db/                        — connect + embedded миграции
  llm/                       — LLMClient (chat, без стриминга) + Embedder
  messages/                  — repository + async embedding worker (pgvector)
  users/                     — tracking статусов (member/suspect/banned)
  moderation/                — judge-промпт + flow диалога + callback → kick
  chat/                      — PHP-assistant: RAG + ответ
  topics/                    — генерация/выбор тем, cron, дайджест
  prompts/                   — встроенные .txt системные промпты
  tg/                        — обёртка над go-telegram/bot, диспетчер update
```

## Потоки данных

- **Сообщение**: `getUpdates` → `tg.Handler.OnMessage` → `messages.Save` (синх.)
  → `VectorRepo.Enqueue` (async embed в 2 worker-горутины).
- **Модерация**: `new_chat_members` → `moderation.OnNewMember` (вопрос + 5м таймер)
  → первый ответ новичка → `Judge` (LLM) → пост вердикта + inline-кнопки →
  callback от админа → `kickChatMember` (только admin).
- **PHP-ответ**: триггер (`/ask`, @, reply) → `chat.Answer`: embed query →
  `pgvector top-8` + `messages.Last(15)` → системный промпт + safety → LLM →
  post в чат.
- **Темы**: cron `0 12,20 * * *` → `messages.CountSince(24ч)` < порога →
  `topics.nextOrGenerate` (бесплатная из БД иначе LLM) → post.
- **Дайджест**: cron `0 9 * * 1` → все сообщения за 7 дней → LLM-суммаризация → post.

---

# Разработка

## Локальный запуск (без Docker)

```bash
# Подними локальный Postgres+pgvector (одноразово):
docker run -d --name php-bot-db -p 5433:5432 \
  -e POSTGRES_USER=phpbot -e POSTGRES_PASSWORD=phpbot -e POSTGRES_DB=php_bot \
  pgvector/pgvector:pg16

# Заполни .env, в PHPBOT_DB_URL укажи postgres://phpbot:phpbot@localhost:5433/php_bot?sslmode=disable

go run ./cmd/bot
```

## Тесты

```bash
go test ./internal/...
```

Покрытие: парсер вердикта судьи (`parseVerdict`) — устойчивость к markdown-обёрткам,
невалидному JSON, unknown verdict (fail-safe → `unclear`); `VerdictEmoji`.

End-to-end с LLM (judge на 10 примерах из плана) — это integration-тест,
требует `PHPBOT_LLM_API_KEY`. Запускается вручную против тестовой группы.

## Линтинг

```bash
go vet ./...
```

---

# Troubleshooting

| Симптом | Что проверить |
|---|---|
| Бот молчит в чате | `PHPBOT_CHAT_ID` верный? Бот админ чата? Токен верный? |
| `/kick` не работает | Боту дано право «Ban users» в настройках чата? |
| Модерация новичков не срабатывает | Бот — админ? `new_chat_members` приходит только админам |
| Ошибки `embeddings HTTP 401` | `PHPBOT_LLM_API_KEY` верный? |
| Ошибка `dimension mismatch` | `PHPBOT_EMBED_DIM` не совпадает с миграцией — дропни `embeddings` и мигрируй заново |
| Дайджест не приходит в пн | `TZ` неверный? cron считает по UTC, если не задать |
| Темы не постятся | `PHPBOT_QUIET_THRESHOLD` слишком низкий? Чат активнее порога |

## Логи

```bash
# Docker
docker compose logs -f bot
docker compose exec bot cat /app/logs/bot.log | tail -100

# systemd
sudo journalctl -u php-bot -f
tail -f /srv/php-bot/logs/bot.log
```

---

# Известные ограничения

- Long-polling (не webhook) — одного чата хватает, для масштаба — webhook.
- Старая история до запуска бота не импортируется (опционально через TG export JSON).
- Картинки/голосовые не индексируются — только текст/caption.
- `messages.id` — это TG message_id, уникальный в пределах чата; при нескольких
  чатах теоретически возможна коллизия (в планке — один чат, проблем нет).
