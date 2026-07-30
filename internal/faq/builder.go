package faq

import (
	"context"
	"math"
	"strings"
	"time"

	"fmt"
	"github.com/jmoiron/sqlx"
	"github.com/pgvector/pgvector-go"
	"github.com/robfig/cron/v3"
	"log/slog"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
)

const (
	clusterMaxDist = 0.22 // косинусное расстояние: вопросы в один кластер
	buildMsgLimit  = 800  // сколько последних вопросо-сообщений анализировать
	maxFAQItems    = 60   // потолок записей на чат
	minAnswerRunes = 40   // порог длины ответа: короче — мусор, не FAQ
)

// Builder офлайн собирает FAQ: кластеризует вопросо-сообщения по эмбеддингам и для
// каждого кластера генерирует канон-ответ через LLM.
type Builder struct {
	db      *sqlx.DB
	llm     *llm.LLMClient
	msgs    *messages.Repository
	repo    *Repo
	chatIDs []int64
	cron    *cron.Cron
}

// NewBuilder создаёт билдер.
func NewBuilder(db *sqlx.DB, llm *llm.LLMClient, msgs *messages.Repository, repo *Repo, chatIDs []int64) *Builder {
	return &Builder{db: db, llm: llm, msgs: msgs, repo: repo, chatIDs: chatIDs}
}

// questionRow — строка JOIN messages↔embeddings для кластеризации.
type questionRow struct {
	ID     int64           `db:"id"`
	Text   string          `db:"text"`
	TS     time.Time       `db:"ts"`
	UserID int64           `db:"user_id"`
	Embed  pgvector.Vector `db:"embedding"`
}

// Build строит FAQ для чата: возвращает число записей или ошибку. Идемпотентен —
// полностью заменяет содержимое чата (ReplaceAll). Нечего кластеризовать → (0, nil).
func (b *Builder) Build(ctx context.Context, chatID int64) (int, error) {
	var rows []questionRow
	if err := b.db.SelectContext(ctx, &rows, `
		SELECT m.id, m.text, m.ts, m.user_id, e.embedding
		FROM messages m
		JOIN embeddings e ON e.message_id = m.id
		WHERE m.chat_id = $1
		  AND m.text LIKE '%?%'
		  AND char_length(m.text) BETWEEN 15 AND 200
		  AND m.text NOT ILIKE '%http%'
		  AND m.text NOT ILIKE '%www.%'
		  AND m.text NOT ILIKE '%t.me%'
		ORDER BY m.ts DESC LIMIT $2
	`, chatID, buildMsgLimit); err != nil {
		return 0, fmt.Errorf("select questions: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	clusters := clusterQuestions(rows, clusterMaxDist)

	items := make([]Item, 0, len(clusters))
	for _, cl := range clusters {
		if len(items) >= maxFAQItems {
			break
		}
		// Канон = самая короткая формулировка (лучшая).
		canonIdx := cl[0]
		for _, idx := range cl {
			if len(rows[idx].Text) < len(rows[canonIdx].Text) {
				canonIdx = idx
			}
		}

		// Ответы: следующее сообщение после каждого вопроса, если оно от другого автора.
		seen := make(map[string]struct{})
		var answers []string
		for _, idx := range cl {
			ans, err := b.msgs.NextAfter(ctx, chatID, rows[idx].TS)
			if err != nil {
				slog.Warn("faq next-after", "err", err)
				continue
			}
			if ans == nil || ans.UserID == rows[idx].UserID {
				continue
			}
			if len([]rune(ans.Text)) < minAnswerRunes {
				continue
			}
			if _, dup := seen[ans.Text]; dup {
				continue
			}
			seen[ans.Text] = struct{}{}
			answers = append(answers, ans.Text)
		}
		if len(answers) == 0 {
			continue // нет содержательных ответов — не FAQ
		}

		userMsg := "Вопрос: " + rows[canonIdx].Text +
			"\n\nОтветы участников:\n" + strings.Join(answers, "\n")
		resp, _, _, err := b.llm.Chat(ctx, []llm.Message{
			{Role: "system", Content: prompts.Get(prompts.FAQ)},
			{Role: "user", Content: userMsg},
		})
		if err != nil {
			slog.Warn("faq llm skip cluster", "err", err)
			continue
		}
		resp = strings.TrimSpace(resp)
		if resp == "" || resp == "SKIP" || strings.HasPrefix(resp, "SKIP") {
			continue
		}

		items = append(items, Item{
			ChatID:   chatID,
			Question: rows[canonIdx].Text,
			Answer:   resp,
			Vec:      rows[canonIdx].Embed.Slice(),
		})
	}

	if len(items) == 0 {
		// Нечего сохранить, но почистим устаревшие записи чата.
		if err := b.repo.ReplaceAll(ctx, chatID, nil); err != nil {
			return 0, fmt.Errorf("replace faq (empty): %w", err)
		}
		slog.Info("faq build empty", "chat", chatID)
		return 0, nil
	}

	if err := b.repo.ReplaceAll(ctx, chatID, items); err != nil {
		return 0, fmt.Errorf("replace faq: %w", err)
	}
	slog.Info("faq build", "chat", chatID, "items", len(items))
	return len(items), nil
}

// clusterQuestions — жадная in-memory кластеризация. Для каждого ещё не посещённого
// элемента создаёт кластер и добавляет последующие непосещённые в радиусе clusterMaxDist.
func clusterQuestions(rows []questionRow, maxDist float64) [][]int {
	visited := make([]bool, len(rows))
	var clusters [][]int
	for i := range rows {
		if visited[i] {
			continue
		}
		cluster := []int{i}
		visited[i] = true
		vi := rows[i].Embed.Slice()
		for j := i + 1; j < len(rows); j++ {
			if visited[j] {
				continue
			}
			if cosineDist(vi, rows[j].Embed.Slice()) < maxDist {
				cluster = append(cluster, j)
				visited[j] = true
			}
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

// cosineDist — косинусное расстояние (1 - similarity) для []float32.
// Разная длина или нулевая норма → 1.0 (полностью несходны).
func cosineDist(a, b []float32) float64 {
	if len(a) != len(b) {
		return 1.0
	}
	var dot, na, nb float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	if na == 0 || nb == 0 {
		return 1.0
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// Start регистрирует cron-расписание и запускает периодическую пересборку FAQ для
// каждого чата. cronExpr — стандартный 5-полный cron (напр. "0 4 * * 1" — пн 04:00).
func (b *Builder) Start(ctx context.Context, cronExpr string) error {
	c := cron.New()
	_, err := c.AddFunc(cronExpr, func() {
		for _, chatID := range b.chatIDs {
			bg, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			n, berr := b.Build(bg, chatID)
			cancel()
			if berr != nil {
				slog.Error("faq cron build", "chat", chatID, "err", berr)
				continue
			}
			slog.Info("faq cron done", "chat", chatID, "items", n)
		}
	})
	if err != nil {
		return fmt.Errorf("add faq cron: %w", err)
	}
	c.Start()
	b.cron = c
	slog.Info("faq builder started", "cron", cronExpr)
	return nil
}

// Stop останавливает cron.
func (b *Builder) Stop() {
	if b.cron != nil {
		b.cron.Stop()
	}
}
