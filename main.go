// brains keeps durable records for AI-assisted work. See DESIGN.md.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const usage = `usage: brains <command> [arguments]

setup:
  init                   create this persona's database from nothing
  install                copy this build to the per-user install path
  mcp                    run as an MCP server over stdio; brains fresh adds -session CODE

sessions:
  fresh ROLE REPO        register a session in that role on that repo, then start Claude Code
                         here as it; anything after the repo goes to claude, a prompt included.
                         It's nicknamed for its role, as in auditor1, unless -as NAME comes first
  resume NAME            continue a session brains fresh started, by its nickname or its code;
                         resume ROLE REPO continues the latest one in that role on that repo
  sessions               list sessions in view: -old lists every one

roles and beliefs, which sessions are told when they start:
  roles                  list roles
  role NAME              show a role, or define or change it: -description TEXT  -fresh-eyes[=false]
  beliefs                list beliefs: -status accepted|proposed|retired|all, and with
                         -role R  -repo REPO, only those that reach that role and/or repo
  believe STATEMENT      add a belief, accepted: [-role R]  [-repo REPO]  [-note WHY]
                         [-replaces CODE] retires the belief this one supersedes
  accept CODE...         accept proposed beliefs, retiring any belief they replace
  reject CODE...         delete proposed beliefs
  retire CODE...         retire beliefs that no longer hold

tasks, acting as you rather than as a session:
  tasks                  list tasks in view: -status active|open|claimed|done|dropped|all
                         -repo REPO  -type T  -old (out of view too)
  task CODE              show one task in full
  file                   file a task: -title T  -body B | -body-file F  -to ROLE  -repo REPO
                         [-type audit_finding|handoff|improvement]  [-severity S]  [-to-session CODE]
                         [-replaces CODE] closes the task of that type this one supersedes
  close CODE             finish a task: [-outcome O]  [-dup-of CODE]  [-resolution TEXT]

A REPO is owner/name, or just its name once brains knows it. In view: open tasks, decisions that
stand (accepted as designed, proven false, deferred), anything closed in the last 30 days, and
sessions active in the last 30 days. A CODE can be just its ending, like a9a6, when nothing else
in view ends that way; a full code reaches anything.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = runInit(args)
	case "install":
		err = runInstall()
	case "mcp":
		err = runMCP(args)
	case "fresh":
		err = runFresh(args)
	case "resume":
		err = runResume(args)
	case "roles", "role":
		err = runRoleCommand(cmd, args)
	case "beliefs", "believe", "accept", "reject", "retire":
		err = runBeliefCommand(cmd, args)
	case "tasks", "task", "file", "close", "sessions":
		err = runTaskCommand(cmd, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "brains: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "brains:", err)
		os.Exit(1)
	}
}

func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	dbPath := flags.String("db", "", "database path (default: $BRAINS_DB, else the per-user data directory)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	path := *dbPath
	if path == "" {
		var err error
		if path, err = defaultDBPath(); err != nil {
			return err
		}
	}
	if err := initDB(path); err != nil {
		return err
	}
	fmt.Println("created", path)
	return nil
}

func openStore() (*store, error) {
	path, err := defaultDBPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no database at %s; run brains init first", path)
	}
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func runMCP(args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	session := flags.String("session", "", "serve this session from the start; brains fresh passes it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.db.Close()
	return serveMCP(st, *session, os.Stdin, os.Stdout)
}

// runFresh registers a session in a role on a repo, then starts Claude Code
// as it. brains names the session before it starts, so the session never
// registers, and its instructions carry its role and every belief that
// reaches it from the first turn. brains also chooses the conversation's ID,
// which links the record to the transcript and lets brains resume find it.
func runFresh(args []string) error {
	flags := flag.NewFlagSet("fresh", flag.ContinueOnError)
	as := flags.String("as", "", "the session's nickname; otherwise its role and a number, as in auditor1")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if args = flags.Args(); len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return errors.New("fresh needs a role and a repo: brains fresh [-as NAME] ROLE REPO [claude arguments]")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude isn't on PATH")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	conversation := uuidString(newUUID())
	sess, err := st.register(registration{role: args[0], repo: args[1], nickname: *as, externalID: conversation})
	st.db.Close()
	if err != nil {
		return err
	}
	return runClaude(claude, sess, append([]string{"--session-id", conversation}, args[2:]...))
}

// runResume continues, in Claude Code, a session brains fresh started: one
// named by its nickname or code, or the latest in a role on a repo.
func runResume(args []string) error {
	first, args, err := firstArg("resume", "a session's nickname or code, or a role and a repo,", args)
	if err != nil {
		return err
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude isn't on PATH")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	var sess *sessionInfo
	var id []byte
	if _, roleErr := findRole(st.db, first); roleErr == nil {
		var repo string
		if repo, args, err = firstArg("resume "+first, "a repo", args); err == nil {
			sess, id, err = latestSession(st.db, first, repo)
		}
	} else {
		sess, id, err = findSession(st.db, first)
	}
	if err == nil && sess.externalID == "" {
		err = fmt.Errorf("%s registered itself instead of starting from brains fresh, so brains doesn't know which conversation to resume", sess.code)
	}
	if err == nil {
		err = logAction(st.db, sess, "session.resume", id, "resumed by brains resume")
	}
	st.db.Close()
	if err != nil {
		return err
	}
	return runClaude(claude, sess, append([]string{"--resume", sess.externalID}, args...))
}

// runClaude runs Claude Code as a session, named for its nickname, with
// brains as its only MCP server. claude's --mcp-config takes any number of
// values, so it goes last, where it can't swallow a prompt.
func runClaude(claude string, sess *sessionInfo, args []string) error {
	name := sess.nickname
	if name == "" {
		name = sess.code
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cfg, err := os.CreateTemp("", "brains-session-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(cfg.Name())
	err = json.NewEncoder(cfg).Encode(map[string]any{"mcpServers": map[string]any{
		"brains": map[string]any{"type": "stdio", "command": self, "args": []string{"mcp", "-session", sess.code}},
	}})
	if closeErr := cfg.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	args = append(append([]string{"--name", name}, args...), "--strict-mcp-config", "--mcp-config", cfg.Name())
	cmd := exec.Command(claude, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runInstall copies this build to the per-user install path. Windows won't
// overwrite or delete a running executable, but it will rename one, so the
// installed copy is renamed aside first: sessions already running keep it,
// new ones get the new build, and the registered path never changes.
func runInstall() error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	name := "brains"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if a, err := os.Stat(self); err == nil {
		if b, err := os.Stat(target); err == nil && os.SameFile(a, b) {
			return errors.New("this is already the installed copy; run install from a new build")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	olds, _ := filepath.Glob(target + ".old-*")
	for _, old := range olds {
		os.Remove(old) // still in use by a running session: it goes next time
	}
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, target+".old-"+time.Now().Format("20060102-150405")); err != nil {
			return err
		}
	}
	if err := copyFile(self, target); err != nil {
		return err
	}
	fmt.Println("installed", target)
	fmt.Printf("\nRegister it with Claude Code once, in your user config:\n  claude mcp add --scope user brains -- \"%s\" mcp\n", target)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// firstArg splits off the argument a command takes before its flags.
func firstArg(cmd, what string, args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("%s needs %s first", cmd, what)
	}
	return args[0], args[1:], nil
}

// runRoleCommand lists roles, shows one, or defines or changes one.
func runRoleCommand(cmd string, args []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.db.Close()
	if cmd == "roles" {
		rows, err := st.roles()
		if err != nil {
			return err
		}
		fmt.Print(renderRoles(rows))
		return nil
	}
	name, args, err := firstArg(cmd, "a role name", args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	description := flags.String("description", "", "what the role is for, as its sessions should hear it")
	freshEyes := flags.Bool("fresh-eyes", false, "its sessions may only file, and follow up on their own filings")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var fresh *bool
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "fresh-eyes" {
			fresh = freshEyes
		}
	})
	if *description == "" && fresh == nil {
		r, err := findRole(st.db, name)
		if err != nil {
			return err
		}
		fmt.Print(renderRoles([]roleInfo{r}))
		return nil
	}
	created, err := st.defineRole(name, *description, fresh)
	if err != nil {
		return err
	}
	if created {
		fmt.Println("defined", strings.ToLower(name))
	} else {
		fmt.Println("changed", strings.ToLower(name))
	}
	return nil
}

// runBeliefCommand lists beliefs, adds one, or settles one.
func runBeliefCommand(cmd string, args []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.db.Close()
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch cmd {
	case "beliefs":
		status := flags.String("status", "accepted", "accepted, proposed, retired, or all")
		role := flags.String("role", "", "only those that reach this role")
		repo := flags.String("repo", "", "only those that reach this repo, as owner/name")
		if err := flags.Parse(args); err != nil {
			return err
		}
		rows, err := st.beliefs(*status, *role, *repo)
		if err != nil {
			return err
		}
		fmt.Print(renderBeliefs(rows))
	case "believe":
		statement, args, err := firstArg(cmd, "the belief, in quotes,", args)
		if err != nil {
			return err
		}
		role := flags.String("role", "", "the role it's for; every role if left out")
		repo := flags.String("repo", "", "the repo it's about, as owner/name; every repo if left out")
		note := flags.String("note", "", "why it's held: where it came from, what it prevents")
		replaces := flags.String("replaces", "", "the accepted belief this one supersedes, which retires")
		if err := flags.Parse(args); err != nil {
			return err
		}
		code, err := st.believe(nil, statement, *role, *repo, *note, *replaces)
		if err != nil {
			return err
		}
		fmt.Println("added", code)
		if *replaces != "" {
			fmt.Println("retired what it replaces")
		}
	default:
		if _, _, err := firstArg(cmd, "a belief's code", args); err != nil {
			return err
		}
		for _, typed := range args {
			code, retired, err := st.settleBelief(typed, cmd)
			if err != nil {
				return err
			}
			fmt.Println(map[string]string{"accept": "accepted", "reject": "rejected", "retire": "retired"}[cmd], code)
			if retired != "" {
				fmt.Println("retired", retired, "in its place")
			}
		}
	}
	return nil
}

// runTaskCommand runs the task commands as you, not as a session.
func runTaskCommand(cmd string, args []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.db.Close()

	var code string
	if cmd == "task" || cmd == "close" {
		if code, args, err = firstArg(cmd, "a task code", args); err != nil {
			return err
		}
	}
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch cmd {
	case "tasks":
		status := flags.String("status", "active", "active, open, claimed, done, dropped, or all")
		repo := flags.String("repo", "", "only this repo, as owner/name or its name")
		typ := flags.String("type", "", "only this type: "+strings.Join(taskTypes, ", "))
		old := flags.Bool("old", false, "include tasks out of view: closed over 30 days ago, and not a standing decision")
		if err := flags.Parse(args); err != nil {
			return err
		}
		rows, err := st.list(nil, "", *status, *repo, *typ, *old)
		if err != nil {
			return err
		}
		fmt.Print(renderTasks(rows))
	case "task":
		d, err := st.show(nil, code, "", false)
		if err != nil {
			return err
		}
		fmt.Print(renderTask(d))
	case "file":
		var in fileInput
		flags.StringVar(&in.typ, "type", "audit_finding", strings.Join(taskTypes, ", "))
		flags.StringVar(&in.title, "title", "", "one line, at most 200 characters")
		flags.StringVar(&in.body, "body", "", "the full text")
		bodyFile := flags.String("body-file", "", "read the body from this file")
		flags.StringVar(&in.severity, "severity", "", "critical, high, medium, low, or info")
		flags.StringVar(&in.toRole, "to", "", "the role it's for, for example fixer")
		flags.StringVar(&in.repo, "repo", "", "the repo it's about, as owner/name")
		flags.StringVar(&in.toSession, "to-session", "", "a session code, instead of -to")
		flags.StringVar(&in.replaces, "replaces", "", "a task of this type that this one supersedes, which closes")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if *bodyFile != "" {
			b, err := os.ReadFile(*bodyFile)
			if err != nil {
				return err
			}
			in.body = string(b)
		}
		filed, err := st.file(nil, in)
		if err != nil {
			return err
		}
		fmt.Println("filed", filed)
	case "close":
		outcome := flags.String("outcome", "", strings.Join(outcomes, ", "))
		dupOf := flags.String("dup-of", "", "for -outcome duplicate: the task it repeats")
		resolution := flags.String("resolution", "", "what was fixed, or why not; the filer will see it")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if err := st.close(nil, code, *outcome, *dupOf, *resolution); err != nil {
			return err
		}
		fmt.Println("closed", code)
	case "sessions":
		old := flags.Bool("old", false, "include sessions out of view: none of their activity in the last 30 days")
		if err := flags.Parse(args); err != nil {
			return err
		}
		rows, err := st.sessions(*old)
		if err != nil {
			return err
		}
		fmt.Print(renderSessions(rows))
	}
	return nil
}
