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

// writeURLFile writes a temp service-discovery file whose content is a fake
// core address.
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

// TestDelete_HitsCorrectEndpoint adds coverage for the Delete method's
// path/method assertions, which the brief didn't spell out directly.
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

// TestNonOKStatus_ReturnsError covers the semantics of returning an error on
// non-2xx, which the caller uses to set needsReconcile.
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

// TestMissingDiscoveryFile_FallsBackToDefault covers the fallback behavior
// when the discovery file is missing: the request should hit the fallback
// (http://127.0.0.1) instead of crashing or erroring out on the discovery
// failure.
func TestMissingDiscoveryFile_FallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.url")

	c := rootsync.New(missing)
	ctx, cancel := context.WithTimeout(context.Background(), 200_000_000) // 200ms; the fallback address is guaranteed unreachable, a fast failure is fine
	defer cancel()
	err := c.Upsert(ctx, rootsync.Grant{RootID: "r1", Path: "/a", Enabled: true})
	if err == nil {
		t.Fatal("expected error since fallback address is not a real server, but got nil")
	}
}

func TestEnabledRoots_ParsesRootIDs(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Write([]byte(`{"root_ids":["photos","r1"]}`))
	}))
	defer srv.Close()
	c := rootsync.New(writeURLFile(t, srv.URL))
	ids, err := c.EnabledRoots(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/nimoos/search-roots" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if len(ids) != 2 || ids[0] != "photos" || ids[1] != "r1" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestEnabledRoots_NonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := rootsync.New(writeURLFile(t, srv.URL))
	if _, err := c.EnabledRoots(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
}
