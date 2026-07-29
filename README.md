# NimoOS-Wiki

NimoOS 的「**可见长期记忆**」服务 —— 在用户的存储空间里维护 `.wiki.md` 导航地图，让用户和 Agent 都能直接读到「NAS 里有什么、在哪里、属于什么主题」。

> ### About / 关于本项目
>
> NimoOS is a fork of [CasaOS](https://github.com/IceWhaleTech/CasaOS)
> (Apache-2.0), originally developed by IceWhale Technology Co., Ltd.
> Building on that foundation, NimoOS adds an AI agent, RAG-based
> retrieval, a knowledge layer, and a built-in web terminal.
>
> NimoOS 基于 [CasaOS](https://github.com/IceWhaleTech/CasaOS)（Apache-2.0）
> fork 而来，原始项目由 IceWhale Technology Co., Ltd. 开发。在此基础上，
> NimoOS 重建了 AI Agent、RAG 检索、知识库与内置终端等能力。
>
> 归属详情见 [`NOTICE`](./NOTICE)。CasaOS 与 IceWhale 是 IceWhale Technology
> Co., Ltd. 的商标；NimoOS 是独立项目，与 IceWhale 无隶属关系。
>
> 本仓库是 NimoTech 原创，不含 CasaOS 衍生代码。

> ⚠️ Multi-user isolation is incomplete — Photos and Search are not yet
> per-user scoped. Read
> [SECURITY.md](https://github.com/NimoTech/NimoOS/blob/main/SECURITY.md#known-limitations)
> before deploying NimoOS for more than one person.
>
> ⚠️ 多用户隔离尚不完整（Photos 与搜索未按用户隔离）。若要给多人使用，请先阅读
> [SECURITY.md](https://github.com/NimoTech/NimoOS/blob/main/SECURITY.md#known-limitations)。

## 这是什么

绑定 localhost、由 NimoOS Gateway 转发，API 前缀 `/v1/wiki`。

## 主要能力

| 能力 | 说明 |
|---|---|
| `.wiki.md` 导航地图 | 在存储空间内就地维护，用户用任何编辑器都能读 |
| 多根管理 | 可配置纳入哪些目录，支持启停 |
| 增量维护 | 跟随文件变动更新，大目录走后台限速解析 |
| 摘要生成 | 由本地或云端模型生成节点摘要 |

## 与其他服务的关系

Wiki 是 NimoOS 检索栈的一环 —— 与 `NimoOS-Search`（检索聚合）、`NimoOS-Parser`
（文档解析与嵌入）、`NimoOS-AI`（Agent 消费）协同工作。

## 构建

需要完整的 NimoOS monorepo checkout —— 所有 Go 服务通过 `replace` 指向本地的
`NimoOS-Common`，`go.mod` 里的版本号是装饰性的。

```bash
CGO_ENABLED=1 go build ./...   # go-systemd 需要 CGO
go test ./...
```

## 文档

架构、请求流转与运行时细节见 [`OVERVIEW.md`](./OVERVIEW.md)。

## 许可

Apache-2.0，见 [`LICENSE`](./LICENSE)。
