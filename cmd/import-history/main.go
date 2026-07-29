// import-history — утилита разового импорта истории чата (экспорт Telegram Desktop JSON)
// в БД бота: таблица messages + embeddings (чтобы /digest и /search работали по реальной истории).
//
// Запуск (внутри контейнера бота, где есть PHPBOT_* env и доступ к db):
//
//	import-history -file /tmp/result.json [-chat-id <id>] [-batch 32]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/pgvector/pgvector-go"

	"phpbot/internal/db"
	"phpbot/internal/importer"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
)

func main() {
	filePath := flag.String("file", "", "путь к result.json (экспорт Telegram Desktop)")
	chatID := flag.Int64("chat-id", envChatID("PHPBOT_CHAT_ID", 0), "id целевого чата (или PHPBOT_CHAT_ID)")
	batch := flag.Int("batch", 32, "размер батча для embedding (текстов за один API-вызов)")
	flag.Parse()

	if *filePath == "" || *chatID == 0 {
		fmt.Fprintln(os.Stderr, "использование: import-history -file <result.json> [-chat-id N] [-batch 32]")
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
	for i := range msgs {
		if err := repo.Save(ctx, &msgs[i]); err != nil {
			slog.Warn("save message", "id", msgs[i].ID, "err", err)
		}
	}
	after := countMsgs(ctx, dbx, *chatID)
	slog.Info("messages in db", "before", before, "after", after, "added", after-before)

	embedder := llm.NewEmbedder(
		envOr("PHPBOT_LLM_URL", "https://api.vsellm.ru/v1"),
		os.Getenv("PHPBOT_LLM_API_KEY"),
		envOr("PHPBOT_EMBED_MODEL", "text-embedding-3-small"),
	)
	rows := needingEmbedRows(ctx, dbx, *chatID)
	slog.Info("embedding missing", "pending", len(rows), "batch", *batch)
	done, failed := embedBatched(ctx, dbx, embedder, rows, *batch)

	fmt.Printf("Готово: в файле %d, в БД теперь %d (+%d новых), векторизовано %d (ошибок %d).\n",
		len(msgs), after, after-before, done, failed)
}

type msgRow struct {
	ID   int64  `db:"id"`
	Text string `db:"text"`
}

// embedBatched векторизует батчами через Embedder.EmbedBatch и льёт bulk-INSERT'ом.
func embedBatched(ctx context.Context, dbx *sqlx.DB, e *llm.Embedder, rows []msgRow, size int) (done, failed int64) {
	if size < 1 {
		size = 1
	}
	model := e.Model()
	for i := 0; i < len(rows); i += size {
		j := i + size
		if j > len(rows) {
			j = len(rows)
		}
		chunk := rows[i:j]
		texts := make([]string, len(chunk))
		for k, r := range chunk {
			texts[k] = r.Text
		}
		vecs, err := e.EmbedBatch(ctx, texts)
		if err != nil {
			slog.Warn("embed batch", "from_id", chunk[0].ID, "size", len(chunk), "err", err)
			failed += int64(len(chunk))
			continue
		}
		var b strings.Builder
		b.WriteString("INSERT INTO embeddings (message_id, embedding, model) VALUES ")
		args := make([]any, 0, len(chunk)*3)
		for k, r := range chunk {
			if k > 0 {
				b.WriteByte(',')
			}
			p := k*3 + 1
			fmt.Fprintf(&b, "($%d,$%d,$%d)", p, p+1, p+2)
			args = append(args, r.ID, pgvector.NewVector(vecs[k]), model)
		}
		b.WriteString(" ON CONFLICT (message_id) DO UPDATE SET embedding=EXCLUDED.embedding, model=EXCLUDED.model")
		if _, err := dbx.ExecContext(ctx, b.String(), args...); err != nil {
			slog.Warn("insert embeddings batch", "from_id", chunk[0].ID, "err", err)
			failed += int64(len(chunk))
			continue
		}
		done += int64(len(chunk))
		if (i/size)%25 == 0 {
			slog.Info("embedding progress", "done", done, "failed", failed, "of", len(rows))
		}
	}
	return done, failed
}

func countMsgs(ctx context.Context, dbx *sqlx.DB, chatID int64) int64 {
	var n int64
	_ = dbx.GetContext(ctx, &n, `SELECT count(*) FROM messages WHERE chat_id = $1`, chatID)
	return n
}

// needingEmbedRows — (id,text) текстовых сообщений чата без embeddings (идемпотентно).
func needingEmbedRows(ctx context.Context, dbx *sqlx.DB, chatID int64) []msgRow {
	var rows []msgRow
	if err := dbx.SelectContext(ctx, &rows, `
		SELECT m.id, m.text FROM messages m
		WHERE m.chat_id = $1 AND m.text <> ''
		  AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.message_id = m.id)
		ORDER BY m.id
	`, chatID); err != nil {
		slog.Error("select unembedded", "err", err)
	}
	return rows
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
