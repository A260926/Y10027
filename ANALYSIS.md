# stick：从 Twig token 到继承模板、block 作用域与自动转义

本文档对应代码版本包含以下落点（每个结论都能回到具体分支与用例，不以“全套通过”作为结论）：

- token 层：`parse/inherit_token_test.go`
- node 树 / loader 次序 / 输出缓冲：`inherit_trace_internal_test.go`
- 局部 context 复制与共享：`inherit_scope_internal_test.go`
- loader error 的 path / cause：`inherit_error_internal_test.go`
- 修复的直接反证：`inherit_regression_internal_test.go`
- 自动转义 / SafeValue / 继承链：`twig/inherit_escape_trace_test.go`
- 端到端模板组与两种继承深度对比：`inherit_trace_e2e_test.go`

生产代码只改了三个文件，且全部沿用原有抽象（`state` / `scopeStack` / `blocks` 栈 /
`SafeValue` / `Loader`），没有复制新链路：`exec.go`、`loader.go`、`parse/error.go`。

---

## 1. 基线链路：token → node → loader → 缓冲

### 1.1 token 到 node

`parse/lex.go` 的 lexer 按 `{{ … }}`、`{% … %}`、`{# … #}` 切出 token，`parse/parse_tag.go`
把关键字落为节点：

- `extends` → `ExtendsNode`，挂在 `ModuleNode.Parent`（一个模板只允许一个）；
- `block … endblock` → `BlockNode`，同时写入 `tree.blocks` 栈顶 map，并把
  `BlockNode.Origin` 记为 `tree.Name`；
- `include/embed` → `IncludeNode`（`With/Only/IgnoreMissing` 三个标志）/ `EmbedNode`；
- `macro/import/from` → `MacroNode/ImportNode/FromNode`。

`TestInheritTokenStream` 固定了三层继承片段的 token 子序列
（`extends` 名字串、`block a`、`{{ x|raw }}`、`include … with … only`）与分隔符配对；
`TestTokenPositions` 固定 `Pos.Line` 为 1 基行号、`Pos.Offset` 为行内 0 基列号（lexer 在
遇到换行时把 offset 重置为换行后字节数，不是文件绝对偏移）。这两个断言在 node 树
*之前*，因此任何把词法切分改坏的回归会直接指向 lexer，而不是表现为输出变化。

### 1.2 node 到 loader 次序

执行入口 `execute` → `env.load(name)`（`Loader.Load` + `tree.Parse()`）→
`state.walk(tree.Root())`。`walk` 对 `*parse.ModuleNode` 的处理是：

1. 若有 `Parent`，求值父名、`env.load(parent)`，把父树的 `Blocks()` **append 到
   `s.blocks`**；
2. 先 `walkChild(node.BodyNode)`，只执行子模板允许“提前生效”的节点；
3. 再 `s.walk(tree.Root())` 走父模板正文。

`TestThreeLevelNodeTreeAndLoadOrder` 用 `recordingLoader` 实测三层链
（leaf→mid→base）的去重调用次序为 `leaf.twig, mid.twig, base.twig`（叶→根）。
端到端用例 `TestTemplateGroupEndToEnd` 进一步给出带 import/include 的完整次序：

```
home.html.twig, page.html.twig, widgets.html.twig,
layout.html.twig, shared.html.twig, shared.html.twig,
solo.html.twig, solo.html.twig
```

关键、且容易猜错的两点：

- **macro 库在 walkChild 阶段加载，发生在根父模板 layout 之前**。这是
  第 4 节 import 修复后才成立的顺序，用例把它钉死。
- **每个 include 名字出现两次**：`walkIncludeNode` 先 `Loader.Load` 做存在性探测，
  `execute` 再 `load`（Load+Parse）一次。用例同时断言了去重次序与精确多重性（各 2 次）。

### 1.3 输出缓冲的次序

`parent()`、`block()`、macro、`{% set x %}…{% endset %}`、`{% filter %}` 都通过
临时换 `s.out` 为 `bytes.Buffer` 捕获内容。`TestParentBufferOrdering` 不看最终字符串，
而是实现 `io.Writer` 记录每次 Write：三层链的非空白写单元必须是

```
[base: | LEAFA( | MIDA(BASEA) | ) | | | LEAFB | ]
```

即父 block 体（`MIDA(BASEA)`）作为 **parent() 的返回值整体** 在 `LEAFA(` 之后、`)`
之前落到外层 writer。若父体直接写 `s.out` 而非换缓冲，`BASEA` 会先于 `LEAFA(` 出现。

---

## 2. 父 block 何时可见

### 2.1 block 栈的方向

`execute` 先压入当前树的 `tree.Blocks()`，每遇到一层 `extends` 就 append 父树的 map。
因此 `s.blocks` 永远是 **根在最前、叶在最后**：

```
[ rootTreeBlocks, ..., parentTreeBlocks, leafTreeBlocks ]
```

- `getBlock(name)` 从前向后取第一个 → 离叶最近的覆盖者胜出（同名 block 覆盖）。
- 子模板里定义、但父模板没有槽位的 block 不会出现在父正文里
  （`TestParentBlockVisibilityAtTwoDepths/depth 2` 的 `CHILDC` 断言）。
- 父有、子未覆盖的 block 直接渲染父定义（同例 `BASEB` 可见）。
- include 走的是 `execute` 新建的全新 `state`，所以被包含模板既看不到也覆盖不了
  调用方的 blocks（`TestIncludeStartsFreshBlockStack` 直接对 `getBlock("b")` 做结构断言）。

### 2.2 `parent()` 的相对化（本次修复的最危险缺陷）

修复前的 `getParentBlock(name)` 只是“跳过栈中第一个同名块再取下一个”。它在两层链
（leaf→base）恰好正确，但在三层链（base←mid←leaf）里：

- leaf 的 block 调 `parent()`：跳过 leaf，得到 mid —— 正确；
- mid 的 block 体内部又调 `parent()`：此时全局“第一个”就是 mid 自己，跳过之后仍然是
  base……看似没问题，但关键在于**执行 mid 体时没有把 `s.current` 从 leaf 块切换到
  mid 块**，于是“跳过当前块”的参照系始终是 leaf：mid 的 `parent()` 再次解析到 mid，
  无限递归直到 goroutine 栈溢出。

修复（`exec.go`）做了两件事：

1. `getParentBlockAfter(from, name)`：以**当前正在渲染的 BlockNode 指针**为基准，先在
   栈上定位 `from` 的下标，再向下找下一个同名定义。指针比较无歧义，因为 parse 为每个
   定义构造独立的 `BlockNode`，embed/use 贡献的同名块也不会混淆。
   `TestBlockStackStructureAfterExtends` 直接构造栈验证：leaf→mid、mid→base、base→nil。
2. `parent()` 渲染父体前临时把 `s.current = blk`（defer 恢复），并像 `walk(BlockNode)`
   一样按 `blk.Origin` 切换 `s.name`。这样嵌套 `parent()` 逐层上溯，
   `Context.Name()`/`Meta()` 的自定义 filter 也能看到正确来源
   （`TestContextNameFollowsOrigin`）。

反证用例（每个都单独可回退验证）：

- `TestParentChainNoStackOverflow`：三层链 + 5 秒超时守护，修复前直接栈溢出崩溃；
  修复后输出 `LEAFMIDBASE`。
- `TestParentBlockVisibilityAtTwoDepths`：**同一套 block 语义在两种继承深度各跑一次**，
  depth 2 期望 `LEAFA(BASEA)`、`BASEB` 可见、`CHILDC` 不可见；depth 3 期望
  `LEAFA(MIDA(BASEA))`。
- `TestTemplateGroupDepthComparison`：同一 leaf 内容分别挂两层与三层链，唯一允许差异
  是 mid 层的包裹：`PL(PBLK)` vs `PL(M(PBLK))`。

---

## 3. 局部 context 何时复制、何时共享

### 3.1 include

`walkIncludeNode` 的行为：

- 先建空 map；
- 无 `only` 时：`ctx = s.scope.All()` —— 这是对**当前 scope 链的拍平快照（新 map）**，
  再用 `with` 的 hash 键覆盖；
- 有 `only` 时：跳过拍平，`ctx` 只含 `with`；
- 然后调用 `execute(tpl, s.out, ctx, env)`，**新建一个 `state`**，其根 scope 就是这份
  独立 map。

因此：

- include 内的 `{% set %}` 不会回写调用方（map 不同）。
  `TestIncludeContextCopyNotShare` 让 inc 把 `outer` 改成 `MUT`，调用方输出仍是
  `<after:OUT>`，且 inc 内新建的 `inner` 不出现在调用方 map。
- `only` 时调用方变量完全不可见（同例 `only{outer=UNDEF…h.k=NOH}`）。
- 快照是**浅拷贝**：`h.k` 这样的嵌套 hash/struct 仍是同一底层值，与 Twig 对“对象按
  引用、标量按值快照”的习惯一致；用例显式断言共享变量 `h.k=K` 在普通 include 中可见。

### 3.2 macro

`callMacro` 每次调用 `s.scope.push()`、把形参 `setLocal` 到新局部 scope，结束 pop；
`{% set %}` 经 `scopeStack.Set` 命中形参所在层，不会写到外层。
`TestMacroScopeIsolation` 连续调用 `f('X')`、`f('Y')` 并在体内改写形参：输出
`[XZ][YZ]NOLEAK`，证明形参与体内变量既不跨调用残留也不泄漏到模板全局。

### 3.3 子模板顶层 set / import（相对 walkChild 的特殊规则）

`walkChild` 默认对节点“什么都不做”，但有三个刻意例外，均在父正文 walk **之前**作用于
共享 scope/blocks：

- `UseNode`：提前合并被 use 模板的 blocks；
- `SetNode`：子模板顶层 `{% set %}` 必须对父 block 可见
  （`TestChildSetVisibleInParentBlock`：leaf `set greeting='HI'`，base 打印出 `HI`）；
- **本次补上的 `ImportNode`/`FromNode`**：修复前 walkChild 直接漏掉这两种节点，父
  block 里调用子模板导入的 macro 会得到 `Undeclared function`。
  `TestChildImportVisibleInParentBlock` 用 `{% from 'm' import greet %}` 与
  `{% import 'm' as ui %}` 两种形式反证。

### 3.4 Env 层叠加

include/macro 全程共享同一个 `*Env`：自定义 `Functions/Filters/Tests`、Visitors、Loader
都沿用入口 env，而不是重新构造一个只剩内置项的 env。
`TestStackedEnvSeenByFunctionsAndFilters` 在根模板调自定义函数、在被包含模板调自定义
filter，必须同时解析。

---

## 4. SafeValue 何时阻止二次转义

### 4.1 自动转义是怎么挂上的

`twig.New` 注册 `AutoEscapeExtension`，其 `autoEscapeVisitor` 在 parse 后的遍历期把每个
`PrintNode.X` 改写为 `escape(X, 策略)`。策略来自名字：`ModuleNode.Origin` 与每个
`BlockNode.Origin` 各 push 一层（按文件扩展名猜，默认 html），离开时 pop。

- block 体按 **block 自己的 Origin** 决定策略：`TestAutoEscapeStrategyFollowsOrigin`
  直接解析 AST，断言 html 模板里 block 的 PrintNode 被包成 `escape(…, 'html')`。
- include 进来的 `.js.twig` 用 js 策略转义（同例：`a"b` → `a\u0022b`），与调用方是
  html 无关。
- 策略名没有注册 escaper 时（如 `.txt`）escape filter 原样返回值——这是既有行为，未改动。

`escape` filter（`twig/escape.go`）对入参做判断：若已经是 `SafeValue` 且对当前策略
`IsSafe(ct)`，原样返回；否则调 escaper 并包成该策略的 SafeValue。

### 4.2 二次转义缺陷（本次修复）

修复前 `callMacro` 与 `parent()`/`block()` 返回的是**裸 string**。但它们渲染出的内容
在各自 PrintNode 上已经按策略转过一次；调用点的 PrintNode 又会对“数据 string”再转一次：

```
macro box(x) = <b>{{ x }}</b>，x = <i>
修复前：&lt;b&gt;&amp;lt;i&amp;gt;&lt;/b&gt;   （字面 <b> 被转，参数被转两次）
修复后：<b>&lt;i&gt;</b>
```

修复：在执行层新增 `newTrustedMarkup(s)`（`exec.go`），把 macro 缓冲与
parent/block 缓冲包成对全部内置策略（`html, html_attr, js, css, url`）安全的 SafeValue
（策略集合与 `twig/filter` 的 `raw` 对齐；由拥有 `SafeValue` 抽象的根包持有，避免根包
反向依赖 twig 子包）。这对应 PHP-Twig 中 macro/parent 返回 `Twig\Markup` 的契约：
模板作者产出的标记可信，数据在“内部那一次”已经转过。

反证（不只看最终字符串）：

- `twig/TestSafeValuePreventsDoubleEscapingInheritance`：macro、三层 parent、`block()`、
  对 SafeValue 显式连打两次 `escape` 四个子用例；并显式断言输出中不含 `&amp;`。
- `TestMacroAndParentNotReEscaped`（核心包，只用自定义 escape filter，不引入 twig 包）：
  用 `escapeCounter` 记录真正发生转换的次数——macro 数据恰好转 1 次、外层对模板标记
  0 次；parent 缓冲跨边界同样只转 1 次。这证明“可信”来自**执行层返回类型**，而非 twig
  子包的特殊处理。
- `TestSafeValueScopeSpecific`：安全是**按策略**的。对 html 安全的值在 `escape('js')`
  下仍被 js 转义（`"` → `\u0022`），纠正“safe 是全局的”这一最常见误解。
- `|raw` 产出形状由 `TestRawFilterOutputShape` 固定（五类策略均 IsSafe、底层值不变）。

---

## 5. loader error 如何保留 path 与 cause

### 5.1 两个真实缺陷

1. **MemoryLoader 丢失模板名与 cause**。修复前
   `return nil, os.ErrNotExist`，经 include/extends/import/入口四条路径上浮的都是裸
   `file does not exist`，既不知道是哪个模板，也无法 `errors.Is(err, os.ErrNotExist)`
   （`%w` 之前）。现改为：

   ```go
   fmt.Errorf("stick: template %q does not exist: %w", name, os.ErrNotExist)
   ```

   `FilesystemLoader` 本来就返回 `*os.PathError`（含 path 与 cause），保持不变；
   executor 对 loader error 不再额外包装，原样返回。

2. **ParsingError 接口从未被满足（潜伏缺陷）**。`parseError` 嵌入的是 `Pos` **字段**
   而不是 `Pos()` **方法**，导致 `*UnexpectedTokenError` 等具体错误缺少方法集里的
   `Pos()`，`Tree.enrichError` 的 `err.(ParsingError)` 永远失败——解析错误因此从未带上
   模板 `Name()`。修复把字段改为私有 `pos` 并提供 `Pos() Pos` 方法（`parse/error.go`），
   错误文本格式保持不变（仍是 `parse: … on line L, column C in NAME`）。

### 5.2 反证用例

- `TestLoaderErrorKeepsPathAndCause` 覆盖全部四个解析路径（include / extends / import /
  入口模板），每个都同时断言错误文本含精确模板名、且
  `errors.Is(err, os.ErrNotExist)` 成立。
- `TestParentParseErrorKeepsName`：父模板里的语法错误必须归到父名与精确行列
  （`line 3` in `broken.twig`），而不是栽到正在执行的 leaf。
- `TestInheritParseErrorKeepsNameAndPos`（parse 包内）：直接断言 enrich 后错误实现
  `ParsingError`，`Name()=="mid.html.twig"`、`Pos()==3:9`。

### 5.3 `ignore missing` 只吞 not-exist

这是最容易“猜对一半”的语义。PHP-Twig 的 `ignore missing` 只隐藏“模板不存在”，其他
loader 故障（权限、I/O、loader 结构性拒绝）必须立刻上浮。修复后的 `walkIncludeNode`
对每次探测结果 `errors.Is(lerr, os.ErrNotExist)`：非 not-exist 立即返回该错误
（不包装，保留 cause 与候选 path），并停止后续候选探测；只有全部候选都 not-exist 时
才在 IgnoreMissing 下返回空渲染。

`TestIgnoreMissingSwallowsOnlyNotExist`：

- 单名字 include + ignore missing + not-exist → 渲染 `[]`；
- 自定义 loader 对名字返回包装了 `os.ErrPermission` 的错误 → 即使 ignore missing 也上浮，
  `errors.Is(…, os.ErrPermission)` 成立、文本含候选名，且 loader 只被调用 1 次
  （不能继续尝试后面的候选）；
- 候选数组首个名字即结构性错误 → 立即上浮并带原 cause，`second.twig` 不被探测。

这里的自定义 loader 通过 `composedLoader`（实现 Loader 契约做路由）注入，测试不依赖
外网、sleep、本机绝对路径，也不对任何 fixture 名称特判。

---

## 6. 端到端模板组走读

`TestTemplateGroupEndToEnd` 用 `MemoryLoader` 装一组模板（名字均为逻辑名）：

| 模板 | 角色 |
| --- | --- |
| `layout.html.twig` | 根：`aside`/`content` block 槽 + 自定义 `footer()` 函数 |
| `page.html.twig` | 中间层：extends layout，覆盖 `aside`，`content` 调 `parent()` |
| `home.html.twig` | 叶：extends page，`{% from widgets import badge %}`，覆盖 `content`：macro → include(共享上下文) → include only → `parent()` |
| `widgets.html.twig` | macro `badge(name)`：输出 `<span class="badge">…</span>`，参数显式 escape |
| `shared.html.twig` | 普通 include：读共享 `user`、`with` 进来的 `note|raw`、自定义函数 `shout` |
| `solo.html.twig` | include only：读不到 `user`（default NOUSER），只有 `note` |

一次执行同时追踪三类观测：

1. **loader 调用次序与多重性**（见 1.2）；
2. **输出缓冲单元次序**：`HOME[` → macro 单元 → shared → solo → `PAGE(BASEC)`（父链）
   → layout 的 safe footer；用例用单元下标断言先后而非大快照；
3. **最终输出的结构化子串**，每条映射到不同执行路径：
   - `aside:PASIDE`（中层独占覆盖）、`HOME[`（叶覆盖）、`PAGE(BASEC)`（三层 parent）、
     macro 数据单次转义、shared 的上下文叠加与 raw、solo 的 only 隔离、
     SafeValue 函数输出原样落地，以及全局不含 `&amp;`。

---

## 7. 不变量 → 用例索引（审查入口）

| 不变量 / 最易猜错点 | 代码落点 | 用例 |
| --- | --- | --- |
| token 子序列与位置（Offset=列号） | `parse/lex.go` | `TestInheritTokenStream`, `TestTokenPositions` |
| loader 叶→根、include 探测两次 | `exec.go` walk(ModuleNode)/walkIncludeNode | `TestThreeLevelNodeTreeAndLoadOrder`, `TestTemplateGroupEndToEnd` |
| 父块缓冲作为值整体返回 | `exec.go` evalFunction parent + newTrustedMarkup | `TestParentBufferOrdering` |
| 同名 block 叶优先；child-only 不渲染；include 不见调用方 block | `getBlock`, `execute` 新 state | `TestParentBlockVisibilityAtTwoDepths`, `TestIncludeStartsFreshBlockStack` |
| 三层 `parent()` 逐层上溯、不崩 | `getParentBlockAfter` + `s.current` 切换 | `TestParentChainNoStackOverflow`, depth-3 子用例 |
| 两种继承深度对比 | 同上 | `TestTemplateGroupDepthComparison` |
| include 上下文拍平复制、only 隔离、浅共享 | walkIncludeNode `s.scope.All()` | `TestIncludeContextCopyNotShare` |
| macro 参数/set 不泄漏、不串调用 | callMacro push/setLocal/pop | `TestMacroScopeIsolation` |
| 子模板 set/import 对父 block 可见 | walkChild | `TestChildSetVisibleInParentBlock`, `TestChildImportVisibleInParentBlock` |
| 自定义 fn/filter 沿同一 Env 叠加 | execute 共享 `*Env` | `TestStackedEnvSeenByFunctionsAndFilters` |
| Context.Name 跟随 block/macro Origin | walk(BlockNode)/parent() 的 s.name | `TestContextNameFollowsOrigin` |
| macro/parent/block 输出不被二次转义 | `newTrustedMarkup` | twig escape 四连测 + `TestMacroAndParentNotReEscaped` |
| safe 按策略区分 | twig escape filter | `TestSafeValueScopeSpecific` |
| 策略跟随 Origin（block/include js） | autoEscapeVisitor | `TestAutoEscapeStrategyFollowsOrigin` |
| loader 错误保留 path+cause（4 路径） | `loader.go`, `exec.go` | `TestLoaderErrorKeepsPathAndCause` |
| 父模板 parse 错误归父名行列 | `parse/error.go` Pos() 修复 | `TestParentParseErrorKeepsName`, `TestInheritParseErrorKeepsNameAndPos` |
| ignore missing 只吞 not-exist | walkIncludeNode errors.Is | `TestIgnoreMissingSwallowsOnlyNotExist` |

## 8. 刻意保持不变的公开行为

- `SafeValue` 接口、`NewSafeValue` 签名与 `|raw` 的五类策略集合不变；`CoerceString` 对
  SafeValue 透传内层字符串的既有约定不变。
- include 的上下文仍是“新 map 浅快照”；`only`/`with`/候选数组/ignore missing 的解析
  语法与错误文案不变。
- MemoryLoader 成功路径、StringLoader/FilesystemLoader 行为不变；新增错误文案仍包含既
  有测试依赖的 `does not exist` 子串，且仍是 `os.IsNotExist`/`errors.Is` 可识别。
- 解析错误文本格式（`parse: … on line L, column C [in NAME]`）不变；只是 NAME 现在真的
  会被填上。
- autoescape 对未知策略（如 `.txt`）“原样返回”的既有宽松行为不变。
- 未引入新的公开类型/函数（`getParentBlockAfter`、`newTrustedMarkup` 均为包内符号）。

## 9. 已知的既有限制（本次未改，避免越界）

- macro 词法上能看到外层变量（Twig 严格隔离 macro 作用域）：当前 block/macro 设计如此，
  文档与用例只锁定“形参与体内 set 不泄漏”，未改可见性。
- 未知转义策略不报错而原样返回（escape.go 中原有 TODO）。
- `TestExec` 的 `fakePerson` 等夹具在同一进程 `-count=N` 下依赖可变共享状态，会累积；
  正常单次/独立进程执行稳定，本次未改弱也未改动该夹具。
