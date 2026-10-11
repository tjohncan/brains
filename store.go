package main

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// dataDir is the per-user folder for brains: under %LOCALAPPDATA% on
// Windows, so each Windows user gets their own and can't open the other's.
func dataDir() (string, error) {
	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	} else if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		base = xdg
	} else if home, err := os.UserHomeDir(); err == nil {
		base = filepath.Join(home, ".local", "share")
	}
	if base == "" {
		return "", errors.New("can't find a per-user data directory; set BRAINS_DB")
	}
	return filepath.Join(base, "brains"), nil
}

// defaultDBPath is this persona's database: BRAINS_DB if set, otherwise
// brains.db in the per-user data directory.
func defaultDBPath() (string, error) {
	if p := os.Getenv("BRAINS_DB"); p != "" {
		return p, nil
	}
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "brains.db"), nil
}

// openDB opens the database with the settings every connection needs.
// SQLite applies foreign_keys per connection, so it goes in the DSN rather
// than being run once. Transactions begin immediate, taking the write lock
// up front, so two sessions claiming the same task queue up instead of both
// reading it as open.
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + uriPath(path) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// uriPath makes a filesystem path safe to embed in a file: URI.
func uriPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
}

// initDB creates a database at path from nothing: the settings that must
// precede the first table, the embedded schema, then a history__ companion
// for every table. It refuses to touch an existing file, and removes what it
// created if any step fails, so a retry starts clean.
func initDB(path string) (err error) {
	if _, statErr := os.Stat(path); statErr == nil {
		return fmt.Errorf("%s already exists; init only creates new databases", path)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer func() {
		db.Close()
		if err != nil {
			removeDB(path)
		}
	}()

	// auto_vacuum only takes effect if it's set before the first table exists.
	if _, err = db.Exec("pragma auto_vacuum = incremental"); err != nil {
		return fmt.Errorf("auto_vacuum: %w", err)
	}
	var mode string
	if err = db.QueryRow("pragma journal_mode = wal").Scan(&mode); err != nil {
		return fmt.Errorf("journal_mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("journal_mode is %q, want wal", mode)
	}
	if _, err = db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if err = generateHistory(db); err != nil {
		return fmt.Errorf("history tables: %w", err)
	}
	return nil
}

// removeDB deletes a database and the side files WAL mode keeps next to it.
func removeDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
}

// generateHistory gives every table a history__<table> companion and the
// triggers that fill it: each old row, when it changed, and whether it was
// an update (U) or a delete (D).
// Tables that already have one are skipped. action is excluded: it's a log.
func generateHistory(db *sql.DB) error {
	tables, err := strings0(db, `
		select name from sqlite_master
		 where type = 'table'
		   and name not like 'sqlite\_%' escape '\'
		   and name not like 'history\_\_%' escape '\'
		   and name <> 'action'
		   and 'history__' || name not in (select name from sqlite_master where type = 'table')
		 order by name`)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range tables {
		cols, err := columns(tx, table)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		for _, stmt := range historyDDL(table, cols) {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
		}
	}
	return tx.Commit()
}

type column struct{ name, typ string }

func columns(tx *sql.Tx, table string) ([]column, error) {
	rows, err := tx.Query("select name, type from pragma_table_info(?) order by cid", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.typ); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// historyDDL is the history table, its cleanup index, and the two capture
// triggers for one base table. History columns keep the base types but none
// of the constraints, so any past row always fits.
func historyDDL(table string, cols []column) []string {
	h := "history__" + table
	var def, names, olds strings.Builder
	def.WriteString("create table " + quote(h) + " (\n")
	def.WriteString("  history_created_at text not null default (datetime('now'))\n")
	def.WriteString(", history_triggering_action_code text not null\n")
	for _, c := range cols {
		def.WriteString(", " + quote(c.name) + " " + strings.ToLower(c.typ) + "\n")
		names.WriteString(", " + quote(c.name))
		olds.WriteString(", old." + quote(c.name))
	}
	def.WriteString(", history_pin integer primary key autoincrement\n\n")
	def.WriteString(", constraint " + quote("ck_history_"+table+"_action_code") +
		" check(history_triggering_action_code in ('U', 'D'))\n);")

	trigger := func(event, code string) string {
		return "create trigger " + quote("trg_"+h+"_"+event) + " before " + event + " on " + quote(table) + "\n" +
			"begin\n" +
			"  insert into " + quote(h) + " (history_triggering_action_code" + names.String() + ")\n" +
			"  values ('" + code + "'" + olds.String() + ");\n" +
			"end;"
	}
	return []string{
		def.String(),
		"create index " + quote("idx_"+h+"_created_at") + " on " + quote(h) + "(history_created_at);",
		trigger("update", "U"),
		trigger("delete", "D"),
	}
}

func quote(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// strings0 returns the first column of every row.
func strings0(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
