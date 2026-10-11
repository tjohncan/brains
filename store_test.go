package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var baseTables = []string{
	"action", "association_type", "belief", "brand", "initiative",
	"initiative_association", "repo", "role", "session", "session_initiative",
	"session_repo", "task", "task_type", "technology",
}

// newDB runs init into a fresh temporary directory and opens the result.
func newDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brains.db")
	if err := initDB(path); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func one(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s
}

func list(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	out, err := strings0(db, query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

func TestInitBuildsFromNothing(t *testing.T) {
	db, path := newDB(t)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no database file at %s: %v", path, err)
	}
	t.Logf("sqlite %s", one(t, db, "select sqlite_version()"))

	for pragma, want := range map[string]string{
		"pragma auto_vacuum":     "2", // incremental
		"pragma journal_mode":    "wal",
		"pragma foreign_keys":    "1",
		"pragma integrity_check": "ok",
	} {
		if got := one(t, db, pragma); got != want {
			t.Errorf("%s = %s, want %s", pragma, got, want)
		}
	}

	tables := list(t, db, `select name from sqlite_master where type = 'table'
		and name not like 'sqlite\_%' escape '\' and name not like 'history\_\_%' escape '\' order by name`)
	if !reflect.DeepEqual(tables, baseTables) {
		t.Fatalf("tables = %v\nwant %v", tables, baseTables)
	}

	for _, table := range baseTables {
		h := "history__" + table
		exists := one(t, db, "select count(*) from sqlite_master where type = 'table' and name = ?", h) == "1"
		if table == "action" {
			if exists {
				t.Error("action is a log and must not have a history table")
			}
			continue
		}
		if !exists {
			t.Errorf("%s has no history table", table)
			continue
		}
		base := list(t, db, "select name from pragma_table_info(?) order by cid", table)
		want := append(append([]string{"history_created_at", "history_triggering_action_code"}, base...), "history_pin")
		if got := list(t, db, "select name from pragma_table_info(?) order by cid", h); !reflect.DeepEqual(got, want) {
			t.Errorf("%s columns = %v\nwant %v", h, got, want)
		}
		triggers := list(t, db, "select name from sqlite_master where type = 'trigger' and tbl_name = ? order by name", table)
		if want := []string{"trg_" + h + "_delete", "trg_" + h + "_update"}; !reflect.DeepEqual(triggers, want) {
			t.Errorf("%s triggers = %v, want %v", table, triggers, want)
		}
	}

	for query, want := range map[string]string{
		"select count(*) from brand":            "1",
		"select count(*) from technology":       "2",
		"select count(*) from association_type": "3",
		"select count(*) from task_type":        "3",
	} {
		if got := one(t, db, query); got != want {
			t.Errorf("%s = %s, want %s", query, got, want)
		}
	}
}

func TestInitRefusesExistingDatabase(t *testing.T) {
	db, path := newDB(t)
	mustExec(t, db, "insert into initiative (id, code_name, display_name) values (randomblob(16), 'keep', 'Keep me')")
	if err := initDB(path); err == nil {
		t.Fatal("a second init succeeded; it must refuse an existing file")
	}
	if one(t, db, "select count(*) from initiative where code_name = 'keep'") != "1" {
		t.Error("the refused init changed the existing database")
	}
}

func TestHistoryRecordsUpdatesAndDeletes(t *testing.T) {
	db, _ := newDB(t)
	mustExec(t, db, "insert into initiative (id, code_name, display_name) values (randomblob(16), 'h', 'first')")
	mustExec(t, db, "update initiative set display_name = 'second', updated_at = datetime('now') where code_name = 'h'")
	mustExec(t, db, "delete from initiative where code_name = 'h'")
	got := list(t, db, "select history_triggering_action_code || ':' || display_name from history__initiative order by history_pin")
	if want := []string{"U:first", "D:second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db, _ := newDB(t)
	_, err := db.Exec("insert into session (id, code_name, technology_pin, started_at) values (randomblob(16), 's', 999, datetime('now'))")
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("session with a missing technology: err = %v, want a foreign key failure", err)
	}
}

func TestSymmetricLinksAreStoredOnce(t *testing.T) {
	db, _ := newDB(t)
	mustExec(t, db, "insert into initiative (id, code_name, display_name) values (randomblob(16), 'a', 'A'), (randomblob(16), 'b', 'B'), (randomblob(16), 'c', 'C')")
	link := func(from int, kind string, to int, symmetric string) error {
		_, err := db.Exec(`insert into initiative_association (initiative_pin, association_type_pin, related_initiative_pin, is_symmetric)
			select ?, pin, ?, `+symmetric+` from association_type where code_name = ?`, from, to, kind)
		return err
	}
	if err := link(1, "relates_to", 2, "is_symmetric"); err != nil {
		t.Errorf("a relates_to b, lower pin first: %v", err)
	}
	if err := link(3, "relates_to", 1, "is_symmetric"); err == nil || !strings.Contains(err.Error(), "symmetric_order") {
		t.Errorf("c relates_to a, wrong order: err = %v, want the order check", err)
	}
	if err := link(3, "relates_to", 1, "0"); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("relates_to with a forged flag: err = %v, want a foreign key failure", err)
	}
	if err := link(3, "contains", 1, "is_symmetric"); err != nil {
		t.Errorf("c contains a: %v", err)
	}
	got := list(t, db, `select a.code_name || ' ' || v.relation || ' ' || b.code_name
		from v_initiative_association v
		join initiative a on a.pin = v.initiative_pin
		join initiative b on b.pin = v.related_initiative_pin
		order by 1`)
	want := []string{"a is part of c", "a relates to b", "b relates to a", "c contains a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("both ends = %v, want %v", got, want)
	}
}

func TestSessionsMustSayWhy(t *testing.T) {
	db, _ := newDB(t)
	mustExec(t, db, "insert into action (operation) values ('task.add')") // you: no purpose needed
	if _, err := db.Exec("insert into action (session_pin, operation) values (7, 'task.claim')"); err == nil {
		t.Error("a session action without a purpose was accepted")
	}
	mustExec(t, db, "insert into action (session_pin, operation, purpose) values (7, 'task.claim', 'fixing the cors findings')")
}
