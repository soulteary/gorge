# Phorge 兼容契约

本文件记录 Gorge 的 Go 服务与 Phorge PHP 端之间**不能随意改动**的十项约定。这些约束此前只以注释形式散落在代码里，而它们的共同特征是：**破坏之后不会有任何报错**。（第九项是这句话第一次要打折扣的地方，理由见那一节开头——它的一半约束的对手不是 Phorge，而是一个按 Phorge 原本的投递写好的第三方接收端。）

| 约定 | 破坏后的表现 |
|---|---|
| 一、语言别名表 | 两个高亮后端把同一语言解析到不同 lexer，无断言捕捉 |
| 二、Chroma formatter 配置 | 全站高亮静默失效——页面正常渲染，只是没有颜色 |
| 三、端口与路由 | PHP 侧配置指错地方，表现为 `ERR_NOT_FOUND` |
| 四、unified diff 输出格式 | 解析器接受错误的 hunk 头，然后**静默地把之后每一行都放错位置**（第 4.6 节写明了保证到哪里为止） |
| 五、Aphlict 线兼容（通知） | 四条子约束，最坏的一条（5.4）**连错误状态码都不产生**：请求答 200、fingerprint 合法、`messages.in` 照常增长，只有消息内容被静默揉碎 |
| 六、mailer 的错误码与字段名 | 唯一一项会**改变 PHP 侧行为**的约定：`ERR_PERMANENT_FAILURE` 决定 worker 要不要重投这封信，两个方向的误判分别是「无限重投」与「静默丢信」，都要几天后看邮件统计才发现 |
| 七、search 的字段名与分析器链 | 五条子约束，全部是「写得进去、答 200、就是查不到」型。7.3 的 4 字符字段名与 7.5 的 `cjk` 子字段是其中最安静的两条：索引照常增长、每条路径照常 200，只有检索结果悄悄变空 |
| 八、file storage 的 handle 与 engine identifier | **既有文件变得读不出来，而且是从改动生效那一刻起、对全部存量文件同时发生**：新写入的文件一切正常，所以问题会在很久以后才以「某些旧附件 404」的形式露头。另有一条不同性质的子约束（8.7）：`/readyz` 的判据会让整个栈在首次启动时**死锁**——而且实测表明，遵守「不查表」这条约定**仍然不够**，DSN 里那个库名足以独立触发它 |
| 九、webhook 投递的字节与回写字段 | 分成性质相反的两半。出站那半（9.1 payload 的 2 空格缩进与末尾换行、9.2 签名头）**会**报错，只是错误在**别人的服务器上**——签名算的是整个字符串含末尾换行，所以改缩进就是改签名，而你这一侧只看到一批 4xx；回写那半完全静默，其中 9.4 的 `status` 取值域还是 Go 侧整个抢占机制的地基。另有一条独一份的（9.7）：`gorge.webhook.uri` 是**接管开关**而非服务地址，漏配的表现不是失效而是**每个 webhook 发两次**，且接收方分不出它与真正的重复事件 |
| 十、task queue 与 worker 的字段名与租约语义 | 整节静默型。`taskClass`/`dataID`/`leaseOwner`/`failureCount` 直接映射 `worker_activetask` 列名，改错一个只让 PHP 侧读到空值；`leaseExpires` 与 `(yield)` 哨兵是抢占地基，写坏就让任务被两个 worker 同时取走；必须替换 `phd` 而非并存，否则每个任务跑两遍 |
| 十一、db-api 的字段名、错误码与库/表名 | 整节静默型。`refKey`/`isFatal`/`connectionStatus` 等是 PHP 侧 `PhabricatorDatabaseRef` / `DatabaseSetupCheck` / `MySQLSetupCheck` 直接按键读的契约，改错一个只让对方读到空值——其中 `isFatal` 决定 Phorge 把一个 setup issue 当阻断还是当告警；库名 `{namespace}_meta_data`、表名 `patch_status`/`hoststate` 都是 Phorge 的，不能顺手现代化。另有一条编排约束（第 8.7 节的逐字翻版）：`/readyz` 若查表或让 DSN 带库名，会让首启死锁 |

第四项是其中最隐蔽的：它没有「失效」这个状态，只有「悄悄错位」。第五项走得更远：5.4 破坏之后**没有任何一处产生错误**——不是「错误被 PHP 吞掉」，是压根没有错误可吞，因为那个 POST 成功了。第六项的性质又不一样：它**会**产生一个明确的失败状态，只是方向是反的，所以看日志找不出问题——每条记录看起来都合理。第七项则是把「静默」推到了另一个维度：破坏之后**写入侧一切正常**，索引在长大、统计在增加、集群面板全绿，错的只是「写进去的键」与「查出来的键」对不上，而没有任何一层会去比对这两者。第八项的时间尺度是独一份的：它破坏的是**存量数据的可达性**，而验证一次改动是否安全的常规办法（写一个文件、读回来、通过）恰恰完全看不见它——新旧两条路都自洽，只是不再互通。第九项换的是另一个维度：**它的一半约束的对手不在这个系统里。**payload 的字节与签名头是给第三方接收端看的，而那些接收端是按 Phorge 原本的投递写好的、并不知道换了实现；所以这一半破坏之后**会**报错，只是错误发生在别人的服务器上，你这一侧看到的是一批 4xx，而 Herald 界面上它和「接收端自己坏了」没有任何区别。它还带着全仓库唯一一处失配方向是反的东西（9.7）：漏配接管开关的表现不是「配了不生效」，而是每个 webhook 发两次。

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

### 4.8 prose diff 的约束宽得多

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
## 八、file storage 的 handle 与 engine identifier

**Go 侧**：`go/internal/filestorage/`（`localdisk.go` / `mysqlblob.go` / `s3.go` / `http.go`）、`go/internal/contracts/filestorage.go`
**PHP 侧**：`PhabricatorGorgeFileStorageEngine` 与 `PhabricatorGorgeFileStorageClient`

这一节与前七节的性质都不同，因为它约束的**不是一次调用，是存量数据的可达性**。

Phorge 对每一个文件只存一对 `(engine, handle)`。本服务不持有任何元数据，也没有第二条线索可以回退——**engine 字符串和 handle 的解读方式就是全部**。任何一处改动都不会让当前的读写失败：新写进去的文件用新规则写、用新规则读，自洽得很；只有那些用旧规则写下的存量文件，从改动生效那一刻起同时变得不可达。所以「写一个文件、读回来、通过」这个最自然的验证动作，对本节的每一条都**完全没有分辨能力**。

四条路径本身（`POST` / `GET` / `DELETE /api/file/blob` 与 `GET /api/file/engines`）与端口 `:8100` 当然也是契约，`PhabricatorGorgeFileStorageClient` 按字面调它们、`TestRoutePathsAreStable` 钉着它们；但那一条破坏后会答 `ERR_NOT_FOUND`，属于第三节那种「配置指错地方」的可见故障，不是本节要防的东西。

### 8.1 三个 engine identifier 字符串

| identifier | 后端 | 优先级 |
|---|---|---|
| `blob` | Phorge `file_storageblob` 表的一行 | 1 |
| `local-disk` | 本地磁盘上的一个文件 | 5 |
| `amazon-s3` | 对象存储里的一个对象 | 100 |

这三个字符串**被写进 Phorge 的数据库**，每个文件一条。它们不是显示名，也不是内部枚举——它们和 **Phorge 自己那几个存储引擎的 identifier 是同一批字符串**，这正是本服务写下的文件能被 Phorge 原生引擎读到、反之亦然的原因。

最容易「顺手修正」的是 `blob`：它看起来该叫 `mysql`，毕竟另外两个都以介质命名。**不要改**——Phorge 的 `PhabricatorMySQLFileStorageEngine` 用的就是 `blob`。

改掉任何一个的表现：Phorge 拿着旧字符串来读，`Router.GetEngine` 找不到，答 400；页面上表现为那一批附件打不开，而新上传的一切正常。

### 8.2 复合 handle `engine/handle`：按**第一个**斜杠切

上一节那三个字符串并不单独占一个字段。Phorge 的 `file` 表对每个文件只有一个 `storageHandle` 列，而读回字节必须同时给出引擎名——服务端不做推断（handle 形态跨后端有重叠，猜错的表现是读出**另一个文件**而不是失败）。于是引擎名只能编进 handle 里：

```
local-disk/ab/cd/0123456789abcdef0123456789ab
blob/12345
amazon-s3/phabricator/ab/cd/0123456789abcdef
```

**这个复合串是 PHP 侧独有的，Go 侧从头到尾没见过它。**服务答的是 `{"engine": …, "handle": …}` 两个字段（`contracts.WriteResult`），拼接与拆解都发生在 `PhabricatorGorgeFileStorageEngine`：`writeFile()` 返回 `$engine.'/'.$handle`，`parseHandle()` 再拆回来交给客户端。所以这一条**没有任何 Go 侧的测试守得住它**，它只存在于 PHP 与库里那些字符串之间。

**必须按第一个斜杠切，不是最后一个，也不是 `explode('/')` 取两段。**三个 identifier 都不含斜杠而 handle 含（8.3），所以第一个斜杠是唯一无歧义的分界。按最后一个切会把 `local-disk/ab/cd/{28 hex}` 拆成引擎 `local-disk/ab/cd` 和 handle `{28 hex}`，`Router.GetEngine` 随即答 `unknown storage engine`；`explode` 取前两段则会把 handle 截成 `ab`。两种写法都能通过「写一个文件再读回来」——因为写和读用的是同一份代码——只有存量文件全数 404。

`parseHandle` 还拒绝 `$slash === 0`，即以斜杠开头的串。这不是洁癖：引擎名为空会让 `GetEngine("")` 去查一个不存在的键，报出来的错说的是「未知引擎 ""」，离真正的原因（handle 在写入时就拼坏了）隔着好几步。同源的是写入侧那道检查——服务只答了 `engine` 或只答了 `handle` 时，`writeFile()` 显式抛错而不是拼出 `local-disk/`，因为那个串**非空**，基类的校验会收下它，于是一个从此读不出来的 handle 被存进库里。

长度上有余量：基类要求 handle ≤255 字符，最长的组合 `amazon-s3/phabricator/{instance}/ab/cd/{16 hex}` 在 instance 名不离谱的前提下不到 60 字符。

### 8.3 三种 handle 形态

| 引擎 | handle | 由谁决定 |
|---|---|---|
| `local-disk` | `ab/cd/{28 hex}` | 与 Phorge 自己的本地磁盘引擎同布局 |
| `blob` | 自增行 id 的十进制串（`"12345"`） | 与 Phorge 自己的 MySQL 引擎同方案 |
| `amazon-s3` | 对象 key，见 8.4 | 与 Phorge 自己的 S3 引擎同布局 |

本地磁盘那条是「同布局」而不是「碰巧相似」：两级 `ab/cd/` 目录扇出加 28 位十六进制文件名，意味着**一个由 Phorge 原生引擎写出来的存储目录，挂给本服务就能直接读**，反过来也成立。改动扇出层数、改动 hex 长度、把分隔符从 `/` 换成别的，都会让这个目录里已有的文件一个都找不到。

**本地磁盘的 handle 格式校验同时还是一道安全边界，这一点必须知道，因为它看起来只是个整洁性检查。** handle 是从查询参数进来的，而 `filepath.Join(root, handle)` 会老老实实把 `../../..` 解析出去。`localHandlePattern` 是唯一挡住「读走/删掉这个进程能打开的任意文件」的东西，所以它在**读和删两条路上都校验**，`TestLocalDiskRejectsBadHandle` 逐个试过 `../../../etc/passwd` 这一类。放宽这个正则（比如为了「支持更长的 handle」）等于同时拆掉一道兼容约束和一道安全边界。

blob 那条的要点是它**必须先被解析成整数再进 SQL**：MySQL 会把非数字字符串强制成 0 然后答「无此行」，于是一个畸形 handle 会报出和「文件真的被删了」一模一样的结果——两者的排查成本差着量级。`parseBlobHandle` 做这件事，`TestMySQLBlobRejectsAMalformedHandle` 守着。

### 8.4 S3 的 key 前缀 `phabricator` 是 Phorge 的，不是装饰

```
phabricator[/{instance}]/ab/cd/{16 hex}
```

`phabricator` 这个前缀是 **Phorge 自己的 S3 引擎用的前缀**（沿用 Phabricator 时期的名字），不是本服务加的命名空间，也不是可以「顺手改成 gorge」的东西。中间那段可选的 `{instance}` 来自 `GORGE_FILE_INSTANCE_NAME`，是多个 Phorge 实例共用一个桶时的隔离段，与 Phorge 的 `storage.s3.bucket` 布局对应。

改掉前缀之后，**桶里每一个对象都原地不动、并且不可达**——没有报错，没有迁移，没有任何一处会提示你旧对象还在那儿。对象存储按量计费，所以它们还会继续产生账单。

`s3_test.go` 顶上的 `s3KeyPattern` 与 `TestS3KeyLayout` 把整个形状（含 `phabricator/` 前缀与 instance 段）钉住了。

### 8.5 MySQL blob 后端与 Phorge 原生引擎**写同一张表**

```
{namespace}_file.file_storageblob
```

这不是「结构相同的另一张表」，是同一张：同一个库、同一张表、同一套「自增 id 即 handle」的方案，与 `PhabricatorMySQLFileStorageEngine` 完全重合。库名由 `GORGE_FILE_NAMESPACE` 拼成 `{namespace}_file`，因为 Phorge 就是这么拼的；`TestFileDSN` 把这条拼法钉住了。

**这是一个必须知道的隐患，不是一个特性。**同时启用两侧的 MySQL 引擎，两边会各自往同一张表里 INSERT、各自拿走一段自增 id。当前不会互相覆盖（自增 id 天然不冲突），但要记住两件事：

- **`bin/storage` 的维护动作与 GC 会碰这些行。**它们是 Phorge 的工具，按 Phorge 的账本行事——本服务写下的行在那本账里没有特殊标记，也不该有。
- **`file_storageblob` 这张表由 Phorge 的 `bin/storage upgrade` 创建**，本服务从不建表。表还不存在的那段时间里，每一次 blob 写入都会失败——这正是 `Router.Write` 必须在写失败时回退到下一个引擎的原因（见 [`../../docs/modules/file-storage.md`](../../docs/modules/file-storage.md) 第 3.3 节），也是下面 8.7 的前提。

### 8.6 二进制传输约定：**按状态码分支，不要按 body 是否为空分支**

这是本节唯一一条约束**当次调用**的子项，也是 PHP 客户端最容易写错的地方：

| 情形 | 响应 |
|---|---|
| `GET /api/file/blob` 成功 | 原始 `application/octet-stream` 字节，**不套信封** |
| `GET /api/file/blob` 失败 | `{data, error}` 信封（404 `ERR_NOT_FOUND` 居多） |
| 其余三条路径的成功与失败 | 一律信封 |

这是全仓库 `/api/**` 里**唯一**一个成功响应不是信封的端点（平台层为它没改任何代码，理由见 [`../../docs/platform.md`](../../docs/platform.md) 第 1.1 节）。所以 PHP 侧的判据只能是**状态码**：200 就把 body 当文件交出去，其余一律交给信封解析器。

**不能拿「body 是不是空的」当判据**，因为 **0 字节文件是一个合法的 200 加一个空 body**——Phorge 真的存空文件。照 body 判的客户端会把一个正常的空文件报成错误，而这种文件在库里通常只有零星几个，问题会以「偶发的、无法复现的附件损坏」形式出现。

`Content-Length` 在引擎知道长度时会带上（这是客户端区分「完整文件」与「被截断的文件」的唯一依据），引擎不知道长度时干脆不带——猜一个比不给更坏。契约固件 `read-blob.json` 用 `headerEquals` 同时断言 `Content-Type` 与 `Content-Length`，`read-blob-missing.json` 断言失败那半仍是信封；两份是一对，缺一份就只守住了一半。

### 8.7 `/readyz` **不能**检查 `file_storageblob` 是否存在

这条与前六条不同：破坏它不会让文件读不出来，会让**整个栈在第一次启动时死锁**。

**但先说清这条约定的效力边界**：遵守它是必要的，**不充分**——即使一个字都不多查，同一个死锁也会由 DSN 里那个库名独立触发。那是实测出来的，不是推断，本节下半段是它的证据与修法。

就绪判据只有两条：至少注册了一个引擎，以及持有连接的引擎能连上（当前只有 blob 引擎，它做一次 `db.PingContext`）。看起来「顺手」该加的那第三条——查一下 `file_storageblob` 在不在——是一个闭环：

- 这张表由 Phorge 的 `bin/storage upgrade` 创建；
- 那条命令跑在 Phorge 应用容器里；
- 而那个容器**要等本服务 healthy 之后才启动**。

于是本服务等一张只有 Phorge 能建的表，Phorge 等本服务健康，谁都不会先动，两个容器一起停在启动阶段。

**但「不查表」这个约定不足以躲开那个闭环，这一段此前的推理是错的，端到端验证把它证伪了。**原文写的是「ping 数据库是安全的，因为数据库服务器是一个独立容器，谁都不依赖」。独立的是**服务器**，而 DSN 里带的是**库名**：

```
{user}:{pass}@tcp({host}:3306)/{namespace}_file?…
                                └────────────┘
```

go-sql-driver 在**握手阶段**就把这个库名发过去，所以库不存在时 ping 失败在连接上，而不是失败在某条查询上。实测（真实二进制、真实 MySQL、新数据卷）：

```
GET /healthz → 200
GET /readyz  → 503
  reason: engine blob: ping database: Error 1049 (42000): Unknown database 'phabricator_file'
```

而 `{namespace}_file` 这个**库**同样是 Phorge 的 `bin/storage upgrade` 建的，跑在同一个要等本服务 healthy 的容器里。于是 8.7 这条约定要防的死锁**照样发生**，只是触发点从「表」挪到了「库」——`db-init` 也帮不上忙，`db-grant.sql` 只有一条 GRANT，一个库都不建。新数据卷上必然发生，而且不会自愈。

**这一条逐字适用于 `webhook.HeraldDSN()`**（`{namespace}_herald`，同样由 `bin/storage upgrade` 建）**以及 db-api 的探测 DSN**（`{namespace}_meta_data`，见第十一节 11.5）。webhook 躲过去只是因为它的编排依赖本来就是 `service_started`（[`../../docs/findings.md`](../../docs/findings.md) 第 41 条），**不是因为它的 readiness 有什么本质区别**；db-api 走得更远一步——它的探测 DSN 干脆不带库名，理由与实测见第十一节。别把这些域之间的差异读成「谁的探针写得更好」。

所以真正的分界不在「查不查表」，而在这两句话之间：

| 说法 | 成立吗 |
|---|---|
| ping **数据库服务器**是安全的 | 成立——服务器是独立容器，谁都不依赖 |
| ping **DSN 里那个库**是安全的 | **不成立**——那个库由 Phorge 建，于是探针重新指回了等它的那个容器 |

**修法在编排侧，不在 Go 侧。**`phorge` 对 `gorge-file-storage` 的依赖改成 `service_started`（与 `gorge-webhook` 一致）。Go 侧的 `/readyz` 语义**刻意没有动**，两个理由：

- 本节这条约定（不查表存在性）本身仍然成立，改 readiness 会正面踩到它；
- **而且这个 503 是对的。**库建出来之前 blob 后端确实一个字节都写不进去，而那个中间状态早就有兜底——写入按 priority 下沉到本地磁盘（见 3.3 与 8.5 末尾）。所以 Phorge 根本不需要等本服务就绪，它等的东西从来就不是它需要的东西。

登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 43 条。

`Router.Ready` 与 `MySQLBlobEngine.Ready` 的注释里写着「不查表」这一条，`TestMySQLBlobReadyReportsAnUnreachableDatabase` 断言 ping 失败会被如实报出来——**注意它断言的是「如实报出来」，不是「这个失败无害」**，这两件事在上面那个 1049 上分道。

还有一个方向的推论仍然成立：**不要在启动时 ping**。`OpenDB` 用 `sql.Open` 而它是惰性的，所以数据库还没起来时服务照常启动、照常答 `/healthz`，由 `/readyz` 去报告连不上——这正是编排区分「正在启动」与「坏了」所需要的。在启动路径上 ping 只会让一个「慢」的依赖把容器打进重启循环。这一条不受上面的修正影响，因为它说的是「别把探针的判据搬到启动路径上」，而不是「那个判据是安全的」。

## 九、webhook 投递：出站的字节与回写的字段

**Go 侧**：`go/internal/webhook/`（`dispatcher.go` 的 `buildPayload` / `signPayload` / `phidType`、`model.go` 的全部常量、`store.go` 的 `UpdateResult`）、`go/internal/contracts/webhook.go`
**PHP 侧**：`HeraldWebhookRequest`、`HeraldWebhookWorker`、`HeraldWebhook`、`PhabricatorGorgeWebhookClient`
（参考实现见 `phorge-fork/src/applications/herald/`）

**这一节与前八节的结构不同，因为它的约束指向两个不同的对手。**前八节讲的都是「本服务与 Phorge 之间」；本域的一半约束讲的是「本服务与**第三方接收端**之间」，而那个接收端是按 Phorge **原本**的投递写好的，并不知道换了实现。所以本节分成两半，性质正好相反：

| 半 | 约束 | 破坏后的表现 |
|---|---|---|
| 出站字节 | 9.1 payload 的字节、9.2 签名头与 HMAC、9.3 `object.type` | **会报错，但错误在别人的服务器上。**接收端算出的签名对不上，于是拒绝——你这一侧看到的是一批 4xx，而 Herald 界面上它和「接收端自己坏了」没有任何区别 |
| 回写字段 | 9.4 `status` 的取值范围、9.5 三个结果列、9.6 请求级 `errorCode` | **完全静默。**Phorge 的 UI 直接渲染这些值，写一个它不认识的进去只会让那一栏空着或显示一个原始串。其中 9.4 更进一步：它是 Go 侧整个抢占机制的地基，多一个值会同时弄坏界面和 PHP 的回退路径 |
| 接管开关 | 9.7 `gorge.webhook.uri` 的语义 | **每个 webhook 发两次。**这是全仓库唯一一处「失配的表现不是失效而是重复」的地方，见 9.7 |
| 路径与端口 | 9.8 两条只读路径与 `:8160` | `ERR_NOT_FOUND`，属于第三节那种「配置指错地方」的可见故障 |

先说清一件贯穿全节的事，因为它决定了怎么验证本节：**队列在数据库里，PHP 与 Go 之间一次 HTTP 都不发。**PHP 侧照旧把 request 行写进 `{namespace}_herald.herald_webhookrequest`，Go 侧自己轮询、抢占、投递、回写同一行。所以本节没有一条能靠「打一个接口看它答什么」来验证——9.1 到 9.6 全部只能通过**读那张表**或者**在接收端那一侧观察**来验。

### 9.1 payload 是逐字节的契约：2 空格缩进 + 末尾换行

```go
encoded, err := json.MarshalIndent(payload, "", "  ")
…
return string(encoded) + "\n", nil
```

这两件事都不是格式偏好。它们是 PHP 的 `PhutilJSON::encodeFormatted()` 的产出，而 9.2 那个签名是**对这整个字符串算的，末尾那个换行也在内**。所以：

> **改缩进就是改签名。**

一个接收端只要在校验签名（这是 Phorge 文档推荐的做法，也是这个头存在的全部理由），它就会在本服务把两个空格改成四个、或者去掉末尾换行的那一刻开始拒绝每一次投递。**键顺序同样在契约里**——`contracts.WebhookPayload` 的结构体字段声明顺序就是 JSON 的键顺序，Go 的 `encoding/json` 按声明顺序输出，所以重排那几个字段是一次兼容性变更：

```json
{
  "object": {
    "type": "TASK",
    "phid": "PHID-TASK-abcdefghijklmnopqrst"
  },
  "triggers": [
    {
      "phid": "PHID-HWTR-trigger0000000001"
    }
  ],
  "action": {
    "test": true,
    "silent": false,
    "secure": false,
    "epoch": 1700000000
  },
  "transactions": [
    {
      "phid": "PHID-XACT-TASK-transaction01"
    }
  ]
}
```

`TestPayloadIsByteExact` 拿的就是这一整段做整值比对。**这份契约无法由契约固件承载**：固件的形式是「一个请求加它的期望应答」，而这是本服务**发出**的东西，不是它答的东西。这也是本域固件只有 5 份的原因，别据此以为它的契约面小（见 [`../../docs/testing.md`](../../docs/testing.md) 第 2 节）。

两处容易被当成冗余的细节：

- **`triggers` 与 `transactions` 空的时候必须是 `[]` 而不是 `null`。**`buildPayload` 用 `make(..., 0, len(...))` 而不是声明一个 nil slice，只为这一条。一个按数组遍历的接收端在 `null` 上的行为由它自己的语言决定——PHP 的 `foreach (null)` 是一条 warning，JS 的 `.map` 是一次 TypeError——而这不该由本服务来赌。`TestPayloadCarriesEmptyListsRatherThanNull` 守它。
- **`action.epoch` 是 request 行的 `dateCreated`，不是这次尝试的时刻。**一次重投描述的是同一个事件，所以接收端可以按这个值给事件排序、也可以据它去重。换成 `time.Now()` 之后每次重投都成为一个「新事件」，而这一处偏离在本服务这一侧完全不可观测。

顺带记一条不属于字节但属于设计的：**payload 里只有标识符，没有标题、没有评论正文、没有字段值。**这是 Phorge 的设计而不是本服务的简化——接收端要什么就自己走一趟 Conduit，而那趟调用会带着它自己的凭据、受权限检查。所以往 payload 里「顺手加一个 title 省一次往返」不只是改字节，是把一份可能没有权限看的内容发给一个第三方 endpoint。

### 9.2 签名头名与算法

| 项 | 值 |
|---|---|
| 头名 | `X-Phabricator-Webhook-Signature` |
| 算法 | HMAC-SHA256，小写 hex |
| key | `herald_webhook.hmacKey`，每个 hook 一份 |
| 被签的内容 | 9.1 那个字符串的**全部字节**，含末尾换行 |

对应 PHP 的 `PhabricatorHash::digestHMACSHA256`。头名是 Phorge 的，前缀里那个 `Phabricator` 是历史遗留而**不能**「顺手现代化」成 `X-Phorge-` 或 `X-Gorge-`：每一个现存接收端都在按这个名字取头，而一个换了名字的头的表现是**接收端读到 null**——它接下来做什么由它自己决定，可能是拒绝，也可能是**当成一次未签名的投递接受下来**。后者比前者坏得多。

`TestSignatureIsHMACOverTheExactBytes` 断言签名算的就是 `buildPayload` 输出的那些字节，而不是它的某个「规范化」版本。

**这个 key 也是「`/api/webhook/hooks` 只答一个计数」的原因**：它就是一次投递可信的全部依据，所以它不能出现在任何一个诊断端点上。`tests/e2e/webhook.sh` 第 8 条从反面钉住这一点。

### 9.3 `object.type` 是 PHID 的第二段

`PHID-TASK-abcdefg…` → `TASK`。对应 PHP 的 `phid_get_type()`，`phidType()` 用 `strings.SplitN(phid, "-", 3)` 取第二段，认不出来的 PHID 得到**空串而不是错误**——那也是那个 PHP 函数的行为。

接收端按这个字段路由（「这是任务还是代码评审」），而它不必为此走一趟 Conduit，所以这个字段的存在本身就是在替接收端省调用。写错的表现是接收端把每一个对象都路由到同一个分支，或者整个跳过——而本服务这一侧看到的是一个 2xx。

### 9.4 `status` 的取值范围**不可扩展**，这是抢占机制的地基

```
queued | sent | failed
```

三个值，一个都不能多。这不是「别改枚举」那种整洁性要求，它有两个具体的、独立的持有者：

- **Phorge 的 UI 按 status 渲染图标。**一个它不认识的值让那一行显示不出状态。
- **`HeraldWebhookWorker::doWork()` 的前置检查要求 `status === queued`。**这是 PHP 的回退路径——把 `gorge.webhook.uri` 撤掉之后投递该回到 phd 手里——所以一个卡在自造状态上的行，在回退之后**永远不会被投递，也永远不会被报告**。

**这一条正是 Go 侧不得不用 `dateModified` 做乐观版本号的原因。**抢占的自然写法是把行标成 `claimed`，而那个值不能存在，于是「正在投递」与「在队列里等着」从查询侧看起来一模一样，抢占只能靠另一列。整套机制（lease、`dateModified = dateCreated` 那一半条件、`GREATEST(+1, now)`）都是从这个约束推出来的，见 [`../../docs/modules/webhook.md`](../../docs/modules/webhook.md) 第 3.1 节与 [`../../docs/findings.md`](../../docs/findings.md) 第 30 条。

所以看到 `store.go` 里那条 `UPDATE` 用 `dateModified` 而不用一个 status 值时，**不要把它「简化」成加一个状态**——那是这一节里唯一一条会同时弄坏三样东西的改动。

### 9.5 回写的三个结果列

| 列 | 取值域 | 含义 |
|---|---|---|
| `status` | 见 9.4 | |
| `lastRequestResult` | `none` / `okay` / `fail` | `none` = **一次投递都没发生**（配置类失败），与 `fail` 不是一回事 |
| `lastRequestEpoch` | Unix 秒；**永久 hook 错误时为 `0`** | |
| `properties.errorType` | `hook` / `http` / `timeout` | Phorge UI 分别渲染成「Hook Error」/「HTTP Status Code」/「Request Timeout」 |
| `properties.errorCode` | 见 9.6 | |

三件事各自都能被单独破坏：

- **`none` 与 `fail` 的区别是有行为后果的。**熔断（`HeraldWebhook::isInErrorBackoff` 与 Go 侧的 `CountRecentFailures`）只数 `fail`。把配置类失败也记成 `fail`，一个 hook 就会因为「有几个指着它的 request 属性坏了」而被判定成坏掉并停止投递——而一个 hook 并不因为一个指着它的 request 坏了就坏了。`configFailed()` 写 `none` 加 `epoch = 0` 正是 Phorge 自己的 `failRequest` 的做法。
- **`lastRequestEpoch` 的 `0` 不是「未知」的占位符，是「没发生过」。**它同时也是上一条的另一半：`0` 让这一行永远落在熔断窗口之外。
- **一次成功的投递也会写 `errorType` 与 `errorCode`。**`delivered()` 写 `http` 与状态码字符串，尽管什么都没失败。这看起来是 bug，其实是抄 PHP worker 的行为：它在按结果分支**之前**就把两者设好了，所以 Phorge 界面上一次成功显示成「HTTP Status Code / 200」。**省掉它们会让本服务的投递在界面上看起来和 Phorge 的不一样**，而那种「不一样」是运维排查时最容易被误读成故障的东西。

还有一条不在表里、但破坏后果更大的：**`properties` 是整列覆盖的。**回写把这一列整个重写，所以 `RequestProperties` 必须**原样带回**本服务不读的那些键——`transactionPHIDs` 与 `triggerPHIDs` 是 Phorge 请求详情页的内容，丢掉就是把那一页清空。`omitempty` 那组 tag 对应的是 Lisk 的 JSON 序列化把缺失属性留空的方式。（属性本身解不开时这一列会丢掉 Phorge 写的全部内容，那是本服务唯一一条这样的写路径，登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 35 条。）

### 9.6 请求级 `errorCode`：沿用 Phorge 的值域，外加三个它没有的

| 值 | `errorType` | PHP 对应物 |
|---|---|---|
| `disabled` | `hook` | Phorge 的 `HeraldWebhookRequest::ERROR_DISABLED`，渲染成「Hook Disabled」 |
| `not-found` | `hook` | **无。**按原文渲染 |
| `invalid-properties` | `hook` | **无。**按原文渲染 |
| `request-build-error` | `hook` | **无。**按原文渲染 |
| `timeout` | `timeout` | **无。**按原文渲染 |
| HTTP 状态码字符串（`"200"` / `"502"` …） | `http` | 同 PHP |
| Go 的原始传输错误字符串 | `http` | 同旧的独立服务；见下 |

`disabled` 那一条必须逐字符相同，因为它是 Phorge 自己的常量值、有一个显示串对应它。另外四个**没有 PHP 对应物，按原文渲染，而这是刻意的**：为一个只有本服务能产出的状态去给 Phorge 打补丁加一个显示串，代价大于收益——那意味着每次 Go 侧多一种失败分类都要改一次 PHP。

最后一行是一处**已知遗留**：传输失败时 `errorCode` 是 Go 的原始错误字符串，会连同它解析出的 IP 与端口一起渲染到请求详情页上。保留是为了与旧服务及 Phorge UI 既有观感一致，但要知道 **Phorge 自己在这个位置放的是短码**，所以本服务是在扩大那一栏的值域。登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 34 条。

### 9.7 `gorge.webhook.uri` 不是「服务地址」，是**接管开关**

这是本节最容易被误判的一条，也是全仓库唯一一个失配方向是反的域。

其余五个 `gorge.*.uri` 都是「PHP 要调 Go，得知道打哪儿」。本域的 PHP 与 Go 之间一次 HTTP 都不发，所以这个配置项的作用完全不同：**它一写进去，Phorge 就立刻停止给 `HeraldWebhookWorker` 派任务。**

| 配置项 | 作用 |
|---|---|
| `gorge.webhook.uri` | 接管开关（`setLocked(true)`）。非空且全局静默未开 ⇒ PHP 侧不再调度投递 |
| `gorge.webhook.token` | 只用于那两个诊断端点（`setHidden(true)`）。**投递本身不经过它**——投递走数据库，签名用每个 hook 自己的 HMAC key |

**守卫的单一真源是 `PhabricatorGorgeWebhookClient::isDeliveryDelegated()`**，两个插桩点都问它而不是各自去读配置项：

- `HeraldWebhookRequest::queueCall()`——满足条件时不 `scheduleTask`，INSERT 逻辑照旧留在 PHP。
- `HeraldWebhookWorker::doWork()`——CLI 的 `webhook call` 会 `setRunAllTasksInProcess(true)` 绕过队列，所以这条路必须同样守卫。**位置在现有的 silent 检查之后**，顺序很重要。

两处问同一个方法，是为了让「队列这条路」与「`bin/webhook call` 这条路」不会漂开。

**由此得到的失配规则必须记住：容器在跑、而 `gorge.webhook.uri` 没写进 Phorge，等于 phd 与本服务同时排空同一个队列——每个 webhook 发两次。**而接收方**无法把这种重复与一次真正的重复事件区分开**：payload 逐字节相同（同一个 `dateCreated`、同一批 transaction PHID），签名也相同。所以「两个消费者共用一个队列」不是扩容方案，要横向扩容就多起几个 `gorge-webhook`（抢占机制正是为此存在的）。登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 38 条。

反过来那个方向是安全的：撤掉配置项，投递就回到 phd 手里——前提是 9.4 那个 `status` 取值范围没被破坏。

编排侧下发这两项的是 `docker/entrypoint.sh` 的 `gorge_config_set` 段，走 render / file-storage 那种标量覆盖模式，不需要 mailer / search 的 JSON 合并。

### 9.8 两条路径与端口

| 方法 | 路径 | PHP 侧调用点 |
|---|---|---|
| GET | `/api/webhook/stats` | `PhabricatorGorgeWebhookClient::getStats()` |
| GET | `/api/webhook/hooks` | `PhabricatorGorgeWebhookClient::getHooks()` |

端口 `:8160`，与迁入前的独立服务相同——现存部署已经指着它。`TestRoutePathsAreStable` 断言这两条仍注册着。

这一条与前七条性质不同，属于第三节那种可见故障：破坏后 PHP 客户端拿到 `ERR_NOT_FOUND` 并抛异常，`PhabricatorGorgeWebhookSetupCheck` 在配置页面当场报出来。**但要注意它的影响面比看起来小**——这两条路径的唯一消费者就是那个 setup check，投递本身一条都不经过它们。所以打不通这两个端点意味着「配置页面看不到队列状态」，**不**意味着投递停了；反过来，这两个端点全绿也不意味着投递在工作。真正对应「投递在不在工作」的信号是 `/readyz`。

`wire` 字段名（`queuedCount` / `sentCount` / `failedCount` / `activeWebhooks` / `total`）声明在 [`../../go/internal/contracts/webhook.go`](../../go/internal/contracts/webhook.go)，按契约层的规则改一个就是一次兼容性变更；改名的表现是配置页面上那一栏渲染成空白。

### 9.9 已知偏离：Go 侧不认全局静默

`phabricator.silent` 是 Phorge **服务器**的配置项，而本服务从不读 Phorge 的配置——它只能看到 request 行 `properties` 里那个 per-request 的 `silent` 属性，而那个属性描述的是一次事务，不是整个装置。

所以一个开了全局静默、又把投递交给本服务的部署，webhook 会照常发出去。**这正是 9.7 那个守卫要用合取条件的原因**：静默的装置继续走 PHP 路径，由 worker 的 `failRequest(..., ERROR_SILENT)` 把 request 标成 `failed`，而本服务只取 `queued`，于是自然碰不到它们——静默模式的行为和本服务不存在时完全一样。

**修法的方向要记清楚**：它不是「让 Go 侧学会读 Phorge 的配置」，而是「让静默这一类流量根本不进入 Go 侧的视野」。前者需要本服务去解析 `conf/local/local.json` 或者新增一个必须与 Phorge 手工保持同步的环境变量，两者都是把一个配置项变成两处真源。登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 37 条。

`action.silent` 这个字段照旧在 payload 里（9.1），它携带的是那个 per-request 的值——所以**不要**因为本节而以为 payload 里那个布尔值没意义，它只是不承载全局设置。

### 改动本节任何一条之后怎么验证

**9.1 到 9.6 都没有「打一个接口看它答什么」这条路**，因为它们既不在入站请求上、也不在本服务的应答里。三层：

| 层 | 覆盖 |
|---|---|
| `go/internal/webhook/dispatcher_test.go` | 9.1 的整段字节、9.2 的签名、9.3 的 PHID 切分、9.5 的三条结果路径（`delivered` / `attemptFailed` / `configFailed` 各一组） |
| `go/internal/webhook/store_test.go` | 9.4 的推论：两条 SQL 的形状，也就是抢占的那三个 WHERE 条件都还在 |
| `tests/contract/webhook/` | 只到 9.8，即那两条路径与它们的字段名 |

出站字节那一半的**端到端**验证只能在接收端那一侧做，最短路径是起一个记录原始 body 与头的接收器：

```bash
# 在 Phorge 里建一个 webhook 指向这个接收器，然后制造一次事务。
# 要检查的是三件事，一件都不能省：
#   1. 只收到一次（9.7：确认 gorge.webhook.uri 已写进 Phorge）
#   2. body 是 2 空格缩进、以换行结尾（9.1）
#   3. X-Phabricator-Webhook-Signature 等于对整个 body（含末尾换行）
#      用该 hook 的 hmacKey 算出的 HMAC-SHA256 小写 hex（9.2）
printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$HMAC_KEY" -hex
```

**第 2、3 步要用原始字节，不要用任何框架给你解析好的对象**：一个把 body 解成 JSON 再重新序列化的接收器会让缩进与末尾换行的偏差完全消失，于是这次验证退化成一个永远通过的检查。

回写那一半只能读表：

```sql
SELECT status, lastRequestResult, lastRequestEpoch, properties
  FROM herald_webhookrequest ORDER BY id DESC LIMIT 5;
```

再把接收端改成返回 500，确认重试间隔是 **60 秒**而不是 1 秒（[`../../docs/findings.md`](../../docs/findings.md) 第 31 条），累积到 10 次之后进入 300 秒熔断。出厂默认下实测为 60.01 秒与 59.98 秒。**要注意生效值是 `max(claim lease, retry backoff)`**，所以在一个调过这两项的部署上验证之前先算一下该等多久，否则容易把 lease 抬上来的那个间隔读成 bug（第 45 条）。

---

## 十、task queue 与 worker：任务字段名与租约语义

**Go 侧**：`go/internal/taskqueue/`（`mysql_store.go` / `redis_store.go` 的 SQL 与键结构、`http.go` 的路由）、`go/internal/worker/`（`consumer.go` 的回报分岔、`handlers/` 的 Conduit 委派）、`go/internal/contracts/taskqueue.go`（全部字段名与常量）
**PHP 侧**：`PhabricatorWorkerActiveTask`、`PhabricatorWorkerArchiveTask`、`PhabricatorWorker`、`PhabricatorWorkerLeaseQuery`、`PhabricatorTaskmasterDaemon`
（参考实现见 `phorge-fork/src/infrastructure/daemon/workers/`）

**这一节与 webhook 那节同源——队列在数据库里，PHP 与 Go 之间一次 HTTP 都不发**（除非 worker 配了 Conduit 委派，那是反方向的、Go 打 PHP）。PHP 侧照旧把任务写进 `{namespace}_worker.worker_activetask` / `worker_taskdata`，Go 侧租走、跑完、归档进 `worker_archivetask`。所以本节没有一条能靠「打一个接口看它答什么」来验证，全部只能通过**读那几张表**来验。整节都是**静默型**：破坏后不报错，只错到没人发现。

| 组 | 约束 | 破坏后的表现 |
|---|---|---|
| 任务字段名 | 10.1 `worker_activetask` / `worker_archivetask` 的列名映射 | **PHP 侧读到空值或界面显示错乱。**这些 JSON 键直接映射列名，改错一个不报错，`bin/worker` 与 Web UI 照旧渲染，只是渲染出空或错的值 |
| 抢占语义 | 10.2 `leaseOwner` + `leaseExpires` 与 `(yield)` 哨兵 | **任务被两个 worker 同时取走，或 yield 任务失联。**这是抢占机制的地基，和 webhook 的 `status` 同源，任何一层都不报错 |
| 结果值域 | 10.3 归档 `result` 整数、优先级带数值 | **控制台读不出结果，或排队顺序错乱。**整数值与数值本身是契约，不只是名字 |
| 部署耦合 | 10.4 必须替换 `phd`；Redis 后端的可见性 | **每个任务跑两遍，或 Phorge 界面看到空队列。**见 10.4 |

### 10.1 任务字段名逐一映射 `worker_activetask` 的列

`contracts.Task` 的每个 JSON 键就是列名，一个都不能改名：

```
id  taskClass  leaseOwner  leaseExpires  failureCount
dataID  failureTime  priority  objectPHID  containerPHID
dateCreated  dateModified  data
```

`data` 来自 `worker_taskdata`（`JOIN` 出来），`dataID` 是它的外键。归档表（`contracts.ArchivedTask`）在这些之上多三列：`result`、`duration`、`archivedEpoch`。可空列（`leaseOwner` / `leaseExpires` / `failureTime` / `objectPHID` / `containerPHID`）在 JSON 里带 `omitempty` 且用指针，是为了让「没有值」和「值为零」可区分——一个从没被租的任务没有 `leaseExpires`，不是「在 epoch 时刻被租」。

> **改任一字段名 = PHP 侧那一列读到空。**且不报错：Lisk 按列名映射对象属性，缺一个键只是那个属性保持默认值。

**`worker_activetask.id` 由 `lisk_counter` 计数器分配，不是 AUTO_INCREMENT。**`PhabricatorWorkerActiveTask::getConfiguration()` 声明 `CONFIG_IDS => IDS_COUNTER`，所以它的 `id` 列是 `int unsigned NOT NULL` 且**没有** AUTO_INCREMENT——Phorge 在应用层用 `LiskDAO::loadNextCounterValue()` 从共享的 `lisk_counter` 表（`counterName = 'worker_activetask'`）取下一个值再写入。这带来两条约束：

- **enqueue 的 INSERT 必须显式写 `id`。**一条省略 `id`、指望 `LastInsertId()` 的 INSERT 会直接报 `Error 1364 Field 'id' doesn't have a default value`——这正是迁入时的真实故障。Go 侧 `mysql_store.go` 的 `Enqueue` 因此先在同一事务里跑一遍 Phorge 那条 `INSERT ... ON DUPLICATE KEY UPDATE counterValue = LAST_INSERT_ID(counterValue + 1)`（`nextCounterValue`），拿到 id 再显式写进 `worker_activetask`。
- **必须用同一个计数器行，不能改成 AUTO_INCREMENT 或另一套序列。**`phd` 与 gorge-taskqueue 会各自入队（见 10.4），两条路径共用 `lisk_counter` 的 `worker_activetask` 行才不会分配出撞号的 id。把 Go 侧换成 AUTO_INCREMENT（哪怕先给列加上）会让两套序列独立增长，迟早撞号，且不报错。`worker_taskdata` 是另一回事：它用 Phorge 默认的 `IDS_AUTOINCREMENT`，所以它的 id 照常靠 `LastInsertId()` 拿，不走计数器。

（`enqueue` 的请求体因此**不带** `id`：契约不变，id 由 store 分配、随响应的 `id` 返回给 PHP，`PhabricatorWorker::scheduleTask` 再把它设到 ephemeral task 上。见 `api/openapi/taskqueue.yaml` 的 enqueue 描述。）


### 10.2 抢占靠 `leaseOwner` + `leaseExpires`，yield 靠 `(yield)` 哨兵

这张表没有 `status` 列（那是 webhook 队列的事），任务的「谁持有、持到几时」全在 `leaseOwner`（可空）与 `leaseExpires`（可空）两列上：

- **未租** = `leaseOwner IS NULL`。租约阶段一只取这些。
- **租约过期** = `leaseExpires < now`。阶段二取这些（崩溃的 worker、到期重试）。
- **临时失败退避** = 清 `leaseOwner`、把 `leaseExpires` 设为 `now + retryWait`：既不算未租、也不算过期，退避期内租不到。
- **yield** = `leaseOwner = '(yield)'`（`contracts.YieldOwner` 哨兵）、`leaseExpires = now + duration`。`awaken` 精确按这个字符串识别 yield 任务。

> **`(yield)` 这个字面值不能改。**改了 `awaken` 就认不出任何 yield 任务，它们会一直躺到租约过期才被当成「过期任务」重新租走——语义变了，且不报错。**租约时长 / 退避 / yield 窗口的语义也不能各自为政**：它们共用 `leaseExpires` 一列，任一处把「未来的过期」写成「过去」，那行就立刻能被另一个 worker 抢走，于是同一个任务跑两遍。

### 10.3 归档 `result` 是整数，优先级带是固定数值

- `result` ∈ {`0`=success, `1`=failure, `2`=cancelled}，对齐 `PhabricatorWorkerArchiveTask::RESULT_*`。**是整数不是字符串**——写成字符串 Phorge 控制台读不出结果。
- 优先级带：`PriorityAlerts=1000`、`PriorityDefault=2000`、`PriorityCommit=2500`、`PriorityBulk=3000`、`PriorityIndex=3500`、`PriorityImport=4000`。租约按 `ORDER BY priority ASC, id ASC`，而 Phorge 侧按同一套数值入队，所以改数值就是改跨 PHP/Go 的排队顺序。缺省优先级是 `PriorityDefault`（2000）。

### 10.4 部署耦合：必须替换 `phd`，Redis 后端读不到旧队列

- **`gorge-taskqueue` + `gorge-worker` 必须替换 Phorge 的 `phd` taskmaster 守护进程，而不是与之并存。**队列在库里，`phd` 与 gorge-worker 谁都能租 `worker_activetask`——两边同时跑就是每个任务跑两遍，且不报错。上线本管线前先停掉 PHP 侧的 `phd`。与第九节 9.7、[`../../docs/findings.md`](../../docs/findings.md) 第 38 条同源（webhook 是同一类耦合）。
- **选 Redis 后端时，队列不在 Phorge 的库里。**于是 Phorge 的 `bin/worker` 与 Web UI 的任务视图读到一个空队列——这不是 bug，是「把队列挪出主库」的代价。要保留 Phorge 自己的任务视图就用默认的 MySQL 后端。

**验证本节**：入队一个任务后读表确认列名与值——

```sql
SELECT id, taskClass, leaseOwner, leaseExpires, failureCount, priority
  FROM worker_activetask ORDER BY id DESC LIMIT 5;
SELECT id, taskClass, result, duration FROM worker_archivetask ORDER BY id DESC LIMIT 5;
```

lease 一次确认 `leaseOwner` 被写成请求的 `X-Lease-Owner`、`leaseExpires` 是未来；yield 一次确认 `leaseOwner` 变成 `(yield)`；complete 一次确认行从 active 消失、出现在 archive 且 `result=0`。

### 10.5 Conduit 委派：`worker.execute` 必须走表单编码，不能发 JSON

gorge-worker 租到一个自己没有本地实现的 task class 时，经 conduit 网关（`GORGE_WORKER_CONDUIT_URL`）调 Phorge 的 `worker.execute` 把业务逻辑交回 PHP。这里有两条硬约束：

- **请求必须是表单编码，不能是 `application/json`。**Phorge 的 `PhabricatorConduitAPIController` 会**显式拒绝** `Content-Type: application/json`（"Use form-encoded data to submit parameters to Conduit endpoints"），而它无法当作 Conduit 请求解析的 body 会被更外层的 HTTP 栈用一张 **HTML 页面**回应——这正是迁入时委派环节 `invalid character '<'` 的真实故障。Go 侧 `handlers/conduit.go` 因此照 Phorge 自家客户端（arcanist 的 `ConduitClient`、旧的 `PhabricatorGoConduitGatewayClient`）的线格式发：`POST /api/worker.execute`，`Content-Type: application/x-www-form-urlencoded`，body 带一个 `params` 字段（值是参数 map 的 JSON，token 塞在 `__conduit__.token`），外加 `output=json`；网关另用 `X-Service-Token` 头认证。
- **`worker.execute` 是 phorge-fork 侧新增的 Conduit method**（`PhabricatorWorkerExecuteConduitAPIMethod`），必须存在于 `__phutil_library_map__.php` 里，否则 Phorge 会以 Conduit 的方法未知错误（JSON 信封）或——若请求格式又不对——HTML 回应。它 `shouldRequireAuthentication()=false` 且 `shouldAllowUnguardedWrites()=true`（网关已认证、无用户会话、无 CSRF 面），按 `taskClass`+`data` 用 `newv()` 造出真正的 worker 跑 `executeTask()`，把分类**回报**而非自己驱动队列：`success` / `yield`（带 `retry`）/ `permanent-failure` / `failure`（临时）。gorge-worker 据此翻译成 `complete` / `yield` / `fail(permanent)` / `fail(临时)`。改这个方法名或它的返回分类，会让委派回来的任务全部被当成临时失败反复重试。

---

## 十一、db-api：字段名、错误码与库/表名

**Go 侧**：`go/internal/dbapi/`（`health.go` / `diff.go` / `setup.go` / `migration.go` 的探测逻辑、`router.go` 的分区路由、`errors.go` 与 `mysqlerr.go` 的错误分类、`config.go` 的 DSN 与库名拼接）、`go/internal/contracts/dbapi.go`（全部字段名）
**PHP 侧**：`PhabricatorDatabaseRef`、`PhabricatorDatabaseSetupCheck`、`PhabricatorMySQLSetupCheck`、`PhabricatorConfigSchemaQuery`、`PhabricatorGorgeDBClient`
（参考实现见 `phorge-fork/src/applications/config/` 与 `src/infrastructure/storage/`）

**这一节与 render / mailer / search 那几节同源——本服务与 Phorge PHP 之间是真的走 HTTP 的**（PHP 打 Go 的 `/api/db/**` 读回集群自省），所以它没有 webhook / taskqueue 那种「队列在库里、一次 HTTP 都不发」的特殊性。它值得单独一节的地方，是它同时是好几个「第一」的反面：它像 file-storage 那样**只读**（不建库、不建表、不跑迁移，只 `SHOW` / `SELECT` / `INFORMATION_SCHEMA`），又像 render 那样**由入站请求驱动**（七条路由全是「有人来问、答一句」，没有后台循环），所以它既没有 webhook 第 38 条那种「必须替换不能并存」的耦合，也没有 taskqueue 那种双写同一张表的隐患——**观察一个不改动它的集群，多一个观察者不会弄坏任何东西**。

整节的判据与第五、七节相同：不是「PHP 会不会报错」，而是「破坏之后还有谁能发现」。按这个判据本节分五组，全部是静默型，只有最后一条（11.5 的编排约束）例外——它会让首启死锁，不是静默失效。

| 约束 | 破坏之后谁会发现 |
|---|---|
| 11.1 wire 字段名 camelCase | 没人报错。改掉的那个字段静默变成零值，PHP 侧按空值渲染 |
| 11.2 `isFatal` 的语义 | 没人报错，而且后果比别的字段大一档：它决定 Phorge 阻断启动还是仅告警，改错名字让每个 setup issue 都退化成告警 |
| 11.3 三个域级错误码的语义 | PHP 侧据不同的码走不同的动作，塌成一个码之后「数据库 down」与「没权限」不可分 |
| 11.4 库名与表名约定 | 库/表名对不上，探测答「未初始化」或「连不上」，看起来像「Phorge 还没建好」 |
| 11.5 `/readyz` 的探测形状（编排约束） | **首启死锁**，不是静默失效——第 8.7 节那条的逐字翻版 |

### 11.1 wire 字段名一律 camelCase，改一个就是一次兼容性变更

字段名声明在 [`../../go/internal/contracts/dbapi.go`](../../go/internal/contracts/dbapi.go)，按契约层的规则**改一个字段名就是一次兼容性变更**。`PhabricatorGorgeDBClient` 把应答直接摊成数组、按这些键读，所以这些名字**就是线上契约本身**，不是本服务的命名风格。迁入时它们从旧独立服务的 **snake_case 全量改成了 camelCase**，下面是主要的映射与它们各自的 PHP 消费点：

| camelCase（现） | snake_case（旧） | 结构 | PHP 消费点 |
|---|---|---|---|
| `refKey` | `ref_key` | `ServerRef` | `PhabricatorDatabaseRef::getRefKey()`（`host:port`），也是 `/servers/:ref/health` 的路径参数 |
| `connectionStatus` | `connection_status` | `ServerRef` | 集群数据库面板的连接列（`ok`/`fail`/`auth`/`replication-client`） |
| `connectionMessage` | `connection_message` | `ServerRef` | 同上，`fail` 时的原因 |
| `replicationStatus` | `replication_status` | `ServerRef` | 复制列（`ok`/`replica-slow`/…） |
| `secondsBehindMaster` | `seconds_behind_master` | `ServerRef` | 复制延迟秒数 |
| `isMaster` | `is_master` | `ServerRef` | 区分 master / replica |
| `isFatal` | `is_fatal` | `SetupIssue` | 见 11.2——**载重最大的一个** |
| `issueKey` | `issue_key` | `SetupIssue` / `SchemaIssue` | Phorge 的 issue 常量名，据它去重与定位 |
| `databaseName` | `database_name` | `SchemaNode` / `SchemaIssue` | 三级树的库层键，`{namespace}_meta_data` 等 |
| `tableName` / `columnName` | `table_name` / `column_name` | `SchemaNode` / `SchemaIssue` | 树的表层与列层键 |
| `characterSet` / `collation` / `engine` / `columnType` / `nullable` | 独立服务迁入后补齐 | `SchemaNode` | `INFORMATION_SCHEMA` 的实际库、表、列属性，PHP 据它们构造实际 schema 后比较 |
| `expected` / `actual` | 同名 | `SchemaIssue` | schema 差异的两侧值 |
| `patch` / `initialized` | 同名 / `is_initialized` | `MigrationStatus` | `patch_status` 里跑过的 patch 列表与「库建了没」 |

改名的表现是那个字段**静默变成零值**，其余字段照常——症状是局部的：把 `secondsBehindMaster` 改个名，面板照常显示每台服务器、只有复制延迟那一列空着，而没有任何一层报错。

### 11.2 `isFatal` 是本节载重最大的字段

`SetupIssue.isFatal` 镜像 Phorge 的 `PhabricatorSetupIssue::isFatal()`。Phorge 据它决定一个环境/schema 问题是**阻断启动**（fatal，装置根本起不来直到修好）还是**仅在配置页告警**（非 fatal）。所以它不是一个展示字段，是一个控制字段——**改错它的名字或把它的值算反，会让每一个 setup issue 都退化成告警**，包括那些本该阻断启动的（比如 `{namespace}_meta_data` 库缺失、MySQL 版本过低）。装置于是「看起来能起来」，直到某个本该被 fatal 挡住的问题在运行时以另一种形式炸出来。

它单独拎出来，是因为它是本节唯一一个破坏后果不是「少显示一栏」而是「改变 Phorge 的启动决策」的字段——性质更接近第六节的 `ERR_PERMANENT_FAILURE`（改行为，不只改显示）。判断某个 setup issue 该不该 fatal 的规则原样抄自 Phorge 的两个 setup check（`PhabricatorDatabaseSetupCheck` / `PhabricatorMySQLSetupCheck`），**不要按 Go 侧的直觉重新判定**。

### 11.3 三个域级错误码的语义

三个，都从旧独立服务沿用，因为它们是**调用方需要区分**的失败——PHP 侧据不同的码走不同的动作，所以不能塌进平台的 `ERR_INTERNAL`（同 file-storage 的 `ERR_NO_ENGINE` 一个道理）：

| 码 | 状态 | 语义 | 为什么单独一个码 |
|---|---|---|---|
| `ERR_DB_UNREACHABLE` | 503 | 一台配置的库服务器连不上 | 503 因为**本服务没坏**——数据库 down 了或还没起来，是运维/编排问题 |
| `ERR_READONLY` | 409 | 对一个已降级为只读的连接/router 发起了写 | 409 因为调用方可以把写改发向一台可达的 master 来解决 |
| `ERR_DB_ACCESS_DENIED` | 403 | 配置的库用户缺少该操作所需的权限 | 403 且与本服务自己的 token 校验（401）分开——修法是 GRANT，不是重试 |

映射由 `codeForKind` 完成（`errors.go`）：域内先把驱动错误按 errno 分类成 `DBError`（`mysqlerr.go`，access-denied 一族 → `kindAccessDenied`，2006/2013 连接中断 → `kindUnreachable`），handler 的 `fail` 再把可被调用方处置的 kind 翻成上面三个码，**并且只答一句通用文案**——`genericMessage` 只说「哪一类东西出了问题」，绝不带主机名、库名或查询。一个通过了 token 校验的服务间调用方仍不该从响应体里拿到集群拓扑；真正的错误留给 `slog` 日志（走 `ERR_INTERNAL` 那条 `return err` 的路径）。

**`ERR_READONLY` 目前是一条定义了但七条只读路由都到不了的码。** 它随 `Router` 的只读降级逻辑（抄自 Phorge 的 `PhabricatorLiskDAO`：连不上 master 时翻只读、后续写被 `GetWriter` 拒成 `ERR_READONLY`）一起从旧服务搬来，保留是为了忠实复现 Phorge 的路由/降级语义、也为后续可能的写路径留着接口；但当前七个 handler 全是只读探测，走各 service 自己开的短连接，不经过 Router 的写入分支。**别因为「用不到」就把它删掉**，也别把它塌进平台码——它的语义是调用方可处置的，与另外两个同理。见 [`../../docs/modules/dbapi.md`](../../docs/modules/dbapi.md) 第 1、6 节。

### 11.4 库名与表名约定：都是 Phorge 的，不能顺手现代化

- **库名按 Phorge 的方式拼成 `{namespace}_meta_data`**（以及其它 `{namespace}_<app>`），`{namespace}` 来自 `GORGE_DB_NAMESPACE`（旧名 `STORAGE_NAMESPACE`，默认 `phorge`），**必须与该装置的 `storage.default-namespace` 一致**。拼法在 `config.go` 的 `DatabaseName`。它选错的表现是探测连到一个不存在的库，`MigrationStatus.initialized` 留 `false`，看起来像「Phorge 还没建好」而不是「namespace 配错了」。
- **迁移状态读 `patch_status` 表**：`MigrationService.Status` 按 Phorge 分区路由选出承载 `meta_data` 的 enabled master，再由 `checkRef` 对它的 `{namespace}_meta_data` 跑 `SELECT patch FROM patch_status`，对齐 Phorge 的 `bin/storage` 写进这张表的账本；其它应用的专属 master 不承载这个库，不能被误报成未初始化。响应字段名是 Phorge 所读的 `patch`。表名和字段名都是兼容契约，改了就读不到迁移进度。replica 的 `patch_status` 通过复制到达，不是它自己迁出来的。建连或 Ping 失败仍表示尚未初始化；一旦 Ping 成功，账本查询失败必须显式报错，不能返回一个看似成功的空 patch 列表。
- **多 master 同步状态读 `hoststate` 表**：额外跑一次 `SELECT stateValue FROM hoststate WHERE stateKey = 'cluster.databases'`，这是 Phorge 在多 master 之间同步 `cluster.databases` 的表。**当前读出来就丢**——保留这个读点只为对上旧服务预留的多 master 同步接口，不消费它的值。表名同样是 Phorge 的。

`{namespace}_meta_data` 库不存在**不是错误，是如实报告**：那正是 `bin/storage upgrade` 跑之前的状态，`initialized` 留 `false`、调用方读到「未初始化」就对了。这一条与 11.5 的死锁直接相关——正因为这个库由 Phorge 建、而 Phorge 排在本服务之后启动。

### 11.5 `/readyz` 不能查表、探测 DSN 不能带库名（编排约束）

这一条是第 8.7 节那条的**逐字翻版，而且走得更远一步**，所以本节结论先写在前面：**本服务的探测 DSN 根本不带库名，healthcheck 打 `/healthz` 而非 `/readyz`，phorge-fork 侧对它的依赖必须是 `service_started` 而非 `service_healthy`。** 三者任一被「顺手修正」都会让首启死锁。

`/readyz` 的判据只有一条：至少一台配置的 master 能被 ping 通（`anyReachable`）。看起来该加的第二条——查一下 `{namespace}_meta_data` 或 `patch_status` 在不在——是一个闭环，与 file-storage / webhook 同源：这个库由 Phorge 的 `bin/storage upgrade` 建，那条命令跑在**排在本服务之后启动**的 Phorge 容器里，于是本服务等一个只有 Phorge 能建的库、Phorge 等本服务健康，两个容器一起停在启动阶段。

**而 8.7 实测出来的那件事在这里同样成立、且被本服务提前一步躲开了**：光「不查表」不够——go-sql-driver 在握手阶段就把 DSN 里的库名发过去，库不存在时 ping 会失败在**连接**上而不是失败在查询上。file-storage 的探测 DSN 带 `{namespace}_file`，于是它即便不查表也会撞上 `Error 1049 Unknown database`（8.7 下半段的证据）。db-api 从那次实测里学到了教训：**它的探测 DSN 干脆不带任何库名**，一个 ping 就能打通一台 Phorge 库还没建出来的服务器，`/readyz` 只据「握手成功」判就绪。

对应的编排结果与理由：

| 说法 | 成立吗 |
|---|---|
| ping **数据库服务器**是安全的 | 成立——服务器是独立容器 |
| ping **不带库名的 DSN** 是安全的 | 成立——这正是 db-api 采取的形状 |
| ping **带 `{namespace}_meta_data` 的 DSN** 是安全的 | **不成立**——那个库由 Phorge 建，探针会重新指回等它的那个容器 |

所以别把 healthcheck 改成 `/readyz`、别把依赖改成 `service_healthy`、也别为了「探得更实」给探测 DSN 补上库名——三者都会把这个躲开的死锁请回来。这个 compose 文件本身不声明 MySQL 也不声明 Phorge，所以那里没有 `depends_on` 要写，`service_started` 这条约束活在 phorge-fork 的编排里。见 [`../../deploy/compose/docker-compose.yml`](../../deploy/compose/docker-compose.yml) 里 `gorge-db-api` 那段注释、[`../../docs/modules/dbapi.md`](../../docs/modules/dbapi.md) 第 3.5 节，与本文件第 8.7 节。

### 改动本节任何一条之后怎么验证

11.1 与 11.3 靠固件与单元测试；11.4 与 11.5 只能对着一个真实（或缺失）的 `{namespace}_meta_data` 库观察。最短路径是分两种库状态各走一趟：

```bash
# 库还没建（新装置、bin/storage upgrade 之前）：
#   /healthz 必须 200，/readyz 视 master 可达性而定，migrations/status 报未初始化
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/readyz
curl -s -H 'X-Service-Token: dev-token' http://127.0.0.1:8080/api/db/migrations/status
# initialized 必须是 false 而不是一个错误——这是 11.4 的核心

# 库建好之后：migrations/status 报出跑过的 patch 列表，setup-issues 的 isFatal 与
# Phorge 配置页一致
curl -s -H 'X-Service-Token: dev-token' http://127.0.0.1:8080/api/db/setup-issues
```

**11.5 用 curl 只能验到「库缺失时 /readyz 不因带库名而挂在连接上」这一半**：起一个新数据卷、不跑 `bin/storage upgrade`，确认 `/readyz` 要么据 master 可达性答 200、要么答 503 且原因是「master 连不上」而**不是** `Unknown database`。后者一旦出现，就说明探测 DSN 又带上库名了——那正是 8.7 下半段那个 1049 的形状。

---

## 附：鉴权与响应信封

六个 PHP 客户端（Render / Mailer / Search / FileStorage / Webhook / DB，共同的请求构建与信封解析已抽到 `PhabricatorGorgeServiceClient` 基类）依赖以下两点，改动会直接打断 PHP 侧：

（Webhook 那个是六个里的异类，值得知道：其余五个都是它们所代表的那份能力的**唯一**入口，而 webhook 的投递走数据库、与这个类无关——它上面最要紧的成员因此不是任何一个请求方法，而是 `isDeliveryDelegated()` 这个谓词。见 9.7。db-api 是六个里最规矩的一个，它的七条路由全走这个基类，没有 webhook 那样的旁路。）

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

`ERR_NOT_FOUND` 对 PHP 侧最有诊断价值：`gorge.render.uri` 尾部多一个斜杠、或 base URL 拼接出双斜杠时，拿到的就是它；`cluster.search` 条目的 host/port 拼错时同理。两个路由细节别误判（对 `/api/highlight/**`、`/api/diff/**`、`/api/mailer/**`、`/api/search/**`、`/api/file/**`、`/api/webhook/**` 六个分组都成立）：分组的鉴权早于路由解析，不带 token 打不存在的路径返回 401 而不是 404；同样在这些分组下方法用错返回 404 而不是 405（分组为了鉴权匹配了所有方法），所以 `ERR_METHOD_NOT_ALLOWED` 实际只在健康探针路径上见得到。

`ERR_TOO_LARGE` 有**三个**来源，同码是刻意的，PHP 客户端按码分支即可，不需要知道是哪一道：

| 来源 | 阈值 | 检查点 |
|---|---|---|
| 域级字节数 | `GORGE_RENDER_MAX_BYTES` / `GORGE_DIFF_MAX_BYTES`（各默认 1MiB） | handler 内 |
| 传输层 body | 固定 2M | `httpx` 中间件 |
| LCS 表单元数 | 4,000,000（编译期常量，仅 diff 域） | 切完行之后 |

默认配置下只会命中第一道；把域级上限调到 2M 以上就会改走第二道。第三道不能被前两道替代：LCS 表分配的是 `n*m` 而非行数，2001 行对 2001 行只有几十 KB 却要一张四百万单元的表，而 100 行对 100000 行反而便宜、必须放过。

diff 域的字节检查算的是 **`len(old) + len(new)` 之和**，不是任一侧——两个 600 KiB 的文件加起来就超限了。

`ERR_INTERNAL` 的 `message` 恒为一句通用文案，panic 值与堆栈只进 `slog` 日志。**排查 500 要看服务日志，不要指望响应体。**

**db-api 的三个也没落进平台码，理由与 mailer 那两个同源——调用方需要区分。**`ERR_DB_UNREACHABLE`(503) / `ERR_READONLY`(409) / `ERR_DB_ACCESS_DENIED`(403) 分别对应「数据库 down 或还没起来」「写打在一个已降级只读的连接上」「库用户权限不足」，PHP 侧据不同的码走不同动作（等编排、改发 master、GRANT），塌成一个码就分不开了，见第十一节 11.3。其中 `ERR_READONLY` 当前是一条**定义了但七条只读路由都到不了**的码——它随 Router 的只读降级逻辑一起保留，不是遗漏。

域级错误码**因此是十二个**，都是迁移前就有、Phorge 侧已经在用的码，故未收敛进平台码：render 域的 `ERR_HIGHLIGHT_FAILED`(500)，mailer 域的 `ERR_PERMANENT_FAILURE`(422) 与 `ERR_SEND_FAILED`(502)，search 域的 `ERR_INDEX_FAILED` / `ERR_SEARCH_FAILED` / `ERR_INIT_FAILED` / `ERR_CHECK_FAILED` / `ERR_STATS_FAILED`（均 502），file-storage 域的 `ERR_NO_ENGINE`(503)，以及 db-api 域的 `ERR_DB_UNREACHABLE`(503) / `ERR_READONLY`(409) / `ERR_DB_ACCESS_DENIED`(403)。全局错误处理器不会覆盖它们——`httpx.Fail` 一写响应就 committed，处理器见到 `Committed` 就不再落笔。

**webhook 域一个都没加，而这是决定而不是遗漏。**它的两个端点都只做一件事——数行——所以唯一的失败是数据库没答话，平台的 `ERR_INTERNAL` 已经说完了；而真正需要被区分出来的那个状态（「服务活着但连不上队列」）由 `/readyz` 报告，还附带一句失败原因，一个新码在这上面改进不了任何东西。这个选择由 `tests/contract/webhook/unavailable/stats-database-unreachable.json` 从**反面**钉住：既然没有域码承载细节，message 就必须保持通用、body 不得泄漏 SQL、库名、主机或端口。**它与 diff 域「刻意没有域级错误码」不是同一个理由**——diff 是「没有可报告的失败模式」，webhook 是「失败模式只有一个，而平台码已经说完了」。判据是那个失败在调用方那里是否引出一个与平台码不同的动作。

**taskqueue 与 worker 也都没加，同 webhook 的理由。**taskqueue 的失败要么是入参错（`ERR_BAD_REQUEST` 400、任务不存在 `ERR_NOT_FOUND` 404），要么是后端没答话（`ERR_INTERNAL` 500）；「服务活着但连不上队列」同样由 `/readyz` 报告。这个选择由 `tests/contract/taskqueue/unavailable/`（`stats.json`、`tasks.json`）从反面钉住：message 保持通用、body 不得泄漏 SQL、库名、主机、端口或「connection refused」。worker 的 `/api/worker/stats` 读进程内计数器，永不失败，连错误路径都没有。所以**域级错误码总数是十二个**（webhook / taskqueue / worker 三个域各自都没加，db-api 则带来三个，见上）。

mailer 那两个的区别不是文案而是**行为**，见第六节 6.2；另外 mailer 域的后端失败一律落在 422 或 502，**不落 500**——那里的 500 只意味着服务自己出了问题。search 域的五个同理：全部 502，500 在那个域只意味着服务自己坏了。

**但 search 那五个目前在 PHP 侧没有消费者。**`PhabricatorGorgeSearchClient` 没有覆盖 `newServiceErrorException()`（`PhabricatorGorgeMailerClient` 覆盖了，因为 6.2 那两个码必须分道），所以十一个码全部塌成同一个通用异常，码本身只作为文本活在异常消息里。这是当前**刻意保留**的行为，登记在 [`../../docs/findings.md`](../../docs/findings.md) 第 21 条——记录事实，不是提议改 Go 侧。五个码分开的价值在 `bin/search` 与运维读日志时兑现，不在 PHP 的异常分支上。

`ERR_NO_ENGINE` 与它们又不同：它区分的是**「一次都没试」与「试了并且坏了」**。503 意味着没有任何已配置的后端会接下这个文件（一个后端都没配，或者这个大小没有后端能收），是一个运维改配置就能解决的状态，也是 Phorge 侧 setup check 唯一能据以行动的写失败；而某个后端真的坏了落 500。把两者混成一个码，「服务没配好」与「存储挂了」在 PHP 侧就不可分了。

**diff 域刻意没有域级错误码。**两个引擎都没有可报告的失败模式：prose 引擎是全函数，unified 引擎唯一会拒绝的是过大的输入，而那已经是 `ERR_TOO_LARGE` 了。在那里造一个码，它永远不会被返回。新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。

**空 source 不是错误**：`{"source": ""}` 返回 200 与空 `html`，不返回 400。Phorge 渲染空文件时依赖这个行为。

**健康探针不套信封**：`GET /`、`GET /healthz`、`GET /readyz` 返回裸 `{"status":"ok"}`。这是给容器探针和负载均衡用的，不要「顺手统一」成信封格式。

本附录讲的是 `/api/**`，即 render、diff、mailer、search、file-storage、webhook、taskqueue、worker 与 db-api 九个域——九者的鉴权口径完全一致，信封口径有一处**记录在案的例外**，另外传输上限各不相同：

- **例外只有一个**：file-storage 的 `GET /api/file/blob` **成功**时答原始 `application/octet-stream` 字节而非信封，失败仍是信封。所以那一条路径上按状态码分支，别按 body 形状分支，理由与陷阱见第八节 8.6。除它之外，本附录对九个域一字不差地成立——包括 file-storage 自己的另外三条路径，以及它端口上任何由框架产生的响应（`TestUnknownPathKeepsTheEnvelope` 断言这一点，webhook 与 taskqueue 域各有一份同名的）。
- **`ERR_TOO_LARGE` 的来源**：render / diff 的传输层上限是 `2M` 并另有域级字节检查；mailer 是 `10M`（base64 让附件涨三分之一），没有域级字节检查，正文超限是静默截断而不是拒绝；search 用平台默认的 `2M`，也没有域级字节检查——一份文档多大是 Phorge 的事，而语料大到成问题时那是存储的配置问题，不是线上的；file-storage 是 **`16M`**（文件是裸请求体），它的「域级」检查是各存储引擎自己的 `MaxFileSize()`——只有在请求**指名了引擎**时才答 413，未指名而所有引擎都收不下时答的是 503 `ERR_NO_ENGINE`；webhook 用平台默认的 `2M` 而且**永远碰不到它**，因为它的两个端点都是 `GET`、没有请求体；taskqueue 用平台默认的 `2M`（入队的任务负载都远小于此），worker 只有一个 `GET` 状态端点、同样碰不到；db-api 的七条路由全是 `GET`、没有请求体，同样碰不到，用平台默认的 `2M`。
- **webhook、taskqueue 与 worker 只在这个附录的范围内占一半。**它们的 `/api/**` 完全照本附录办事，但真正要紧的契约在别处：webhook 在它**发出去**的那份文档上（第九节），taskqueue 与 worker 在它们**读写的那几张 worker 表**上（第十节），那些东西都不由本仓库的任何一个端点承载。别把「这些端点都符合附录」读成「这几个域的兼容面已经覆盖了」。db-api 与它们相反——它的契约面**完全**落在这些端点上（字段名、错误码都在应答里），只有 11.4 的库/表名约定是个例外，那是它查询的对象而非它的应答。

**notification 的两个端口不在这个范围内**：它们不鉴权、成功响应不套信封、client 口的 `GET /` 连探针都不是。要改那两个端口先看第五节，不要照这一节的口径推。
