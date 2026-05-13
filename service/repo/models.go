package repo

type WikiRoot struct {
	ID            string
	Path          string
	Level         string
	WatchMode     string
	StorageMode   string
	Enabled       bool
	ScanIntervalS int
	CreatedAt     int64
	LastScanAt    int64
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

type FileEvent struct {
	ID          string
	RootID      string
	Path        string
	Op          string
	RenameTo    string
	IsDir       bool
	DetectedAt  int64
	ProcessedAt int64
	Archived    bool
}
