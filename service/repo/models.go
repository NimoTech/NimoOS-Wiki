package repo

type WikiRoot struct {
	ID             string
	Path           string
	Level          string
	WatchMode      string
	StorageMode    string
	Enabled        bool
	ScanIntervalS  int
	CreatedAt      int64
	LastScanAt     int64
	NeedsReconcile bool
}

type WikiNode struct {
	ID                 string
	RootID             *string
	Path               string
	Level              string
	ChildCount         int
	LastModified       int64
	ChecksumSystem     string
	UserNotes          string
	UserNotesETag      string
	UserNotesUpdatedAt int64
	Dirty              bool
	LastFlushedAt      int64
	LastFlushedMtime   int64
	UpdatedAt          int64
	AILabel            string // set by future AI summary worker; empty until then
}

type FileIndex struct {
	ID       string
	RootID   string
	Path     string
	Parent   string
	IsDir    bool
	IsOpaque bool
	Mtime    int64
	Size     int64
	Inode    int64
	Status   string
	Ext      string
}

// FileEvent serializes to snake_case JSON. NimoOS-Parser's WikiConsumer
// (Python) reads ev["root_id"], ev["path"], ev["detected_at"] etc., so the
// JSON tags are part of the cross-repo wire contract — don't drop them.
type FileEvent struct {
	ID          string `json:"id"`
	RootID      string `json:"root_id"`
	Path        string `json:"path"`
	Op          string `json:"op"`
	RenameTo    string `json:"rename_to"`
	IsDir       bool   `json:"is_dir"`
	DetectedAt  int64  `json:"detected_at"`
	ProcessedAt int64  `json:"processed_at"`
	Archived    bool   `json:"archived"`
	// Seq is the SQLite rowid, exposed only by ListSinceSeq for keyset
	// pagination (same-millisecond bursts overflow a detected_at-only
	// cursor). 0 everywhere else.
	Seq int64 `json:"seq"`
}
