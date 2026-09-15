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

补充采集每个域名额外请求 1 个站长工具页面；只有排名或分类缺失才追加对应动态请求，最多 3 个站长工具请求。不调用 Chinaz `Rank.ashx`。补充结果只复制上述五项，动态响应附带的 PR、反链和站长工具权重均不会覆盖爱站数据。神马未列入补充范围。

头条权重和移动预计来路在页面存在，但原数据库没有对应独立字段，此次不扩展业务模型。导航栏出现某项工具，不代表本次查询返回了该数据。页面的 TDK 更新时间也不是权重更新时间，因此没有当作权重采集日期。

## 失败与数据质量

- 核对页面查询输入框的域名，防止首页、跳转或其他域名的结果被写入。
- 百度 PC、移动权重两项必须均有效。只有流量、PR、导航图标或加载占位内容的页面会失败，不写当日快照。
- 非核心字段缺失保留空值；真实 0 保留为 0。旧日期历史记录保留；如果强制重采已有日期，原有 `ReplaceOne` 语义会用新来源的完整快照替换该日记录，不混入旧来源字段。
- `source_url` 记录实际爱站链接，`raw_sha256` 记录返回正文摘要，`collected_at` 表示本系统采集时间，不能证明爱站数据当天更新。
- `supplemental_source_url`、`supplemental_raw_sha256`、`supplemental_collected_at` 分别记录站长工具补充来源、响应摘要和采集时间，新增字段兼容现有 MongoDB 文档。
- 没有有效结果时沿用 MongoDB 持久任务队列的失败/重试记录。失败不调用 `SaveJobResult`，因此不会把旧成功记录覆盖成假零。
- 两家请求任一失败则整条任务延后重试，不写半份快照。因此站长工具持续故障也会延迟爱站权重入库；已有成功快照保留。站长工具正常返回明确的无数据状态（`StateCode=0` 且包含 `Result`）时，可成功入库，对应补充字段留空。

## 频率、并发与容量

爱站默认请求完成后随机等待 10–20 秒；站长工具独立按请求起始时间间隔 3–8 秒。爱站模式要求 `WORKER_COUNT=1`，混合采集器内部也串行化整个域名任务，包括重试。一个域名通常总计 2–4 次请求。配置限制爱站最小间隔不少于 10 秒，但这只是本项目的保守策略，不是爱站公开保证的安全额度。

403、429、重定向、关键权重不完整、验证码/空结果直接停止本次任务并启用全源冷却；网络错误和无 `Retry-After` 的 5xx 最多请求两次，重试也走同一节流器。失败后默认冷却 15 分钟，连续失败逐次翻倍，最高 1 小时。HTTP `Retry-After` 的秒数或日期可以延长冷却。恢复成功后重置连续失败计数。

冷却期间先等待再从 MongoDB 领取任务，避免批量耗尽尚未请求的域名的重试次数。失败任务继续按 `COLLECTION_RETRY_DELAYS=10m,30m,1h` 延后，实际请求还必须等全源冷却结束。重试不会无限持续，进程取消时等待可立即退出。

站长工具补充请求不在任务内立即重试，失败同样触发独立的 15–60 分钟递增冷却并遵守 `Retry-After`，交由持久队列重试。两家冷却都解除后才领取下一个任务。单条混合任务请求阶段最长 4 分钟，其中补充阶段最长 3 分钟，避免长期占用运行中的任务。

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

无需 MongoDB schema 迁移，现有 JSON/BSON 字段兼容。两家权重计算口径不同，切换后应建立新的告警基线；原权重通知脚本若跨来源比较，可能把换源当作权重变化。补充字段按固定来源合并，权重不会自动回退为站长工具数值。

## 验证

```sh
go test ./...
go run ./cmd/aizhan-probe -domain www.aizhan.com
go run ./cmd/aizhan-probe -domain www.aizhan.com -with-chinaz
```

probe 默认只查询爱站；加 `-with-chinaz` 验证完整混合采集。它只查询一个域名、输出解析结果，不连接 MongoDB。不要对它开启外部并发或紧密循环。实际入库复用现有 collector/store；本次运行了全量 Go 测试和 BSON 零值/缺失字段回归，没有连接生产 MongoDB 或修改线上数据。

测试覆盖真实 HTML 摘录、注释节点、域名不匹配、0/缺失、范围单位、部分结果、403/429/重定向/验证码、`Retry-After`、有界重试、并发串行化与请求间隔、可取消冷却、响应体大小，以及配置迁移防误用。
