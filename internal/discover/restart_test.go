package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentrm/internal/model"
)

func TestRestartLeftoverRunsWhereItRan(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("AGENTRM_HOME", home)
	out := filepath.Join(work, "out")
	r := &model.Resource{ID: "left:1", Kind: model.RKLeftover, Cwd: work,
		RestartArgv: []string{"sh", "-c", `echo "$GREETING $(pwd)" > out`},
		RestartEnv:  []string{"PATH=/bin:/usr/bin", "GREETING=hello"}}
	if err := Restore(r); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for i := 0; i < 50 && len(b) == 0; i++ {
		time.Sleep(50 * time.Millisecond)
		b, _ = os.ReadFile(out)
	}
	want, _ := filepath.EvalSymlinks(work)
	if got := strings.TrimSpace(string(b)); got != "hello "+want && got != "hello "+work {
		t.Fatalf("restarted with the wrong env or directory: %q", got)
	}
	if err := Restore(&model.Resource{ID: "left:2", Kind: model.RKLeftover}); err == nil {
		t.Fatal("a leftover with no recorded command claimed to restart")
	}
}
