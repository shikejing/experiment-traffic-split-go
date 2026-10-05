# experiment-traffic-split-go

一个可复用的 Go 内部组件，用于把**用户稳定地分配到实验变体**、按属性**定向/分层**、管理**互斥实验**、支持配置**热更新与灰度放量**，并异步**记录分配事件**供下游统计流量占比。

- 纯 Go，无第三方依赖；
- 读路径无锁（原子快照），单次分配约 ~200ns / 1 次小分配；
- 全部决策是输入的纯函数：同用户同配置结果长期一致，不随时间、进程重启、GC 漂移；
- 内存有界：不可变快照 + 有界事件队列 + 可回收的历史版本/统计。

## 安装

```go
import split "github.com/shikejing/experiment-traffic-split-go"
```

## 快速开始

```go
cfg := &split.Config{
    Version: "2026-10-05",
    Experiments: []split.Experiment{{
        ID:             "checkout-redesign",
        TrafficPercent: 50,                       // 命中规则的用户中 50% 进入实验
        Variants: []split.Variant{
            {Name: "control",  Weight: 1},
            {Name: "redesign", Weight: 1},
        },
        Rules: []split.TargetingRule{{
            Name: "mobile",
            // OR-of-ANDs：任一内层分组全部成立即命中
            Conditions: [][]split.Condition{
                {{Attribute: "os", Operator: split.OpIn, Values: []string{"ios", "android"}}},
            },
        }},
    }},
}

s, err := split.New(cfg, split.Options{})
// ...
al, err := s.Allocate(ctx, "checkout-redesign", userID, split.Attributes{"os": "ios"})
switch {
case errors.Is(err, split.ErrExperimentGone):
    // 实验不存在
case al.InExperiment:
    // al.Variant 即稳定命中的变体
default:
    // 未命中：al.Reason 给出可解释原因（未定向/未进流量切片/实验停用）
}
```

## 分桶语义（稳定性契约）

分两层独立哈希，域前缀隔离，互不相关：

1. **流量门控桶（是否进实验）**
   - `gateBucket = FNV1a64("gate|<域>|<userID>") mod 100`
   - 独立实验域为 `exp:<experimentID>`；同一互斥组/分层内的实验**共享**组域（`group:<id>` / `layer:<name>`）。
   - 用户进入实验当且仅当 `sliceStart <= gateBucket < sliceEnd`（独立实验是 `[0, traffic_percent)`）。
2. **变体选择（进哪个变体）**：加权 Rendezvous Hashing（HRW）
   - 对每个变体 `i`：`score_i = u_i^(1/weight_i)`，`u_i` 为 `FNV1a64("var|<expID>|<salt>|<variantID>|<userID>")` 映射到 `(0,1)`；取 `score` 最大者。
   - 命中概率严格正比于权重：`P(i) = weight_i / Σweight`。
   - **关键性质：最小迁移**。调权重时，只有最高分易主的用户才会换组，未受影响的用户绝不动。把 50/50 调成 75/25 只有约 25% 用户迁移，而不是全员重新洗牌。
3. **哈希算法固化**：FNV-1a 常量在代码中显式实现并被视为语义契约，永不更换（更换等于让全体用户漂移）。用户 ID 放在哈希串末尾，以便 FNV 将其高熵充分混入所用的低位。

### 什么情况下用户会换桶？

| 变更 | 既有用户是否漂移 | 说明 |
|---|---|---|
| 调整变体权重 | 仅最小集合迁移 | 变大的变体只"拉入"用户；缩小的变体只释放必要用户，方向可解释 |
| 调整 `traffic_percent` | 放量为**单调前缀** | 调高只新增用户；调低会让桶位在新边界外的用户回到未进组（放量语义如此，见下） |
| 增/删变体 | 仅涉及该变体得分的用户 | HRW 天然最小变动 |
| 实验停用/下线 | 所有人安全兜底为"未进组" | 不报错 |
| **修改 `salt`** | **有意地全体重分桶** | 这是唯一被支持的"强制重洗牌"手段；必须在配置审计里写明业务原因（如实验重置） |

`Salt` 是显式的迁移开关：只有主动改 salt 才会重洗牌，权重类常规操作绝不无故漂移。

## 配置格式

`Config` 是一份自包含、整体版本化的快照（JSON tag 可直接用于配置下发）：

```jsonc
{
  "version": "2026-10-05-1",
  "experiments": [
    {
      "id": "exp_a",
      "layer": "checkout",            // 可选：隐式互斥层，同层流量切片之和 <= 100
      "salt": "",                     // 仅在需要强制重分桶时修改
      "disabled": false,
      "traffic_percent": 30,          // [0,100]
      "variants": [
        {"name": "control", "weight": 1},
        {"name": "treat_a", "weight": 1}
      ],
      "rules": [                      // 可选；为空表示全量用户
        {
          "name": "target-rule",
          "conditions": [             // OR：任一子数组成立即命中
            [                         // AND：子数组内条件全部成立
              {"attribute": "os", "operator": "in", "values": ["ios", "android"]},
              {"attribute": "age", "operator": "gte", "values": ["18"]}
            ]
          ]
        }
      ]
    }
  ],
  "groups": [                         // 可选：显式互斥组
    {"id": "checkout_group", "experiment_ids": ["exp_a", "exp_b"]}
  ]
}
```

校验规则（任何热更/灰度都会先校验，失败则旧配置继续服务）：

- `version` 非空；实验 ID 唯一；变体名唯一、权重非负且总权重大于 0；`traffic_percent ∈ [0,100]`。
- 互斥组成员必须存在、实验不得同时属于多个组；组（及隐式 `layer`）内各实验 `traffic_percent` 之和 ≤ 100。
- 规则操作符合法，正则可编译，`mod` 要求 `divisor>0 且 0<=remainder<divisor`。

支持的操作符：`eq, ne, in, not_in, prefix, suffix, contains, gt, gte, lt, lte, regex, exists, not_exists, mod`。

### 定向命中的确定性与兜底

- 命中判定是纯函数；属性缺失、取值不是数字、正则异常等**绝不报错**：对应判定不成立，用户确定性地落入"未定向"兜底，`Allocation.Reason` 给出原因（如 `not_targeted:rule:mobile:missing:os`）。
- 数值比较（`gt/gte/lt/lte`）要求两侧可解析为数字；任一不可解析则该判定不成立，而不是猜测或报错。

## 互斥组 / 实验分层

两种等价机制保证**同一用户在同一互斥域内最多命中一个实验**：

- 显式 `groups[].experiment_ids`；
- 隐式：给实验设置相同的非空 `layer`。

实现方式：成员实验按 ID 排序后被赋予 `[0,100)` 内互不相交的百分比切片，并共享同一个门控桶域。用户在该域内只有一个门控桶，因此最多落入一个切片。实验自身的定向规则仍然各自生效（先过定向，再看切片）。

## 热更新、回滚与并发一致性

```go
// 直接整体热替换（先编译校验，再原子指针切换）
err := s.HotReplace(newCfg, "adjust treat weight 1->2")

// 或者：注册灰度版本 -> 分比例放量 -> 全量 / 回滚
_ = s.PutConfig(v2)
_ = s.Ramp("v2", 10, "canary 10%")
_ = s.Ramp("v2", 50, "ramp to 50%")
_ = s.Promote("v2", "full rollout")   // 或 s.RollbackVersion("v2", "regression found")
```

- **一致性**：活动配置存在一个 `atomic.Pointer` 指向的不可变快照中。读取只做一次原子加载，看到的要么是旧配置、要么是新配置的**完整**状态，不存在半更新；切换本身只是一次原子指针写，热更期间吞吐不下降（有并发基准/测试守护）。
- **失败回退**：`Replace/TryReplace/Promote` 先在不影响流量的情况下完成编译校验；`TryReplace` 还支持切换后钩子，钩子失败会原子回退指针，旧配置继续服务。
- 写入（替换/放量/回滚）串行化；读取与写入可并发。

## 灰度放量与可追溯

- 放量桶：`rolloutBucket = FNV1a64("rollout|<version>|<userID>") mod 100`，与实验门控相互独立。
- 版本 `V` 放量到 `p%` 时：`rolloutBucket < p` 的用户读新版本，其余用户继续读登记的旧版本（fallback）。放量门对用户是稳定的；且**放量集合是单调前缀**——把 p 调高只让更多用户进入新版本，已在新版本上的用户不会在放量途中掉回旧版。
- `RollbackVersion` 立即把该版本置为 0%，全量用户确定性地回到旧版本；若该版本此前已 `Promote` 到 100%，活动快照也会被原子切回旧版本。
- `RolloutAudit()` 返回 `set_percent / activate / rollback` 的完整事件流（时间、版本、比例、原因、回退目标），用于追溯。
- 历史版本可通过 `ForgetVersion` 显式回收，统计计数可用 `RatioStats.ForgetVersion` 回收，避免长跑进程内存随更新次数增长。

## 分配事件、采样与下游统计

事件采集与分流主链路解耦：

```go
rec, _ := split.NewEventRecorder(sink, split.RecorderOptions{
    QueueSize:     4096,               // 有界队列，默认 4096
    BatchSize:     256,
    FlushInterval: time.Second,
    Workers:       2,
    Policy:        split.DropNewest,   // 积压时默认丢弃最新并计数，绝不拖垮分流
    Sampler:       split.NewRatioSampler(0.1), // 稳定地按用户采 10%
})
s, _ := split.New(cfg, split.Options{Recorder: rec})
```

- `Record` 只有一次采样判定 + 一次**非阻塞** channel 发送：无互斥锁、无 IO，永远不会因为下游故障而拖慢或失败分流。
- 队列打满按 `DropNewest`（默认，计入 `Dropped()`）或 `BlockNewest`（显式反压）处理；sink 失败有界重试后计入丢弃，不会无限堆积。
- 采样器按 `(experiment,user)` 做确定性哈希采样：同一用户始终被采或不被采，用户级指标稳定。
- `Flush`/`Close` 提供优雅停机；`RatioStats` 可直接消费事件统计各变体观测占比（见测试 `TestVariantRatiosObserved`）。

## API 速览

| 类型/方法 | 用途 |
|---|---|
| `New(cfg, opts)` | 以已校验的初始配置构建组件 |
| `(*Splitter).Allocate(ctx, expID, userID, attrs)` | 分配；属性异常不报错，返回可解释 `Allocation` |
| `PutConfig / Ramp / Promote / RollbackVersion` | 灰度注册/放量/全量/回滚 |
| `HotReplace(cfg, reason)` | 直接原子热替换（失败自动保留旧版） |
| `CurrentVersion / GrayState / RolloutAudit` | 版本与放量状态、审计 |
| `ForgetVersion` | 回收历史版本内存 |
| `NewEventRecorder / Record / Flush / Close / Dropped` | 异步事件、采样、积压统计 |
| `NewRatioStats / Observe / Snapshot / ForgetVersion` | 变体流量占比统计 |
| `Config.Validate` | 独立复用的配置校验 |

哨兵错误：`ErrConfigInvalid`、`ErrExperimentGone`、`ErrGroupInvalid`、`ErrVersionMissing`、`ErrPercentInvalid`、`ErrRecorderClosed`。

## 测试覆盖

`go test -race ./...` 覆盖：

1. 分桶长期稳定（重复调用、重建实例、跨 GC 周期不漂移）；权重调整最小迁移与迁移方向；salt 变更触发可解释重分桶；变体观测占比逼近配置权重。
2. 规则命中、属性缺失/非法数值兜底、实验停用、未知实验错误。
3. 互斥组与分层的排他性、流量切片比例、超量配置被拒绝。
4. 热更新原子性（并发读者只见完整配置）、非法配置回退、切换钩子失败回退。
5. 灰度放量比例、放量单调性、放量期确定性、回滚全量回到旧版、审计可追溯。
6. 事件投递、采样稳定性、队列积压丢弃、sink 失败重试后丢弃、关闭后安全调用。
7. 200 次热更后的堆内存有界性、热更期间吞吐不塌。

## 设计边界

- 同一时刻只跟踪一个灰度候选版本（新老两份配置同时服务）；多候选串行为多次放量周期。
- 组件不负责持久化配置；下发来源（文件/配置中心）在组件外，落地为 `Config` 后交给本组件。
- 放量/回滚审计为进程内内存态；如需跨进程持久审计，可通过包装 `Store`/事件流外发到审计中心。
