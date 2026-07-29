// Package prompts загружает системные промпты из встроенных .txt-файлов.
// Промпты редактируются в internal/prompts/*.txt, встраиваются в бинарник через //go:embed.
package prompts

import (
	"embed"
	"fmt"
)

//go:embed *.txt
var fs embed.FS

// Get возвращает текст промпта по имени файла (без пути). Второй аргумент — args
// для fmt.Sprintf, если промпт содержит placeholder'ы (%s).
func Get(name string, args ...any) string {
	b, err := fs.ReadFile(name)
	if err != nil {
		// Промпты обязательны — отсутствие критическая ошибка. Но возвращаем
		// пустую строку, чтобы не ронять бота целиком; вызывающий код обработает.
		return ""
	}
	if len(args) == 0 {
		return string(b)
	}
	return fmt.Sprintf(string(b), args...)
}

// Имена файлов промптов.
const (
	Judge  = "judge.txt"
	Chat   = "chat.txt"
	Topic  = "topic.txt"
	Digest = "digest.txt"
	Safety = "safety.txt"
)
