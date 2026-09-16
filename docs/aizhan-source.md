# 爱站数据源迁移与实际验证

核对日期：2026-09-12。爱站读取公开 `/cha/{domain}/` HTML，不调用 `apistore.aizhan.com/baidurank/siteinfos/[私钥]`，不需要私钥。主数据通常只需一个爱站页面请求；APPPC 排名、分类、注册人、邮箱和到期日继续从站长工具页面及其原有动态数据请求取得。

## 实测结果与字段映射

先读取 `www.baidu.com` 的真实页面，再使用新增的 Go 采集器查询 `www.aizhan.com`，均成功。后者于 2026-09-12 03:14 UTC 返回：百度 PC 4、移动 4、搜狗 4、360 4、必应 0、PR 5；百度来路 `64,550 ~ 92,118`、反链 87、域名年龄 `18年7月12日`。这验证了请求和解析，不代表其他域名一定有数据，也不是持续大批量稳定性测试。

后续于 2026-09-12 10:02 UTC 实测 `-with-chinaz` 完整混合采集成功：同一域名从站长工具补回 APPPC 排名 `384`、分类 `科技数码`、注册人 `ename technology co.,ltd.`、到期日 `2031-02-01`。邮箱未返回。爱站权重仍为 PC/移动各 4，PR 5、反链 87；两个来源的链接、摘要和采集时间分别输出。

| 数据库字段 | 来源字段 | 实际情况 |
| --- | --- | --- |
| `baidu_pc_weight` | `baidurank_br` 图片 `/br/N.png` | 可获取，0–10 |
| `baidu_mobile_weight` | `baidurank_mbr` 图片 `/mbr/N.png` | 可获取，0–10 |
| `sogou_weight` | `sogou_pr` | 可获取，搜狗 PC |
| `so_360_weight` | `360_pr` | 可获取，360 PC |
| `bing_weight` | `bing_pr` | 可获取，必应 PC；实测真实 0 被保留 |
| `pr_weight` | `google_pr` | 页面显示的 PR 值，可能是历史数据，不承诺实时更新 |
| `traffic_text/min/max` | `baidurank_ip` | 页面“百度来路”；保留文字并解析范围、千分位及万/亿单位 |
| `backlink_count` | `backlink` | 爱站反链口径，与站长工具数值可能不同 |
| `domain_age_text/days` | `whois_created span` | 原文年龄；天数沿用原系统按年/月估算的语义，非精确注册日差值 |
| `shenma_weight` | `sm_pr` | 实测 HTML 中被注释，留空；以后出现真实节点才读取 |
| `apppc_pc_rank` | Chinaz `.apppcarank` / `SiteAPPAndPC.ashx` 的 `WeekRank` | 从站长工具补充 |
| `site_category` | Chinaz `.webrank i.color-63` / `GetTopRanked.ashx` | 从站长工具补充 |
| `registrant_name/email` | Chinaz 结果表“域名信息” | 沿用原页面解析；源站隐藏或未提供时留空 |
| `expires_on` | Chinaz 域名信息中的“过期时间为” | 沿用原页面解析，不从爱站年龄推算 |

补充采集每个域名额外请求 1 个站长工具页面；只有排名或分类缺失才追加对应动态请求，最多 3 个站长工具请求。补充任务不调用 Chinaz `Rank.ashx`；每天独立执行的站长权重任务会调用该接口，并标记 `weight_source=chinaz`。补充结果只复制上述五项，动态响应附带的 PR、反链和站长工具权重均不会覆盖爱站数据。神马未列入补充范围。

头条权重和移动预计来路在页面存在，但原数据库没有对应独立字段，此次不扩展业务模型。导航栏出现某项工具，不代表本次查询返回了该数据。页面的 TDK 更新时间也不是权重更新时间，因此没有当作权重采集日期。

## 失败与数据质量

- 核对页面查询输入框的域名，防止首页、跳转或其他域名的结果被写入。
- 百度 PC、移动权重两项必须均有效。只有流量、PR、导航图标或加载占位内容的页面会失败，爱站结果不写入权重快照，仅重试爱站任务；站长权重由另一队列独立采集。
- 非核心字段缺失保留空值；真实 0 保留为 0。旧日期历史记录保留；如果强制重采已有日期，原有 `ReplaceOne` 语义会用新来源的完整快照替换该日记录，不混入旧来源字段。
- `source_url` 记录实际爱站链接，`raw_sha256` 记录返回正文摘要，`collected_at` 表示本系统采集时间，不能证明爱站数据当天更新。
- `supplemental_source_url`、`supplemental_raw_sha256`、`supplemental_collected_at` 分别记录站长工具补充来源、响应摘要和采集时间，新增字段兼容现有 MongoDB 文档。
- 没有有效结果时沿用 MongoDB 持久任务队列的失败/重试记录。失败不调用 `SaveJobResult`，因此不会把旧成功记录覆盖成假零。
- 两家使用独立的持久队列和 worker。任一来源成功即更新当天快照中属于自己的字段；另一个来源失败不影响写入。失败不清空已有字段；成功但明确缺失的可选字段仅清除该来源自己负责的旧值。站长工具正常返回明确的无数据状态（`StateCode=0` 且包含 `Result`）时，可成功入库，对应补充字段留空。

## 频率、并发与容量

爱站默认请求完成后随机等待 10–20 秒；站长工具独立按请求起始时间间隔 3–8 秒。爱站模式要求 `WORKER_COUNT=1`；爱站权重、站长权重、站长补充分别有一个 worker。站长两个 worker 共享单请求并发及节流，每域名站长权重最多 2 次、补充最多 3 次请求（不含任务重试）。配置限制爱站最小间隔不少于 10 秒，但这只是本项目的保守策略，不是爱站公开保证的安全额度。

只有源站 403/429、明确的安全验证页面或失败响应带有效 `Retry-After` 才启用该来源冷却。普通超时、空结果、解析不完整、重定向和普通 5xx 只影响当前任务，其他域名仍按正常间隔执行。爱站网络错误和无 `Retry-After` 的 5xx 最多请求两次，重试也走同一节流器。源站封禁冷却默认 15 分钟，连续封禁逐次翻倍，最高 1 小时；`Retry-After` 可以延长冷却，成功后重置计数。Agent 自身容量不足的 HTTP 429 不视为爱站封禁。

各权重 worker 在自己的来源冷却结束后才从 MongoDB 领取任务；一个来源冷却时，另一个来源独立运行，避免批量耗尽尚未请求的域名的重试次数。失败任务继续按 `COLLECTION_RETRY_DELAYS=10m,30m,1h` 延后，实际请求仍须遵守对应站点的冷却。重试不会无限持续，进程取消时等待可立即退出。

站长工具补充请求不在任务内立即重试，由自己的持久队列延后重试。只有明确封禁触发站长工具 15–60 分钟递增冷却，爱站队列照常工作；反过来也一样。单条补充任务最长 3 分钟。生产服务不再使用串行的 `Hybrid.Fetch`；该辅助函数仅保留给单域名诊断 probe。

爱站沿用 `collection_jobs`，站长权重使用 `chinaz_weight_jobs`，站长补充使用 `chinaz_supplement_jobs`，服务启动自动建立索引，兼容 `SKIP_MONGO_INIT=1`。升级时自动为原有 queued/running 任务补建独立站长权重和补充任务。手动、定时、启动采集同时入队三个任务；去重与成功检查各自独立，force 也不会重复创建正在排队或执行的同源任务。进程重启后的任务恢复、归档取消和历史清理覆盖三个队列。

重启不会取消整轮采集或清除排队任务。正常关闭时，worker 使用独立的 5 秒写库窗口将中断任务立即退回 queued，并退还该次尝试计数；主进程等待 worker 释放任务后退出。异常退出或暂时写库失败遗留的 running 任务在超过 `STALE_JOB_AFTER`（默认 20 分钟）后，由每分钟巡检自动恢复；启动时尚未超时的任务也会在后续巡检恢复，不再永久悬挂。巡检排除本进程仍在执行的任务，以免较慢但活跃的请求被重复领取。三个队列均适用。

`GET /api/v1/collect/progress` 顶层汇总两站权重任务：498 个域名对应 996 个权重任务；`sources.aizhan/chinaz` 分别显示两站进度，`supplement` 单独显示补充进度。日志和任务查询的 `source` 支持 `aizhan`、`chinaz`、`chinaz_supplement`；`GET /api/v1/jobs?source=chinaz` 查看站长权重任务，默认查看爱站任务。

两来源通过 MongoDB 原子字段更新合并到同一个 domain/date 文档，不再整条替换。补充先到时，快照可只包含补充字段；权重字段仍为空。为兼容已有严格 validator，补充首次插入时使用真实 Chinaz 来源填充通用 provenance；爱站完成后更新通用来源，Chinaz 来源始终单独记录在 `supplemental_*`。

**部署约束：一个出口 IP 只运行一个 SEO 采集后端实例。** 当前信号量、请求间隔和冷却是进程内状态，重启会重置；MongoDB 原子领取任务只防止重复领取，不能协调多个实例的请求频率。不要通过增加后端副本、多个 probe 进程或频繁重启提升吞吐。证书/标题 Agent 的并发配置不影响 SEO 请求。如果将来必须多副本，需要先加入 MongoDB/Redis 共享限流与冷却。

下面是仅爱站按平均 15 秒间隔计算的参考量级，不含网络耗时、重试、冷却和站长工具补充。站长工具等待可与爱站间隔部分重叠；混合采集的实际一轮耗时仍需实测，不能据此保证容量：

| 域名数 | 一轮最低量级 |
| --- | --- |
| 100 | 约 25 分钟 |
| 1,000 | 约 4.2 小时 |
| 5,000 | 约 20.8 小时 |
| 10,000 | 约 41.7 小时，不能满足每日一轮 |

实际耗时更长。先用少量自己的域名观察至少一轮，检查成功率、429/403、无数据错误和 pending/running 积压，再逐步扩量。持续触发冷却时降低频率，查明访问限制；不要自动绕过验证码或轮换出口冲击源站。接近 5,000/日或更高的规模，应评估官方 API 和账户配额，不能靠加并发保证抓全。

爱站[官方百度权重 API 文档](https://apistore.aizhan.com/detail/23/)确认提供 `pc_br`、`m_br`、`ip`，每次最多提交 50 个站点，按站点计调用次数，需要账户私钥，并定义 `100008` 为查询过频。它不等同于无限免费额度，也不能用一个百度接口补齐所有现有字段。当前实现不需要私钥，也没有声称已接入或实测该认证 API；大规模使用前需确认账户额度、频率和其他字段所需接口。

## 旧部署切换

### 可选：爱站页面通过指定 Agent 获取

主控直连仍是默认行为。2026-09-15 实际诊断中，同一域名在主控返回 HTTP 200、0 字节，在 `49.7.214.217:8002` 所在节点返回正常 HTML。这只能确认当时两条网络路径的结果不同，不能断言主控永久不可访问爱站。

配置 Agent 后优先由 Agent 获取爱站页面。Agent 遇到 HTTP 400、普通超时、空响应、格式错误或权重解析不完整时，主控等待原有 10–20 秒请求间隔，再直连同一爱站查询地址复测一次。复测同样校验域名、完整 PC/移动权重及响应大小；成功立即写库，失败保留两条路径的错误并交由持久队列延后重试。每次任务最多一次 Agent 请求和一次 master 复测，复测成功不会永久切换节点，下一任务仍优先 Agent。

源站明确返回 403/429、验证码或封禁冷却标记时，遵守原有冷却，不通过切换出口继续请求。Agent 自身忙碌返回的 HTTP 429 属于容量不足，可以触发 master 复测。master 复测也共享串行限频，遇到明确封禁会开启爱站冷却。未配置 Agent 时保持原有主控直连行为。无需新增配置，本次复测功能只更新主控即可；master 向爱站发起请求时不携带 Agent Token。

另一条独立任务由主控直连站长工具获取五个补充字段并写库，不受复测逻辑影响。两家数据都不通过 Agent 标题解析器。

先升级 SituationAwareness-agent 至支持 `type=seo` 的版本，保持已有 8002 端口与共享 Token，`AGENT_MAX_TIMEOUT` 不小于主控 `SCRAPE_TIMEOUT`（默认 25s）。检查：

```bash
curl -fsS http://49.7.214.217:8002/healthz
```

响应 `taskTypes` 必须包含 `seo`。旧版本不支持该任务；仅配置现有证书/标题 URL 不会自动启用爱站转发。

升级主控后，在 `/usr/local/seo_monitor/.env` 中设置或替换（避免重复键）：

```dotenv
AIZHAN_AGENT_URL=http://49.7.214.217:8002
AIZHAN_AGENT_TOKEN=
```

空的 `AIZHAN_AGENT_TOKEN` 复用 `TITLE_AGENT_TOKEN`，再回退 `CERTIFICATE_AGENT_TOKEN`；必须与目标 Agent 的 `AGENT_SHARED_TOKEN` 相同。无需把 Token 发到聊天或提交进 Git。重新启动 `seo-monitor` 使配置生效；不需要重复提交仍在排队的 498 个任务。切回主控直连时把 `AIZHAN_AGENT_URL` 清空并重启。

日志 `Aizhan request started` 的 `route` 显示 `direct`、`agent:http://.../api/v1/tasks` 或 `direct:master-recheck`。Agent 失败触发复测时额外记录 `Aizhan Agent failed; master recheck scheduled`，包含 Agent 的原始失败原因。`Aizhan response received` 显示页面字节数和传输错误；对应来源的 `domain collection succeeded` 才代表该来源解析及入库完成。空响应明确报告 `HTTP 200 with an empty body`。快照 `collection_route` 记录最终成功路径。

Agent 只允许固定的 `https://www.aizhan.com/cha/{domain}/` 地址，禁止 URL/端口参数，不跟随重定向，最多读取 3 MiB。原始正文以 JSON base64 返回，主控再次限制响应大小、校验任务 ID/域名/来源 URL，并沿用原页面解析校验。Agent 独立限制单个 SEO 请求、完成后至少间隔 10 秒，普通超时不会启用长冷却；源站 403/429 或失败响应的 Retry-After 才冷却 15–60 分钟，并返回 `sourceBlocked` 和 `retryAt`。主控仍按 10–20 秒节流。此次更新需同时更新 Agent，旧版 Agent 自身仍会因普通超时进入长冷却。多个主控不能借同一 Agent 提升吞吐。

本地完整链路验证（2026-09-15）：启动临时新版 Agent，通过主控 probe 查询 `itgirls.cn` 收到 94,016 字节并解析出百度 PC/移动 0、搜狗 3、360 1、反链 5。未连接生产数据库；部署到 boce 后还需检查实际采集日志。

### 原数据源参数

更新后端程序后，在实际服务读取的环境文件中修改下列项目并重启服务。安装脚本对已有环境文件的保留行为不变，不会替你改线上配置。

```dotenv
SOURCE_PROVIDER=aizhan
SOURCE_BASE_URL=https://www.aizhan.com
SOURCE_DATA_URL=https://othertool.chinaz.com
CHINAZ_SUPPLEMENT_BASE_URL=https://seo.chinaz.com
CHINAZ_SUPPLEMENT_MIN_DELAY=3s
CHINAZ_SUPPLEMENT_MAX_DELAY=8s
CHINAZ_SUPPLEMENT_COOLDOWN=15m
AIZHAN_COOLDOWN=15m
SCRAPE_TIMEOUT=25s
SCRAPE_MIN_DELAY=10s
SCRAPE_MAX_DELAY=20s
SCRAPE_RETRIES=2
WORKER_COUNT=1
STALE_JOB_AFTER=20m
COLLECTION_RETRY_DELAYS=10m,30m,1h
```

`SOURCE_DATA_URL` 用于站长工具动态请求，混合模式也会使用。旧配置在 `SOURCE_BASE_URL` 残留 Chinaz URL、主抓取间隔仍为 3–8 秒或配置多个 worker 会在爱站模式启动时报错。`CHINAZ_SUPPLEMENT_*` 与主抓取配置独立。回退时同时设置 `SOURCE_PROVIDER=chinaz`、`SOURCE_BASE_URL=https://seo.chinaz.com`、`SOURCE_DATA_URL=https://othertool.chinaz.com`。

MongoDB 每日文档新增 `weight_snapshots.aizhan/chinaz`，已知历史来源在后台自动迁移，无需清库或重新初始化。默认顶层结果优先有效爱站，其次有效站长，两份快照独立保存。详情见 [双源每日快照](dual-source-snapshots.md)；本次不修改通知脚本。

## 验证

```sh
go test ./...
go run ./cmd/aizhan-probe -domain www.aizhan.com
go run ./cmd/aizhan-probe -domain www.aizhan.com -with-chinaz
```

probe 默认只查询爱站；加 `-with-chinaz` 验证完整混合采集。它只查询一个域名、输出解析结果，不连接 MongoDB。不要对它开启外部并发或紧密循环。实际入库复用现有 collector/store；本次运行了全量 Go 测试和 BSON 零值/缺失字段回归，没有连接生产 MongoDB 或修改线上数据。

测试覆盖真实 HTML 摘录、注释节点、域名不匹配、0/缺失、范围单位、部分结果、403/429/重定向/验证码、`Retry-After`、有界重试、并发串行化与请求间隔、可取消冷却、响应体大小，以及配置迁移防误用。

双源独立采集、历史迁移和接口说明见 [双源每日快照](dual-source-snapshots.md)。
