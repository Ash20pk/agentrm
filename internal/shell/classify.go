package shell

import (
	"regexp"
	"strings"

	"agentrm/internal/model"
)

type classifier struct {
	kind string
	re   *regexp.Regexp
}

// Heavy commands and the lease kind each needs. Matched against Active text, one command per
// line, so `^` means the start of a command.
var builtin = []classifier{
	{model.KCluster, regexp.MustCompile(`(?m)^(kind\s+create\s+cluster|minikube\s+start|k3d\s+cluster\s+create|colima\s+start|limactl\s+start|orb\s+start)\b`)},
	{model.KCompose, regexp.MustCompile(`(?m)^(docker[- ]compose\b.*\bup\b|docker\s+compose\b.*\bup\b|supabase\s+start\b)`)},
	{model.KDockerBuild, regexp.MustCompile(`(?m)^(docker\s+(buildx\s+)?build|podman\s+build|nerdctl\s+build)\b`)},
	{model.KCargoBuild, regexp.MustCompile(`(?m)^cargo\s+(build|test|clippy|run|nextest|bench|install)\b`)},
	{model.KGradle, regexp.MustCompile(`(?m)^(\S*gradlew|gradle)\s+\S`)},
	{model.KXcodeBuild, regexp.MustCompile(`(?m)^(xcodebuild|swift\s+build|swift\s+test)\b`)},
	{model.KSimulator, regexp.MustCompile(`(?m)^(\S*simctl\s+boot|xcrun\s+simctl\s+boot|open\s+-a\s+Simulator|flutter\s+run|expo\s+run:ios|npx\s+expo\s+run:ios|react-native\s+run-ios|npx\s+react-native\s+run-ios)\b`)},
	{model.KEmulator, regexp.MustCompile(`(?m)^(\S*emulator\s+(-avd|@)|expo\s+run:android|npx\s+expo\s+run:android|react-native\s+run-android|npx\s+react-native\s+run-android)`)},
	{model.KVM, regexp.MustCompile(`(?m)^(utmctl\s+start|qemu-system-\S+|vagrant\s+up|multipass\s+(launch|start)|tart\s+run)\b`)},
	{model.KModel, regexp.MustCompile(`(?m)^(ollama\s+(serve|run)|llama-server|llama-cli|mlx_lm\.server|lms\s+(server\s+start|load)|vllm\s+serve)\b`)},
	{model.KLoadtest, regexp.MustCompile(`(?m)^(k6\s+run|vegeta\s+attack|locust|hey\s+-|wrk\s|ab\s+-n|artillery\s+run)`)},
	{model.KBrowser, regexp.MustCompile(`(?m)^(npx\s+playwright\s+test|playwright\s+test|npx\s+cypress\s+run|cypress\s+run|npx\s+puppeteer)\b`)},
	{model.KServer, regexp.MustCompile(`(?m)^((npm|pnpm|yarn|bun)\s+(run\s+)?(dev|start|start:dev|serve)\b|next\s+dev\b|npx\s+next\s+dev\b|vite(\s+dev)?\s*$|npx\s+vite\b|uvicorn\s|flask\s+run\b|rails\s+s(erver)?\b|go\s+run\b|python3?\s+manage\.py\s+runserver|brew\s+services\s+(start|run)\b)`)},
}

// Classifier decides which lease kind, if any, a command line needs.
type Classifier struct{ extra []classifier }

// NewClassifier adds the user's extra patterns (config extra_patterns) ahead of the built-ins.
// Invalid patterns are skipped and returned, so the caller can report them.
func NewClassifier(extra map[string][]string) (*Classifier, []string) {
	c := &Classifier{}
	var bad []string
	for kind, pats := range extra {
		for _, p := range pats {
			re, err := regexp.Compile("(?m)" + p)
			if err != nil {
				bad = append(bad, p)
				continue
			}
			c.extra = append(c.extra, classifier{kind, re})
		}
	}
	return c, bad
}

// Classify returns the lease kind cmd needs, or "". The user's patterns see the commands as
// typed; the built-in ones see programs by base name.
func (c *Classifier) Classify(cmd string) string {
	raw := ActiveRaw(cmd)
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	if c != nil {
		for _, x := range c.extra {
			if x.re.MatchString(raw) {
				return x.kind
			}
		}
	}
	active := normalize(raw)
	for _, x := range builtin {
		if x.re.MatchString(active) {
			return x.kind
		}
	}
	return ""
}

// Classify with the built-in patterns only.
func Classify(cmd string) string { return (*Classifier)(nil).Classify(cmd) }
