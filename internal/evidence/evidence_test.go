package evidence

import (
	"sort"
	"strings"
	"testing"

	"agentrm/internal/model"
)

func TestTouchedBy(t *testing.T) {
	res := map[string]*model.Resource{
		"kind:obx":     {ID: "kind:obx", Kind: model.RKCluster, Containers: []string{"obx-control-plane"}, Ports: []string{"61234"}},
		"compose:shop": {ID: "compose:shop", Kind: model.RKContainers, Containers: []string{"shop-db-1"}, Ports: []string{"5432"}},
		"srv:42":       {ID: "srv:42", Kind: model.RKServer, PID: 42, Ports: []string{"3000"}},
		"sim:ABC-123":  {ID: "sim:ABC-123", Kind: model.RKSimulator, UDID: "ABC-123", Name: "iPhone 16"},
		"gradle:7":     {ID: "gradle:7", Kind: model.RKGradle, PID: 7},
		"db:postgres":  {ID: "db:postgres", Kind: model.RKDatabase},
		"model:ollama": {ID: "model:ollama", Kind: model.RKModel},
	}
	cases := []struct {
		cmd, cwd, ctx string
		want          []string
	}{
		{"kubectl --context kind-obx get pods", "", "", []string{"kind:obx"}},
		{"kubectl get pods -A", "", "kind-obx", []string{"kind:obx"}},
		{"kubectl get pods -A", "", "prod-cluster", nil},
		{"kubectl --context prod get pods", "", "kind-obx", nil},
		{"helm --kube-context kind-obx list", "", "", []string{"kind:obx"}},
		{"kind export logs --name obx", "", "", []string{"kind:obx"}},
		{"docker exec -it shop-db-1 psql", "", "", []string{"compose:shop"}}, // psql runs in the container, not the Homebrew one
		{"docker compose -p shop logs", "", "", []string{"compose:shop"}},
		{"docker compose logs api", "/code/shop", "", []string{"compose:shop"}},
		{"curl -s http://localhost:3000/health", "", "", []string{"srv:42"}},
		{"curl -s http://localhost:30001/health", "", "", nil},
		{"xcrun simctl install booted App.app", "", "", []string{"sim:ABC-123"}},
		{"./gradlew assembleDebug", "", "", []string{"gradle:7"}},
		{"psql -h localhost mydb", "", "", []string{"db:postgres"}},
		{"ollama run llama3", "", "", []string{"model:ollama"}},
		{`echo "kubectl --context kind-obx"`, "", "", nil},
		{`grep kind-obx ~/.kube/config`, "", "", nil},
		{`agentrm claim kind:obx --purpose "kubectl --context kind-obx"`, "", "", nil},
		{"echo go; kubectl --context kind-obx get pods", "", "", []string{"kind:obx"}},
		{"git status", "", "kind-obx", nil},
	}
	for _, c := range cases {
		got := TouchedBy(c.cmd, c.cwd, res, c.ctx)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("TouchedBy(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestActivityLearnsBackgroundTraffic(t *testing.T) {
	a := NewActivity()
	r := &model.Resource{ID: "kind:x", NetBytes: 1e6}
	if a.Active(r, nil) {
		t.Fatal("the first sample counted as activity")
	}
	// Steady background chatter of 100 KB per sweep is learned, not counted.
	for i := 0; i < 10; i++ {
		r.NetBytes += 100 * 1024
		if a.Active(r, nil) {
			t.Fatalf("background chatter counted as use at sample %d", i)
		}
	}
	r.NetBytes += 5 * 1024 * 1024
	if !a.Active(r, nil) {
		t.Fatal("a 5 MB burst over background was not seen as use")
	}
	if !a.Active(&model.Resource{ID: "srv", Ports: []string{"3000"}}, map[string]bool{"3000": true}) {
		t.Fatal("an established connection was not seen as use")
	}
}
