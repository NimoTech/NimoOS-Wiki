package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
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

func TestMigration_WikiSummariesTableExists(t *testing.T) {
	d, err := Open(":memory:")
	require.NoError(t, err)
	defer d.Close()

	rows, err := d.Query(`PRAGMA table_info(wiki_summaries)`)
	require.NoError(t, err)
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk))
		cols[name] = true
	}
	for _, c := range []string{"path", "summary", "generated_at", "based_on_last_modified", "generator_version"} {
		require.True(t, cols[c], "column %q missing from wiki_summaries", c)
	}
}

func TestMigration_WikiSummariesCascadeDelete(t *testing.T) {
	d, err := Open(":memory:")
	require.NoError(t, err)
	defer d.Close()

	now := time.Now().UnixMilli()
	_, err = d.Exec(`INSERT INTO wiki_nodes (id, path, level, updated_at) VALUES (?, ?, ?, ?)`,
		"n1", "/x", "project", now)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO wiki_summaries
		(path, summary, generated_at, based_on_last_modified, generator_version)
		VALUES (?, ?, ?, ?, ?)`, "/x", "hello", now, now, "v1")
	require.NoError(t, err)

	_, err = d.Exec(`DELETE FROM wiki_nodes WHERE path = ?`, "/x")
	require.NoError(t, err)

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM wiki_summaries WHERE path = ?`, "/x").Scan(&n))
	require.Equal(t, 0, n, "CASCADE delete should have removed the summary")
}
