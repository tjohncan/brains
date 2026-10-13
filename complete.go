package main

import (
	"bufio"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Tab completion for bash. With this line in ~/.bashrc:
//
//	complete -C brains brains
//
// bash runs brains to complete its own arguments, with the command line in
// COMP_LINE and the cursor's place in COMP_POINT. brains prints the words that
// fit, one per line: commands, flags and their values, roles, repos,
// nicknames, and the endings of codes in view. bash doesn't filter what it's
// given, so brains does. Where nothing fits, Tab does nothing: brains doesn't
// complete file names. Completing only reads the database, and logs nothing.

// commands are the words brains takes first.
var commands = []string{"accept", "belief", "beliefs", "believe", "close", "file", "fresh", "help", "init",
	"install", "mcp", "reject", "resume", "retire", "role", "roles", "sessions", "task", "tasks"}

// commandFlags are the flags each command defines in main.go, true for one
// that takes a value. Keep them in step.
var commandFlags = map[string]map[string]bool{
	"init":     {"db": true},
	"mcp":      {"session": true, "dir": true},
	"fresh":    {"as": true},
	"sessions": {"old": false},
	"role":     {"description": true, "fresh-eyes": false},
	"beliefs":  {"status": true, "role": true, "repo": true},
	"believe":  {"role": true, "repo": true, "note": true, "replaces": true},
	"tasks":    {"status": true, "repo": true, "type": true, "old": false},
	"file": {"type": true, "title": true, "body": true, "body-file": true, "severity": true, "to": true,
		"repo": true, "to-session": true, "replaces": true},
	"close": {"outcome": true, "dup-of": true, "resolution": true},
}

// runComplete answers bash: line is the command line, and point is where the
// cursor is in it. Any trouble means nothing to offer, never an error.
func runComplete(w io.Writer, line, point string) {
	if n, err := strconv.Atoi(point); err == nil && n >= 0 && n <= len(line) {
		line = line[:n]
	}
	st, err := openStore()
	if err == nil {
		defer st.db.Close()
	}
	out := bufio.NewWriter(w)
	for _, word := range complete(st, line) {
		out.WriteString(word + "\n")
	}
	out.Flush()
}

// complete is what could replace the last word of line, the command line up
// to the cursor, sorted. Without a database, st is nil, and only what doesn't
// come from one is offered.
func complete(st *store, line string) []string {
	words, cur, ok := splitWords(line)
	// bash also breaks words at these, so it wouldn't replace what brains saw.
	if !ok || len(words) == 0 || strings.ContainsAny(cur, "=:<>;|&(") {
		return nil
	}
	c := &completer{st: st, cur: cur}
	if len(words) == 1 {
		return c.match(commands)
	}
	cmd, rest := words[1], words[2:]
	flags := commandFlags[cmd]
	var args []string
	for i := 0; i < len(rest); i++ {
		if len(rest[i]) < 2 || rest[i][0] != '-' {
			args = append(args, rest[i])
			continue
		}
		name, _, inline := strings.Cut(strings.TrimLeft(rest[i], "-"), "=")
		if !flags[name] || inline {
			continue // a flag on its own, or with its value
		}
		if i == len(rest)-1 {
			return c.match(c.flagValues(cmd, name)) // its value is what's being typed
		}
		i++ // its value
	}
	if strings.HasPrefix(cur, "-") {
		return c.match(flagNames(flags))
	}
	if words, expected := c.positional(cmd, args); expected {
		return c.match(words)
	}
	return c.match(flagNames(flags))
}

type completer struct {
	st  *store // nil without a database
	cur string // the word being completed, as typed so far
}

// positional is what can be typed for a command's next argument that isn't a
// flag, and whether it takes one there at all.
func (c *completer) positional(cmd string, args []string) ([]string, bool) {
	switch cmd {
	case "fresh":
		switch len(args) {
		case 0:
			return c.roles(false), true
		case 1:
			return c.repos(), true
		}
		return nil, true // the rest goes to claude
	case "resume":
		switch {
		case len(args) == 0 && c.cur != "" && c.cur[0] >= '0' && c.cur[0] <= '9':
			return c.list("select code_name from session where " + sessionInView), true
		case len(args) == 0:
			return append(c.nicknames(false), c.roles(false)...), true
		case len(args) == 1 && slices.Contains(c.roles(false), args[0]):
			return c.repos(), true
		}
		return nil, true // the rest goes to claude
	case "believe":
		return nil, len(args) == 0 // the statement
	case "accept", "reject":
		return without(c.codes("belief", "status = 'proposed'"), args), true
	case "retire":
		return without(c.codes("belief", "status = 'accepted'"), args), true
	}
	if len(args) > 0 {
		return nil, false
	}
	switch cmd {
	case "role":
		return c.roles(false), true
	case "belief":
		return c.codes("belief", beliefInView), true
	case "task":
		return c.codes("task", taskInView), true
	case "close":
		return c.codes("task", "status in ('open', 'claimed')"), true
	}
	return nil, false
}

// flagValues are what a flag can be given.
func (c *completer) flagValues(cmd, flag string) []string {
	switch flag {
	case "status":
		if cmd == "beliefs" {
			return []string{"accepted", "proposed", "retired", "all"}
		}
		return []string{"active", "open", "claimed", "done", "dropped", "all"}
	case "type":
		return taskTypes
	case "severity":
		return severities
	case "outcome":
		return outcomes
	case "role":
		return c.roles(false)
	case "to":
		return c.roles(true)
	case "repo":
		return c.repos()
	case "to-session":
		return c.nicknames(true)
	case "session":
		return c.nicknames(false)
	case "dup-of":
		return c.codes("task", taskInView)
	case "replaces":
		if cmd == "believe" {
			return c.codes("belief", "status = 'accepted'")
		}
		return c.codes("task", "status in ('open', 'claimed')")
	}
	return nil // free text, or a file name
}

// roles are the roles' names. With inbox, only those whose sessions have an
// inbox, which fresh eyes doesn't, so only those a task can be addressed to.
func (c *completer) roles(inbox bool) []string {
	if inbox {
		return c.list("select code_name from role where is_fresh_eyes = 0")
	}
	return c.list("select code_name from role")
}

// nicknames are those of the sessions in view; with inbox, only sessions that
// have one.
func (c *completer) nicknames(inbox bool) []string {
	query := "select nickname from session where nickname is not null and " + sessionInView
	if inbox {
		query += " and role_pin in (select pin from role where is_fresh_eyes = 0)"
	}
	return c.list(query)
}

// repos are the repos as owner/name, and, once what's typed can't be an
// owner, by a name only one owner has.
func (c *completer) repos() []string {
	repos := c.list("select owner || '/' || name from repo")
	if c.cur == "" || strings.Contains(c.cur, "/") {
		return repos
	}
	return append(repos, c.list("select min(name) from repo group by name collate nocase having count(*) = 1")...)
}

// codes are the codes in table that fit where, given by their endings, the
// way they're usually typed, unless what's typed can only start a full code:
// an ending never has a 0, a 1 or a hyphen, and a code's date always does.
func (c *completer) codes(table, where string) []string {
	codes := c.list("select code_name from " + table + " where " + where)
	if strings.ContainsAny(c.cur, "01-") {
		return codes
	}
	for i, code := range codes {
		codes[i] = code[strings.LastIndex(code, "-")+1:]
	}
	return codes
}

// list is the first column of a query's rows, or nothing without a database
// or on any error.
func (c *completer) list(query string) []string {
	if c.st == nil {
		return nil
	}
	out, err := strings0(c.st.db, query)
	if err != nil {
		return nil
	}
	return out
}

// match keeps the words that start with what's typed, in any case, sorted
// and each once.
func (c *completer) match(words []string) []string {
	var out []string
	for _, w := range words {
		if len(w) >= len(c.cur) && strings.EqualFold(w[:len(c.cur)], c.cur) {
			out = append(out, w)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func flagNames(flags map[string]bool) []string {
	var out []string
	for name := range flags {
		out = append(out, "-"+name)
	}
	return out
}

// without is words less those already typed, for a command that takes several.
func without(words, typed []string) []string {
	return slices.DeleteFunc(words, func(w string) bool { return slices.Contains(typed, w) })
}

// splitWords splits a command line at blanks outside quotes, as bash does:
// the words before the one being typed, and that one, which is empty after a
// blank. It isn't ok with the cursor inside quotes.
func splitWords(line string) (words []string, cur string, ok bool) {
	var word strings.Builder
	var quote rune
	inWord := false
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words, inWord = append(words, word.String()), false
				word.Reset()
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, "", false
	}
	if inWord {
		cur = word.String()
	}
	return words, cur, true
}
