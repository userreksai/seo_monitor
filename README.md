# 域名权重每日采集服务（Go + MongoDB）

本项目每天从 `https://seo.chinaz.com/{domain}` 获取截图红框中的数据，写入一个**全新的 MongoDB 数据库 `seo_monitor`**。现有的 `monitor_nodes` 不会被使用或修改。

采集字段包括：

- 全网流量原文及区间（`traffic_text`、`traffic_min`、`traffic_max`）
- 百度 PC、百度移动、搜狗、必应、360、神马、PR 权重
- APPPC PC 排名、网站分类、反向链接数
- 注册人/机构、注册人邮箱、域名年龄、预计域名年龄天数、过期日期
- 数据来源、采集时间、原始页面 SHA-256（便于审计和排查页面变化）

## 为什么继续使用 MongoDB

你已经有 MongoDB，几百个域名每天一次的规模对 MongoDB 很小。按 500 个域名计算，一年约 18.25 万条快照，普通单机即可承载。趋势图查询使用 `{domain_id: 1, snapshot_date: -1}` 索引；同一域名同一天只保留一条数据，由 `{domain: 1, snapshot_date: 1}` 唯一索引强制保证。

新库包含三个集合：

| 集合 | 用途 |
| --- | --- |
| `domains` | 域名 CRUD、启用/归档状态 |
| `domain_daily_metrics` | 每日指标快照，趋势图数据源 |
| `collection_jobs` | 持久任务队列、失败原因和执行状态 |

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
db.getCollectionInfos().forEach(x => printjson({name: x.name, validator: x.options.validator}))
```

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
API_TOKEN=一段足够长的随机字符串
```

默认每天北京时间 `02:15` 排队采集，应用启动时会补采当天尚未成功的域名：

```dotenv
SNAPSHOT_TIMEZONE=Asia/Shanghai
COLLECT_CRON=15 2 * * *
QUEUE_ON_START=true
```

默认只有一个采集 Worker，每次请求随机间隔 3–8 秒。即使域名扩到几百个，也建议先保持低并发，避免给来源站造成压力或触发限流。

## 三、服务器源码部署（不使用 Docker）

以下以 Ubuntu/Debian Linux 为例。服务器需安装 Go 1.23 或更高版本、`mongosh`，MongoDB 可以在本机或内网另一台服务器。

### 1. 上传源码并编译

```bash
sudo useradd --system --home /opt/seo-monitor --shell /usr/sbin/nologin seo-monitor
sudo mkdir -p /opt/seo-monitor
sudo chown -R "$USER":seo-monitor /opt/seo-monitor
cd /opt/seo-monitor
# 将本项目全部源码上传到这里后执行：
sh scripts/build.sh
```

`scripts/build.sh` 会依次下载 Go 依赖、运行测试并生成 `bin/seo-monitor`。也可以手动执行：

```bash
go mod download
go test ./...
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/seo-monitor ./cmd/server
```

### 2. 初始化独立数据库

```bash
cd /opt/seo-monitor
mongosh 'mongodb://管理员:密码@127.0.0.1:27017/admin?authSource=admin' --file scripts/mongo-init.js
```

再按第一节的命令创建只拥有 `seo_monitor` 读写权限的应用账号。

### 3. 写运行配置

```bash
cd /opt/seo-monitor
cp .env.example .env
chmod 600 .env
```

修改 `.env` 中的 MongoDB URI 和 `API_TOKEN`。若 MongoDB 在另一台内网服务器，把 `127.0.0.1` 改为其内网地址；不要向公网开放 27017 端口。

### 4. 注册 systemd 服务

```bash
sudo cp deploy/seo-monitor.service /etc/systemd/system/seo-monitor.service
sudo chown -R seo-monitor:seo-monitor /opt/seo-monitor
sudo systemctl daemon-reload
sudo systemctl enable --now seo-monitor
sudo systemctl status seo-monitor
journalctl -u seo-monitor -f
```

更新代码时：

```bash
cd /opt/seo-monitor
sudo systemctl stop seo-monitor
sudo -u seo-monitor sh scripts/build.sh
sudo systemctl start seo-monitor
```

Go 服务直接监听 `.env` 的 `HTTP_ADDR`（默认 `127.0.0.1:8080`）。如需公网访问，建议由 Nginx/Caddy 反向代理到它并启用 HTTPS；MongoDB 只允许本机或内网访问。

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

主要接口：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/domains` | 新增域名 |
| `POST` | `/api/v1/domains/bulk` | 批量新增，最多 1000 个 |
| `GET` | `/api/v1/domains` | 域名列表 |
| `GET` | `/api/v1/domains/{id}` | 域名详情 |
| `PATCH` | `/api/v1/domains/{id}` | 修改显示名或启用状态 |
| `DELETE` | `/api/v1/domains/{id}` | 软删除/归档 |
| `POST` | `/api/v1/domains/{id}/collect` | 手动排队采集单个域名 |
| `POST` | `/api/v1/collect` | 排队采集全部启用域名 |
| `GET` | `/api/v1/domains/{id}/latest` | 最新快照 |
| `GET` | `/api/v1/domains/{id}/metrics?from=2026-01-01&to=2026-07-10` | 趋势数据 |
| `GET` | `/api/v1/jobs?status=failed&limit=100` | 任务与错误记录 |

新增一个域名：

```powershell
$headers = @{ Authorization = "Bearer 你的API_TOKEN" }
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/api/v1/domains `
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
internal/httpapi/            域名 CRUD、采集、趋势 API
scripts/mongo-init.js        新建 seo_monitor 库、字段校验、索引
scripts/build.sh             源码测试与编译
deploy/seo-monitor.service   Linux systemd 服务配置
```
