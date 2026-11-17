package sys

import (
	"testing"
	"time"
)

func TestParseElapsed(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:05":      5 * time.Second,
		"12:34":      12*time.Minute + 34*time.Second,
		"01:02:03":   time.Hour + 2*time.Minute + 3*time.Second,
		"3-04:05:06": 3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second,
	} {
		if got, ok := ParseElapsed(in); !ok || got != want {
			t.Errorf("ParseElapsed(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseElapsed("garbage"); ok {
		t.Error("garbage parsed")
	}
}
