# 爱站、站长每日独立快照

## 行为

`SOURCE_PROVIDER=aizhan` 默认每天分别采集 `www.aizhan.com` 和 `seo.chinaz.com`。爱站成功也会执行站长权重任务，两站成功就分别写库，不互相等待。APPPC 排名、分类、注册人、邮箱、到期日继续由独立站长补充任务维护。显式配置 `SOURCE_PROVIDER=chinaz` 仍为原有站长单源模式。

爱站沿用 Agent 获取页面、普通失败后 master 复测的链路；无需改 Agent、无需新增 API 私钥。每家任务各自重试，普通 HTTP 400、超时、空页或字段不全只影响当前任务；明确限流或封禁才冷却该数据源。

## 数据库方案

保留 `domain_daily_metrics` 的 `{domain, snapshot_date}` 唯一索引，每域名每天一条文档，新增两个嵌套快照。这样既保留两站数据，也兼容现有查询，不需要拆库或重建全部历史。

```json
{
  "domain": "example.com",
  "snapshot_date": "2026-09-16T00:00:00Z",
  "weight_source": "aizhan",
  "weight_valid": true,
  "baidu_pc_weight": 3,
  "weight_snapshots": {
    "aizhan": {
      "valid": true,
      "last_attempt_at": "2026-09-16T02:00:00Z",
      "metric": {
        "weight_source": "aizhan",
        "weight_valid": true,
        "baidu_pc_weight": 3,
        "baidu_mobile_weight": 2,
        "collected_at": "2026-09-16T02:00:00Z",
        "source_url": "https://www.aizhan.com/cha/example.com/"
      }
    },
    "chinaz": {
      "valid": true,
      "last_attempt_at": "2026-09-16T02:01:00Z",
      "metric": {
        "weight_source": "chinaz",
        "weight_valid": true,
        "baidu_pc_weight": 1,
        "baidu_mobile_weight": 0,
        "collected_at": "2026-09-16T02:01:00Z",
        "source_url": "https://seo.chinaz.com/example.com"
      }
    }
  }
}
```

示例省略其他字段；每份 `metric` 还保存该站返回的流量、其他权重、反链、域龄、响应摘要和采集路径等主采集字段。补充资料仍保存在顶层，来源记录在 `supplemental_*`，不混入权重快照。

- 两份快照分别更新，MongoDB 原子更新管道同时计算顶层兼容结果：有效爱站优先，其次有效站长。完成先后顺序不改变优先级，也不会覆盖另一站快照。
- 同一天同一站重新采集成功会更新该站快照；保存的是每日结果，不是每次请求的版本归档。不同日期的快照独立，仍遵守原有历史保留配置。
- 某站刷新失败时保留它当天最后成功的 `metric`，但外层 `valid=false`，同时记录 `last_attempt_at` 和 `error_message`。有效性以外层 `valid` 为准，内层 `metric` 是上次成功响应。另一站不受影响。
- 两站都无效时，顶层保留已有展示值但 `weight_valid=false`；缺失权重不会伪造为 0。当天从未写入任何结果时，失败先保存在该源任务集合，不伪造一份成功快照。

## 自动迁移与升级

服务启动在后台为已知历史来源补建对应快照。旧数据只有爱站就只迁爱站，只有站长就只迁站长；无法确认来源或有效性的历史不猜测。未采集过的另一个来源历史数据无法恢复。

迁移不阻塞健康检查，可重复运行；已有快照不会被迁移覆盖。实时写入也会先保留旧来源结果，因此不依赖后台迁移先完成。任务的新集合与索引在启动时自动创建。现有 MongoDB validator 允许新增属性，仍可用 `SKIP_MONGO_INIT=1` 更新，无需清库、删除索引或执行破坏性数据库变更。自定义过额外严格 validator 的部署需要同时允许 `weight_snapshots` 对象。

升级后定时采集会自动执行双源任务。若当天已经采集完成，可提交普通采集补齐尚未完成的来源：

```sh
curl -fsS -X POST 'http://127.0.0.1:10001/api/v1/collect' \
  -H "Authorization: Bearer $SEO_API_TOKEN" \
  -H 'Content-Type: application/json' -d '{}'
```

`{"force":true}` 会重新安排两站权重和补充任务；已经 queued/running 的同源任务仍去重，不重复创建。接口的 `queued` 继续按域名统计，进度接口的总量按权重任务统计。

## 接口与进度

- 默认 `/api/v1/domains/{id}/metrics` 返回兼容结果以及两份 `weight_snapshots`。
- 加 `?source=aizhan` 或 `?source=chinaz` 查询指定来源的每日历史；可与原有 `from`、`to` 参数组合。没有该源快照的日期不返回，不借用另一来源或更早日期。失败快照返回 `weight_valid=false`，保留错误状态以便诊断。
- 最新列表同时返回 `weight_collections.aizhan/chinaz`；旧 `collection` 跟随当前展示的数据来源。
- `/api/v1/jobs?source=aizhan`、`source=chinaz`、`source=chinaz_supplement` 分别查看爱站权重、站长权重、站长补充任务。各自有持久化队列、重试、恢复与终态。
- `/api/v1/collect/progress` 顶层为两站权重合计，`sources.aizhan`、`sources.chinaz` 为分源进度，`supplement` 仍单独统计。498 个域名对应 996 个权重任务；成功、最终失败和取消都计入完成，等待重试不计完成。

当前前端仍显示兼容结果及其来源；本次只修改采集后端，没有新增前端的双列展示。

## 频率与通知边界

爱站默认单 worker、请求间隔 10–20 秒。站长权重和补充 worker 共享单请求并发、3–8 秒间隔与站长冷却，避免两个队列叠加请求速率；它们的任务重试和数据写入独立。站长权重每域名最多页面加动态权重两次请求，补充最多三次，双源保留会增加站长请求总量。498 个域名最多约 2490 次站长请求，不含重试；平均间隔 5.5 秒对应约 3.8 小时，仅为间隔量级估算，网络、重试和冷却会延长耗时。不要通过增加实例提高频率，当前限流为单进程。

**本次不修改 `weight_change_notifier.py`。** 现有通知继续读取顶层优先结果，按原有同来源且有效的相邻两天规则比较。两站都保存后，通知脚本并不会自动同时比较两套历史；以后如需按源独立通知，可以使用上述 source 参数或读取快照，再单独调整通知程序。

## 验证

```sh
go test ./...
go vet ./...
# 可选：专用测试 MongoDB；仅创建并删除随机命名 seo_dual_test_* 数据库
SEO_TEST_MONGO_URI='mongodb://127.0.0.1:27017' go test ./internal/store -run TestDualSourceMongoIntegration -count=1 -v
```

集成测试覆盖写入顺序、并发插入、补充字段保留、单源及双源失败、恢复、历史迁移幂等和分源进度。
