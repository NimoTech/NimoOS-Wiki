package repo

import (
	"database/sql"
	"errors"
)

// WikiSummary is one row of wiki_summaries.
//
// Path mirrors wiki_nodes.path (FK + ON DELETE CASCADE in schema).
// BasedOnLastModified is the wiki_nodes.last_modified snapshot the worker
// saw when generating this row — race-free freshness key, not GeneratedAt.
type WikiSummary struct {
	Path                string
	Summary             string
	GeneratedAt         int64
	BasedOnLastModified int64
	GeneratorVersion    string
}

// NeedsSummaryRow is what ListNeedsSummary returns: enough for the worker
// to decide whether to summarize and to take the freshness snapshot.
type NeedsSummaryRow struct {
	Path           string `json:"path"`
	Level          string `json:"level"`
	LastModifiedMs int64  `json:"last_modified_ms"`
	CurrentAILabel string `json:"current_ai_label"`
	ChildCount     int    `json:"child_count"`
}

type WikiSummariesRepo struct{ db *sql.DB }

func NewWikiSummaries(d *sql.DB) *WikiSummariesRepo { return &WikiSummariesRepo{d} }

// Get returns nil, nil when no row exists for path.
func (r *WikiSummariesRepo) Get(path string) (*WikiSummary, error) {
	row := r.db.QueryRow(`SELECT path, summary, generated_at, based_on_last_modified, generator_version
		FROM wiki_summaries WHERE path = ?`, path)
	s := &WikiSummary{}
	err := row.Scan(&s.Path, &s.Summary, &s.GeneratedAt, &s.BasedOnLastModified, &s.GeneratorVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

func (r *WikiSummariesRepo) Upsert(s WikiSummary) error {
	_, err := r.db.Exec(`INSERT OR REPLACE INTO wiki_summaries
		(path, summary, generated_at, based_on_last_modified, generator_version)
		VALUES (?, ?, ?, ?, ?)`,
		s.Path, s.Summary, s.GeneratedAt, s.BasedOnLastModified, s.GeneratorVersion)
	return err
}

// ListNeedsSummary returns wiki_nodes that need (re-)summarizing, ordered
// by last_modified DESC. A node qualifies if any of:
//
//	a) ai_label is empty (never summarized);
//	b) no wiki_summaries row exists;
//	c) wiki_summaries.based_on_last_modified < wiki_nodes.last_modified
//	   (content changed since the summary was made).
func (r *WikiSummariesRepo) ListNeedsSummary(limit int) ([]NeedsSummaryRow, error) {
	rows, err := r.db.Query(`
		SELECT n.path, n.level, COALESCE(n.last_modified, 0), n.ai_label, n.child_count
		  FROM wiki_nodes n
		  LEFT JOIN wiki_summaries s ON s.path = n.path
		 WHERE n.ai_label = ''
		    OR s.based_on_last_modified IS NULL
		    OR s.based_on_last_modified < COALESCE(n.last_modified, 0)
		 ORDER BY COALESCE(n.last_modified, 0) DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NeedsSummaryRow
	for rows.Next() {
		var nr NeedsSummaryRow
		if err := rows.Scan(&nr.Path, &nr.Level, &nr.LastModifiedMs, &nr.CurrentAILabel, &nr.ChildCount); err != nil {
			return nil, err
		}
		out = append(out, nr)
	}
	return out, rows.Err()
}
