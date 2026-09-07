# Phorge 兼容契约

本文件记录 Gorge 的 Go 服务与 Phorge PHP 端之间**不能随意改动**的七项约定。这些约束此前只以注释形式散落在代码里，而它们的共同特征是：**破坏之后不会有任何报错**。

| 约定 | 破坏后的表现 |
|---|---|
| 一、语言别名表 | 两个高亮后端把同一语言解析到不同 lexer，无断言捕捉 |
| 二、Chroma formatter 配置 | 全站高亮静默失效——页面正常渲染，只是没有颜色 |
| 三、端口与路由 | PHP 侧配置指错地方，表现为 `ERR_NOT_FOUND` |
| 四、unified diff 输出格式 | 解析器接受错误的 hunk 头，然后**静默地把之后每一行都放错位置**（第 4.6 节写明了保证到哪里为止） |
| 五、Aphlict 线兼容（通知） | 四条子约束，最坏的一条（5.4）**连错误状态码都不产生**：请求答 200、fingerprint 合法、`messages.in` 照常增长，只有消息内容被静默揉碎 |
| 六、mailer 的错误码与字段名 | 唯一一项会**改变 PHP 侧行为**的约定：`ERR_PERMANENT_FAILURE` 决定 worker 要不要重投这封信，两个方向的误判分别是「无限重投」与「静默丢信」，都要几天后看邮件统计才发现 |
| 七、search 的字段名与分析器链 | 五条子约束，全部是「写得进去、答 200、就是查不到」型。7.3 的 4 字符字段名与 7.5 的 `cjk` 子字段是其中最安静的两条：索引照常增长、每条路径照常 200，只有检索结果悄悄变空 |

第四项是其中最隐蔽的：它没有「失效」这个状态，只有「悄悄错位」。第五项走得更远：5.4 破坏之后**没有任何一处产生错误**——不是「错误被 PHP 吞掉」，是压根没有错误可吞，因为那个 POST 成功了。第六项的性质又不一样：它**会**产生一个明确的失败状态，只是方向是反的，所以看日志找不出问题——每条记录看起来都合理。第七项则是把「静默」推到了另一个维度：破坏之后**写入侧一切正常**，索引在长大、统计在增加、集群面板全绿，错的只是「写进去的键」与「查出来的键」对不上，而没有任何一层会去比对这两者。

改动其中任何一项，都必须同步改动 PHP 侧并在这里更新说明。

---

## 一、Pygments 语言别名表：PHP 表是下界，Go 表可以是超集

**Go 侧**：`go/internal/render/highlight/lexermap.go` 的 `buildLexerMap()`
**PHP 侧**：`PhutilPygmentsSyntaxHighlighter::getPygmentsLexerNameFromLanguageName()`
（参考实现见 `phorge-fork/src/infrastructure/markup/syntax/highlighter/PhutilPygmentsSyntaxHighlighter.php`）

Go 侧的别名表是从 PHP 侧那张 `static $map` 抄过来的（PHP 166 条，Go 184 条）。它的作用是把 Phorge 数据库里存量的语言标识（`adb`、`ads`、`ahkl`、`bat`、`cxx` 这类历史别名）翻译成 Chroma 认得的 lexer 名。

要防的是什么：Phorge 并没有全量切到 Go 服务，`PhutilPygmentsSyntaxHighlighter` 仍是可选的高亮后端。两个后端把同一个语言标识解析到不同 lexer 时，产生的 HTML 不同，且没有任何断言会捕捉到。**但这不等于两张表必须逐条相等**，下面两小节把范围划准。

### 必须同步的方向只有 PHP → Go

PHP 侧查表是 `idx($map, $language, $language)`：**未命中就把语言名原样透传**给 pygmentize。这条透传语义决定了约束是不对称的。

判据是「PHP 表对该键做了**非恒等映射**」——现存 166 条恰好全部满足（`adb` → `ada` 这类），所以实践上就是一句话：**PHP 表有的键，Go 侧必须有，且映射到等价的 lexer。**非恒等映射意味着 pygmentize 认不得原始名、或认得但指向另一个 lexer，必须靠表改写；这种键 Go 侧漏掉时，Go 会把 `adb` 原样交给 Chroma，落到内容嗅探，两个后端就此分叉。**漏一条就是一次静默漂移，这是本节真正要守的东西。**

### 反过来，Go 表可以是 PHP 表的超集

Go 目前独有 20 条，**这不算违约，也不要求补到 PHP 侧**：

- 15 条现代语言键：`ts` / `tsx` / `jsx` / `rs` / `kt` / `kts` / `swift` / `toml` / `tf` / `hcl` / `gradle` / `dockerfile` / `containerfile` / `graphql` / `gql`
- 5 条 PHP 混合大小写键的小写补充：`gnumakefile` / `rakefile` / `rout` / `sconscript` / `sconstruct`

无害的理由就在上面那条透传语义：pygmentize 本身就认 `ts`、`rs`、`kt` 这类别名，PHP 未命中后把原始名透传过去，落到的是同一个 lexer。两个后端结果一致，没有漂移可言。Go 侧多这几条只是省掉一次 Chroma 的猜测。

新增 Go 独有键时唯一要确认的就是这个前提：**pygmentize 透传该名字后能落到与 Go 相同的 lexer**。若不成立（pygmentize 完全不认，PHP 侧会退回 `PhutilDefaultSyntaxHighlighter`），那就得同时补 PHP 侧。

### 这张表区分大小写

PHP 侧是一个普通 PHP 数组加 `idx()`，键的大小写原样参与匹配；而传进来的语言名是 `PhutilDefaultSyntaxHighlighterEngine::getLanguageFromFilename()` 从文件名里切出来的扩展名，**没有做归一化**，所以 `foo.R` 真的会以 `R` 的形式到达这里。表里有两组同字母异映射：

| 键 | 目标 lexer | 含义 |
|---|---|---|
| `R` / `S` | `splus` | R 语言（`splus` 是 Chroma 里 R lexer 的别名） |
| `r` | `rebol` | REBOL，Chroma 无对应 lexer，退化为纯文本 |
| `s` | `gas` | GAS 汇编 |

因此 Go 侧 `resolveLexer()` **先用原始字符串查表，未命中才降级到 `strings.ToLower` 再查一次**。早期实现无条件先 `ToLower`，把 `R` 折成 `r`、`S` 折成 `s`，结果是所有 `.R` 文件按 REBOL 处理（即无高亮）、所有 `.S` 文件按汇编处理。这是本约束唯一一次真实漂移，`tests/contract/render/render-language-case-{uppercase,lowercase}.json` 与 `TestCaseSensitiveAliasesReachDistinctLexers` 现在把它锁住了。`lexermap.go` 末尾单列了一组混合大小写键，与 PHP 表逐条对应，方便 diff。

### Chroma 与 Pygments 的 lexer 命名差异不算漂移

Go 侧的目标名必须是 Chroma 真的认得的，否则 `lexers.Get()` 返回 nil，请求静默退化成内容嗅探——补了等于没补。

**能照抄就照抄。**即使 Chroma 同时接受某个同义写法，目标名也一律用 PHP 的那个（`rb` 而不是 `ruby`、`coffee-script` 而不是 `coffeescript`、`Cucumber` 而不是 `cucumber`），这样两张表能逐字 diff，不必每次都判断「写法不同但等价」。下表是**偏离的完整清单**，已逐条实测（Chroma v2.27），下一个人不必重查：

| PHP 写的名字（涉及的键） | 在 Chroma 里 | Go 侧写什么 |
|---|---|---|
| `splus`（`R` / `S`） | ✅ 存在，是 Chroma R lexer 的别名 | 照抄，所以 `R` / `S` 的修复真实生效 |
| `rebol`（`r` / `r3`） | ❌ 不存在 | 照抄，退化为纯文本；忠实反映 PHP 的意图，Chroma 无力实现 |
| `rconsole`（`Rout`） | ❌ 不存在 | 照抄，同上 |
| `antlr-ruby`（`g` / `G`） | ❌ 不存在 | **偏离**：写 `antlr` |
| `ragel-em`（`rl`） | ❌ 不存在 | **偏离**：写 `ragel` |
| `v`（`sv`） | ⚠️ 存在，但指向 **V/vlang 语言**，不是 Verilog | **偏离**：写 `verilog`，改回 `v` 会得到彻底错误的语言 |
| `html+evoque` / `xml+evoque`（`html` / `xml`） | ❌ 不存在 | **偏离**：整条不进表。进表反而让 `html` / `xml` 从直接命中 Chroma 的 HTML/XML lexer 退化成内容嗅探 |

与 PHP 不一致的就只有标「偏离」的这四组，其余一律逐字相同。这两类偏离（换等价名、刻意留空）都不算漂移，不要「顺手修正」回 Pygments 的原名。

`v` 那条是对齐时最容易踩的坑，也是判据的来处：**`lexers.Get()` 返回非 nil 不代表解析对了。**要对齐某个写法时，比对的是两个写法拿到的 lexer **身份**（`Config().Name`）是否相同，只判空会把 `sv` → `v` 放过去。

**新增别名的检查清单**：PHP 表有的键 Go 必须有，键的大小写照抄；Go 独有键先确认 pygmentize 透传后落到同一 lexer；目标名先用 `lexers.Get()` 确认 Chroma 认得；最后在 `tests/contract/render/` 补一条固件。

## 二、Chroma formatter 的三项配置是固定的

`go/internal/render/highlight/highlight.go` 顶部：

```go
formatter = html.New(
    html.WithClasses(true),
    html.PreventSurroundingPre(true),
)
defaultStyle = styles.Get("pygments")
```

三项都不能改，各有各的理由：

| 配置 | 原因 |
|---|---|
| `styles.Get("pygments")` | Chroma 的 style 决定输出的 CSS **类名**。只有 `pygments` 这一套的类名（`k`、`nf`、`nb`、`s2`、`mi`、`c1` …）与 Phorge 既有样式表对得上。换成 `monokai`、`github` 之类会输出另一批类名，页面上所有 token 都失去样式。 |
| `WithClasses(true)` | 输出 `class="k"` 而不是内联 `style="color:#008000"`。内联样式会绕过 Phorge 的样式表与暗色主题，且体积暴涨。 |
| `PreventSurroundingPre(true)` | 只输出 token 片段，不带外层 `<pre>`。外层容器由 Phorge 自己渲染（它要挂行号、diff 高亮等附加结构），Chroma 再包一层会造成嵌套 `<pre>`。 |

守住这条的是 `go/internal/render/highlight/compat_test.go` 的 `TestPygmentsCSSClassCompatibility`：它断言输出里出现 `k`/`n`/`nf`/`nb`/`s2`/`mi`/`c1` 这批类名。render 域的契约固件同样只做 contains 断言——Chroma 升级会改变 HTML 的具体结构，精确 golden 匹配必然频繁误报，真正要锁死的只是类名集合。

（第四项约定的固件反过来做**整值**比对。断言精度按输出的稳定性来定，不是全仓库一个口径；判据见 [`../../tests/contract/README.md`](../../tests/contract/README.md)。）

## 三、端口与路由变更记录

### 路由：按域命名，不按二进制命名

`gorge-render` 这一个进程承载整个 render 域。路由保持 `/api/highlight/*`，**不是** `/api/render/*`：

- `POST /api/highlight/render`
- `GET /api/highlight/languages`

这两个路径是 `PhabricatorGorgeRenderClient`（`src/infrastructure/cluster/PhabricatorGorgeRenderClient.php`）已经在调的，改了要同步改 PHP。按域而非按二进制命名的好处已经兑现了一次：diff 并进同一进程时直接加了 `/api/diff/*`，两个域的路由都没有改动。

diff 域的两条路径同样属于契约：

- `POST /api/diff/generate`
- `POST /api/diff/prose`

### 端口：`:8130` 已并入 `:8140`

原先 highlight 与 diff 是两个独立服务，现在是一个进程两个域：

| 服务 | 旧端口 | 现状 |
|---|---|---|
| `gorge-highlight` | `:8140` | 由 `gorge-render` 继承，保持 `:8140` |
| `gorge-diff` | `:8130` | **已废弃**，diff 域在 `gorge-render` 的 `:8140` 上 |

对 PHP 侧的影响：`gorge.render.uri` 配置项（与之配套的 `gorge.render.token`）不需要改。**接入 diff 时，原本指向 `:8130` 的 diff 配置要改指 `:8140`**；旧的 `phorge/docker/services/docker-compose.yml` 里 `diff` 服务那一段应当整体删除，而不是留着空跑。

一个 token 同时守两个域：它认证的是调用方对这个**进程**的身份，不是对某个路由分组的身份，所以 PHP 侧不需要第二个 token 配置项。

### 环境变量：新名优先，旧名兜底

因为 highlight 与 diff 共用一个进程，`MAX_BYTES`、`TIMEOUT_SEC` 这类裸名会真的撞车。新配置引入 `GORGE_` 前缀，服务级知识再加域名段；`platform/config` 按顺序查找，取第一个非空值，所以旧编排文件里的裸名仍然能跑。

| 新名 | 旧名（兜底） | 默认值 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `LISTEN_ADDR` | `:8140` |
| `GORGE_SERVICE_TOKEN` | `SERVICE_TOKEN` | 空（空则不鉴权） |
| `GORGE_CONFIG_FILE` | `HIGHLIGHT_CONFIG_FILE` | 无 |
| `GORGE_RENDER_MAX_BYTES` | `MAX_BYTES` | `1048576` |
| `GORGE_RENDER_TIMEOUT_SEC` | `TIMEOUT_SEC` | `15` |
| `GORGE_DIFF_MAX_BYTES` | **无**（见下） | `1048576` |

旧名保留是为了让 `phorge/docker/services/docker-compose.yml` 不改也能起来，属于过渡措施，不要在新编排里使用。

`GORGE_DIFF_MAX_BYTES` 是唯一**没有**旧名兜底的一条，这是刻意的。`gorge-diff` 原先读 `MAX_BODY_SIZE`，但那是个 Echo 传输层限制、值是字符串（`"10M"`）、作用于整个请求体；而 `GORGE_DIFF_MAX_BYTES` 是字节数、作用于 `len(old)+len(new)`。两者的**单位、语法和作用对象都不同**，认旧名等于静默地重新解释它的值，所以这是一次重命名而不是兜底。

（`config.EnvInt` 用 `strconv.Atoi`，解析失败就跳过该键回落到默认值。所以真去兜底 `MAX_BODY_SIZE`，`"10M"` 会被静默丢弃、悄悄降到默认的 1 MiB，而运维以为设的是 10 MB。不报错的收紧比报错更难查，这也是不做兜底的理由。）

旧编排里的 `MAX_BODY_SIZE` 现在直接被忽略，迁移编排时要显式设新名。

## 四、unified diff 的输出格式：逐字节替代 `diff -U65535`

**Go 侧**：`go/internal/diff/unified/unified.go`
**PHP 侧**：`PhabricatorDifferenceEngine::generateRawDiffFromFileContent()`
（参考实现见 `phorge-fork/src/infrastructure/diff/PhabricatorDifferenceEngine.php`）

这是四项约定里最严的一条，因为**输出是被解析的，不是被渲染的**。

PHP 侧原先 `proc_open` 调 `diff -U65535 -L <name> -L <name>`，输出交给 `ArcanistDiffParser` 与 `DifferentialHunkParser`。所以 Go 侧要复现的不是「一份合法的 unified diff」，而是 **GNU（以及 Apple/FreeBSD）diff 逐字节写出来的那一份**。

**偏离不会在任何地方报错。**解析器接受一个错误的 hunk 头，然后静默地把它之后的每一行都放错位置——错误在代码评审页面上表现为渲染错位，离出错点已经很远，而且没有任何测试或日志会指向格式。

### 4.1 行是 `(文本, 是否有尾换行)` 的二元组

**GNU diff 认为「无尾换行的一行」与「同样文字但有尾换行的一行」是不同的行。**所以 Go 侧的行模型两个字段都参与相等判定：

```go
type line struct {
	text       string
	hasNewline bool
}
```

于是 `"a\nb"` 对 `"a\nb\nc\n"` 会报 `-b` / `+b`，而不是把 `b` 当共享上下文保留。给文件补一个尾换行是真实变更，必须渲染成变更。

按 `\n` 切成 `[]string` 并丢掉尾部空元素的写法会丢掉这个信息，**这是迁入前的实现的做法，已经改掉，不要改回去。**

### 4.2 hunk 头的计数有三种写法

全文上下文（`-U65535`）下每个 hunk 都从第 1 行开始，只有计数在变。但计数**不是统一格式**：

| 该侧行数 | GNU 写法 | 说明 |
|---|---|---|
| 0 | `0,0` | 空的一侧从 0 开始——没有「第 1 行」可指 |
| 1 | `1` | **完全省略计数**，即 `@@ -1 +1 @@` |
| ≥2 | `1,count` | 常规形态 |

写成 `-1,1` 而 GNU 写 `-1`，足以让整个 hunk 错位。这是最容易引入、也最难察觉的一处偏离。

### 4.3 `\ No newline at end of file` 跟在承载它的记录之后

一条规则推出三种可观察情形：

| 情形 | 标记出现 |
|---|---|
| 两侧共享的末行都缺尾换行 | 一次，跟在那条上下文行后 |
| `-`/`+` 配对且两侧都缺 | 两次，各自一处 |
| 只有一侧缺 | 一次，在那一侧的位置 |

注意 diff 文本自身的每一行（**包括标记行**）都以换行结尾——缺尾换行的是被描述的内容，不是 diff。

### 4.4 相同输入这一支照抄 PHP，**不**照抄 GNU

这是全条约定里唯一以 PHP 而非 GNU 为基准的地方，也是最容易被「顺手修正」的地方。

`diff` 在两个文件相同时退出码 0 且**什么都不输出**。PHP 侧因此在这一支自己拼字符串，好让调用方仍能据此渲染那个未变更的文件——它手里没有第二份内容：

```php
$entire_file = explode("\n", $old);
foreach ($entire_file as $k => $line) {
  $entire_file[$k] = ' '.$line;
}
$len = count($entire_file);
$diff = "--- {$old_name}\n+++ {$new_name}\n@@ -1,{$len} +1,{$len} @@\n"
      . implode("\n", $entire_file) . "\n";
```

那个合成串就是 `ArcanistDiffParser` 一直以来收到的东西，所以它的**字面行为（连怪癖一起）**才是契约：

| 怪癖 | 后果 |
|---|---|
| `explode("\n", …)` 不丢尾部空元素 | `"a\nb\n"` 算 **3** 行，末行渲染成一个空格 |
| 计数硬编码为 `-1,{len} +1,{len}` | 保留了 GNU 会省略的 `,1` |
| 空输入仍 explode 出一个元素 | 得到一行 hunk，而非只有文件头 |

所以两侧都为空时返回的是 `@@ -1,1 +1,1 @@` 加一个空格行，而不是空字符串。**不要「顺手修正」成 GNU 的行为。**

### 4.5 其余对齐点

| 项 | 值 | 理由 |
|---|---|---|
| 文件名默认值 | `/dev/universe` | 对应 PHP 侧 `nonempty()` 的兜底 |
| 时间戳 | 固定 `9999-99-99`（两个 `-L` 都带） | PHP 传的是哨兵而非真实 mtime，好让输出可复现。解析器忽略它，但它的存在与形状属于格式 |
| 替换行的顺序 | `-old` 先于 `+new` | GNU 的顺序，也是 hunk 解析器期望的顺序。Go 侧靠 LCS 回溯时「平局优先 insert」得到 |
| `normalize` | 去掉所有空格与 Tab（**不含换行**） | 对应 `PhabricatorDifferenceEngine::normalizeFile` |
| 行尾 | 全程 `\n` | `\r` 是归属前一行的普通字符，CRLF 文本 diff 出来仍是 CRLF |

### 4.6 保证的边界：格式逐字节一致，对齐的选择不保证

这一条是随机化交叉验证测出来的，手写用例没能覆盖，**写在这里免得下一个人以为承诺更强**。

当一行在文件里重复出现时，可能存在多个**同样最小**的对齐方案。GNU 的选择来自 Myers 算法加它自己的边界平移启发式，本包的 LCS 不复现这套选择，于是两者可能给出不同（但同样最小）的编辑脚本。

在约 2900 组生成输入上实测：

| 指标 | 结果 |
|---|---|
| 与 GNU 逐字节一致 | 91.4% |
| 存在分歧 | 8.6%，**全部**发生在含重复行的输入上 |
| hunk 头不同 | **0 次** |
| 编辑数不同（即产出更差的 diff） | **0 次** |

所以**保证的是**：文件头、hunk 头计数、`\ No newline` 标记的位置——也就是第 4.1 到 4.5 节那些规则——逐字节等同于 GNU；编辑脚本始终最小。**不保证的是**：在对齐有歧义时选中与 GNU 相同的那一个。

这个边界为什么可以接受：会静默造成损害的失败模式是 hunk 头算错，那会让 `DifferentialHunkParser` 把之后每一行都放错位置；而实测 hunk 头 0 次不同。对齐选择不同只是高亮了另一组同样合法的行，行号不受影响。要真正做到全等需要换成 Myers，理由与另一条改进建议重合，见 [`../../docs/findings.md`](../../docs/findings.md) 第 5 条。

### 4.7 改动这条时怎么验证

期望值**不要手写**，从真实二进制抓：

```bash
printf '<old>' > a; printf '<new>' > b
diff -U65535 -L 'a 9999-99-99' -L 'b 9999-99-99' a b
```

守住这条的是四层：

| 层 | 断言 |
|---|---|
| `unified_test.go` 的 11 组格式契约 | 与 GNU 整值比对（另有 4 组 identical 分支照 PHP） |
| `systemdiff_test.go` | 直接调系统 `diff` 交叉验证：尾换行/行数矩阵 100 组要求**全等**，另一组刻意构造歧义输入只要求 hunk 头与编辑数相同 |
| `tests/contract/diff/` 的 9 份固件 | 对 `data.diff` 整值比对 |
| `tests/e2e/diff.sh` | 过一趟真实 HTTP，唯一能验证含反斜杠的标记不被 JSON 转义改坏的一层 |

第二层是这条约定的主力：它每次 `go test` 都真的去问系统 `diff`，所以格式漂移不需要有人记得重跑命令就会被发现。`diff` 不在 PATH 时它 skip 而非失败。

### 4.7 prose diff 的约束宽得多

`PhutilProseDifferenceEngine` 的输出被 Phorge 渲染成 markup，不会再解析回去，所以没有字节级契约。要守的只有一条**无损不变量**：

> `=` 与 `-` 片段拼起来精确还原旧文本，`=` 与 `+` 拼起来精确还原新文本。

引擎里每一处切分都保留分隔符正是为此。丢一个或多一个分隔符，在渲染出的 diff 里看不见，文本只是读起来稍微不对。

---

## 五、Aphlict 线兼容：通知服务的四条约束

**Go 侧**：`go/internal/notification/{admin.go,client.go,config.go}`、`go/internal/contracts/notification.go`
**PHP 侧**：`PhabricatorNotificationServerRef`、`PhabricatorNotificationServersConfigType`、`PhabricatorNotificationClient`
（参考实现见 `phorge-fork/src/applications/notification/`，被替换掉的 Node 实现见 `phorge-fork/support/aphlict/server/`）

替换 Aphlict 与替换 Pygments 的差别，在于 PHP 侧怎么处理失败。高亮失败至少还渲染出一个没有颜色的代码块，是可见的；通知失败什么都不留：

```php
foreach ($servers as $server) {
  try {
    $server->postMessage($data);
    return;
  } catch (Exception $ex) {
    // Just ignore any issues here.
  }
}
```

这就是 `PhabricatorNotificationClient::tryToPostMessage()` 的全部错误处理。**PHP 侧从不读本服务的响应体，也从不上报它的失败**，所以本节的判据不是「PHP 会不会报错」——它不会——而是「破坏之后还有谁能发现」。

按这个判据，下面四条从最容易发现排到最难：

| 约束 | 破坏之后谁会发现 |
|---|---|
| 5.1 双端口 | PHP 侧存配置时就抛异常，当场可见 |
| 5.2 client 口 501 | `testClient()` 抛异常，集群面板报 Connection Error |
| 5.3 admin 不套信封 | 没人报错；集群面板的 Uptime/Clients/Messages 列变成空白或 0 |
| 5.4 不能用 binder | **没有任何一处发现。**请求答 200、fingerprint 合法、计数照常增长，只有消息内容被揉碎 |

5.4 甚至连上面那个 catch 都用不上——它没有异常可吞，因为 POST 成功了。

### 5.1 双端口不可合并

`PhabricatorNotificationServersConfigType::validateStoredValue()` 遍历 `notification.servers` 时要求两件事：

- 至少一条未禁用的 `type: "admin"`，至少一条未禁用的 `type: "client"`，缺任一类直接抛异常（第 121-137 行）；
- `"{$host}:{$port}"` 在列表里不得重复（第 109-118 行）。

所以「一个端口同时当 admin 和 client」这种配置 PHP 侧**根本存不下来**：写两条记录会撞 host:port 检查，写一条记录又凑不齐两个 type。这也是 notification 不能像 diff 并进 `gorge-render` 那样共用一个端口的直接原因。

**这是五项约定里唯一会当场报错的一条**，也因此是最不危险的。真正要记住的是它的推论——两个端口的地址是**不对称**的，不能照 render 域「一个地址走到底」的直觉配：

| 端口 | 谁来连 | `host` 填什么 |
|---|---|---|
| admin `:22281` | phorge 容器里的 PHP | compose 内网服务名 |
| client `:22280` | 用户浏览器里的 `JX.Aphlict` | **浏览器可达的外部地址** |

client 那条填错不算破坏约定，但和 5.4 一样属于「服务端观察不到」的那一类：`getWebsocketURI()` 是把这个地址**发给浏览器**的，所以填成只在内网解析得开的服务名之后，服务端一切正常、`testClient()` 通过、集群面板双绿，只有每个真实用户连不上。

走 Traefik 之类反向代理时，client 条目改填 `path: "/ws/"` + 443 + https。注意 `path` **只对 client 类型合法**，给 admin 条目加 `path` 会被上面那个校验单独拒掉（第 95-104 行）。

### 5.2 client 端口的 `GET /` 必须回 501，响应体逐字节

`PhabricatorNotificationServerRef::testClient()`（第 181-203 行）把 501 当健康信号，把 200 当故障：

```php
try {
  id(new HTTPSFuture($server_uri))
    ->setTimeout(2)
    ->resolvex();
} catch (HTTPFutureHTTPResponseStatus $ex) {
  // This is what we expect when things are working correctly.
  if ($ex->getStatusCode() == 501) {
    return true;
  }
  throw $ex;
}

throw new Exception(
  pht('Got HTTP 200, but expected HTTP 501 (WebSocket Upgrade)!'));
```

响应体也照抄：逐字节 `HTTP/501 Use Websockets\n`，末尾那个换行也在内（Aphlict 的 `AphlictClientServer.js:78` 原文）。PHP 目前不读这个 body，`tests/contract/notification/client/` 的固件按原文断言它，因为它是「这个端口还在讲 Aphlict 的话」唯一可见的证据，而将来客户端 JS 去读它的成本是零。

**这条与平台层正面冲突，所以平台层为它长了一个字段。** `health.Register()` 本来无条件注册 `e.GET("/", Live())` 返回 200；`httpx.Config.SkipRootProbe` 为 true 时跳过这一条，把根路径让给域包。**全仓库只有 notification 的 client 端口设它**，理由写在 `health.go` 的注释里。摘掉这个字段、或者「为了一致性」把根探针加回这个端口，Phorge 会报 `Got HTTP 200, but expected HTTP 501`——这一条至少会报错，因为它走的是 `testClient()` 而不是 `postMessage()`。

`/healthz` 与 `/readyz` 照样注册，所以容器探针不受影响。豁免只挑根路径，不是整包跳过。

### 5.3 admin 的成功响应不套信封（错误响应可以）

两个 admin 端点的**成功**响应刻意不套 `{data,error}`：

- `POST /` → 裸 `{"fingerprint":"..."}`
- `GET /status/` → 带点号键的扁平 map

PHP 侧的读法是 `phutil_json_decode($body)` 之后**直接索引**（`PhabricatorConfigClusterNotificationsController`）：

```php
$clients = pht(
  '%s Active / %s Total',
  new PhutilNumber(idx($details, 'clients.active')),
  new PhutilNumber(idx($details, 'clients.total')));
```

两个推论：

- **键里的点是字面量，不是嵌套约定。** 改成 `{"clients":{"active":…}}` 之后 `idx()` 全部落空，面板显示 0 或空白，不报错。`contracts/notification.go` 的 json tag 就是这些带点的字面串。
- **套上信封同样是「取不到」而不是「取错」。** 每个字段都退到 `data` 下面，`idx($details, 'version')` 返回 null，面板显示一个后面什么都没有的 "Version"。

所以这两个 handler 用 `c.JSON()` 而**不是** `httpx.OK()`。`admin_test.go` 的 `decodeBare()` 在每条成功响应上断言「恰好一个 JSON 文档，且顶层没有 `data` / `error` 键」。

**错误路径可以走信封，这不是不一致。** PHP 用 `resolvex()`，它在非 2xx 上抛 `HTTPFutureHTTPResponseStatus` 而**从不解析响应体**。反过来说也成立：**不要指望用响应体给 PHP 侧传递失败原因**，那个字段没有读者。

还有一处细节：`history.age` 在 history 为空时必须是 `null` 而不是 0，所以 `contracts.AphlictStatus.HistoryAge` 是指针。PHP 只在 `idx($details, 'history.size')` 为真时才读它，所以这一条当前无害；保留它是为了不必将来再考古一次 Aphlict 的行为（`AphlictAdminServer.js:139` 也是 `var history_age = null;`）。

### 5.4 admin handler 不能用 Echo 的 binder

**这一条是实现阶段才发现的，也正是这份文件存在的理由。**

Phorge 发消息走 `HTTPSFuture`，body 是 `phutil_json_encode($data)` 出来的裸 JSON 字符串：

```php
$server_uri = $this->getURI('/');
$payload = phutil_json_encode($data);

$this->newFuture($server_uri, $payload)
  ->setMethod('POST')
  ->resolvex();
```

而这个请求到达时带的 `Content-Type` 是 curl 给字符串 body 贴的默认值 **`application/x-www-form-urlencoded`**（`HTTPSFuture` 在 arcanist 里、不在本仓库，所以这一条是从实际请求上观察到的，不是从代码读出来的）。

Echo 的 `c.Bind()` 按 `Content-Type` 分派，而它对这个头**不报错**：`DefaultBinder.BindBody` 的 `case MIMEApplicationForm` 分支照字面意思去做表单解析（`bind.go` 第 105-112 行），而 `hub.Message` 是 `map[string]any`，正好落在 `bindData` 支持的那几种 map 目标里（第 169-190 行）。所以 admin 的 `POST /` 必须自己解 body：

```go
var msg hub.Message
if err := json.NewDecoder(c.Request().Body).Decode(&msg); err != nil {
	return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
}
```

**用 `c.Bind()` 的后果比「被拒绝」更糟：请求成功。**实测（Echo v4.15.4）把 `{"type":"notification"}` 贴上这个头交给 `c.Bind`，表单解析把整段 JSON 当成一个没有 `=` 的键，得到

```
map[string]any{"{\"type\":\"notification\"}": ""}
```

一个键、值为空串，`msg["type"]` 是 nil。handler 拿着这坨东西照常往下走：`AddFingerprint` 看不到 `touched` 于是判定「消息是新的」，`Publish` 把它塞进 history 并按「没有 subscribers」当广播扇出，最后 `c.JSON` 答一个**完全合法的 200 加真 fingerprint**。

于是**根本没有异常给 5.1 上面那个 catch 吞**——PHP 侧的 `postMessage()` 顺利返回，它以为消息发出去了。症状是通知内容被静默揉碎：浏览器要么收到一条没有 `type` 的垃圾消息，要么因为原本的 `subscribers` 已经丢失而收到本不该收到的广播。而 Config → Cluster → Notification 页面**两台服务器全绿**（那个页面走 `/status/` 与 `testClient()`，都不经过这个 handler），`messages.in` 照常增长，**本服务日志里连一条 4xx 都没有**。

这是本文件所有约束里最彻底的一条：其余几条至少在某处留下一个错误状态码，这一条什么都不留。

**415 确实存在，但不在这条路上。** `BindBody` 的 `default:` 分支返回 `ErrUnsupportedMediaType`，命中它的是 Echo **不认识**的 mediatype——包括**空** `Content-Type`（第 82-84 行按 `;` 切完之后 mediatype 为空串）。Phorge 从不发空头，所以真实流量永远走不到 415。别照着 415 去找这个问题。

守它的断言有三处。`c.Bind()` 破坏这条约束有**两条**路——payload 里带非法百分号转义的，在进 handler 之前就被拒成 400；不带的，答 200 而把 body 揉成垃圾键——下面这两处各只挡住其中一条（两条都挡的第三处见 5.5 末尾）。把它们各自守住多少记清楚很要紧，因为记强了比不记更坏：

- **契约固件 `tests/contract/notification/admin/post-form-content-type.json` 挡的是「被拒」那条。** 它的 payload 里带 `100% done`，而 `% d` 对表单解析器是个非法的百分号转义，于是换成 `c.Bind` 之后请求被拒成 400 `invalid URL escape "% d"`，固件的 `status: 200` 当场失败。**teeth 在 payload 的字节上，不在断言上**：断言只有「200 + 有 fingerprint + 没有信封」，而上面已经说明这三条在消息被揉碎的情况下全部成立，所以另一条路它看不见。改这份固件时别把那个百分号「清理」掉——清理掉它就退化成一个在 `c.Bind` 下照样通过的检查。（同一段里的 `&` 与 `=` 只改变垃圾键的形状、不改变状态码——实测带 `&`、`=` 但不带 `%` 的 payload 在 `c.Bind` 下照样 200。撑住这条断言的只有那个非法转义。）
- **单元测试 `TestContentTypeIsIgnored` 挡的是「被揉碎」那条**，也就是本节开头说的那条什么都不留的路；三处里只有它是直接撞在内容断言上的。它跑 `application/json` / `application/x-www-form-urlencoded` / 空头三种，断言 200 **并且** `history[0]["type"] == "notification"`。换成 `c.Bind` 实测，镜像 Phorge 的那个 form-urlencoded 子测试失败在后一句上：`the message reached the hub mangled: map[touched:[…] {"type":"notification"}:]`——整段 JSON 成了一个垃圾键，`type` 是 nil。**teeth 在「查那一条 history 的内容」而不是「数它有几条」上。** 这一行是后来补的，测试自己的注释写明了理由（"inspected rather than counted"）：被揉碎的 body 一样会在 history 里留下一条，只有内容分得出两者。**别把它改回只数条数**——只数条数的那个旧版本在 `c.Bind` 下只有空头子测试会红（415 ≠ 200），而空头是 Phorge 不会发的形状，照着那个 415 去找问题会找错地方。它挡不住的是另一条：payload 里没有百分号，所以「被拒成 400」那条路它永远走不到；而且它只看到 hub，不保证内容到得了浏览器。

`tests/e2e/notification.sh` 第 2 条场景用的是同一手法（payload 里同样带 `100% done`），所以它也真的挡得住。

顺带：空 body 在这里解出 `io.EOF`，落到 400 `ERR_BAD_REQUEST`。这是 admin 端口唯一走信封的响应。

### 5.5 一处已知偏离，与验证方法

admin 口的 `GET /` 现在回 200 探针响应，Aphlict 回 405（`AphlictAdminServer.js:114`）。`POST /` 与 `GET /` 方法不同、可以共存，所以平台层的根探针在这个端口上留着了。PHP 侧只打 `POST /` 与 `GET /status/`，观察不到，记在 [`../../docs/findings.md`](../../docs/findings.md) 第 11 条。**别把这个处理方式套到 client 口上**——那边的 `GET /` 必须是 501。

对着跑起来的实例验证本节的最短路径：

```bash
# 5.2：必须 501，且 body 逐字节是 "HTTP/501 Use Websockets\n"
curl -i http://127.0.0.1:22280/

# 5.4：贴着 form-urlencoded 头的 JSON 必须被接受。
# payload 里的 "100%" 不是凑数的：% d 对表单解析器是非法转义，所以只有
# 「不看头、直接按 JSON 解」的 handler 才会答 200。
# 换掉这个 payload 会让本条退化成一个永远通过的检查——见 5.4。
curl -s -o /dev/null -w '%{http_code}\n' \
  -X POST -H 'Content-Type: application/x-www-form-urlencoded' \
  -d '{"type":"notification","title":"build 100% done"}' http://127.0.0.1:22281/

# 5.3：顶层必须直接是那些带点的键，没有 data 包裹
curl -s http://127.0.0.1:22281/status/
```

`tests/e2e/notification.sh` 跑的就是这几条。5.1 只能在 PHP 侧验证：起栈之后打开 Config → Cluster → Notification，两台服务器都显示正常——这一步同时验证了 5.2 的 501 与 5.3 的响应形状。

**但 5.4 用 curl 只能验到「没被拒」这一半。**「消息的键有没有原样进 hub」在 HTTP 层看不见：`/status/` 的 `messages.in` 在消息被揉碎的情况下同样会 +1。

而「form-urlencoded 的标签」与「内容原样到达」这两半，跨两个端口一直验到浏览器的那一份由 `client_test.go` 的 `TestMessagePostedToAdminReachesASubscribedBrowser` 守住（同一对断言在 hub 那一层的版本是 5.4 的 `TestContentTypeIsIgnored`）：它的两个 POST 走 `postAsPhorge`（贴的是 Phorge 真发的那个头，不是 `postTo` 写死的 `application/json`，而 `c.Bind` 处理 JSON 是正确的、拿它测等于不设防），payload 里带 `build 100% done`，消息则是从 WebSocket 上**逐字段读回来**的（`key` 与 `title`）。于是 binder 坏掉这条约束的两条路都落在它手上：非法转义被直接拒成 400 `invalid URL escape "% d"`，或者答 200 但把 body 表单解析掉、断言的那两个字段随之消失。这是把 handler 换成 `c.Bind()` 实测过的，失败信息就是前一条。**teeth 在 payload 里那个非法百分号转义与「逐字段读」这两件事上**：把 `postAsPhorge` 换回 `postTo`、把百分号「清理」掉、或者把字段断言简化成 `expectFirstMessage` 那样只看第一条是谁——任何一步都会让这个测试继续通过，而守卫无声消失。

---

## 六、mailer 服务的四条约定

**Go 侧**：`go/internal/mailer/`、`go/internal/contracts/mailer.go`
**PHP 侧**：`PhabricatorMailGorgeAdapter` 与 `PhabricatorGorgeMailerClient`

这一节与前五节的性质略有不同：前五节多是「破坏后静默失效」，本节第 6.2 条**会**产生一个明确的失败，但**失败的方向是反的**——邮件明明发得出去，却被记成永久失败丢掉；或者明明地址写错，却被无限重投。两者都要几天后看邮件统计才发现。

### 6.1 路径与字段名

两条路径是契约，`PhabricatorGorgeMailerClient` 已经在调：

- `POST /api/mailer/send`
- `GET /api/mailer/mailers`

请求体 `{message, mailerKeys}` 与响应 `data` 里的字段名**一律 camelCase，一个都不能改**：`from` / `replyTo` / `to` / `cc` / `subject` / `textBody` / `htmlBody` / `headers` / `attachments`，地址是 `{name, address}`，头是 `{name, value}`，附件是 `{filename, mimeType, data}`，结果是 `{mailerKey, messageId}`。

它们声明在 [`go/internal/contracts/mailer.go`](../../go/internal/contracts/mailer.go)，按契约层的规则，**改一个字段名就是一次兼容性变更**。PHP 侧 `serializeMessage()` 直接按这些键拼数组，改名的表现是那个字段静默变成空值——比如 `htmlBody` 改成 `html`，所有邮件都退化成纯文本版，没有任何一处报错。

### 6.2 `ERR_PERMANENT_FAILURE` 的语义：本域最要紧的一条

这是全仓库唯一一个**改变 PHP 侧行为**而不只是改变它报告内容的错误码。

| 码 | 状态 | PHP 侧的反应 |
|---|---|---|
| `ERR_PERMANENT_FAILURE` | 422 | 抛 `PhabricatorMetaMTAPermanentFailureException`，worker **停止重投**，邮件落 `FAIL` |
| `ERR_SEND_FAILED` | 502 | 普通异常，worker **重新入队** |

两个方向的误判代价不对称，而且都不会有任何一处报错：

- **永久判成临时**：收件人地址写错，Phorge 的 worker 无限重投同一封信。这正是迁入前的实际状态——老代码定义了 `PermanentError` 但七个适配器从不返回它。
- **临时判成永久**：provider 限流或抖动了一下，本可以在下一次投递成功的信被直接丢掉，且在 Phorge 里的状态看起来就像地址写错了。

所以 Go 侧的分类是**保守**的：只有明确描述「这封信」的信号才判永久——SMTP 5xx、provider HTTP 4xx（**429 除外**，限流说的是「现在不行」）、sendmail 的 `EX_NOUSER` / `EX_DATAERR` / `EX_NOHOST` 一族退出码。**任何不认识的信号一律判临时。**

改动分类规则时，先想清楚要往哪个方向错。`tests/contract/mailer/send-permanent-failure.json` 与 `send-temporary-failure.json` 是成对的，缺一条就只守住了一半。

### 6.3 附件的 base64 编码位置

`Attachment.data` 在**这一层**永远是 base64：PHP 侧 `serializeMessage()` 做 `base64_encode($att->getData())`，Go 侧按需解码——SMTP / sendmail / SES / SendGrid / Postmark 原样透传（它们本来就要 base64），只有 Mailgun 解回原始字节（它的 multipart 表单要文件本身）。

把编码挪到任何一侧都会**损坏每一个附件而不改变任何状态码**：少编码一次，JSON 编码器会把二进制字节按 UTF-8 处理并替换掉非法序列；多编码一次，收件人拿到一个装着 base64 文本的文件。

这也是 `gorge-mailer` 把传输层 `BodyLimit` 显式设成 `10M` 的原因（平台默认 `2M`）：base64 让附件在请求体里比它本身大约三分之一。

### 6.4 配置形态：只走 `cluster.mailers` 的 `options`

端点与 token 只放在 Phorge `cluster.mailers` 条目的 `options` 里，**不新增任何全局 config key**。

这消掉的是老 `phorge` 里一个真实的坑：老实现要求两层配置（一个全局 `go-mailer.url`，外加 `cluster.mailers` 里的条目），而 entrypoint 只写了其中一层，表现为「明明设了 URL，却依然一封信都不发」。

配套的两个后果，改 PHP 侧时都不能省：`cluster.mailers` 是一个**共享列表**（用户可能手工配了 postmark 等条目），下发时必须**合并而非整体重写**；生成的条目要显式带 `"inbound": false` 与 `"media": ["email"]`——gorge-mailer 只做出站，而 `inbound` 的默认值是 `true`，适配器侧没有覆盖它的钩子。

### 6.5 顺带记一条不属于契约但会被误读的事

`/readyz` 返回 503 的含义是「一个后端都没配」，**不是**「SMTP 连不上」。Go 侧刻意不拨测第三方：那会让就绪状态随外部抖动翻转，而多后端 failover 本来就是为此存在的。所以 PHP 侧的 setup check 把 `/readyz` 失败单独报出来是对的——它精确对应「服务活着但一个后端都没配」这个最容易踩的状态——但不要据此推断「就绪 = 下一封信会到」。

---

## 七、search 的字段名与分析器链：五条约定

**Go 侧**：`go/internal/search/http.go`（七条路由）、`go/internal/search/esquery/builder.go`（16 个四字符常量与 `cjk` 子字段名）、`go/internal/search/engine/backend.go`（默认索引名）、`go/internal/search/engine/elasticsearch/backend.go`（`buildIndexConfig()` 与 `buildSearchSpec()`）、`go/internal/contracts/search.go`
**PHP 侧**：`PhabricatorGorgeFulltextStorageEngine`、`PhabricatorGorgeSearchClient`、`PhabricatorSearchDocumentFieldType`、`PhabricatorSearchRelationship`
（参考实现见 `phorge-fork/src/applications/search/`）

第六节是唯一一节会改变 PHP 侧**行为**的，本节回到另一个极端：**五条全部是「写得进去、答 200、就是查不到」型。**判据与第五节相同——不是「PHP 会不会报错」，而是「破坏之后还有谁能发现」。

先说清本节五条共同的形状，因为它是这一整节的组织原则：**写入侧与查询侧是两条独立的路径，各自都能独立地完全正常。**一份文档按 `titl` 写进去、一个查询按 `title` 查出来，两侧都合法、都答 200，索引在长大、`/api/search/stats` 的文档数在上涨、集群面板全绿。Elasticsearch 对「查一个不存在的字段」不报错，它只是不匹配；而**没有任何一层会去比对「写进去的键」与「查出来的键」**。所以本节守的不是某个值「对不对」，是两条路径上的**同一个名字有没有分叉**。

按发现难度从易到难：

| 约束 | 破坏之后谁会发现 |
|---|---|
| 7.1 七条路径 | PHP 客户端拿到 `ERR_NOT_FOUND` 并抛异常，setup check 与 `bin/search` 当场报错 |
| 7.4 默认索引名 `phabricator` | `indexExists()` 答 false，看起来像「索引还没建」——**而按这个读数去 `bin/search init` 会把它变回静默**：新索引建出来了、是空的、没有一处再报错 |
| 7.2 wire 字段名 camelCase | 没人报错。改掉的那个字段静默变成零值，其余字段照常 |
| 7.3 16 个四字符名 | **没有任何一处发现。**写入答 200、索引在长大、检索结果悄悄变空 |
| 7.5 `cjk` 子字段 | 没有任何一处发现，而且**只有中文用户看得见**——英文检索的每一项指标都不动 |

7.3 与 7.5 是本节最安静的两条，也是顶部那张总表点名的两条。它们比 5.4 更难发现的地方在于：5.4 至少让浏览器收到一条形状可疑的消息，而这两条的症状是「搜不到」——**而「搜不到」是搜索功能的一个正常输出**，没有用户会为它提工单。

### 7.1 七条路径

`PhabricatorGorgeFulltextStorageEngine` 按字面调这七条，路径按**域**命名而非按二进制命名（理由同第三节）：

| 方法 | 路径 | PHP 侧调用点 |
|---|---|---|
| POST | `/api/search/index` | `reindexAbstractDocument()` |
| POST | `/api/search/query` | `executeSearch()` |
| POST | `/api/search/init` | `initIndex()` |
| GET | `/api/search/exists` | `indexExists()` |
| GET | `/api/search/stats` | `getIndexStats()` |
| POST | `/api/search/sane` | `indexIsSane()` |
| GET | `/api/search/backends` | `PhabricatorGorgeSearchClient::getBackends()`，**目前没有调用者** |

`TestRoutePathsAreStable` 断言这七条仍注册着。

最后一条要说明白，免得下一个人照着一句好听的注释去推断它的地位：`getBackends()` 定义了，但**PHP 侧没有任何地方调它**。集群面板那一页确实会打本服务，但打的是 `/stats`（`PhabricatorConfigClusterSearchController` → `getEngine()->getIndexStats()`）；后端**那几列**来自 `PhabricatorGorgeSearchHost::getStatusViewColumns()`，而那个方法只读本地的 `cluster.search` 配置，一个 HTTP 请求都不发。所以 `/api/search/backends` 是一条**诊断端点**：它回答的是「跑着的服务自己认为它有哪些后端」，与「配置文件里写了什么」是两个问题——而这是唯一能把两者分开的办法，也正是它值得留着的理由。

这不改变它的契约地位（PHP 侧的方法签名在那儿，删掉路径它就断了），但确实改变了「凭据泄进它会怎样」的推理：后果不是「被打印在一个网页上」，而是「出现在任何一次诊断输出、日志与工单附件里」。后者已经足够，**不需要靠前者那个假前提来加强**——`tests/contract/search/list-backends.json` 的 `jsonAbsent: ["data.0.apiKey"]` 该留着，理由换成真的那个。

（顺带划清一条边界，因为两者容易混：`/stats` 的 `storage_bytes` **确实**被面板渲染成「Storage Used」那一列，见 7.2。「面板读 `/stats`」是真的，「面板读 `/backends`」是假的。）

方法与鉴权的两个细节与 render / mailer 一致，见文末附录：分组鉴权早于路由解析，所以不带 token 打不存在的路径答 401 而非 404；同样在这个分组下方法用错答 404 而非 405。

### 7.2 wire 字段名一律 camelCase，`storage_bytes` 是唯一的例外

字段名声明在 [`go/internal/contracts/search.go`](../../go/internal/contracts/search.go)，按契约层的规则**改一个字段名就是一次兼容性变更**。`PhabricatorGorgeFulltextStorageEngine` 把 `PhabricatorSearchAbstractDocument` 与 `PhabricatorSavedQuery` 直接摊成数组、按这些键拼，所以这些名字**就是线上契约本身**，不是本服务的命名风格：

| 位置 | 字段 |
|---|---|
| 文档 | `phid` / `type` / `title` / `dateCreated` / `dateModified` / `fields` / `relationships` |
| field | `name` / `corpus` / `aux` |
| relationship | `name` / `relatedPHID` / `rtype` / `timestamp` |
| 查询 | `query` / `types` / `authorPHIDs` / `ownerPHIDs` / `subscriberPHIDs` / `projectPHIDs` / `repositoryPHIDs` / `statuses` / `withAnyOwner` / `withUnowned` / `exclude` / `offset` / `limit` |
| 应答 | `phids` / `count` / `exists` / `sane` / `status` / `docTypes` |

改名的表现是那个字段**静默变成零值**，其余字段照常工作。这比整条请求失败更难查，因为症状是局部的：把 `relatedPHID` 改成 `phid`，文档照常入索引、只是所有关系都空了，于是「按作者筛」要等到下一次全量重建之后才开始返回空——而那时改动已经过去很久。两个布尔值尤其容易被当成冗余而丢掉：`withAnyOwner` 与 `withUnowned` 是 Phorge 的查询 UI 三个所有者状态里的后两个（「任何人拥有」与「没有人拥有」），它们在索引里表达为 `ownr` 关系的**存在与不存在**，而不是一个 PHID 列表。所以它们既不能从 `ownerPHIDs` 推出来，丢掉之后也不报错——**两者都静默退化成「不筛选」，也就是「全部」**，而「全部」是一个看起来完全合理的结果集。

**`storage_bytes` 是本服务线上唯一一个 snake_case 名字，这是刻意保留的。**它早于 monorepo，PHP 侧按这个拼法读。改成 `storageBytes` 的表现是集群面板的存储列变空——而 `IndexStats` 是一张**开放 map**，多一个键少一个键都不是错误，所以这一次改名连一个类型错误都产生不了。**不要「顺手统一」它。**（`tests/e2e/search.sh` 第 16 条断言响应里不出现 `storageBytes`，这是唯一一处会当场拦下这次「统一」的地方。）

### 7.3 16 个四字符名必须与 PHP 常量逐字符相等

`titl` / `body` / `cmnt` / `full` / `core`，以及 `auth` / `book` / `revw` / `subs` / `comm` / `ownr` / `proj` / `repo` / `open` / `clos` / `unow`——5 个字段名加 11 个关系名，一共 16 个。**这些不是本服务选的缩写风格，是 `PhabricatorSearchDocumentFieldType` 与 `PhabricatorSearchRelationship` 的常量值**，而 `buildDocSpec()` 把它们当作顶层键原样写进索引，所以**它们就是索引里的键**。

16 个全部集中在 `esquery/builder.go` 一处。`TestNamesMatchThePHPConstants` 把每一个都对着 PHP 常量名钉住，`TestTheListsCoverEveryConstant` 另外断言每个名字**恰好四个字符**且互不重复。

改一个值而不改 PHP 常量，就是本节开头那个形状的最纯粹版本：写入侧按新拼法写、Phorge 的查询侧按常量拼法查，**两侧都答 200，交集是空集**。既有索引里的存量文档一并变得查不到，而没有任何一层会说出这件事。

三条从这一条推出来的、同样属于契约的东西：

- **最后三个关系名不是指向另一个对象的链接，是文档自己的状态标记。**`open` / `clos` 说它是开着还是关了，`unow` 说它没有所有者。所以「查开着的文档」在查询侧是一次 `exists` 检查而不是 term 匹配——这个区别本身就是契约，写成 term 匹配需要一个值，而这些键在索引里没有值可匹配。
- **带时间戳的关系额外写一个 `<name>_ts` 键**，`open` 与 `clos` 靠它携带状态改变的时刻。这个键名是从关系名拼出来的，所以它随 7.3 一起漂移；而它比本节其余部分更安静一档，因为**本服务从不读回它**——查询侧一处都没用到它（无查询文本时排序用的是 `dateCreated`）。所以这一处漂移在今天不产生任何可观测的差别，只会在将来某个真的去读它的东西上浮出来。写着它是为了不必将来再考古一次；别因为「没人用」就把它删掉。
- **`AllFields()` 与 `AllRelationships()` 这两个列表也是契约**，理由与名字本身不同：mapping 是从它们生成的，**漏一项就是那个字段永远没有 mapping**，写进去的文档按集群 dynamic mapping 猜出来的类型入索引。这同样不报错。

### 7.4 默认索引名 `phabricator`

`engine.DefaultIndexName`，与 Phorge 自带的 `PhabricatorElasticFulltextStorageEngine` 用的名字相同。这条的价值是**存量**：一个已经在跑 Elasticsearch 的 Phorge 装置改配 `type: gorge` 之后指向同一份数据，不需要搬索引。

它是本节唯一一条破坏之后**会先给出一个信号**的：换掉默认值，`indexExists()` 在新名字上答 false。但这个信号的标准处置方式会把它变回静默——「索引不存在」的反应是 `bin/search init`，而那会**成功地**建出一个空索引，旧索引带着全部数据留在原地、没有一处再提到它。之后每一次检索都答 200 加空列表，直到有人跑完一次 `bin/search index --all --force`。

所以这条要记的不是「别改索引名」——按部署需要改是合理的，`deploy/compose/.env.example` 里写了怎么改——而是**改名不是一次配置调整，是一次数据迁移**，它的收尾是一次全量重建。

### 7.5 `cjk` 子字段：mapping 里要有它，查询侧要点它的名

这是迁入时补上的能力，也是本节最安静的一条。它有**两半**，各自都能独立地静默失效。

**上半：mapping。**`buildIndexConfig()` 给 `titl` / `body` / `cmnt` 三个语料字段各挂一个 `cjk` 子字段（与既有的 `raw` / `keywords` / `stems` 并列），分析器是 `cjk_text`：

```go
filterCJKBigram: map[string]any{"type": "cjk_bigram", "output_unigrams": true},

analyzerCJKText: map[string]any{
	"tokenizer": "standard",
	"filter":    []string{"cjk_width", "lowercase", filterCJKBigram},
},
```

`cjk_width` 与 `cjk_bigram` 都是 Elasticsearch **内置**的 filter，这条链不需要 `analysis-icu`、也不需要 `smartcn`。它存在的理由是另外三条链（`english_exact` / `letter_stop` / `english_stem`）全是英文链，而它们对 CJK 失效的方向恰好相反：`letter_stop` 背后的 `letter` tokenizer 把一整串汉字当成**一个不可分的 token**，所以「跳转」什么都匹配不上；另两条背后的 `standard` tokenizer 把它**碎成单字**，所以「跳转」匹配每一份含「跳」或「转」的文档。一个太严一个太松，**而两者都答 200**。`output_unigrams: true` 是这条链上唯一一个非默认选项，它保留单字，否则二元切分会让「猫」在一份明写着这个字的文档里一无所获。

**下半：查询侧的点名。**`buildSearchSpec()` 为它**另起一条 should 子句**，而不是往既有那条 `english_exact` 子句上加字段：

```go
bq.AddShould(map[string]any{
	"simple_query_string": map[string]any{
		"query": q.Query,
		"fields": []string{
			esquery.FieldTitle + "." + esquery.SubfieldCJK + "^4",
			esquery.FieldBody + "." + esquery.SubfieldCJK + "^3",
			esquery.FieldComment + "." + esquery.SubfieldCJK + "^1.2",
		},
		"analyzer":         analyzerCJKText,
		"default_operator": "and",
	},
})
```

另起一条的理由是**分析器是子句的属性，不是字段的属性**：用 `english_exact` 去打一个按二元组建索引的字段，中文被切成单字，评分基本等于随机。

**这两半的失效方式不同，要分开记，因为它们的症状差一个量级：**

- **摘掉 mapping 里的子字段**：中文检索退化成三条英文链凑巧能匹配到的东西。每条路径照常 200，索引照常增长，只有中文结果悄悄变空或变成噪声。
- **保留子字段、把那条 should 子句删掉**：`titl.*` 那条 must 子句仍然通过通配符**覆盖到** `cjk` 子字段，所以中文还搜得到——**丢掉的是排序**。这是本节最容易被误判的一处：它看起来「还能用」，所以最可能被当成冗余删掉，而它的症状是中文语料上的相关度排序崩掉，没有任何一项指标会动。
- **反过来那个方向一样安静**：一个在 mapping 里存在、而查询侧一处都没点到名的子字段是一次**彻底的空操作**——索引为它多占空间、多花索引时间，检索结果一个字节都不变。所以「加了子字段」与「加了子字段并且它真的在被查」是两件事，只有后者有效果，而两者在任何一处观测上都长得一样。

**顺带一条会报错的：改分析器链强制全量重建索引。**`IndexIsSane()` 拿 `configDeepMatch(actual, b.buildIndexConfig(docTypes))` 比对线上 mapping 与本服务**今天**会建出的配置，所以只要 `buildIndexConfig()` 变了——加一个子字段、动一个 filter 的顺序都算——**所有既有索引立刻报 not sane**，必须 `bin/search init` 加 `bin/search index --all --force` 重建。

**这一条与本节其余部分正好相反：它明确报 false，不是静默失效。**本文件一贯区分这两类，所以要写明白：它是可接受的迁移代价而不是缺陷，`indexIsSane()` 的存在正是为了让这类改动有一个可报告的信号。代价是大库上 `index --all --force` 属于小时级操作、期间检索结果不完整，所以它必须写进 `DOCKER.md` 与模块文档，登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 18 条。

（`bin/search ngrams` 在本引擎下**不适用**。它是 Ferret（MySQL）专属路径，`PhabricatorSearchNgrams` 与 `PhabricatorFerretEngine` 全在 MySQL 侧。旧 `phorge/DOCKER.md` 里那套「跑 ngrams 启用中文搜索」的说法在这个引擎下是误导，不要照抄。）

### 改动本节任何一条之后怎么验证

7.1 与 7.2 靠固件：`tests/contract/search/` 的 17 份加 `unavailable/` 的 6 份，路径、字段名与五个域级错误码都在其中。7.3 靠 `esquery/builder_test.go` 那两个测试，它们是这 16 个名字唯一的守卫——**别把那张对照表简化成一个字符串列表**，表里的 `phpConstant` 列是它的全部价值，它让「改了值」在失败信息里直接指向要同步改的那个 PHP 常量。

7.5 的两半各有守卫，都在 `engine/elasticsearch/backend_test.go`：断言 mapping 里三个语料字段都带 `cjk` 子字段且分析器是 `cjk_text`（上半）、断言第二条 should 子句的 `analyzer` 与三个 `*.cjk` 字段都在（下半）。另有两条断言「删掉子字段的索引必须报 not sane」与「删掉分析器的索引必须报 not sane」——**这两条守的是 `configDeepMatch` 本身不被削弱**，因为削弱它是让一次强制重建「消失」的最省事的办法。

对着跑起来的实例，最短路径是拿一份中文文档走一圈：

```bash
curl -s -X POST -H 'X-Service-Token: dev-token' \
  -d '{"phid":"PHID-TASK-cjk","type":"TASK","title":"登录跳转丢失查询参数",
       "fields":[{"name":"titl","corpus":"登录跳转丢失查询参数"}]}' \
  http://127.0.0.1:8120/api/search/index

# 两字查询必须命中（这是 cjk 子字段唯一的存在理由）
curl -s -X POST -H 'X-Service-Token: dev-token' \
  -d '{"query":"跳转"}' http://127.0.0.1:8120/api/search/query
```

`tests/e2e/search.sh` 第 11、12 两条场景跑的就是这一圈。**但它只在真的 Elasticsearch 上有意义**：内存 `test` 后端做的是大小写不敏感的子串扫描、根本不过分析器，所以它会把这两条**答对**而什么都没证明——脚本因此在识别出 `"type":"test"` 之后把它们 **skip 而不是 pass**，理由写在脚本第 11 条的注释里。**别把那个 skip 改成 pass**：本节最安静的一条会因此获得一个看起来像覆盖的守卫。

---

## 附：鉴权与响应信封

三个 PHP 客户端（Render / Mailer / Search，共同的请求构建与信封解析已抽到 `PhabricatorGorgeServiceClient` 基类）依赖以下两点，改动会直接打断 PHP 侧：

**鉴权**：请求头 `X-Service-Token` 优先，查询参数 `?token=` 兜底；服务端 token 配置为空时全部放行。PHP 客户端走的是请求头。

**响应信封**：`/api/**` 返回 `{data, error}`，`data` 与 `error` 恰有一个非空。PHP 客户端先检查 `$envelope['error']`，非空则抛异常（异常消息里带 `error.code`），否则返回 `$envelope['data']`。

这条对没进到 handler 就失败的请求同样成立。`go/internal/platform/httpx/errors.go` 用 `e.HTTPErrorHandler` 顶掉了 Echo 的默认错误处理器，所以路由不匹配、请求体超过传输上限、handler panic 被 `Recover` 兜住这几种情况，PHP 客户端拿到的仍是信封，而不是 Echo 默认的 `{"message": "..."}`——后者会让客户端既读不到 `error` 也读不到 `data`，退化成一句语焉不详的解析失败。**别把这个处理器摘掉，也别在 `httpx.New()` 之外另建 Echo 实例。**

唯一不带信封的错误响应是 `HEAD` 请求：协议不允许带响应体，只有状态码。PHP 客户端只发 POST/GET，不受影响。

平台级错误码六个：

| 码 | 状态 | 出现场景 |
|---|---|---|
| `ERR_BAD_REQUEST` | 400 | 请求体不是合法 JSON |
| `ERR_UNAUTHORIZED` | 401 | token 缺失或不匹配 |
| `ERR_NOT_FOUND` | 404 | 没有路由匹配 |
| `ERR_METHOD_NOT_ALLOWED` | 405 | 路径存在但不接受该方法 |
| `ERR_TOO_LARGE` | 413 | 请求体超限 |
| `ERR_INTERNAL` | 500 | panic 或其他非预期失败 |

`ERR_NOT_FOUND` 对 PHP 侧最有诊断价值：`gorge.render.uri` 尾部多一个斜杠、或 base URL 拼接出双斜杠时，拿到的就是它；`cluster.search` 条目的 host/port 拼错时同理。两个路由细节别误判（对 `/api/highlight/**`、`/api/diff/**`、`/api/mailer/**`、`/api/search/**` 四个分组都成立）：分组的鉴权早于路由解析，不带 token 打不存在的路径返回 401 而不是 404；同样在这些分组下方法用错返回 404 而不是 405（分组为了鉴权匹配了所有方法），所以 `ERR_METHOD_NOT_ALLOWED` 实际只在健康探针路径上见得到。

`ERR_TOO_LARGE` 有**三个**来源，同码是刻意的，PHP 客户端按码分支即可，不需要知道是哪一道：

| 来源 | 阈值 | 检查点 |
|---|---|---|
| 域级字节数 | `GORGE_RENDER_MAX_BYTES` / `GORGE_DIFF_MAX_BYTES`（各默认 1MiB） | handler 内 |
| 传输层 body | 固定 2M | `httpx` 中间件 |
| LCS 表单元数 | 4,000,000（编译期常量，仅 diff 域） | 切完行之后 |

默认配置下只会命中第一道；把域级上限调到 2M 以上就会改走第二道。第三道不能被前两道替代：LCS 表分配的是 `n*m` 而非行数，2001 行对 2001 行只有几十 KB 却要一张四百万单元的表，而 100 行对 100000 行反而便宜、必须放过。

diff 域的字节检查算的是 **`len(old) + len(new)` 之和**，不是任一侧——两个 600 KiB 的文件加起来就超限了。

`ERR_INTERNAL` 的 `message` 恒为一句通用文案，panic 值与堆栈只进 `slog` 日志。**排查 500 要看服务日志，不要指望响应体。**

域级错误码有八个，都是迁移前就有、Phorge 侧已经在用的码，故未收敛进平台码：render 域的 `ERR_HIGHLIGHT_FAILED`(500)，mailer 域的 `ERR_PERMANENT_FAILURE`(422) 与 `ERR_SEND_FAILED`(502)，以及 search 域的 `ERR_INDEX_FAILED` / `ERR_SEARCH_FAILED` / `ERR_INIT_FAILED` / `ERR_CHECK_FAILED` / `ERR_STATS_FAILED`（均 502）。全局错误处理器不会覆盖它们——`httpx.Fail` 一写响应就 committed，处理器见到 `Committed` 就不再落笔。

mailer 那两个的区别不是文案而是**行为**，见第六节 6.2；另外 mailer 域的后端失败一律落在 422 或 502，**不落 500**——那里的 500 只意味着服务自己出了问题。search 域的五个同理：全部 502，500 在那个域只意味着服务自己坏了。

**但 search 那五个目前在 PHP 侧没有消费者。**`PhabricatorGorgeSearchClient` 没有覆盖 `newServiceErrorException()`（`PhabricatorGorgeMailerClient` 覆盖了，因为 6.2 那两个码必须分道），所以十一个码全部塌成同一个通用异常，码本身只作为文本活在异常消息里。这是当前**刻意保留**的行为，登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 21 条——记录事实，不是提议改 Go 侧。五个码分开的价值在 `bin/search` 与运维读日志时兑现，不在 PHP 的异常分支上。

**diff 域刻意没有域级错误码。**两个引擎都没有可报告的失败模式：prose 引擎是全函数，unified 引擎唯一会拒绝的是过大的输入，而那已经是 `ERR_TOO_LARGE` 了。在那里造一个码，它永远不会被返回。新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。

**空 source 不是错误**：`{"source": ""}` 返回 200 与空 `html`，不返回 400。Phorge 渲染空文件时依赖这个行为。

**健康探针不套信封**：`GET /`、`GET /healthz`、`GET /readyz` 返回裸 `{"status":"ok"}`。这是给容器探针和负载均衡用的，不要「顺手统一」成信封格式。

本附录讲的是 `/api/**`，即 render、diff、mailer 与 search 四个域——四者的鉴权与信封口径完全一致，只有 `ERR_TOO_LARGE` 的来源不同：mailer 的传输层上限是 `10M` 而非 `2M`（base64 让附件涨三分之一），且它没有域级字节检查，正文超限是静默截断而不是拒绝；search 用平台默认的 `2M`，也没有域级字节检查——一份文档多大是 Phorge 的事，而语料大到成问题时那是存储的配置问题，不是线上的。

**notification 的两个端口不在这个范围内**：它们不鉴权、成功响应不套信封、client 口的 `GET /` 连探针都不是。要改那两个端口先看第五节，不要照这一节的口径推。
