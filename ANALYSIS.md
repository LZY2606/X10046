# MetricsQL：函数元数据、optimizer 下推与 prettifier 回写的语义合流

本文档串起从查询入口到文本回写的完整链路：解析入口、优化状态、正则缓存、错误
传播和最终输出，并给出每个改写结论对应的 AST 与合法性条件。

## 1. 链路总览：入口 → 状态 → 缓存 → 错误 → 输出

```
查询字符串
  │ Parse(s)                         parser.go
  ├─ lexer 词法扫描                  lexer.go（标识符转义、字符串、duration）
  ├─ parseExpr 递归下降              parser.go
  │    ├─ MetricExpr（含 LabelFilterss，or-组选择器）
  │    ├─ RollupExpr（[window:step] offset @）
  │    ├─ FuncExpr / AggrFuncExpr（Name + Args + Modifier）
  │    └─ BinaryOpExpr（Op、GroupModifier、JoinModifier、
  │                    JoinModifierPrefix、FillLeft/FillRight、
  │                    KeepMetricNames）
  ├─ expandWithExpr / removeParensExpr / simplifyConstants
  ├─ checkSupportedFunctions         函数元数据：aggr.go / rollup.go / transform.go
  └─ *Expr AST
       │
       ├─ Optimize(e)                optimizer.go
       │    ├─ canOptimize           是否存在可下推的 BinaryOpExpr
       │    ├─ Clone(e)              用 AppendString+Parse 深拷贝，不改原 AST
       │    ├─ getCommonLabelFilters 按“函数元数据决定取哪个参数”收集公共 filter
       │    └─ pushdownBinaryOpFiltersInplace 按同一元数据决定往哪个参数下推
       │
       └─ Prettify(q) / AppendString prettifier.go + lexer.go
            └─ 输出必须可被 Parse 再次解析为等价 AST（往返）
```

- 入口：`Parse`（`parser.go`）、`Optimize`（`optimizer.go`）、
  `Prettify`（`prettifier.go`）、`PushdownBinaryOpFilters`（`optimizer.go`）。
- 状态：optimizer 从不在原 AST 上改写。`Optimize` 先 `Clone`（内部用
  `AppendString` 序列化再 `Parse`），全部修改发生在拷贝上；调用方持有的 AST
  保持不变，测试 `TestOptimize`/`TestPushdownBinaryOpFilters` 显式断言这一点。
- 缓存：选择器中的正则 filter 经 `CompileRegexp`/`CompileRegexpAnchored`
  （`regexp_cache.go`）进入带字符上限（`regexpCacheCharsMax`）的 LRU 式缓存；
  缓存键是正则文本，值同时缓存编译结果和错误（非法正则不会重复编译）。
  optimizer 只新增/搬运 `LabelFilter`，不重新编译正则，因此缓存语义不受影响。
- 错误传播：lexer 错误经 `parseInternal` 包成
  `...; unparsed data: "<上下文>"`；`Clone` 在“自身序列化结果无法被解析”时
  panic 并标注 `BUG:`（理论不可达，因为输出由 AST 自身生成）。本次修复不新增
  错误路径，保持所有诊断信息原样上抛。
- 输出：`AppendString` 是 AST 的权威文本形式；`Prettify` 在超过
  `maxPrettifiedLineLen` 时按节点类型多行排版，但必须产出仍可解析、语义不变
  的文本。

## 2. 函数元数据如何决定 optimizer 的下推与名称保留

optimizer 的两条核心规则都由“函数类别 + 参数下标”驱动：

1. `getCommonLabelFilters` 决定某个函数调用**对外暴露**哪些公共 filter；
2. `pushdownBinaryOpFiltersInplace` 决定这些 filter 能**进入哪个参数**。

两者共用 `getFuncArgIdxForOptimization`，它先查三张元数据表：

- `IsRollupFunc`（`rollup.go`）→ `getRollupArgIdxForOptimization`；
- `IsTransformFunc`（`transform.go`）→ `getTransformArgIdxForOptimization`；
- `IsAggrFunc`（`aggr.go`）→ `getAggrArgIdxForOptimization`。

返回 `-1` 表示该函数“既不贡献公共 filter、也不接受下推”（如 `absent`、
`scalar`、`drop_common_labels`）。label manipulation 函数
（`label_set/label_replace/label_del/label_keep/...`）与
`count_values(_over_time)` 有专门分支：它们会创建、删除或覆写 label，
因此对“被写出的 label”必须丢弃对应 filter。

`keep_metric_names` 是 `BinaryOpExpr` 上的标志位，只影响结果是否保留
`__name__`，不改变 filter 下推规则；`__name__` filter 一律不参与公共 filter
（`getLabelFiltersWithoutMetricName`），所以名称保留与下推互不干扰。

### 2.1 合法性条件（下推必须同时满足）

- filter 必须在表达式输出的 label 空间里成立；凡是函数会“重写 label 名”，
  引用输出 label 的 filter 不得进入其输入。
- 对 binary join，filter 必须经过 `on(...)`/`ignoring(...)` 裁剪
  （`TrimFiltersByGroupModifier`）以及聚合的 `by/without` 裁剪
  （`trimFiltersByAggrModifier`）。
- 对序列集合并运算（`or`/`default`），只有两侧都有的 filter 才能下推；
  对交/差运算（`and`/`if`/`ifnot`/`unless`），结果只来自左值，左值 filter
  可进入右值作为匹配条件，但右值独有的 filter 不可进入左值。

## 3. 缺陷一：`prometheus_buckets` 的元数据缺口（函数元数据 → 下推）

### 3.1 语义

`prometheus_buckets(histogram_series)` 把 VictoriaMetrics 原生直方图桶的
`vmrange` label 重写为 Prometheus 兼容的 `le` label。其输入序列只携带
`vmrange`，输出序列只携带 `le`。

修复前 `prometheus_buckets` 落到 `getTransformArgIdxForOptimization` 的默认
分支（返回 0），于是 optimizer 把 binary op 另一侧推断出的 `le="0.5"`
filter 直接下推进它的输入选择器，而输入选择器根本没有 `le` label。

### 3.2 改写前后的 AST

查询：`prometheus_buckets(foo_bucket) / on(le) bar_bucket{le="0.5"}`

- 改写前（正确）

```
BinaryOpExpr{Op:"/", GroupModifier:on(le)}
├─ Left:  FuncExpr{Name:"prometheus_buckets"}
│           └─ Args[0]: MetricExpr foo_bucket            // 只有 vmrange
└─ Right: MetricExpr bar_bucket{le="0.5"}
```

- 改写后（修复前的错误行为）

```
BinaryOpExpr{Op:"/", GroupModifier:on(le)}
├─ Left:  FuncExpr{Name:"prometheus_buckets"}
│           └─ Args[0]: MetricExpr foo_bucket{le="0.5"} // le 在输入侧不存在
└─ Right: MetricExpr bar_bucket{le="0.5"}
```

### 3.3 合法性条件（此处被违反）

- “filter 必须在被下推表达式的**输出** label 空间成立”：`le` 是
  `prometheus_buckets` 的输出 label，不是其输入 label，下推后输入侧所有序列
  都不匹配，结果变空。
- join 的 `on(le)` 只说明两侧在 `le` 上对齐，并不授权把 `le` 过滤到函数内部。

### 3.4 修复

`getTransformArgIdxForOptimization` 增加 `case "prometheus_buckets": return -1`
（`optimizer.go`）。返回 -1 同时关闭“公共 filter 提取”和“下推”，与
`absent`/`scalar` 的保守处理一致。这是有意的保守选择：要做精确下推需要在
optimizer 内实现 `vmrange ↔ le` 的区间反解，收益小且易错，不在本次范围。

## 4. 缺陷二：`default` 被误当作数值算术运算（binary join → 下推）

### 4.1 语义

MetricsQL 的 `default` 是**序列级并集**（coalesce）：结果包含左值全部序列，
只有当某个 label 签名在左值缺失时才取右值。它和 `or` 一样是集合合并，而不是
逐点算术。修复前它落入 `getCommonLabelFilters` 的 `default:` 分支，按算术
运算做 `unionLabelFilters`，把每一侧独有的 filter 交叉注入另一侧。

### 4.2 改写前后的 AST

查询：`foo{x="1"} default bar{x="2"}`

- 改写前（正确）：

```
BinaryOpExpr{Op:"default"}
├─ Left:  MetricExpr foo{x="1"}
└─ Right: MetricExpr bar{x="2"}
结果 = {foo x=1} ∪ {bar x=2}                 // 两条序列
```

- 改写后（修复前的错误行为）：

```
BinaryOpExpr{Op:"default"}
├─ Left:  MetricExpr foo{x="1",x="2"}        // 同一 label 两个互斥值 → 空
└─ Right: MetricExpr bar{x="1",x="2"}        // 同上 → 空
结果 = ∅
```

`x` 同时被要求等于 `1` 和 `2`，两侧选择器都匹配不到任何序列。

### 4.3 合法性条件（此处被违反）

- “并集运算只允许两侧**共有**的 filter 下推”——这正是 `or` 分支的规则。
  `default` 的输出是两侧序列的并集，侧特有 filter 一旦进入另一侧，会错误地
  删掉本该作为 fallback 暴露的序列。

### 4.4 修复

`getCommonLabelFilters` 在 `case "or"` 旁新增 `case "default"`，同样执行
`intersectLabelFilters(lfsLeft, lfsRight)` 再经
`TrimFiltersByGroupModifier` 裁剪（`optimizer.go`）。带 `on/ignoring` 的
`default` join 也一并按交集处理。

## 5. prettifier 回写：往返不丢 modifier、不改 label matching

prettifier 的多行分支按节点类型重排：`BinaryOpExpr`（含 `keep_metric_names`
包裹与 `appendModifiers`）、`RollupExpr`（`[d:s] offset @`）、
`AggrFuncExpr/FuncExpr`（参数逐行）、`MetricExpr`（or-组与转义 metric 名）。

本快照已包含下列回写正确性修复（经与上游逐文件比对确认，本次无需再改）：

- `appendQuotedIdent` 对内嵌 `"`/`\\` 的转义（`lexer.go`），保证
  `{foo="bar","la\\"bel"="val"}` 这类 label 名往返可解析；
- 当 metric 名需要转义且是唯一 selector 时，多行分支显式输出
  `{"<name>"}`（`prettifier.go`），不再漏发名字；
- `VisitAll` 遍历 `JoinModifierPrefix`（`utils.go`），使
  `group_left() prefix "..."` 的前缀表达式对访问者可见。

本次新增的定向测试 `TestPrettifyParseRoundTrip` 与
`TestOptimizeRoundTripComposed` 用“Prettify → Parse → 比较 canonical
`AppendString`”断言：join 的 `on/ignoring/group_left/group_right/prefix`、
fill、rollup 修饰、`keep_metric_names`、转义 metric/label 名在往返后逐字节
等价，从而不会改变 label matching。

## 6. 每个 binary op 的公共 filter 规则（合法性条件速查）

| 运算 | 结果来源 | 公共 filter 规则 |
|---|---|---|
| `+ - * / % ^ atan2`、比较 | 两侧按 matching 签名成对 | `union` 后按 group modifier 裁剪 |
| `and` / `if` | 仅左值（右值作匹配门） | 默认 union（左 filter 入右安全） |
| `unless` / `ifnot` | 仅左值减去右值匹配 | 只取左值，再按 group modifier 裁剪 |
| `or` | 两侧并集 | 两侧 `intersect` |
| `default` | 左值优先、右值 fallback（并集） | 两侧 `intersect`（本次修复） |
| `group_left` | 左 label + join 列表 | 右 filter 按 group modifier 裁剪后并入 |
| `group_right` | 右 label + join 列表 | 左 filter 按 group modifier 裁剪后并入 |
| fill_left / fill_right | 单侧填充 | 只允许对应方向的下推 |

## 7. 可执行反证

无需外网、无随机 sleep、无本机绝对路径、无针对特定 fixture 文件名的特判：

- 参考模型：`confluence/semantics.go` 用一个极小的“label 集合语义”解释器，
  只建模“哪些序列存活、携带哪些 label”（optimizer 与 label matching 只与这
  两者有关）。它显式建模 selector、rollup、聚合 by/without、label manipulation、
  `prometheus_buckets`（`vmrange` 上界 → `le`）、`or/default/and/if/unless/ifnot`
  与 `on/ignoring/group_left/group_right`。
- 场景：`confluence/cases.go`（优化器保序场景 + prettifier 往返场景）。
- 可执行入口：`go run ./cmd/metricsql-confluence`，全部成立时退出码为 0，
  任一改写改变序列集或往返丢信息时退出码为 1 并打印差异。
- 测试：`go test ./confluence/ -run TestOptimizePreservesSeriesSet -v`
  可单独定位；`TestPrettifyParseRoundTrip` 单独定位回写。

在还原修复（`git stash` 掉 `optimizer.go` 改动）后，前三个场景被参考模型判为
`UNSOUND`：

- `prometheus_buckets-vmrange-to-le-join`：改写后丢失
  `__name__=foo_bucket;le=0.5;`；
- `default-coalesce-keeps-right-only-series`：两侧 fallback 序列全部丢失；
- `default-common-filter-still-pushed`：共有 filter 场景同样被错误交叉污染。

## 8. 定向单测与全量验证

- 新增单测位于 `optimizer_test.go` 的 `TestPushdownBinaryOpFilters`（直接
  下推 API）与 `TestOptimize`（端到端）：
  - `prometheus_buckets` 不下推 `le=...` 与任意其它 filter；
  - 嵌套在 `sum(...) by (le)` 内的 `prometheus_buckets` 同样不下推；
  - `default` 的侧特有 filter 不交叉、共有 filter 才下推、带 `on(...)` 与
    嵌套算术/逻辑表达式时按交集处理。
- 运行：
  - 定向：`go test . -run 'TestOptimize|TestPushdownBinaryOpFilters' -count=1`
  - 反证：`go test ./confluence/ -count=1 -v`
  - 全量：`go test ./... -count=1`

## 9. 相邻语义的退化保护

- `or` 的既有交集行为与全部现有用例保持不变（`default` 复用同一路径但独立
  case，未改动 `or` 分支代码）。
- `and/if/ifnot/unless` 分支未改动；其既有单测全绿。
- `prometheus_buckets` 返回 -1 只影响 optimizer，不影响解析、求值函数注册或
  prettifier；`histogram_quantile` 等仍正常下推 `le`（其输入本就以 `le`
  为桶 label）。
- label manipulation、`count_values(_over_time)`、聚合 modifier 裁剪、fill
  方向限制的既有断言全部保留并通过。
