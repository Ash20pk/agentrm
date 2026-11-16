package main

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"kind create cluster --name x":            "cluster",
		"docker compose -p openbox-local up -d":   "compose",
		"supabase start":                          "compose",
		"docker build -t img .":                   "docker-build",
		"docker buildx build --platform x .":      "docker-build",
		"cd middleware && cargo test --all":       "cargo-build",
		"./gradlew assembleDebug":                 "gradle",
		"xcrun simctl boot ABC":                   "simulator",
		"xcodebuild -scheme App":                  "xcode-build",
		"k6 run load.js":                          "loadtest",
		"pnpm dev":                                "server",
		"yarn start:dev":                          "server",
		"go run ./cmd/core":                       "server",
		"git status":                              "",
		"docker ps":                               "",
		"cargo fmt":                               "",
		"ls ~/.gradle":                            "",
		"grep -r 'kind create cluster' docs":      "cluster", // conservative: text mentions count
	}
	for cmd, want := range cases {
		if got := classify(cmd); got != want {
			t.Errorf("classify(%q) = %q, want %q", cmd, got, want)
		}
	}
}
