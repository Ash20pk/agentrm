package sys

import (
	"os"
	"path/filepath"
	"strings"
)

// GitHead finds the repository a directory is in and the branch checked out there, by reading
// .git/HEAD (no git process). A worktree's .git is a file naming its git directory. Detached
// HEAD gives the commit. Both are "" outside a repository.
func GitHead(dir string) (repo, branch string) {
	for d := dir; d != "" && d != "/" && d != "."; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		fi, err := os.Stat(g)
		if err != nil {
			continue
		}
		gitDir := g
		if !fi.IsDir() {
			b, err := os.ReadFile(g)
			if err != nil {
				return "", ""
			}
			p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
			if !ok {
				return "", ""
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(d, p)
			}
			gitDir = p
		}
		b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
		if err != nil {
			return d, ""
		}
		head := strings.TrimSpace(string(b))
		if ref, ok := strings.CutPrefix(head, "ref: refs/heads/"); ok {
			return d, ref
		}
		if len(head) >= 12 {
			return d, head[:12]
		}
		return d, head
	}
	return "", ""
}
