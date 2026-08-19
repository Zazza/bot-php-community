// Package topics — еженедельный дайджест чата (суммаризация сообщений за период через LLM).
package topics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"
	"phpbot/internal/llm"
	"phpbot/internal/md"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
)

// Poster — минимальный интерфейс для постинга в чат (реализует обёртка tg).
type Poster interface {
	PostMessage(ctx context.Context, chatID int64, text string) error
}

// ErrTooFewMessages — дайджест не строится: за период слишком мало сообщений.
// Cron трактует как не-ошибку (debug), ручная команда /digest — сообщает пользователю.
var ErrTooFewMessages = errors.New("too few messages for digest")

// minDigestMessages — минимальное число сообщений за период, чтобы дайджест имел смысл.
const minDigestMessages = 3

const (
	pastYearsMax  = 5     // глубина ретро-рубрики: недели 1..N лет назад
	pastMaxChars  = 12000 // бюджет материала ретро-недели для LLM
	pastMaxMsgLen = 500   // кап длины одного сообщения в материале

	// digestPostMaxUTF16 — потолок итогового поста: TG-лимит 4096 UTF-16 code units,
	// запас на разметку. Ретро сверх него молча дропается — основной дайджест важнее.
	digestPostMaxUTF16 = 3900
)

// pastWeek — окно «той же недели» N лет назад и число сообщений в нём.
type pastWeek struct {
	YearsBack int
	Start     time.Time
	End       time.Time
	Count     int
}

// Digester делает недельную суммаризацию чата.
type Digester struct {
	db      *sqlx.DB
	llm     *llm.LLMClient
	msgs    *messages.Repository
	api     Poster
	chatIDs []int64
	cron    *cron.Cron
}

// NewDigester создаёт Digester.
func NewDigester(db *sqlx.DB, llm *llm.LLMClient, msgs *messages.Repository, api Poster, chatIDs []int64) *Digester {
	return &Digester{db: db, llm: llm, msgs: msgs, api: api, chatIDs: chatIDs}
}

// Start регистрирует еженедельный отчёт (по умолчанию пн 09:00) и запускает cron.
func (d *Digester) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		bg, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		end := time.Now()
		start := end.Add(-7 * 24 * time.Hour)
		for _, chatID := range d.chatIDs {
			if err := d.PostDigest(bg, chatID, chatID, start, end); err != nil {
				if errors.Is(err, ErrTooFewMessages) {
					slog.Debug("weekly digest skipped", "chat_id", chatID, "err", err)
				} else {
					slog.Error("weekly digest", "chat_id", chatID, "err", err)
				}
			}
		}
	})
	if err != nil {
		return fmt.Errorf("add digest cron: %w", err)
	}
	c.Start()
	d.cron = c
	slog.Info("digest scheduler started", "cron", spec)
	return nil
}

// Stop останавливает cron.
func (d *Digester) Stop() {
	if d.cron != nil {
		d.cron.Stop()
	}
}

// PostDigest суммаризирует сообщения за [start, end), постит в чат, пишет topic_digests.
// dataChatID — чат-источник данных (выборка); postChatID — куда постить. В группе
// они совпадают; в ЛС (админ) postChatID = ЛС, dataChatID = группа.
func (d *Digester) PostDigest(ctx context.Context, dataChatID, postChatID int64, start, end time.Time) error {
	msgs, err := d.msgs.Since(ctx, dataChatID, start, end)
	if err != nil {
		return fmt.Errorf("since: %w", err)
	}
	if len(msgs) < minDigestMessages {
		slog.Info("digest: too few messages, skip", "chat_id", dataChatID, "count", len(msgs))
		return fmt.Errorf("%w: have %d, need %d", ErrTooFewMessages, len(msgs), minDigestMessages)
	}
	material := chatFormatForDigest(msgs)
	prompt := prompts.Get(prompts.Digest, material)
	resp, _, _, err := d.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Сделай недельный дайджест."},
	})
	if err != nil {
		return fmt.Errorf("digest llm: %w", err)
	}
	summary := strings.TrimSpace(resp)

	header := fmt.Sprintf("📋 Дайджест недели (%s — %s)\n\n",
		start.Format("02.01"), end.Add(-24*time.Hour).Format("02.01"))
	post := appendRetro(header+summary, d.pastSection(ctx, dataChatID, start, end))
	if err := d.api.PostMessage(ctx, postChatID, post); err != nil {
		return fmt.Errorf("post digest: %w", err)
	}
	// summary без ретро: ретро-секция описывает другой период, в topic_digests не входит.
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO topic_digests (chat_id, period_start, period_end, summary, posted)
		VALUES ($1, $2, $3, $4, TRUE)
	`, dataChatID, start, end, summary); err != nil {
		slog.Warn("save digest row", "err", err)
	}
	return nil
}

// pastSection строит ретро-секцию дайджеста: самая активная «та же неделя» 1..pastYearsMax
// лет назад. НИКОГДА не возвращает error — ретро вспомогательное, провал не должен ронять
// основной дайджест. Пустая строка — секции нет.
func (d *Digester) pastSection(ctx context.Context, chatID int64, start, end time.Time) string {
	if end.Sub(start) != 7*24*time.Hour {
		return ""
	}
	cands := pastWeekCandidates(start, end, pastYearsMax)
	for i := range cands {
		n, err := d.msgs.CountBetween(ctx, chatID, cands[i].Start, cands[i].End)
		if err != nil {
			slog.Warn("digest retro count", "chat_id", chatID, "err", err)
			return ""
		}
		cands[i].Count = n
	}
	pick := pickPastWeek(cands, minDigestMessages)
	if pick == nil {
		slog.Debug("digest retro: no active past week", "chat_id", chatID)
		return ""
	}
	msgs, err := d.msgs.Since(ctx, chatID, pick.Start, pick.End)
	if err != nil {
		slog.Warn("digest retro since", "chat_id", chatID, "err", err)
		return ""
	}
	if len(msgs) < minDigestMessages {
		slog.Warn("digest retro: too few messages", "chat_id", chatID, "count", len(msgs))
		return ""
	}
	material := formatPastForDigest(msgs, pastMaxChars)
	system := prompts.Get(prompts.DigestPast)
	if system == "" {
		slog.Warn("digest retro: empty prompt", "name", prompts.DigestPast)
		return ""
	}
	// Материал — отдельным user-сообщением: системный промпт статичен, инструкции из
	// старых сообщений участников не исполняются (анти-инъекция, паттерн spam/about).
	resp, _, _, err := d.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: material},
	})
	if err != nil {
		slog.Warn("digest retro llm", "chat_id", chatID, "err", err)
		return ""
	}
	if isSkipText(resp) {
		return ""
	}
	summary := strings.TrimSpace(resp)
	if summary == "" {
		return ""
	}
	return pastSectionHeader(*pick) + "\n" + summary
}

// appendRetro дописывает ретро-секцию к посту, если итог укладывается в лимит TG:
// пост сверх 4096 UTF-16 Telegram отклонит целиком, ретро вторично — жертвуем ею.
func appendRetro(post, retro string) string {
	if retro == "" || utf16Len(post)+2+utf16Len(retro) > digestPostMaxUTF16 {
		return post
	}
	return post + "\n\n" + retro
}

// utf16Len — длина строки в UTF-16 code units (единица лимитов Telegram). Символы
// вне BMP (эмодзи 🕰 и т.п.) занимают 2.
func utf16Len(s string) int { return md.UTF16Len(s) }

// chatFormatForDigest — хронологический текст сообщений для подачи в LLM.
func chatFormatForDigest(msgs []messages.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		ts := m.TS.Format("02.01 15:04")
		text := m.Text
		if len(text) > 500 {
			text = text[:500] + "…"
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", ts, who, text)
	}
	return b.String()
}

// pastWeekCandidates строит окна «той же недели» 1..maxYears лет назад. Ширина окна —
// точный end.Sub(start), а не AddDate обоих бортов: 29 февраля искажало бы ширину на сутки.
func pastWeekCandidates(start, end time.Time, maxYears int) []pastWeek {
	cands := make([]pastWeek, 0, maxYears)
	for n := 1; n <= maxYears; n++ {
		startBack := start.AddDate(-n, 0, 0)
		endBack := startBack.Add(end.Sub(start))
		cands = append(cands, pastWeek{YearsBack: n, Start: startBack, End: endBack})
	}
	return cands
}

// pickPastWeek выбирает самое активное окно; nil, если все ниже min. При равном счёте
// побеждает более свежий год: итерация по возрастанию YearsBack, строго больше для победы.
func pickPastWeek(cands []pastWeek, min int) *pastWeek {
	var best *pastWeek
	for i := range cands {
		if cands[i].Count < min {
			continue
		}
		if best == nil || cands[i].Count > best.Count {
			best = &cands[i]
		}
	}
	return best
}

// yearsAgoLabel — «Год назад» / «2 года назад» / «5 лет назад».
func yearsAgoLabel(n int) string {
	switch {
	case n == 1:
		return "Год назад"
	case n >= 2 && n <= 4:
		return fmt.Sprintf("%d года назад", n)
	default:
		return fmt.Sprintf("%d лет назад", n)
	}
}

var monthPrep = map[time.Month]string{
	time.January:   "январе",
	time.February:  "феврале",
	time.March:     "марте",
	time.April:     "апреле",
	time.May:       "мае",
	time.June:      "июне",
	time.July:      "июле",
	time.August:    "августе",
	time.September: "сентябре",
	time.October:   "октябре",
	time.November:  "ноябре",
	time.December:  "декабре",
}

func pastSectionHeader(w pastWeek) string {
	return fmt.Sprintf("🕰 %s, в %s %d, обсуждали:",
		yearsAgoLabel(w.YearsBack), monthPrep[w.Start.Month()], w.Start.Year())
}

// formatPastForDigest — как chatFormatForDigest, но с общим бюджетом maxChars: строки
// копятся целиком, при превышении бюджета — стоп (строка не рвётся пополам).
func formatPastForDigest(msgs []messages.Message, maxChars int) string {
	var b strings.Builder
	for _, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		text := m.Text
		if len(text) > pastMaxMsgLen {
			text = text[:pastMaxMsgLen] + "…"
		}
		line := fmt.Sprintf("[%s] %s: %s\n", m.TS.Format("02.01 15:04"), who, text)
		if b.Len()+len(line) > maxChars {
			if b.Len() > 0 {
				b.WriteString("…(обрезано)\n")
			}
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// isSkipText — LLM ответил SKIP (ретро не о чем писать). Локальный хелпер: chat.isSkip
// не экспортирован.
func isSkipText(resp string) bool {
	return strings.EqualFold(strings.TrimSpace(resp), "SKIP")
}
