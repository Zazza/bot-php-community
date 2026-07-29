// import-history — утилита разового импорта истории чата (экспорт Telegram Desktop JSON)
// в БД бота: таблица messages + embeddings (чтобы /digest и /search работали по реальной истории).
//
// Запуск (внутри контейнера бота, где есть PHPBOT_* env и доступ к db):
//
//	import-history -file /tmp/messages.json [-chat-id <id>] [-workers 4]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jmoiron/sqlx"

	"phpbot/internal/db"
	"phpbot/internal/importer"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
)

func main() {
	filePath := flag.String("file", "", "путь к messages.json (экспорт Telegram Desktop)")
	chatID := flag.Int64("chat-id", envChatID("PHPBOT_CHAT_ID", 0), "id целевого чата (или PHPBOT_CHAT_ID)")
	workers := flag.Int("workers", 4, "число параллельных embedding-воркеров")
	flag.Parse()

	if *filePath == "" || *chatID == 0 {
		fmt.Fprintln(os.Stderr, "использование: import-history -file <messages.json> [-chat-id N] [-workers 4]")
		os.Exit(2)
	}

	ctx := context.Background()

	data, err := os.ReadFile(*filePath)
	if err != nil {
		die("read file: %w", err)
	}
	msgs, err := importer.Parse(data, *chatID)
	if err != nil {
		die("parse: %w", err)
	}
	slog.Info("parsed from file", "messages", len(msgs), "chat_id", *chatID)
	if len(msgs) == 0 {
		fmt.Println("Нет текстовых сообщений для импорта (проверь формат файла).")
		return
	}

	dbURL := os.Getenv("PHPBOT_DB_URL")
	if dbURL == "" {
		die("PHPBOT_DB_URL не задан")
	}
	dbx, err := db.Connect(ctx, dbURL)
	if err != nil {
		die("db connect: %w", err)
	}
	defer dbx.Close()

	repo := messages.New(dbx)

	before := countMsgs(ctx, dbx, *chatID)
	for _, m := range msgs {
		mm := m // избегаем захвата loop-переменной
		if err := repo.Save(ctx, &mm); err != nil {
			slog.Warn("save message", "id", m.ID, "err", err)
		}
	}
	after := countMsgs(ctx, dbx, *chatID)
	slog.Info("messages in db", "before", before, "after", after, "added", after-before)

	embedder := llm.NewEmbedder(
		envOr("PHPBOT_LLM_URL", "https://api.vsellm.ru/v1"),
		os.Getenv("PHPBOT_LLM_API_KEY"),
		envOr("PHPBOT_EMBED_MODEL", "text-embedding-3-small"),
	)
	vec := messages.NewVectorRepo(dbx, embedder, 0)

	ids := needingEmbed(ctx, dbx, *chatID)
	slog.Info("embedding missing", "pending", len(ids), "workers", *workers)
	done, failed := runEmbed(ctx, vec, ids, *workers)

	fmt.Printf("Готово: в файле %d сообщений, в БД теперь %d (+%d новых), векторизовано %d (ошибок %d).\n",
		len(msgs), after, after-before, done, failed)
}

// runEmbed параллельно векторизует message-ids через VectorRepo.EmbedAndSave.
func runEmbed(ctx context.Context, vec *messages.VectorRepo, ids []int64, workers int) (done, failed int64) {
	if workers < 1 {
		workers = 1
	}
	ch := make(chan int64)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				if err := vec.EmbedAndSave(ctx, id); err != nil {
					slog.Warn("embed", "id", id, "err", err)
					atomic.AddInt64(&failed, 1)
					continue
				}
				atomic.AddInt64(&done, 1)
			}
		}()
	}
	for _, id := range ids {
		ch <- id
	}
	close(ch)
	wg.Wait()
	return done, failed
}

func countMsgs(ctx context.Context, dbx *sqlx.DB, chatID int64) int64 {
	var n int64
	_ = dbx.GetContext(ctx, &n, `SELECT count(*) FROM messages WHERE chat_id = $1`, chatID)
	return n
}

// needingEmbed — id текстовых сообщений чата без embeddings (только недостающее → идемпотентно).
func needingEmbed(ctx context.Context, dbx *sqlx.DB, chatID int64) []int64 {
	var ids []int64
	if err := dbx.SelectContext(ctx, &ids, `
		SELECT m.id FROM messages m
		WHERE m.chat_id = $1 AND m.text <> ''
		  AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.message_id = m.id)
		ORDER BY m.id
	`, chatID); err != nil {
		slog.Error("select unembedded", "err", err)
	}
	return ids
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envChatID читает первый id из PHPBOT_CHAT_ID (может быть списком через запятую).
func envChatID(k string, def int64) int64 {
	v := strings.TrimSpace(strings.Split(os.Getenv(k), ",")[0])
	if v == "" {
		return def
	}
	var n int64
	fmt.Sscanf(v, "%d", &n)
	if n == 0 {
		return def
	}
	return n
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "import-history: "+format+"\n", args...)
	os.Exit(1)
}
