package repo

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// NewID generates a random 16-byte hex ID for primary keys.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// EscapeLikeArg escapes %, _, and \ in the argument so it can be safely
// concatenated into a LIKE pattern with `ESCAPE '\'`.
// CRITICAL for path-prefix queries: a literal underscore in a directory name
// would otherwise act as a single-char wildcard.
func EscapeLikeArg(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullable(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}
