package quiz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"phpbot/internal/llm"
	"phpbot/internal/prompts"
)

// errGenFailed — ни одна попытка не дала проверенного вопроса (LLM не уверен / повтор).
// cron при этой ошибке пропускает день — fail-safe: лучше молчание, чем неверный ответ.
var errGenFailed = errors.New("не удалось собрать проверенный вопрос викторины")

// maxAttempts — число попыток генерация+верификация, прежде чем сдаться.
const maxAttempts = 3

// dedupDays — окно дедупа по содержанию строгого раунда (не повторять тему/вопрос).
const dedupDays = 30

// dedupFallbackDays — ослабленное окно второго раунда: свежие повторы всё ещё недопустимы.
const dedupFallbackDays = 7

// maxBannedQuestions — сколько текстов недавних вопросов показывается генератору.
const maxBannedQuestions = 15

// maxQuestionRunes — предел длины текста вопроса в бан-листе генератору.
const maxQuestionRunes = 300

// Лимиты validQuiz — страховка от разошедшейся LLM (промпт просит 900/60/150,
// код отсекает только опасное для поста/кнопок TG, стиль не карая).
const (
	maxQuestionBodyRunes = 1200
	maxOptionRunes       = 64
	maxExplanationRunes  = 160
)

// allowedCategories — белый список категорий вопроса-суждения (договор с quiz.txt).
var allowedCategories = map[string]struct{}{
	"solid":       {},
	"patterns":    {},
	"clean-code":  {},
	"refactoring": {},
	"arch":        {},
	"oop":         {},
	"testability": {},
}

// Generator собирает вопрос на инженерное суждение через LLM и независимой
// сверкой защищает верный ответ.
type Generator struct {
	gen    *llm.LLMClient
	verify *llm.LLMClient
	repo   *Repository
}

// quizJSON — структура ответа генератора (строгий JSON из quiz.txt).
type quizJSON struct {
	Skip        bool     `json:"skip"`
	Category    string   `json:"category"`
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Correct     int      `json:"correct"`
	Explanation string   `json:"explanation"`
}

// Generate: LLM генерит MCQ-вопрос на инженерное суждение → валидация структуры и
// категории → дедуп по содержанию → независимая self-verify верного ответа.
// Повтор/не сошлось/LLM не уверен → следующая попытка. Два раунда: сначала
// окно дедупа dedupDays, при неудаче — ослабленное dedupFallbackDays (текст-дедуп не
// выключается). Генерация идёт творческим клиентом (temp=0.9), сверка — детерминированным
// (temp=0). Оба раунда исчерпаны → ошибка (cron пропустит день: лучше молчание, чем повтор).
func (g *Generator) Generate(ctx context.Context, chatID int64) (*Question, error) {
	recent, err := g.repo.RecentKeys(ctx, chatID, dedupDays)
	if err != nil {
		slog.Warn("quiz recent keys", "err", err)
		recent = nil
	}
	now := time.Now()
	for _, days := range []int{dedupDays, dedupFallbackDays} {
		since := now.AddDate(0, 0, -days)
		seenText := buildSeenKeys(recent, since)
		banned := buildRecentQuestions(recentSince(recent, since), maxBannedQuestions)
		for attempt := 0; attempt < maxAttempts; attempt++ {
			raw, err := g.genOne(ctx, banned)
			if err != nil || raw == nil || raw.Skip || !validQuiz(raw) {
				continue
			}
			if _, dup := seenText[normalizeKey(raw.Question)]; dup {
				continue
			}
			ok, err := g.verifyAnswer(ctx, raw)
			if err != nil || !ok {
				continue
			}
			return &Question{
				Kind:        strings.TrimSpace(raw.Category),
				Prompt:      strings.TrimSpace(raw.Question),
				Opts:        trimOpts(raw.Options),
				Correct:     raw.Correct,
				Explanation: strings.TrimSpace(raw.Explanation),
			}, nil
		}
	}
	return nil, errGenFailed
}

// genOne просит LLM сгенерировать один вопрос-суждение; недавние тексты вопросов
// (бан-лист с категорией-префиксом) идут в user-сообщении — промпт статичен.
func (g *Generator) genOne(ctx context.Context, banned []string) (*quizJSON, error) {
	sys := prompts.Get(prompts.Quiz)
	if sys == "" {
		return nil, errors.New("quiz prompt missing in embed")
	}
	user := "Сгенерируй один вопрос на инженерное суждение для викторины PHP-сообщества."
	if len(banned) > 0 {
		user += "\n\nЭти вопросы уже задавались (не повторяй их и похожие по смыслу):\n— " +
			strings.Join(banned, "\n— ")
	}
	resp, _, _, err := g.gen.Chat(ctx, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	})
	if err != nil {
		return nil, fmt.Errorf("quiz gen chat: %w", err)
	}
	var q quizJSON
	if err := json.Unmarshal([]byte(extractJSONObject(resp)), &q); err != nil {
		return nil, nil
	}
	return &q, nil
}

// verifyAnswer — независимая сверка верного ответа (детерминированным клиентом temp=0).
// Вернёт true, только если второй вызов однозначно указывает на тот же индекс;
// -1 (неоднозначно/некорректно) → отбраковка.
func (g *Generator) verifyAnswer(ctx context.Context, q *quizJSON) (bool, error) {
	sys := prompts.Get(prompts.QuizVerify)
	if sys == "" {
		return false, errors.New("quiz verify prompt missing in embed")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Вопрос: %s\nВарианты:\n", strings.TrimSpace(q.Question))
	letters := "ABCD"
	for i, o := range q.Options {
		if i >= len(letters) {
			break
		}
		fmt.Fprintf(&b, "%c) %s\n", letters[i], strings.TrimSpace(o))
	}
	resp, _, _, err := g.verify.Chat(ctx, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return false, fmt.Errorf("quiz verify chat: %w", err)
	}
	idx := parseVerify(resp)
	return idx >= 0 && idx == q.Correct, nil
}

// validQuiz проверяет структуру: непустой вопрос в пределах лимита, категория из
// белого списка (без учёта регистра), ровно 4 непустых варианта, корректный индекс.
func validQuiz(q *quizJSON) bool {
	if q == nil || strings.TrimSpace(q.Question) == "" {
		return false
	}
	if len([]rune(q.Question)) > maxQuestionBodyRunes {
		return false
	}
	if _, ok := allowedCategories[strings.ToLower(strings.TrimSpace(q.Category))]; !ok {
		return false
	}
	if len(q.Options) != 4 {
		return false
	}
	for _, o := range q.Options {
		if strings.TrimSpace(o) == "" || len([]rune(o)) > maxOptionRunes {
			return false
		}
	}
	if len([]rune(strings.TrimSpace(q.Explanation))) > maxExplanationRunes {
		return false
	}
	if q.Correct < 0 || q.Correct > 3 {
		return false
	}
	return true
}

// normalizeKey приводит текст к каноничному виду для дедупа: нижний регистр, удаление
// небуквенно-цифровых, схлопывание пробелов.
func normalizeKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r >= 'а' && r <= 'я', r == 'ё':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// trimOptions обрезает варианты до 4 и тримит пробелы.
func trimOpts(opts []string) []string {
	out := make([]string, 0, 4)
	for i, o := range opts {
		if i >= 4 {
			break
		}
		out = append(out, strings.TrimSpace(o))
	}
	return out
}

// buildSeenKeys собирает множество нормализованных текстов вопросов окна since
// для дедупа по содержанию.
func buildSeenKeys(recent []RecentKey, since time.Time) map[string]struct{} {
	text := make(map[string]struct{}, len(recent))
	for _, k := range recent {
		if k.CreatedAt.Before(since) {
			continue
		}
		text[normalizeKey(k.Question)] = struct{}{}
	}
	return text
}

// recentSince — вопросы из recent, заданные не раньше since (окно раунда дедупа).
func recentSince(recent []RecentKey, since time.Time) []RecentKey {
	out := make([]RecentKey, 0, len(recent))
	for _, k := range recent {
		if !k.CreatedAt.Before(since) {
			out = append(out, k)
		}
	}
	return out
}

// buildRecentQuestions — строки бан-листа генератору: «category: текст вопроса»
// (префикс категории, чтобы одна категория не маскировала другую); recent уже
// отсортирован DESC (свежие первыми), текст обрезан до maxQuestionRunes рун.
func buildRecentQuestions(recent []RecentKey, max int) []string {
	out := make([]string, 0, max)
	for _, k := range recent {
		if len(out) >= max {
			break
		}
		q := strings.TrimSpace(k.Question)
		if q == "" {
			continue
		}
		q = truncateRunes(q, maxQuestionRunes)
		if c := strings.TrimSpace(k.Category); c != "" {
			q = c + ": " + q
		}
		out = append(out, q)
	}
	return out
}

// truncateRunes обрезает строку до max рун, помечая усечение «…».
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// parseVerify достаёт целочисленный вердикт verifier'а (первый токен-число; -1 = неоднозначно).
func parseVerify(s string) int {
	for _, tok := range strings.Fields(strings.TrimSpace(s)) {
		tok = strings.TrimRight(tok, ".,;:!?")
		if n, err := strconv.Atoi(tok); err == nil {
			return n
		}
	}
	return -1
}

// extractJSONObject вырезает первый JSON-объект из ответа LLM (с пояснениями и markdown-оградкой).
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < 0 || j < i {
		return "{}"
	}
	return s[i : j+1]
}
