# MetricsQL：函数元数据、optimizer 下推与 prettifier 回写的语义合流

本文档追踪一条具体链路，并给出每一步的 AST 改写、合法（legality）条件与
可执行反证：

```
查询文本
  └─ Parse（lexer + parser + WITH 展开 + parens 消解 + 常量化简）
       └─ 表达式树（Expr: MetricExpr / RollupExpr / FuncExpr / AggrFuncExpr / BinaryOpExpr …）
            └─ Optimize（函数元数据 → 共同标签过滤器收集 → 屏障裁剪 → 下推）
                 └─ 输出文本：AppendString（规范化）与 Prettify（多行回写）
                      └─ 再 Parse：必须得到同一棵语义树
```

“语义合流”的含义：同一查询经过 `Parse → Optimize → 输出 → Parse` 的任意
路径后，求值得到的 **（标签集合, 值）多重集** 不变；尤其 `keep_metric_names`
不得丢失，binary op 的 group/join/bool/fill modifier 不得改变 label matching。

可执行验证全部在包内测试中，无需外网、无随机 sleep、无绝对路径、无针对
fixture 名称的特判：

- `TestConfluenceOptimizeSound` — 优化前后微型求值结果逐序列相等 + 优化幂等。
- `TestConfluenceNaivePushdownCounterExample` — 故意非法的“朴素下推”必须被反证捕获。
- `TestConfluencePrettifyRoundTrip` — prettify 文本必须可再解析且 modifier/求值不变。
- `TestConfluenceErrorDiagnostics` — 错误携带可诊断上下文；非法 modifier 位置必须失败。
- `TestConfluenceRegexpCache` — 正则缓存对成功/失败结果都稳定。

微型标签/值求值器见 `semantics_eval_test.go`；它**不是** MetricsQL 的实现，
只是一个固定合成数据集上的独立 oracle（见文末“求值器口径”）。

## 1. 入口、状态、缓存、错误传播、输出

### 1.1 入口（`parser.go`）

- `Parse(s) (Expr, error)`（parser.go:14）：完整管线。
  1. `parseInternal` 驱动 `lexer` 产生首个 token 并递归下降；
  2. `expandWithExpr` 展开内置/显式 WITH（`ru`、`ttf`、`range_median`、`alias`）；
  3. `removeParensExpr` 消解仅包一个表达式的括号（但保留 `(binary) keep_metric_names` 所需结构）；
  4. `simplifyConstants` 折叠常量；
  5. `checkSupportedFunctions` 用三类函数元数据（rollup/transform/aggr）做合法性校验。
- `Prettify(q)`（prettifier.go:4）走 `parseInternal`（**不**做 WITH 展开与化简，
  保留用户书写的结构），再 `removeParensExpr` + 多行排版。
- `Optimize(e)`（optimizer.go:16）与 `PushdownBinaryOpFilters(e, lfs)`（optimizer.go:369）
  是仅针对 AST 的转换；二者都先 `Clone(e)`，不改动入参。
- `Clone`（optimizer.go:46）= `AppendString` 后再 `Parse`。这意味着“能被
  AppendString 无损序列化”本身就是 optimizer 的隐式前提，也是 prettifier
  回写必须守住的性质。

### 1.2 状态与缓存

- parser/optimizer/prettifier 对单次调用是**无状态**的：`parser` 在
  `parseInternal` 内局部构造；`Optimize` 只写 `Clone` 出来的新树。
- 唯一的进程级可变状态是正则缓存 `regexpCacheV`（regexp_cache.go:35）：
  - key 为正则源串，value 同时缓存 `*regexp.Regexp` 与编译 `error`；
  - `requests/misses` 用原子计数，map 受 `sync.RWMutex` 保护；
  - 超过 `regexpCacheCharsMax` 时按 ~10% 字符量随机序淘汰（map 迭代序），
    淘汰只影响性能不影响正确性；
  - 选择器的正则匹配（`=~`/`!~`）与 `label_replace` 都经由 `CompileRegexpAnchored`，
    因此编译错误会被复用而不是每次重复构造。
- WITH 默认表达式表 `defaultWithWithArgExprs` 用 `sync.Once` 惰性构建。

### 1.3 错误传播

- 词法/语法错误统一回到 `parseInternal`：子错误包裹一个带剩余上下文的
  `…; unparsed data: %q`（`lexer.Context()` 给出当前 token + 剩余串），
  因此每条错误都能定位到输入片段，而不是不透明字符串。
- WITH 展开失败被包装为 `cannot expand WITH expressions: …`；
  未知函数为 `unsupported function %q`（utils.go `checkSupportedFunctions`）。
- optimizer 内部的“不可达分支”用 `panic("BUG: …")` 表达元数据不一致
  （例如 `count_values` 必须已被特判）。这类 panic 只在函数元数据表与
  switch 分支失同步时触发，属于**内部不变量**而非用户错误；
  `TestConfluenceErrorDiagnostics` 保证用户侧错误只走 error 返回。

### 1.4 输出

- `Expr.AppendString` 是规范化序列化，`Clone`、`ExpandWithExprs`、optimizer
  结果比较都依赖它。
- `Prettify` 在单行不超过 `maxPrettifiedLineLen=80` 时直接输出规范化文本；
  否则按节点类型多行展开（prettifier.go:35 起）。多行分支必须逐字回写所有
  modifier——这是 `TestConfluencePrettifyRoundTrip` 的检查点。

## 2. 函数元数据如何决定下推：入口表与参数索引

optimizer 不做名字硬编码的求值语义，而是依赖三张函数类别表和“承载序列的
参数索引”：

- 类别表：`rollupFuncs`（rollup.go:8）、`transformFuncs`（transform.go:5）、
  `aggrFuncs`（aggr.go:7）。公开判定器 `IsRollupFunc/IsTransformFunc/IsAggrFunc`。
- 参数索引：
  - rollup：`getRollupArgIdxForOptimization`（optimizer.go:692）必须与
    公开 API `GetRollupArgIdx`（rollup.go:94）保持同步；默认 arg0，
    `quantile_over_time`/`aggr_over_time`/`hoeffding_bound_*` 为 arg1，
    `quantiles_over_time` 为最后一个参数，`absent_over_time` 为 -1（无标签可推）。
  - transform：`getTransformArgIdxForOptimization`（optimizer.go:709）；
    `histogram_quantile`/`range_quantile` 等为 arg1，
    `limit_offset`/`histogram_fraction` 为 arg2，
    `scalar`/`absent`/`drop_common_labels`/时间常量函数为 -1。
  - aggr：`getAggrArgIdxForOptimization`（optimizer.go:660）；
    `topk*`/`bottomk*`/`quantile` 等为 arg1，`quantiles` 为最后参数。
- 少数函数因会**改写标签集合**而被单独建模（收集与下推成对出现）：
  `label_set`、`label_replace/label_join/label_map/label_match/label_mismatch/
  label_transform`、`label_copy/label_move`、`label_del/label_uppercase/
  label_lowercase/labels_equal`、`label_keep`、`count_values(_over_time)`，
  以及跨参数求交/并集的 `range_normalize`/`union`/空名函数（union 同义词）。

合流约束：**收集侧 `getCommonLabelFilters*` 与下推侧
`pushdownLabelFilters*` 必须成对**。新增函数时若只改一处，会出现
“收集到了但推错位置”或“推下去了但收集没排除”的语义裂缝。微型求值器的
`getTransformArgIdxForOracle` 刻意镜像参数索引表，使元数据漂移会直接表现为
求值不一致而非静默通过。

## 3. 改写前后 AST 与每一条合法性条件

下例均可由仓库当前代码复现（`Optimize(Parse(q))`）。

### 3.1 嵌套 rollup × 双聚合：分组修饰词是屏障

```
before:
  sum(rate(http_requests_total[5m])) by(instance,zone)
    + sum(rate(errors_total{job="api",zone="z1"}[5m])) by(instance,zone)
共同过滤器（右侧选择器，去掉 __name__）: {job="api", zone="z1"}
after:
  sum(rate(http_requests_total{zone="z1"}[5m])) by(instance,zone)
    + sum(rate(errors_total{job="api",zone="z1"}[5m])) by(instance,zone)
```

逐条件：

1. rollup `rate` 的序列参数是 arg0（rollup 元数据），过滤器可穿过
   `RollupExpr` 直达内层 `MetricExpr`。
2. 两个 `sum … by(instance,zone)` 只**保留分组键标签**；
   `trimFiltersByAggrModifier` 把共同过滤器裁剪到 `{zone="z1"}`，
   `job="api"` 被丢弃——聚合后 job 已不存在，推入任何一侧都会改变结果集。
3. 默认 1:1 binary op 的共同过滤器是两侧标签的并集，再按
   `GroupModifier`（此处无 on/ignoring，原样保留）裁剪。

### 3.2 binary join：`group_left()` 与 `on()` 的联合裁剪

```
http_requests_total * on(instance) group_left()
  errors_total{job="api",zone="z1"}
after: （无新增过滤器）
```

- `on(instance)` 把匹配签名限定为 `instance`；
- `group_left()` 分支只保留**右侧**经过 `TrimFiltersByGroupModifier`
  裁剪后的过滤器：`job/zone` 不在 `on()` 列表里 → 全部剔除，
  因此右侧过滤器不能推入左侧（左侧没有匹配签名之外的相等保证）。
- 对照：若把共同过滤器换成 `{instance="h1"}`，它在 `on()` 列表中，
  就能推入两侧。`TestConfluenceOptimizeSound` 的 group_left 用例固化该行为，
  `counterExampleQueries` 的 container 用例证明朴素下推会把右侧聚合清空。

### 3.3 label manipulation：写入/删除的标签不是前置条件

```
before: label_set(http_requests_total, "x", "y") + errors_total{x="y",job="api"}
after : label_set(http_requests_total{job="api"}, "x", "y")
          + errors_total{job="api",x="y"}
```

- `label_set` 在求值**之后**写入 `x="y"`，所以共同过滤器里的 `x="y"` 是
  函数的**输出**，绝不能作为内层选择器的输入约束（收集侧特判丢弃该标签，
  下推侧同样丢弃）；
- `job="api"` 不被任何标签函数触碰，可以安全推入 arg0。
- `label_replace`（目标标签丢弃）、`label_del`/`label_uppercase`/
  `label_lowercase`（删除/改写标签丢弃）、`label_copy`/`label_move`
  （目标标签丢弃）、`label_keep`（只保留列举标签）都遵循同一条原则：
  **过滤器只在标签语义保持的路径上流动**。

### 3.4 `keep_metric_names`：名称保留与下推正交但会被回写校验

```
(http_requests_total{job="api"} + errors_total{job="api"}) keep_metric_names
```

- AST 上体现为 `BinaryOpExpr.KeepMetricNames`（parser.go:1965）或
  `FuncExpr.KeepMetricNames`（parser.go:2144）；求值语义是**不要剥离
  `__name__` 标签**。
- optimizer 收集共同过滤器时经 `getCommonLabelFiltersWithoutMetricName`
  始终剔除 `__name__`：度量名永远不参与跨操作符下推（不同度量名之间无
  相等保证），与 keep 标志正交——keep 只影响输出标签，不改变过滤器流。
- 序列化侧：`AppendString` 对带标志的 binary op 外包一层
  `( … ) keep_metric_names`（parser.go:1982），并在右侧需要时由
  `needRightParens` 给带标志的 FuncExpr 补括号（parser.go:2019）。
  这是回写正确性最脆弱的点之一：丢括号或丢后缀都会让再解析落到另一棵树。

### 3.5 fill 修饰词的单向传播

- `fill_left()`：只允许 **right → left**；`fill_right()`：只允许
  **left → right**；`fill()`（两侧都填）：**不做**跨侧下推
  （optimizer.go:69 起的三个 case）。因为填充会引入原本不存在的序列，
  只有在“过滤器一定对被填充侧成立”时才安全，方向错了就会把填充值放到
  本不该匹配的标签组合上。
- `TestConfluenceOptimizeSound` 的 `+ fill_left(0)` 用例固化单向语义。

### 3.6 集合操作

- `or`：两侧共同过滤器取**交集**（两侧都独立成立的标签才算数），再按
  group modifier 裁剪。
- `unless` / `ifnot`：只保留**左侧**过滤器（右侧只用于判定存在性）。
- `if`/`and`：沿默认并集 + modifier 裁剪路径。

### 3.7 两道裁剪：收集侧与下推侧的纵深防御

group modifier 的裁剪实际发生在**两个**位置：收集侧
`getCommonLabelFilters` 的各 join/set 分支，以及下推侧
`pushdownBinaryOpFiltersInplace` 遇到嵌套 `*BinaryOpExpr` 时再次执行的
`TrimFiltersByGroupModifier`（optimizer.go:432）。因此即使只移除收集侧
裁剪，下推侧仍会在过滤器进入子树前把非签名标签剔除；反之亦然。
`TestConfluencePushdownModifierTrim` 直接向下推 API 传入一个非签名标签
过滤器（`route="/v1"` 与 `on(instance)`），独立钉住下推侧这道屏障，
同时确认签名标签 `instance` 可以正常下推。

## 4. prettifier 回写：哪些 token 必须出现

多行分支（prettifier.go:35 起）不是简单缩进版的 `AppendString`，而是手工
重建结构，因此每个分支都必须显式回写：

- `BinaryOpExpr`：左/右操作数（各自按需括号）、中间单独成行的
  `appendModifiers`（op、`bool`、`on/ignoring(...)`、
  `group_left/group_right(...)` 及可选 `prefix "…"`、
  `fill(_left/_right)(…)`），以及带标志时外层的 `(` 与
  `) keep_metric_names`。
- `AggrFuncExpr` / `FuncExpr`：多行参数列表后必须重新接上
  `t.appendModifiers(dst)`，否则 `by/without(…) limit N` 与函数级
  `keep_metric_names` 会在多行路径上静默消失。
- `RollupExpr`：`appendModifiers` 必须保留 `[window:step] offset … @ …`。

`TestConfluencePrettifyRoundTrip` 用超过 80 列的查询强制走多行分支，断言：

1. prettify 输出确实多行（避免测试空走单行路径）；
2. 输出可再 `Parse`；
3. 再解析结果的规范化 `AppendString` 与原查询完全一致；
4. 逐节点抽取的 binary modifier 签名（op/bool/group/join/prefix/fill/keep）、
   函数 keep 标志、聚合 modifier 多重集相等；
5. 经第 1 节的求值 oracle，prettify 前后（标签, 值）结果一致。

## 5. 可执行反证的结构（`confluence_test.go` / `semantics_eval_test.go`）

### 5.1 健全性（应保持等价）

`TestConfluenceOptimizeSound` 对 `confluenceQueries` 中每条查询：

```
want := eval(Parse(q))
got  := eval(Optimize(Parse(q)))
assert 排序后的 (标签键, 值) 多重集完全相等
assert Optimize(Optimize(x)) == Optimize(x)   // 幂等
```

查询集同时含：嵌套 rollup（`rate(...[5m])` 外套 `sum by` 与 binary op）、
binary join（1:1、`group_left`、`unless on(...)`）、`keep_metric_names`
（binary 与 rollup func 两种位置）、label manipulation（set/replace/del/keep）、
`count_values_over_time`、`fill_left`。

### 5.2 反证（非法改写必须被发现）

`TestConfluenceNaivePushdownCounterExample` 内置一个**故意无视所有合法性
条件**的改写器：它不经函数元数据、不看 by/on/group_left、不识别标签
增删，就把共同过滤器推进每个叶子选择器。对五条带屏障的查询，要求：

- 生产 `Optimize` 仍然等价（证明不是测试数据本身有问题）；
- 朴素改写的求值结果**必须不同**（证明屏障条件携带真实语义，删了就会出错）。

五个屏障分别是：无修饰词 `sum`（标签全部坍缩）、`label_set` 输出标签、
`by(instance)` 对 zone 的坍缩、`label_del` 删除标签、`group_left` +
`on(instance)` 对非签名标签的裁剪。若未来有人把 optimizer 改成朴素实现，
这些用例会立刻失败；若有人把 oracle 调成“什么都相等”，反证会立刻失败。

### 5.3 错误与缓存

- `TestConfluenceErrorDiagnostics`：残缺括号/过滤器等必须返回带上下文的
  error；错位 modifier（`sum(x) by (a) keep_metric_names`、以 `on(...)`
  开头）必须失败。
- `TestConfluenceRegexpCache`：同一正则两次编译返回同一实例；非法正则
  两次返回**相同**的错误文本（错误也被缓存）。

### 5.4 求值器口径（为什么它足以做反证）

oracle 只在一个固定、小规模的标签宇宙上运行（3 个度量名 × job/instance/
zone/route/pod/container 的有限取值），每条序列一个采样点。它实现的是
**标签传播 + 连接签名 + keep_metric_names** 的最小语义，而不是数值精度：

- 选择器按 `LabelFilter`（含 `=~`/`!~`，缺失标签按空串）过滤；
- binary op 实现 1:1、`group_left/group_right`、`or/and/unless/if/ifnot/
  default`、`bool`、`fill_left/fill_right/fill`，签名由 on/ignoring 计算，
  `__name__` 默认剥离、keep 时保留；
- 聚合实现 by/without/无修饰词分组；rollup 在单采样点上是标签恒等
  （count_values(_over_time) 单独实现目标标签合成）；
- label manipulation 按真实语义改写标签 map。

因为 optimizer 的正确性声明是“过滤器下推不改变每个时间点的序列多重集”，
标签集合 + 一个代表性的值已足以区分：非法下推只会表现为**序列消失/签名
改变**，与时间维度无关。数值聚合实现刻意保持简单，避免 oracle 自身成为
被测对象。

## 6. 最危险的反例（AST/优化器/label 语义交汇处）

最危险的反例不是“过滤器推错了一个叶子”，而是**三个机制叠加后错误互相伪装**：

```
sum(rate(http_requests_total{zone="z1"}[5m])) by (instance)
  * on(instance) group_left()
sum(rate(errors_total{instance="h1",zone="z2"}[5m])) by (instance)
```

这一条同时踩中：

1. **嵌套 rollup**：过滤器要穿过 `RollupExpr` 与 `rate`（arg0 元数据）才能到达选择器；
2. **聚合屏障**：`by(instance)` 在求值时已经丢弃 `zone`，所以 `zone="z2"`
   不是左侧聚合输出的性质——朴素下推把它塞进左侧选择器会静默删掉 h1/z1；
3. **join 裁剪**：`on(instance) group_left()` 的匹配签名只有 `instance`，
   右侧的 `zone="z2"` 既不能进入左侧，也不能在签名之外被假定相等。

错误后果是双重静默：数值结果不会报错、不会 NaN，只是**少了本应存在的
h1 序列**——这正是生产环境最难排查的一类（“为什么这个实例的比率没了”）。
对应回归用例是
`TestConfluenceNaivePushdownCounterExample/sumhttp_requests_totalzoneeqz1_by_instance_mul_oninstance_gr`
（生产优化器保持等价；朴素下推被求值差异捕获）。

另一个等危的回写反例：`rate(…长过滤器…[5m]) keep_metric_names / …`
超过 80 列走 FuncExpr 多行分支时，若分支末尾漏接 `appendModifiers`，
`keep_metric_names` 会消失且**文本仍可再解析**——不报错，只是名称标签被
剥离。对应回归：`TestConfluencePrettifyRoundTrip` 中第二条 keep_metric_names
查询的“必须多行 + 规范串逐字相等 + 求值相等”三重断言。

## 7. 运行方式

```sh
go mod download
go test ./... -count=1                       # 全套
go test -run 'TestConfluence' -count=1 -v .  # 仅合流反证（每个子测试可单独定位）
```

新增测试不依赖网络与外部进程；数据集在 `newSEUniverse()` 内确定性构造。
