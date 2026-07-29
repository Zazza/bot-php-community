package messages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/pgvector/pgvector-go"
	"phpbot/internal/llm"
)

// VectorRepo хранит и ищет эмбеддинги сообщений через pgvector.
type VectorRepo struct {
	db      *sqlx.DB
	embed   *llm.Embedder
	queue   chan int64 // message_id для отложенного embedding
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewVectorRepo создаёт репозиторий. queueSize — буфер канала очереди.
func NewVectorRepo(db *sqlx.DB, embed *llm.Embedder, queueSize int) *VectorRepo {
	if queueSize <= 0 {
		queueSize = 100
	}
	return &VectorRepo{
		db:    db,
		embed: embed,
		queue: make(chan int64, queueSize),
		stop:  make(chan struct{}),
	}
}

// Enqueue ставит message_id в очередь на embedding (non-blocking: если очередь
// забита — пропускаем, бот не должен блокировать обработку update).
func (v *VectorRepo) Enqueue(messageID int64) {
	select {
	case v.queue <- messageID:
	default:
		slog.Warn("embedding queue full, dropping", "message_id", messageID)
	}
}

// Start запускает N worker-горутин. Останавливаются через Stop().
func (v *VectorRepo) Start(ctx context.Context, workers int) {
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		v.wg.Add(1)
		go v.worker(ctx, i)
	}
	slog.Info("embedding workers started", "count", workers)
}

// Stop дожидается завершения worker'ов.
func (v *VectorRepo) Stop() {
	close(v.stop)
	v.wg.Wait()
}

func (v *VectorRepo) worker(ctx context.Context, id int) {
	defer v.wg.Done()
	for {
		select {
		case <-v.stop:
			return
		case <-ctx.Done():
			return
		case msgID := <-v.queue:
			if err := v.embedMessage(ctx, msgID); err != nil {
				slog.Error("embed message failed", "message_id", msgID, "err", err)
			}
		}
	}
}

// embedMessage достаёт текст сообщения, эмбеддит и сохраняет вектор.
func (v *VectorRepo) embedMessage(ctx context.Context, msgID int64) error {
	var text string
	if err := v.db.GetContext(ctx, &text,
		`SELECT text FROM messages WHERE id = $1`, msgID); err != nil {
		return fmt.Errorf("get message: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		return nil // пустые/сервисные сообщения не индексируем
	}
	vec, err := v.embed.Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	if _, err := v.db.ExecContext(ctx, `
		INSERT INTO embeddings (message_id, embedding, model)
		VALUES ($1, $2, $3)
		ON CONFLICT (message_id) DO UPDATE SET embedding = EXCLUDED.embedding, model = EXCLUDED.model
	`, msgID, pgvector.NewVector(vec), v.embed.Model()); err != nil {
		return fmt.Errorf("save embedding: %w", err)
	}
	return nil
}

// EmbedAndSave — синхронный путь для query-time вектора (поиск/ответ).
func (v *VectorRepo) EmbedAndSave(ctx context.Context, msgID int64) error {
	return v.embedMessage(ctx, msgID)
}

// EmbedText эмбеддит произвольный текст (без сохранения).
func (v *VectorRepo) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return v.embed.Embed(ctx, text)
}

// SearchMessage — строка результата поиска с текстом сообщения.
type SearchMessage struct {
	Message
	Distance float64 `db:"distance" json:"distance"`
}

// SearchTopK ищет top-K ближайших сообщений в чате по косинусу к вектору запроса.
func (v *VectorRepo) SearchTopK(ctx context.Context, chatID int64, queryVec []float32, k int) ([]SearchMessage, error) {
	if k <= 0 {
		k = 8
	}
	var rows []SearchMessage
	// <=> — косинусное расстояние (HNSW vector_cosine_ops). Меньше = релевантнее.
	if err := v.db.SelectContext(ctx, &rows, `
		SELECT m.id, m.chat_id, m.user_id, m.username, m.text, m.ts, m.reply_to_id,
		       e.embedding <=> $1 AS distance
		FROM embeddings e
		JOIN messages m ON m.id = e.message_id
		WHERE m.chat_id = $2
		ORDER BY e.embedding <=> $1
		LIMIT $3
	`, pgvector.NewVector(queryVec), chatID, k); err != nil {
		return nil, fmt.Errorf("search top-k: %w", err)
	}
	return rows, nil
}

// FormatSearchResult — человекочитаемая выдача для /search в TG.
func FormatSearchResult(rows []SearchMessage) string {
	var b strings.Builder
	if len(rows) == 0 {
		return "Ничего не нашёл по истории чата."
	}
	for i, r := range rows {
		who := r.Username
		if who == "" {
			who = "user"
		}
		date := r.TS.Format("02.01 15:04")
		text := r.Text
		if len(text) > 200 {
			text = text[:200] + "…"
		}
		fmt.Fprintf(&b, "%d. [%s] %s: %s\n", i+1, date, who, text)
	}
	return b.String()
}
