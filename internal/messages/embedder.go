package messages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pgvector/pgvector-go"
	"phpbot/internal/llm"
)

// VectorRepo хранит и ищет эмбеддинги сообщений через pgvector.
type VectorRepo struct {
	db    *sqlx.DB
	embed *llm.Embedder
	queue chan int64 // message_id для отложенного embedding
	stop  chan struct{}
	wg    sync.WaitGroup
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

// SearchTopK ищет top-K ближайших сообщений в чате по косинусу к вектору запроса
// без фильтров. Тонкая обёртка над search — поведение Answerer не меняется.
func (v *VectorRepo) SearchTopK(ctx context.Context, chatID int64, queryVec []float32, k int) ([]SearchMessage, error) {
	return v.search(ctx, chatID, queryVec, k, "", nil, nil)
}

// SearchFiltered ищет top-K по косинусу с опциональными фильтрами по автору
// (username без @) и периоду [since, until). Пустой username и nil-границы
// отключают соответствующие фильтры.
func (v *VectorRepo) SearchFiltered(ctx context.Context, chatID int64, queryVec []float32, k int, username string, since, until *time.Time) ([]SearchMessage, error) {
	return v.search(ctx, chatID, queryVec, k, username, since, until)
}

// search — общий движок поиска. username="" → nil-параметр (IS NULL истинно,
// фильтр отключен). nil since/until → NULL (pgx шлёт NULL для nil *time.Time).
func (v *VectorRepo) search(ctx context.Context, chatID int64, queryVec []float32, k int, username string, since, until *time.Time) ([]SearchMessage, error) {
	if k <= 0 {
		k = 8
	}
	var userParam interface{}
	if username != "" {
		userParam = username
	}
	var rows []SearchMessage
	// <=> — косинусное расстояние (HNSW vector_cosine_ops). Меньше = релевантнее.
	if err := v.db.SelectContext(ctx, &rows, `
		SELECT m.id, m.chat_id, m.user_id, m.username, m.text, m.ts, m.reply_to_id,
		       e.embedding <=> $1 AS distance
		FROM embeddings e
		JOIN messages m ON m.id = e.message_id
		WHERE m.chat_id = $2
		  AND ($3::text IS NULL OR m.username = $3)
		  AND ($4::timestamptz IS NULL OR m.ts >= $4)
		  AND ($5::timestamptz IS NULL OR m.ts < $5)
		ORDER BY e.embedding <=> $1
		LIMIT $6
	`, pgvector.NewVector(queryVec), chatID, userParam, since, until, k); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return rows, nil
}

// Expert — строка агрегации: кто чаще всего пишет по теме.
type Expert struct {
	UserID   int64  `db:"user_id" json:"user_id"`
	Username string `db:"username" json:"username"`
	Count    int    `db:"cnt" json:"count"`
}

// Experts считает, сколько сообщений каждого пользователя лежат в радиусе maxDist
// (косинусное расстояние) к теме — кто чаще пишет по ней.
func (v *VectorRepo) Experts(ctx context.Context, chatID int64, queryVec []float32, maxDist float64, limit int) ([]Expert, error) {
	if limit <= 0 {
		limit = 5
	}
	var rows []Expert
	if err := v.db.SelectContext(ctx, &rows, `
		SELECT m.user_id, COALESCE(NULLIF(m.username,''),'user') AS username, count(*) AS cnt
		FROM embeddings e
		JOIN messages m ON m.id = e.message_id
		WHERE m.chat_id = $1 AND (e.embedding <=> $2) < $3
		GROUP BY m.user_id, m.username
		ORDER BY cnt DESC
		LIMIT $4
	`, chatID, pgvector.NewVector(queryVec), maxDist, limit); err != nil {
		return nil, fmt.Errorf("experts: %w", err)
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
