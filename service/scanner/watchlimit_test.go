package scanner

import (
	"os"
	"syscall"
	"testing"
)

func TestIsWatchLimit(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EMFILE, syscall.ENFILE} {
		if !isWatchLimit(&os.PathError{Op: "inotify_add_watch", Err: errno}) {
			t.Fatalf("%v not detected", errno)
		}
	}
	if isWatchLimit(os.ErrNotExist) || isWatchLimit(nil) {
		t.Fatal("false positive")
	}
}
