# brains

Durable context for Claude Code sessions: what each session is for,
what it should know from the start,
and the work sessions pass to each other.

A new Claude Code session (with built-in memory features turned off)
doesn't know your machine, your house rules, or what the last session got done,
so most sessions start with you explaining. With brains, you explain once.
`brains fresh fixer owner/app` starts a session that already knows it's a fixer, what fixers do,
and every rule and fact you've accepted for it. Findings and handoffs
pass between sessions as tasks, so the work outlives any one conversation.

brains is one Go binary: the CLI you use and the MCP server your sessions use,
over one SQLite file per user account.

## How it works

- **Sessions** start with `brains fresh ROLE REPO`, which registers the session, then starts
  Claude Code with brains as its only MCP server. brains briefs the session with its nickname,
  what its role is for, and every belief that reaches it. The briefing sits in the system
  prompt, so it survives compaction. brains knows which conversation a session is, so if its
  server restarts, or you reopen the conversation with plain `claude`, it's picked up again as
  itself.
- **Roles** say what a session is for, such as fixing or auditing. You define them, and a role's
  description is what its sessions are told they're for.
- **Tasks** carry work between sessions: audit findings, handoffs, and improvements, each
  addressed to a role on a repo or to one session. A session claims a task before working on it,
  and closes it with an outcome and a resolution the filer can read. A task can replace an
  earlier one of its type, which closes it, so a chain of handoffs leaves one open.
- **Beliefs** are the rules and facts sessions are told: never push, tests run in WSL, Node 22 is
  installed. Each is for one role, one repo, both, or every session. Sessions can propose
  beliefs, and none reaches a session until you accept it. A belief that sharpens another
  retires it when you accept the replacement, so nobody is told two versions of a rule.
- **Fresh eyes** is a behavior a role can have. A fresh-eyes session can file findings and follow
  up on its own, and can't see anything else in brains, so earlier rounds can't steer it.

brains offers sessions eleven MCP tools: register, file, withdraw, inbox, mine, show, claim,
release, close, list, and propose. A fresh-eyes session gets four: file, withdraw, mine, and show.

Everything lives in the one SQLite file, not in CLAUDE.md files or Claude Code settings. brains
logs every operation, with who did it and why, and keeps every earlier version of every row.
Finished work drops out of view after 30 days, but nothing is deleted.

## What to keep in mind

- **A belief is an instruction to every session it reaches.** That's the point of them, and it's
  why accepting one is yours alone: no tool offers accept, reject or retire to a session. A
  proposal reaches nobody until you accept it, so read it as something your sessions will follow
  without asking again. brains records which session proposed it and why.
- **What one session filed is information to the next, not orders.** A finding or handoff is
  another session's report, and brains tells sessions to read it that way. Nothing a session
  writes into a task gives it authority over the session that reads it; that comes from you,
  through roles and beliefs.
- **Keep secrets out of tasks.** Every session working on a repo can read what's filed against
  it. Name the file to look in rather than pasting what's inside it.
- **Sessions share a database, not a sandbox.** brains decides what a session is told and which
  of its own tools it gets; it doesn't limit what Claude Code can do, and a session with a shell
  can reach the database directly. Fresh eyes is there to keep a reader from being steered, not
  to contain anything.

## A round of work

One fixer and two auditors on one repo, each in its own terminal (Setup below defines both
roles):

    brains fresh fixer owner/app "check the handoff"
    brains fresh auditor owner/app "audit for security and correctness"
    brains fresh auditor owner/app "audit for code quality and tests"

They come up as fixer1, auditor1, and auditor2. The auditors file findings to the fixer on
owner/app. Neither sees the other's filings or any earlier round's, so each reads the code cold,
and the fixer closes repeats as duplicates.

The fixer's loop is the whole of brains, in order: check the inbox, claim a task before working
on it, then close it with an outcome and a resolution the auditor will read, or release it with a
note if it has to stop. "Check the handoff" means that first step. Before it stops for good, it
files a handoff that replaces the one it started from, so the next fixer finds one open handoff
and needs nothing before it.

    brains tasks              open and claimed tasks, most severe first
    brains task CODE          one task in full
    brains resume fixer1      continue a session

A session starts in whatever folder you run `brains fresh` from, so resume it from there too.
This repo is set up to be that folder: clone the repos sessions work on into
`REPOS/<owner>/<repo>`, which git ignores (see [REPOS/README.md](REPOS/README.md)).

## Setup

Building takes Go 1.26 or later, and running sessions takes Claude Code, with `claude` on your
PATH. From this folder:

    go build
    ./brains install
    ./brains init

`install` copies the build into your brains folder: `%LOCALAPPDATA%\brains` on Windows, and
`$XDG_DATA_HOME/brains` (by default `~/.local/share/brains`) elsewhere. Put that folder on your
PATH. Sessions keep running the copy they started with, so you can rebuild and install again
while they work. `init` creates the database there, `brains.db`; set `BRAINS_DB` to use another
file. Each user account gets its own folder, so a work account and a personal one keep separate
records.

Next, define a role for each kind of session you run, with a description written to its
sessions:

    brains role fixer -description "You fix what's filed against the repo. Before you stop, file a handoff to the fixer on this repo, replacing the one you started from."
    brains role auditor -description "You read the repo cold and file what you find, one finding per filing." -fresh-eyes

Then add as beliefs what you'd otherwise explain at the start of every session, each scoped as
narrowly as it's true:

    brains believe "Never push; the owner pushes." -role fixer
    brains believe "Tests run in WSL, against a copy of the repo." -repo owner/app
    brains believe "Node 22 is installed." -note "Saves sessions from checking."

`brains fresh` connects each session it starts to brains, with nothing else to set up. To give
plain `claude` sessions brains too, run the `claude mcp add` command that `install` prints; a
plain session registers itself once you tell it its role and repo.

## Docs

- [ORIENTATION.md](ORIENTATION.md): using brains day to day, from starting sessions to writing
  beliefs.
- [DESIGN.md](DESIGN.md): what's decided and why, and what's still open.
- `brains help` lists every command.

## The code

| file | holds |
|---|---|
| `main.go` | the CLI, including `fresh`, `resume`, and `install` |
| `mcp.go` | the MCP server: its tools, and what sessions are told |
| `ops.go` | sessions, tasks, repos, codes, and what's in view |
| `roles.go`, `beliefs.go` | roles and beliefs |
| `store.go` | the database: where it lives, `init`, and the history tables |
| `schema.sql` | every table, embedded in the binary |
| `flow_test.go`, `store_test.go` | the tests: `go test` |

The SQLite driver, `modernc.org/sqlite`, is the only direct dependency. It's pure Go, so building
needs no C compiler.

## Status

A working prototype, in use on Windows. It builds for Linux and macOS too, but hasn't been used
there. The schema isn't settled and there are no migrations: a change is applied to the live
database by hand, or the database is rebuilt with its rows carried over.

MIT licensed. It's built for one person's own use, so there's no issue template and no roadmap.
