package oncall

import (
	"reflect"
	"testing"
)

func TestParsedPlans(t *testing.T) {
	cases := []struct {
		name  string
		plans []byte
		want  []Plan
	}{
		{"nil_bytes", nil, nil},
		{"empty_bytes", []byte{}, nil},
		{"invalid_json", []byte(`не json`), nil},
		{"json_null", []byte(`null`), nil},
		{"empty_array", []byte(`[]`), []Plan{}},
		{"valid_plans", []byte(`[{"title":"Наблюдение","summary":"ждём"},{"title":"Откат","summary":"назад"}]`),
			[]Plan{{Title: "Наблюдение", Summary: "ждём"}, {Title: "Откат", Summary: "назад"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := RoundRow{Plans: c.plans}
			got := row.ParsedPlans()
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: ParsedPlans() = %+v, want %+v", c.name, got, c.want)
			}
		})
	}
}
