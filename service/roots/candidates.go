package roots

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Candidate struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  int64  `json:"size,omitempty"`
	Label string `json:"label,omitempty"`
}

// localStorageChild mirrors the subset of NimoOS-LocalStorage's
// GET /v1/storage response (data[].children[]) that we care about. The
// usable filesystem roots are the children's mount points — the disk-level
// `path` is a device node (/dev/sda), never a directory.
type localStorageChild struct {
	MountPoint string `json:"mount_point"`
	Size       string `json:"size"`
	Type       string `json:"type"`
	Label      string `json:"label"`
	DriveName  string `json:"drive_name"`
}

type localStorageDisk struct {
	Children []localStorageChild `json:"children"`
}

// QueryLocalStorage returns candidate Wiki Roots: the user-visible system
// root /DATA, plus every mounted volume LocalStorage reports (RAID arrays,
// mergerfs volumes, extra disks). Volumes are deduped by mount point —
// every RAID member disk repeats the same child.
//
// Best-effort: if LocalStorage is unreachable the list still contains /DATA.
func QueryLocalStorage(runtimePath string) ([]Candidate, error) {
	out := []Candidate{{Path: "/DATA", Type: "system", Label: "System"}}
	seen := map[string]bool{"/DATA": true}

	addr := localStorageAddr(runtimePath)
	if addr == "" {
		return out, nil
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(strings.TrimRight(addr, "/") + "/v1/storage")
	if err != nil {
		return out, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, nil
	}
	var body struct {
		Data []localStorageDisk `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return out, nil
	}
	for _, d := range body.Data {
		for _, ch := range d.Children {
			mp := strings.TrimSpace(ch.MountPoint)
			if mp == "" || seen[mp] {
				continue
			}
			seen[mp] = true
			size, _ := strconv.ParseInt(ch.Size, 10, 64)
			label := ch.Label
			if label == "" {
				label = ch.DriveName
			}
			out = append(out, Candidate{Path: mp, Type: ch.Type, Size: size, Label: label})
		}
	}
	return out, nil
}

// localStorageAddr discovers the LocalStorage service address. LocalStorage
// does not write a *.url file into the runtime dir (it only registers its
// routes with the Gateway), so the reliable source is the Gateway's
// persisted routes.json; a local-storage.url file is honored first in case
// the service ever starts writing one.
func localStorageAddr(runtimePath string) string {
	if raw, err := os.ReadFile(filepath.Join(runtimePath, "local-storage.url")); err == nil {
		if a := strings.TrimSpace(string(raw)); a != "" {
			return a
		}
	}
	raw, err := os.ReadFile(filepath.Join(runtimePath, "routes.json"))
	if err != nil {
		return ""
	}
	var routes map[string]string
	if err := json.Unmarshal(raw, &routes); err != nil {
		return ""
	}
	return strings.TrimSpace(routes["/v1/storage"])
}
