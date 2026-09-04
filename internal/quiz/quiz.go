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
	"phpbot/internal/md"
)

// Quiz — домен викторины: генерит вопрос, постит с кнопками, обрабатывает ответы.
type Quiz struct {
	api     *bot.Bot
	gen     *Generator
	repo    *Repository
	chatIDs []int64
}

// New создаёт Quiz. genLLM — творческий клиент генерации вопросов (temp=0.9),
// verifyLLM — детерминированный клиент сверки верного ответа (temp=0).
func New(api *bot.Bot, genLLM, verifyLLM *llm.LLMClient, repo *Repository, chatIDs []int64) *Quiz {
	return &Quiz{api: api, gen: &Generator{gen: genLLM, verify: verifyLLM, repo: repo}, repo: repo, chatIDs: chatIDs}
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
		ChatID:      chatID,
		Text:        md.ToHTML(renderQuestion(qn.Prompt, qn.Opts, 0, 0)),
		ParseMode:   models.ParseModeHTML,
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
// отвечает тостом ✅/❌ и обновляет live-tally на сообщении. «Показать ответ» отвечает
// модальным алертом — он виден ТОЛЬКО нажавшему, не срывает голосование и не исчезает
// сам (в отличие от тоста); раскрывается лишь проголосовавшему (по quiz_ballots), сбой
// БД — fail-closed, без раскрытия. Возвращает (текст, showAlert). Текст собирается через
// composeAlert под лимит 200 UTF-16 (Telegram answerCallbackQuery): приоритет — объяснению
// «почему», под нож первой идёт декор-тэлли, пояснение режется по границе слова лишь если
// само не влезает. При превышении лимита алерт молча не покажется.
func (q *Quiz) HandleQuizCallback(ctx context.Context, cb *models.CallbackQuery) (string, bool) {
	parts := strings.Split(cb.Data, ":") // quiz:<id>:<opt>
	if len(parts) != 3 {
		return "", false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "Некорректная кнопка", false
	}
	row, err := q.repo.Get(ctx, id)
	if err != nil || row == nil {
		return "Вопрос уже не активен", false
	}

	// «Показать ответ»: модальный алерт (showAlert=true) виден ТОЛЬКО нажавшему. Сообщение
	// не меняем, кнопки не снимаем, голосование не срываем — один человек не раскрывает ответ всем.
	// Гейт: ответ раскрывается только проголосовавшему, иначе участники подглядывают верный
	// вариант и отвечают безошибочно. При сбое БД не раскрываем (fail-closed).
	if parts[2] == "r" {
		voted, err := q.repo.HasBallot(ctx, id, cb.From.ID)
		if text, reveal := revealGate(voted, err); !reveal {
			return text, false
		}
		counts, _ := q.repo.BallotCounts(ctx, id)
		return capAlert(revealToast(row, counts)), true
	}

	choice, err := strconv.Atoi(parts[2])
	if err != nil || choice < 0 || choice > 3 {
		return "Некорректная кнопка", false
	}

	isNew, err := q.repo.RecordBallot(ctx, id, cb.From.ID, choice)
	if err != nil {
		return "Ошибка, попробуй ещё", false
	}
	if !isNew {
		return "Ты уже отвечал 🙂", false
	}

	letters := "ABCD"
	opts := row.Opts()
	picked := strings.TrimSpace(opts[choice])
	right := strings.TrimSpace(opts[row.Correct])
	var head string
	if choice == row.Correct {
		head = fmt.Sprintf("Ты выбрал %c) %s — ✅ Верно!", letters[choice], picked)
	} else {
		head = fmt.Sprintf("Ты выбрал %c) %s — ❌. Правильно: %c) %s", letters[choice], picked, letters[row.Correct], right)
	}
	toast := composeAlert(head, strings.TrimSpace(row.Explanation), "")

	// live-tally: обновим сообщение (кнопки оставляем — отвечает каждый по разу).
	total, correct, _ := q.repo.CountBallots(ctx, id)
	if row.MessageID != 0 {
		kb := quizKeyboard(id, row.Opts())
		if _, eerr := q.api.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      row.ChatID,
			MessageID:   int(row.MessageID),
			Text:        md.ToHTML(renderQuestion(row.Question, row.Opts(), total, correct)),
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: &kb,
		}); eerr != nil {
			slog.Warn("quiz tally edit", "err", eerr)
		}
	}
	return capAlert(toast), false
}

// revealGate решает, раскрывать ли правильный ответ нажавшему «Показать ответ»:
// только проголосовавшему; сбой БД — fail-closed, без раскрытия.
func revealGate(voted bool, err error) (text string, reveal bool) {
	if err != nil {
		return "Ошибка, попробуй ещё", false
	}
	if !voted {
		return "Сначала выбери вариант 🙂", false
	}
	return "", true
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
// Верный вариант — обязательная «шапка», объяснение «почему» приоритетнее декоративной
// тэлли: под лимит алерта тэлли выкидывается первой (см. composeAlert).
func revealToast(row *Row, counts map[int]int) string {
	letters := "ABCD"
	opts := row.Opts()
	head := fmt.Sprintf("👁 Правильно: %c) %s", letters[row.Correct], strings.TrimSpace(opts[row.Correct]))
	total := 0
	for _, c := range counts {
		total += c
	}
	var tally string
	if total > 0 {
		tally = fmt.Sprintf(" (верно %d из %d)", counts[row.Correct], total)
	}
	return composeAlert(head, strings.TrimSpace(row.Explanation), tally)
}

// alertMaxUTF16 — лимит Telegram answerCallbackQuery: text 0–200, считаются UTF-16
// code units (эмодзи = 2). Превышение → 400, алерт/тост молча не покажется.
const alertMaxUTF16 = 200

// composeAlert собирает текст всплывающего ответа под лимит alertMaxUTF16 с приоритетом
// объяснения «почему»: head (верный вариант) сохраняется целиком, затем в остаток бюджета
// укладывается explanation (обрезается по границе слова с «…», лишь если сам не влезает),
// и только если после этого остаётся место И пояснение не урезано — добавляется декор tail
// (тэлли голосов). Так «почему» больше не режется ради счётчика — под нож первым идёт декор.
func composeAlert(head, explanation, tail string) string {
	out := head
	truncated := false
	if explanation != "" {
		body, cut := fitRunes("\n💡 "+explanation, alertMaxUTF16-utf16Len(out))
		out += body
		truncated = cut
	}
	if tail != "" && !truncated && utf16Len(out)+utf16Len(tail) <= alertMaxUTF16 {
		out += tail
	}
	return out
}

// fitRunes возвращает префикс s, влезающий в budget UTF-16, и флаг «обрезано». При обрезке
// откатывается до последней границы слова (пробела) и дописывает «…», чтобы не рвать на
// полуслове. budget≤1 или пустой префикс → ("", true).
func fitRunes(s string, budget int) (string, bool) {
	if utf16Len(s) <= budget {
		return s, false
	}
	if budget <= 1 {
		return "", true
	}
	limit := budget - 1 // место под «…»
	var b strings.Builder
	n := 0
	for _, r := range s {
		cost := utf16Len(string(r))
		if n+cost > limit {
			break
		}
		b.WriteRune(r)
		n += cost
	}
	out := b.String()
	if i := strings.LastIndexByte(out, ' '); i > 0 {
		out = out[:i]
	}
	out = strings.TrimRight(out, " \n")
	if out == "" {
		return "", true
	}
	return out + "…", true
}

// capAlert — финальный предохранитель под лимит answerCallbackQuery (на случай, когда одна
// только head-шапка длиннее лимита). Основная укладка с приоритетом «почему» — в composeAlert;
// здесь простое посимвольное усечение с «…».
func capAlert(s string) string {
	if utf16Len(s) <= alertMaxUTF16 {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		cost := utf16Len(string(r))
		if n+cost+1 > alertMaxUTF16 { // +1 — место под «…»
			break
		}
		b.WriteRune(r)
		n += cost
	}
	return b.String() + "…"
}

// utf16Len возвращает длину строки в UTF-16 code units (как считает Telegram).
func utf16Len(s string) int { return md.UTF16Len(s) }

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
