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
const maxQuestionRunes = 120

// Generator собирает знаниевый вопрос через LLM и независимой сверкой защищает верный ответ.
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

// Generate: LLM генерит MCQ по знаниям PHP → дедуп по содержанию → независимая self-verify
// верного ответа. Повтор/не сошлось/LLM не уверен → следующая попытка. Два раунда: сначала
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
		seenText, seenCats := buildSeenKeys(recent, since)
		avoid := categoryList(seenCats)
		banned := buildRecentQuestions(recentSince(recent, since), maxBannedQuestions)
		for attempt := 0; attempt < maxAttempts; attempt++ {
			raw, err := g.genOne(ctx, avoid, banned)
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

// genOne просит LLM сгенерировать один MCQ, избегая недавних категорий и текстов вопросов.
func (g *Generator) genOne(ctx context.Context, avoid, banned []string) (*quizJSON, error) {
	user := "Сгенерируй один вопрос для викторины по знаниям PHP/веб."
	if len(avoid) > 0 {
		user += " Уже были темы — не повторяй их: " + strings.Join(avoid, ", ") + "."
	}
	if len(banned) > 0 {
		user += "\n\nЭти вопросы уже задавались (не повторяй их и похожие по смыслу):\n— " +
			strings.Join(banned, "\n— ")
	}
	resp, _, _, err := g.gen.Chat(ctx, []llm.Message{
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

// verifyAnswer — независимая сверка верного ответа (детерминированным клиентом temp=0).
// Вернёт true, только если второй вызов однозначно указывает на тот же индекс;
// -1 (неоднозначно/некорректно) → отбраковка.
func (g *Generator) verifyAnswer(ctx context.Context, q *quizJSON) (bool, error) {
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

// buildSeenKeys собирает множества нормализованных текстов и категорий вопросов окна since
// для дедупа по содержанию.
func buildSeenKeys(recent []RecentKey, since time.Time) (text, cats map[string]struct{}) {
	text = make(map[string]struct{}, len(recent))
	cats = make(map[string]struct{}, len(recent))
	for _, k := range recent {
		if k.CreatedAt.Before(since) {
			continue
		}
		text[normalizeKey(k.Question)] = struct{}{}
		if c := normalizeKey(k.Category); c != "" {
			cats[c] = struct{}{}
		}
	}
	return text, cats
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

// buildRecentQuestions — тексты недавних вопросов для бан-листа генератору: recent уже
// отсортирован DESC (свежие первыми), каждый обрезан до maxQuestionRunes рун.
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
		out = append(out, truncateRunes(q, maxQuestionRunes))
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
