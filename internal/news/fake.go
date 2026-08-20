package news

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"phpbot/internal/llm"
	"phpbot/internal/md"
	"phpbot/internal/prompts"
)

// Пятничный выпуск: LLM-пародия на PHP-дайджест (все новости/пакеты/ссылки вымышлены).
// Пост НЕ отличается от обычного выпуска: заголовок общий (digestTitle), маркеров
// выдуманности нет. Отдельная ссылка не проходит модерацию ссылок реальных фидов —
// у неё свой white-список доменов (виды «правдоподобных» ссылок) и своя санитайз-логика
// по блокам.
const fakePostMaxUTF16 = 3900

// fakeHosts — единственные домены, допустимые в ссылках пятничного выпуска: пародия
// «по форме» на реальную экосистему, но все пути ведут на несуществующие страницы.
var fakeHosts = map[string]struct{}{
	"php.net":       {},
	"www.php.net":   {},
	"github.com":    {},
	"packagist.org": {},
}

// mdLinkRe — markdown-ссылка [текст](url) в ответе LLM.
var mdLinkRe = regexp.MustCompile(`\[[^\[\]]*\]\((https?://[^)\s]+)\)`)

// fakeMetaMarkers — МЕТА-маркеры выдуманности (не контентные слова юмора): их утечка
// из LLM самопомечает выпуск, нарушая инвариант «неотличим от обычного». Заголовок
// контролирует код (digestTitle), тело — промпт; это страховка последней линии.
var fakeMetaMarkers = []string{"пятничн", "вымышлен", "выдум", "первое апреля", "первого апреля", "не всерьёз"}

// isFridaySlot — пятничный слот ежедневного cron-прогона дайджеста.
func isFridaySlot(t time.Time) bool { return t.Weekday() == time.Friday }

// fakeHostAllowed — домен ссылки входит в fakeHosts. Ошибка парсинга/пустой хост/
// userinfo (легитимные выдуманные ссылки его не содержат) → false.
func fakeHostAllowed(link string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	_, ok := fakeHosts[u.Host]
	return ok
}

// PostFake генерирует и постит «пятничный выпуск» — шуточный полностью вымышленный
// дайджест. Ручной запуск (/news fake) и пятничный cron-слот (fallback на обычный Post
// при ошибке — в вызывающем коде).
func (d *Digester) PostFake(ctx context.Context, chatID int64) error {
	system := prompts.Get(prompts.FakeNews)
	if system == "" {
		return fmt.Errorf("fake news: empty prompt %s", prompts.FakeNews)
	}
	resp, _, _, err := d.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: "Выпуск от " + time.Now().Format("02.01.2006") + "."},
	})
	if err != nil {
		return fmt.Errorf("fake news llm: %w", err)
	}
	body := sanitizeFakeBody(resp)
	if body == "" {
		return fmt.Errorf("fake news: empty body after sanitize")
	}
	if err := d.api.PostMessage(ctx, chatID, assembleFakePost(body)); err != nil {
		return fmt.Errorf("post fake news: %w", err)
	}
	return nil
}

// sanitizeFakeBody чистит ответ LLM по блокам (разделитель "\n\n"): блок дропается, если
// содержит markdown-ссылку с чужим доменом, голый URL вне markdown-ссылки или МЕТА-маркер
// выдуманности. Блоки без URL (заголовки секций) проходят. Не осталось ни одной валидной
// ссылки → "".
func sanitizeFakeBody(resp string) string {
	var blocks []string
	validLinks := 0
	for _, block := range strings.Split(resp, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		mds := mdLinkRe.FindAllStringSubmatch(block, -1)
		ok := true
		for _, m := range mds {
			if !fakeHostAllowed(m[1]) {
				ok = false
				break
			}
		}
		// Каждый валидный md-матч поглощает ровно один "](": оставшийся "](" —
		// ссылка-конструкт с чужой схемой/регистром (HTTPS://, tg://, javascript:),
		// которую md.ToHTML отрендерит кликабельной в обход белого списка.
		residue := mdLinkRe.ReplaceAllString(block, "")
		if strings.Contains(residue, "](") {
			ok = false
		}
		// Голый URL — вне markdown-обёртки.
		if urlTokenRe.MatchString(residue) {
			ok = false
		}
		// Утечка МЕТА-маркера выдуманности — блок самопомечает выпуск.
		low := strings.ToLower(block)
		for _, m := range fakeMetaMarkers {
			if strings.Contains(low, m) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		validLinks += len(mds)
		blocks = append(blocks, block)
	}
	if validLinks == 0 {
		return ""
	}
	return strings.Join(blocks, "\n\n")
}

// assembleFakePost собирает пост: заголовок обычного дайджеста + тело в пределах
// лимита TG. Маркеров выдуманности нет — выпуск неотличим от обычного.
func assembleFakePost(body string) string {
	budget := fakePostMaxUTF16 - md.UTF16Len(digestTitle) - 2
	return digestTitle + "\n\n" + capFakeBody(body, budget)
}

// capFakeBody ужимает тело до budget UTF-16: блоки копятся целиком ("\n\n" = 2), блок,
// который не влезает, обрывает накопление («…» хвостом). Первый блок-переросток режется
// по рунам.
func capFakeBody(body string, budget int) string {
	var kept []string
	n := 0
	for _, block := range strings.Split(body, "\n\n") {
		cost := md.UTF16Len(block)
		if len(kept) == 0 {
			if cost > budget {
				return truncateRunes(block, budget-1) + "…"
			}
			kept = append(kept, block)
			n = cost
			continue
		}
		if n+2+cost > budget {
			joined := strings.Join(kept, "\n\n")
			if n+1 > budget { // «…» не влезает — без неё, инвариант ≤ budget важнее
				return joined
			}
			return joined + "…"
		}
		kept = append(kept, block)
		n += 2 + cost
	}
	return strings.Join(kept, "\n\n")
}

// truncateRunes режет строку по рунам до лимита UTF-16 code units (руна вне BMP = 2).
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		cost := md.UTF16Len(string(r))
		if n+cost > limit {
			break
		}
		b.WriteRune(r)
		n += cost
	}
	return b.String()
}
