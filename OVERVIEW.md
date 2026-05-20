# NimoOS-Wiki

NimoOS 的「可见长期记忆」服务 — 在用户的存储空间里维护 `.wiki.md` 导航地图，让用户和 Agent 都能直接读到「NAS 里有什么、在哪里、属于什么主题」。当前版本 `v0.1.0-alpha`。

绑定 localhost、由 Gateway 转发，API 前缀 `/v1/wiki`。详细设计见 [`nimo_os_docs/docs/superpowers/specs/2026-05-13-wiki-design.md`](../nimo_os_docs/docs/superpowers/specs/2026-05-13-wiki-design.md)。

---

## 在整体架构中的位置

```
                外部请求 (Gateway 转发, /v1/wiki/*)
                            │
                            ▼
           ┌────────────────────────────────────┐
           │   nimoos-wiki.service (Go,Echo)   │
           │                                    │
           │   Watcher  ─┐                      │
           │   Recon.   ─┼─▶ file_events ─▶ EventProcessor │
           │             │                      ▼          │
           │             │   wiki_nodes ◀── markNearest    │
           │             │      │                          │
           │             │      ▼  (dirty=1)               │
           │             │   WikiWriter ──▶ .wiki.md       │
           │             │                  (Chtimes+rename)│
           │             └◀───── 反向 sync ◀── 外部编辑     │
           └────────────────────────────────────┘
                       │              ▲
                       ▼              │
          /var/lib/nimoos/wiki/wiki.db (SQLite)
                       │
                       ▼
                  MessageBus
              (Wiki:NodeUpdated /
               Wiki:RecentChanged / ...)
```

Wiki 只做**导航地图**;**内容搜索**(向量库 / 全文索引)是接下来另开的项目,通过 `_internal` 接口对接,本服务只留 hook、不实施。

---

## API 路由 (`/v1/wiki`)

所有公开路由走 JWT 中间件,**localhost 来源豁免**(由 `RealIP` 判断,Gateway 转发的外部请求 RealIP 已被改写为真实客户端 IP,不命中本机分支)。这是 2026-05-14 Agent 集成时为了让 Python Agent 直连而引入的策略,与 Photos 服务同模式。`_internal` 组例外:`LocalhostOnly` 中间件强制 RealIP=loopback,即便经 Gateway 也访问不到。

### A 组 — Root 管理

| Method | Path | 用途 |
|---|---|---|
| GET | `/v1/wiki/roots` | 列出所有 Root |
| POST | `/v1/wiki/roots` | 启用 Root: `{path, level, watch_mode?, storage_mode?}` |
| DELETE | `/v1/wiki/roots/:id` | 停用 Root,可选 `?purge_files=true` 删除落盘 `.wiki.md` |
| POST | `/v1/wiki/roots/:id/rescan` | 强制对账扫描(把 last_scan_at 清零) |
| GET | `/v1/wiki/candidates` | 枚举 LocalStorage 上报的挂载点候选 |

启用 Root 时做**写测试**(在路径下创建并删除 `.nimoos-wiki-write-test`);失败返回 409。

> ⚠ `storage_mode=mirror` **当前未实现**。`POST /roots` 接受这个字段、`wiki_roots.storage_mode` 也存了下来,但 `WikiWriter` 始终把 `.wiki.md` 写到 `nodePath/.wiki.md`,不会路由到 `/var/lib/nimoos/wiki/mirror/<root_id>/...`。在不可写路径上目前没有替代方案,只能放弃注册或在用户层先把目录改成可写。该缺口列在下面的「实施现状」一节。

### B 组 — Wiki 内容

| Method | Path | 用途 |
|---|---|---|
| GET | `/v1/wiki/tree?root_id=&depth=` | 返回 Wiki 节点骨架(不含 summary) |
| GET | `/v1/wiki/node?path=...` | 读单节点(结构化 JSON,含 etag) |
| GET | `/v1/wiki/raw?path=...` | 读原始 `.wiki.md` 文本 |
| PUT | `/v1/wiki/user-notes?path=...` | 写 User Notes,需 `If-Match: <etag>` 乐观锁 |
| GET | `/v1/wiki/recent-changes?root_id=&since_ms=&limit=` | 跨 Root 拉最近变化(`since_ms` 是游标,`limit` 默认 50、上限 200) |

### C 组 — 内部接口(localhost only, 不注册到 Gateway)

| Method | Path | 用途 |
|---|---|---|
| GET | `/v1/wiki/_internal/file-events?root_id=&since=&limit=` | 给 Parser worker 拉文件变化(**本次实现**) |
| GET | `/v1/wiki/_internal/needs-summary?limit=` | 给 AI 摘要 worker 拉待摘要节点(503 stub) |
| POST | `/v1/wiki/_internal/summary` | AI 摘要 worker 上报摘要(503 stub) |
| POST | `/v1/wiki/_internal/index-status` | Parser worker 上报解析状态(503 stub) |

### MessageBus 事件

| 事件 | 实施状态 | 备注 |
|---|---|---|
| `Wiki:NodeUpdated` | ✅ 已发 | 节点落盘后,`{path, root_id}` |
| `Wiki:RecentChanged` | ✅ 已发(按 batch 聚合) | `{root_id}`,一次 `ProcessBatch` 内每个 root 最多 publish 一次,避免大批量操作刷屏 |
| `Wiki:PendingChanged` | ❌ 未发 | spec 里规划的事件,代码里没人发送 |
| `Wiki:RootEnabled` / `Wiki:RootDisabled` | ✅ 已发 | Create/Delete 成功后发送,payload `{root_id, path, level}` |
| `Wiki:WriteFailed` | ❌ 未发 | spec 设想连续 5 次失败告警,代码只是记日志 |

---

## 关键模块

| 包 | 职责 |
|---|---|
| `main.go` | 启动、随机端口、写 `wiki.url`、向 Gateway 注册路由、systemd notify、信号处理 + graceful flush |
| `pkg/config/` | Viper INI 配置 (`/etc/nimoos/wiki.conf`),自动写出 sample 并应用默认值 |
| `pkg/db/` | SQLite 打开 + PRAGMAs (`WAL` / `case_sensitive_like=ON` / `busy_timeout=5000`) + migrations |
| `pkg/pathutil/` | 大小写敏感的路径工具 (`Clean`、`IsUnder`、`Parent`) |
| `pkg/ignore/` | 容器目录(node_modules / .git 等)+ 系统噪声忽略规则 |
| `pkg/wikimd/` | `.wiki.md` 渲染 + 解析(user-notes 区抽取) |
| `pkg/childmap/` | Child Map 渲染:聚合 + Top-N 收敛 |
| `service/repo/` | SQLite CRUD:wiki_roots / wiki_nodes / file_index / file_events / parse_status。**所有路径前缀 LIKE 都用 EscapeLikeArg + ESCAPE '\\'** |
| `service/scanner/` | Watcher (fsnotify, 路径 1/2/3 分流) + Reconciler (load-to-map 对账) |
| `service/processor/` | EventProcessor:debounce + MOVED_FROM/MOVED_TO 配对 + 目录 rename 级联 UPDATE + user-notes 反向同步 |
| `service/writer/` | WikiWriter:Chtimes 锁 mtime + DB commit + atomic rename 的严格顺序 |
| `service/roots/` | Root 生命周期 + 写测试 + FS 类型检测(nfs/cifs/fuse 自动降级 scan_only) |
| `service/eventbus/` | NimoOS-Common MessageBus 包装,带 Noop fallback |
| `route/` | JWT 中间件 + LocalhostOnly 中间件 |
| `route/v1/` | A/B/C 三组 handlers |
| `tests/integration/` | E2E 集成测试 (build tag `integration`),覆盖 spec §13 全部验收项 |

---

## 数据模型

SQLite 数据库 `/var/lib/nimoos/wiki/wiki.db` (CGO_ENABLED=1)。所有时间字段统一存 **Unix 毫秒 INTEGER**。

| 表 | 用途 |
|---|---|
| `wiki_roots` | 已注册的 Root:path / level / watch_mode / storage_mode / enabled / scan_interval_s。每行由 `POST /v1/wiki/roots` 写入 |
| `wiki_nodes` | **每行对应一个被显式注册的 Root**(并且**仅由 Root 注册时 seed**,见下方说明)。`user_notes` 内容直接存这里,DB 是权威源,`.wiki.md` 是渲染产物;`last_flushed_mtime` 用于 Watcher 区分「自己写的」vs「外部编辑」 |
| `file_index` | 监控范围内的所有文件 + 容器目录占位条目;Reconciler 对账用。`is_opaque=1` 标记容器目录(node_modules 等),内部不递归 |
| `file_events` | 统一的文件事件表(合并旧设计中的 pending_events + recent_changes)。三种消费模式:`processed_at IS NULL` 队列、`since=` 增量查询、`ORDER BY detected_at DESC` 最近变化渲染 |
| `parse_status` | 给将来的 Parser worker 上报解析/索引状态;本服务只在 create 事件时入库 `pending`,不读 |

**关于 wiki_node 的产生(很容易踩坑的点):**

`wiki_nodes` 表里**只有**通过 `POST /v1/wiki/roots` 显式注册过的目录会有一行。代码里整个仓库唯一插入 `wiki_nodes` 的位置是 `service/roots/manager.go:Create`(seed root 节点)。Processor 的 `markNearestWikiNodeDirty` 是**向上**找最近的现存 wiki_node 来标脏,**从不**自动给子目录建节点。

也就是说:Wiki 是「用户登记几个 Root,就长几个 wiki_node」—— 不是文件树的镜像。要给 `/DATA` 当 space、`/DATA/Projects/nimoos` 当 project,这两个 Root 都得分别 POST 一次。schema 里 `wiki_nodes.level` 留了 `system`/`space`/`project` 三档,但**`system` 单例节点(root_id=NULL)在代码里没有任何 seed 路径**,实际跑起来只用到 `space` 和 `project` 两档。Agent 集成里的「地图」也只渲染两层。

**关键正确性不变量:**

1. `PRAGMA case_sensitive_like = ON` — Linux 文件系统大小写敏感,LIKE 默认却不敏感;不开这个,目录 rename 时 `WHERE path LIKE '/DATA/ProjectA/%'` 会误伤 `/DATA/projecta` 下的文件。
2. 所有路径前缀 LIKE 用 `EscapeLikeArg` 转义 `_` 和 `%` + `ESCAPE '\\'` — 否则路径里包含 `_` 时会被 SQLite 当通配符。
3. 目录 rename 的级联 UPDATE 在**单事务**里同步更新 `file_index`、`wiki_nodes`、未处理的 `file_events`。
4. WikiWriter 落盘顺序: write tmp → `os.Chtimes(tmp, T, T)` → **DB commit `last_flushed_mtime = T`** → atomic rename。DB commit 必须在 rename 之前;否则 fsnotify 事件先到、Watcher 读到旧 mtime、误判为外部编辑、触发无意义反向同步。

---

## 数据存储与运行时

```
/etc/nimoos/wiki.conf              配置(INI,样例随包安装)
/var/lib/nimoos/wiki/
  └── wiki.db                      SQLite (WAL 模式)
/var/run/nimoos/
  ├── wiki.url                     服务发现地址
  └── wiki.pid                     systemd PID 文件
/var/log/nimoos/nimoos-wiki.log    zap 日志
```

`.wiki.md` 始终落在对应 wiki_node 的 `<root>/.wiki.md`(inline 模式)。spec 里规划的 `mirror/<root_id>/...` 目录布局**当前未实现**,见「实施现状」。

---

## 配置样例 (`build/sysroot/etc/nimoos/wiki.conf.sample`)

```ini
[common]
RuntimePath = /var/run/nimoos
DataPath = /var/lib/nimoos/wiki
LogPath = /var/log/nimoos

[wiki]
DefaultScanIntervalSec = 21600          # 6 小时定期对账
ScanOnlyScanIntervalSec = 600           # scan_only 模式 10 分钟
WikiWriteDebounceSec = 5                # 同一节点最少 5 秒重写一次
EventDebounceMs = 200                   # 同 (root,path,op) 200ms 内合并
ChildMapAggregateThreshold = 50         # 同扩展名 >50 个文件聚合显示
RecentChangesKeep = 20                  # Recent Changes 段最多 20 条
RecentChangesRetentionDays = 90         # 90 天后 archived=1, 180 天后物理删除
ShutdownFlushTimeoutSec = 5             # SIGTERM 时尝试 flush 上限
ContainerDirs = node_modules,.git,__pycache__,venv,.venv,...
```

启动时若 `/etc/nimoos/wiki.conf` 不存在,会用 sample 写入一份。

---

## 启动顺序与依赖

systemd 依赖关系 (`build/sysroot/usr/lib/systemd/system/nimoos-wiki.service`):

```
nimoos-gateway.service ──┐
nimoos-message-bus.service ─┼──▶ nimoos-wiki.service
nimoos-local-storage.service ┘
```

`Type=notify`,`SdNotify(Ready)` 后才视为启动完成。

main.go 启动顺序 (关键):

1. 打开 SQLite + 创建数据/日志目录
2. **对每个 enabled Root 跑一次 Reconciler 对账**(补齐重启期间漏掉的文件变化)
3. 启动 Watcher (fsnotify) + EventProcessor + WikiWriter + 周期 Reconciler + archive job
4. 绑定随机端口 + 写 `wiki.url`
5. 向 Gateway 注册 `/v1/wiki` 和 `/doc/v1/wiki`
6. SdNotify(Ready)
7. 启动 HTTP server

收到 SIGTERM:WikiWriter 尝试 flush 所有 dirty 节点(上限 `ShutdownFlushTimeoutSec`,默认 5s),然后 cancel 所有 goroutine + http.Shutdown。

---

## 构建与部署

```bash
# 标准服务构建(CGO=1,需要 gcc)
cd NimoOS-Wiki && CGO_ENABLED=1 go build -o nimoos-wiki .

# 多架构发布(amd64 + arm64)
goreleaser release --snapshot --clean

# 一键安装到系统(systemd unit + conf + binary + enable)
sudo bash nimo_os_docs/scripts/install-wiki.sh --start

# 更新已部署的服务(只换二进制 + 重启)
bash nimo_os_docs/scripts/deploy.sh wiki

# 跑测试
go test ./...                              # 单元 + repo
go test -tags integration ./tests/...      # E2E (~10s,涉及 fsnotify)
```

`build/scripts/` 下是给官方 `.deb` 安装包用的 setup/migration/cleanup 三件套(numeric prefix `08`,NimoOS=03、AI=07),由 goreleaser 打包时连同 `build/sysroot/` 一起放进 archive。

---

## 与其他服务的关系

- **依赖 UserService**:从 `/var/run/nimoos/` 取公钥校验 JWT。
- **依赖 Gateway**:启动时通过 `POST /v1/gateway/routes` 注册 `/v1/wiki` 和 `/doc/v1/wiki`。
- **依赖 LocalStorage**(可选):`GET /v1/wiki/candidates` 时从 `/var/run/nimoos/local-storage.url` 读地址,调 `GET /v1/storage` 列挂载点。不可用时返回空列表。
- **依赖 MessageBus**:通过 Unix socket `/tmp/message-bus.sock` 发 `Wiki:*` 事件;不可用时静默降级,不影响主流程。
- **被 NimoOS-AI 间接依赖**(规划):Agent 通过 Wiki 定位用户数据的「主题地图」,再决定要不要进一步检索具体内容(下一步项目)。

---

## 设计要点

1. **DB 是 user-notes 的权威源,不是文件**。WikiWriter 渲染时直接从 `wiki_nodes.user_notes` 取内容拼接,不再「先读现有 .wiki.md 提取保留区」。这消除了 SMB 并发编辑期间 WikiWriter 覆盖用户笔记的风险。外部编辑由 Watcher 路径 2 反向同步进 DB,再让 WikiWriter 重新落盘。

2. **fsnotify 事件三路分流**(`service/scanner/watcher.go`):
   - **路径 1**:`.wiki.md.tmp` / 系统噪声 → 直接丢弃
   - **路径 2**:`.wiki.md` → 按 op 分支处理;CREATE/MODIFY 时比对 `mtime` 与 `wiki_nodes.last_flushed_mtime`,匹配则是自己刚写的(忽略),不匹配则是外部编辑(触发反向同步);DELETE/MOVED_FROM 直接标 dirty 让 WikiWriter 重建
   - **路径 3**:普通文件/目录 → 入 file_events;新建目录如果是容器(node_modules 等)则单条 opaque 条目 + 不递归,否则递归 backfill

3. **目录 rename 的级联 UPDATE 必须 case-sensitive + escape-safe**。`PRAGMA case_sensitive_like = ON` 是底线;每个前缀 LIKE 还得 `EscapeLikeArg` 处理 `_` 和 `%` + `ESCAPE '\\'`,否则用户目录里出现下划线时会污染数据。

4. **MOVED_FROM/MOVED_TO 配对在 EventProcessor 做**,不在 Watcher。Linux 的 `fsnotify` 库不暴露 inotify cookie,Watcher 把 Rename(src) 和 Create(dst) 当独立事件发出;EventProcessor 在 1 秒时间窗里把同 root、Create.IsDir=true 的 Rename+Create 配对成单个 rename 事件,然后跑级联 UPDATE。不能在窗内配上的 Rename 退化为 Delete。

5. **大目录聚合 vs 容器目录跳过是两件事**。容器目录(node_modules)整体不索引、Child Map 只显示一行「N 个文件 (已跳过)」;**有理解价值的同质大目录**(照片夹)file_index 仍然记录每个文件(给未来向量库用),但 Child Map 渲染时按扩展名聚合("10342 个 .jpg, 124 个 .raw")。

6. **不**做内容搜索。Wiki 是地图,不是证据库。所有「具体内容里哪里提到了什么」的查询走未来的搜索服务(Parser 解析 + 全文/向量索引),Wiki 提供 `_internal/file-events` 接口给 Parser worker 消费文件变化流。

---

## 已知限制 / 接下来要做

详见 spec §12「待将来项目接续的开放事项」:

1. **AI 摘要 Worker** — `Summary` 和 `Key Sources` 段当前是占位符 `_暂未生成_`;按文件类型设计 Skill(markdown / pdf / 源码 / 图集 / 视频...)是单独项目。
2. **Parser Worker + 向量库** — `parse_status` 表已建、`_internal/file-events` 已实现,等接入。
3. **跨 Root 软链接** — 当前不跟随,目标若在另一个 Root 内由那个 Root 自行扫描。
4. **NFS/CIFS 上的事件可靠性** — 自动降级为 `scan_only`,延迟取决于 `ScanIntervalSec`。
5. **UI** — 当前没有前端,所有交互通过 API。

---

## 实施现状 vs 设计 spec(2026-05-20 对齐)

为避免文档与代码漂移误导读者,这里把「设计有、代码没做(或做了一半)」的差异统一列出。修复优先级仅供参考。

### A. 设计有,代码完全没实现

| 项 | spec 来源 | 当前代码行为 | 影响 |
|---|---|---|---|
| `storage_mode=mirror` | 2026-05-13 §5(line 317)、§8.1 | `WikiWriter` 始终写 `<root>/.wiki.md`,无 mirror 分支 | 不可写 Root 没有 fallback;写测试失败的提示信息在骗用户 |
| inotify watch 上限检测降级 | 2026-05-13 §9 line 577 | 没有读 `/proc/sys/fs/inotify/max_user_watches`,触顶时只能静默丢 watch | 大型 NAS 上部分目录失去实时事件,只能靠 6h 周期 reconcile 兜底 |
| `Wiki:PendingChanged` / `Wiki:WriteFailed` 事件 | 2026-05-13 §7.4 | 完全没人 publish | UI / 监控拿不到 pending 计数变化和写盘失败告警(`Wiki:RootEnabled`/`Wiki:RootDisabled` 已实施于 2026-05-20) |
| Root 路径运行时消失自动标 `enabled=0` | 2026-05-13 §9 line 574 | Watcher 报错只记日志,不动 `enabled` | 拔盘后 wiki_root 仍是 enabled,反复试写失败 |
| `wiki_nodes.level='system'` 单例顶层节点 | 2026-05-13 §4.1 schema | 无任何 seed 路径,完全死代码 | 实际只有两层(space / project),三层结构是假的 |
| 用户手编 system 区后备份到 user-notes 上方 | 2026-05-13 §9 line 576 | WikiWriter 不读现有文件,被破坏的 system 区直接被下次 flush 覆盖,无备份 | 用户手贱在 system 区写的东西会无声丢失 |
| inotify cookie / 跨 watch 的 rename 跟踪 | 2026-05-13 §6.1 末段 | EventProcessor 用 1s 时间窗口做 best-effort 配对,失败退化为 delete | 跨 watch 的 mv 会丢前缀级联,变成两个独立的 delete+create |

### B. 设计有,代码部分实现 / 有逻辑漏洞

| 项 | 当前缺陷 |
|---|---|
| user-notes 反向同步 | `Watcher.SyncOut` chan 容量 64,满了 `default` 丢弃 → 一次外部编辑事件丢了之后,这次编辑的笔记永远进不了 DB,得等用户再改一次 |
| WikiWriter ↔ reverse-sync 之间的 user_notes 时序 | ~~spec §6.5 强调了 mtime + DB commit 的顺序~~ **已修复 2026-05-20**:`pkg/nodelock` 提供 per-path mutex,Writer.FlushOne 和 Processor.SyncUserNotesFromDisk 通过 `main.go` 注入的共享 `*nodelock.Locks` 在同 nodePath 上串行 |
| 服务停机期间 `.wiki.md` 的外部编辑 | **已修复 2026-05-20**:`Processor.BootSyncRoot` 在 main.go 启动时(`bootReconcile` 之后、watcher 注册之前)对每个 enabled root 的所有 wiki_nodes 跑一次 stat-vs-`last_flushed_mtime` 比对,有变化的直接调用 SyncUserNotesFromDisk 同步进 DB |
| Boot reconcile 和 Watcher 注册之间的窗口 | main.go 是「先 reconcile,再 `wch.Watch(...)`」,两步都是 `WalkDir`,期间产生的文件变化两边都看不到,要等下一轮周期 reconcile 才补上 |
| Reconciler 修改检测 | 仅靠 `(mtime, size)` 判 modify,`touch` / `cp -p` 等"内容变了但元数据不变"完全检测不到 |
| 删 Root 目录后 wiki_node 残留 | 处理 fsnotify delete 只动 `file_index`,不动 `wiki_nodes`。删整个 Root 目录后,DB 留下一个孤儿 dirty 节点,WikiWriter 反复试写 ENOENT |
| `mtime <= LastFlushedMtime` 判定 | 毫秒粒度下,用户编辑恰好和 WikiWriter 落盘同一毫秒 → 被当作自己的回声丢掉;低概率但非 0,网络 FS 更易触发 |
| MessageBus 事件聚合 | **已修复 2026-05-20**:`ProcessBatch` 末尾按 `root_id` 去重发布,一次 batch 内每个 root 最多一次 `Wiki:RecentChanged` |
| 内部接口 stub | `_internal/needs-summary` / `summary` / `index-status` 都返回 503,设计预留但 worker 接进来之前都用不了 |

### C. 设计与代码不一致(文档漂移)

| 项 | 旧设计 | 当前代码 |
|---|---|---|
| JWT 中间件 | 2026-05-13 §7 写"所有路由强制 JWT" | 2026-05-14 v2 design §4.2 改成 localhost 豁免 + 信任 `X-NimoOS-User-ID`,**代码按 v2 跑** —— 本文档已修正 |
| `recent-changes` 查询参数 | 旧描述只有 `root_id` + `limit` | 代码支持 `since_ms` 游标,2026-05-14 v2 design §4.5 加的 —— 本文档已修正 |
| Subwikis 语义 | spec `/node` 响应里有 `subwikis` 字段,设计语境暗示是嵌套 wiki 结构 | 实际是按 path 直接前缀找**当前 wiki_nodes 表里恰好是它直系下一级的**节点,跨级 Root(如 `/DATA` 和 `/DATA/Projects/nimoos`,中间没注册 `/DATA/Projects`)不会互相挂上 |

### D. 修复优先级建议(我个人排序)

**已完成(2026-05-20):**
- ✅ B 区:WikiWriter ↔ reverse-sync 时序、服务停机期间外部编辑、事件聚合 → 修复(详见 `nimo_os_docs/docs/superpowers/plans/2026-05-20-wiki-reliability-fixes.md`)
- ✅ A 区:`Wiki:RootEnabled` / `Wiki:RootDisabled` 事件 → 已实施

**还要做的(按优先级):**

1. **A 区第 1 项 `mirror` 模式**:要么实施,要么从 API + 错误信息里完全移除,别让用户以为有 fallback。
2. **B 区第 1 项 `Watcher.SyncOut` 满了 drop**:这次没动,仍是 chan 容量 64 的丢任务路径 —— 一次性外部编辑高峰下仍会丢笔记。
3. **A 区第 5 项 `level='system'` 死代码**:把 schema enum 收成 `{space, project}`,把 Render 里的 `Subwikis` 字段语义说清楚。
4. **A 区第 2 项 inotify 上限检测降级**:大型 NAS 上的实际可靠性问题。
5. 剩下的(reconciler 仅靠 (mtime,size)、删 root 目录后孤儿 wiki_node、`Wiki:PendingChanged`/`WriteFailed` 事件):优先级更低。
