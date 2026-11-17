package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"agentrm/internal/manager"
	"agentrm/internal/model"
	"agentrm/internal/store"
	"agentrm/internal/trace"
)

// runReplay replays one policy, or compares AgentRM with the reaper, over a trace.
func runReplay(cmd, path, policy, configFile string, asJSON bool) error {
	if path == "" {
		path = trace.Dir(store.Dir())
	}
	var cfg *model.Config
	src := configFile
	if src == "" {
		src = filepath.Join(store.Dir(), "config.json")
	}
	if b, err := os.ReadFile(src); err == nil {
		c := model.DefaultConfig()
		if err := json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		cfg = &c
	} else if configFile != "" {
		return err
	}
	policies := []string{policy}
	if cmd == "compare" {
		policies = []string{"agentrm", "reaper"}
	}
	var reports []*manager.ReplayReport
	for _, p := range policies {
		r, err := manager.Replay(path, manager.ReplayOptions{Policy: p, Config: cfg})
		if err != nil {
			return err
		}
		reports = append(reports, r)
	}
	if asJSON {
		b, _ := json.MarshalIndent(reports, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	for _, r := range reports {
		fmt.Print(r.String())
	}
	return nil
}
