// Package wikimd renders and parses the .wiki.md sidecar files.
//
// The on-disk format has three regions:
//
//  1. YAML front matter (metadata + checksum of the system region).
//  2. <!-- BEGIN: system --> ... <!-- END: system --> — auto-maintained.
//  3. <!-- BEGIN: user-notes --> ... <!-- END: user-notes --> — user-owned.
//
// The checksum covers ONLY the system region byte stream, so detecting drift
// (user edited the system region by hand) is a fast equality check.
package wikimd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Doc is the structured input passed to Render. It carries everything needed
// to produce a stable, deterministic .wiki.md byte stream.
type Doc struct {
	Version      int
	RootID       string
	Path         string
	Level        string
	GeneratedAt  time.Time
	Generator    string
	OriginalPath string

	Summary       string
	ChildMap      []ChildEntry
	KeySources    []string
	RecentChanges []ChangeEntry
	PendingCount  int
	PendingFailed []string

	UserNotes string
}

// ChildEntry is one row of the Child Map section.
type ChildEntry struct {
	Name        string
	Description string
	IsOpaque    bool
	IsSubwiki   bool
}

// ChangeEntry is one row of the Recent Changes section.
type ChangeEntry struct {
	When time.Time
	Op   string
	Path string
}

// Render produces the full .wiki.md content and the sha256 hex digest of the
// system region (which is also embedded in the front matter as `checksum`).
// Output is fully deterministic for a given Doc.
func Render(d Doc) (string, string) {
	if d.Generator == "" {
		d.Generator = "nimoos-wiki/0.1.0"
	}

	var sys bytes.Buffer
	sys.WriteString("<!-- BEGIN: system -->\n")
	sys.WriteString("<!-- This section is maintained automatically by the system. Do not edit it by hand. To write notes, use the User Notes region at the end of the file. -->\n\n")

	sys.WriteString("## Summary\n")
	if d.Summary == "" {
		sys.WriteString("_Not generated yet (pending AI summary worker)_\n\n")
	} else {
		sys.WriteString(d.Summary + "\n\n")
	}

	sys.WriteString("## Child Map\n")
	if len(d.ChildMap) == 0 {
		sys.WriteString("_Empty directory_\n\n")
	} else {
		for _, c := range d.ChildMap {
			sys.WriteString("- `" + c.Name + "/` — " + c.Description + "\n")
		}
		sys.WriteString("\n")
	}

	sys.WriteString("## Key Sources\n")
	if len(d.KeySources) == 0 {
		sys.WriteString("_Not generated yet_\n\n")
	} else {
		for _, s := range d.KeySources {
			sys.WriteString("- " + s + "\n")
		}
		sys.WriteString("\n")
	}

	sys.WriteString("## Recent Changes\n")
	if len(d.RecentChanges) == 0 {
		sys.WriteString("_No recent changes_\n\n")
	} else {
		for _, ch := range d.RecentChanges {
			sys.WriteString(fmt.Sprintf("- %s  %-9s %s\n",
				ch.When.Format("2006-01-02 15:04"), ch.Op, ch.Path))
		}
		sys.WriteString("\n")
	}

	sys.WriteString("## Pending Index\n")
	if d.PendingCount == 0 {
		sys.WriteString("_All indexed_\n\n")
	} else {
		sys.WriteString(fmt.Sprintf("- %d file(s) pending parse (queued)\n", d.PendingCount))
		for _, p := range d.PendingFailed {
			sys.WriteString("- Parse failed: `" + p + "`\n")
		}
		sys.WriteString("\n")
	}

	sys.WriteString("<!-- END: system -->\n")

	sysBytes := sys.Bytes()
	hsum := sha256.Sum256(sysBytes)
	hash := hex.EncodeToString(hsum[:])

	var fm bytes.Buffer
	fm.WriteString("---\n")
	ver := d.Version
	if ver < 1 {
		ver = 1
	}
	fm.WriteString(fmt.Sprintf("wiki_version: %d\n", ver))
	if d.RootID != "" {
		fm.WriteString("root_id: " + d.RootID + "\n")
	}
	fm.WriteString("path: " + d.Path + "\n")
	fm.WriteString("level: " + d.Level + "\n")
	fm.WriteString("generated_at: " + d.GeneratedAt.Format("2006-01-02T15:04:05Z07:00") + "\n")
	fm.WriteString("generator: " + d.Generator + "\n")
	if d.OriginalPath != "" {
		fm.WriteString("original_path: " + d.OriginalPath + "\n")
	}
	fm.WriteString("checksum: " + hash + "\n")
	fm.WriteString("---\n\n")

	var out bytes.Buffer
	out.Write(fm.Bytes())
	out.Write(sysBytes)
	out.WriteString("\n<!-- BEGIN: user-notes -->\n")
	out.WriteString("## User Notes\n\n")
	if strings.TrimSpace(d.UserNotes) == "" {
		out.WriteString("(Write any notes you want the AI to remember here. The system will not modify this section.)\n")
	} else {
		out.WriteString(d.UserNotes)
		if !strings.HasSuffix(d.UserNotes, "\n") {
			out.WriteString("\n")
		}
	}
	out.WriteString("\n<!-- END: user-notes -->\n")

	return out.String(), hash
}
