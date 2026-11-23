package main

import (
	"os/exec"
	"strings"
	"testing"

	"agentrm/internal/api"
)

// What reveal prints, evaluated by a shell, sets the variables and marks them as the vault's,
// adding to any marked before.
func TestRevealMarksItsVariables(t *testing.T) {
	var b strings.Builder
	printExports(&b, &api.VaultResp{Env: map[string]string{"MYSQL_PWD": "it's secret", "MYSQL_USER": "ash"}, Unset: []string{"AWS_PROFILE"}})
	out, err := exec.Command("sh", "-c", "AGENTRM_VAULT_VARS=OLD; "+b.String()+`printf '%s|%s|%s' "$MYSQL_PWD" "$AGENTRM_VAULT_VARS" "${AWS_PROFILE-unset}"`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if got := string(out); got != "it's secret|OLD,MYSQL_PWD,MYSQL_USER|unset" {
		t.Fatalf("got %q from:\n%s", got, b.String())
	}
}
