package prompts

import (
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
	for _, name := range []string{Chat, Ask, Digest, DigestPast, FakeNews} {
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
