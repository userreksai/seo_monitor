# 域名权重每日采集服务（Go + MongoDB）

本项目每天从 `https://seo.chinaz.com/{domain}` 获取截图红框中的数据，写入一个**全新的 MongoDB 数据库 `seo_monitor`**。现有的 `monitor_nodes` 不会被使用或修改。

采集字段包括：

- 全网流量原文及区间（`traffic_text`、`traffic_min`、`traffic_max`）
- 百度 PC、百度移动、搜狗、必应、360、神马、PR 权重
- APPPC PC 排名、网站分类、反向链接数
- 注册人/机构、注册人邮箱、域名年龄、预计域名年龄天数、过期日期
- 数据来源、采集时间、原始页面 SHA-256（便于审计和排查页面变化）

## 为什么继续使用 MongoDB

你已经有 MongoDB，几百个域名每天一次的规模对 MongoDB 很小。服务默认保留当天及往前 60 个自然日的数据。趋势图查询使用 `{domain_id: 1, snapshot_date: -1}` 索引；同一域名同一天只保留一条数据，由 `{domain: 1, snapshot_date: 1}` 唯一索引强制保证。

新库包含五个集合：

| 集合 | 用途 |
| --- | --- |
| `domains` | 域名 CRUD、启用/归档状态 |
| `domain_daily_metrics` | 每日指标快照，趋势图数据源 |
| `collection_jobs` | 持久任务队列、失败原因和执行状态 |
| `domain_certificates` | 每个启用域名最近一次 TLS 证书检测结果 |
| `domain_certificate_history` | TLS 证书每次轮询记录和失败原因 |

API 删除域名时执行软删除（`active=false`），不会删除历史趋势。应用使用 MongoDB 原子抢占任务；重启后卡在 `running` 的旧任务会重新排队。

## 一、创建新库、字段校验和索引

MongoDB 不需要预先逐列创建字段；数据库在第一次创建集合时生成。项目通过 JSON Schema Validator 限定字段类型，通过索引限定唯一值。

有账号密码时执行：

```powershell
mongosh "mongodb://用户名:密码@127.0.0.1:27017/admin?authSource=admin" --file .\scripts\mongo-init.js
```

无认证的本地 MongoDB：

```powershell
mongosh "mongodb://127.0.0.1:27017" --file .\scripts\mongo-init.js
```

完整可重复执行的创建命令见 `scripts/mongo-init.js`。脚本第一条有效命令是：

```javascript
db = db.getSiblingDB("seo_monitor");
```

因此它不会操作 `monitor_nodes`。执行后检查：

```javascript
use seo_monitor
show collections
db.domains.getIndexes()
db.domain_daily_metrics.getIndexes()
db.domain_certificates.getIndexes()
db.domain_certificate_history.getIndexes()
db.getCollectionInfos().forEach(x => printjson({name: x.name, validator: x.options.validator}))
```

即使不手动运行脚本，默认的 `ENSURE_INDEXES=true` 也会在服务启动时自动创建
`domain_certificates`、`domain_certificate_history` 集合和索引；手动脚本额外配置 JSON Schema Validator。服务启动后会在后台检测
所有启用域名的 443 端口，并按 `CERTIFICATE_CRON` 定时更新。可通过
`CERTIFICATE_WORKERS` 和 `CERTIFICATE_TIMEOUT` 控制并发数及单域名超时时间。
`CERTIFICATE_RETENTION_DAYS` 默认为 `7`：当天及往前 7 个自然日的轮询结果会保留；旧失败记录会删除，
但每个域名最近一次成功检测向前 7 天的成功记录会额外保留。即使新一周全部检测失败，也不会清掉上一周最后的有效证书数据。

主控节点无法解析或连接某些域名时，可以配置一个或多个
[`SituationAwareness-agent`](https://github.com/userreksai/SituationAwareness-agent) 作为证书检测回退节点：

```dotenv
CERTIFICATE_AGENT_URLS=http://10.0.1.10:8002,http://10.0.2.10:8002
CERTIFICATE_AGENT_TOKEN=与各Agent的AGENT_SHARED_TOKEN一致
CERTIFICATE_AGENT_TIMEOUT=15s
CERTIFICATE_AGENT_MAX_CONCURRENT=4
```

主控始终先执行本地检测；本地失败后才会从不同 Agent 开始轮询，任一 Agent 成功即把证书、实际连接地址和 Agent 名称写入 `domain_certificates`。每个 Agent 的并发由 `CERTIFICATE_AGENT_MAX_CONCURRENT` 单独限制，避免批量检测时触发节点的 429 保护。所有节点失败时，错误信息会同时保留主控及各 Agent 的失败原因。未配置 `CERTIFICATE_AGENT_URLS` 时行为与以前完全一致。Agent 的 `AGENT_MAX_TIMEOUT` 必须不小于 `CERTIFICATE_AGENT_TIMEOUT`，`AGENT_MAX_CONCURRENT` 应不小于主控的单 Agent 并发值，并建议通过防火墙只允许主控访问 Agent 的 8002 端口。

如果 MongoDB 已启用权限控制，建议给应用创建只操作新库的账号：

```javascript
use seo_monitor
db.createUser({
  user: "seo_monitor_app",
  pwd: passwordPrompt(),
  roles: [{ role: "readWrite", db: "seo_monitor" }]
})
```

然后使用连接串：

```text
mongodb://seo_monitor_app:密码@127.0.0.1:27017/seo_monitor?authSource=seo_monitor
```

## 二、配置

复制配置文件：

```powershell
Copy-Item .env.example .env
```

至少修改：

```dotenv
MONGODB_URI=mongodb://seo_monitor_app:密码@127.0.0.1:27017/seo_monitor?authSource=seo_monitor
MONGODB_DATABASE=seo_monitor
# 不需要脚本调用时留空；启用时至少使用 32 字节随机值并定期轮换。
API_TOKEN=
DEFAULT_ADMIN_USERNAME=admin
DEFAULT_ADMIN_PASSWORD=请替换为随机生成的强密码
AUTH_SESSION_TTL=8h
AUTH_COOKIE_SECURE=true
AUTH_LOGIN_PAIR_MAX_FAILURES=5
AUTH_LOGIN_IP_MAX_FAILURES=10
AUTH_LOGIN_FAILURE_WINDOW=15m
AUTH_LOGIN_LOCKOUT=15m
AUTH_TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
```

默认每天北京时间 `02:15` 排队采集，应用启动时会补采当天尚未成功的域名：

```dotenv
SNAPSHOT_TIMEZONE=Asia/Shanghai
COLLECT_CRON=15 2 * * *
QUEUE_ON_START=true
```

每日指标和轮询任务日志会在服务启动时、以及每天定时采集前清理。`snapshot_date < 当天-RETENTION_DAYS` 的记录会被删除，边界日保留；默认值 60 即保留当天及往前 60 个自然日：

```dotenv
RETENTION_DAYS=60
```

### 域名文件

权重采集和证书检测分别读取独立文件：

- 权重：`/usr/local/seo_monitor/domains.json`
- 证书：`/usr/local/seo_monitor/certificate_domains.json`

systemd 的工作目录为 `/usr/local/seo_monitor`。一键安装脚本会保留服务器已有的两份列表；旧版本首次升级且尚无证书文件时，会复制当前 `domains.json` 作为初始 `certificate_domains.json`，因此升级不会漏掉原有证书域名。之后两份文件可以独立修改。推荐使用标准 JSON 数组：

```json
[
  "123.com",
  "222.com",
  "4444.com",
  "baibai.com"
]
```

两份文件都支持对象格式 `{"domains":["123.com","222.com"]}`，以及兼容未加引号、带尾逗号的宽松格式。权重文件在服务启动和每天定时采集前读取；证书文件在服务启动及每次证书检测前读取。重复域名会被忽略，非法域名会让程序明确报错。

权重文件执行增量导入，从文件删除域名不会删除历史数据；需要停用权重采集时使用域名 CRUD API。证书文件则是证书页面和检测任务的当前完整名单，从中删除域名会停止后续证书检测，但不会删除历史证书记录。可通过环境变量分别指定其他文件，相对路径仍以程序当前工作目录为基准：

```dotenv
DOMAINS_FILE=domains.json
CERTIFICATE_DOMAINS_FILE=certificate_domains.json
```

默认只有一个采集 Worker，每次请求随机间隔 3–8 秒。即使域名扩到几百个，也建议先保持低并发，避免给来源站造成压力或触发限流。

## 三、服务器源码部署（不使用 Docker）

以下以 Ubuntu/Debian Linux 为例。服务器需安装 Go 1.25 或更高版本、`mongosh`，MongoDB 可以在本机或内网另一台服务器。

### 一键安装或更新（推荐）

下面的完整脚本会拉取 `main` 到 `/usr/local/seo_monitor`、保留已有 `domains.json`、`certificate_domains.json` 和 `.env`（缺少时补入必要默认项）、初始化 MongoDB、测试编译、安装 systemd 服务、启动并检查健康状态：

```bash
curl -fsSL https://raw.githubusercontent.com/userreksai/seo_monitor/main/install.sh \
  -o /tmp/install-seo-monitor.sh
sudo sh /tmp/install-seo-monitor.sh
```

MongoDB 开启认证时，分别传入用于建库的管理员连接串和应用运行连接串。用户名或密码中的特殊字符应先做 URL 编码：

```bash
sudo env \
  MONGO_ADMIN_URI='mongodb://管理员:密码@127.0.0.1:27017/admin?authSource=admin' \
  MONGODB_URI='mongodb://seo_monitor_app:密码@127.0.0.1:27017/seo_monitor?authSource=seo_monitor' \
  sh /tmp/install-seo-monitor.sh
```

如果 `/usr/local/seo_monitor` 只有域名 JSON、`.env`，或者是以前手动上传且尚未包含 `.git` 的项目源码，脚本会把整个旧目录保存为 `/usr/local/seo_monitor.backup.YYYYMMDDHHMMSS`，克隆正式仓库，并自动迁移旧配置；不会直接覆盖或删除旧文件。目录出现不属于项目的未知文件时仍会安全停止。

如果已经手动初始化数据库，可以增加 `SKIP_MONGO_INIT=1`。即使跳过 `mongo-init.js`，Go 服务启动时仍会自动创建 `users`、`auth_sessions` 集合、所需索引以及默认管理员；已存在的管理员不会在重启时被覆盖。脚本必须以 root 运行，并要求服务器已安装 `git`、`go`、`systemctl`；未跳过数据库初始化时还要求 `mongosh`。首次安装会分别生成随机 API Token 和 64 位随机管理员密码，写入权限为 `600` 的 `/usr/local/seo_monitor/.env`，并只在首次安装结束时显示密码；后续运行不会覆盖或再次显示它。

### 1. 上传源码并编译

```bash
sudo useradd --system --home /usr/local/seo_monitor --shell /usr/sbin/nologin seo-monitor
sudo mkdir -p /usr/local/seo_monitor
sudo chown -R "$USER":seo-monitor /usr/local/seo_monitor
cd /usr/local/seo_monitor
# 将本项目全部源码上传到这里后执行：
[ -f domains.json ] || cp domains.example.json domains.json
[ -f certificate_domains.json ] || cp certificate_domains.example.json certificate_domains.json
sh build.sh
```

根目录 `build.sh` 会自动定位项目目录，因此可以从任意工作目录调用；它会依次下载 Go 依赖、运行测试并生成 `bin/seo-monitor`。`sh scripts/build.sh` 仍然兼容。也可以手动执行：

```bash
go mod download
go test ./...
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/seo-monitor ./cmd/server
```

### 2. 初始化独立数据库

```bash
cd /usr/local/seo_monitor
mongosh 'mongodb://管理员:密码@127.0.0.1:27017/admin?authSource=admin' --file scripts/mongo-init.js
```

再按第一节的命令创建只拥有 `seo_monitor` 读写权限的应用账号。

### 3. 写运行配置

```bash
cd /usr/local/seo_monitor
cp .env.example .env
chmod 600 .env
```

修改 `.env` 中的 MongoDB URI 和 `API_TOKEN`。若 MongoDB 在另一台内网服务器，把 `127.0.0.1` 改为其内网地址；不要向公网开放 27017 端口。

### 4. 注册 systemd 服务

```bash
sudo cp deploy/seo-monitor.service /etc/systemd/system/seo-monitor.service
sudo chown -R seo-monitor:seo-monitor /usr/local/seo_monitor
sudo systemctl daemon-reload
sudo systemctl enable --now seo-monitor
sudo systemctl status seo-monitor
journalctl -u seo-monitor -f
```

更新代码时：

```bash
cd /usr/local/seo_monitor
sudo systemctl stop seo-monitor
sudo -u seo-monitor sh build.sh
sudo systemctl start seo-monitor
```

Go 服务直接监听 `.env` 的 `HTTP_ADDR`（默认 `127.0.0.1:10001`）。不要把该端口直接开放到公网；由只监听回环地址的 Web 服务转发 API，再由 Nginx 在 443 终止 TLS。MongoDB 只允许本机或内网访问，绝不能向公网开放 27017。

### 公网登录安全

登录接口同时执行两层失败计数：同一“IP + 账号”在 15 分钟内失败 5 次，或同一 IP 针对任意账号累计失败 10 次，都会锁定 15 分钟并返回 `429` 与 `Retry-After`。计数在密码哈希校验前预留并发名额，可阻止并发请求绕过阈值。登录成功只清除该 IP/账号组合的失败计数，不会清除该 IP 的撞库/喷洒计数。

浏览器会话使用 `HttpOnly; Secure; SameSite=Strict` Cookie，写操作还要求 CSRF 防护头；前端不再把会话令牌持久化到 `localStorage`。公网 HTTPS 必须保持 `AUTH_COOKIE_SECURE=true`，只有本机纯 HTTP 开发时才可临时设为 `false`。

登录后可在页面右上角选择“修改密码”。新密码要求 12–72 字节；修改成功会注销该账号的所有现有会话。已有数据库若仍使用早期默认密码，请上线前立即修改，因为更新 `.env` 不会覆盖数据库中已创建账号的密码。

也可以在服务器本机交互式重置（密码不会出现在命令行参数或 shell 历史中）：

```sh
sudo sh /usr/local/seo_monitor/scripts/change-password.sh admin
```

默认仍要求新密码为 12–72 字节。如果因兼容性必须临时使用 8–11 字节的旧密码，可由服务器 root 管理员显式绕过最短长度限制（网页端不会开放该能力）：

```bash
sudo sh /usr/local/seo_monitor/scripts/change-password.sh --allow-weak-password admin
```

该选项会显示安全警告；公网部署应配合登录限速，并尽快恢复 12 字节以上的唯一强密码。

`AUTH_TRUSTED_PROXY_CIDRS` 只填写真正能直连 Go 后端的代理地址。标准部署中只有本机 Web 代理能连接后端，因此保留 `127.0.0.1/32,::1/128`。后端从代理链右向左查找第一个不可信地址作为客户端 IP，直接访问后端时会忽略伪造的 `X-Forwarded-For`。

公网入口请使用前端仓库中的 `deploy/nginx-seo-monitor.conf.example`：它包含登录专用限速（每 IP 平均 5 次/分钟）、通用 API 限速、并发连接上限、1 MiB 请求体限制、超时、TLS 1.2/1.3、HSTS、CSP 和其他安全响应头。部署后只在防火墙/安全组开放必需的 SSH 来源以及 TCP 80/443；`8889`、`10001`、`27017` 均应仅限本机或内网。

应用内失败计数存放在单个 Go 进程内，服务重启会清空。如果以后部署多个后端实例，应把封禁状态迁移到 Redis 等共享存储，或在统一的 WAF/CDN 层执行；对于高风险后台，优先再加 VPN/IP 白名单或支持 MFA 的身份代理。

Windows 服务器也可直接源码编译运行：

```powershell
go mod download
go test ./...
New-Item -ItemType Directory -Force .\bin | Out-Null
go build -trimpath -ldflags="-s -w" -o .\bin\seo-monitor.exe .\cmd\server
$env:MONGODB_URI="mongodb://seo_monitor_app:密码@127.0.0.1:27017/seo_monitor?authSource=seo_monitor"
$env:MONGODB_DATABASE="seo_monitor"
$env:API_TOKEN="你的随机令牌"
.\bin\seo-monitor.exe
```

域名页面的增删改查通过下方 Go API 完成，后续前端不需要直接连接 MongoDB。运维人员可使用 `mongosh` 或 MongoDB Compass 检查数据。

## 四、API

设置公共请求头：

```text
Authorization: Bearer <API_TOKEN>
Content-Type: application/json
```

同源浏览器页面默认使用安全会话 Cookie；`Authorization` 方式保留给脚本和受信任的服务调用。若不需要这类调用，可将 `API_TOKEN` 留空以减少长期静态凭据。

主要接口：

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/v1/auth/login` | 账号密码登录（无需 Token） |
| `GET` | `/api/v1/auth/me` | 获取当前登录账号 |
| `POST` | `/api/v1/auth/logout` | 退出并注销当前会话 |
| `POST` | `/api/v1/auth/password` | 修改当前账号密码并注销全部会话 |
| `POST` | `/api/v1/domains` | 新增域名 |
| `POST` | `/api/v1/domains/bulk` | 批量新增，最多 1000 个 |
| `GET` | `/api/v1/domains` | 域名列表 |
| `GET` | `/api/v1/domains/{id}` | 域名详情 |
| `PATCH` | `/api/v1/domains/{id}` | 修改显示名或启用状态 |
| `DELETE` | `/api/v1/domains/{id}` | 软删除/归档 |
| `POST` | `/api/v1/domains/{id}/collect` | 手动排队采集单个域名 |
| `POST` | `/api/v1/collect` | 排队采集全部启用域名 |
| `GET` | `/api/v1/collect/progress` | 当天采集进度、成功数和失败数 |
| `GET` | `/api/v1/domains/{id}/latest` | 最新快照 |
| `GET` | `/api/v1/domains/{id}/metrics?from=2026-01-01&to=2026-07-10` | 趋势数据 |
| `GET` | `/api/v1/search?field=domain&q=example&status=failed&sort_by=traffic&sort_order=asc&page=1&limit=50` | 按指定字段搜索域名及最新指标；`status=failed` 筛选最近采集失败；`sort_by` 支持 `traffic`、`weight`、`rank`，`sort_order` 支持 `asc`、`desc` |
| `POST` | `/api/v1/certificates/refresh` | 启动全部证书检测 |
| `GET` | `/api/v1/certificates/progress` | 当前/最近一次证书检测进度 |
| `GET` | `/api/v1/jobs?status=failed&limit=100` | 任务与错误记录 |

新增一个域名：

```powershell
$headers = @{ Authorization = "Bearer 你的API_TOKEN" }
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:10001/api/v1/domains `
  -Headers $headers -ContentType application/json `
  -Body '{"domain":"qiyeshangpu.com","display_name":"大众信息网"}'
```

批量新增：

```json
{
  "domains": ["qiyeshangpu.com", "example.com", "https://www.example.org/path"]
}
```

域名会自动去协议、路径、末尾点并转换为小写；中文域名会转换为 Punycode。

强制重采当天数据（成功后通过 upsert 覆盖当天快照，不会增加重复行）：

```json
{ "force": true }
```

趋势接口已按日期升序返回，前端可直接将 `snapshot_date` 作为 X 轴，把各权重或流量作为多条序列。

## 五、直接查询 MongoDB

```javascript
use seo_monitor

// 最新数据
db.domain_daily_metrics.find({domain: "qiyeshangpu.com"}).sort({snapshot_date: -1}).limit(1)

// 日期区间趋势
db.domain_daily_metrics.find({
  domain: "qiyeshangpu.com",
  snapshot_date: {
    $gte: ISODate("2026-01-01T00:00:00Z"),
    $lte: ISODate("2026-07-10T00:00:00Z")
  }
}).sort({snapshot_date: 1})

// 查看失败任务
db.collection_jobs.find({status: "failed"}).sort({queued_at: -1}).limit(100)
```

## 六、采集稳定性与边界

- 数据来自第三方页面，站点结构变化时解析器会明确报错，不会把验证码页错误写成全 0 数据。
- 代码不会绕过验证码。出现验证码、403 或持续限流时，应降低频率、联系数据提供方或改接正式 API。
- 正式运行前请确认目标站点的服务条款允许自动采集。站长之家页面也注明各项结果仅供分析参考。
- 权重为 `0` 与“来源未返回”不同：数据库中真实 0 保存为 `0`，缺失值不写入对应字段。
- `domain_age_days` 是按年/月折算的近似值，展示应优先使用来源原文 `domain_age_text`。

## 七、代码结构

```text
cmd/server/                  程序入口、定时器、优雅退出
internal/scraper/            站长工具 HTML 采集与解析
internal/store/              MongoDB、唯一索引、持久任务队列
internal/collector/          Worker 与日期逻辑
internal/certificate/        TLS 证书读取与并发刷新
internal/httpapi/            域名 CRUD、采集、趋势 API
scripts/mongo-init.js        新建 seo_monitor 库、字段校验、索引
build.sh                     源码测试与编译（可从任意目录调用）
install.sh                   拉取、初始化、编译并安装 systemd 服务
domains.example.json         域名列表示例；安装时生成本地 domains.json
certificate_domains.example.json 证书域名列表示例；安装时生成本地 certificate_domains.json
scripts/build.sh             兼容入口，转发到根目录 build.sh
deploy/seo-monitor.service   Linux systemd 服务配置
```
