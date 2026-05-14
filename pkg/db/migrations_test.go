package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	d, err := sql.Open("sqlite3", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func columnExists(t *testing.T, d *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := d.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == col {
			return true
		}
	}
	return false
}

func TestAddColumnIfMissing_AddsWhenAbsent(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.Exec("CREATE TABLE foo (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := addColumnIfMissing(d, "foo", "bar", "TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatal(err)
	}
	if !columnExists(t, d, "foo", "bar") {
		t.Fatal("column bar not added")
	}
}

func TestAddColumnIfMissing_NoopWhenPresent(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.Exec("CREATE TABLE foo (id INTEGER, bar TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := addColumnIfMissing(d, "foo", "bar", "TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatal(err)
	}
	if err := addColumnIfMissing(d, "foo", "bar", "TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatal(err)
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	d := openTestDB(t)
	if err := runMigrations(d); err != nil {
		t.Fatalf("first runMigrations: %v", err)
	}
	if err := runMigrations(d); err != nil {
		t.Fatalf("second runMigrations: %v", err)
	}
	if !columnExists(t, d, "wiki_nodes", "ai_label") {
		t.Fatal("wiki_nodes.ai_label not present after runMigrations")
	}
}
