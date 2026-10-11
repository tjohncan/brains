-- ============================================================================
-- brains - Core Schema DDL (SQLite)
-- ============================================================================
-- One database per persona, created by `brains init` under %LOCALAPPDATA%\brains\.
-- Before this file runs, init sets auto_vacuum = incremental (it must precede
-- the first table) and journal_mode = wal. Every connection sets
-- foreign_keys = on.
--
-- After this file runs, init generates a history__<table> companion, with
-- update and delete capture triggers, for every table except action.
--
-- Conventions:
--   pin         sequential integer key: internal, fast joins
--   id          UUID: durable identity, and what action.target_id points at
--   code_name   the readable handle you and agents type
--   created_at  column default; omit from inserts
--   updated_at  set explicitly by every UPDATE
--   A null session anywhere means you, acting directly.
-- ============================================================================

begin;

-- ============================================================================
-- REFERENCE - Brands and the technologies sessions run in
-- ============================================================================

create table brand (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, id blob not null
, code_name text not null
, display_name text not null
, note text

, constraint uix_brand_id unique(id)
, constraint uix_brand_code_name unique(code_name)
, constraint ck_brand_code_name_len check(length(code_name) <= 100)
, constraint ck_brand_display_name_len check(length(display_name) <= 200)
, constraint ck_brand_note_len check(length(note) <= 2000)
);

create table technology (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, code_name text not null
, display_name text not null
, brand_pin integer not null
, is_active integer not null default 1
, description text not null

, constraint uix_technology_code_name unique(code_name)
, constraint ck_technology_is_active check(is_active in (0, 1))
, constraint ck_technology_code_name_len check(length(code_name) <= 100)
, constraint ck_technology_display_name_len check(length(display_name) <= 200)
, constraint ck_technology_description_len check(length(description) <= 2000)
, constraint fk_technology_brand foreign key(brand_pin) references brand(pin)
);

insert into brand (id, code_name, display_name) values
  (randomblob(16), 'anthropic', 'Anthropic');

insert into technology (code_name, display_name, brand_pin, description) values
  ('claude-code', 'Claude Code', (select pin from brand where code_name = 'anthropic'), 'Agentic coding sessions in a terminal')
, ('claude-in-excel', 'Claude in Excel', (select pin from brand where code_name = 'anthropic'), 'Claude sessions inside Excel workbooks');

-- ============================================================================
-- WORK - Initiatives and repositories
-- ============================================================================

create table initiative (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, id blob not null
, code_name text not null
, display_name text not null
, status text not null default 'active'
, note text

, constraint uix_initiative_id unique(id)
, constraint uix_initiative_code_name unique(code_name)
, constraint ck_initiative_status check(status in ('active', 'parked', 'done'))
, constraint ck_initiative_code_name_len check(length(code_name) <= 100)
, constraint ck_initiative_display_name_len check(length(display_name) <= 200)
, constraint ck_initiative_note_len check(length(note) <= 2000)
);

create table association_type (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, code_name text not null
, display_name text not null
, inverse_display_name text not null
, is_symmetric integer not null default 0

, constraint uix_association_type_code_name unique(code_name)
, constraint uix_association_type_pin_symmetric unique(pin, is_symmetric)
, constraint ck_association_type_is_symmetric check(is_symmetric in (0, 1))
, constraint ck_association_type_symmetric_label check(is_symmetric = 0 or inverse_display_name = display_name)
);

insert into association_type (code_name, display_name, inverse_display_name, is_symmetric) values
  ('contains', 'contains', 'is part of', 0)
, ('blocked_by', 'is blocked by', 'blocks', 0)
, ('relates_to', 'relates to', 'relates to', 1);

-- Typed links between initiatives. The composite key copies is_symmetric from
-- the type (the client_resource_server trick), so a CHECK can store symmetric
-- links once, lower pin first. v_initiative_association shows both ends.
create table initiative_association (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, initiative_pin integer not null
, association_type_pin integer not null
, related_initiative_pin integer not null
, is_symmetric integer not null

, constraint uix_initiative_association unique(initiative_pin, association_type_pin, related_initiative_pin)
, constraint ck_initiative_association_not_self check(initiative_pin <> related_initiative_pin)
, constraint ck_initiative_association_symmetric_order check(is_symmetric = 0 or initiative_pin < related_initiative_pin)
, constraint fk_initiative_association_initiative foreign key(initiative_pin) references initiative(pin)
, constraint fk_initiative_association_related foreign key(related_initiative_pin) references initiative(pin)
, constraint fk_initiative_association_type foreign key(association_type_pin, is_symmetric) references association_type(pin, is_symmetric)
);

-- "What links to X" filters on the related side; the unique index leads with
-- the other, so the leftmost-prefix rule means it can't serve that query.
create index idx_initiative_association_related
  on initiative_association(related_initiative_pin);

create view v_initiative_association as
select ia.initiative_pin, t.display_name as relation, ia.related_initiative_pin
  from initiative_association ia
  join association_type t on t.pin = ia.association_type_pin
union all
select ia.related_initiative_pin, t.inverse_display_name, ia.initiative_pin
  from initiative_association ia
  join association_type t on t.pin = ia.association_type_pin;

create table repo (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, id blob not null
, owner text not null
, name text not null
, url text
, note text

, constraint uix_repo_id unique(id)
, constraint uix_repo_owner_name unique(owner, name)
, constraint ck_repo_owner_len check(length(owner) <= 100)
, constraint ck_repo_name_len check(length(name) <= 100)
, constraint ck_repo_url_len check(length(url) <= 2000)
, constraint ck_repo_note_len check(length(note) <= 2000)
);

-- ============================================================================
-- ROLES - What a session is for. Each persona defines its own; none are seeded
-- ============================================================================
-- A role picks from the behaviors the code offers: whether its sessions work
-- with fresh eyes, what they're told the role is for, and, through belief,
-- what they're told to hold to. A different brief for the same behavior is a
-- lens in the prompt, not another role.

create table role (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, id blob not null
, code_name text not null                   -- also the word in its sessions' codes
, description text not null                 -- what the role is for; its sessions are told this at the start
, is_fresh_eyes integer not null default 0  -- 1: its sessions may file, and follow up on their own filings, nothing more

, constraint uix_role_id unique(id)
, constraint uix_role_code_name unique(code_name)
, constraint ck_role_code_name check(length(code_name) between 1 and 20 and code_name not glob '*[^a-z0-9-]*')
, constraint ck_role_description_len check(length(description) <= 2000)
, constraint ck_role_is_fresh_eyes check(is_fresh_eyes in (0, 1))
);

-- ============================================================================
-- SESSIONS - Working sessions in any technology
-- ============================================================================

create table session (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))  -- when brains learned of it
, updated_at text not null default (datetime('now'))

, id blob not null
, code_name text not null
, nickname text                                        -- what you call it, as in auditor1: unique among sessions in view, not forever
, technology_pin integer not null
, external_id text                                     -- the technology's own id (Claude Code: session UUID)
, role_pin integer
, started_at text not null                             -- when it actually began
, note text

, constraint uix_session_id unique(id)
, constraint uix_session_code_name unique(code_name)
, constraint uix_session_external_id unique(technology_pin, external_id)
, constraint ck_session_code_name_len check(length(code_name) <= 100)
, constraint ck_session_nickname check(nickname is null or (length(nickname) <= 30 and nickname glob '[a-z]*' and nickname not glob '*[^a-z0-9-]*'))
, constraint ck_session_external_id_len check(length(external_id) <= 200)
, constraint ck_session_note_len check(length(note) <= 2000)
, constraint fk_session_technology foreign key(technology_pin) references technology(pin)
, constraint fk_session_role foreign key(role_pin) references role(pin)
);

create table session_initiative (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, session_pin integer not null
, initiative_pin integer not null

, constraint uix_session_initiative unique(session_pin, initiative_pin)
, constraint fk_session_initiative_session foreign key(session_pin) references session(pin)
, constraint fk_session_initiative_initiative foreign key(initiative_pin) references initiative(pin)
);

create table session_repo (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, session_pin integer not null
, repo_pin integer not null

, constraint uix_session_repo unique(session_pin, repo_pin)
, constraint fk_session_repo_session foreign key(session_pin) references session(pin)
, constraint fk_session_repo_repo foreign key(repo_pin) references repo(pin)
);

-- ============================================================================
-- TASKS - Provisional: the shape settles once handoffs run through it
-- ============================================================================

create table task_type (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, code_name text not null
, display_name text not null
, description text not null

, constraint uix_task_type_code_name unique(code_name)
, constraint ck_task_type_code_name_len check(length(code_name) <= 100)
, constraint ck_task_type_display_name_len check(length(display_name) <= 200)
, constraint ck_task_type_description_len check(length(description) <= 2000)
);

insert into task_type (code_name, display_name, description) values
  ('handoff', 'Handoff', 'Work passed to another session, or to a role on a repo')
, ('audit_finding', 'Audit finding', 'Confirm or deny, then fix')
, ('improvement', 'Improvement', 'A change worth making that isn''t a defect');

-- Claiming is: update task set status = 'claimed', claimed_by_session_pin = ?
-- where pin = ? and status = 'open'. A second claimant updates zero rows.
create table task (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, id blob not null
, code_name text not null
, task_type_pin integer not null
, title text not null
, body text
, severity text                             -- critical, high, medium, low, info
, status text not null default 'open'
, outcome text                              -- how it ended, once done
, duplicate_of_task_pin integer             -- outcome duplicate: the task it repeats
, replaces_task_pin integer                 -- the task this one supersedes, closed as this was filed
, resolution text                           -- why it ended as it did: what was fixed, or why not
, note text                                 -- where it stands, for whoever picks it up next
, from_session_pin integer                  -- null: you
, assignee_session_pin integer
, assignee_role_pin integer                 -- whoever holds this role on repo_pin
, repo_pin integer
, initiative_pin integer
, claimed_by_session_pin integer

, constraint uix_task_id unique(id)
, constraint uix_task_code_name unique(code_name)
, constraint ck_task_status check(status in ('open', 'claimed', 'done', 'dropped'))
, constraint ck_task_severity check(severity is null or severity in ('critical', 'high', 'medium', 'low', 'info'))
, constraint ck_task_outcome check(outcome is null or (status = 'done' and outcome in ('fixed', 'proven_false', 'accepted_as_designed', 'deferred', 'duplicate')))
, constraint ck_task_duplicate check((outcome is 'duplicate') = (duplicate_of_task_pin is not null))
, constraint ck_task_resolution check(outcome is null or outcome = 'duplicate' or resolution is not null)
, constraint ck_task_resolution_len check(length(resolution) <= 20000)
, constraint ck_task_note_len check(length(note) <= 20000)
, constraint ck_task_recipient check(assignee_session_pin is not null or (assignee_role_pin is not null and repo_pin is not null))
, constraint ck_task_code_name_len check(length(code_name) <= 100)
, constraint ck_task_title_len check(length(title) <= 200)
, constraint ck_task_body_len check(length(body) <= 200000)
, constraint fk_task_task_type foreign key(task_type_pin) references task_type(pin)
, constraint fk_task_from foreign key(from_session_pin) references session(pin)
, constraint fk_task_assignee foreign key(assignee_session_pin) references session(pin)
, constraint fk_task_assignee_role foreign key(assignee_role_pin) references role(pin)
, constraint fk_task_repo foreign key(repo_pin) references repo(pin)
, constraint fk_task_initiative foreign key(initiative_pin) references initiative(pin)
, constraint fk_task_claimed_by foreign key(claimed_by_session_pin) references session(pin)
, constraint fk_task_duplicate_of foreign key(duplicate_of_task_pin) references task(pin)
, constraint fk_task_replaces foreign key(replaces_task_pin) references task(pin)
);

-- Inboxes. Partial, so finished tasks aren't indexed.
create index idx_task_open_assignee
  on task(assignee_session_pin)
  where status in ('open', 'claimed');

create index idx_task_open_role
  on task(repo_pin, assignee_role_pin)
  where status in ('open', 'claimed');

-- ============================================================================
-- BELIEFS - Provisional: agents propose, you accept
-- ============================================================================
-- A session is told, when it starts, every accepted belief for its role or
-- every role, on its repos or every repo. Sessions only ever see accepted
-- beliefs. Rejecting a proposal deletes it (history keeps the record); a
-- superseded belief is retired, not deleted, so it stays queryable after
-- history retention purges. A belief that sharpens another names it in
-- replaces_belief_pin, and accepting the new one retires the old, so no
-- session is told two versions of one rule.

create table belief (
  pin integer primary key autoincrement
, created_at text not null default (datetime('now'))
, updated_at text not null default (datetime('now'))

, id blob not null
, code_name text not null
, statement text not null
, status text not null default 'proposed'
, note text                                 -- why it's held: where it came from, what it prevents
, from_session_pin integer                  -- who proposed it; null: you
, role_pin integer                          -- null: every role
, repo_pin integer                          -- null: every repo
, replaces_belief_pin integer               -- the belief this one supersedes, retired when this is accepted

, constraint uix_belief_id unique(id)
, constraint uix_belief_code_name unique(code_name)
, constraint ck_belief_status check(status in ('proposed', 'accepted', 'retired'))
, constraint ck_belief_code_name_len check(length(code_name) <= 100)
, constraint ck_belief_statement_len check(length(statement) <= 2000)
, constraint ck_belief_note_len check(length(note) <= 2000)
, constraint fk_belief_from foreign key(from_session_pin) references session(pin)
, constraint fk_belief_role foreign key(role_pin) references role(pin)
, constraint fk_belief_repo foreign key(repo_pin) references repo(pin)
, constraint fk_belief_replaces foreign key(replaces_belief_pin) references belief(pin)
);

create index idx_belief_accepted
  on belief(role_pin, repo_pin)
  where status = 'accepted';

-- ============================================================================
-- LOGGING - Every operation through brains
-- ============================================================================
-- A log, not a table of records: no primary key, no foreign keys,
-- append-only, purged by age. Sessions must say why; you don't have to.

create table action (
  session_pin integer                        -- null: you, directly
, acted_at text not null default (datetime('now'))
, operation text not null                    -- e.g. task.add, task.claim, belief.propose
, target_id blob                             -- the id of the row it touched
, purpose text

, constraint ck_action_session_purpose check(session_pin is null or purpose is not null)
);

create index idx_action_acted_at
  on action(acted_at);

create index idx_action_target
  on action(target_id);

commit;
