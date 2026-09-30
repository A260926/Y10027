# Changelog

## Unreleased — 继承 block 作用域、可信标记与 loader 错误保真

本次工作以三层 `extends` + 同名 block 覆盖 + `include with/only` + macro +
safe/escape filter 的模板组为线索，沿 lexer → parser → node 树 → loader →
执行期 block/scope/缓冲链路排查并修复。详细不变量与用例索引见 `ANALYSIS.md`。

### 修复（生产代码）

1. **三层继承中 `parent()` 无限递归（栈溢出）** — `exec.go`
   - 旧 `getParentBlock(name)` 只“全局跳过第一个同名块”，且渲染父 block 体时不更新
     `s.current`；base←mid←leaf 三层链里 mid 的 `parent()` 解析回 mid，栈溢出。
   - 改为 `getParentBlockAfter(from, name)`：按当前 BlockNode 指针在 root-most-first 的
     blocks 栈上相对定位；`parent()` 渲染期间切换 `s.current` 与 `s.name`（Origin）。

2. **macro / parent() / block() 输出被二次自动转义** — `exec.go`
   - 三者原先返回裸 string，调用点 PrintNode 再转一次（字面标签变实体、数据双重编码）。
   - 新增包内 `newTrustedMarkup`，把模板作者产出的缓冲包成对 html/html_attr/js/css/url
     安全的 `SafeValue`（与 twig `|raw` 策略集一致，根包自持该集合，不反向依赖 twig）。

3. **子模板顶层 `{% import %}` / `{% from import %}` 对父 block 不可见** — `exec.go`
   - `walkChild` 原本只处理 use/set，漏掉两类 import 节点。
   - 补上 `ImportNode`/`FromNode` 分支，使其在父正文 walk 前绑定到共享 scope。

4. **MemoryLoader 错误丢失模板名与 cause** — `loader.go`
   - 裸 `os.ErrNotExist` 改为
     `stick: template %q does not exist: %w`，保留精确逻辑路径并支持
     `errors.Is(err, os.ErrNotExist)`；FilesystemLoader 本来就返回带路径的 PathError，
     executor 统一不再二次包装 loader error。

5. **`ignore missing` 误吞结构性 loader 错误** — `exec.go`
   - 现在仅当失败是 `os.ErrNotExist` 时才忽略；权限/I/O 等错误（含候选数组情形）立即
     带原 cause 上浮并停止候选探测，与 PHP-Twig 一致。

6. **解析错误从未真正实现 `ParsingError`（潜伏）** — `parse/error.go`
   - `parseError` 嵌入的是 `Pos` 字段而非 `Pos()` 方法，导致具体错误类型方法集缺
     `Pos()`，`Tree.enrichError` 的名称补全永远失败。
   - 字段改为私有 `pos` 并提供 `Pos() Pos`；错误文本格式不变，模板名现在正确出现。

### 结构选择

- 全部修复落在既有抽象内：`state.blocks` 栈方向（root-most-first）、`scopeStack`
  push/setLocal/pop、`SafeValue`、`Loader` 接口；没有复制并行的渲染/加载实现。
- “可信标记”由拥有 `SafeValue` 概念的根包产生，twig 包无需感知 macro/parent 的存在，
  自定义 escaper 的纯 stick 用户同样受益。
- 错误以 `%w` 保留 cause、以逻辑模板名保留 path，不在 executor 里拼本机文件系统绝对路径。

### 原有覆盖空白 → 对应用例

- 三层 `parent()` 链无任何覆盖（只有两层）：`TestParentChainNoStackOverflow`、
  `TestParentBlockVisibilityAtTwoDepths/depth 3`、`TestTemplateGroupDepthComparison`。
- 二次转义只在“显式 SafeValue 入参”上有例子，macro/parent/block 返回值路径无覆盖：
  `twig/TestSafeValuePreventsDoubleEscapingInheritance`、
  `TestMacroAndParentNotReEscaped`（用转换计数器证明执行层产生可信标记）。
- include 的上下文“复制而非共享”只有最终字符串用例：`TestIncludeContextCopyNotShare`
  直接断言调用方 map 不被改写、only 隔离、嵌套值浅共享。
- loader 错误无路径/cause 断言：`TestLoaderErrorKeepsPathAndCause`（四条解析路径）、
  `TestParentParseErrorKeepsName`、`TestIgnoreMissingSwallowsOnlyNotExist`。
- 词法/节点/缓冲次序此前都只靠最终输出间接体现：
  `parse/TestInheritTokenStream`、`TestTokenPositions`、
  `TestThreeLevelNodeTreeAndLoadOrder`、`TestParentBufferOrdering`、
  `TestTemplateGroupEndToEnd`（Write 单元 + loader 调用次序/多重性）。

### 最危险的反例

- `TestParentChainNoStackOverflow`：修复前直接 goroutine 栈溢出（非普通断言失败），
  配 5s 超时守护，防止回归时整个测试二进制崩溃。
- `TestIgnoreMissingSwallowsOnlyNotExist`：用注入的失败 loader 反证“ignore missing
  不是忽略所有错误”，并断言候选探测立即停止。
- `TestSafeValueScopeSpecific`：反证“safe 全局通用”的直觉——html-safe 在 js 策略下仍转义。

### 刻意保持的公开行为

- SafeValue/`NewSafeValue`/`|raw` 契约、include（with/only/数组/ignore missing）语义、
  解析错误文本格式、未知策略原样返回、成功路径 loader 行为均未改变。
- 未新增公开 API；新增符号（`getParentBlockAfter`、`newTrustedMarkup`）为包内可见。
- 未改弱任何既有断言；`testdata` 既有模板与 example 输出保持不变。

### 验证

- `go build ./...`、`go vet ./...`、`gofmt` 干净。
- 先跑新增定向用例、再跑全套：`go test -race ./...` 独立进程多次稳定通过。
- 注：既有的 `TestExec` 在同进程 `go test -count=N`（N>1）下因 `fakePerson` 等可变
  夹具累积而失败，基线代码同样如此，与本次改动无关；单次执行与独立进程重复运行稳定。
