# webhook 模块

把 Phorge 排进 Herald 队列的 webhook request 投递出去：轮询 `{namespace}_herald.herald_webhookrequest`，抢占其中 `queued` 的行，带 HMAC-SHA256 签名 POST 到 hook 配置的 URI，再把结果回写同一行。独占 `gorge-webhook` 这个二进制与 `:8160` 这个端口。

| | |
|---|---|
| 二进制 | `gorge-webhook` |
| 端口 | `:8160` |
| 包 | `go/internal/webhook/` |
| 契约 | [`api/openapi/webhook.yaml`](../../api/openapi/webhook.yaml) |
| 固件 | `tests/contract/webhook/`（含 `unavailable/`） |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第九节 ← **改动前必读** |

**这是仓库里第一个「工作不由入站请求驱动」的域**，而这件事改变的东西比它听起来多。前六个域的形状都是「有人来问、答一句」，所以「服务在正常工作」与「服务答得出请求」是同一件事；本域不是——它的两个 HTTP 端点都只是**报数**，没有任何一条路径能启动一次投递。于是每一层的分辨能力都要重新评估一遍：契约固件覆盖的是这个域较小的那一半（见 [`../../tests/contract/webhook/README.md`](../../tests/contract/webhook/README.md)），e2e 脚本压根碰不到投递，而真正的字节级契约只有单元测试守得住。第 3 节那几条也是同一个理由才值得写下来：它们描述的是一个没有调用方能观察到的循环。

## 1. 职责边界

**负责**：把队列里 `queued` 的 request 变成一次出站 POST，并把「投出去了没有、对方怎么答的」如实写回那一行。三个时间窗——claim 的 lease、单个 request 的失败退避、单个 hook 的熔断——都由它管，见 3.2。

**不负责**：**入队**。哪个事务该触发哪个 hook、request 行长什么样、`retry` 是 `never` 还是 `forever`，全部由 Phorge 侧的 `HeraldWebhookRequest` 决定并写好；本服务只读它、投递它、回写它。它也不建表、不改表结构、不需要 DDL 权限——`herald_webhookrequest` 与它的索引都是 Phorge 的 `bin/storage upgrade` 的产出。

也不负责 hook 的**管理面**。`herald_webhook` 这张表本服务只读，而且只读到「有几行」这个程度：`GET /api/webhook/hooks` 答的是一个计数，不是一个列表。这不是省事——每个 hook 背后是它的 URI 与 HMAC key，而那个 key 就是「一次投递可信」的全部依据。hook 该在 Phorge 自己的界面里看，那里能校验看的人有没有权限。`tests/e2e/webhook.sh` 第 8 条把这条钉住：响应里出现 `hmac`、`URI` 或 `http` 任一片段就算失败。

**它必须替换 PHP 侧的投递，而不是与之并存。** 队列在库里，两边谁都能取，所以「多一个消费者」在这个域里不是扩容而是**给别人的 endpoint 发重复 POST**——而接收方无法把它与一次真正的重复事件区分开。让路的开关在 PHP 侧（`gorge.webhook.uri`），语义见第 5 节，登记在 [`../findings.md`](../findings.md) 第 38 条。

**有外部依赖，而且是这一类里最硬的一个。** mailer、search 与 file-storage 都能在没有任何外部东西的情况下起成「就绪」——test 适配器、内存索引、本地磁盘各自都是一个真后端。本域没有对应物，因为**数据库不是它写穿的后端，而是它的工作本身**：两个端点都在数它的行，循环没有它就没有东西可读。所以「一个后端都没配」这个状态在这里不存在，`/readyz` 只有一条判据，见 3.5。

## 2. 路由与依赖

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/webhook")
	g.Use(auth.Token(deps.Token))

	g.Get("/stats", stats(deps))
	g.Get("/hooks", listHooks(deps))
}
```

| 方法 | 路径 | 鉴权 | 成功响应 |
|---|---|---|---|
| GET | `/api/webhook/stats` | 需要 | 信封，`{queuedCount, sentCount, failedCount, activeWebhooks}` |
| GET | `/api/webhook/hooks` | 需要 | 信封，`{total}` |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） | 裸 `{"status":"ok"}` |

**整个 API 是只读的，这是设计而不是「还没写」。** 队列的内容归 Phorge——是它写那些行——所以一个能让调用方推一行进去的端点，等于给一张本服务只该**排空**的表开第二个入口。`TestTheEndpointsAreReadOnly` 与 e2e 第 10 条各压一遍（POST / PUT / DELETE 都不许答 200）。

**两个端点而不是一个字段**，因为它们回答的是两个问题：`stats.activeWebhooks` 数的是**投递得出去的** hook（`status <> 'disabled'`），而 `hooks.total` 数的是每一个 hook，禁用的也算。「一个 hook 都还没建」与「建了但全被关掉了」是运维要分别处置的两个状态，而 `activeWebhooks` 把两者都报成 `0`。这两个数之间还有一条不变量——`activeWebhooks` 永不大于 `hooks.total`——它跨两个端点、两次独立查询，是 e2e 第 7 条唯一一条单元测试拿不到的断言：一个把某个 COUNT 写在了错误的表上的实现，在忙碌的队列上会产出一组看起来很合理的数字。

四个计数**每次调用都真的去数**，不留内存计数器。所以它们描述的是 Phorge 自己的 UI 看到的那个队列——包括其它实例的工作，也包括本进程没在跑的时候排进来的行。

`Deps` 只有 `Store` 与 `Token`。**`Store` 是 interface，而这是契约固件能存在的前提**：本域两个端点都要查库，没有 file-storage 那种「换一个 local-disk 后端就绕开 MySQL」的余地，所以固件与 handler 单测注入的是一份手写的内存实现（`memstore_test.go`）。顺带修掉的是独立服务时期的一处分层破坏——那时 `listHooks` 通过 `Store.DB()` 自己执行 `SELECT COUNT(*)`，把 SQL 放进了 HTTP 层，也让 store 的边界成了装饰。

`main.go` 与另外五个二进制的差别只有一处，但那一处是这个域的全部：**它多一个 goroutine**。

```go
ctx, stopPolling := context.WithCancel(context.Background())
polling := make(chan struct{})
go func() {
	defer close(polling)
	webhook.NewDispatcher(store, cfg).Run(ctx)
}()

runErr := srv.Run()

stopPolling()
<-polling
```

一个信号停两半：`srv.Run()` 在 SIGINT / SIGTERM 上返回，取消这个 context 才是让轮询循环收尾的东西。**先排空再关连接池**（`<-polling` 在 `store.Close()` 之前），否则在途投递的结果写入会被抽掉连接。`Close` 与 file-storage 同理是显式调用而非 `defer`——下面那条失败分支要 `os.Exit`。

**listener 起不来就让进程死掉，是刻意的。**监听失败不影响投递循环，所以「留着循环继续跑」是一个技术上可行的选择；但那样就得到一个在投递、而任何人都问不到它状态的实例，编排也不会把它换掉。

## 3. 核心实现

### 3.1 claim：用 `dateModified` 当乐观版本号

**这是本次迁入修掉的最要紧的一个缺陷，而它在单实例部署下也会发生。** 独立服务时期的候选查询就是 `SELECT ... WHERE status = 'queued'`，没有任何抢占动作；投递期间那一行的 status 仍然是 `queued`，而轮询间隔默认 1 秒、投递超时上限 15 秒——所以下一个 tick 会**再取到同一批行**，同一个事件被 POST 出去两次到十几次。

修法的第一条约束是**不能改 `status` 的取值范围**。Phorge 的 UI 按 status 渲染图标，`HeraldWebhookWorker::doWork()` 的前置检查要求 `status === queued`，所以在这里发明一个 `claimed` 值会同时弄坏界面和 PHP 的回退路径。抢占必须发生在别的地方。

用的是 `dateModified`：

```sql
UPDATE herald_webhookrequest
   SET dateModified = GREATEST(dateModified + 1, UNIX_TIMESTAMP())
 WHERE id = ? AND status = 'queued' AND dateModified = ?
```

只有 `RowsAffected() == 1` 算抢到。这一列是**正确的、也是唯一可用的那一列**：Lisk 自动管理它、Phorge 从不展示它、herald 的 GC 只看 `dateCreated`，而 PHP 侧的守卫上线之后本服务是它唯一的写者。于是抢占不需要新增列、不需要长事务，也不需要 `SELECT … FOR UPDATE SKIP LOCKED`——后者要 MySQL 8.0 或 MariaDB 10.6，而一个 Phorge 装置跑在什么版本上是不能假设的。

`GREATEST(dateModified + 1, UNIX_TIMESTAMP())` 里那个 `+ 1` **不是保险，是必需的**。这一列只有秒级精度，所以「同一秒内插入并抢占」的那一行会被写回它已经持有的值——`WHERE` 对第二个抢占者仍然成立，而那正是这条语句要挡的全部失败。`+ 1` 同时保证一串连续抢占不会停滞：时钟不动，版本也照样前进。`TestAClaimInTheSameSecondStillChangesTheVersion` 守它。

候选查询那一侧的对应条件是：

```sql
WHERE status = ?
  AND (dateModified = dateCreated OR dateModified <= ?)   -- lease
  AND (lastRequestResult <> ? OR lastRequestEpoch <= ?)   -- 失败退避
ORDER BY id ASC
LIMIT ?
```

`dateModified = dateCreated` 这一半容易被当成冗余，它不是：**它让新插入的行不必先等一个它从未进入过的 lease**。Lisk 的 `willSaveObject` 在插入时把两个时间戳写成同一秒，而本服务之后的每一次写入都让 `dateModified` 严格变大，所以「两者相等」就等于「没人碰过」。这个前提哪天不成立了，代价也只是这个条件不再命中——延迟，不是正确性。`TestAFreshRequestIsClaimableImmediately` 与 `TestAClaimedRequestIsNotACandidateUntilTheLeaseExpires` 是它的两半。

`status = ?` 写在最前面、排序按 `id`，是为了吃到 PHP 侧 autopatch 新加的 `key_status (status, id)` 索引。**加索引不改变这个查询的任何结果**：Phorge 自带的索引里 `status` 列一个都没覆盖，所以在此之前这是一次全表扫，而这张表的 `sent` 行由 GC 保留 7 天、随流量增长且从不缩回零。索引的另一半故事（它必须同时声明进 `CONFIG_KEY_SCHEMA`，否则 `bin/storage adjust` 会在某个与本次改动毫无关联的时刻把它删掉）在 [`../findings.md`](../findings.md) 第 40 条。

`TestClaimStatementIsAnOptimisticCompareAndSet` 与 `TestFetchClaimableStatementKeepsItsConditionsSeparate` 直接断言这两条 SQL 的形状。这看起来是在测字符串，实际测的是「这两个条件没有被合并」——那是下一节。

**抢占在生产里是看不见的**，这一点在排查时要先知道。「有人抢输了，所以没有重复投递」这件事只由 `WEBHOOK_CLAIM_LOST` 一条日志表达，而它用的是 `slog.Debug`，而本仓库任何地方都没有配置日志级别——`log/slog` 的默认级别是 Info，所以那条日志在**任何**部署里都打不出来。端到端验证只能靠「POST 总数恰好等于队列行数」反推抢占在工作（三实例 / 单 hook / 40 行加压下零重复）。登记在 [`../findings.md`](../findings.md) 第 44 条。

### 3.2 三个时间窗，刻意不合并

| 窗口 | 默认 | 判据列 | 管的是 | 对齐的 Phorge 行为 |
|---|---|---|---|---|
| claim lease | 30s（不低于投递超时） | `dateModified` | 「抢到这行的进程死了」 | 无对应物，本次新增 |
| 失败退避 | 60s | `lastRequestEpoch` | 一个 request 失败后等多久重投（实际生效值见下） | `PhabricatorWorkerLeaseQuery::getDefaultWaitBeforeRetry()` |
| 熔断 | 300s / 10 次 | `lastRequestEpoch` 计数 | 一个 hook 坏到什么程度就停投 | `HeraldWebhook::getErrorBackoffWindow()` / `getErrorBackoffThreshold()` |

**lease 与失败退避看起来能合并成一个条件，但那会让两者都错。** lease 回答的是「是不是已经有另一次尝试在进行中」，那是一个关于投递超时的问题；失败退避回答的是「这个 request 失败之后等够了没有」，那是一个关于「一个坏掉的接收端该被多快重试」的问题。合并之后，一个被崩溃进程遗弃的行要等满 60 秒才有人接，而一个刚失败的 request 会在 30 秒后就被重投——两个方向都不是想要的。`newClaimQuery` 从**一次**读时钟里派生两个 cutoff，所以两个条件描述的是同一个瞬间。

**但两个条件是 AND，而一次失败回写同时刷新两列，所以重投的实际间隔是 `max(ClaimLease(), RetryBackoffSec)`。**`UpdateResult` 把 `dateModified` 与 `lastRequestEpoch` 都写成当下，于是那一行要重新成为候选就得**同时**越过两道门——先到的那道不算数。默认值下 30 < 60，退避那道晚，所以生效的是 60，与设计意图一致（端到端实测两次失败重投间隔为 60.01 秒与 59.98 秒）。

**要留神的是把 `GORGE_WEBHOOK_RETRY_BACKOFF_SEC` 往下调的那个方向：调到 `ClaimLease()` 以下不会有任何效果，会被 lease 静默抬回去**（实测 `retry=2` 加 `lease=20` 得到 20.1 秒，而不是 2 秒）。**设成 `0` 也不是「下一个 tick 立刻重投」，而是 lease 那么久**——默认 30 秒。这一点没有任何日志或校验会提醒你，`Config.ClaimLease()` 那种「低于投递超时就抬上去」的显式抬升在这条路上也不存在，因为抬升发生在查询条件的合取里而不在配置里。登记在 [`../findings.md`](../findings.md) 第 45 条，那里也写了根治方向（启动时校验并告警实际生效值）。

**失败退避是本次补上的第二个缺陷修复。** 独立服务的 `handleFailure` 把 `retry = forever` 的 request 直接写回 `queued`，下一秒立刻重投——于是「累积 10 次失败触发 300 秒熔断」这道护栏在十秒内就烧完了，而在它生效之前接收端已经被打了十次。

**lease 不得短于一次投递。** `Config.ClaimLease()` 在 `ClaimLeaseSec < DeliveryTimeout` 时返回后者，因为一个比投递还短的 lease 恰好把 claim 要挡的重复投递放了回来：行在整个尝试期间都是 `queued`（status 不能长出 `claimed`），所以 lease 一过期，第二次尝试就能拿走一个首次 POST 还在飞的行。`TestClaimLeaseNeverUndercutsADelivery` 与 `TestClaimLeaseCoversAWholeDeliveryAttempt` 各守一层。

熔断的窗口是**滑动**的，所以恢复不需要复位动作：旧的失败滑出窗口，计数自己落回阈值以下，投递恢复。`TestTheCircuitBreakerRecoversAsTheWindowSlides` 断言这一点。计数本身失败时报「在熔断中」，是保守的那个方向——留在队列里的 request 一个 tick 之后还会再来，而在数据库出错时照发出去的那一个，可能正打在一个已经被打垮的接收端上。

**熔断期间的日志量需要预期**：`WEBHOOK_HOOK_IN_ERROR_BACKOFF` 是**每个候选行每个 tick** 一条 Warn，而熔断窗口本身就是 300 秒。按出厂默认（poll 1s、`MaxConcurrent` 8）大约 8 条/秒，一个坏掉的 hook 带着积压跑一天约 69 万条 Warn。这与上一节那条打不出来的 Debug 是同一个问题的两头，一并登记在 [`../findings.md`](../findings.md) 第 44 条。

还有一道与这三个窗口并列、但性质不同的护栏：**同一个 hook 的投递被串行化**（`lockHook`）。Phorge 用一把名为 `webhook(PHID)` 的全局锁做同一件事，理由也一样——一个积压的 hook 否则会把它队列里的每一行同时投出去，而熔断追不上一个它自己没能参与计量的突发。跨 hook 的并发上限则是 `MaxConcurrent` 的信号量，它同时封住 goroutine 数与 socket 数。

### 3.3 步骤顺序就是契约

```
processRequest
  ├─ properties 解不开        → 记 failed / none / invalid-properties，**不 claim**
  ├─ hook 查库失败            → 什么都不写，下一 tick 再来
  ├─ hook 不存在              → 记 failed / none / not-found，**不 claim**
  ├─ hook 被禁用              → 记 failed / none / disabled，**不 claim**
  ├─ 取 hook 串行锁
  ├─ 在熔断窗口内             → 什么都不写，等窗口滑过
  ├─ claim 失败或没抢到       → 什么都不写，由抢到的那个进程记结果
  └─ 投递
```

**配置类失败在 claim 之前就把 request 退役掉，这是刻意的**：哪个进程记录一个「每个进程都会算出同样答案」的终态，无关紧要。而这之后的每一步都受保护——**claim 是 POST 之前的最后一件事**，所以一个在等 hook 锁、或者在数最近失败次数上花了时间的进程，不会去投递一个同期已被别人拿走的 request。`TestAConfigurationFailureIsRecordedWithoutClaiming` 与 `TestALostClaimDeliversNothing` 成对钉住这条边界。

「hook 查库失败」与「hook 不存在」分岔得开，也必须分得开：后者是 Phorge 的 GC 退役了一个 hook 而队列里还有指着它的 request，是一个**预期的答案**，所以 `GetWebhook` 用 `(nil, nil)` 表达它而不是报错；前者只是库没答话，那一行原封不动留在队列里。

### 3.4 投出去的那份文档是字节级契约

```go
encoded, err := json.MarshalIndent(payload, "", "  ")
…
return string(encoded) + "\n", nil
```

**两空格缩进与末尾那个换行不是格式偏好。**它们是 PHP 的 `PhutilJSON::encodeFormatted()` 的产出，而签名是**对这整个字符串（含末尾换行）**算的：

```go
mac := hmac.New(sha256.New, []byte(key))
mac.Write([]byte(payload))
// → X-Phabricator-Webhook-Signature: 小写 hex
```

所以改缩进就是改签名。一个按 Phorge 自己的投递写好、正在校验签名的接收端，只要这个函数的输出变一个字节就开始拒绝——而它拒绝的方式是它自己的事，本服务这一侧只会看到一个非 2xx。字段顺序同样在契约里（`contracts.WebhookPayload` 的结构体字段顺序即 JSON 键顺序），`TestPayloadIsByteExact` 拿一份完整的期望字符串压住它，`TestSignatureIsHMACOverTheExactBytes` 压住签名算的就是这些字节。权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第九节。

两处容易被「顺手优化」掉的细节：

- **`triggers` 与 `transactions` 空的时候是 `[]` 而不是 `null`。**`buildPayload` 用 `make(..., 0, len(...))` 而不是声明一个 nil slice，正是为此。`TestPayloadCarriesEmptyListsRatherThanNull` 守它——一个按数组遍历的接收端在 `null` 上的表现由它自己的语言决定，而这不该由本服务来赌。
- **`action.epoch` 是 request 行的 `dateCreated`，不是这次尝试的时刻。**一次重投描述的是同一个事件，所以接收端能按这个值排序。

`object.type` 取 PHID 的第二段（`PHID-TASK-…` → `TASK`），这是 PHP 的 `phid_get_type`；认不出来的 PHID 得到空串而不是错误，与那个函数的行为一致。`TestObjectTypeComesFromThePHID` 守着。

### 3.5 `/readyz` 只 ping，不查表

就绪判据只有一条——数据库答话——因为这个服务没有别的可等的东西，也没有它能在数据库之外做的事。**它不检查 `herald_webhookrequest` 存不存在**，理由与 file-storage 那条（见 [`file-storage.md`](file-storage.md) 第 3.5 节）同源但更直接：那张表由 Phorge 自己的 `bin/storage upgrade` 建，而跑那条命令的容器可能后启动，所以要求表存在等于把一个健康的部署在它第一次迁移期间报成坏的。`OpenDB` 同样**不 ping**，`NewMySQLStore` 也不再像独立服务那样在 ping 失败时 `os.Exit(1)`——那让一个排在数据库之前的容器进入重启循环。

**但「只 ping」并没有把这个探针从那个闭环里摘出来，这一点必须知道，因为它看起来已经摘出来了。**`HeraldDSN()` 里带的是**库名**（`{namespace}_herald`），go-sql-driver 在握手阶段就把它发过去，所以库不存在时 ping 失败在连接上，报的是 `Error 1049 (42000): Unknown database`。而那个库同样是 `bin/storage upgrade` 建的。**换句话说本服务首次启动时必然有一段 `/readyz` 不通的窗口，而那段窗口只能由 Phorge 自己结束。**

**本服务躲开死锁靠的是编排侧的依赖强度，不是这个探针**：`phorge` 对它用 `service_started` 而不是 `service_healthy`（[`../findings.md`](../findings.md) 第 41 条）。file-storage 此前用的是 `service_healthy`，于是同一条链在那边变成了一个首启永久死锁——实测出来的，见 [`../findings.md`](../findings.md) 第 43 条。**别把两者的差别读成「webhook 的探针写得更好」**，两个探针在这件事上是同一个形状；差别只在依赖那一行。

**这个缺口对本域比对任何其他域都严重，这是 `/readyz` 在这里必须存在的全部理由。**一个连不上库的实例：在监听，`/healthz` 答一个高高兴兴的 200，投递量为零，而且**任何地方都不出现失败**——Phorge 继续入队，那些行就静静躺着，Herald 界面上它们停在蓝色的 Queued 图标上，和「服务慢了一拍」长得一模一样。独立服务时期只有 `/` 与 `/healthz` 两个无条件 200，也就是说这个状态在那时候完全不可见。容器 healthcheck 因此打 `/readyz`，`tests/e2e/webhook.sh` 第 2 条的注释写的就是这一段。

## 4. 配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `:8160` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权。**它只保护那两个诊断端点**——投递走数据库，签名用每个 hook 自己的 HMAC key |
| `GORGE_WEBHOOK_MYSQL_HOST` | `127.0.0.1` | 见下 |
| `GORGE_WEBHOOK_MYSQL_PORT` | `3306` | |
| `GORGE_WEBHOOK_MYSQL_USER` | `phorge` | |
| `GORGE_WEBHOOK_MYSQL_PASS` | 空 | |
| `GORGE_WEBHOOK_NAMESPACE` | `phorge` | Phorge 的存储命名空间；DSN 里的库名由它拼成 `{namespace}_herald`，必须与该装置的 `storage.default-namespace` 一致 |
| `GORGE_WEBHOOK_POLL_INTERVAL_MS` | `1000` | 轮询间隔，同时也是一次变更的投递延迟 |
| `GORGE_WEBHOOK_DELIVERY_TIMEOUT` | `15` | 单次投递的 HTTP 超时（秒），与 `HeraldWebhookWorker` 给它的 `HTTPSFuture` 的值相同 |
| `GORGE_WEBHOOK_MAX_CONCURRENT` | `8` | 跨全部 hook 的在途投递上限 |
| `GORGE_WEBHOOK_ERROR_BACKOFF_SEC` | `300` | 熔断窗口（秒） |
| `GORGE_WEBHOOK_ERROR_THRESHOLD` | `10` | 熔断阈值（次） |
| `GORGE_WEBHOOK_RETRY_BACKOFF_SEC` | `60` | 单个失败 request 的重投间隔（秒） |
| `GORGE_WEBHOOK_CLAIM_LEASE_SEC` | `30` | claim 的 lease（秒），低于投递超时时自动抬到投递超时 |

规范变量命名规则见 [`../platform.md`](../platform.md) 第 4 节。最后两项在迁入前不存在，升级时无需映射旧配置。

**`GORGE_WEBHOOK_MYSQL_HOST` 在这里不是开关**，与 file-storage 的对应变量正相反（那边的 blob 后端必须显式给 host，理由见 [`../findings.md`](../findings.md) 第 22 条）。本域没有东西可以关掉：两个端点都读库，循环也是围着库转的一圈，所以「没配」与「连不上」不是两个值得区分的状态——两者 `/readyz` 都报，也都意味着一条都投不出去。留空则落到 `127.0.0.1`，在容器里就是容器自己，于是服务起来、不就绪。这是诚实的。

`TestDefaultsMatchPhorge` 把那几个与 PHP 侧对齐的默认值（15 / 300 / 10 / 60）钉在一起——它守的是「这些数字有出处」，不是这些字面值本身；要动其中任何一个，先去看对应的那个 PHP 方法。

连接池的三个参数（`maxOpenConns=20` / `maxIdleConns=5` / `connMaxLifetime=5m`）定在 `db.go`，按本域的用法定：池要覆盖 `MaxConcurrent` 次并发投递，每次投递碰库三到四回（claim、查 hook、数最近失败、回写结果），外加两个状态端点。**`platform/` 仍然没有数据库设施，而本域就是「第二个需要它的域」那个判据的实际到来**——判断是继续不加，理由见 [`../findings.md`](../findings.md) 第 39 条。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第九节，这里是概述。本域的约束分成性质完全不同的两组，混着读会误判它们的轻重：

**一组是出站字节，破坏之后接收端会明确拒绝。** 签名头名 `X-Phabricator-Webhook-Signature`、HMAC-SHA256 小写 hex、payload 的 2 空格缩进与末尾换行、键顺序、`object.type` 取 PHID 第二段。这一组是本仓库少见的「**会**报错」的兼容约束——只是报错发生在别人的服务器上，你这一侧只看到一批 4xx，而 Herald 界面上它们和「接收端自己坏了」没有任何区别。

**另一组是回写字段，破坏之后什么都不会发生。** `status` ∈ {`queued`, `sent`, `failed`}、`lastRequestResult` ∈ {`none`, `okay`, `fail`}、`lastRequestEpoch`（永久 hook 错误时为 `0`）、`properties.errorType` ∈ {`hook`, `http`, `timeout`}、`properties.errorCode`。这些值 Phorge 的 UI 直接渲染，写一个它不认识的进去只会让那一栏空着或者显示一个原始串。**其中 `status` 的取值范围是整个 claim 机制的地基**（3.1），它不只是「不该改」，而是「多一个值就同时弄坏界面和 PHP 的回退路径」。

三条容易踩的细节：

- **`properties` 是整列覆盖的。**回写把这一列整个重写，所以 `RequestProperties` 必须**原样带回**本服务不读的那些键（`transactionPHIDs` / `triggerPHIDs`）——它们是 Phorge 请求详情页的内容，丢掉就是把那一页清空。
- **一次成功的投递也会写 `errorType` 与 `errorCode`。**`delivered()` 写 `http` 与状态码字符串，尽管什么都没失败。这是 PHP worker 留下的样子（它在按结果分支之前就把两者设好了），所以 Phorge 界面上一次成功显示成「HTTP Status Code / 200」。省掉它们会让本服务的投递看起来和 Phorge 的不一样。
- **请求级 `errorCode` 沿用 Phorge 的值域，但本服务多产出三个它没有的。**`disabled` 对应 Phorge 的 `ERROR_DISABLED`、渲染成「Hook Disabled」；`not-found`、`invalid-properties`、`request-build-error` 与 `timeout` 没有 PHP 对应物，按原文渲染。这是刻意的——为一个只有本服务能产出的状态去给 Phorge 打补丁加一个显示串，代价大于收益。

**PHP 侧的接管开关是 `gorge.webhook.uri`（配套 `gorge.webhook.token`），守卫的单一真源是 `PhabricatorGorgeWebhookClient::isDeliveryDelegated()`。** 它是本仓库六个 `gorge.*.uri` 里唯一一个**不是「服务地址」而是「接管开关」**的：一写进去，Phorge 就立刻停止给 `HeraldWebhookWorker` 派任务。所以这个域的失配方向和别的域是反的——不是「配了不生效」，而是「服务在跑但配置没写进去 = 每个 webhook 发两次」。

**「Go 不认全局静默」是一处已知偏离**，也正是那个守卫要用合取条件（`gorge.webhook.uri` 非空 **且** `phabricator.silent` 未开）的原因：`phabricator.silent` 是 Phorge 服务器的配置，本服务读不到，它只能看到 request 行里的 per-request `silent` 属性。登记在 [`../findings.md`](../findings.md) 第 37 条。

## 6. 域级错误码

**一个都没有，而这是一个决定而不是遗漏。**

两个端点都只做一件事——数行——所以它们唯一的失败模式是「数据库没答话」，而平台的 `ERR_INTERNAL`（500）已经把这句话说完了。真正需要被区分出来的那个状态是「服务活着但连不上队列」，而它由 `/readyz` 报告，还附带一句失败原因——一个新错误码在这上面改进不了任何东西。造一个 `ERR_QUEUE_UNAVAILABLE` 只会得到一个第二处、更差的说法。

这个选择由 `tests/contract/webhook/unavailable/stats-database-unreachable.json` **从反面**钉住：既然没有域码承载细节，那么 message 就必须保持通用，并且 body 里不得出现 SQL、库名、主机或端口。独立服务时期不是这样的——那时 `respondErr(..., err.Error())` 会把驱动错误里的主机与端口送到 500 的响应体里，登记在 [`../findings.md`](../findings.md) 第 33 条。handler 现在把 store 的错误**原样往上抛**，交给平台错误处理器：

```go
result, err := deps.Store.Stats(c.Request().Context())
if err != nil {
	return err
}
```

`TestAStoreFailureIsAnOpaque500` 守着这一点。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**
