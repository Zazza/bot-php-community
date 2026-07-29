package importer

import (
	"encoding/json"
	"testing"
	"time"
)

// TestFlattenText — поле text у TG Desktop бывает строкой или массивом сегментов.
func TestFlattenText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain_string", `"привет"`, "привет"},
		{"array_of_strings", `["a","b","c"]`, "abc"},
		{"array_of_objects", `[{"type":"mention","text":"@x"},{"type":"text","text":" y"}]`, "@x y"},
		{"mixed_array", `["a",{"text":"b"},"c"]`, "abc"},
		{"empty_string", `""`, ""},
		{"empty_array", `[]`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := flattenText(json.RawMessage(c.raw))
			if got != c.want {
				t.Errorf("flattenText(%s) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestParseUserID — from_id бывает "user#123", "user123" или пустым.
func TestParseUserID(t *testing.T) {
	cases := map[string]int64{
		"user#123":    123,
		"user456":     456,
		"":            0,
		"channel#789": 789,
		"junk":        0,
	}
	for in, want := range cases {
		if got := parseUserID(in); got != want {
			t.Errorf("parseUserID(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestParse — полный разбор экспорта: service и пустые тексты пропускаются,
// текст-массив склеивается, from_id→user_id.
func TestParse(t *testing.T) {
	const chatID = int64(-1003718002488)
	data := []byte(`{
		"name": "Семейство",
		"type": "private_supergroup",
		"id": -1003718002488,
		"messages": [
			{"id":1,"type":"service","date":"2024-01-10T10:00:00","from":"Alice","text":""},
			{"id":2,"type":"message","date":"2024-01-10T10:01:00","from":"Alice","from_id":"user#111","text":"Привет"},
			{"id":3,"type":"message","date":"2024-01-10T10:02:00","from":"Bob","from_id":"user222",
			 "text":[{"type":"mention","text":"@Alice"},{"type":"text","text":" как дела?"}]},
			{"id":4,"type":"message","date":"2024-01-10T10:03:00","from":"Carol","from_id":"user333","text":""},
			{"id":5,"type":"message","date":"2024-01-10T10:04:00","from":"Anon","text":"без from_id"}
		]
	}`)
	msgs, err := Parse(data, chatID)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3 (service + empty text skipped)", len(msgs))
	}
	want2 := struct{ text string; user int64; name string }{"Привет", 111, "Alice"}
	if msgs[0].ID != 2 || msgs[0].Text != want2.text || msgs[0].UserID != want2.user || msgs[0].Username != want2.name {
		t.Errorf("msg[0] = %+v, want text=%q user=%d name=%q", msgs[0], want2.text, want2.user, want2.name)
	}
	if msgs[1].ID != 3 || msgs[1].Text != "@Alice как дела?" || msgs[1].UserID != 222 {
		t.Errorf("msg[1] = %+v, want array-text flattened to %q, user=222", msgs[1], "@Alice как дела?")
	}
	if msgs[2].ID != 5 || msgs[2].UserID != 0 {
		t.Errorf("msg[2] = %+v, want id=5 user=0 (no from_id)", msgs[2])
	}
	if !msgs[0].TS.Equal(time.Date(2024, 1, 10, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("msg[0].TS = %v, want 2024-01-10T10:01:00Z", msgs[0].TS)
	}
	if msgs[0].ChatID != chatID {
		t.Errorf("ChatID = %d, want %d", msgs[0].ChatID, chatID)
	}
}
