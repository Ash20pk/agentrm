package shell

import (
	"path/filepath"
	"regexp"
	"strings"
)

// The hand-written splitter is kept as the fallback for input the parser rejects (an unclosed
// quote, a construct from another shell). It is coarser but never fails.

var heredocRe = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

func fallbackActive(cmd string) string {
	var keep []string
	for _, seg := range segments(stripHeredocs(cmd)) {
		args := unwrap(strings.Fields(seg))
		if len(args) == 0 {
			continue
		}
		word := filepath.Base(strings.TrimLeft(args[0], "({"))
		if word == "" || inert[word] || strings.HasPrefix(word, "#") {
			continue
		}
		keep = append(keep, strings.Join(args, " "))
	}
	return strings.Join(keep, "\n")
}

func stripHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		m := heredocRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		segs := segments(lines[i][:strings.Index(lines[i], "<<")])
		toShell := false
		if len(segs) > 0 {
			if f := unwrap(strings.Fields(segs[len(segs)-1])); len(f) > 0 {
				toShell = shells[filepath.Base(f[0])]
			}
		}
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != m[1]; i++ {
			if toShell {
				out = append(out, lines[i])
			}
		}
	}
	return strings.Join(out, "\n")
}

// segments splits a command at ; | & newlines, $( and backticks, outside quotes.
func segments(cmd string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\\' && i+1 < len(cmd):
			cur.WriteByte(c)
			i++
			cur.WriteByte(cmd[i])
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			flush()
			i++
		case c == '`':
			flush()
		case quote == '"':
			if c == '"' {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ';' || c == '|' || c == '&' || c == '\n':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}
