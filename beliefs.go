package main

import (
	"errors"
	"fmt"
	"strings"
)

// believe records a belief for a role on a repo; leaving either out means
// every one. From you it's accepted at once. From a session it's a proposal,
// which no session sees until you accept it. With replaces, it supersedes an
// accepted belief, which retires as this one is accepted, so no session is
// told two versions of one rule.
func (s *store) believe(sess *sessionInfo, statement, role, repo, note, replaces string) (string, error) {
	statement, note = strings.TrimSpace(statement), strings.TrimSpace(note)
	if statement == "" {
		return "", errors.New("statement is required: the belief, as a session should read it")
	}
	if sess != nil && note == "" {
		return "", errors.New("note is required: say where this comes from and what it prevents")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var rolePin, repoPin, from any
	if strings.TrimSpace(role) != "" {
		r, err := findRole(tx, role)
		if err != nil {
			return "", err
		}
		rolePin = r.pin
	}
	if strings.TrimSpace(repo) != "" {
		p, err := ensureRepo(tx, repo)
		if err != nil {
			return "", err
		}
		repoPin = p
	}
	status, op := "accepted", "belief.add"
	if sess != nil {
		status, op, from = "proposed", "belief.propose", sess.pin
	}
	var superseded beliefRef
	var replacedPin any
	if strings.TrimSpace(replaces) != "" {
		if superseded, err = findBelief(tx, replaces); err != nil {
			return "", err
		}
		if superseded.status != "accepted" {
			return "", fmt.Errorf("%s is %s, not accepted: only a belief in force can be replaced", superseded.code, superseded.status)
		}
		replacedPin = superseded.pin
	}
	code, err := newCode(tx, "belief", "belief", beliefInView)
	if err != nil {
		return "", err
	}
	id := newUUID()
	if _, err := tx.Exec(`insert into belief (id, code_name, statement, status, note, from_session_pin, role_pin, repo_pin,
		replaces_belief_pin) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, code, statement, status, nullable(note), from, rolePin, repoPin, replacedPin); err != nil {
		return "", err
	}
	if err := logAction(tx, sess, op, id, note); err != nil {
		return "", err
	}
	// Yours is in force at once, so what it replaces goes out of force at once.
	if replacedPin != nil && status == "accepted" {
		if err := retireBelief(tx, superseded, code); err != nil {
			return "", err
		}
	}
	return code, tx.Commit()
}

type beliefRef struct {
	pin          int64
	id           []byte
	code, status string
	replaces     int64 // the belief this one supersedes; 0 for none
}

// findBelief looks a belief up by its code, or by an ending only it has among
// the beliefs in view.
func findBelief(ex execer, typed string) (beliefRef, error) {
	var b beliefRef
	code, err := resolveCode(ex, "belief", "belief", beliefInView, typed)
	if err != nil {
		return b, err
	}
	err = ex.QueryRow("select pin, id, code_name, status, coalesce(replaces_belief_pin, 0) from belief where code_name = ?", code).
		Scan(&b.pin, &b.id, &b.code, &b.status, &b.replaces)
	return b, err
}

// retireBelief takes an accepted belief out of force because successor
// replaces it.
func retireBelief(ex execer, b beliefRef, successor string) error {
	if _, err := ex.Exec("update belief set status = 'retired', updated_at = datetime('now') where pin = ?", b.pin); err != nil {
		return err
	}
	return logAction(ex, nil, "belief.retire", b.id, "retired: replaced by "+successor)
}

// The calls you can make on a belief: the status it must have, and the one it
// moves to. A rejected proposal is deleted; history keeps it.
var beliefMoves = map[string]struct{ from, to string }{
	"accept": {"proposed", "accepted"},
	"reject": {"proposed", ""},
	"retire": {"accepted", "retired"},
}

// settleBelief accepts or rejects a proposal, or retires a belief that no
// longer holds. These are yours alone: no tool offers them to sessions.
// Accepting one that replaces another retires that one, and names it. If
// another replacement retired it first, both are in force, and the warning
// says so.
func (s *store) settleBelief(typed, verdict string) (code, retired, warning string, err error) {
	move, ok := beliefMoves[verdict]
	if !ok {
		return "", "", "", fmt.Errorf("verdict %q: want accept, reject, or retire", verdict)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", "", err
	}
	defer tx.Rollback()
	b, err := findBelief(tx, typed)
	if err != nil {
		return "", "", "", err
	}
	if b.status != move.from {
		return "", "", "", fmt.Errorf("%s is %s, not %s", b.code, b.status, move.from)
	}
	if move.to == "" {
		_, err = tx.Exec("delete from belief where pin = ?", b.pin)
	} else {
		_, err = tx.Exec("update belief set status = ?, updated_at = datetime('now') where pin = ?", move.to, b.pin)
	}
	if err != nil {
		return "", "", "", err
	}
	if err := logAction(tx, nil, "belief."+verdict, b.id, ""); err != nil {
		return "", "", "", err
	}
	if verdict == "accept" && b.replaces != 0 {
		var old beliefRef
		if err := tx.QueryRow("select pin, id, code_name, status from belief where pin = ?", b.replaces).
			Scan(&old.pin, &old.id, &old.code, &old.status); err != nil {
			return "", "", "", err
		}
		if old.status == "accepted" {
			if err := retireBelief(tx, old, b.code); err != nil {
				return "", "", "", err
			}
			retired = old.code
		} else {
			var others string
			if err := tx.QueryRow(`select coalesce(group_concat(code_name, ', '), '') from belief
				 where replaces_belief_pin = ? and status = 'accepted' and pin <> ?`, old.pin, b.pin).Scan(&others); err != nil {
				return "", "", "", err
			}
			warning = old.code + ", which this replaces, was already " + old.status
			if others != "" {
				warning += " in favor of " + others + ", so both are in force; retire one, or merge them"
			}
		}
	}
	return b.code, retired, warning, tx.Commit()
}

// scopedBelief is a belief with the scope it holds in, so what a session is
// told can be grouped the way it's scoped.
type scopedBelief struct{ statement, role, repo string }

// beliefsFor is what a session is told to hold to: every accepted belief for
// its role or every role, on its repos or every repo, broadest first. With no
// session yet, only those for every role on every repo.
func (s *store) beliefsFor(sess *sessionInfo) ([]scopedBelief, error) {
	var pin int64
	if sess != nil {
		pin = sess.pin
	}
	rows, err := s.db.Query(`select b.statement, coalesce(ro.code_name, ''), coalesce(r.owner || '/' || r.name, '')
		  from belief b
		  left join role ro on ro.pin = b.role_pin
		  left join repo r on r.pin = b.repo_pin
		 where b.status = 'accepted'
		   and (b.role_pin is null or b.role_pin = (select role_pin from session where pin = ?1))
		   and (b.repo_pin is null or b.repo_pin in (select repo_pin from session_repo where session_pin = ?1))
		 order by b.role_pin is not null, b.repo_pin is not null, b.pin`, pin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scopedBelief
	for rows.Next() {
		var b scopedBelief
		if err := rows.Scan(&b.statement, &b.role, &b.repo); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type beliefRow struct {
	code, status, statement, note, role, repo, from, replaces string
	added, updated                                            string
}

const beliefSelect = `
select b.code_name, b.status, b.statement, coalesce(b.note, ''),
       coalesce(ro.code_name, ''), coalesce(r.owner || '/' || r.name, ''), coalesce(fs.code_name, ''),
       coalesce((select code_name from belief p where p.pin = b.replaces_belief_pin), ''), b.created_at, b.updated_at
  from belief b
  left join role ro on ro.pin = b.role_pin
  left join repo r on r.pin = b.repo_pin
  left join session fs on fs.pin = b.from_session_pin
 where `

func (s *store) queryBeliefs(where string, args ...any) ([]beliefRow, error) {
	rows, err := s.db.Query(beliefSelect+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []beliefRow
	for rows.Next() {
		var r beliefRow
		if err := rows.Scan(&r.code, &r.status, &r.statement, &r.note, &r.role, &r.repo, &r.from, &r.replaces,
			&r.added, &r.updated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// beliefs lists beliefs in one status (accepted unless asked otherwise), or
// all. Given a role or a repo, only those that reach a session in that role
// or on that repo.
func (s *store) beliefs(status, role, repo string) ([]beliefRow, error) {
	where, args := []string{"1 = 1"}, []any{}
	switch status = strings.TrimSpace(status); status {
	case "", "accepted":
		where = append(where, "b.status = 'accepted'")
	case "proposed", "retired":
		where = append(where, "b.status = ?")
		args = append(args, status)
	case "all":
	default:
		return nil, fmt.Errorf("status %q: want accepted, proposed, retired, or all", status)
	}
	if strings.TrimSpace(role) != "" {
		r, err := findRole(s.db, role)
		if err != nil {
			return nil, err
		}
		where = append(where, "(b.role_pin is null or b.role_pin = ?)")
		args = append(args, r.pin)
	}
	if strings.TrimSpace(repo) != "" {
		pin, err := knownRepo(s.db, repo)
		if err != nil {
			return nil, err
		}
		where = append(where, "(b.repo_pin is null or b.repo_pin = ?)")
		args = append(args, pin)
	}
	return s.queryBeliefs(strings.Join(where, " and ")+`
		 order by case b.status when 'proposed' then 0 when 'accepted' then 1 else 2 end,
		          b.role_pin is not null, b.repo_pin is not null, b.pin`, args...)
}

type beliefDetail struct {
	beliefRow
	replacedBy []string // the beliefs that name this one in replaces, with their status
}

// belief is one belief in full, in any status: what it says and why, who
// proposed it, what it replaces, and what replaces it.
func (s *store) belief(typed string) (*beliefDetail, error) {
	ref, err := findBelief(s.db, typed)
	if err != nil {
		return nil, err
	}
	rows, err := s.queryBeliefs("b.pin = ?", ref.pin)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no belief %q", typed)
	}
	d := &beliefDetail{beliefRow: rows[0]}
	d.replacedBy, err = strings0(s.db, `select code_name || ' (' || status || ')' from belief
		 where replaces_belief_pin = ? order by pin`, ref.pin)
	return d, err
}

func renderBelief(d *beliefDetail) string {
	role, repo, from := d.role, d.repo, d.from
	if role == "" {
		role = "every role"
	}
	if repo == "" {
		repo = "every repo"
	}
	if from == "" {
		from = "you"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  (belief)\n\n", d.code)
	fmt.Fprintf(&b, "status:      %s\nfor:         %s on %s\nfrom:        %s\nadded:       %s UTC\n",
		d.status, role, repo, from, d.added)
	if d.updated != d.added {
		fmt.Fprintf(&b, "updated:     %s UTC\n", d.updated)
	}
	if d.replaces != "" {
		fmt.Fprintf(&b, "replaces:    %s\n", d.replaces)
	}
	if len(d.replacedBy) > 0 {
		fmt.Fprintf(&b, "replaced by: %s\n", strings.Join(d.replacedBy, ", "))
	}
	b.WriteString("\n" + wrap(d.statement, "", 76))
	if d.note != "" {
		b.WriteString("\n" + wrap("why: "+d.note, "", 76))
	}
	return b.String()
}

func renderBeliefs(rows []beliefRow) string {
	if len(rows) == 0 {
		return "No beliefs.\n"
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		role, repo := r.role, r.repo
		if role == "" {
			role = "every role"
		}
		if repo == "" {
			repo = "every repo"
		}
		fmt.Fprintf(&b, "%s  %s  %s on %s", r.code, r.status, role, repo)
		if r.replaces != "" {
			fmt.Fprintf(&b, "  (replaces %s)", r.replaces)
		}
		if r.from != "" {
			fmt.Fprintf(&b, "  (from %s)", r.from)
		}
		b.WriteByte('\n')
		b.WriteString(wrap(r.statement, "    ", 76))
		if r.note != "" {
			b.WriteString(wrap("why: "+r.note, "    ", 76))
		}
	}
	return b.String()
}
