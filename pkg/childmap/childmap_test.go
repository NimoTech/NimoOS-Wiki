package childmap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAggregate_SmallDir(t *testing.T) {
	entries := []Entry{
		{Name: "a.go", IsDir: false, Ext: "go"},
		{Name: "b.go", IsDir: false, Ext: "go"},
	}
	groups := Aggregate(entries, 50)
	require.Len(t, groups, 2)
}

func TestAggregate_LargeHomogeneous(t *testing.T) {
	var entries []Entry
	for i := 0; i < 100; i++ {
		entries = append(entries, Entry{Name: "x.jpg", IsDir: false, Ext: "jpg"})
	}
	groups := Aggregate(entries, 50)
	require.Len(t, groups, 1)
	require.Equal(t, 100, groups[0].Count)
	require.Equal(t, "jpg", groups[0].Ext)
}

func TestAggregate_TopN(t *testing.T) {
	var entries []Entry
	for _, ext := range []string{"jpg", "png", "raw", "mp4", "mov", "tiff", "gif", "bmp", "webp", "heic"} {
		for i := 0; i < 60; i++ {
			entries = append(entries, Entry{Name: "f." + ext, IsDir: false, Ext: ext})
		}
	}
	groups := Aggregate(entries, 50)
	require.Len(t, groups, 9)
	require.Equal(t, OtherBucket, groups[8].Ext)
}

func TestAggregate_PreservesDirsAndOpaque(t *testing.T) {
	entries := []Entry{
		{Name: "src", IsDir: true},
		{Name: "node_modules", IsDir: true, IsOpaque: true, ChildFileCount: 13428},
		{Name: "x.jpg", IsDir: false, Ext: "jpg"},
	}
	groups := Aggregate(entries, 50)
	require.Equal(t, 3, len(groups))
}
