# brains: orientation

How to work with brains day to day, for its owner. DESIGN.md says why it's built this way; this
says how to use it.

## The pieces

- **Sessions** are Claude Code conversations brains knows about. Each has a code for the record
  (`20260514-fixer-k3f9`) and a nickname for you (`fixer1`).
- **Roles** say what a session is for. You define them. The pair to start from is a fixer, which
  works through what's filed, and an auditor, which reads with fresh eyes.
- **Tasks** pass work between sessions: audit findings, handoffs, and improvements. Each is
  addressed to a role on a repo, or to one session.
- **Beliefs** are what sessions are told when they start: rules and facts, each for a role, a
  repo, both, or everyone. Sessions can propose them; only you accept them.

It all lives in one SQLite file, `%LOCALAPPDATA%\brains\brains.db`. Sessions use brains through
its tools; you use the `brains` command.

## Start a session

    brains fresh fixer owner/app "check the handoff"
    brains fresh auditor app "audit the token endpoints"
    brains fresh -as hm auditor app "read it the way a hiring manager would"

- The role comes first, then the repo (owner/name, or just the name once brains knows it), then
  anything for claude, usually the opening prompt. `-as` goes before the role.
- The session is registered before it opens. It's told its code and nickname, what its role is
  for, and every belief that reaches it, and that stays with it through compaction.
- An auditor works with fresh eyes: it can file findings and follow up on its own, and see
  nothing else.
- To come back to one: `brains resume fixer2` by nickname, or `brains resume fixer app` for the
  latest fixer there.
- Claude Code tells brains which conversation it's in, so a session is picked up again by itself:
  when its brains server restarts mid-conversation, and when the conversation is reopened with
  plain `claude --resume` or `-c` instead of `brains resume`. It keeps its role, its beliefs and
  its filings, and doesn't register again.
- A plain `claude` session has brains too. Tell it to register as a role on a repo; that records
  the conversation as well, so `brains resume` reaches it afterwards.
- After compaction, or after beliefs change mid-session, tell a session to call brief: it reads
  its role and beliefs again as brains has them then.

## See what's going on

    brains tasks                         open and claimed tasks, most severe first
    brains tasks -repo app -status all   everything on one repo
    brains task CODE                     one task in full
    brains sessions                      who's been working

- "open, noted" means a fixer confirmed the task and put it down with a sketch of the fix.
- A code can be cut to its ending, its last four characters, for anything in view: open tasks,
  decisions that stand (accepted as designed, proven false, deferred), anything closed in the
  last 30 days, and sessions active in the last 30 days. Older things need the full code, and
  `-old` lists them.

## Do things by hand

    brains file -title "..." -body "..." -to fixer -repo app [-type handoff] [-severity high]
    brains close CODE -outcome fixed -resolution "..."

Sessions do most of the filing and closing. These are for handing a session something yourself,
or tidying up.

`-replaces CODE` files a task that supersedes another of the same type, closing it as the new one
is filed. That's how a chain of handoffs stays one open handoff: each fixer files its own with
`-replaces` set to the one it started from, so the next session reads the latest and nothing
before it. A loose end that keeps being carried forward belongs in its own `-type improvement`
task instead, where it stays open until someone acts on it or declines it.

## Roles

    brains roles
    brains role fixer
    brains role fixer -description "You fix what's filed against the repo. ..."
    brains role reviewer -description "..." -fresh-eyes

- A description is the role's job, written to the session, the way it will hear it.
- A role has to behave differently to earn a row. A different brief is a prompt, not a role: a
  hiring-manager read is an auditor with a lens.

## Beliefs

    brains beliefs                            accepted ones, broadest first
    brains beliefs -role fixer -repo app      exactly what a fixer there is told
    brains beliefs -status proposed           waiting for you
    brains believe "..." [-role R] [-repo REPO] [-replaces CODE] -note "..."
    brains accept CODE...     brains reject CODE...     brains retire CODE...

A good belief:

- is one rule or fact, a sentence or two, written the way a session should read it;
- is scoped as narrowly as it's true: a role, a repo, both, or neither;
- carries a note saying where it came from and what it prevents (the note is for you; sessions
  only see the statement);
- is retired when it stops being true, like a version number after an upgrade.

A session is told the beliefs that existed when it started, so add them before you start the
sessions they're for.

When a belief needs sharpening rather than retiring, write the new one with `-replaces` set to
the old, and the old one retires in its place: `brains believe "..." -replaces CODE`. A session
proposing one does the same, and the old belief retires when you accept the new one, so nobody is
ever told two versions of a rule. `brains beliefs -status proposed` shows which one a proposal
replaces, so you can read the pair before deciding.

## Getting your bearings

Run these in order the first time, and again whenever a repo is new to you.

1. Define your roles, then read them back as a session will hear them: `brains roles`.
2. Add two machine facts sessions would otherwise have to discover. Ideas: where repos are
   cloned (`REPOS/<owner>/<repo>`), what's installed in WSL, which account `gh` is logged in to.
3. Add one code-style preference of your own, for fixers everywhere or for one repo.
4. Add the house rules for one repo, scoped to the repo rather than to a role, so auditors
   reading it cold are held to them too.
5. Check what a session will be told before you start it: `brains beliefs -role fixer -repo app`.
6. Start the round: `brains fresh fixer app "check the handoff"`.
7. Afterwards, review what the session proposed: `brains beliefs -status proposed`. Accept what's
   right and reject what isn't. For one you'd say or scope differently, reject it and `believe`
   your own.

## Where things live

- `%LOCALAPPDATA%\brains\brains.db` is the database on Windows, and
  `$XDG_DATA_HOME/brains/brains.db` elsewhere; `BRAINS_DB` points brains at another file.
- `sqlite3` opens the database for anything the commands don't show yet, like the action log
  (every operation, who did it and why) and the `history__` tables (every earlier version of
  every row). A database made by an older build may need it too, since there are no migrations.
