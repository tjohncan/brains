# brains: design

brains keeps durable records for AI-assisted work: the sessions you run, the initiatives they
serve, the repos they touch, tasks passed between them, and beliefs worth carrying forward. One
Go binary is both a CLI for you and an MCP server for agents. Data lives in SQLite, one file per
persona.

The schema isn't settled, and there's no migration mechanism: a change is applied to a live
database by hand, or the database is recreated and its rows carried over. `README.md` says what
brains is and how to set it up, and `ORIENTATION.md` how to use it. This file says why it's built
this way, so none of it has to be argued again.

## Decisions

**Storage: SQLite, one file per persona.** The file lives in the per-user data directory, so each
account gets its own with no configuration and the operating system keeps the other accounts out;
`BRAINS_DB` points at another file. SQLite is built for small transactional reads and writes from
several processes, has full-text search (FTS5) built in, and its file format is supported through
2050.
Not DuckDB: only one process can write at a time, it has no triggers or partial indexes, and its
Go driver needs cgo. DuckDB is still useful for analysis: it can attach the SQLite file read-only.

**Language: Go, standard library except the SQLite driver.** A single binary with no runtime. The
schema is embedded, so `brains init` needs nothing else on disk, and it cross-compiles with one
environment variable.
Launching an executable costs tens of milliseconds whatever it's written in, which is more than
any of this work takes, so runtime speed didn't separate Go, Rust, and Zig. Rust means slower
builds and a multi-gigabyte Windows toolchain for protection this program barely needs. Zig is
pre-1.0, breaks every release, is harder for agents to write, and has no MCP SDK.
Driver: `modernc.org/sqlite`, the mainstream pure-Go driver: v1, years of track record, and no C
compiler needed. It ties `ncruces/go-sqlite3` on startup, both at the launch floor. ncruces has
fewer modules and faster rebuilds, but it's pre-1.0 and less proven, and nothing exotic belongs in
this stack. `mattn/go-sqlite3` is the most used overall, but it needs cgo.

**Interfaces: a CLI for you, MCP for agents.** Both run the same code. The MCP server speaks stdio
and is written by hand on `encoding/json`; the official SDK pulls in JWT and OAuth libraries for
transports brains doesn't use. Agents see tool names, parameters, and results, never SQL or the
file path. Read tools require a purpose. Running as a server also keeps the launch cost off every
call.

**Install: one fixed path per user.** `brains install` renames the installed `brains.exe` aside
and writes the new build in its place, because Windows allows renaming a running executable but
not overwriting it. Sessions already running keep the copy they started with, new ones get the new
build, and the path in the MCP registration never changes.

**Schema conventions.** `pin` is the sequential integer key for joins, `id` a UUID for durable
identity and what `action.target_id` points at, and `code_name` the readable handle you and agents
type. `created_at` comes from the column default and every UPDATE sets `updated_at`. Constraints
are named with `uix_`, `ck_`, `fk_`, and `idx_` prefixes, and the SQL is lowercase with leading
commas. Every table except `action` gets a `history__` companion, filled by update and delete
triggers that `brains init` generates. The triggers record changes and never block them, so
cleaning up by hand is always possible.

**Links are explicit tables.** Junction tables connect sessions to initiatives and to repos;
initiatives link to each other with a type (contains, blocked_by, relates_to). A generic
entity/association layer is deferred. If it comes, it will be a registry sitting on top of the
detail tables, not a replacement for them. Revisit when task and belief multiply the links.

**Approval gate on beliefs only.** A belief is an instruction to every session it reaches, and
pruning a bad one later doesn't undo the sessions it already steered. So agents propose freely,
proposals stay invisible until you accept them, and states run proposed → accepted → retired. No
tool offers accept, reject, or retire to a session. Everything else is open, then audited and
pruned later.

**"Who" lives in the action log.** Every operation through brains writes an `action` row. A null
session anywhere means you, acting directly. Sessions must give a purpose; you don't have to.
Running `sqlite3` directly for maintenance skips the log, but the history tables still record the
change.

**Roles belong to each persona.** A role is a row in the database, not a word in the code: its
name, what it's for (its sessions are told this at the start), and whether its sessions work with
fresh eyes. The code offers the behaviors and a role picks from them, so no list of roles is
committed here. Only you define roles; no tool offers it to sessions. Registering or filing to a
role that doesn't exist is refused, with the roles that do, so nothing goes to a role nobody
holds; names match in any case. A role has to behave differently to earn a row. A different brief
is a lens in the prompt: a hiring-manager read is an auditor with a lens.

**Sessions start from brains.** `brains fresh ROLE REPO` registers the session, then starts Claude
Code with brains as its only MCP server (`--strict-mcp-config`), already serving that session.
Knowing the session up front is what lets brains brief it. MCP instructions sit in the system
prompt, so unlike a tool reply they survive compaction. They name the session, say what its role
is for, and list every accepted belief that reaches it, as of when the server started. brains also
picks the conversation's ID (`--session-id`), so `session.external_id` ties the record to its
transcript. A plain `claude` session still gets brains at user scope and registers itself. Until
it does, it's told only the beliefs for every role on every repo; the register reply gives it the
rest. A tool reply doesn't outlast compaction the way instructions do, so the brief tool repeats
the whole briefing on demand, as brains has it then.

**Sessions have nicknames.** A code is for records; a nickname is for people. Each session gets one
when it registers: the name given with `brains fresh -as NAME`, or its role and the lowest number
no session in view has, as in auditor1. Claude Code shows it as the conversation's name, and
`brains resume auditor1` continues that conversation (`brains resume ROLE REPO` continues the
latest session in that role there). A nickname is unique only among sessions in view, so the
numbers stay small: auditor1 comes free again once its session has been idle 30 days.

**Fresh eyes is a role behavior.** A session in a fresh-eyes role gets five tools: file and
withdraw, mine and show, which reach only its own filings, and brief. It can file findings, take
back its own, and read their resolutions, but nothing else in brains, so earlier rounds can't
steer it.
A plain session that registers in a fresh-eyes role loses its other tools from then on. This keeps
a reader from being steered; it isn't containment, since brains doesn't limit what Claude Code
itself can do. If a fixer is editing while auditors read, give the auditors a git worktree so they
see a stable snapshot.

**Beliefs reach sessions by role and repo.** A belief names a role, a repo, both, or neither, and a
session is told every accepted belief that fits it, broadest first, grouped under the scope it
holds in. Scoping to the repo rather than the role is what lets house rules reach fresh auditors
too, so they measure the code against the same standards the fixer does.

**A task or belief can replace another.** Filing with `replaces` closes the task it supersedes, of
the same type, as the new one is filed; proposing a belief with `replaces` retires the belief it
supersedes when you accept the new one. Without it, each new handoff is two calls and leaves a
chain the next session reads back through, and a belief that sharpens another leaves both in force,
so sessions are told overlapping rules. One call does what took two, nothing is briefly open twice
or in force twice, and the link records what followed what. A replacement has to stand on its own:
the chain is for the record, not for reading back through. What replacement doesn't solve is a
loose end carried from handoff to handoff; that belongs in its own improvement task, which stays
open until someone acts on it.

**A server recognizes its conversation.** Claude Code passes every MCP server it starts the
conversation's own ID, and that's what `brains fresh` chose and recorded as `session.external_id`,
so a server started without being told its session still finds it. That covers a brains server
restarting mid-conversation and a conversation reopened with plain `claude --resume` or `-c`
instead of `brains resume`, since a resumed conversation keeps its ID. A session that registers
itself records its conversation too, which is also what lets `brains resume` reopen it. One
conversation is one session: registering a conversation brains already knows is refused, naming
it, rather than starting a second record for work that's already someone's. Likewise a server
serves one session, so once it has one, register isn't offered again. Resuming by code reaches
only a session of this conversation, or one with no conversation recorded, which then takes this
one.

**Old work leaves view; nothing is deleted.** A task stays in view while it's open, while it stands
as a decision (accepted as designed, proven false, deferred), and for 30 days after it closes
otherwise. A session stays in view for 30 days after it last did anything, and a belief until it's
retired. Lists show what's in view, a code's ending only reaches what's in view, and a new code is
drawn so that no code in view already ends the same way, so the four characters agents cite keep
meaning one thing however large the database grows. Out of view, a row is still there: its full
code still works, and `-old` lists it. Decisions stay in view because fresh auditors raise them
again and the fixer closes the repeats as their duplicates; deferred work hasn't happened yet.

**Layout.** Sessions start at the brains root, which is the repository folder itself, so the tools
a session uses and the repos it works on sit under one path. Repos are cloned to
`REPOS/<owner>/<repo>`, which makes the path derivable from a repository's identifier on any
machine; any other first-level folder under `REPOS/` is a scratch copy. Nothing inside `REPOS/` is
tracked.

## Tables

| table | holds |
|---|---|
| brand | who makes a technology (Anthropic) |
| technology | what sessions run in (Claude Code, Claude in Excel) |
| initiative | an area of effort |
| association_type | contains, blocked_by, relates_to (symmetric) |
| initiative_association | typed links between initiatives |
| repo | a source repository, by owner and name |
| role | what a session is for, defined by each persona |
| session | one working session in some technology |
| session_initiative, session_repo | what a session serves and works in |
| task, task_type | provisional: work items, including handoffs; one may replace another |
| belief | provisional: conventions and facts sessions are told, gated; one may replace another |
| action | every operation, who and why; purged by age |
| history__* | every earlier version of every row; generated |

See `schema.sql`.

## CLI and MCP

`brains fresh` is the way in, for every role. Any plain `claude` session also gets the full set of
brains tools, because brains is registered with Claude Code at user scope.

    brains init                      create a persona's database from nothing
    brains install                   copy a build to the per-user install path
    brains mcp [-session CODE]       run as an MCP server over stdio
    brains fresh [-as NAME] ROLE REPO      register a session and start Claude Code as it
    brains resume NAME | ROLE REPO         continue a session brains knows the conversation of
    brains roles | role              list roles; show, define, or change one
    brains beliefs | believe | accept | reject | retire      work with beliefs
    brains tasks | task | file | close | sessions          work with tasks as you

MCP tools, full mode: register, file, withdraw, inbox, mine, show, claim, release, close, list,
propose, brief. A session brains already knows, because it registered, because `brains fresh`
registered it, or because brains recognized its conversation, isn't offered register. Fresh eyes:
file, withdraw, mine, show, brief. Commands and tools take a code by its ending (the four
characters agents cite) when nothing else in view ends that way, and name the candidates when
something does. A repo can be given by its name alone once brains knows it, unless two owners
have one by that name.

Lists show an open task that carries a note as "open, noted": in the audit loop, that means the
fixer confirmed it and put it down with a sketch of the fix.

## Not built

Initiatives and their links; `brains clean`, to purge old actions and history; any way to read the
action log and the history tables, which are written but nothing shows; a recurrence view, of how
often each settled finding is raised again; and a real "confirmed" status, should the "noted"
marker prove too little.

## Open questions

- The shape of task.
- Rules for every role that writes code. A belief names one role or all of them, so if a second
  code-writing role appears, its rules are filed twice or the belief needs more than one role.
- More behaviors for a role to pick from. A dreamer, keeping the initiatives and handing work
  down, would need initiative tools that no role gets yet. A builder is probably a fixer handed a
  plan instead of findings.
- Rejecting cycles in contains and blocked_by (would need an insert-time trigger).

## Parked

- message_pointer: a decision plus the ID of the message it was made in. Keeping records of work
  conversations outside the system that holds them is a question of policy before it's a question
  of schema.
- The entity registry.
- initiative_repo.
