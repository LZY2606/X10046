# CHANGELOG

## 2026-09-21 — MetricsQL 函数元数据 / optimizer 下推 / prettifier 回写语义合流

### 背景与基线

- 工作树与上游 `github.com/VictoriaMetrics/metricsql`（2026-09-04 快照）
  的生产源码逐文件一致；既有测试 `go test ./... -count=1` 全绿。
- 既有覆盖的形状是“**字符串快照**”：`optimizer_test.go` 断言
  `PushdownBinaryOpFilters` / `getCommonLabelFilters` 的输出文本，
  `prettifier_test.go` 断言 prettify 文本。它能锁定当前行为，但存在三处
  空白（见下）。本次**不改动任何生产代码**，新增的是把这三处空白缝合起来
  的可执行反证与文档。

### 交付物

- `semantics_eval_test.go`：固定合成数据集上的独立标签/值求值 oracle
  （选择器、嵌套 rollup、binary 1:1 与 group_left/right、集合操作、
  bool、fill、聚合、label manipulation、count_values）。
- `confluence_test.go`：五个可独立定位的顶层测试：
  - `TestConfluenceOptimizeSound`（14 条组合查询；优化前后求值相等 + 幂等）
  - `TestConfluenceNaivePushdownCounterExample`（5 条屏障反例）
  - `TestConfluencePrettifyRoundTrip`（7 条强制多行的往返查询 + modifier 签名比对）
  - `TestConfluencePushdownModifierTrim`（独立钉住下推侧的 on/ignoring 二次裁剪）
  - `TestConfluenceErrorDiagnostics`
  - `TestConfluenceRegexpCache`
- `ANALYSIS.md`：入口/状态/缓存/错误/输出的链路说明、改写前后 AST 与
  逐条合法性条件、求值器口径、最危险反例。

### 实现选择（为什么这样而不是那样）

- **不引入数值引擎，只做标签传播 oracle**。optimizer 的正确性声明是
  “下推不改变序列多重集”，非法下推的可观测后果是序列消失/签名改变，
  在单采样点上即可暴露；数值引擎只会让 oracle 变成第二个被测对象。
- **求值器镜像而非复用 optimizer 的参数索引**
  （`getTransformArgIdxForOracle` 与 rollup 的 `GetRollupArgIdx` 同源）。
  这样函数元数据表一旦与求值假设漂移，测试会以求值差异失败，而不是因为
  两边共用同一份错误逻辑而双双通过。
- **反证用“必须失败”的朴素重写器**而不是变异测试框架：无随机 sleep、
  无外部进程；每个屏障对应一条确定性查询，失败信息直接给出改写后的表达式。
- **prettify 往返用规范化串逐字相等 + modifier 签名 + 求值相等的三重断言**，
  并要求输出必须含换行，防止查询过短导致多行回写分支未被覆盖。
- 数据集在 `newSEUniverse()` 中确定性生成；没有针对 fixture 名称的特判，
  也没有任何本机绝对路径或真实外网访问。

### 原覆盖的空白

1. optimizer 测试只比对文本，没有任何地方证明“下推后求值不变”：
   一条合法但语义错误的下推只要文本符合预期就会通过。
2. 没有“负空间”证明：哪些过滤器**不能**推（无修饰词聚合、被改写/删除的
   标签、group 修饰词裁剪、fill 方向）从未被求值级证据约束。
3. prettifier 测试未在同一用例上同时验证“多行展开 + modifier 保留 +
   再解析 + label matching 不变 + keep_metric_names 保留”。

### 相邻语义的退化保护

- group modifier 的两道裁剪（收集侧 + 下推侧）分别由健全性用例与
  `TestConfluencePushdownModifierTrim` 独立覆盖；变异验证确认移除任意一道
  都会被对应测试捕获。
- 幂等断言（`Optimize(Optimize(x)) == Optimize(x)`）防止重复下推/重复排序回归。
- 原始入参不可变由既有 `PushdownBinaryOpFilters` 测试与
  `TestConfluenceOptimizeSound`（优化后再 eval 原表达式）共同约束。
- `__name__` 不参与跨操作符下推由求值器在 keep_metric_names 两条用例上
  显式区分（keep 时名称保留、不 keep 时剥离）。
- fill 方向、`or/unless/ifnot` 的非对称过滤器流各自有查询覆盖。
- 错误测试锁定诊断上下文（`unparsed data` / `unexpected token`），
  防止错误退化为不透明字符串；regexp 缓存测试锁定“错误也被缓存”。
- 全套既有测试保持原样通过，新增文件均为 `_test.go`，不导出任何新 API。

### 最危险反例与对应回归（单列）

> `sum(rate(http_requests_total{zone="z1"}[5m])) by (instance) * on(instance) group_left() sum(rate(errors_total{instance="h1",zone="z2"}[5m])) by (instance)`

嵌套 rollup + 聚合屏障（zone 在 join 前已坍缩）+ `on(instance) group_left()`
签名裁剪三者叠加；朴素下推会静默删掉 h1 序列而不报错。回归用例：
`TestConfluenceNaivePushdownCounterExample` 的 by(instance) 子用例
（生产优化器等价；非法重写被求值差异捕获）。

回写侧等危反例：长 `rate(...[5m]) keep_metric_names` 走 FuncExpr 多行分支
时漏接 `appendModifiers`，后缀消失但文本仍可解析。回归用例：
`TestConfluencePrettifyRoundTrip` 中函数级 keep_metric_names 查询。
