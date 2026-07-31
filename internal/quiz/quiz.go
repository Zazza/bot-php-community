package quiz

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
)

// Quiz — домен викторины: генерит вопрос, постит с кнопками, обрабатывает ответы.
type Quiz struct {
	api     *bot.Bot
	gen     *Generator
	repo    *Repository
	chatIDs []int64
}

// New создаёт Quiz.
func New(api *bot.Bot, llm *llm.LLMClient, msgs *messages.Repository, repo *Repository, chatIDs []int64) *Quiz {
	return &Quiz{api: api, gen: &Generator{msgs: msgs, llm: llm, repo: repo}, repo: repo, chatIDs: chatIDs}
}

// Post генерирует вопрос и шлёт его с inline-кнопками в чат.
func (q *Quiz) Post(ctx context.Context, chatID int64) error {
	qn, err := q.gen.Generate(ctx, chatID)
	if err != nil {
		return err
	}
	id, err := q.repo.SaveQuiz(ctx, qn, chatID)
	if err != nil {
		return err
	}
	sent, err := q.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: renderQuestion(qn.Prompt, qn.Opts, 0, 0),
		ReplyMarkup: quizKeyboard(id, qn.Opts),
	})
	if err != nil {
		return fmt.Errorf("quiz send: %w", err)
	}
	if sent != nil {
		if err := q.repo.SetMessage(ctx, id, int64(sent.ID)); err != nil {
			slog.Warn("quiz set message", "err", err)
		}
	}
	slog.Info("quiz posted", "chat_id", chatID, "kind", qn.Kind, "id", id)
	return nil
}

// HandleQuizCallback обрабатывает тап по варианту: учитывает голос (один на юзера),
// мгновенно отвечает тостом ✅/❌ и обновляет live-tally на сообщении. Возвращает текст тоста.
func (q *Quiz) HandleQuizCallback(ctx context.Context, cb *models.CallbackQuery) string {
	parts := strings.Split(cb.Data, ":") // quiz:<id>:<opt>
	if len(parts) != 3 {
		return ""
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "Некорректная кнопка"
	}
	row, err := q.repo.Get(ctx, id)
	if err != nil || row == nil {
		return "Вопрос уже не активен"
	}

	// «Показать ответ»: ответ виден ТОЛЬКО нажавшему (приватный тост). Сообщение не
	// меняем, кнопки не снимаем, голосование не срываем — один человек не раскрывает ответ всем.
	if parts[2] == "r" {
		counts, _ := q.repo.BallotCounts(ctx, id)
		return revealToast(row, counts)
	}

	choice, err := strconv.Atoi(parts[2])
	if err != nil || choice < 0 || choice > 3 {
		return "Некорректная кнопка"
	}

	isNew, err := q.repo.RecordBallot(ctx, id, cb.From.ID, choice)
	if err != nil {
		return "Ошибка, попробуй ещё"
	}
	if !isNew {
		return "Ты уже отвечал 🙂"
	}

	letters := "ABCD"
	opts := row.Opts()
	picked := strings.TrimSpace(opts[choice])
	right := strings.TrimSpace(opts[row.Correct])
	var toast string
	if choice == row.Correct {
		toast = fmt.Sprintf("Ты выбрал %c) %s — ✅ Верно!", letters[choice], picked)
	} else {
		toast = fmt.Sprintf("Ты выбрал %c) %s — ❌. Правильно: %c) %s", letters[choice], picked, letters[row.Correct], right)
	}

	// live-tally: обновим сообщение (кнопки оставляем — отвечает каждый по разу).
	total, correct, _ := q.repo.CountBallots(ctx, id)
	if row.MessageID != 0 {
		kb := quizKeyboard(id, row.Opts())
		if _, eerr := q.api.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID: row.ChatID, MessageID: int(row.MessageID),
			Text:        renderQuestion(row.Question, row.Opts(), total, correct),
			ReplyMarkup: &kb,
		}); eerr != nil {
			slog.Warn("quiz tally edit", "err", eerr)
		}
	}
	return toast
}

// renderQuestion собирает текст вопроса с вариантами и (если есть голоса) live-tally.
func renderQuestion(prompt string, opts []string, total, correct int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🧩 %s\n\n", prompt)
	letters := "ABCD"
	for i, o := range opts {
		if i >= len(letters) {
			break
		}
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		fmt.Fprintf(&b, "%c) %s\n", letters[i], o)
	}
	if total > 0 {
		fmt.Fprintf(&b, "\n🗳 Голосов: %d · ✅ Верно: %d", total, correct)
	}
	return b.String()
}

// revealToast строит приватный текст правильного ответа для нажавшего «Показать ответ».
func revealToast(row *Row, counts map[int]int) string {
	letters := "ABCD"
	opts := row.Opts()
	total := 0
	for _, c := range counts {
		total += c
	}
	s := fmt.Sprintf("👁 Правильно: %c) %s", letters[row.Correct], strings.TrimSpace(opts[row.Correct]))
	if total > 0 {
		s += fmt.Sprintf(" (верно %d из %d)", counts[row.Correct], total)
	}
	return s
}

// quizKeyboard строит inline-кнопки вариантов. callback_data: quiz:<id>:<opt index>.
func quizKeyboard(quizID int64, opts []string) models.InlineKeyboardMarkup {
	prefix := "quiz:" + strconv.FormatInt(quizID, 10) + ":"
	letters := "ABCD"
	btns := make([]models.InlineKeyboardButton, 0, 4)
	for i, o := range opts {
		if i >= len(letters) {
			break
		}
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		btns = append(btns, models.InlineKeyboardButton{
			Text:         fmt.Sprintf("%c) %s", letters[i], o),
			CallbackData: prefix + strconv.Itoa(i),
		})
	}
	rows := make([][]models.InlineKeyboardButton, 0, (len(btns)+1)/2)
	for i := 0; i < len(btns); i += 2 {
		end := i + 2
		if end > len(btns) {
			end = len(btns)
		}
		rows = append(rows, btns[i:end])
	}
	// отдельной строкой — кнопка раскрытия правильного ответа (callback_data quiz:<id>:r).
	rows = append(rows, []models.InlineKeyboardButton{{
		Text:         "👁 Показать ответ",
		CallbackData: prefix + "r",
	}})
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}
