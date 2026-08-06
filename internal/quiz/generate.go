package quiz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"phpbot/internal/llm"
	"phpbot/internal/prompts"
)

// errGenFailed — ни одна попытка не дала проверенного вопроса (LLM не уверен / повтор).
// cron при этой ошибке пропускает день — fail-safe: лучше молчание, чем неверный ответ.
var errGenFailed = errors.New("не удалось собрать проверенный вопрос викторины")

// maxAttempts — число попыток генерация+верификация, прежде чем сдаться.
const maxAttempts = 3

// dedupDays — окно дедупа по содержанию (не повторять тему/вопрос).
const dedupDays = 30

// Generator собирает знаниевый вопрос через LLM и независимой сверкой защищает верный ответ.
type Generator struct {
	llm  *llm.LLMClient
	repo *Repository
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

// Generate: LLM генерит MCQ по знаниям PHP → дедуп по содержанию → независимая self-verify
// верного ответа. Повтор/не сошлось/LLM не уверен → следующая попытка. Сначала идём со строгим
// дедупом; если за maxAttempts вариантов не осталось — ослабляем окно (без текст-дедупа, но
// всё ещё с verify). Все попытки провалились → ошибка (cron пропустит день).
func (g *Generator) Generate(ctx context.Context, chatID int64) (*Question, error) {
	recent, _ := g.repo.RecentKeys(ctx, chatID, dedupDays)
	seenText, seenCats := buildSeenKeys(recent)
	avoid := categoryList(seenCats)
	for _, enforceDedup := range []bool{true, false} {
		for attempt := 0; attempt < maxAttempts; attempt++ {
			raw, err := g.genOne(ctx, avoid)
			if err != nil || raw == nil || raw.Skip || !validQuiz(raw) {
				continue
			}
			if enforceDedup {
				if _, dup := seenText[normalizeKey(raw.Question)]; dup {
					continue
				}
			}
			ok, err := g.verify(ctx, raw)
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

// genOne просит LLM сгенерировать один MCQ, избегая недавних категорий.
func (g *Generator) genOne(ctx context.Context, avoid []string) (*quizJSON, error) {
	user := "Сгенерируй один вопрос для викторины по знаниям PHP/веб."
	if len(avoid) > 0 {
		user += " Уже были темы — не повторяй их: " + strings.Join(avoid, ", ") + "."
	}
	resp, _, _, err := g.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompts.Get(prompts.Quiz)},
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

// verify — независимая сверка верного ответа. Вернёт true, только если второй вызов
// однозначно указывает на тот же индекс; -1 (неоднозначно/некорректно) → отбраковка.
func (g *Generator) verify(ctx context.Context, q *quizJSON) (bool, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Вопрос: %s\nВарианты:\n", strings.TrimSpace(q.Question))
	letters := "ABCD"
	for i, o := range q.Options {
		if i >= len(letters) {
			break
		}
		fmt.Fprintf(&b, "%c) %s\n", letters[i], strings.TrimSpace(o))
	}
	resp, _, _, err := g.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: "Ты PHP-эксперт. Для вопроса викторины верни ОДНУ цифру — " +
			"индекс верного варианта (0-3). Если верных несколько, ни одного или вопрос " +
			"допускает трактовки — верни -1. Только цифра, без текста."},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return false, fmt.Errorf("quiz verify chat: %w", err)
	}
	idx := parseVerify(resp)
	return idx >= 0 && idx == q.Correct, nil
}

// validQuiz проверяет структуру: ровно 4 непустых варианта, корректный индекс в диапазоне,
// непустой вопрос.
func validQuiz(q *quizJSON) bool {
	if q == nil || strings.TrimSpace(q.Question) == "" {
		return false
	}
	if len(q.Options) != 4 {
		return false
	}
	for _, o := range q.Options {
		if strings.TrimSpace(o) == "" {
			return false
		}
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

func categoryList(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// buildSeenKeys собирает множества нормализованных текстов и категорий недавних вопросов
// для дедупа по содержанию.
func buildSeenKeys(recent []RecentKey) (text, cats map[string]struct{}) {
	text = make(map[string]struct{}, len(recent))
	cats = make(map[string]struct{}, len(recent))
	for _, k := range recent {
		text[normalizeKey(k.Question)] = struct{}{}
		if c := normalizeKey(k.Category); c != "" {
			cats[c] = struct{}{}
		}
	}
	return text, cats
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
