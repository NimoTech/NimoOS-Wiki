// Package roots manages Wiki Root lifecycle: registration with write-test,
// FS-type detection for watch_mode auto-downgrade, and listing.
package roots

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Wiki/common"
	"github.com/NimoTech/NimoOS-Wiki/service/eventbus"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
)

type Manager struct {
	roots *repo.WikiRootsRepo
	nodes *repo.WikiNodesRepo
	bus   eventbus.Bus
}

func NewManager(roots *repo.WikiRootsRepo, nodes *repo.WikiNodesRepo, bus eventbus.Bus) *Manager {
	if bus == nil {
		bus = eventbus.Noop{}
	}
	return &Manager{roots: roots, nodes: nodes, bus: bus}
}

type CreateArgs struct {
	Path          string
	Level         string // 'space' | 'project'
	WatchMode     string // 'auto' | 'scan_only'  (default 'auto')
	StorageMode   string // 'inline' | 'mirror'    (default 'inline')
	ScanIntervalS int
}

var (
	ErrPathNotWritable = errors.New("path is not writable (set storage_mode=mirror to use a central mirror)")
	ErrInvalidArgs     = errors.New("invalid arguments")
	ErrPathNotExist    = errors.New("path does not exist")
)

// Create registers a new Wiki Root after validation:
//  1. path must be absolute and exist
//  2. level must be 'space' or 'project'
//  3. for inline storage_mode: writeTest must succeed
//  4. FS type detection — nfs/cifs/fuse → force scan_only
//
// Returns the new Root ID.
func (m *Manager) Create(args CreateArgs) (string, error) {
	if !filepath.IsAbs(args.Path) {
		return "", fmt.Errorf("%w: path must be absolute", ErrInvalidArgs)
	}
	args.Path = filepath.Clean(args.Path)

	info, err := os.Stat(args.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrPathNotExist
		}
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: path is not a directory", ErrInvalidArgs)
	}

	switch args.Level {
	case "space", "project":
	default:
		return "", fmt.Errorf("%w: level must be 'space' or 'project'", ErrInvalidArgs)
	}

	if args.WatchMode == "" {
		args.WatchMode = "auto"
	}
	if args.StorageMode == "" {
		args.StorageMode = "inline"
	}
	if args.ScanIntervalS <= 0 {
		args.ScanIntervalS = 21600
	}

	// FS-type-driven watch_mode downgrade
	if args.WatchMode == "auto" {
		switch DetectFSType(args.Path) {
		case "nfs", "nfs4", "cifs", "smb", "fuse", "fuseblk":
			args.WatchMode = "scan_only"
		}
	}

	if args.StorageMode == "inline" {
		if err := writeTest(args.Path); err != nil {
			return "", ErrPathNotWritable
		}
	}

	id := repo.NewID()
	now := time.Now().UnixMilli()
	if err := m.roots.Insert(repo.WikiRoot{
		ID: id, Path: args.Path, Level: args.Level,
		WatchMode: args.WatchMode, StorageMode: args.StorageMode,
		Enabled: true, ScanIntervalS: args.ScanIntervalS, CreatedAt: now,
	}); err != nil {
		return "", err
	}

	// Seed an initial wiki_node for the Root itself. Mark dirty so WikiWriter
	// produces an initial .wiki.md on first flush; otherwise the file only
	// appears after the first file change in the Root.
	rootIDCopy := id
	_ = m.nodes.Upsert(repo.WikiNode{
		ID: repo.NewID(), RootID: &rootIDCopy, Path: args.Path,
		Level: args.Level, Dirty: true, UpdatedAt: now,
	})

	m.bus.Publish(common.EventRootEnabled, map[string]any{
		"root_id": id,
		"path":    args.Path,
		"level":   args.Level,
	})

	return id, nil
}

// Delete removes a Root. If purgeFiles is true, also removes the .wiki.md files
// under the Root for all its wiki_nodes (best-effort).
func (m *Manager) Delete(id string, purgeFiles bool) error {
	root, err := m.roots.Get(id)
	if err != nil {
		return err
	}

	if purgeFiles {
		nodes, _ := m.nodes.List(id)
		for _, n := range nodes {
			_ = os.Remove(filepath.Join(n.Path, ".wiki.md"))
		}
	}

	nodes, _ := m.nodes.List(id)
	for _, n := range nodes {
		_ = m.nodes.Delete(n.Path)
	}

	if err := m.roots.Delete(id); err != nil {
		return err
	}

	m.bus.Publish(common.EventRootDisabled, map[string]any{
		"root_id": id,
		"path":    root.Path,
		"level":   root.Level,
	})
	return nil
}

// Rescan touches last_scan_at to 0 so the next reconciler tick treats this
// Root as overdue. Caller-side, the main loop wakes on its ticker; this call
// makes the next tick reconcile immediately.
func (m *Manager) Rescan(id string) error {
	return m.roots.UpdateLastScanAt(id, 0)
}

func writeTest(path string) error {
	f := filepath.Join(path, ".nimoos-wiki-write-test")
	if err := os.WriteFile(f, []byte("test"), 0644); err != nil {
		return err
	}
	return os.Remove(f)
}

// DetectFSType reads /proc/mounts and finds the FS type for the longest
// mountpoint prefix matching path. Returns "unknown" on any error.
func DetectFSType(path string) string {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "unknown"
	}
	bestMP := ""
	bestType := "unknown"
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mp, fstype := fields[1], fields[2]
		// Match: exact or descendant
		if (path == mp || strings.HasPrefix(path, mp+"/")) && len(mp) > len(bestMP) {
			bestMP = mp
			bestType = fstype
		}
	}
	return bestType
}
