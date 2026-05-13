package roots

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Candidate struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  int64  `json:"size,omitempty"`
	Label string `json:"label,omitempty"`
}

// QueryLocalStorage reads the LocalStorage service URL from runtimePath
// and calls its mounts API. Returns a list of candidate Wiki Roots.
// Best-effort: returns empty slice + nil error if LocalStorage unreachable.
func QueryLocalStorage(runtimePath string) ([]Candidate, error) {
	addrPath := filepath.Join(runtimePath, "local-storage.url")
	raw, err := os.ReadFile(addrPath)
	if err != nil {
		return nil, nil // not running — surface as empty list, not error
	}
	addr := strings.TrimSpace(string(raw))
	if addr == "" {
		return nil, nil
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(strings.TrimRight(addr, "/") + "/v2/storage/mounts")
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var body struct {
		Data []Candidate `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil
	}
	return body.Data, nil
}
