package news

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

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

// fakeRubric — рубрика пятничного выпуска: тематический фокус + подсказки тона.
type fakeRubric struct {
	ID    string // стабильный идентификатор (в БД и логах)
	Title string // название для user-сообщения
	Brief string // 1–2 предложения: о чём выпуск (примеры тем)
	Tone  string // акцент тона для этой рубрики
}

// fakeRubrics — пул рубрик; якорь недели = fakeRubrics[ISOWeek%len] (stateless-ротация,
// 12 рубрик ≈ квартальный цикл без хранения состояния).
var fakeRubrics = []fakeRubric{
	{
		ID:    "unusual",
		Title: "Необычное применение PHP",
		Brief: "Несуществующие, но реалистичные кейсы PHP вне веба: станок с ЧПУ под управлением PHP-демона, метеостанция на Raspberry Pi, умный аквариум с автодоливом, телеметрия дрона, сервер для ретро-игр, касса в пекарне, автополив теплицы.",
		Tone:  "Техническая конкретика приборов и железа: датчики, порты, протоколы, миллисекунды; юмор — в невозмутимой серьёзности, с которой PHP берётся за задачу любого масштаба.",
	},
	{
		ID:    "releases",
		Title: "Релизы и RFC ядра",
		Brief: "Несуществующие релизы PHP и придуманные RFC: курьёзные строки changelog, предложения по ядру, голосования за синтаксис, депрекации древних фич, релиз-менеджеры-герои.",
		Tone:  "Сдержанный тон официальных релиз-ноутов; юмор — в абсолютно серьёзной мотивации заведомо странных изменений.",
	},
	{
		ID:    "legacy",
		Title: "Легаси-археология",
		Brief: "Находки в старом коде и PHP 4/5 в проде: слои эпох в одном файле, комментарии 2007 года, mysql_*, деплой по FTP, CMS ранних нулевых, которое всё ещё держит нагрузку.",
		Tone:  "Тон сдержанного уважения к выжившему; юмор — в контрасте эпох: код старше разработчика, который его чинит.",
	},
	{
		ID:    "composer",
		Title: "Composer и пакетный ад",
		Brief: "Зависимости и пакетный менеджмент: несуществующие пакеты с неожиданными транзитивными зависимостями, конфликты версий, чудеса автолоада, зеркала-призраки, lock-файл от разработчика, который уволился.",
		Tone:  "Тон усталого смирения перед деревом зависимостей; юмор — в гордости за распутанные 400 пакетов ради одной функции.",
	},
	{
		ID:    "typing",
		Title: "Типизация и сравнения",
		Brief: "Типы, строгие режимы и сравнения: == против ===, жонглирование типами, строка, которая оказалась не строкой, enum'ы и аннотации, вечные споры о generics.",
		Tone:  "Педантичная точность с примерами сравнений; юмор — в невозмутимой подаче неожиданных результатов.",
	},
	{
		ID:    "frameworks",
		Title: "Фреймворк-войны",
		Brief: "Несуществующие фреймворки-пародии (НЕ реальные имена): релизы, миграции с фреймворка на фреймворк, бенчмарки hello-world, обратная совместимость, документация, опаздывающая на релиз.",
		Tone:  "Тон военных сводок с фронтов чужих релизов; юмор — в серьёзной аналитике надуманных противостояний.",
	},
	{
		ID:    "interview",
		Title: "Собеседования и найм",
		Brief: "Придуманные задачи и кейсы с собеседований: прожарки, вопросы про замыкания на позицию в пекарню, тестовые на неделю, белые доски, зарплатные вилки и офферы.",
		Tone:  "Тон протокола диалога с рекрутером; юмор — в абсурдно завышенных требованиях к простым ролям.",
	},
	{
		ID:    "perf",
		Title: "Производительность и бенчмарки",
		Brief: "Оптимизации и измерения: отжатые опсекунды, кэш поверх кеша, микробенчмарки foreach vs array_map, тюнинг OPcache, экономия памяти на строках.",
		Tone:  "Тон инженерного отчёта с измерениями; юмор — в гордости за сэкономленные 12 мс ценой двух недель работы.",
	},
	{
		ID:    "security",
		Title: "Security-зоопарк",
		Brief: "Несуществующие CVE и advisory: странные векторы атак, патчи безопасности, ответственные раскрытия, эскалации через php.ini, санитайзеры и обфускация.",
		Tone:  "Тон срочного бюллетеня безопасности; юмор — в серьёзном CVSS-скоринге ничтожной уязвимости.",
	},
	{
		ID:    "tooling",
		Title: "Инструменты и CI",
		Brief: "Линтеры, статанализ и CI: несуществующие анализаторы, драконовские правила кодстайла, пайплайны, собирающиеся дольше, чем живёт проект, все фиксы одним PR.",
		Tone:  "Тон лендинга девтула с обещаниями; юмор — в машинной уверенности линтера, который прав всегда.",
	},
	{
		ID:    "tests",
		Title: "Тесты и QA",
		Brief: "Несуществующие истории о тестах: coverage 100% при нулевых проверках, мок, мокающий мок, флакающий тест, который чинят перезапуском, снапшот на 40000 строк, тест, написанный под уже сломанный код.",
		Tone:  "Тон уверенного QA-отчёта; юмор — в самоуспокоенности метриками при полном отсутствии проверок.",
	},
	{
		ID:    "ai",
		Title: "ИИ-ассистенты в коде",
		Brief: "Несуществующие истории об ИИ-помощниках в PHP: ассистент, уверенно импортирующий пакеты, которых нет, рефакторинг на 2000 строк за минуту и месяц проверки, ИИ-ревьюер, ставящий LGTM всем, агент, который сам открыл PR и сам его смёржил.",
		Tone:  "Тон восторженного пресс-релиза; юмор — в полной передаче ответственности машине.",
	},
}

// fakeExtraOffsets — смещения ISO-недели для дополнительных рубрик гибрида «2+2».
// Инвариант: 3, 7 и разность 4 не кратны 12 → якорь и обе дополнительные — всегда
// три РАЗНЫЕ рубрики при любом w.
var fakeExtraOffsets = [2]int{3, 7}

// fakePlan — план гибридного выпуска: якорь недели (2 статьи) + две дополнительные (1–2 статьи).
type fakePlan struct {
	Anchor fakeRubric   // = fakeRubrics[w%12] — обратная совместимость ротации
	Extras []fakeRubric // ровно 2: fakeRubrics[(w+3)%12], fakeRubrics[(w+7)%12], в порядке смещений
}

// pickFakePlan — план выпуска детерминированно по ISO-номеру недели (stateless).
func pickFakePlan(t time.Time) fakePlan {
	_, w := t.ISOWeek()
	return fakePlan{
		Anchor: fakeRubrics[w%len(fakeRubrics)],
		Extras: []fakeRubric{
			fakeRubrics[(w+fakeExtraOffsets[0])%len(fakeRubrics)],
			fakeRubrics[(w+fakeExtraOffsets[1])%len(fakeRubrics)],
		},
	}
}

// fakeRubricKey — ключ плана в news_fake_posts.rubric (TEXT, без миграции):
// "anchor+extra1+extra2", якорь первым. Колонку никто не парсит (бан-лист работает
// по body через extractFakeHeadlines) — это наблюдаемость ротации в БД/логах;
// старые строки с одним ID остаются валидной историей якорей.
func fakeRubricKey(p fakePlan) string {
	ids := make([]string, 0, len(p.Extras)+1)
	ids = append(ids, p.Anchor.ID)
	for _, r := range p.Extras {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, "+")
}

const fakeMemoryIssues = 24       // сколько прошлых выпусков помним (2 цикла ротации 12 рубрик)
const fakeRecentHeadlines = 168   // кап ban-листа тем по количеству
const fakeRecentTotalRunes = 6000 // кап ban-листа тем по суммарной длине (бюджет user-сообщения)
const fakeHeadlineMaxRunes = 120  // кап одного заголовка бан-листа

// mdTitleLinkRe — md-ссылка с захватом текста (mdLinkRe берёт только URL).
var mdTitleLinkRe = regexp.MustCompile(`\[([^\[\]]+)\]\((https?://[^)\s]+)\)`)

// extractFakeHeadlines — тексты md-ссылок (заголовки статей/имена пакетов) из тел
// прошлых выпусков: whitespace/control-символы схлопнуты в один пробел (текст из LLM
// не может подделать структуру user-сообщения переносами строк), каждый заголовок
// капится по fakeHeadlineMaxRunes, весь список — по fakeRecentHeadlines и суммарной
// длине fakeRecentTotalRunes; дедуп с сохранением порядка первого вхождения.
func extractFakeHeadlines(bodies []string) []string {
	seen := make(map[string]struct{})
	var out []string
	n := 0
	for _, body := range bodies {
		for _, m := range mdTitleLinkRe.FindAllStringSubmatch(body, -1) {
			h := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) || unicode.IsControl(r) {
					return ' '
				}
				return r
			}, m[1])
			h = strings.Join(strings.Fields(h), " ")
			if h == "" {
				continue
			}
			if r := []rune(h); len(r) > fakeHeadlineMaxRunes {
				h = string(r[:fakeHeadlineMaxRunes])
			}
			if _, ok := seen[h]; ok {
				continue
			}
			seen[h] = struct{}{}
			out = append(out, h)
			n += len([]rune(h))
			if len(out) >= fakeRecentHeadlines || n > fakeRecentTotalRunes {
				return out
			}
		}
	}
	return out
}

// bodiesOf — тела прошлых выпусков из строк БД (для бан-листа тем).
func bodiesOf(rows []FakeRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Body)
	}
	return out
}

// buildFakeUserMessage — user-сообщение: дата, распределение статей по рубрикам
// плана, ban-лист тем. Динамический контент только здесь (промпт статичен —
// анти-инъекция).
func buildFakeUserMessage(t time.Time, plan fakePlan, recent []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Выпуск от %s.\n\n", t.Format("02.01.2006"))
	b.WriteString("Распределение статей по рубрикам:\n")
	fmt.Fprintf(&b, "- Статьи 1 и 2 — якорная рубрика выпуска «%s»: %s Тон: %s.\n",
		plan.Anchor.Title, plan.Anchor.Brief, plan.Anchor.Tone)
	fmt.Fprintf(&b, "- Статья 3 — рубрика «%s»: %s Тон: %s.\n",
		plan.Extras[0].Title, plan.Extras[0].Brief, plan.Extras[0].Tone)
	fmt.Fprintf(&b, "- Статья 4 (если она есть) — рубрика «%s»: %s Тон: %s.\n",
		plan.Extras[1].Title, plan.Extras[1].Brief, plan.Extras[1].Tone)
	b.WriteString("\nПакеты — без привязки к рубрикам: любая тема PHP-экосистемы.\n")
	if len(recent) > 0 {
		b.WriteString("\nТемы прошлых выпусков — НЕ повторяй их и близкие вариации:\n")
		for _, h := range recent {
			b.WriteString("- " + h + "\n")
		}
	}
	return b.String()
}

// PostFake генерирует и постит «пятничный выпуск» — шуточный полностью вымышленный
// дайджест по гибридному плану «2+2»: якорная рубрика недели (stateless-ротация по
// ISO-номеру недели) + две дополнительные рубрики, с памятью тем прошлых выпусков
// (news_fake_posts) как ban-листом. Ручной запуск (/news fake) и пятничный cron-слот
// (fallback на обычный Post при ошибке — в вызывающем коде).
func (d *Digester) PostFake(ctx context.Context, chatID int64) error {
	system := prompts.Get(prompts.FakeNews)
	if system == "" {
		return fmt.Errorf("fake news: empty prompt %s", prompts.FakeNews)
	}
	now := time.Now()
	plan := pickFakePlan(now)
	past, err := d.repo.ListFake(ctx, chatID, fakeMemoryIssues)
	if err != nil {
		slog.Warn("fake news memory read", "err", err)
	}
	user := buildFakeUserMessage(now, plan, extractFakeHeadlines(bodiesOf(past)))
	resp, _, _, err := d.llmFake.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
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
	// Память только после успешного поста: err не возвращаем — cron-fallback иначе
	// запостил бы обычный дайджест поверх уже вышедшего фейка. Сохраняем тело после
	// того же капа, что видит чат (память ≡ опубликованное), под свежим контекстом —
	// ctx ручного вызова мог почти истечь к моменту записи.
	ctxSave, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.repo.SaveFake(ctxSave, chatID, fakeRubricKey(plan), capFakeBody(body, fakeBodyBudget())); err != nil {
		slog.Warn("fake news memory write", "err", err)
	}
	return nil
}

// sanitizeFakeBody чистит ответ LLM по блокам (разделитель "\n\n"): блок дропается, если
// содержит markdown-ссылку с чужим доменом, голый URL вне markdown-ссылки или МЕТА-маркер
// выдуманности. Блок без ссылок проходит ТОЛЬКО как точный литерал заголовка секции
// пакетов (packagesSectionHeader) — на творческой температуре модель тянет вступления/
// заключения/комментарии, которых в обычном выпуске не бывает. Не осталось ни одной
// валидной ссылки → "".
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
		// Блок без ссылок — только заголовок секции пакетов.
		if len(mds) == 0 && block != packagesSectionHeader {
			ok = false
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

// fakeBodyBudget — бюджет UTF-16 на тело поста (лимит TG минус шапка и разделитель).
func fakeBodyBudget() int { return fakePostMaxUTF16 - md.UTF16Len(digestTitle) - 2 }

// assembleFakePost собирает пост: заголовок обычного дайджеста + тело в пределах
// лимита TG. Маркеров выдуманности нет — выпуск неотличим от обычного.
func assembleFakePost(body string) string {
	return digestTitle + "\n\n" + capFakeBody(body, fakeBodyBudget())
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
