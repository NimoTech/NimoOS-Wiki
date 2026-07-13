package roots

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueryLocalStorage_RealSchemaAndDiscovery(t *testing.T) {
	// LocalStorage returns disks whose usable roots live in
	// children[].mount_point (the disk-level `path` is a device node like
	// /dev/sda); RAID members repeat the same child on every member disk.
	payload := `{"success":200,"message":"ok","data":[
		{"disk_name":"WDC","path":"/dev/sda","type":"sata","children":[
			{"uuid":"u1","mount_point":"/media/RAID_raid10","size":"2000138797056","label":"RAID_raid10","drive_name":"md127","type":"btrfs"}]},
		{"disk_name":"WDC","path":"/dev/sdb","type":"sata","children":[
			{"uuid":"u1","mount_point":"/media/RAID_raid10","size":"2000138797056","label":"RAID_raid10","drive_name":"md127","type":"btrfs"}]},
		{"disk_name":"NoMount","path":"/dev/sdc","type":"sata","children":[
			{"uuid":"u2","mount_point":"","size":"1","label":"","drive_name":"sdc1","type":"ext4"}]}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/storage", r.URL.Path)
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	rt := t.TempDir()
	// LocalStorage never writes local-storage.url; discovery goes through the
	// Gateway's persisted routes.json.
	require.NoError(t, os.WriteFile(filepath.Join(rt, "routes.json"),
		[]byte(`{"/v1/storage":"`+srv.URL+`"}`), 0600))

	cands, err := QueryLocalStorage(rt)
	require.NoError(t, err)
	require.Len(t, cands, 2)
	require.Equal(t, "/DATA", cands[0].Path) // system root always offered first
	require.Equal(t, "/media/RAID_raid10", cands[1].Path)
	require.Equal(t, "RAID_raid10", cands[1].Label)
	require.Equal(t, int64(2000138797056), cands[1].Size)
	require.Equal(t, "btrfs", cands[1].Type)
}

func TestQueryLocalStorage_UnreachableStillOffersSystemRoot(t *testing.T) {
	cands, err := QueryLocalStorage(t.TempDir())
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Equal(t, "/DATA", cands[0].Path)
}

func TestQueryLocalStorage_URLFileTakesPrecedence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"children":[{"mount_point":"/mnt/x","size":"5","type":"ext4","label":"X"}]}]}`))
	}))
	defer srv.Close()

	rt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rt, "local-storage.url"),
		[]byte(srv.URL+"\n"), 0600))

	cands, err := QueryLocalStorage(rt)
	require.NoError(t, err)
	require.Len(t, cands, 2)
	require.Equal(t, "/mnt/x", cands[1].Path)
}
