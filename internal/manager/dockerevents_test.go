package manager

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"agentrm/internal/discover"
)

// Docker being off or absent is not something AgentRM failed to read; anything else is.
func TestDockerEventsProblem(t *testing.T) {
	notFound := &exec.Error{Name: "docker", Err: exec.ErrNotFound}
	for _, c := range []struct {
		err    error
		engine discover.EngineState
		want   string
	}{
		{nil, discover.EngineUp, ""},
		{errDockerEventsEnded, discover.EngineDown, ""}, // Docker was quit
		{notFound, discover.EngineUnknown, ""},          // not installed
		{fmt.Errorf("wrapped: %w", notFound), discover.EngineUnknown, ""},
		{errDockerEventsEnded, discover.EngineUp, string(errDockerEventsEnded)},
		{errors.New("permission denied"), discover.EngineHung, "permission denied"},
	} {
		if got := dockerEventsProblem(c.err, c.engine); got != c.want {
			t.Errorf("%v, engine %v: %q, want %q", c.err, c.engine, got, c.want)
		}
	}
}
