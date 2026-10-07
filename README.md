# level-authoring-core

一个以**库**形式提供的关卡 / 地图编辑内核。纯进程内运行，不依赖任何外部服务，
也不使用真实凭据。所有编辑表达为**可应用、可撤销的命令**，编辑结果可**确定性地
打包为带版本的产物**。

- 语言：Go（module `github.com/cuixudong74/level-authoring-core-go`）
- 依赖：仅 Go 标准库
- 包路径：`levelcore`

## 快速开始

```go
doc := levelcore.NewDocument(64, 64)
ed, err := levelcore.NewEditor(doc, levelcore.DefaultConfig())

ts := time.Unix(1700000000, 0) // 时间戳由调用方提供，保证跨机器确定
ed.Submit(levelcore.NewAddLayerCommand(
    levelcore.Layer{ID: "ground", Name: "Ground", Width: 64, Height: 64}, ts))

ed.Submit(levelcore.NewSetTileCommand("ground", levelcore.Point{X: 3, Y: 4}, 7, ts))
ed.Submit(levelcore.NewPlaceEntityCommand(levelcore.Entity{
    ID: "e1", Kind: "npc", LayerID: "ground",
    Pos: levelcore.Point{X: 10, Y: 10}, Solid: true,
}, ts))

ed.Undo() // 撤销最近一次编辑
ed.Redo() // 重做

pkg, err := levelcore.Pack(ed.Document(), ed.Artifact()) // 确定性打包
d2, a2, err := levelcore.Unpack(pkg)                     // 安全读回
```

## 编辑对象

| 对象 | 内容 |
| --- | --- |
| 多层地形 `Layer` | 命名的二维单元网格，单元值为 `uint16`；按 16×16 分块稀疏存储 |
| 实体 `Entity` | 摆放在某图层上的对象，含类型、坐标、`Solid` 标记、字符串属性 |
| 区域 `Zone` | 左闭右开矩形标记（出生点、触发区等），含类型与字符串属性 |

## 命令语义

所有命令实现 `levelcore.Command`，通过构造函数创建并交给 `Editor.Submit` /
`SubmitBatch`：

| 构造函数 | 语义 |
| --- | --- |
| `NewSetTileCommand(layer, pos, value, ts)` | 设置某层某格地形单元值；`0` 表示清除 |
| `NewAddLayerCommand(layer, ts)` | 新增图层（尺寸固定，不越出地图） |
| `NewRemoveLayerCommand(layerId, ts)` | 删除图层及其单元（其上不得有实体） |
| `NewPlaceEntityCommand(entity, ts)` | 摆放实体 |
| `NewMoveEntityCommand(entityId, to, ts)` | 移动实体（连续拖拽自动合并） |
| `NewRemoveEntityCommand(entityId, ts)` | 删除实体 |
| `NewAddZoneCommand(zone, ts)` | 新增区域标记 |
| `NewRemoveZoneCommand(zoneId, ts)` | 删除区域标记 |

### 撤销 / 重做

- `Undo()` 撤销最近一条历史；批次内命令按**提交顺序的逆序**撤销。
- `Redo()` 重新应用最近被撤销的历史。
- 撤销之后再提交新编辑，会**截断重做历史**，符合通常编辑器语义。
- 撤销 / 重做使派生产物同步回到对应状态，不残留过期内容。
- 撤销 / 重做会产生新的**修订号**（Revision 单调递增），用于区分不同执行轨迹；
  文档与产物的**派生内容**则只由命令序列决定。

### 命令合并（连续同类微调）

合并规则由 `Config.Coalesce.Window` 定义（默认 500ms），且**时间戳由提交方
随命令提供**，内核不读墙钟：

- 连续的 `SetTile`：同一图层、同一格、同种类、时间戳在窗口内，并入同一条历史；
- 连续的 `MoveEntity`：同一实体在窗口内的多次移动，并入同一条历史。

合并后的历史条目保留**最早命令的逆操作信息**（一次撤销回到合并前）与
**最新命令的当前状态**；条目 `Count` 记录并入的原始命令数。合并是折叠式的，
历史中保存的命令槽恒定为 2，不因微调次数增长。

合并前后的可解释对齐：合并 N 次同目标 `SetTile` 后，文档状态等于“直接执行
最后一次取值”，历史长度比“不合并”少 N-1 条。

### 原子批次

`SubmitBatch(cmds)` 把多条命令作为一个整体提交：

1. 顺序在工作副本上应用全部命令（文档采用写时复制，不污染已发布快照）；
2. 在“全部应用后的最终文档”上做跨对象约束校验；
3. 任一命令结构非法或最终约束失败：**逆序回滚全部已应用部分**，文档、产物、
   历史、修订号完全回到提交前。

失败返回的 `*levelcore.RejectError` 中，`CommandIndex` 定位到批次中触发失败的
具体命令下标（单条提交时为 0）。

## 约束规则

拒绝原因通过稳定的错误码 `RejectError.Code` 区分：

| 错误码 | 规则 |
| --- | --- |
| `INVALID_VALUE` | 地图 / 图层尺寸必须为正；对象 ID、实体 / 区域 Kind 非空；区域矩形非空 |
| `OUT_OF_BOUNDS` | 图层尺寸不越出地图；实体坐标不越出其图层；区域不越出地图；单元不越出图层 |
| `ILLEGAL_OWNER` | 实体必须归属已存在图层；删除图层前其上不得仍有实体 |
| `NOT_FOUND` | 移动 / 删除引用的对象必须存在；顺序数组不得引用缺失对象 |
| `DUPLICATE_ID` | 图层 / 实体 / 区域 ID 不得重复 |
| `OVERLAP` | 同一图层上 `Solid=true` 的实体不得占据同一格（单点占用模型） |
| `CORRUPT` / `UNKNOWN_VERSION` / `CHECKSUM_MISMATCH` | 打包读入阶段的损坏、版本与校验和问题 |

约束被拒绝后文档与编辑前**逐字段一致**（批次会逆序回滚）。

## 派生产物（Artifact）

`Editor.Artifact()` 返回由文档确定性派生、不可变的产物快照：

- `Chunks[layerID]`：图层全量块网格的确定性编码。块编码仅包含非零单元，
  格式为 `idx:v` 对按块内下标升序、以逗号连接（例：`5:7,18:2`）；空块为空串。
- `EntityIndex[layerID]`：该图层实体按 ID 升序的摘要
  `id|kind|x|y|solid|k=v;k=v`。
- `ZoneList`：区域按 ID 升序的摘要
  `id|kind|minX,minY,maxX,maxY|k=v;k=v`。
- `Revision`：编辑 / 撤销 / 重做产生的单调修订号。
- `BuiltAt`：固定字符串 `"deterministic"`，不含墙钟。

### 增量与全量一致性

- 每次提交 / 撤销 / 重做只更新受影响的块、实体索引项与区域项；
- 测试 `TestIncrementalEqualsFull` 在每一步都用从零全量重建与增量结果逐字段
  比对，要求完全一致（撤销路径同样校验）。

## 打包格式

`Pack(doc, artifact) -> []byte`，大端定长头部 + 规范化 JSON + 校验和：

```
偏移   长度  字段
0      4    magic "LVPK"
4      2    version (uint16, 当前为 1)
6      4    payloadLen (uint32)
10     N    payload（canonical JSON，见下）
10+N   32   SHA-256(payload)
```

- JSON 中数组均按确定顺序输出（图层顺序、ID 排序、块行列序、单元下标序、
  属性键排序），同一文档重复打包字节一致；
- 读入时依次校验：长度、magic、版本范围（`[MinSupportedVersion,
  MaxSupportedVersion]`）、声明长度、SHA-256 校验和、JSON 结构与全部一致性
  约束。任何一步失败都返回 `*RejectError`，不产生需人工清理的状态。
- JSON 形态可用 `MarshalDocument` / `UnmarshalDocument` 单独使用。

旧版本（`< MinSupportedVersion`）与未来版本（`> MaxSupportedVersion`）均以
`UNKNOWN_VERSION` 安全拒绝；载荷损坏以 `CHECKSUM_MISMATCH` / `CORRUPT` 返回。

## 规模与上限

- 地形按 16×16 分块稀疏存储；文档快照、地形块、图层结构、产物块表均采用
  **写时复制 / 持久化结构**。单次编辑成本只与本次改动量（改动块、受影响实体）
  相关，不随文档总单元数、实体总数线性增长；`Document()` / `Artifact()`
  返回快照为 O(1)。
- 重叠检查使用空间占用索引（O(1)），“图层是否被实体引用”使用每图层计数（O(1)）。
- `Config.HistoryLimit`（默认 256）：撤销历史条目上限，超出后淘汰**最旧**条目，
  不影响最近编辑的正确性；撤销之后新编辑截断重做历史。
- `Config.CacheLimit`（默认 512）：块编码 LRU 缓存条目上限，按块指针身份缓存
  （块不可变，命中不会返回过期值）。

## 测试

```
go test ./...            # 全部用例
go test -race ./...      # 竞态检测
go test -bench . ./...   # 微基准（编辑成本与规模解耦）
```

覆盖：撤销 / 重做与重做截断、命令合并与合并后撤销、批次整体提交失败与命令定位、
各类约束拒绝及拒绝后不变、增量与全量逐字段一致（含撤销回退）、打包版本兼容与
损坏输入安全处理、跨机器确定性重放、历史 / 缓存上限淘汰与大体量编辑成本。
