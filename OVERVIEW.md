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

所有路由强制 JWT (无 localhost 豁免,匹配 AI 服务的安全策略)。`_internal` 组例外:仅监听 localhost,不通过 Gateway 暴露。

### A 组 — Root 管理

| Method | Path | 用途 |
|---|---|---|
| GET | `/v1/wiki/roots` | 列出所有 Root |
| POST | `/v1/wiki/roots` | 启用 Root: `{path, level, watch_mode?, storage_mode?}` |
| DELETE | `/v1/wiki/roots/:id` | 停用 Root,可选 `?purge_files=true` 删除落盘 `.wiki.md` |
| POST | `/v1/wiki/roots/:id/rescan` | 强制对账扫描(把 last_scan_at 清零) |
| GET | `/v1/wiki/candidates` | 枚举 LocalStorage 上报的挂载点候选 |

启用 Root 时做**写测试**(在路径下创建并删除 `.nimoos-wiki-write-test`);失败返回 409,提示用户改用 `storage_mode=mirror`。

### B 组 — Wiki 内容

| Method | Path | 用途 |
|---|---|---|
| GET | `/v1/wiki/tree?root_id=&depth=` | 返回 Wiki 节点骨架(不含 summary) |
| GET | `/v1/wiki/node?path=...` | 读单节点(结构化 JSON,含 etag) |
| GET | `/v1/wiki/raw?path=...` | 读原始 `.wiki.md` 文本 |
| PUT | `/v1/wiki/user-notes?path=...` | 写 User Notes,需 `If-Match: <etag>` 乐观锁 |
| GET | `/v1/wiki/recent-changes?root_id=&limit=` | 跨 Root 拉最近变化 |

### C 组 — 内部接口(localhost only, 不注册到 Gateway)

| Method | Path | 用途 |
|---|---|---|
| GET | `/v1/wiki/_internal/file-events?root_id=&since=&limit=` | 给 Parser worker 拉文件变化(**本次实现**) |
| GET | `/v1/wiki/_internal/needs-summary?limit=` | 给 AI 摘要 worker 拉待摘要节点(503 stub) |
| POST | `/v1/wiki/_internal/summary` | AI 摘要 worker 上报摘要(503 stub) |
| POST | `/v1/wiki/_internal/index-status` | Parser worker 上报解析状态(503 stub) |

### MessageBus 事件

- `Wiki:NodeUpdated` — `{path, root_id}`,节点落盘后
- `Wiki:RecentChanged` — `{root_id}`,新事件入库后
- `Wiki:PendingChanged` — `{root_id, pending_count}`
- `Wiki:RootEnabled` / `Wiki:RootDisabled`
- `Wiki:WriteFailed` — WikiWriter 连续 5 次失败后告警

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
| `wiki_roots` | 已注册的 Root:path / level / watch_mode / storage_mode / enabled / scan_interval_s |
| `wiki_nodes` | 每个有 `.wiki.md` 的目录一行。**`user_notes` 内容直接存这里**,DB 是权威源,`.wiki.md` 是渲染产物;`last_flushed_mtime` 用于 Watcher 区分「自己写的」vs「外部编辑」 |
| `file_index` | 监控范围内的所有文件 + 容器目录占位条目;Reconciler 对账用。`is_opaque=1` 标记容器目录(node_modules 等),内部不递归 |
| `file_events` | 统一的文件事件表(合并旧设计中的 pending_events + recent_changes)。三种消费模式:`processed_at IS NULL` 队列、`since=` 增量查询、`ORDER BY detected_at DESC` 最近变化渲染 |
| `parse_status` | 给将来的 Parser worker 上报解析/索引状态;本服务只在 create 事件时入库 `pending`,不读 |

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
  └── mirror/<root_id>/...         mirror 模式下落盘的 .wiki.md
/var/run/nimoos/
  ├── wiki.url                     服务发现地址
  └── wiki.pid                     systemd PID 文件
/var/log/nimoos/nimoos-wiki.log    zap 日志
```

各启用 Root 下的 `<root>/.wiki.md` (inline 模式) 或 `mirror/...` (mirror 模式)。

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
