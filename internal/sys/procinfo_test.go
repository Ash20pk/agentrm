package sys

import "testing"

// A vault password under a name that does not look secret (DB_PASS, MYSQL_PWD) is still never
// kept for a restart: `agentrm vault run` names what it set, in any order.
func TestVaultVarsNeverKept(t *testing.T) {
	var p ProcInfo
	for _, kv := range [][2]string{{"MYSQL_PWD", "hunter2"}, {"PATH", "/bin"}, {VaultVarsEnv, "DB_USER,MYSQL_PWD"}, {"DB_USER", "ash"}} {
		p.addEnv(kv[0], kv[1])
	}
	p.finish()
	if _, ok := p.Env["MYSQL_PWD"]; ok {
		t.Fatal("a vault password was kept")
	}
	if _, ok := p.Env["DB_USER"]; ok {
		t.Fatal("a vault variable was kept")
	}
	if p.Env["PATH"] != "/bin" {
		t.Fatalf("env %v", p.Env)
	}
}
