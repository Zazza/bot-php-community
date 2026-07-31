package quiz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"

	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
)

// errNoData — типа не хватило данных для 4 вариантов (оркестратор попробует другой тип).
var errNoData = errors.New("недостаточно данных для вопроса")

// seedTerms — термины из мира PHP/веб, по которым в истории наверняка есть обсуждения.
var seedTerms = []string{
	"yii3", "yii2", "laravel", "symfony", "composer", "phpunit", "psr",
	"docker", "pgvector", "postgres", "mysql", "redis", "nginx", "git",
	"php8", "swoole", "roadrunner", "amphp", "psalm", "phpstan",
	"xdebug", "monolog", "doctrine", "twig", "blade", "reactphp",
	"vue", "react", "typescript", "kubernetes", "websocket", "grpc",
	"graphql", "memcached", "gitlab", "rabbitmq", "elasticsearch",
}

// Generator собирает вопрос из истории чата. Корректность всегда из БД; LLM только
// сочиняет «обманки» для true/false-вопросов.
type Generator struct {
	msgs *messages.Repository
	llm  *llm.LLMClient
}

// Generate перебирает типы в случайном порядке и возвращает первый собравшийся.
func (g *Generator) Generate(ctx context.Context, chatID int64) (*Question, error) {
	kinds := []string{"whoTop", "whoFirst", "stat", "mentioned"}
	rand.Shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
	for _, k := range kinds {
		var (
			q   *Question
			err error
		)
		switch k {
		case "whoTop":
			q, err = g.genWhoTop(ctx, chatID)
		case "whoFirst":
			q, err = g.genWhoFirst(ctx, chatID)
		case "stat":
			q, err = g.genStat(ctx, chatID)
		case "mentioned":
			q, err = g.genMentioned(ctx, chatID)
		}
		if err == nil && q != nil {
			return q, nil
		}
	}
	return nil, fmt.Errorf("ни один тип не собрался (мало данных в чате)")
}

// genWhoTop — «Кто чаще всех говорил про X?» (топ по числу упоминаний).
func (g *Generator) genWhoTop(ctx context.Context, chatID int64) (*Question, error) {
	for _, t := range shuffledTerms() {
		rows, err := g.msgs.ExpertByKeyword(ctx, chatID, t, 5)
		if err != nil || len(rows) < 4 {
			continue
		}
		names := make([]string, 0, len(rows))
		for _, r := range rows {
			names = append(names, r.Username)
		}
		opts := topDistinct(names, 4)
		if len(opts) < 4 {
			continue
		}
		return newQuestion("whoTop", fmt.Sprintf("Кто чаще всех говорил в чате про «%s»?", t), opts, 0), nil
	}
	return nil, errNoData
}

// genWhoFirst — «Кто первым заговорил про X?» (минимальный ts упоминания).
func (g *Generator) genWhoFirst(ctx context.Context, chatID int64) (*Question, error) {
	for _, t := range shuffledTerms() {
		rows, err := g.msgs.FirstByKeyword(ctx, chatID, t, 5)
		if err != nil || len(rows) < 4 {
			continue
		}
		names := make([]string, 0, len(rows))
		for _, r := range rows {
			names = append(names, r.Username)
		}
		opts := topDistinct(names, 4)
		if len(opts) < 4 {
			continue
		}
		return newQuestion("whoFirst", fmt.Sprintf("Кто первым заговорил в чате про «%s»?", t), opts, 0), nil
	}
	return nil, errNoData
}

// genStat — «Кто самый ночной / кодер / длиннопис?» (лидерборд по критерию).
func (g *Generator) genStat(ctx context.Context, chatID int64) (*Question, error) {
	type spec struct {
		criterion, prompt string
	}
	specs := []spec{
		{"night", "Кто чаще всех пишет ночью (0–5 утра)?"},
		{"code", "Кто чаще всех пишет код?"},
		{"long", "Кто пишет самые длинные сообщения?"},
	}
	pi := rand.Perm(len(specs))
	for _, i := range pi {
		s := specs[i]
		rows, err := g.msgs.Leaderboard(ctx, chatID, s.criterion, 5)
		if err != nil || len(rows) < 4 {
			continue
		}
		names := make([]string, 0, len(rows))
		for _, r := range rows {
			names = append(names, r.Username)
		}
		opts := topDistinct(names, 4)
		if len(opts) < 4 {
			continue
		}
		return newQuestion("stat", s.prompt, opts, 0), nil
	}
	return nil, errNoData
}

// genMentioned — «Правда ли, что упоминали X?» (true/false). TRUE — редкий реальный термин
// (count 1..3); FALSE — несуществующий термин от LLM (Yii4 и т.п.) или реальный, но count==0.
func (g *Generator) genMentioned(ctx context.Context, chatID int64) (*Question, error) {
	// TRUE: редкий реальный термин.
	if rand.Intn(2) == 0 {
		for _, t := range shuffledTerms() {
			c, err := g.msgs.MentionCount(ctx, chatID, t)
			if err == nil && c >= 1 && c <= 3 {
				return &Question{
					Kind: "mentioned", Opts: []string{"Да", "Нет"}, Correct: 0,
					Prompt: fmt.Sprintf("Правда ли, что в чате упоминали «%s»?", t),
				}, nil
			}
		}
	}
	// FALSE: проверяем кандидатов (LLM-обманки + seed), оставляем count==0.
	cands := append(g.fakeTerms(ctx), shuffledTerms()...)
	for _, t := range cands {
		c, err := g.msgs.MentionCount(ctx, chatID, t)
		if err == nil && c == 0 {
			return &Question{
				Kind: "mentioned", Opts: []string{"Да", "Нет"}, Correct: 1,
				Prompt: fmt.Sprintf("Правда ли, что в чате упоминали «%s»?", t),
			}, nil
		}
	}
	return nil, errNoData
}

// fakeTerms просит LLM придумать правдоподобные несуществующие PHP-термины.
func (g *Generator) fakeTerms(ctx context.Context) []string {
	resp, _, _, err := g.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompts.Get(prompts.Quiz)},
		{Role: "user", Content: "Сгенерируй 12 правдоподобных несуществующих терминов."},
	})
	if err != nil {
		return nil
	}
	var terms []string
	if err := json.Unmarshal([]byte(extractJSONArray(resp)), &terms); err != nil {
		return nil
	}
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if len(t) >= 2 && len(t) <= 24 {
			out = append(out, t)
		}
	}
	return out
}

// newQuestion перемешивает варианты и проставляет индекс правильного.
func newQuestion(kind, prompt string, opts []string, correctIdx int) *Question {
	correctVal := opts[correctIdx]
	perm := rand.Perm(len(opts))
	out := make([]string, len(opts))
	for i, p := range perm {
		out[p] = opts[i]
	}
	ci := 0
	for i, o := range out {
		if o == correctVal {
			ci = i
			break
		}
	}
	return &Question{Kind: kind, Prompt: prompt, Opts: out, Correct: ci}
}

// topDistinct берёт первые n уникальных осмысленных имён (пропускает пустое и «user»).
func topDistinct(names []string, n int) []string {
	out := make([]string, 0, n)
	seen := map[string]bool{}
	for _, nm := range names {
		nm = strings.TrimSpace(nm)
		if nm == "" || nm == "user" || seen[nm] {
			continue
		}
		seen[nm] = true
		out = append(out, nm)
		if len(out) == n {
			break
		}
	}
	return out
}

func shuffledTerms() []string {
	out := append([]string(nil), seedTerms...)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// extractJSONArray вырезает первый JSON-массив из ответа LLM (может быть с пояснениями).
func extractJSONArray(s string) string {
	i := strings.IndexByte(s, '[')
	j := strings.LastIndexByte(s, ']')
	if i < 0 || j < 0 || j < i {
		return "[]"
	}
	return s[i : j+1]
}
