package cli

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// unsafeWord answers why Codex's Windows command parser refuses word, or ""
// when it accepts it. It mirrors lower_command_text in codex-rs
// shell-command/src/command_safety/powershell_tree_sitter.rs: a quoted word
// is accepted unless a double-quoted one holds $; a bare word is refused
// for any of $ @ ' " ( ) { } [ ] ; | & > < , or when it starts with a digit
// and is not a plain integer. A refused word sends the whole command past
// the host's allow rules, to an approval or the sandbox.
func unsafeWord(word string) string {
	switch {
	case word == "":
		return ""
	case strings.HasPrefix(word, "'") && strings.HasSuffix(word, "'") && len(word) > 1:
		return ""
	case strings.HasPrefix(word, `"`) && strings.HasSuffix(word, `"`) && len(word) > 1:
		if strings.Contains(word, "$") {
			return "$ in a double-quoted word"
		}
		return ""
	case strings.ContainsAny(word, "$@'\"(){}[];|&><,"):
		return "a bare word holding a PowerShell character"
	case word[0] >= '0' && word[0] <= '9' && strings.Trim(word, "0123456789") != "":
		return "a bare word starting with a digit"
	}
	return ""
}

// shellWords splits a command into its words, a quoted run staying one word.
func shellWords(cmd string) []string {
	var out []string
	var b strings.Builder
	quote := byte(0)
	for i := range len(cmd) {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ' ':
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteByte(c)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

var (
	// listTail is a synopsis's "[,<x>...]", which an agent fills with more
	// list items: it stands for ",x" so an unquoted list is caught.
	listTail = regexp.MustCompile(`\[,<[^<>]*>\.\.\.\]|,\.\.\.`)
	// placeholder is a synopsis's "<x>"; a duration fills with a real one,
	// since a bare "2h" is the word the parser refuses.
	placeholder = regexp.MustCompile(`<[^<>]*>`)
)

// filled is cmd as an agent runs it: optional "[...]" groups outside quotes
// dropped, list tails and placeholders filled, a here-document dropped.
func filled(cmd string) string {
	cmd, _, _ = strings.Cut(cmd, " <<")
	cmd = listTail.ReplaceAllString(cmd, ",x")
	var b strings.Builder
	quote, depth := byte(0), 0
	for i := range len(cmd) {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '[':
			depth++
			continue
		case c == ']' && depth > 0:
			depth--
			continue
		}
		if depth == 0 {
			b.WriteByte(c)
		}
	}
	return placeholder.ReplaceAllStringFunc(b.String(), func(p string) string {
		switch p {
		case "<d>", "<d|time>":
			return "2h"
		case "<name>", "<its name>":
			return "toolbelt-dev"
		}
		return "x"
	})
}

// commands are the grelot commands in a markdown text: every inline code
// span and every code-block segment that starts with "grelot ".
func commands(text string) []string {
	var out []string
	inBlock := false
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			inBlock = !inBlock
			continue
		}
		if inBlock {
			for seg := range strings.SplitSeq(line, " | ") {
				if seg = strings.TrimSpace(seg); strings.HasPrefix(seg, "grelot ") {
					out = append(out, seg)
				}
			}
			continue
		}
		out = append(out, spans(line)...)
	}
	return out
}

// spans are the backtick spans of text that start with "grelot ".
func spans(text string) []string {
	var out []string
	parts := strings.Split(text, "`")
	for i := 1; i < len(parts); i += 2 {
		if strings.HasPrefix(parts[i], "grelot ") {
			out = append(out, parts[i])
		}
	}
	return out
}

// literalCommands are the grelot commands in a Go source's string
// literals: each run from "grelot <verb> " to the end of its clause, so a
// usage error or a hint printed outside a backtick span is checked too.
func literalCommands(t *testing.T, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	var s scanner.Scanner
	s.Init(fset.AddFile("", fset.Base(), len(src)), src, nil, 0)
	var out []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok != token.STRING {
			continue
		}
		text, err := strconv.Unquote(lit)
		if err != nil {
			t.Fatalf("unquote %s: %v", lit, err)
		}
		for _, m := range literalCommand.FindAllStringIndex(text, -1) {
			if _, ok := verbs[strings.Fields(text[m[0]:])[1]]; !ok {
				continue
			}
			cmd := text[m[0]:]
			// A placeholder's own words ("<what and why>") never end it.
			masked := placeholder.ReplaceAllStringFunc(cmd, func(p string) string {
				return strings.Repeat("x", len(p))
			})
			if i := clauseEnd.FindStringIndex(masked); i != nil {
				cmd = cmd[:i[0]]
			}
			out = append(out, cmd)
		}
	}
}

var (
	// literalCommand is where a command may start in a string literal.
	literalCommand = regexp.MustCompile(`\bgrelot [a-z-]+(\s|$)`)
	// clauseEnd is where a command in prose stops: a closing backtick, a
	// line end, the comma or semicolon that starts the next clause, a
	// parenthesis, or a word that joins the command to the sentence.
	clauseEnd = regexp.MustCompile("`|\n|, |; | \\(|\\)| (in|for|to|and|or|if|when) ")
)

// chain splits a shell line at the " | " and " && " that Codex's parser
// reads as separators between commands.
var chain = regexp.MustCompile(` (\|\||&&|\|) `)

// checkParseSafe fails for each command in cmds whose filled form holds a
// word Codex's parser refuses. A synopsis's "a|b" is a choice, so each
// alternative is checked as the word an agent writes.
func checkParseSafe(t *testing.T, where string, cmds []string) {
	t.Helper()
	for _, cmd := range cmds {
		for _, part := range chain.Split(filled(cmd), -1) {
			for _, w := range shellWords(part) {
				alts := []string{w}
				if w[0] != '\'' && w[0] != '"' {
					alts = strings.Split(w, "|")
				}
				for _, a := range alts {
					if why := unsafeWord(a); why != "" {
						t.Errorf("%s: %q: %s, %q", where, cmd, why, a)
					}
				}
			}
		}
	}
}

func TestUnsafeWord(t *testing.T) {
	for word, want := range map[string]bool{
		"--ttl=2h": false, "2h": true, "15m": true, "4812": false, "@me": true, "'@me'": false,
		"name,paths": true, "'name,paths'": false, `"$HOME"`: true, `"no change"`: false,
		"/workspace/subflux": false, "subflux-dev/review": false,
	} {
		if got := unsafeWord(word) != ""; got != want {
			t.Errorf("unsafeWord(%q) refuses = %v, want %v", word, got, want)
		}
	}
	if got := filled("grelot post @<name> --paths <path>[,<path>...] --ttl <d> [--by <d>] <<'EOF'"); got != "grelot post @toolbelt-dev --paths x,x --ttl 2h " {
		t.Errorf("filled = %q", got)
	}
}

// TestManual_everyCommandIsParseSafe holds the manual's commands to the
// forms Codex's Windows parser accepts: --ttl=2h, '@name', quoted lists.
func TestManual_everyCommandIsParseSafe(t *testing.T) {
	cmds := commands(manual)
	if len(cmds) < 40 {
		t.Fatalf("found %d commands in the manual, want every usage line and example", len(cmds))
	}
	checkParseSafe(t, "manual", cmds)
}

// bareForm is a refused word in plain prose, outside a backtick span: a
// bare duration after its flag, or an unquoted @name after a flag or post.
// A format verb counts as a duration: the value it prints is one.
var bareForm = regexp.MustCompile(`--(ttl|timeout|by|since|until) [<0-9%]|(post|--from|--with|--notify-read|--allow-agent|--deny-agent) @`)

// TestHints_everyCommandIsParseSafe holds the commands the client's lines,
// the provider files and the docs print to the same forms as the manual.
func TestHints_everyCommandIsParseSafe(t *testing.T) {
	checkParseSafe(t, "hook lines", spans(lineDormant+" "+lineUnregistered))
	var files []string
	for _, g := range []string{"../*/*.go", "../../*.go", "../../providers/*.yaml", "../../docs/*.md", "../../docs/hosts/*.md", "../../README.md"} {
		m, _ := filepath.Glob(g)
		files = append(files, m...)
	}
	if len(files) < 50 {
		t.Fatalf("found %d files, want the sources, provider files and docs", len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if strings.HasSuffix(f, ".md") {
			checkParseSafe(t, f, commands(text))
		} else {
			checkParseSafe(t, f, spans(text))
		}
		if strings.HasSuffix(f, ".go") {
			checkParseSafe(t, f+" literals", literalCommands(t, b))
		}
		for _, m := range bareForm.FindAllString(text, -1) {
			t.Errorf("%s: %q: write --ttl=<d> and quote '@name'", f, m)
		}
	}
}

// TestManual_hereDocumentHasAPipeForm keeps the manual usable from a shell
// with no here-document: a section whose example feeds the body with <<
// also shows the body piped in, a form PowerShell runs too.
func TestManual_hereDocumentHasAPipeForm(t *testing.T) {
	sections := strings.Split(manual, "\n### ")
	n := 0
	for _, sec := range sections[1:] {
		if !strings.Contains(sec, " <<'EOF'") {
			continue
		}
		n++
		if !strings.Contains(sec, " | grelot ") {
			head, _, _ := strings.Cut(sec, "\n")
			t.Errorf("%s: the body comes only from a here-document; show it piped in too", head)
		}
	}
	if n == 0 {
		t.Fatal("found no here-document example in the manual")
	}
}
