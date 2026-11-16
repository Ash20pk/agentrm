// Package shell works out what a command line actually runs.
//
// A command line is parsed into a syntax tree (mvdan.cc/sh). Only commands that execute
// something count: quoted strings, comments and heredoc bodies are data, and inert commands
// (echo, grep, git, ...) neither start a heavy job nor use a resource by mentioning one. Command
// substitutions run, so they count wherever they appear; so do the scripts given to `sh -c`,
// `eval`, or a shell fed a heredoc.
package shell

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// inert commands only read or print: naming a resource in one is not using it, and naming a
// heavy command in one is not running it.
var inert = map[string]bool{}

func init() {
	for _, w := range strings.Fields("echo printf grep egrep fgrep rg ag cat bat less more head tail ls find fd " +
		"sed awk wc sort uniq cut tr git diff cd pwd true false test [ [[ export set unset alias which type man " +
		"tee touch mkdir rm cp mv ln chmod read sleep date basename dirname jq yq agentrm : column fmt fold " +
		"base64 shasum md5 open pbcopy pbpaste say history") {
		inert[w] = true
	}
}

// wrappers run the rest of their arguments as a command.
var wrappers = map[string]bool{"sudo": true, "time": true, "nohup": true, "env": true, "exec": true,
	"command": true, "nice": true, "caffeinate": true, "timeout": true, "gtimeout": true, "doas": true,
	"stdbuf": true, "unbuffer": true, "arch": true}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// Active returns the commands in cmd that execute something, one per line, with each
// program named by its base name (`/opt/homebrew/bin/kind` → `kind`). It is what the built-in
// classifier and the use-matcher look at.
func Active(cmd string) string { return normalize(ActiveRaw(cmd)) }

// ActiveRaw is Active with programs written as typed (`./stack.sh up`), for user patterns.
func ActiveRaw(cmd string) string {
	var out []string
	if err := activeInto(cmd, &out, 0); err != nil {
		return fallbackActive(cmd)
	}
	return strings.Join(out, "\n")
}

func normalize(active string) string {
	lines := strings.Split(active, "\n")
	for i, l := range lines {
		if f := strings.SplitN(l, " ", 2); f[0] != "" {
			f[0] = filepath.Base(f[0])
			lines[i] = strings.Join(f, " ")
		}
	}
	return strings.Join(lines, "\n")
}

func activeInto(src string, out *[]string, depth int) error {
	if depth > 4 {
		return nil
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return err
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		args := make([]string, len(call.Args))
		for i, w := range call.Args {
			args[i] = wordText(w)
		}
		args = unwrap(args)
		if len(args) == 0 {
			return true
		}
		word := filepath.Base(args[0])
		switch {
		case inert[word] || strings.HasPrefix(word, "#"):
			// Not run for its arguments; command substitutions inside them are still visited.
		case shells[word] && len(args) >= 3 && args[1] == "-c":
			_ = activeInto(unquote(args[2]), out, depth+1)
		case word == "eval" && len(args) >= 2:
			_ = activeInto(unquote(strings.Join(args[1:], " ")), out, depth+1)
		default:
			*out = append(*out, strings.Join(args, " "))
		}
		// A shell fed a heredoc runs its body.
		if shells[word] {
			if st, ok := parentStmt(f, call); ok {
				for _, r := range st.Redirs {
					if r.Hdoc != nil && (r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc) {
						_ = activeInto(literal(r.Hdoc), out, depth+1)
					}
				}
			}
		}
		return true
	})
	return nil
}

// unwrap drops leading assignments and wrapper commands (with their flags) so the real command
// word comes first: `FOO=1 sudo -E nice -n 5 docker build .` → `docker build .`.
func unwrap(args []string) []string {
	for len(args) > 0 {
		a := args[0]
		switch {
		case assignRe.MatchString(a):
			args = args[1:]
		case wrappers[filepath.Base(a)]:
			w := filepath.Base(a)
			args = args[1:]
			for len(args) > 0 && (strings.HasPrefix(args[0], "-") || assignRe.MatchString(args[0])) {
				args = args[1:]
			}
			if (w == "timeout" || w == "gtimeout") && len(args) > 0 {
				args = args[1:] // the duration
			}
			if w == "nice" && len(args) > 0 && isNumber(args[0]) {
				args = args[1:]
			}
		default:
			return args
		}
	}
	return args
}

func isNumber(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func wordText(w *syntax.Word) string {
	var b bytes.Buffer
	_ = syntax.NewPrinter().Print(&b, w)
	return b.String()
}

// literal returns the text of a word without expanding it (heredoc bodies).
func literal(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		if l, ok := p.(*syntax.Lit); ok {
			b.WriteString(l.Value)
		} else {
			var bb bytes.Buffer
			_ = syntax.NewPrinter().Print(&bb, p)
			b.Write(bb.Bytes())
		}
	}
	return b.String()
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' && s[len(s)-1] == '\'' || s[0] == '"' && s[len(s)-1] == '"') {
		return s[1 : len(s)-1]
	}
	return s
}

// parentStmt finds the statement whose command is call, to read its redirections.
func parentStmt(f *syntax.File, call *syntax.CallExpr) (*syntax.Stmt, bool) {
	var found *syntax.Stmt
	syntax.Walk(f, func(n syntax.Node) bool {
		if st, ok := n.(*syntax.Stmt); ok && st.Cmd == syntax.Command(call) {
			found = st
			return false
		}
		return found == nil
	})
	return found, found != nil
}

// CommandWords returns the program each active command runs.
func CommandWords(cmd string) []string {
	var words []string
	for _, l := range strings.Split(Active(cmd), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			words = append(words, filepath.Base(f[0]))
		}
	}
	return words
}
