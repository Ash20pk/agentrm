package shell

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"kind create cluster --name x":                 "cluster",
		"/opt/homebrew/bin/kind create cluster --name": "cluster",
		"docker compose -p myapp up -d":                "compose",
		"supabase start":                               "compose",
		"docker build -t img .":                        "docker-build",
		"docker buildx build --platform x .":           "docker-build",
		"cd middleware && cargo test --all":            "cargo-build",
		"./gradlew assembleDebug":                      "gradle",
		"xcrun simctl boot ABC":                        "simulator",
		"xcodebuild -scheme App":                       "xcode-build",
		"k6 run load.js":                               "loadtest",
		"pnpm dev":                                     "server",
		"yarn start:dev":                               "server",
		"go run ./cmd/core":                            "server",
		"ollama serve":                                 "model",
		"emulator -avd Pixel_8":                        "emulator",
		"npx playwright test":                          "browser",
		"limactl start default":                        "cluster",
		"git status":                                   "",
		"docker ps":                                    "",
		"cargo fmt":                                    "",
		"ls ~/.gradle":                                 "",
		"grep -r 'kind create cluster' docs":           "",
		`echo "kind create cluster"`:                   "",
		`git commit -m "docker build fix"`:             "",
		`grep -r "cargo build" docs | head`:            "",
		`cd svc && docker build -t x .`:                "docker-build",
		`FOO=1 sudo -E docker build .`:                 "docker-build",
		`nice -n 10 cargo build`:                       "cargo-build",
		`timeout 600 cargo test`:                       "cargo-build",
		`echo start; cargo test --all`:                 "cargo-build",
		`agentrm acquire --kind cluster --purpose "kind create cluster"`: "",
		`X=$(docker build -q .)`:          "docker-build",
		`echo "$(docker build -q .)"`:     "docker-build",
		`bash -c "cargo build --release"`: "cargo-build",
		`sh -c 'kind create cluster'`:     "cluster",
		`eval "cargo test"`:               "cargo-build",
		"# cargo build\nls":               "",
		"cat > notes.md <<'EOF'\nkind create cluster --name demo\ndocker build -t x .\nEOF\ngo test ./...": "",
		"python3 - <<EOF\nprint('cargo build')\nEOF":                                                       "",
		"bash <<EOF\ndocker build -t x .\nEOF":                                                             "docker-build",
		"cat > f <<EOF\nhello\nEOF\ndocker build .":                                                        "docker-build",
		`echo "unclosed quote; cargo build`:                                                                "", // falls back, still inert
	}
	for cmd, want := range cases {
		if got := Classify(cmd); got != want {
			t.Errorf("Classify(%q) = %q, want %q (active: %q)", cmd, got, want, Active(cmd))
		}
	}
}

func TestExtraPatterns(t *testing.T) {
	c, bad := NewClassifier(map[string][]string{"compose": {`^\./stack\.sh\s+up`}, "server": {"("}})
	if len(bad) != 1 {
		t.Errorf("invalid pattern not reported: %v", bad)
	}
	if got := c.Classify("./stack.sh up"); got != "compose" {
		t.Errorf("extra pattern not used: %q", got)
	}
	if got := c.Classify(`echo ./stack.sh up`); got != "" {
		t.Errorf("extra pattern matched an inert mention: %q", got)
	}
}

func TestActiveNeverPanics(t *testing.T) {
	for _, s := range []string{"", "(", "$(", "<<", "'", `"`, "a | | b", "for x in; do", "\x00", "case x in", "<<EOF"} {
		_ = Active(s)
		_ = Classify(s)
	}
}
