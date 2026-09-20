# Changelog

## MetricsQL：函数元数据、optimizer 下推与 prettifier 回写的语义合流

### 修复

1. **optimizer 不再把 filter 下推进 `prometheus_buckets`**（`optimizer.go`）
   - `getTransformArgIdxForOptimization` 对 `prometheus_buckets` 返回 -1。
   - 该函数把输入的 `vmrange` 重写为输出的 `le`；从 binary op 另一侧推断出的
     `le=...`（或任意）filter 进入其输入选择器后匹配不到任何序列，导致结果
     变空。返回 -1 同时关闭公共 filter 提取与下推，是保守且与 `absent`/
     `scalar` 一致的处理。

2. **`default` 按序列级并集（交集）规则下推**（`optimizer.go`）
   - `getCommonLabelFilters` 新增 `case "default"`，与 `or` 一样对两侧公共
     filter 取交集并按 group modifier 裁剪。
   - 此前它落入算术运算的 union 分支，把 `foo{x="1"} default bar{x="2"}`
     改写成两侧都是 `{x="1",x="2"}`（同一 label 两个互斥值），结果恒为空。

### 实现选择

- 两个修复都只改“函数元数据 → 参数下标 / 集合运算规则”这一处决策点，不改
  AST 结构、解析、序列化或求值，影响面最小。
- 不为 `prometheus_buckets` 做 le 到 vmrange 的精确区间反解：区间反推会把
  等值 filter 变区间、正则 filter 基本无法反解，复杂度高且容易产生新的
  语义偏差；保守禁用下推保证正确性。
- `default` 直接复用 `or` 已验证的交集路径，并同样经过
  `TrimFiltersByGroupModifier`，因此 on/ignoring 行为一致。

### 原覆盖的空白

- 现有测试没有任何 `default` 用例（optimizer_test.go 全表无 default），
  该集合运算长期走错分支而无回归网。
- `prometheus_buckets` 没有 optimizer 下推用例；它虽然是 transform 函数，
  却具有“label 重命名”语义，默认参数下标 0 的假设对它不成立。
- 此前没有“优化前后求值一致性”的测试：所有 optimizer 断言都只比对改写文本，
  无法发现“文本合法但结果改变”的缺陷。
- 此前没有把 Optimize 与 Prettify 串起来的往返断言。

### 新增测试与反证

- `optimizer_test.go`
  - `TestPushdownBinaryOpFilters`：`prometheus_buckets` 的直接下推拒绝、
    `default` 的单侧下推契约。
  - `TestOptimize`：`prometheus_buckets`（含嵌套聚合）端到端不下推、`default`
    的侧特有/共有 filter、on(...) 与嵌套表达式的交集行为。
- `confluence/`（可执行反证，纯 label 集合语义参考模型）
  - `semantics.go`：selector/rollup/聚合/label manipulation/binary join 的
    序列集与 label 空间解释器。
  - `cases.go`：优化器保序 + prettifier 往返场景。
  - `confluence_test.go`：`TestOptimizePreservesSeriesSet`（对比 optimize
    前后求值结果）、`TestPrettifyParseRoundTrip`、
    `TestOptimizeRoundTripComposed`。
  - `cmd/metricsql-confluence`：`go run ./cmd/metricsql-confluence`，违例退出
    码 1。

### 最危险反例与对应回归用例

最危险的一类是“**输出 label 由函数重写，而 optimizer 仍把对输出 label 的
过滤推进输入**”：`prometheus_buckets` 的 vmrange 到 le 重写。它在直方图
查询里极其常见（prometheus_buckets(rate(vmrange 桶[5m])) / on(le) ...），
错误改写会静默把整侧结果清空，且改写后的文本仍然是合法 MetricsQL，普通
parser/prettifier 测试完全发现不了。

- 反例：`prometheus_buckets(foo_bucket) / on(le) bar_bucket{le="0.5"}`
  - 修复前：prometheus_buckets(foo_bucket{le="0.5"}) / on(le) bar_bucket{le="0.5"}（左值空）
  - 回归：单测 `TestOptimize` 的 prometheus_buckets 三条；可执行反证场景
    `prometheus_buckets-vmrange-to-le-join`（还原修复后报
    `UNSOUND: missing after rewrite: __name__=foo_bucket;le=0.5;`）。

次危险反例是把序列级并集 `default` 当算术运算交叉下推，可令任何带
`default` 的兜底查询静默返回空：

- 反例：`foo{x="1"} default bar{x="2"}`（修复前）两侧变成 `{x="1",x="2"}`。
- 回归：`TestOptimize` 的 default 五条；可执行反证场景
  `default-coalesce-keeps-right-only-series` 与
  `default-common-filter-still-pushed`。

### 相邻语义的退化保护

- `or` 分支代码未改动，既有全部 `or` 用例保持通过。
- and/if/ifnot/unless、group_left/group_right、fill 方向、聚合 by/without
  裁剪、label manipulation 与 count_values(_over_time) 的专门分支均未改动，
  相关既有测试全绿。
- 不新增错误路径；lexer/parser 诊断上下文与 regexp 缓存行为不变。
- prettifier/lexer 中已存在的转义与唯一 selector 回写修复经上游比对确认
- 已在本快照，新增往返测试将其锁定，防止回归。
