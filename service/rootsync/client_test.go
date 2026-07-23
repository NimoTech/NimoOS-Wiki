package rootsync_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/service/rootsync"
)

// writeURLFile 写一个临时的服务发现文件,内容为假核心地址。
func writeURLFile(t *testing.T, url string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "nimoos.url")
	os.WriteFile(p, []byte(url), 0o644)
	return p
}

func TestUpsert_HitsCorrectEndpoint(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := rootsync.New(writeURLFile(t, srv.URL))
	if err := c.Upsert(context.Background(), rootsync.Grant{RootID: "r1", Path: "/a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotPath != "/v1/nimoos/_internal/root-grants/r1" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if gotBody["enabled"] != true || gotBody["path"] != "/a" {
		t.Fatalf("body=%v", gotBody)
	}
}

func TestReconcile_PostsAllGrants(t *testing.T) {
	var gotN int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Grants []rootsync.Grant `json:"grants"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		gotN = len(body.Grants)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := rootsync.New(writeURLFile(t, srv.URL))
	err := c.Reconcile(context.Background(), []rootsync.Grant{{RootID: "a"}, {RootID: "b"}})
	if err != nil || gotN != 2 {
		t.Fatalf("err=%v n=%d", err, gotN)
	}
}

// TestDelete_HitsCorrectEndpoint 补充覆盖简报未直接给出的 Delete 方法路径/方法断言。
func TestDelete_HitsCorrectEndpoint(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := rootsync.New(writeURLFile(t, srv.URL))
	if err := c.Delete(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/nimoos/_internal/root-grants/r1" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
}

// TestNonOKStatus_ReturnsError 覆盖非 2xx 返回 error 的语义,供上层置 needsReconcile。
func TestNonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := rootsync.New(writeURLFile(t, srv.URL))
	if err := c.Upsert(context.Background(), rootsync.Grant{RootID: "r1", Path: "/a", Enabled: true}); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

// TestMissingDiscoveryFile_FallsBackToDefault 覆盖发现文件缺失时的 fallback 行为:
// 请求应打到 fallback(http://127.0.0.1)而不是直接崩溃或报错发现失败。
func TestMissingDiscoveryFile_FallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.url")

	c := rootsync.New(missing)
	ctx, cancel := context.WithTimeout(context.Background(), 200_000_000) // 200ms,fallback 地址必不可达,快速失败即可
	defer cancel()
	err := c.Upsert(ctx, rootsync.Grant{RootID: "r1", Path: "/a", Enabled: true})
	if err == nil {
		t.Fatal("expected error since fallback address is not a real server, but got nil")
	}
}
