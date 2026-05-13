package wikimd

import "strings"

const (
	userNotesBegin = "<!-- BEGIN: user-notes -->"
	userNotesEnd   = "<!-- END: user-notes -->"
	systemBegin    = "<!-- BEGIN: system -->"
	systemEnd      = "<!-- END: system -->"
)

// ExtractUserNotes returns the user-owned content between the user-notes
// markers, stripped of the boilerplate "## User Notes" heading. The second
// return value is false when the markers are not found.
//
// This is what the reverse-sync watcher reads back from disk after the user
// edits .wiki.md by hand.
func ExtractUserNotes(src string) (string, bool) {
	i := strings.Index(src, userNotesBegin)
	if i < 0 {
		return "", false
	}
	j := strings.Index(src[i:], userNotesEnd)
	if j < 0 {
		return "", false
	}
	inner := src[i+len(userNotesBegin) : i+j]
	inner = strings.TrimLeft(inner, "\n")
	if strings.HasPrefix(inner, "## User Notes") {
		nl := strings.Index(inner, "\n")
		if nl >= 0 {
			inner = inner[nl+1:]
			inner = strings.TrimLeft(inner, "\n")
		}
	}
	return strings.TrimRight(inner, "\n"), true
}

// ExtractSystemRegion returns the raw system region (markers inclusive) so the
// caller can checksum it and compare against the front-matter value to detect
// tampering. The second return is false when the markers are not found.
func ExtractSystemRegion(src string) (string, bool) {
	i := strings.Index(src, systemBegin)
	if i < 0 {
		return "", false
	}
	j := strings.Index(src[i:], systemEnd)
	if j < 0 {
		return "", false
	}
	end := i + j + len(systemEnd)
	return src[i:end], true
}
