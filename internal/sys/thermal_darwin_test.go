package sys

import "testing"

func TestThermalLevelReads(t *testing.T) {
	l := ThermalLevel()
	if l < 0 || l > 4 {
		t.Fatalf("thermal level %d: not read from notify(3)", l)
	}
	if again := ThermalLevel(); again < 0 {
		t.Fatalf("second read failed: %d", again)
	}
}
