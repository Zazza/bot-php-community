package prompts

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestGetMeQuiz(t *testing.T) {
	if Get(Me) == "" {
		t.Fatal("me.txt пустой/отсутствует")
	}
	if Get(Quiz) == "" {
		t.Fatal("quiz.txt пустой/отсутствует")
	}
	if Get(QuizVerify) == "" {
		t.Fatal("quiz-verify.txt пустой/отсутствует")
	}
}

// TestGetAskChat — ask.txt встроен в бинарник и непуст (chat.txt — тот же call-site
// в chat/answer.go: prompts.Get(promptName, contextBlock)).
func TestGetAskChat(t *testing.T) {
	for _, name := range []string{Chat, Ask} {
		if Get(name) == "" {
			t.Errorf("prompts.Get(%q) = %q, want непустой промпт", name, Get(name))
		}
	}
}

// TestSafetyFirstLine — SAFETY-блок обязателен первой строкой (CLAUDE.md),
// правки промпта не должны сдвигать его вниз.
func TestSafetyFirstLine(t *testing.T) {
	for _, name := range []string{Chat, Ask, Digest, DigestPast, Spam, Quiz, QuizVerify,
		OncallScenario, OncallPlans, OncallOutcome, OncallPostmortem} {
		t.Run(name, func(t *testing.T) {
			if idx := strings.Index(Get(name), "SAFETY:"); idx != 0 {
				t.Errorf("%s: индекс \"SAFETY:\" = %d, want 0 (SAFETY-блок не первой строкой)", name, idx)
			}
		})
	}
}

// TestContextPlaceholder — промпты контекста интерполируются ровно одним аргументом
// (contextBlock): не один %s → Sprintf молча ломает системный промпт
// (%!s(MISSING) / %!(EXTRA ...)). DigestPast в списке НЕТ: он статичен, материал
// идёт отдельным user-сообщением (анти-промпт-инъекция).
func TestContextPlaceholder(t *testing.T) {
	for _, name := range []string{Chat, Ask, Digest} {
		t.Run(name, func(t *testing.T) {
			if n := strings.Count(Get(name), "%s"); n != 1 {
				t.Errorf("%s: плейсхолдеров %%s = %d, want 1", name, n)
			}
			if s := Get(name, "<ctx>"); !strings.Contains(s, "<ctx>") || strings.Contains(s, "%!") {
				t.Errorf("%s: Sprintf(%q) ломает промпт: %q", name, "<ctx>", s)
			}
		})
	}
}

// TestDigestPastStatic — ретро-промпт статичен (без плейсхолдера): материал
// дайджеста 5-летней давности не должен попадать в system-роль.
func TestDigestPastStatic(t *testing.T) {
	if n := strings.Count(Get(DigestPast), "%"); n != 0 {
		t.Errorf("digest-past.txt: %%-плейсхолдеров = %d, want 0 (промпт статичен)", n)
	}
}

// TestQuizPromptStatic — промпт викторины статичен (без %-плейсхолдеров): бан-лист
// недавних вопросов идёт отдельным user-сообщением (анти-промпт-инъекция, паттерн
// DigestPast); мёртвый литерал %s из старого промпта удалён.
func TestQuizPromptStatic(t *testing.T) {
	if n := strings.Count(Get(Quiz), "%"); n != 0 {
		t.Errorf("quiz.txt: %%-плейсхолдеров = %d, want 0 (промпт статичен)", n)
	}
}

// TestQuizPromptContract — контракт quiz.txt: вопрос-суждение (не знание факта),
// все 5 формул-архетипов, php-фенс, белый список категорий целиком, ровно 4
// варианта, лимит explanation, skip-гард.
func TestQuizPromptContract(t *testing.T) {
	p := Get(Quiz)
	for _, kw := range []string{
		"СУЖДЕНИЕ",
		"ГЛАВНОЕ не так",
		"принцип нарушен",
		"паттерн здесь уместен",
		"отрефакторить",
		"Архитектурный выбор",
		"```php",
		"Ровно 4 варианта",
		"150",
		`{"skip": true}`,
		"solid", "patterns", "clean-code", "refactoring", "arch", "oop", "testability",
	} {
		if !strings.Contains(p, kw) {
			t.Errorf("quiz.txt: нет маркера %q", kw)
		}
	}
	for _, bad := range []string{"Что выведет", "type-juggling"} {
		if strings.Contains(p, bad) {
			t.Errorf("quiz.txt: след старого знаниевого промпта: %q", bad)
		}
	}
}

// TestQuizFewShotParse — few-shot в quiz.txt обязан быть валидным JSON под структуру
// генератора: LLM копирует его формат, сломанный шаблон молча разваливает генерацию.
// Пример — одна строка: «.» не пересекает перевод строки, а жадный «.*}» берёт
// последнюю скобку строки (внутри question есть фигурные скобки PHP-кода).
func TestQuizFewShotParse(t *testing.T) {
	re := regexp.MustCompile(`\{"category".*\}`)
	matches := re.FindAllString(Get(Quiz), -1)
	if len(matches) != 1 {
		t.Fatalf("quiz.txt: few-shot примеров с {\"category\": ...} = %d, want 1", len(matches))
	}
	var q struct {
		Category    string   `json:"category"`
		Question    string   `json:"question"`
		Options     []string `json:"options"`
		Correct     int      `json:"correct"`
		Explanation string   `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(matches[0]), &q); err != nil {
		t.Fatalf("quiz.txt few-shot: невалидный JSON: %v", err)
	}
	if len(q.Options) != 4 || q.Correct < 0 || q.Correct > 3 || q.Explanation == "" {
		t.Errorf("quiz.txt few-shot: ломает собственный контракт: %+v", q)
	}
	if _, ok := map[string]struct{}{"solid": {}, "patterns": {}, "clean-code": {},
		"refactoring": {}, "arch": {}, "oop": {}, "testability": {}}[q.Category]; !ok {
		t.Errorf("quiz.txt few-shot: категория %q вне белого списка", q.Category)
	}
}

// TestQuizVerifyPromptContract — quiz-verify.txt: сверка получает вопрос без верного
// ответа, вердикт — одна цифра 0–3 или -1 (неоднозначно).
func TestQuizVerifyPromptContract(t *testing.T) {
	for _, kw := range []string{"SAFETY:", "-1", "0–3", "без пояснений"} {
		if !strings.Contains(Get(QuizVerify), kw) {
			t.Errorf("quiz-verify.txt: нет маркера %q", kw)
		}
	}
}

// TestAskSkipPolicy — ask.txt (явный /ask) запрещает SKIP и не содержит инструкций
// молчания в стиле chat.txt: явный вопрос не имеет права остаться без ответа.
func TestAskSkipPolicy(t *testing.T) {
	p := Get(Ask)
	if !strings.Contains(p, "SKIP запрещён") {
		t.Errorf("ask.txt: нет запрета SKIP — явный /ask может замолчать")
	}
	for _, bad := range []string{"выведи SKIP", "одно слово: SKIP"} {
		if strings.Contains(p, bad) {
			t.Errorf("ask.txt: инструкция молчания из chat.txt: %q", bad)
		}
	}
}

// TestSpamFewShotParse — регресс формата few-shot в spam.txt: каждая подстрока
// {"spam": ...} обязана быть валидным JSON под структуру {spam, reason}, иначе LLM
// копирует сломанный шаблон и parseSpamVerdict молча уходит в fallback not-spam.
func TestSpamFewShotParse(t *testing.T) {
	re := regexp.MustCompile(`\{"spam"[^}]*\}`)
	matches := re.FindAllString(Get(Spam), -1)
	if len(matches) < 2 {
		t.Fatalf("spam.txt: few-shot примеров с {\"spam\": ...} = %d, want >= 2", len(matches))
	}
	for _, m := range matches {
		var v struct {
			Spam   bool   `json:"spam"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(m), &v); err != nil {
			t.Errorf("spam.txt few-shot %q: невалидный JSON: %v", m, err)
			continue
		}
		if v.Reason == "" {
			t.Errorf("spam.txt few-shot %q: пустой reason", m)
		}
	}
}

// TestSpamPromptAnnounceCriterion — критерий анонса: анонс PHP/IT-мероприятия
// со ссылкой — НЕ спам. Без него invite от ветерана уходит в enforce по hard-сигналу.
func TestSpamPromptAnnounceCriterion(t *testing.T) {
	p := strings.ToLower(Get(Spam))
	for _, kw := range []string{"анонс", "мероприяти"} {
		if !strings.Contains(p, kw) {
			t.Errorf("spam.txt: нет ключевого слова %q (критерий анонса мероприятия исчез)", kw)
		}
	}
}
