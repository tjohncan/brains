package main

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// store holds the operations brains offers; the CLI and the MCP server both
// call these. A nil session means you, acting directly.
type store struct{ db *sql.DB }

type sessionInfo struct {
	pin        int64
	code, role string
	nickname   string // what you call it, as in auditor1
	freshEyes  bool   // its role's sessions only file, and follow up on their own filings
	externalID string // Claude Code's ID for the conversation, when brains fresh chose it
}

// called is how a session is named to itself: its code, with its nickname.
func (s *sessionInfo) called() string {
	if s.nickname == "" {
		return s.code
	}
	return s.code + " (" + s.nickname + ")"
}

// execer is what *sql.DB and *sql.Tx have in common. With one connection
// open at a time, anything inside a transaction must go through it.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

var (
	taskTypes  = []string{"audit_finding", "handoff", "improvement"} // task_type's rows in schema.sql
	severities = []string{"critical", "high", "medium", "low", "info"}
	outcomes   = []string{"fixed", "proven_false", "accepted_as_designed", "deferred", "duplicate"}
)

func newUUID() []byte {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return b
}

// uuidString writes a UUID the usual way, which is how Claude Code takes one.
func uuidString(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// slug keeps lowercase letters and digits, with single hyphens between runs.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 20 {
		out = strings.TrimRight(out[:20], "-")
	}
	return out
}

// newCode is a readable handle: today's date, a word, and a random ending,
// drawn again while any code in table is the same, or any code in view ends
// the same way, so an ending names one thing among what's in view. Four
// characters give 923,521 endings; should ten draws in a row be taken, the
// ending grows a character.
func newCode(ex execer, table, word, inView string) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	day := time.Now().UTC().Format("20060102")
	for size := 4; size <= 8; size++ {
		for range 10 {
			b := make([]byte, size)
			rand.Read(b)
			for i := range b {
				b[i] = alphabet[int(b[i])%len(alphabet)]
			}
			code := day + "-" + word + "-" + string(b)
			free, err := codeFree(ex, table, inView, code, string(b))
			if err != nil || free {
				return code, err
			}
		}
	}
	return "", errors.New("couldn't find an unused code")
}

// codeFree says whether code is unused in table and no code in view there
// already ends with ending.
func codeFree(ex execer, table, inView, code, ending string) (bool, error) {
	var n int
	err := ex.QueryRow(`select count(*) from `+table+`
		 where code_name = ?1 or (substr(code_name, -length(?2) - 1) = '-' || ?2 and `+inView+`)`, code, ending).Scan(&n)
	return n == 0, err
}

func nullable(s string) any {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return s
}

// requirePurpose enforces the rule that sessions say why they look.
func requirePurpose(sess *sessionInfo, purpose string) error {
	if sess != nil && strings.TrimSpace(purpose) == "" {
		return errors.New("purpose is required: say briefly what you're looking for and why")
	}
	return nil
}

func logAction(ex execer, sess *sessionInfo, op string, target []byte, purpose string) error {
	var pin, t any
	if sess != nil {
		pin = sess.pin
	}
	if target != nil {
		t = target
	}
	_, err := ex.Exec("insert into action (session_pin, operation, target_id, purpose) values (?, ?, ?, ?)",
		pin, op, t, nullable(purpose))
	return err
}

// findRepo looks a repo up by owner/name, or by its name alone when only one
// owner's repo has it, in any case. A repo brains hasn't seen is pin 0.
func findRepo(ex execer, ref string) (pin int64, err error) {
	owner, name, err := splitRepo(ref)
	if err != nil {
		return 0, err
	}
	var n int
	var all string
	err = ex.QueryRow(`select count(*), coalesce(max(pin), 0), coalesce(group_concat(owner || '/' || name, ', '), '')
		  from repo where name = ?1 collate nocase and (?2 = '' or owner = ?2 collate nocase)`, name, owner).
		Scan(&n, &pin, &all)
	if err == nil && n > 1 {
		return 0, fmt.Errorf("repo %q could be %s; give owner/name", ref, all)
	}
	return pin, err
}

// splitRepo reads owner/name, or a name alone, with the owner empty.
func splitRepo(ref string) (owner, name string, err error) {
	owner, name, full := strings.Cut(ref, "/")
	if !full {
		owner, name = "", owner
	}
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if name == "" || (full && owner == "") || strings.ContainsAny(owner+name, "/ \t") {
		return "", "", fmt.Errorf("repo %q: want owner/name", strings.TrimSpace(ref))
	}
	return owner, name, nil
}

// newRepo says whether naming ref would add a repo brains hasn't seen, so a
// command can say so and a misspelling shows.
func newRepo(ex execer, ref string) bool {
	pin, err := findRepo(ex, ref)
	return err == nil && pin == 0
}

// repoName is how brains writes a repo it was given: owner/name, trimmed.
func repoName(ref string) string {
	owner, name, _ := splitRepo(ref)
	return owner + "/" + name
}

// knownRepo is findRepo for reads, where a repo brains hasn't seen is a mistake.
func knownRepo(ex execer, ref string) (int64, error) {
	pin, err := findRepo(ex, ref)
	if err == nil && pin == 0 {
		err = fmt.Errorf("no repo %q", strings.TrimSpace(ref))
	}
	return pin, err
}

// ensureRepo finds a repo, adding it the first time it's named as owner/name.
func ensureRepo(ex execer, ref string) (int64, error) {
	pin, err := findRepo(ex, ref)
	if err != nil || pin != 0 {
		return pin, err
	}
	owner, name, _ := splitRepo(ref)
	if owner == "" {
		return 0, fmt.Errorf("repo %q is new to brains, so give it as owner/name", strings.TrimSpace(ref))
	}
	res, err := ex.Exec("insert into repo (id, owner, name) values (?, ?, ?)", newUUID(), owner, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type registration struct {
	role, repo, note string
	nickname         string // what you'll call it; the role and a number if left out
	externalID       string // the conversation's own ID, when brains fresh starts it and so chooses it
}

// pickNickname is the nickname asked for, if no session in view has it and
// no role is called it, or else the role and the lowest number free, as in
// auditor1. A number comes free again once its session leaves view.
func pickNickname(ex execer, role, asked string) (string, error) {
	taken := func(name string) (bool, error) {
		var n int
		err := ex.QueryRow(`select (select count(*) from session where nickname = ?1 and `+sessionInView+`)
			     + (select count(*) from role where code_name = ?1)`, name).Scan(&n)
		return n > 0, err
	}
	if asked = strings.ToLower(strings.TrimSpace(asked)); asked != "" {
		if slug(asked) != asked || asked[0] < 'a' || asked[0] > 'z' {
			return "", fmt.Errorf("nickname %q: lowercase letters and digits, with single hyphens between, starting with a letter, at most 20 characters", asked)
		}
		if t, err := taken(asked); err != nil || t {
			if err == nil {
				err = fmt.Errorf("nickname %q is taken, by a role or a session in view", asked)
			}
			return "", err
		}
		return asked, nil
	}
	for n := 1; ; n++ {
		name := fmt.Sprintf("%s%d", role, n)
		if t, err := taken(name); err != nil || !t {
			return name, err
		}
	}
}

// register records a new session in a role, working on repo.
func (s *store) register(r registration) (*sessionInfo, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	role, err := findRole(tx, r.role)
	if err != nil {
		return nil, err
	}
	nickname, err := pickNickname(tx, role.code, r.nickname)
	if err != nil {
		return nil, err
	}
	var techPin int64
	if err := tx.QueryRow("select pin from technology where code_name = 'claude-code'").Scan(&techPin); err != nil {
		return nil, fmt.Errorf("technology claude-code: %w", err)
	}
	code, err := newCode(tx, "session", role.code, sessionInView)
	if err != nil {
		return nil, err
	}
	id := newUUID()
	res, err := tx.Exec(`insert into session (id, code_name, nickname, technology_pin, external_id, role_pin, started_at, note)
		values (?, ?, ?, ?, ?, ?, datetime('now'), ?)`, id, code, nickname, techPin, nullable(r.externalID), role.pin, nullable(r.note))
	if err != nil {
		return nil, err
	}
	pin, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.repo) != "" {
		repoPin, err := ensureRepo(tx, r.repo)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec("insert into session_repo (session_pin, repo_pin) values (?, ?)", pin, repoPin); err != nil {
			return nil, err
		}
	}
	sess := &sessionInfo{pin: pin, code: code, role: role.code, nickname: nickname, freshEyes: role.freshEyes,
		externalID: r.externalID}
	if err := logAction(tx, sess, "session.register", id, "registered as "+role.code); err != nil {
		return nil, err
	}
	return sess, tx.Commit()
}

// forgetUnstarted deletes a session brains fresh registered whose server
// never started, as when claude refused its arguments. Nothing happened in
// it, and left in place it would hold its nickname and be what brains resume
// finds. History keeps the row.
func (s *store) forgetUnstarted(sess *sessionInfo) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow("select count(*) from action where session_pin = ? and operation <> 'session.register'",
		sess.pin).Scan(&n); err != nil || n > 0 {
		return false, err
	}
	for _, del := range []string{"delete from session_repo where session_pin = ?",
		"delete from session_initiative where session_pin = ?", "delete from session where pin = ?"} {
		if _, err := tx.Exec(del, sess.pin); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// findSession looks a session up by its nickname or its code, or by an
// ending only it has, among the sessions in view; a full code reaches any.
// Should an old session come back into view under a nickname a newer one
// took, the newer one answers to it.
func findSession(ex execer, typed string) (*sessionInfo, []byte, error) {
	var code string
	err := ex.QueryRow(`select code_name from session where nickname = ? and `+sessionInView+` order by pin desc limit 1`,
		strings.ToLower(strings.TrimSpace(typed))).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		code, err = resolveCode(ex, "session", "session", sessionInView, typed)
	}
	if err != nil {
		return nil, nil, err
	}
	sess := &sessionInfo{}
	var id []byte
	err = ex.QueryRow(`select s.pin, s.id, s.code_name, coalesce(s.nickname, ''), coalesce(r.code_name, ''),
		       coalesce(r.is_fresh_eyes, 0), coalesce(s.external_id, '')
		  from session s
		  left join role r on r.pin = s.role_pin
		 where s.code_name = ?`, code).
		Scan(&sess.pin, &id, &sess.code, &sess.nickname, &sess.role, &sess.freshEyes, &sess.externalID)
	return sess, id, err
}

// sessionByConversation is the session brains recorded for a conversation, or
// nothing when there's none. Claude Code gives every server it starts the
// conversation's own ID, and that's what brains keeps as external_id, so a
// server that wasn't told its session can still recognize it: after its own
// restart, or after Claude Code reopened the conversation by itself.
func sessionByConversation(ex execer, externalID string) (*sessionInfo, []byte, error) {
	var code string
	err := ex.QueryRow("select code_name from session where external_id = ? order by pin desc limit 1",
		strings.TrimSpace(externalID)).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return findSession(ex, code)
}

// resume continues as a session registered earlier, after a restart, in the
// conversation it belongs to. A session with no conversation recorded takes
// this one, so the next restart recognizes it by itself.
func (s *store) resume(code, conversation string) (*sessionInfo, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sess, id, err := findSession(tx, code)
	if err != nil {
		return nil, err
	}
	switch {
	case conversation == "" || sess.externalID == conversation:
	case sess.externalID != "":
		return nil, fmt.Errorf("%s belongs to another conversation; register as a new session instead", sess.called())
	default:
		if _, err := tx.Exec("update session set external_id = ?, updated_at = datetime('now') where pin = ?",
			conversation, sess.pin); err != nil {
			return nil, err
		}
		sess.externalID = conversation
	}
	if err := logAction(tx, sess, "session.resume", id, "resumed after a restart"); err != nil {
		return nil, err
	}
	return sess, tx.Commit()
}

// latestSession is the most recent session in a role on a repo.
func latestSession(ex execer, role, repo string) (*sessionInfo, []byte, error) {
	r, err := findRole(ex, role)
	if err != nil {
		return nil, nil, err
	}
	repoPin, err := knownRepo(ex, repo)
	if err != nil {
		return nil, nil, err
	}
	var code string
	err = ex.QueryRow(`select code_name from session
		 where role_pin = ? and pin in (select session_pin from session_repo where repo_pin = ?)
		 order by started_at desc, pin desc limit 1`, r.pin, repoPin).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("no %s has worked on %s yet", r.code, strings.TrimSpace(repo))
	}
	if err != nil {
		return nil, nil, err
	}
	return findSession(ex, code)
}

// describe tells a session where it works, as in "fixer on owner/name", and
// what its role is for.
func (s *store) describe(sess *sessionInfo) (where, roleDescription string, err error) {
	var repos string
	err = s.db.QueryRow(`select coalesce(r.description, ''),
		       coalesce((select group_concat(p.owner || '/' || p.name, ', ')
		                   from session_repo sr
		                   join repo p on p.pin = sr.repo_pin
		                  where sr.session_pin = s.pin), '')
		  from session s
		  left join role r on r.pin = s.role_pin
		 where s.pin = ?`, sess.pin).Scan(&roleDescription, &repos)
	where = sess.role
	if where == "" {
		where = "no role"
	}
	if repos != "" {
		where += " on " + repos
	}
	return where, roleDescription, err
}

type taskRow struct {
	noted                                       bool // has a note: confirmed and put down, or left with context
	code, typ, status, severity, outcome, title string
	from, to, claimedBy, dupOf, created         string
}

const taskSelect = `
select t.code_name, tt.code_name, t.status, coalesce(t.severity, ''), coalesce(t.outcome, ''), t.title,
       coalesce(fs.code_name, 'you'),
       coalesce(asn.code_name, ar.code_name || ' on ' || r.owner || '/' || r.name, ''),
       coalesce(cb.code_name, ''), coalesce(d.code_name, ''), t.created_at, (t.note is not null)
  from task t
  join task_type tt on tt.pin = t.task_type_pin
  left join session fs on fs.pin = t.from_session_pin
  left join session asn on asn.pin = t.assignee_session_pin
  left join role ar on ar.pin = t.assignee_role_pin
  left join repo r on r.pin = t.repo_pin
  left join session cb on cb.pin = t.claimed_by_session_pin
  left join task d on d.pin = t.duplicate_of_task_pin
 where `

// Handoffs first, since a session starts from one, then most severe first,
// then oldest first.
const taskOrder = `
 order by tt.code_name <> 'handoff', case t.severity when 'critical' then 0 when 'high' then 1 when 'medium' then 2
          when 'low' then 3 when 'info' then 4 else 5 end, t.pin`

func (s *store) queryTasks(where string, args ...any) ([]taskRow, error) {
	rows, err := s.db.Query(taskSelect+where+taskOrder, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []taskRow
	for rows.Next() {
		var r taskRow
		if err := rows.Scan(&r.code, &r.typ, &r.status, &r.severity, &r.outcome, &r.title,
			&r.from, &r.to, &r.claimedBy, &r.dupOf, &r.created, &r.noted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// What's in view: the conditions, on a bare table, for a row to be in lists
// and reachable by its code's ending. A task stays in view while it's open,
// while it stands as a decision (accepted as designed, proven false, or
// deferred), and for 30 days after it closes otherwise; a session, for 30
// days after it last did anything; a belief, until it's retired. Out of view,
// a row is still there and its full code still works. Keeping the set small
// is what keeps the four characters agents cite meaning one thing.
const (
	taskInView = `(status in ('open', 'claimed')
		or outcome in ('accepted_as_designed', 'proven_false', 'deferred')
		or updated_at >= datetime('now', '-30 days'))`
	sessionInView = `(started_at >= datetime('now', '-30 days')
		or pin in (select session_pin from action where acted_at >= datetime('now', '-30 days')))`
	beliefInView = `(status <> 'retired')`
)

// resolveCode turns what someone typed into a full code from table: the code
// itself, or the one code in view that ends with it after a hyphen, since
// agents cite codes by their last four characters. Those aren't unique
// across days, so an ending that fits more than one code is refused, naming
// the candidates.
func resolveCode(ex execer, table, noun, inView, typed string) (string, error) {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return "", fmt.Errorf("which %s? give its code", noun)
	}
	var n, older int
	var code, all string
	err := ex.QueryRow(`select count(*) filter (where code_name = ?1 or `+inView+`),
		       coalesce(max(code_name) filter (where code_name = ?1 or `+inView+`), ''),
		       coalesce(group_concat(code_name, ', ') filter (where code_name = ?1 or `+inView+`), ''),
		       count(*) filter (where code_name <> ?1 and not `+inView+`)
		  from `+table+` where code_name = ?1 or substr(code_name, -length(?1) - 1) = '-' || ?1`, typed).
		Scan(&n, &code, &all, &older)
	switch {
	case err != nil:
		return "", err
	case n == 0 && older > 0:
		return "", fmt.Errorf("no %s %q in view; give the full code of an older one", noun, typed)
	case n == 0:
		return "", fmt.Errorf("no %s %q", noun, typed)
	case n > 1:
		return "", fmt.Errorf("%q could be %s; give the full code", typed, all)
	}
	return code, nil
}

type taskRef struct {
	pin                    int64
	id                     []byte
	code, status, typ      string
	body, resolution, note string
	replaces               string // the code of the task this one supersedes
	from, claimedBy        sql.NullInt64
}

// findTask looks a task up by its code, or by an ending only it has among
// the tasks in view.
func findTask(ex execer, typed string) (taskRef, error) {
	var t taskRef
	code, err := resolveCode(ex, "task", "task", taskInView, typed)
	if err != nil {
		return t, err
	}
	err = ex.QueryRow(`select t.pin, t.id, t.code_name, t.status, tt.code_name, coalesce(t.body, ''),
		coalesce(t.resolution, ''), coalesce(t.note, ''),
		coalesce((select code_name from task p where p.pin = t.replaces_task_pin), ''),
		t.from_session_pin, t.claimed_by_session_pin
		  from task t
		  join task_type tt on tt.pin = t.task_type_pin
		 where t.code_name = ?`, code).
		Scan(&t.pin, &t.id, &t.code, &t.status, &t.typ, &t.body, &t.resolution, &t.note, &t.replaces,
			&t.from, &t.claimedBy)
	return t, err
}

func (t taskRef) filedBy(sess *sessionInfo) bool {
	return sess != nil && t.from.Valid && t.from.Int64 == sess.pin
}

// inbox is what's addressed to this session, or to its role on its repos,
// and not yet done.
func (s *store) inbox(sess *sessionInfo, purpose string) ([]taskRow, error) {
	if err := requirePurpose(sess, purpose); err != nil {
		return nil, err
	}
	if err := logAction(s.db, sess, "task.inbox", nil, purpose); err != nil {
		return nil, err
	}
	return s.queryTasks(`t.status in ('open', 'claimed')
		and (t.assignee_session_pin = ?1
		  or (t.assignee_role_pin = (select role_pin from session where pin = ?1)
		      and t.repo_pin in (select repo_pin from session_repo where session_pin = ?1)))`,
		sess.pin)
}

// mine is everything this session filed, with how each one ended.
func (s *store) mine(sess *sessionInfo, purpose string) ([]taskRow, error) {
	if err := requirePurpose(sess, purpose); err != nil {
		return nil, err
	}
	if err := logAction(s.db, sess, "task.mine", nil, purpose); err != nil {
		return nil, err
	}
	return s.queryTasks("t.from_session_pin = ?", sess.pin)
}

// list is every task in view, optionally only one repo's or one type's, by
// status: active (open or claimed) unless asked otherwise. With old, it
// includes the tasks out of view too.
func (s *store) list(sess *sessionInfo, purpose, status, repo, typ string, old bool) ([]taskRow, error) {
	if err := requirePurpose(sess, purpose); err != nil {
		return nil, err
	}
	where, args := []string{"1 = 1"}, []any{}
	if !old {
		where = append(where, "t.pin in (select pin from task where "+taskInView+")")
	}
	switch status = strings.TrimSpace(status); status {
	case "", "active":
		where = append(where, "t.status in ('open', 'claimed')")
	case "all":
	case "open", "claimed", "done", "dropped":
		where = append(where, "t.status = ?")
		args = append(args, status)
	default:
		return nil, fmt.Errorf("status %q: want active, open, claimed, done, dropped, or all", status)
	}
	if strings.TrimSpace(repo) != "" {
		pin, err := knownRepo(s.db, repo)
		if err != nil {
			return nil, err
		}
		where = append(where, "t.repo_pin = ?")
		args = append(args, pin)
	}
	if typ = strings.TrimSpace(typ); typ != "" {
		if !slices.Contains(taskTypes, typ) {
			return nil, fmt.Errorf("type %q: want one of %s", typ, strings.Join(taskTypes, ", "))
		}
		where = append(where, "tt.code_name = ?")
		args = append(args, typ)
	}
	if err := logAction(s.db, sess, "task.list", nil, purpose); err != nil {
		return nil, err
	}
	return s.queryTasks(strings.Join(where, " and "), args...)
}

type taskDetail struct {
	taskRow
	body, resolution, note, replaces string
}

// show is one task in full. With ownOnly (fresh eyes), only a task this
// session filed.
func (s *store) show(sess *sessionInfo, code, purpose string, ownOnly bool) (*taskDetail, error) {
	if err := requirePurpose(sess, purpose); err != nil {
		return nil, err
	}
	ref, err := findTask(s.db, code)
	if err != nil {
		return nil, err
	}
	if ownOnly && !ref.filedBy(sess) {
		return nil, errors.New("with fresh eyes you can only open tasks you filed")
	}
	rows, err := s.queryTasks("t.pin = ?", ref.pin)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no task %q", code)
	}
	d := &taskDetail{taskRow: rows[0], body: ref.body, resolution: ref.resolution, note: ref.note,
		replaces: ref.replaces}
	return d, logAction(s.db, sess, "task.show", ref.id, purpose)
}

type fileInput struct{ typ, title, body, severity, toRole, repo, toSession, replaces string }

// file creates a task addressed to a role on a repo, or to one session. With
// replaces, it supersedes a task of the same type: that one closes as this
// one is filed, so a chain of handoffs leaves one open, and whoever reads it
// needs nothing earlier.
func (s *store) file(sess *sessionInfo, in fileInput) (string, error) {
	in.typ, in.title = strings.TrimSpace(in.typ), strings.TrimSpace(in.title)
	in.severity, in.toRole = strings.TrimSpace(in.severity), strings.TrimSpace(in.toRole)
	in.repo, in.toSession = strings.TrimSpace(in.repo), strings.TrimSpace(in.toSession)
	in.replaces = strings.TrimSpace(in.replaces)
	if in.title == "" {
		return "", errors.New("title is required")
	}
	if in.typ == "" {
		in.typ = "audit_finding"
	}
	if in.severity != "" && !slices.Contains(severities, in.severity) {
		return "", fmt.Errorf("severity %q: want one of %s", in.severity, strings.Join(severities, ", "))
	}
	if in.toSession == "" && (in.toRole == "" || in.repo == "") {
		return "", errors.New("address it to to_role plus repo, or to to_session")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var typePin int64
	if err := tx.QueryRow("select pin from task_type where code_name = ?", in.typ).Scan(&typePin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("type %q: want one of %s", in.typ, strings.Join(taskTypes, ", "))
		}
		return "", err
	}
	var repoPin, toPin, rolePin, from any
	if in.repo != "" {
		p, err := ensureRepo(tx, in.repo)
		if err != nil {
			return "", err
		}
		repoPin = p
	}
	if in.toSession != "" {
		to, _, err := findSession(tx, in.toSession)
		if err != nil {
			return "", err
		}
		if to.freshEyes {
			return "", fmt.Errorf("%s works with fresh eyes, so it has no inbox to read this in", to.called())
		}
		toPin = to.pin
	}
	if in.toRole != "" {
		role, err := findRole(tx, in.toRole)
		if err != nil {
			return "", err
		}
		if role.freshEyes {
			return "", fmt.Errorf("the %s role works with fresh eyes, so it has no inbox to read this in", role.code)
		}
		rolePin = role.pin
	}
	if sess != nil {
		from = sess.pin
	}
	var superseded taskRef
	var replacedPin any
	if in.replaces != "" {
		if superseded, err = findTask(tx, in.replaces); err != nil {
			return "", err
		}
		if superseded.typ != in.typ {
			return "", fmt.Errorf("%s is %s, not %s: a task can only replace one of its own type",
				superseded.code, superseded.typ, in.typ)
		}
		if superseded.status != "open" && superseded.status != "claimed" {
			return "", fmt.Errorf("%s is already %s, so there's nothing to replace", superseded.code, superseded.status)
		}
		// A session replaces what it filed or what was addressed to it, such
		// as the handoff it started from.
		var ours bool
		if err := tx.QueryRow(`select ?2 is null or from_session_pin = ?2 or assignee_session_pin = ?2
			or (assignee_role_pin = (select role_pin from session where pin = ?2)
			    and repo_pin in (select repo_pin from session_repo where session_pin = ?2))
			  from task where pin = ?1`, superseded.pin, from).Scan(&ours); err != nil {
			return "", err
		}
		if !ours {
			return "", fmt.Errorf("%s wasn't filed by or addressed to this session, so it can't replace it", superseded.code)
		}
		replacedPin = superseded.pin
	}
	word := "finding"
	if in.typ != "audit_finding" {
		word = slug(in.typ)
	}
	code, err := newCode(tx, "task", word, taskInView)
	if err != nil {
		return "", err
	}
	id := newUUID()
	if _, err := tx.Exec(`insert into task (id, code_name, task_type_pin, title, body, severity,
		from_session_pin, assignee_session_pin, assignee_role_pin, repo_pin, replaces_task_pin)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, code, typePin, in.title, nullable(in.body), nullable(in.severity),
		from, toPin, rolePin, repoPin, replacedPin); err != nil {
		return "", err
	}
	if err := logAction(tx, sess, "task.file", id, "filed: "+in.title); err != nil {
		return "", err
	}
	if replacedPin != nil {
		if _, err := tx.Exec(`update task set status = 'done', resolution = ?,
			claimed_by_session_pin = coalesce(claimed_by_session_pin, ?), updated_at = datetime('now')
			where pin = ?`, "Replaced by "+code+".", from, superseded.pin); err != nil {
			return "", err
		}
		if err := logAction(tx, sess, "task.close", superseded.id, "closed: replaced by "+code); err != nil {
			return "", err
		}
	}
	return code, tx.Commit()
}

// claim takes an open task. The transaction holds the write lock from the
// start, so if two sessions race, the second sees it already claimed. A claim
// marks who's working on a task; it isn't a lock, and close doesn't check it.
func (s *store) claim(sess *sessionInfo, code string) error {
	if sess == nil {
		return errors.New("only a registered session can claim a task")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ref, err := findTask(tx, code)
	if err != nil {
		return err
	}
	switch ref.status {
	case "open":
	case "claimed":
		var by string
		if err := tx.QueryRow(`select coalesce(s.code_name, 'you') from task t
			left join session s on s.pin = t.claimed_by_session_pin where t.pin = ?`, ref.pin).Scan(&by); err != nil {
			return err
		}
		return fmt.Errorf("%s is already claimed by %s", ref.code, by)
	default:
		return fmt.Errorf("%s is %s, not open", ref.code, ref.status)
	}
	if _, err := tx.Exec(`update task set status = 'claimed', claimed_by_session_pin = ?, updated_at = datetime('now')
		where pin = ?`, sess.pin, ref.pin); err != nil {
		return err
	}
	if err := logAction(tx, sess, "task.claim", ref.id, "claimed"); err != nil {
		return err
	}
	return tx.Commit()
}

// release puts down a claimed task without finishing it: back to open for
// someone else, with a note on where it stands. The note it replaces goes
// to history.
func (s *store) release(sess *sessionInfo, code, note string) error {
	if strings.TrimSpace(note) == "" {
		return errors.New("note is required: say where it stands for whoever picks it up")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ref, err := findTask(tx, code)
	if err != nil {
		return err
	}
	if ref.status != "claimed" {
		return fmt.Errorf("%s is %s, not claimed", ref.code, ref.status)
	}
	if sess != nil && (!ref.claimedBy.Valid || ref.claimedBy.Int64 != sess.pin) {
		return fmt.Errorf("%s is claimed by another session; only it can release it", ref.code)
	}
	if _, err := tx.Exec(`update task set status = 'open', claimed_by_session_pin = null, note = ?,
		updated_at = datetime('now') where pin = ?`, strings.TrimSpace(note), ref.pin); err != nil {
		return err
	}
	if err := logAction(tx, sess, "task.release", ref.id, "released"); err != nil {
		return err
	}
	return tx.Commit()
}

// withdraw drops a task its filer no longer stands behind, while it's still
// open: a duplicate of their own, or a finding they've retracted. The reason
// is kept as the resolution.
func (s *store) withdraw(sess *sessionInfo, code, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("reason is required: say why you're withdrawing it")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ref, err := findTask(tx, code)
	if err != nil {
		return err
	}
	if !ref.filedBy(sess) {
		return errors.New("you can only withdraw tasks you filed")
	}
	if ref.status != "open" {
		return fmt.Errorf("%s is %s; only open tasks can be withdrawn", ref.code, ref.status)
	}
	if _, err := tx.Exec(`update task set status = 'dropped', resolution = ?, updated_at = datetime('now')
		where pin = ?`, reason, ref.pin); err != nil {
		return err
	}
	if err := logAction(tx, sess, "task.withdraw", ref.id, "withdrew: "+reason); err != nil {
		return err
	}
	return tx.Commit()
}

// close finishes a task. Every outcome except duplicate needs a resolution
// the filer will see. The close tool tells sessions an audit finding needs an
// outcome, and they give one, but the store doesn't check, so the owner can
// close anything bare. Nor does it check the claim: any session can close a
// claimed task, so a claim left by a session that stopped never blocks the
// next one, and there's nothing to release by hand.
func (s *store) close(sess *sessionInfo, code, outcome, dupOf, resolution string) error {
	outcome, dupOf, resolution = strings.TrimSpace(outcome), strings.TrimSpace(dupOf), strings.TrimSpace(resolution)
	if outcome != "" && !slices.Contains(outcomes, outcome) {
		return fmt.Errorf("outcome %q: want one of %s", outcome, strings.Join(outcomes, ", "))
	}
	if (outcome == "duplicate") != (dupOf != "") {
		return errors.New("outcome duplicate and duplicate_of go together: name the task it repeats")
	}
	if outcome != "" && outcome != "duplicate" && resolution == "" {
		return errors.New("resolution is required: say what was fixed, or why not")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ref, err := findTask(tx, code)
	if err != nil {
		return err
	}
	if ref.status != "open" && ref.status != "claimed" {
		return fmt.Errorf("%s is already %s", ref.code, ref.status)
	}
	var dupPin, by any
	if dupOf != "" {
		dup, err := findTask(tx, dupOf)
		if err != nil {
			return err
		}
		if dup.pin == ref.pin {
			return errors.New("a task can't be a duplicate of itself")
		}
		// A duplicate names the task that carries the outcome: the end of any
		// chain of duplicates. A chain that ends here would be a loop.
		var root int64
		if err := tx.QueryRow(`with recursive chain(pin, next) as (
			select pin, duplicate_of_task_pin from task where pin = ?
			union select t.pin, t.duplicate_of_task_pin from task t join chain c on t.pin = c.next)
			select pin from chain where next is null`, dup.pin).Scan(&root); err != nil {
			return err
		}
		if root == ref.pin {
			return fmt.Errorf("%s is already a duplicate of %s", dup.code, ref.code)
		}
		dupPin = root
	}
	if sess != nil {
		by = sess.pin
	}
	if _, err := tx.Exec(`update task set status = 'done', outcome = ?, duplicate_of_task_pin = ?, resolution = ?,
		claimed_by_session_pin = coalesce(claimed_by_session_pin, ?), updated_at = datetime('now')
		where pin = ?`, nullable(outcome), dupPin, nullable(resolution), by, ref.pin); err != nil {
		return err
	}
	why := "closed"
	if outcome != "" {
		why += " as " + outcome
	}
	if err := logAction(tx, sess, "task.close", ref.id, why); err != nil {
		return err
	}
	return tx.Commit()
}

type sessionRow struct{ code, nickname, role, started, repos string }

// sessions lists the sessions in view, or with old, every one.
func (s *store) sessions(old bool) ([]sessionRow, error) {
	where := "1 = 1"
	if !old {
		where = "s.pin in (select pin from session where " + sessionInView + ")"
	}
	rows, err := s.db.Query(`select s.code_name, coalesce(s.nickname, ''), coalesce(ro.code_name, ''), s.started_at,
		coalesce(group_concat(r.owner || '/' || r.name, ' '), '')
		  from session s
		  left join role ro on ro.pin = s.role_pin
		  left join session_repo sr on sr.session_pin = s.pin
		  left join repo r on r.pin = sr.repo_pin
		 where ` + where + `
		 group by s.pin
		 order by s.pin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.code, &r.nickname, &r.role, &r.started, &r.repos); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func statusText(r taskRow) string {
	switch {
	case r.outcome == "duplicate":
		return r.status + ": duplicate of " + r.dupOf
	case r.outcome != "":
		return r.status + ": " + r.outcome
	case r.status == "claimed" && r.claimedBy != "":
		return r.status + " by " + r.claimedBy
	case r.status == "open" && r.noted:
		return "open, noted"
	}
	return r.status
}

func renderTasks(rows []taskRow) string {
	if len(rows) == 0 {
		return "No tasks.\n"
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tTYPE\tSTATUS\tSEVERITY\tFROM\tTO\tTITLE")
	for _, r := range rows {
		severity := r.severity
		if severity == "" {
			severity = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.code, r.typ, statusText(r), severity, r.from, r.to, r.title)
	}
	w.Flush()
	return b.String()
}

func renderTask(d *taskDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  (%s)\n%s\n\n", d.code, d.typ, d.title)
	fmt.Fprintf(&b, "status:      %s\n", statusText(d.taskRow))
	if d.severity != "" {
		fmt.Fprintf(&b, "severity:    %s\n", d.severity)
	}
	fmt.Fprintf(&b, "from:        %s\nto:          %s\nfiled:       %s UTC\n", d.from, d.to, d.created)
	if d.replaces != "" {
		fmt.Fprintf(&b, "replaces:    %s\n", d.replaces)
	}
	if d.resolution != "" {
		fmt.Fprintf(&b, "resolution:  %s\n", d.resolution)
	}
	if d.note != "" {
		fmt.Fprintf(&b, "note:        %s\n", d.note)
	}
	if d.body != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(d.body, "\n"))
	}
	return b.String()
}

// wrap breaks text into lines of at most width characters, each after indent,
// keeping the line breaks it already has.
func wrap(text, indent string, width int) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(indent)
		n := 0
		for i, word := range strings.Fields(line) {
			w := utf8.RuneCountInString(word)
			switch {
			case i == 0:
			case n+1+w > width:
				b.WriteString("\n" + indent)
				n = 0
			default:
				b.WriteByte(' ')
				n++
			}
			b.WriteString(word)
			n += w
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func renderSessions(rows []sessionRow) string {
	if len(rows) == 0 {
		return "No sessions.\n"
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tNICKNAME\tROLE\tSTARTED (UTC)\tREPOS")
	for _, r := range rows {
		nickname := r.nickname
		if nickname == "" {
			nickname = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.code, nickname, r.role, r.started, r.repos)
	}
	w.Flush()
	return b.String()
}
