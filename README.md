# experiment-traffic-split-go

可复用的内部实验分流 / 流量拆分 Go 组件。提供：

- 稳定分桶（同一用户同一实验长期稳定，占比调整最小化漂移）
- 属性定向规则（and/or 组合、类型安全、缺失/异常有确定性兜底与解释）
- 互斥层（同一用户在同一层最多命中一个实验）
- 配置热更新（原子切换，读路径无锁；切换失败自动保留上一份配置）
- 配置版本化与灰度放量（按用户哈希逐步放量，回滚过程全程可审计）
- 分配事件采样与异步记录（有界队列、非阻塞、溢出可观测）
- 面向高并发与长期运行：不可变快照 + `atomic.Pointer`，无后台堆积

```go
import "github.com/shikejing/experiment-traffic-split-go/experiment"
```

## 快速开始

```go
cfg, err := experiment.ParseConfig(jsonBytes) // 解码并立即校验
mgr, err := experiment.NewManager(cfg, experiment.ManagerOptions{
    Sink:          sink,             // 实现 experiment.EventSink；nil 表示不记录事件
    EventSampling: 0.1,              // 10% 采样（按 实验+用户 确定性采样）
    EventQueueSize: 4096,
    DropPolicy:    experiment.DropNewest,
})
defer mgr.Close()

d := mgr.Evaluate(experiment.User{
    ID:         "user-1001",
    Attributes: map[string]experiment.Value{"vip": true, "city": "beijing"},
})
a := d.Assignments["checkout-button"]
// a.Variant 是变体 key；a.Reason 是机器/人可读解释；a.ConfigFrom 是生效配置版本
```

## 分桶语义

1. **哈希**：`FNV-1a` 对长度前缀编码的 `(rebucket_salt, namespace, userID)` 求值，再对
   `Scale = 10000` 取模。长度前缀编码避免字符串拼接造成的键碰撞。
2. **作用域**：namespace 固定为 `"experiment:<experimentKey>"`（互斥层另有
   `"layer-slot:<layerKey>"`、`"layer-traffic:..."`、`"rollout:<version>:<uid>"`）。
   因此：
   - 新增 / 修改**其他**实验不影响本实验分桶；
   - 实验内用户的桶值与权重无关、与时间无关；
   - 同一用户在多个独立实验 / 独立层中相互独立。
3. **变体区间**：变体按**声明顺序**映射为半开累积区间 `[From, To)`，
   `end_i = floor(Scale * Σweight_0..i / Σweight)`，最后一个区间右端钉到 `Scale`
   （末位变体吸收取整误差，保证覆盖率精确）。权重是**相对权重**，不必加总为 100。
4. **占比调整的迁移语义（可解释）**：
   - 缩小某变体：其新区间是旧区间的前缀/子集，只有落在**被移动边界带**上的用户迁移；
     区间内部用户不动。
   - 扩大末位变体 / 在末尾追加变体：仅边界用户受影响，其他人不动。
   - 完全不需要迁移即可扩量时，老用户 0 漂移。
   - 确需**主动重分桶**（如实验方法学要求重新随机化）时，修改实验配置中的
     `rebucket_salt`（例如 `"rebucket-2026-q1"`）：哈希输入显式改变，分桶整体重排，
     这是一次在配置 diff 中可见、可审计的显式迁移，而不是隐式漂移。
5. 每次 `Assignment` 都带有 `Reason`（`assigned` / `targeting_mismatch: ...` /
   `experiment_off` / `layer_excluded` / `layer_slot_assigned_to_other_experiment` 等），
   `Bucket` 为用户在该实验的桶值，便于下游核对。

## 定向规则与兜底

- 规则为递归树：`{"op":"and","all":[...]}`、`{"op":"or","any":[...]}`、
  叶子 `{"where":{"attribute","operator","values"}}`；不写 `rules` 表示面向全量。
- 操作符：`eq / ne / in / not_in / gt / gte / lt / lte / exists / not_exists / prefix`。
- **永不报错、永不 panic**：属性缺失 → 不命中并返回 `... attribute missing`；
  类型不匹配（如对字符串做 `gt`）→ 不命中并返回类型说明；nil 属性 map 也安全。
- 数值支持 int 系列 / float；string/bool/number 跨类型比较判定为“不可比较”而不是 panic。
- **兜底结果**：
  - 未命中定向 / 实验关闭 / 被层流量排除 / 槽位属于同层其他实验时，
    `InExperiment=false`；若配置了 `fallback`（必须是已声明变体），则返回该变体，
    否则 `Variant=""`。调用方应把空字符串视为“不进实验”。

## 互斥层

```json
"layers": [{"key": "homepage", "traffic": 50, "experiments": ["promo", "banner"]}]
```

- 每个用户先按 `"layer-slot:<layerKey>"` 哈希到层内**等宽槽位**，槽位唯一对应一个成员实验；
  因此同一用户在同一层**最多命中一个**实验（得到 `layer_slot_assigned_to_other_experiment`）。
- `traffic`（0..100）限制该层的总进入比例，未进入者所有层内实验统一 `layer_excluded`。
- 不同层之间互不影响，用户可以在每层各命中一个实验；不属于任何层的实验彼此独立。

## 配置格式

完整 JSON 示例见 `cmd/demo/main.go`。顶层：

| 字段 | 说明 |
| --- | --- |
| `version` | 必填，非空版本号，灰度/回滚/审计均以它标识 |
| `experiments[]` | 至少一个；`key` 唯一 |
| `experiments[].enabled` | false 时统一返回 `experiment_off`（可配 fallback） |
| `experiments[].layer` | 所属层 key（需与 layers 成员一致） |
| `experiments[].variants[]` | `{key, weight}`，weight 相对权重、非负、总和 > 0 |
| `experiments[].fallback` | 不进实验时返回的变体（可选，须已声明） |
| `experiments[].rebucket_salt` | 显式重分桶盐，见“迁移语义” |
| `experiments[].rules` | 定向规则树 |
| `layers[]` | `{key, traffic(0..100), experiments[]}` |

`ParseConfig` / `Config.Validate()` 会检查：版本号、变体权重与重复 key、规则树结构、
层流量区间、层成员与实验的 layer 声明一致性、实验是否同属两层等。

## 热更新、灰度放量与回滚

### 热更新（全量切换）

```go
err := mgr.Update(newCfg) // 先整体编译+校验，再原子发布
```

- 内部状态是不可变 `managerState`，通过 `atomic.Pointer` 发布；`Evaluate` 读路径
  **不加锁**，任何并发请求读到的必然是某一份完整配置，不存在半更新。
- 新配置编译/校验失败时返回 error，**当前配置继续服务**（即切换失败自动回退）。
- Update 会保留上一份 stable，可用 `mgr.Rollback("<previousVersion>")` 原子恢复。
- 灰度进行中拒绝全量 Update（返回 `ErrRolloutInProgress`），避免两条发布线交叉。

### 灰度放量（版本化）

```go
mgr.StageRollout(v2)          // 注册候选版本，0% 生效
mgr.SetRollout("v2", 10)      // 10% 用户走 v2（再 50、100…）
mgr.Promote("v2")             // 放量结束：v2 成为唯一 stable
// 或
mgr.Rollback("v2")            // 放弃候选：全部用户立即回到 stable
```

- 放量门控：`hashPercent("rollout:<candidateVersion>:<userID>") < pct`，
  阈值 0..100，**按用户稳定**：同一用户在同一版本放量过程中要么在门内、要么门外。
- **单调加入**：提高 pct 只会把门控哈希值落在新增带内的用户切到新配置，
  已在门内的用户不会回退；降低 pct 即把最新的一批用户撤回旧配置（部分回滚）。
- 未生效用户继续读到旧 stable，行为完全不变。
- `Snapshot()` 返回 `{stable, candidate, previous, rollout_percent, history}`，
  `history` 是有界（最近 50 条）环形审计记录，动作含
  `initialize / activate / stage / rollout / promote / rollback-candidate / rollback-restore`，
  放量与回滚全程可追溯。

## 事件记录与采样

- `ManagerOptions.Sink` 实现 `Write([]AssignmentEvent) error`，批量接收（默认 256/批、
  100ms 定时刷写）。
- 采样率 `[0,1]`，按 `"assignment-sampling" + 实验|用户` 确定性判定：
  同一用户在同一实验要么始终记录、要么始终不记录，统计口径稳定。
- 队列有界，`Enqueue` **永远不阻塞分流主链路**：
  - `DropNewest`（默认）：满了直接丢新事件；
  - `DropOldest`：挤掉一条排队最久的事件放入新事件。
- sink 失败的批次计入丢弃数；`Close()` 会排空已接收事件。
- `EventStats()` 提供 `Enqueued / Written / Dropped / SampledOut / InFlight`，
  队列积压时按既定策略丢弃并计数，而不是拖垮分流。

## 并发与长期运行

- 读路径：一次 `atomic.Pointer.Load` + 纯内存哈希/查表，无 goroutine 分配热点。
- 写路径：互斥锁只串行配置变更，绝不被读路径持有。
- 内存：配置快照不可变且被整体替换；审计历史有上界；事件队列有固定容量。
- 分桶结果只依赖 `(salt, namespace, userID)`，不随时间、进程、机器漂移。
- 全部测试在 `-race` 下通过；`BenchmarkEvaluateDuringHotReload` 对比持续热更新下的读吞吐。

## 测试覆盖

`go test ./... -race` 覆盖：

- 分桶确定性、区间覆盖、权重分布（约 ±2%）；
- 占比调整的最小漂移（仅边界带迁移、增长变体无人迁出）、`rebucket_salt` 显式重排；
- 规则命中/不命中、缺失属性与类型异常的确定性兜底、and/or 组合；
- 互斥层（同层最多一个）、层流量比例、跨层独立；
- 热更新原子性、失败保留旧配置、Update 后回滚、灰度路由稳定/单调加入/提升/回滚、
  并发读写只可能读到完整版本、审计历史有界；
- 事件采样确定性、采样率、两种溢出策略不阻塞、关闭排空。
