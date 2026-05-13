// Package childmap aggregates a directory's children into a compact, human-
// readable summary. Large homogeneous file collections are rolled up by
// extension; directories pass through one-per-row (with opaque containers
// already marked by the scanner).
package childmap

import "sort"

// Entry is one child of a directory, as observed by the scanner.
type Entry struct {
	Name           string
	IsDir          bool
	IsOpaque       bool // for container dirs (node_modules, .git, ...)
	ChildFileCount int  // total descendant file count for opaque dirs
	Ext            string
	Mtime          int64
}

// Group is one row in the rendered child-map. When IsAggregate is true, the
// row represents an extension bucket of Count files; otherwise it represents
// a single named child (file or directory).
type Group struct {
	Name           string
	IsDir          bool
	IsOpaque       bool
	IsAggregate    bool
	Ext            string
	Count          int
	ChildFileCount int
	MtimeMin       int64
	MtimeMax       int64
	SampleNames    []string
}

// Aggregate folds the raw entry list into a slice of Groups.
//
// Rules:
//   - directories always pass through unchanged (one Group each);
//   - files are grouped by extension internally;
//   - if NO extension bucket exceeds `threshold`, every file is emitted as
//     its own Group (no aggregation kicks in);
//   - otherwise, all file buckets are aggregated; the top 8 are kept by Count
//     and the rest collapsed into a single "其它" bucket.
func Aggregate(entries []Entry, threshold int) []Group {
	var out []Group
	byExt := map[string][]Entry{}
	for _, e := range entries {
		if e.IsDir {
			out = append(out, Group{
				Name: e.Name, IsDir: true, IsOpaque: e.IsOpaque,
				ChildFileCount: e.ChildFileCount, MtimeMax: e.Mtime,
			})
			continue
		}
		byExt[e.Ext] = append(byExt[e.Ext], e)
	}

	shouldAggregate := false
	for _, grp := range byExt {
		if len(grp) > threshold {
			shouldAggregate = true
			break
		}
	}

	if !shouldAggregate {
		for _, grp := range byExt {
			for _, e := range grp {
				out = append(out, Group{Name: e.Name, IsDir: false, Count: 1, MtimeMax: e.Mtime})
			}
		}
		return out
	}

	var aggs []Group
	for ext, grp := range byExt {
		g := Group{IsAggregate: true, Ext: ext, Count: len(grp), Name: ext}
		for _, e := range grp {
			if g.MtimeMin == 0 || e.Mtime < g.MtimeMin {
				g.MtimeMin = e.Mtime
			}
			if e.Mtime > g.MtimeMax {
				g.MtimeMax = e.Mtime
			}
		}
		for i := 0; i < len(grp) && i < 3; i++ {
			g.SampleNames = append(g.SampleNames, grp[i].Name)
		}
		aggs = append(aggs, g)
	}

	sort.Slice(aggs, func(i, j int) bool { return aggs[i].Count > aggs[j].Count })

	if len(aggs) > 8 {
		extras := aggs[8:]
		aggs = aggs[:8]
		other := Group{IsAggregate: true, Ext: "其它", Name: "其它"}
		for _, g := range extras {
			other.Count += g.Count
		}
		aggs = append(aggs, other)
	}

	return append(out, aggs...)
}
