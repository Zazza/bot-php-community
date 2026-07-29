# --- build stage ---
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Кеш модулей: сначала только go.mod/go.sum, затем исходники.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGOFF для статического бинарника (чистый Go, no cgo).
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/php-bot ./cmd/bot

# --- runtime stage ---
FROM alpine:3.20

# ca-certificates — для HTTPS-запросов к Telegram API и VseLLM.
# tzdata — корректные таймзоны в логах (cron, дайджесты).
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S phpbot && adduser -S -G phpbot phpbot

# Копируем бинарник и директорию под логи.
COPY --from=builder /out/php-bot /app/php-bot
RUN mkdir -p /app/logs && chown -R phpbot:phpbot /app

USER phpbot
WORKDIR /app

# По умолчанию лог в /app/logs/bot.log (переопределяется через env).
ENV PHPBOT_LOG_PATH=/app/logs/bot.log

ENTRYPOINT ["/app/php-bot"]
