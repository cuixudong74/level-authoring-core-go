# level-authoring-core-go

`levelcore` 是一个以 **Go 库** 形式提供的关卡 / 地图编辑内核。它在单进程内运行，
不依赖任何外部服务，也不需要凭据。核心能力：

- 每次编辑都是一条**可应用、可撤销、可重做**的命令；
- 同一份初始文档 + 同一串命令在任意机器上产生**逐字段一致**的文档、历史与产物；
- 多条命令可作为**一个原子条目**提交，中途失败整体回滚；
- 文档一致性约束（重叠、越界、非法归属、悬空引用）在写入前校验；
- 派生产物（地形统计、实体索引、区域成员、内容摘要）支持**增量重建**，与全量
  重建逐字段一致，撤销时随之回退、不残留过期内容；
- 导出为**带版本号与校验和**的确定性 JSON 包，可识别并安全拒绝旧版本/损坏输入；
- 单次编辑的时间 / 内存只与编辑涉及的局部数据相关，不随文档总规模线性增长；
  撤销历史与区域缓存都有可控上限并按策略淘汰。

## 安装与快速上手

```go
import "github.com/cuixudong74/level-authoring-core-go/levelcore"

doc, _ := levelcore.NewDoc(64, 64)
ed, err := levelcore.NewEditor(doc, levelcore.Options{})
if err != nil { /* 初始文档不合法 */ }

// 单条或多条命令一起提交（多条 = 一个原子复合条目）
err = ed.Commit(
    &levelcore.RegionAdd{Region: levelcore.Region{
        ID: "spawn", Name: "出生点", Area: levelcore.Rect{X: 0, Y: 0, Width: 8, Height: 8}}},
    &levelcore.EntityAdd{Entity: levelcore.Entity{
        ID: "hero", Type: "player", X: 1, Y: 1, Width: 1, Height: 1, Region: "spawn"}},
    &levelcore.SetTiles{Layer: "ground",
        Cells: []levelcore.Cell{{X: 1, Y: 1, Terrain: "grass"}, {X: 2, Y: 2, Terrain: "water"}}},
)

ed.Undo() // 整个批次作为一条历史撤销
ed.Redo()

art := ed.Artifact()           // 派生产物
hist := ed.History()          // 已提交历史（含合并计数、序号、逻辑时钟）

data, _ := levelcore.MarshalBundle(ed)
ed2, err := levelcore.UnmarshalBundle(data) // 跨进程 / 跨机器恢复
```

运行测试：

```bash
go test ./...                 # 完整测试（含大规模场景）
go test ./... -short          # 跳过规模测试
go test -bench . ./...        # 单次编辑成本基准
```

## 文档模型

`Doc` 包含三类编辑对象：

| 字段 | 内容 |
| --- | --- |
| `Width`,`Height` | 文档尺寸（正数） |
| `Layers map[string]Layer` | 多层地形；每层是 `Width*Height` 的行优先格子 `[]Tile` |
| `Entities map[string]Entity` | 实体摆放：ID、类型、外接矩形（`X,Y,Width,Height`）、所属区域 |
| `Regions map[string]Region` | 区域标记：ID、名称、矩形范围 |

图层是相互独立的地形层；空格子用 `Terrain == ""` 表示，图层之间永不冲突。

## 命令语义

所有命令实现 `Command` 接口（应用、撤销、合并键、合并、影响集、名称）。
第一次 `apply` 时命令会从现场快照“前置状态”（prev），因此调用方不需要预知旧值；
快照字段随序列化导出，导入后撤销仍精确。

| 命令 | 作用 | 可合并 |
| --- | --- | --- |
| `SetTile` | 写单层单格地形 | 同层同格连续微调合并 |
| `SetTiles` | 原子写单层多格；同格多次写入以最后一次为准 | 同层连续批次按格子并集合并 |
| `EntityAdd` / `EntityRemove` | 放置 / 删除实体 | 不合并 |
| `EntityMove` | 改实体位置/尺寸 | 同实体连续移动合并（保留最初 From、最新 To） |
| `EntitySetType` | 改实体类型 | 同实体连续修改合并 |
| `EntitySetOwner` | 改所属区域（空串 = 无归属） | 同实体连续修改合并 |
| `RegionAdd` / `RegionRemove` | 增加 / 删除区域 | 不合并 |
| `RegionUpdate` | 改区域名称/面积 | 不合并 |
| `Composite` | 若干命令的原子容器 | 永不与相邻条目合并 |

### 撤销 / 重做与合并策略

- `Commit(...)` 后逻辑时钟 `clock` 加一。
- 若新命令与历史栈顶端**合并键相同**、且提交时钟差 `<= MergeWindow`
  （默认 `8`，`0` 关闭合并），两者合并为一条历史条目，`MergeCount` 累加。
  合并后的命令保留最早的前置状态，因此**一次撤销**即回到合并起点；
  文档最终状态等于最后一次写入。这保证了“合并前后文档状态与历史长度可对齐”：
  合并前后的最终文档完全相同，只是历史条目数从 N 变为 1（`MergeCount = N`）。
- `Undo` 弹出最近条目压入重做栈；在任何**新编辑**发生时，重做栈被整体截断
  （标准 undo/redo 语义）。
- 历史条目有稳定的单调序号 `Seq`；超过 `Options.HistoryLimit`
  （默认 `512`）时淘汰**最旧**条目，最近编辑始终可撤销。
- 一次 `Commit` 传多条命令时自动包成一个 `Composite`；单独提交一个
  `Composite` 会原样存储。

### 原子提交与失败定位

提交按顺序对每条子命令执行 **先校验、再暂存影响、再应用、再增量更新产物**。
任何一条失败：

1. 已应用的前序命令按逆序 `undo`，其产物变更也按同样的增量路径回退；
2. 不写入任何历史条目，逻辑时钟不变；
3. 返回的 `*levelcore.Error` 中 `CommandIndex` 指向失败子命令在批次中的下标，
   `Kind` 给出可区分的原因。

因此失败之后文档与产物与编辑前**字节级一致**。

## 一致性约束

校验在任何写入前进行，拒绝原因用 `Error.Kind` 区分：

| Kind | 触发条件 |
| --- | --- |
| `KindOutOfBounds` | 格子 / 实体外接矩形 / 区域范围超出文档，或矩形尺寸非正 |
| `KindInvalidArgument` | 空 ID、空图层名、空类型、空批次等 |
| `KindNotFound` | 修改 / 删除不存在的图层、实体或区域 |
| `KindDuplicate` | 新增的实体 / 区域 ID 已存在 |
| `KindBadReference` | 实体的 `Region` 指向不存在的区域（悬空归属） |
| `KindOverlap` | 两个实体的外接矩形相交（以已有实体为参照；不检查地形） |
| `KindIllegalOwner` | 删除仍被实体归属的区域（必须先解除所有归属） |
| `KindUnsupportedVersion` | 导入了不支持的包版本 |
| `KindCorrupt` | JSON 损坏、校验和不符、hex 非法、载荷结构非法等 |

重叠候选通过空间哈希网格查询，成本只与局部密度相关；对超大初始文档的校验也是
线性的。

## 派生产物格式

`Artifact` 完全由文档决定：

```jsonc
{
  "tile_counts":  { "<layer>": { "<terrain>": 12, "": 52 } }, // 空格子也计入
  "region_index": { "<regionID>": ["<entityID>", "..."] },    // 锚点(左上角)落在区域内
  "entity_index": {
    "<entityID>": { "center_x": 3, "center_y": 4, "inside_owner_region": true }
  },
  "digest": "0123abcd..." // 64 位 FNV-1a 加性校验和的小写十六进制
}
```

- `region_index`：实体锚点（左上角）被区域矩形包含的实体 ID 列表，按 ID 排序；
  空间包含与“归属”相互独立。
- `entity_index.inside_owner_region`：实体锚点是否被**其归属**区域包含。
- `digest`：对地形统计、图层数、实体索引、区域成员做加性组合的确定性摘要，
  增量更新时只“减旧贡献、加新贡献”，因此计算成本与改动规模成正比。

### 增量与全量一致

- `NewEditor` 对初始文档做一次全量构建。
- 每次提交/撤销只把 `touchSet`（受影响的格子、实体、区域、相关矩形）交给增量引擎；
  地形按格子改直方图，实体/区域通过空间哈希做局部成员变更，图层在撤销而消失时
  整体移除。
- 测试通过以下方式强保证一致性：随机命令流（80 个确定性种子）每一步都把增量产物
  与“对当前文档从零全量重建”的产物做深度比较；全部撤销后再逐次重做同样比较。

## 性能与上限

- 地形编辑只触及列出的格子；实体放置/移动只查询局部网格；区域面积变化只重算
  落在新旧区域内的成员及归属该区域的实体的 `inside` 标志。
- `BenchmarkLocalTileEdit` / `BenchmarkLocalEntityMove` 对比 1k / 10k / 100k
  实体文档下单次编辑耗时，实测均为微秒级且基本不随后者变化
  （见 `go test -bench .`）。
- 历史上限 `Options.HistoryLimit`（默认 512）；区域成员列表 LRU 缓存上限
  `Options.RegionCacheSize`（默认 1024，`0` 关闭缓存）。淘汰只影响最旧历史与
  缓存命中，不影响最近编辑的正确性。

## 包格式与版本兼容

当前版本 **V1**（`levelcore.BundleVersionV1 = 1`）：

```jsonc
{
  "version": 1,
  "sha256": "<payload 的 SHA-256 小写十六进制>",
  "payload": "<canonical JSON(payloadV1) 的十六进制>"
}
```

`payloadV1` 按固定字段顺序、map 按键排序序列化，内容包括：

- 完整文档（图层 / 实体 / 区域均按 ID 排序输出）；
- 当前仍保留的历史条目（序号、逻辑时钟、合并计数、带类型标签的命令；
  命令的 prev 状态也在其中，导入后可直接撤销）；
- 当前逻辑时钟与编辑器选项；
- `format: 1`。

外层用 SHA-256 防篡改；任何字段缺失、hex 非法、校验和不符、历史序号不连续或
时钟倒退都会得到 `KindCorrupt`，不会返回半成品编辑器，也不需要人工清理。

遗留版本 **V0**（仅重放日志）：

```jsonc
{ "version": 0, "width": 64, "height": 64,
  "commands": [ { "type": "set_tile", "data": { ... } } ] }
```

读取时在空文档上确定性重放，得到与 V1 相同的编辑器（含可撤销历史）。
未知版本号返回 `KindUnsupportedVersion`。

确定性保证：文档/历史/产物中所有 map 都按排序后的键输出，序列化不使用时间、
随机数、环境变量或指针地址，因此同状态在任意机器上得到字节相同的包。

## 包与文件结构

```
levelcore/
  doc.go          # 文档模型（多层地形 / 实体 / 区域）
  command.go      # 命令实现：apply/undo/合并/影响集
  constraints.go  # 写入前一致性校验
  history.go      # 有界撤销/重做栈与合并策略
  artifact.go     # 派生产物、空间哈希、增量引擎、摘要
  lru.go          # 有上限 LRU 缓存
  editor.go       # Editor：原子提交、回滚、编排
  export.go       # V1/V0 版本化导入导出
  wire.go         # 命令/文档的确定性有线序列化
  errors.go       # 结构化错误与 Kind 分类
```

## 错误处理

所有失败都返回 `*levelcore.Error`，可用断言函数分类：

```go
if levelcore.IsKind(err, levelcore.KindOverlap) {
    idx := err.(*levelcore.Error).CommandIndex // 批次中失败命令下标
}
```

## 并发性说明

`Editor` 不是并发安全的；多协程使用时请在外部加锁。所有计算都在本进程完成，
不进行任何网络或文件系统调用。
