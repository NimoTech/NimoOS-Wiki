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

// localStorageStorage mirrors the subset of NimoOS-LocalStorage's `model.Storages`
// (returned by GET /v1/storage) that we care about.
type localStorageStorage struct {
	DiskName string `json:"DiskName"`
	Path     string `json:"Path"`
	Size     int64  `json:"Size"`
	Type     string `json:"Type"`
}

// QueryLocalStorage reads the LocalStorage service URL from runtimePath
// and calls GET /v1/storage. Returns a list of candidate Wiki Roots
// (typically disks / RAID arrays / mergerfs volumes).
//
// Best-effort: returns nil + nil if LocalStorage is unreachable or returns
// an unexpected response. Callers should treat nil as "empty list".
func QueryLocalStorage(runtimePath string) ([]Candidate, error) {
	addrPath := filepath.Join(runtimePath, "local-storage.url")
	raw, err := os.ReadFile(addrPath)
	if err != nil {
		return nil, nil
	}
	addr := strings.TrimSpace(string(raw))
	if addr == "" {
		return nil, nil
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(strings.TrimRight(addr, "/") + "/v1/storage")
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var body struct {
		Data []localStorageStorage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil
	}
	out := make([]Candidate, 0, len(body.Data))
	for _, s := range body.Data {
		if s.Path == "" {
			continue
		}
		out = append(out, Candidate{
			Path:  s.Path,
			Type:  s.Type,
			Size:  s.Size,
			Label: s.DiskName,
		})
	}
	return out, nil
}
