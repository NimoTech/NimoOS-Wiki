package roots

// 本文件测试 manager 三处生命周期方法(Create/SetEnabled/Delete)在成功写库
// 后是否正确追加推送核心授权(授权源解耦项目 Task 5)。用 fakePusher 断言
// 调用次数与参数,不依赖真实 rootsync HTTP client。

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Wiki/pkg/db"
	"github.com/NimoTech/NimoOS-Wiki/service/repo"
	"github.com/NimoTech/NimoOS-Wiki/service/rootsync"
	"github.com/stretchr/testify/require"
)

// fakePusher 是 pusher 接口的测试替身,记录每次调用的参数供断言。
type fakePusher struct {
	upserts []rootsync.Grant
	deletes []string
}

func (f *fakePusher) Upsert(_ context.Context, g rootsync.Grant) error {
	f.upserts = append(f.upserts, g)
	return nil
}

func (f *fakePusher) Delete(_ context.Context, id string) error {
	f.deletes = append(f.deletes, id)
	return nil
}

// newManagerWithFakePusher 构造一个带临时内存 wiki.db + fakePusher 的
// manager,建库方式照抄 manager_test.go 的 setupManagerWithIgnore。
func newManagerWithFakePusher(t *testing.T) (*Manager, *fakePusher) {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	roots := repo.NewWikiRoots(d)
	nodes := repo.NewWikiNodes(d)
	files := repo.NewFileIndex(d)
	events := repo.NewFileEvents(d)
	m := NewManager(roots, nodes, files, events, &fakeBus{}, nil)
	fp := &fakePusher{}
	m.SetPusher(fp)
	return m, fp
}

func TestCreate_PushesUpsert(t *testing.T) {
	m, fp := newManagerWithFakePusher(t)
	dir := t.TempDir()

	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	require.Len(t, fp.upserts, 1)
	require.Equal(t, id, fp.upserts[0].RootID)
	require.Equal(t, dir, fp.upserts[0].Path)
	require.True(t, fp.upserts[0].Enabled)
}

func TestSetEnabled_PushesUpsert(t *testing.T) {
	m, fp := newManagerWithFakePusher(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)
	fp.upserts = nil // 只看 SetEnabled 这次

	require.NoError(t, m.SetEnabled(id, false))

	require.Len(t, fp.upserts, 1)
	require.Equal(t, id, fp.upserts[0].RootID)
	require.False(t, fp.upserts[0].Enabled)
}

func TestDelete_PushesDelete(t *testing.T) {
	m, fp := newManagerWithFakePusher(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)
	fp.upserts = nil

	require.NoError(t, m.Delete(id, false))

	require.Len(t, fp.deletes, 1)
	require.Equal(t, id, fp.deletes[0])
}

// pusherErr 是一个总是失败的 fake pusher,用于验证失败路径:仅置
// needs_reconcile,不返回 error、不阻塞调用方。
type pusherErr struct{ err error }

func (p pusherErr) Upsert(context.Context, rootsync.Grant) error { return p.err }
func (p pusherErr) Delete(context.Context, string) error         { return p.err }

// TestCreate_PushFailureMarksNeedsAuthzPushButDoesNotFail 覆盖方案 B 的修复:
// upsert 推送失败应置 needs_authz_push(独立重试字段),而不是误用 FS 重扫语义的
// needs_reconcile(旧 bug,已修复)。
func TestCreate_PushFailureMarksNeedsAuthzPushButDoesNotFail(t *testing.T) {
	m, rootsRepo := setupManager(t)
	m.SetPusher(pusherErr{err: context.DeadlineExceeded})
	dir := t.TempDir()

	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err) // 推送失败不影响 Create 本身成功

	r, err := rootsRepo.Get(id)
	require.NoError(t, err)
	require.True(t, r.NeedsAuthzPush)
	require.False(t, r.NeedsReconcile, "upsert 推送失败不应再误置 FS 重扫标记")
}

// TestSetEnabled_PushFailureMarksNeedsAuthzPush 覆盖 SetEnabled 走的也是
// pushUpsert 失败路径,同样只置 needs_authz_push。
func TestSetEnabled_PushFailureMarksNeedsAuthzPush(t *testing.T) {
	m, rootsRepo := setupManager(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	m.SetPusher(pusherErr{err: context.DeadlineExceeded})
	require.NoError(t, m.SetEnabled(id, false))

	r, err := rootsRepo.Get(id)
	require.NoError(t, err)
	require.True(t, r.NeedsAuthzPush)
}

// TestDelete_PushFailureMarksAuthzDirtyButDoesNotFail 覆盖 delete 推送失败的
// 独立路径:该 root 行此时已被删除,DB 里无处落盘,只能置 manager 内存脏标
// (authzDirty),交给重试循环靠全量 Reconcile 兜底。
func TestDelete_PushFailureMarksAuthzDirtyButDoesNotFail(t *testing.T) {
	m, _ := setupManager(t)
	dir := t.TempDir()
	id, _, err := m.Create(CreateArgs{Path: dir, Level: "space"})
	require.NoError(t, err)

	require.False(t, m.AuthzDirty(), "初始不应有脏标")
	m.SetPusher(pusherErr{err: context.DeadlineExceeded})

	require.NoError(t, m.Delete(id, false)) // 推送失败不影响 Delete 本身成功
	require.True(t, m.AuthzDirty(), "delete 推送失败应置内存脏标")

	m.ClearAuthzDirty()
	require.False(t, m.AuthzDirty())
}
