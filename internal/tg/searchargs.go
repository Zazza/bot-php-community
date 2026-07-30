package tg

import "strings"

// parseSearchArgs разбирает аргументы /search: <запрос> [@user] [период].
// @-токены идут в username (последний выигрывает), последний query-токен
// проверяется как период; если распознан и не "all" — применяется и убирается
// из запроса. Чистая функция — без побочных эффектов, легко тестировать.
func parseSearchArgs(args string) (query, username string, p period) {
	p, _ = parsePeriod("")
	tokens := strings.Fields(args)
	var queryToks []string
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "@") {
			username = tok[1:]
			continue
		}
		queryToks = append(queryToks, tok)
	}
	if len(queryToks) > 0 {
		last := queryToks[len(queryToks)-1]
		if last != "" && last != "all" {
			if np, err := parsePeriod(last); err == nil {
				p = np
				queryToks = queryToks[:len(queryToks)-1]
			}
		}
	}
	query = strings.Join(queryToks, " ")
	return query, username, p
}
