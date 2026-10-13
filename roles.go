package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type roleInfo struct {
	pin               int64
	code, description string
	freshEyes         bool
}

// findRole looks a role up by name, in any case. A name no role has is
// refused, naming the roles there are: a task addressed to a role nobody
// holds would sit unread.
func findRole(ex execer, name string) (roleInfo, error) {
	var r roleInfo
	name = strings.ToLower(strings.TrimSpace(name))
	err := ex.QueryRow("select pin, code_name, description, is_fresh_eyes from role where code_name = ?", name).
		Scan(&r.pin, &r.code, &r.description, &r.freshEyes)
	if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	var known string
	if err := ex.QueryRow(`select coalesce(group_concat(code_name, ', '), '')
		from (select code_name from role order by code_name)`).Scan(&known); err != nil {
		return r, err
	}
	switch {
	case known == "":
		return r, errors.New("no roles are defined yet; the owner defines them with brains role")
	case name == "":
		return r, fmt.Errorf("role is required; the roles are %s", known)
	}
	return r, fmt.Errorf("no role %q; the roles are %s", name, known)
}

// defineRole adds a role, or changes one. A new role needs a description; a
// change touches only what's given. Only you define roles.
func (s *store) defineRole(name, description string, freshEyes *bool) (created bool, err error) {
	name, description = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(description)
	if name == "" || slug(name) != name || name[0] < 'a' || name[0] > 'z' {
		return false, fmt.Errorf("role %q: a name is lowercase letters and digits, with single hyphens between, starting with a letter, at most 20 characters", name)
	}
	var fresh any
	if freshEyes != nil {
		fresh = *freshEyes
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var pin int64
	var id []byte
	err = tx.QueryRow("select pin, id from role where code_name = ?", name).Scan(&pin, &id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if description == "" {
			return false, errors.New("a new role needs a description: what it's for, as its sessions should hear it")
		}
		if n := 0; tx.QueryRow("select count(*) from session where nickname = ? and "+sessionInView, name).Scan(&n) == nil && n > 0 {
			return false, fmt.Errorf("role %q: a session in view has that nickname", name)
		}
		id, created = newUUID(), true
		if _, err := tx.Exec("insert into role (id, code_name, description, is_fresh_eyes) values (?, ?, ?, coalesce(?, 0))",
			id, name, description, fresh); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	case description == "" && freshEyes == nil:
		return false, errors.New("nothing to change: give a description, fresh eyes, or both")
	default:
		if _, err := tx.Exec(`update role set description = coalesce(?, description), is_fresh_eyes = coalesce(?, is_fresh_eyes),
			updated_at = datetime('now') where pin = ?`, nullable(description), fresh, pin); err != nil {
			return false, err
		}
	}
	op := "role.change"
	if created {
		op = "role.define"
	}
	if err := logAction(tx, nil, op, id, ""); err != nil {
		return false, err
	}
	return created, tx.Commit()
}

func (s *store) roles() ([]roleInfo, error) {
	rows, err := s.db.Query("select pin, code_name, description, is_fresh_eyes from role order by code_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []roleInfo
	for rows.Next() {
		var r roleInfo
		if err := rows.Scan(&r.pin, &r.code, &r.description, &r.freshEyes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func renderRoles(rows []roleInfo) string {
	if len(rows) == 0 {
		return "No roles yet. Define one: brains role NAME -description \"...\" [-fresh-eyes]\n"
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(r.code)
		if r.freshEyes {
			b.WriteString("  (fresh eyes)")
		}
		b.WriteByte('\n')
		b.WriteString(wrap(r.description, "    ", 76))
	}
	return b.String()
}
