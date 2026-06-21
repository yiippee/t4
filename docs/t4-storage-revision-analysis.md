# T4 存储、恢复与 Revision 机制分析

本文汇总本轮对 T4 的源码分析和测试结论，覆盖 S3 持久化、一致性、Kubernetes 无本地持久卷场景、`tests/main.go` 调试、对象存储 versioning、KV revision/version 以及 compaction。

## 分析范围与源码入口

主要阅读路径如下：

- 配置入口：[config.go](../config.go)，重点是 `WALSyncUpload`、`SegmentMaxAge`、`CheckpointInterval`、`CheckpointEntries`。
- 写入和提交路径：[writes.go](../writes.go)、[leader.go](../leader.go)，重点是 `Put`、`Txn`、`Compact`、`commitLoop`。
- WAL 持久化：[internal/wal/wal.go](../internal/wal/wal.go)、[internal/wal/entry.go](../internal/wal/entry.go)。
- Checkpoint 和 S3 恢复：[internal/checkpoint/checkpoint.go](../internal/checkpoint/checkpoint.go)、[node.go](../node.go)。
- 本地 KV 存储与历史 revision：[internal/store/store.go](../internal/store/store.go)。
- S3 适配层：[pkg/object/s3.go](../pkg/object/s3.go)。
- etcd 兼容层：[etcd/server.go](../etcd/server.go)、[etcd/kv.go](../etcd/kv.go)。

## 写入一致性不是依赖定时 Checkpoint

T4 的持久性核心不是“等下一次定时 checkpoint”，而是 WAL。

单节点且配置了 `ObjectStore` 时，`WALSyncUpload` 默认是 `true`，源码注释明确说明：每次写入会阻塞到 WAL segment 已经持久上传到 S3，再向客户端确认。默认值在 `Config.setDefaults` 中设置。

关键路径：

1. `Node.Put`/`Txn` 分配 revision，并提交给 `commitLoop`。
2. `commitLoop` 调用 `wal.AppendBatch`。
3. `AppendBatch` 写本地 WAL 并 fsync。
4. 如果开启 `WithSyncUpload`，继续调用 `rotateSyncLocked` 同步上传当前 WAL segment。
5. 上传失败时回滚本批 WAL，写入不会被确认。
6. 成功后再应用到 Pebble 并返回给客户端。

因此，默认单节点模式下，如果客户端已经收到成功响应，该写入对应的 WAL 已经进入对象存储；如果节点在上传前挂掉，客户端不会收到成功确认。Checkpoint 是恢复加速层和快照层，不是每次写入的一致性边界。

多节点模式下，`WALSyncUpload` 对确认路径无效：写入依赖 leader/follower WAL ACK 提供持久性，S3 上传走异步路径。这个模型适合节点磁盘可靠或至少不会同时丢失的场景；如果 Kubernetes 中所有 Pod 都没有持久卷且可能同时被替换，需要谨慎评估。

## Kubernetes 无挂载存储是否适合

单节点、无 PVC、使用 S3 兼容对象存储时，T4 默认配置就是偏向这个场景设计的：`WALSyncUpload=true` 可以让已确认写入进入 S3，即使本地 `DataDir` 后续丢失，也可以从 S3 checkpoint + WAL 恢复。

需要满足这些条件：

- `ObjectStore` 正确配置，bucket、endpoint、prefix 固定。
- 不要关闭 `WALSyncUpload`。
- 不要为每次启动生成新的 prefix。
- 对象存储本身需要可靠。

如果设置 `WALSyncUpload=false`，源码注释说明最多可能丢失 `SegmentMaxAge` 时间窗口内已确认但尚未上传的写入。这个配置适合本地盘已经可靠的场景，例如 PVC，不适合无持久卷的单节点 Pod。

Pod 重启还要区分两种情况：

- 容器进程重启但同一个 Pod 的 `emptyDir` 仍在：通常本地数据还在，直接从本地恢复。
- Pod 被删除/重建或节点替换导致 `DataDir` 消失：T4 需要从 S3 的 `manifest/latest`、checkpoint 和 WAL segment 恢复。

## `tests/main.go` 调试结论

测试程序之前无法恢复的主要原因不是 T4 恢复逻辑本身，而是配置问题。

已确认的问题包括：

- 使用动态 prefix，例如基于 `time.Now().UnixNano()` 生成路径，会导致每次启动看见的是一个全新的对象存储命名空间。删除本地 `DataDir` 后，自然找不到旧数据。
- 访问的 endpoint 需要是 MinIO/S3 API 端口。之前的 `:9000` 探测结果不像 MinIO S3 API，后续使用可用的 S3 endpoint。
- 示例需要确保 bucket 存在，否则打开 store 或后续上传可能失败。

当前测试文件使用稳定 prefix，例如 `t4/restore-demo/`，并在启动前检查/创建 bucket。测试逻辑包括：

- 读取固定 key。
- 写入 `/config/timeout-1`。
- 写入 `/config/retries-1`。
- 使用 `node.List("/config/")` 做 prefix 查询。
- 使用 `node.Watch(ctx, "/config/", 0)` 观察 prefix 下的变更。

注意：文档不重复记录测试文件中的密钥。测试配置如需提交，应避免把真实 access key、secret key 写入仓库。

## 删除 DataDir 后如何恢复

T4 从 S3 恢复依赖固定对象布局：

- `manifest/latest` 指向最新 checkpoint。
- checkpoint index 位于 `checkpoint/<term>/<revision>/manifest.json`。
- WAL segment 位于 `wal/<term>/<first-revision>`。
- SST 文件和 Pebble metadata 由 checkpoint index 记录。

启动时，如果本地 `DataDir` 不存在或为空，T4 会尝试读取 `manifest/latest`，恢复 checkpoint，再 replay checkpoint 之后的 WAL。

删除本地目录后不能恢复，常见原因是：

- prefix 变了。
- bucket 或 endpoint 配错。
- 之前没有成功上传 checkpoint/WAL。
- 对象被 GC 或手动删除。
- 使用了错误的 S3 账号、region 或 path-style/endpoint。

普通“恢复最新状态”不要求开启 S3 bucket versioning。S3 versioning 只对基于对象 version ID 的 point-in-time restore 有要求。

## Prefix 查询能力

T4 原生 API 支持 prefix 查询：

```go
kvs, err := node.List("/config/")
```

相关入口在 [node.go](../node.go)：

- `Get(key)`：按完整 key 读取。
- `List(prefix)`：按 prefix 扫描当前 live key。
- `Count(prefix)`：统计 prefix 下 live key 数量。
- `Watch(ctx, prefix, startRev)`：订阅 prefix 下的变更。

`List` 也可以配合 `WithRevision(rev)` 做指定 revision 的 prefix 读取；如果 revision 已经被 compact，会返回 `ErrCompacted`。

## 普通 Put 会写多少 S3 对象

在单节点默认 `WALSyncUpload=true` 的情况下，一个普通 `Put` 在没有 checkpoint 同时发生时，通常对应一次 WAL segment 上传，也就是写 1 个 S3 对象，路径形如：

```text
wal/<term>/<first-revision>
```

并发写入可能通过 `AppendBatch` 合并，多个写入共享一次 WAL fsync 和一次 WAL segment 上传。

如果你观察到一个普通 `Put` 后 S3 中大量对象变化，通常不是这个 Put 本身直接写了很多业务对象，而是叠加了以下行为：

- 节点启动时 `checkpointLoop` 会先写一次启动 checkpoint。
- 到达 `CheckpointInterval` 或 `CheckpointEntries` 会写 checkpoint。
- checkpoint 会上传 SST、Pebble metadata、checkpoint index，并更新 `manifest/latest`。
- checkpoint 后会做 WAL segment GC、旧 checkpoint GC、孤儿 SST GC。
- 如果 bucket 开启了 versioning，`manifest/latest` 等固定 key 的每次覆盖都会留下历史版本。

Checkpoint 写入对象数量是变量，不是固定 1 个：

- 新增或尚未上传的 SST 文件：0 到多个。
- Pebble metadata：多个，例如 `MANIFEST-*`、`OPTIONS-*`、`CURRENT` 等。
- checkpoint index：1 个。
- `manifest/latest`：1 个覆盖写。
- 后续 GC 可能产生 delete marker 或删除请求。

因此，对于单个普通 Put 的直接成本，应把 WAL 上传和 checkpoint/GC 分开观察。

## S3 是否就地更新文件

S3 语义上不是传统文件系统的“就地修改”。对同一个 object key 执行 `PUT`，表现为替换该 key 的当前对象：

- bucket 未开启 versioning：旧内容不会作为可见历史版本保留。
- bucket 开启 versioning：每次 `PUT` 同一个 key 会生成新的 object version，旧版本成为 non-current version。
- bucket versioning suspended：新写入通常进入 null version，已有历史版本不会自动删除。

所以需要正面区分两件事：

- **S3 API 层面**：PUT 是对象替换，不是局部覆盖文件内容。
- **是否“每次修改都新增一个可见历史文件”**：只有开启 bucket versioning 时，才会保留旧版本并导致历史版本持续增长。

T4 中多数 WAL 对象 key 本身是按 revision/sequence 生成的不可变对象，不依赖覆盖；`manifest/latest` 这类固定 key 会被覆盖。如果 bucket 开启 versioning，固定 key 的覆盖会留下多份历史版本。

## 开启和关闭 S3 Versioning

MinIO `mc` 示例：

```powershell
mc alias set t4minio http://<endpoint> <accessKey> <secretKey>

mc version info t4minio/<bucket>
mc version enable t4minio/<bucket>
mc version suspend t4minio/<bucket>
```

AWS CLI 兼容 S3 示例：

```powershell
aws --endpoint-url http://<endpoint> s3api get-bucket-versioning --bucket <bucket>

aws --endpoint-url http://<endpoint> s3api put-bucket-versioning `
  --bucket <bucket> `
  --versioning-configuration Status=Enabled

aws --endpoint-url http://<endpoint> s3api put-bucket-versioning `
  --bucket <bucket> `
  --versioning-configuration Status=Suspended
```

关闭通常是 suspend，不会删除已有历史版本。若此前已开启 versioning，需要额外配置 lifecycle 或手动清理 non-current versions。

## T4 KV 数据的 Revision 与 Version

T4 的 KV 数据有 revision，并且当前源码中也有 per-key version。

内部 `KeyValue` 定义包含：

```go
type KeyValue struct {
    Key            string
    Value          []byte
    Revision       int64
    CreateRevision int64
    PrevRevision   int64
    Version        int64
    Lease          int64
}
```

WAL `Entry` 也持久化这些字段，其中 `Version` 注释为 `etcd-style per-key modification count`。

语义如下：

- `Revision`：本次写入的全局 revision，类似 etcd 的 `ModRevision`。
- `CreateRevision`：key 第一次创建时的 revision。
- `PrevRevision`：上一次修改该 key 的 revision。
- `Version`：该 key 的修改计数；新建为 1，更新后递增，删除后重建重新从 1 开始。

事务语义也接近 etcd：一个 `Txn` 中多个写操作共享同一个全局 revision，因此这些 key 的 `ModRevision` 相同。

在 etcd 兼容层，T4 会把内部 revision 映射为 etcd wire revision：

```go
func toEtcdRevision(rev int64) int64 {
    return rev + 1
}
```

原因是 T4 内部 revision 从 0 开始，而 Kubernetes 等 etcd 客户端不接受 `resourceVersion=0` 的 list 响应。因此 etcd 客户端看到的 `Header.Revision`、`ModRevision`、`CreateRevision` 会比 T4 内部 revision 大 1。

注意：KV revision/version 和 S3 bucket versioning 完全不是同一个概念。前者是数据库逻辑层 MVCC 元数据，后者是对象存储层对象历史版本。

## Revision Compaction

T4 支持 revision compaction，但当前没有内置的自动 compactor 配置。

公开 API：

```go
err := node.Compact(ctx, revision)
```

etcd 兼容层也实现了 `Compact` RPC，会转发到 `node.Compact`。

内部逻辑：

- `Node.Compact` 写入一条 `OpCompact` WAL entry。
- `Store.applyCompact` 设置 compact watermark。
- 删除 compact revision 之前已经不再需要的历史 log entry。
- 保留每个 key 在 compact 边界附近仍需要的最新版本，避免当前读被破坏。

Compaction 后的影响：

- 低于 compact watermark 的历史读会返回 `ErrCompacted`。
- watch 从已 compact 的 revision 开始会失败。
- 当前最新值仍然可读。
- Pebble 的物理空间回收可能还需要等待 LSM 后台 compaction。

源码中没有看到类似 etcd `--auto-compaction-retention` 的 T4 配置。`CheckpointInterval` 和 `CheckpointEntries` 只控制 checkpoint，不等价于 revision compaction。

如果业务需要控制历史 revision 增长，应由外部定期调用：

```go
rev := node.CurrentRevision()
keep := int64(10000)
if rev > keep {
    _ = node.Compact(ctx, rev-keep)
}
```

保留窗口不要过小，否则断线重连的 watcher 可能无法从旧 revision 恢复，只能重新 list。

## Checkpoint、Compaction、GC 的区别

这三个概念容易混淆：

| 机制 | 作用 | 是否删除 KV 历史 revision | 是否影响 S3 对象数量 |
| --- | --- | --- | --- |
| WAL upload | 持久化写入日志 | 否 | 是，上传 WAL segment |
| Checkpoint | 生成可恢复快照 | 否 | 是，上传 SST/meta/index/manifest |
| Revision Compact | 删除本地历史 revision log | 是 | 间接影响后续 checkpoint 内容 |
| S3 GC / `t4 gc` | 删除旧 checkpoint、WAL、孤儿 SST | 否 | 是，减少对象存储占用 |
| S3 Versioning | 保留同 key 对象历史版本 | 否 | 会显著增加历史对象版本 |

## 运维建议

1. 对同一个逻辑数据库使用稳定 S3 prefix。不要使用时间戳 prefix。
2. Kubernetes 无 PVC 的单节点测试或部署，不要关闭 `WALSyncUpload`。
3. 只有需要基于 S3 object version ID 的 point-in-time restore 时，才开启 bucket versioning。
4. 如果开启 S3 versioning，配置 lifecycle 清理 non-current versions。
5. 定期运行 `t4 gc` 清理旧 checkpoint、WAL segment 和孤儿 SST。
6. 根据业务 watch/relist 容忍度，定期调用 `node.Compact` 控制 revision 历史。
7. 监控 `t4_current_revision`、`t4_compact_revision`、checkpoint 指标和 WAL GC 指标。
8. 测试恢复时，应先写入固定 key，确认 S3 中存在 checkpoint/WAL，再删除本地 `DataDir` 并使用同一 prefix 重启。

## 核心结论

- 默认单节点模式下，T4 已确认写入的一致性依赖同步上传 WAL 到 S3，而不是等待定时 checkpoint。
- 无 PVC 的 Kubernetes 单节点场景可以使用 T4，但前提是对象存储可靠、prefix 固定、`WALSyncUpload=true`。
- 删除本地 `DataDir` 后能否恢复，关键看是否能在同一 S3 prefix 下找到有效的 `manifest/latest`、checkpoint 和 WAL。
- 普通 Put 通常只直接产生 1 个 WAL 对象；大量 S3 对象变化多半来自 checkpoint、GC 或 bucket versioning。
- S3 PUT 不是就地修改；开启 bucket versioning 时，同 key 覆盖会产生新的历史版本。
- T4 KV 层有全局 revision，也有 per-key version；它们和 S3 versioning 无关。
- T4 支持手动 revision compaction，但当前没有内置自动 compaction retention，需要业务或外部任务主动调用。
