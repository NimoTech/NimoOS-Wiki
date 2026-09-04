package repo

import (
	"sync/atomic"
	"time"
)

// ArchiveState tells consumers of the file-events feed where Wiki's archive
// horizon sits. Parser compares its cursor against CutoffMs: a cursor older
// than the cutoff means events between the two were archived away and the
// consumer has silently lost them. HasArchived guards young installs — if
// nothing was ever archived there is no gap to detect.
type ArchiveState struct {
	keepDays    int
	hasArchived atomic.Bool
}

func NewArchiveState(keepDays int) *ArchiveState {
	if keepDays <= 0 {
		keepDays = 90
	}
	return &ArchiveState{keepDays: keepDays}
}

// CutoffMs is the same formula archiveSweep uses for ArchiveOlderThan.
func (s *ArchiveState) CutoffMs(now time.Time) int64 {
	return now.Add(-time.Duration(s.keepDays) * 24 * time.Hour).UnixMilli()
}

func (s *ArchiveState) MarkArchived()     { s.hasArchived.Store(true) }
func (s *ArchiveState) HasArchived() bool { return s.hasArchived.Load() }

// InitFromRepo seeds the flag from the database at startup so a restart does
// not forget that history has already been archived.
func (s *ArchiveState) InitFromRepo(ev *FileEventsRepo) error {
	has, err := ev.HasArchived()
	if err != nil {
		return err
	}
	if has {
		s.MarkArchived()
	}
	return nil
}
