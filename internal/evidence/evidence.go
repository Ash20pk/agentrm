// Package evidence decides which resources a command uses and whether a resource shows
// activity: what the machine shows, as opposed to what an agent says.
package evidence

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentrm/internal/model"
	"agentrm/internal/shell"
)

var portCtx = `(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\]|host\.docker\.internal):`

// Handles returns the strings and patterns a command would contain if it used r.
func Handles(r *model.Resource) (subs []string, res []*regexp.Regexp) {
	for _, p := range r.Ports {
		res = append(res, regexp.MustCompile(portCtx+regexp.QuoteMeta(p)+`\b`))
		res = append(res, regexp.MustCompile(`(?:--port[= ]|PORT=|-p\s*)`+regexp.QuoteMeta(p)+`\b`))
	}
	subs = append(subs, r.Containers...)
	switch r.Kind {
	case model.RKCluster:
		name := strings.TrimPrefix(r.ID, "kind:")
		subs = append(subs, "kind-"+name, name+"-control-plane")
		res = append(res, regexp.MustCompile(`\bkind\b.*--name[= ]`+regexp.QuoteMeta(name)+`\b`))
	case model.RKContainers:
		if proj, ok := strings.CutPrefix(r.ID, "compose:"); ok {
			proj = strings.TrimPrefix(proj, "supabase-")
			res = append(res, regexp.MustCompile(`(?:-p|--project-name)[= ]`+regexp.QuoteMeta(proj)+`\b`))
		}
	case model.RKSimulator:
		subs = append(subs, r.UDID, `"`+r.Name+`"`, `'`+r.Name+`'`)
	case model.RKEmulator:
		if i := strings.Index(r.Detail, "AVD "); i >= 0 {
			subs = append(subs, "@"+r.Detail[i+4:], "-avd "+r.Detail[i+4:])
		}
		res = append(res, regexp.MustCompile(`(?m)^adb\b`))
	case model.RKVM:
		if n, ok := strings.CutPrefix(r.ID, "vm:lima-"); ok {
			res = append(res, regexp.MustCompile(`(?m)^(limactl\s+(shell|copy)\s+`+regexp.QuoteMeta(n)+`\b|lima\b|colima\b)`))
		}
	case model.RKModel:
		res = append(res, regexp.MustCompile(`(?m)^(ollama\s+(run|ps|pull)|llm\b)`))
	case model.RKDatabase:
		switch strings.TrimPrefix(r.ID, "db:") {
		case "postgres":
			res = append(res, regexp.MustCompile(`(?m)^(psql|pg_dump|pg_restore|createdb|dropdb)\b`))
		case "mysqld", "mariadbd":
			res = append(res, regexp.MustCompile(`(?m)^(mysql|mysqldump|mariadb)\b`))
		case "redis-server", "valkey-server":
			res = append(res, regexp.MustCompile(`(?m)^(redis-cli|valkey-cli)\b`))
		case "mongod":
			res = append(res, regexp.MustCompile(`(?m)^(mongosh|mongo)\b`))
		}
	}
	return subs, res
}

var kubeToolRe = regexp.MustCompile(`(?m)^(kubectl|helm|k9s|kubens|kubectx|stern|skaffold|tilt)\b`)
var kubeCtxFlagRe = regexp.MustCompile(`--(kube-)?context[= ]`)
var simAllRe = regexp.MustCompile(`(?m)^xcrun\s+simctl\s+\S+\s+booted\b|(?m)^simctl\s+\S+\s+booted\b`)
var composeRe = regexp.MustCompile(`(?m)^docker[- ]compose\b|(?m)^docker\s+compose\b`)
var projectFlagRe = regexp.MustCompile(`(?:-p|--project-name)[= ]`)

// CurrentKubeContext reads current-context from the default kubeconfig.
func CurrentKubeContext() string {
	p := os.Getenv("KUBECONFIG")
	if p == "" || strings.Contains(p, ":") {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, ".kube", "config")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "current-context:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// TouchedBy returns the resources a command (run in cwd) uses. Only commands that execute
// something count (see package shell).
func TouchedBy(cmd, cwd string, resources map[string]*model.Resource, kubeCtx string) []string {
	active := shell.Active(cmd)
	if strings.TrimSpace(active) == "" {
		return nil
	}
	usesCurrentCtx := kubeToolRe.MatchString(active) && !kubeCtxFlagRe.MatchString(active)
	isGradle := shell.Classify(cmd) == model.KGradle
	simAll := simAllRe.MatchString(active)
	composeHere := composeRe.MatchString(active) && !projectFlagRe.MatchString(active)
	var out []string
	for id, r := range resources {
		hit := false
		switch {
		case r.Kind == model.RKCluster && usesCurrentCtx && kubeCtx == "kind-"+strings.TrimPrefix(id, "kind:"):
			hit = true
		case r.Kind == model.RKGradle && isGradle:
			hit = true
		case r.Kind == model.RKSimulator && simAll:
			hit = true
		case r.Kind == model.RKContainers && composeHere && cwd != "" &&
			(id == "compose:"+strings.ToLower(filepath.Base(cwd)) || r.Cwd != "" && r.Cwd == cwd):
			hit = true
		}
		if !hit {
			subs, res := Handles(r)
			for _, s := range subs {
				if s != "" && s != `""` && s != `''` && strings.Contains(active, s) {
					hit = true
					break
				}
			}
			for _, re := range res {
				if !hit && re.MatchString(active) {
					hit = true
				}
			}
		}
		if hit {
			out = append(out, id)
		}
	}
	return out
}

// Activity tracks signs of use the sweeper can see without anyone running a command.
type Activity struct {
	// netBase is a per-resource moving average of network bytes per sweep while idle, so
	// background chatter (a cluster's own control plane) is learned rather than counted as use.
	netBase map[string]float64
	netLast map[string]float64
}

func NewActivity() *Activity {
	return &Activity{netBase: map[string]float64{}, netLast: map[string]float64{}}
}

// Active reports whether r showed use since the last sweep: an established connection on one
// of its ports, network traffic well above its own background, or CPU for a Gradle daemon or
// an Xcode helper.
func (a *Activity) Active(r *model.Resource, established map[string]bool) bool {
	for _, p := range r.Ports {
		if established[p] {
			return true
		}
	}
	if (r.Kind == model.RKGradle || r.Kind == model.RKXcodeHelper) && r.CPU > 5 {
		return true
	}
	if r.NetBytes > 0 {
		last, seen := a.netLast[r.ID]
		a.netLast[r.ID] = r.NetBytes
		if !seen || r.NetBytes < last {
			return false // first sample, or counters reset by a restart
		}
		delta := r.NetBytes - last
		base := a.netBase[r.ID]
		if delta > max(256*1024, 4*base) {
			return true
		}
		a.netBase[r.ID] = 0.8*base + 0.2*delta
	}
	return false
}

// Forget drops what is known about a resource that is gone.
func (a *Activity) Forget(id string) {
	delete(a.netBase, id)
	delete(a.netLast, id)
}
