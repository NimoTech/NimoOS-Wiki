package nodelock

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLocks_SamePathSerializes(t *testing.T) {
	l := New()
	var inCrit atomic.Int32
	var maxInCrit atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := l.Lock("/x")
			defer unlock()
			n := inCrit.Add(1)
			defer inCrit.Add(-1)
			// CAS loop to record peak without a mutex.
			for {
				m := maxInCrit.Load()
				if n <= m || maxInCrit.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, maxInCrit.Load(), "same path must serialize")
}

func TestLocks_DifferentPathsParallel(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)

	l := New()
	var inCrit atomic.Int32
	var maxInCrit atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		path := "/a" + string(rune('0'+i))
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			unlock := l.Lock(p)
			defer unlock()
			n := inCrit.Add(1)
			defer inCrit.Add(-1)
			// CAS loop to record peak without a mutex.
			for {
				m := maxInCrit.Load()
				if n <= m || maxInCrit.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
		}(path)
	}
	wg.Wait()
	require.Greater(t, int(maxInCrit.Load()), 1, "different paths must not serialize")
}
