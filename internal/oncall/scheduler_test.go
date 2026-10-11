package oncall

import (
	"context"
	"testing"
	"time"
)

// newNoopEngine — движок с пустым списком чатов: Tick проходит цикл и ничего не
// дёргает (ни БД, ни Telegram), поэтому шедулер можно гонять в unit-тесте.
func newNoopEngine() *Engine {
	return NewEngine(&Service{}, NewRepository(nil), nil, nil)
}

func TestSchedulerSpecValidation(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr bool
	}{
		{"five_field_valid", "*/5 * * * *", false},
		{"every_descriptor_valid", "@every 1h", false},
		{"garbage_invalid", "not-a-cron-spec", true},
		{"empty_invalid", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewScheduler(newNoopEngine())
			err := s.Start(context.Background(), c.spec)
			if (err != nil) != c.wantErr {
				t.Errorf("%s: Start(%q) err = %v, wantErr %v", c.name, c.spec, err, c.wantErr)
			}
			s.Stop()
		})
	}
}

func TestSchedulerTicksWithoutPanic(t *testing.T) {
	s := NewScheduler(newNoopEngine())
	if err := s.Start(context.Background(), "@every 100ms"); err != nil {
		t.Fatalf("Start(@every 100ms) = %v, want nil", err)
	}
	time.Sleep(350 * time.Millisecond)
	s.Stop()
}

func TestSchedulerStopBeforeStart(t *testing.T) {
	s := NewScheduler(newNoopEngine())
	s.Stop()
}
