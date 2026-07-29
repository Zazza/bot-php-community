// Package importer — разбор экспорта истории чата (Telegram Desktop JSON) в messages.Message.
package importer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"phpbot/internal/messages"
)

// Export — корневая структура messages.json из Telegram Desktop.
type Export struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Messages []ExportMessage `json:"messages"`
}

// ExportMessage — одно сообщение в формате TG Desktop.
type ExportMessage struct {
	ID     json.RawMessage `json:"id"`     // число (иногда строка в старых экспортах)
	Type   string          `json:"type"`   // "message" | "service"
	Date   string          `json:"date"`
	From   string          `json:"from"`   // отображаемое имя
	FromID string          `json:"from_id"` // "user#123" | "user123" | ""
	Text   json.RawMessage `json:"text"`   // string ИЛИ массив сегментов
}

var digitsRe = regexp.MustCompile(`\d+`)

// Parse разбирает JSON экспорта и возвращает текстовые сообщения для chatID.
// Сервисные сообщения и сообщения без текста пропускаются.
func Parse(data []byte, chatID int64) ([]messages.Message, error) {
	var ex Export
	if err := json.Unmarshal(data, &ex); err != nil {
		return nil, fmt.Errorf("parse export json: %w", err)
	}
	out := make([]messages.Message, 0, len(ex.Messages))
	for _, m := range ex.Messages {
		if m.Type != "message" {
			continue
		}
		text := strings.TrimSpace(flattenText(m.Text))
		if text == "" {
			continue
		}
		ts, err := parseDate(m.Date)
		if err != nil {
			continue // непарсимая дата — пропускаем сообщение, не валить импорт
		}
		out = append(out, messages.Message{
			ID:       parseInt64(m.ID),
			ChatID:   chatID,
			UserID:   parseUserID(m.FromID),
			Username: strings.TrimSpace(m.From),
			Text:     text,
			TS:       ts,
		})
	}
	return out, nil
}

// flattenText приводит поле text (string или массив сегментов) к plain text.
func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var segs []json.RawMessage
	if err := json.Unmarshal(raw, &segs); err != nil {
		return ""
	}
	var b strings.Builder
	for _, seg := range segs {
		var str string
		if err := json.Unmarshal(seg, &str); err == nil {
			b.WriteString(str)
			continue
		}
		var obj struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(seg, &obj); err == nil {
			b.WriteString(obj.Text)
		}
	}
	return b.String()
}

var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05.000000",
	"2006-01-02T15:04:05Z07:00",
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	for _, lay := range dateLayouts {
		if t, err := time.Parse(lay, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown date format: %q", s)
}

func parseUserID(fromID string) int64 {
	if fromID == "" {
		return 0
	}
	d := digitsRe.FindString(fromID)
	if d == "" {
		return 0
	}
	n, err := strconv.ParseInt(d, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseInt64 терпим к числу и к строке-числу (старые экспорты).
func parseInt64(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}
