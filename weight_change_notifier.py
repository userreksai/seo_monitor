#!/usr/bin/env python3
"""Compare daily SEO weights through the project API and send webhook alerts."""

from __future__ import annotations

import argparse
import json
import os
import signal
import socket
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from datetime import date, datetime, timedelta
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple
from urllib import error as urllib_error
from urllib import parse as urllib_parse
from urllib import request as urllib_request
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

try:
    import requests  # type: ignore
except ImportError:  # pragma: no cover - exercised on servers without requests
    requests = None


WEIGHT_FIELDS: Sequence[Tuple[str, str]] = (
    ("baidu_pc_weight", "百度PC权重"),
    ("baidu_mobile_weight", "百度移动权重"),
    ("sogou_weight", "搜狗权重"),
    ("bing_weight", "必应权重"),
    ("so_360_weight", "360权重"),
    ("shenma_weight", "神马权重"),
    ("pr_weight", "PR权重"),
)
REPORT_SEPARATOR = "------------------------------------------------"
WEBHOOK_RETRY_DELAY_SECONDS = 2


class HTTPResponse:
    def __init__(self, status_code: int, text: str):
        self.status_code = status_code
        self.text = text


def http_request(
    method: str,
    url: str,
    *,
    headers: Optional[Mapping[str, str]] = None,
    json_body: Optional[Mapping[str, Any]] = None,
    timeout: int = 10,
    retries: int = 1,
) -> HTTPResponse:
    """Small urllib fallback shared by API and webhook requests."""
    body = None
    request_headers = dict(headers or {})
    if json_body is not None:
        body = json.dumps(json_body, ensure_ascii=False).encode("utf-8")
        request_headers.setdefault("Content-Type", "application/json")

    last_error: Optional[Exception] = None
    for attempt in range(retries + 1):
        request = urllib_request.Request(
            url,
            data=body,
            headers=request_headers,
            method=method,
        )
        try:
            with urllib_request.urlopen(request, timeout=timeout) as response:
                return HTTPResponse(
                    response.getcode(),
                    response.read().decode("utf-8", errors="replace"),
                )
        except urllib_error.HTTPError as exc:
            text = exc.read().decode("utf-8", errors="replace")
            if exc.code < 500 or attempt >= retries:
                return HTTPResponse(exc.code, text)
            last_error = exc
        except (urllib_error.URLError, TimeoutError, OSError) as exc:
            last_error = exc

        if attempt < retries:
            time.sleep(min(2**attempt, 3))

    assert last_error is not None
    raise last_error


def parse_bool(value: Optional[str], default: bool = False) -> bool:
    if value is None or not value.strip():
        return default
    normalized = value.strip().lower()
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise ValueError(f"无效布尔值: {value!r}")


def parse_positive_int(value: str, name: str, minimum: int, maximum: int) -> int:
    try:
        parsed = int(value)
    except ValueError as exc:
        raise ValueError(f"{name} 必须是整数") from exc
    if parsed < minimum or parsed > maximum:
        raise ValueError(f"{name} 必须在 {minimum} 到 {maximum} 之间")
    return parsed


def parse_url_list(value: str) -> Tuple[str, ...]:
    """Parse WEBHOOK_URLS, preserving order and removing duplicate URLs."""
    urls = []
    seen = set()
    for item in value.replace("\n", ",").replace("，", ",").split(","):
        url = item.strip()
        if url and url not in seen:
            urls.append(url)
            seen.add(url)
    return tuple(urls)


def load_dotenv(path: Path) -> None:
    """Load simple KEY=VALUE lines without replacing exported variables."""
    if not path.is_file():
        return
    with path.open("r", encoding="utf-8-sig") as handle:
        for raw_line in handle:
            line = raw_line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            if line.startswith("export "):
                line = line[7:].lstrip()
            key, value = line.split("=", 1)
            key = key.strip()
            value = value.strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
                value = value[1:-1]
            if key:
                os.environ.setdefault(key, value)


@dataclass(frozen=True)
class Config:
    api_base_url: str
    api_token: str
    timezone: ZoneInfo
    timezone_name: str
    daily_hour: int
    daily_minute: int
    run_on_start: bool
    api_workers: int
    request_timeout: int
    max_message_chars: int
    webhook_enabled: bool
    webhook_urls: Tuple[str, ...]
    retry_alert_webhook_url: str

    @classmethod
    def from_environment(cls) -> "Config":
        timezone_name = os.getenv(
            "WEIGHT_ALERT_TIMEZONE", os.getenv("SNAPSHOT_TIMEZONE", "Asia/Shanghai")
        ).strip()
        try:
            timezone = ZoneInfo(timezone_name)
        except ZoneInfoNotFoundError as exc:
            raise ValueError(f"WEIGHT_ALERT_TIMEZONE 无效: {timezone_name}") from exc

        daily_time = os.getenv("WEIGHT_ALERT_DAILY_TIME", "07:00").strip()
        try:
            parsed_time = datetime.strptime(daily_time, "%H:%M")
        except ValueError as exc:
            raise ValueError("WEIGHT_ALERT_DAILY_TIME 必须使用 HH:MM 格式") from exc

        api_base_url = os.getenv(
            "WEIGHT_ALERT_API_BASE_URL", "http://127.0.0.1:10001"
        ).strip().rstrip("/")
        if not api_base_url:
            raise ValueError("WEIGHT_ALERT_API_BASE_URL 不能为空")

        return cls(
            api_base_url=api_base_url,
            api_token=(
                os.getenv("WEIGHT_ALERT_API_TOKEN", "").strip()
                or os.getenv("API_TOKEN", "").strip()
            ),
            timezone=timezone,
            timezone_name=timezone_name,
            daily_hour=parsed_time.hour,
            daily_minute=parsed_time.minute,
            run_on_start=parse_bool(os.getenv("WEIGHT_ALERT_RUN_ON_START"), False),
            api_workers=parse_positive_int(
                os.getenv("WEIGHT_ALERT_API_WORKERS", "8"),
                "WEIGHT_ALERT_API_WORKERS",
                1,
                32,
            ),
            request_timeout=parse_positive_int(
                os.getenv("WEIGHT_ALERT_REQUEST_TIMEOUT", "15"),
                "WEIGHT_ALERT_REQUEST_TIMEOUT",
                1,
                120,
            ),
            max_message_chars=parse_positive_int(
                os.getenv("WEIGHT_ALERT_MAX_MESSAGE_CHARS", "12000"),
                "WEIGHT_ALERT_MAX_MESSAGE_CHARS",
                500,
                30000,
            ),
            webhook_enabled=parse_bool(os.getenv("ENABLE_WEBHOOK_ALERT"), False),
            webhook_urls=parse_url_list(os.getenv("WEBHOOK_URLS", "")),
            retry_alert_webhook_url=os.getenv(
                "WEBHOOK_RETRY_ALERT_URL", ""
            ).strip(),
        )


class ResourceAPI:
    def __init__(self, config: Config):
        self.base_url = config.api_base_url
        self.timeout = config.request_timeout
        self.headers = {"Accept": "application/json"}
        if config.api_token:
            self.headers["Authorization"] = f"Bearer {config.api_token}"

    def _get(self, path: str, query: Optional[Mapping[str, str]] = None) -> Any:
        url = f"{self.base_url}{path}"
        if query:
            url = f"{url}?{urllib_parse.urlencode(query)}"
        if requests is not None:
            response = requests.get(url, headers=self.headers, timeout=self.timeout)
            status_code = response.status_code
            text = response.text
        else:
            response = http_request(
                "GET", url, headers=self.headers, timeout=self.timeout, retries=1
            )
            status_code = response.status_code
            text = response.text
        if not 200 <= status_code < 300:
            raise RuntimeError(f"资源 API 返回 HTTP {status_code}: {text[:500]}")
        try:
            return json.loads(text)
        except json.JSONDecodeError as exc:
            raise RuntimeError("资源 API 返回了无效 JSON") from exc

    def list_active_domains(self) -> List[Mapping[str, Any]]:
        payload = self._get("/api/v1/domains")
        items = payload.get("items") if isinstance(payload, dict) else None
        if not isinstance(items, list):
            raise RuntimeError("域名列表响应缺少 items 数组")
        return [item for item in items if isinstance(item, dict) and item.get("active")]

    def metrics(
        self, domain_id: str, previous_date: date, target_date: date
    ) -> List[Mapping[str, Any]]:
        path = f"/api/v1/domains/{urllib_parse.quote(domain_id, safe='')}/metrics"
        payload = self._get(
            path,
            {"from": previous_date.isoformat(), "to": target_date.isoformat()},
        )
        items = payload.get("items") if isinstance(payload, dict) else None
        if not isinstance(items, list):
            raise RuntimeError("趋势响应缺少 items 数组")
        return [item for item in items if isinstance(item, dict)]


def snapshot_date(metric: Mapping[str, Any]) -> Optional[str]:
    value = metric.get("snapshot_date")
    if not isinstance(value, str) or len(value) < 10:
        return None
    return value[:10]


@dataclass(frozen=True)
class WeightChange:
    domain: str
    field: str
    label: str
    old_value: float
    new_value: float

    @property
    def delta(self) -> float:
        return self.new_value - self.old_value

    def render(self) -> str:
        return (
            f"主域名：{self.domain}  ---  {self.label} "
            f"{self.old_value:g} --> {self.new_value:g}"
        )


def comparable_source(metric: Mapping[str, Any]) -> str:
    """Only explicitly verified daily weights can participate in alerts."""
    source = metric.get("weight_source")
    if source not in ("aizhan", "chinaz") or metric.get("weight_valid") is not True:
        return ""
    for field in ("baidu_pc_weight", "baidu_mobile_weight"):
        value = metric.get(field)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not 0 <= value <= 10:
            return ""
    return source


def comparable_snapshots(previous: Mapping[str, Any], current: Mapping[str, Any]) -> bool:
    source = comparable_source(current)
    return bool(source) and source == comparable_source(previous)


def changes_for_domain(
    domain: str,
    previous: Mapping[str, Any],
    current: Mapping[str, Any],
) -> List[WeightChange]:
    if not comparable_snapshots(previous, current):
        return []
    changes: List[WeightChange] = []
    for field, label in WEIGHT_FIELDS:
        old_value = previous.get(field)
        new_value = current.get(field)
        # Missing means the source did not return a value. It is not weight 0.
        if old_value is None or new_value is None or old_value == new_value:
            continue
        if isinstance(old_value, bool) or not isinstance(old_value, (int, float)):
            continue
        if isinstance(new_value, bool) or not isinstance(new_value, (int, float)):
            continue
        if not (0 <= old_value <= 10 and 0 <= new_value <= 10):
            continue
        changes.append(
            WeightChange(
                domain=domain,
                field=field,
                label=label,
                old_value=float(old_value),
                new_value=float(new_value),
            )
        )
    return changes


def sort_changes(changes: Iterable[WeightChange]) -> Tuple[List[WeightChange], List[WeightChange]]:
    """Return increases and decreases, each ordered by largest span first."""
    field_order = {field: index for index, (field, _label) in enumerate(WEIGHT_FIELDS)}

    def sort_key(change: WeightChange) -> Tuple[float, str, int]:
        return (
            -abs(change.delta),
            change.domain.casefold(),
            field_order.get(change.field, len(field_order)),
        )

    increases = sorted((change for change in changes if change.delta > 0), key=sort_key)
    decreases = sorted((change for change in changes if change.delta < 0), key=sort_key)
    return increases, decreases


@dataclass
class CheckResult:
    changes: List[WeightChange]
    domains_total: int
    domains_compared: int
    domains_missing_snapshot: int
    domains_failed: int
    domains_skipped_source: int = 0

    @property
    def lines(self) -> List[str]:
        """Rendered lines retained for log and caller compatibility."""
        increases, decreases = sort_changes(self.changes)
        return [change.render() for change in [*increases, *decreases]]


def check_weight_changes(
    api: ResourceAPI, target_date: date, workers: int
) -> CheckResult:
    previous_date = target_date - timedelta(days=1)
    domains = api.list_active_domains()
    changes_by_index: Dict[int, List[WeightChange]] = {}
    compared = 0
    missing = 0
    failed = 0
    skipped_source = 0

    def fetch_one(
        index: int, item: Mapping[str, Any]
    ) -> Tuple[int, str, Optional[Mapping[str, Any]], Optional[Mapping[str, Any]]]:
        domain_id = item.get("id")
        domain = item.get("domain")
        if not isinstance(domain_id, str) or not isinstance(domain, str):
            raise RuntimeError("域名记录缺少 id 或 domain")
        metrics = api.metrics(domain_id, previous_date, target_date)
        by_date = {key: metric for metric in metrics if (key := snapshot_date(metric))}
        return (
            index,
            domain,
            by_date.get(previous_date.isoformat()),
            by_date.get(target_date.isoformat()),
        )

    with ThreadPoolExecutor(max_workers=workers) as executor:
        futures = {
            executor.submit(fetch_one, index, item): (index, item.get("domain", "?"))
            for index, item in enumerate(domains)
        }
        for future in as_completed(futures):
            index, domain_hint = futures[future]
            try:
                _, domain, previous, current = future.result()
            except Exception as exc:
                failed += 1
                print(f"[ERROR] 查询域名 {domain_hint} 失败: {exc}", file=sys.stderr)
                continue
            if previous is None or current is None:
                missing += 1
                print(
                    f"[WARN] 跳过 {domain}: 缺少 {previous_date.isoformat()} 或 "
                    f"{target_date.isoformat()} 快照"
                )
                continue
            if not comparable_snapshots(previous, current):
                skipped_source += 1
                print(f"[INFO] 跳过 {domain}: 相邻两天权重来源不同或缺少有效权重")
                continue
            compared += 1
            domain_changes = changes_for_domain(domain, previous, current)
            if domain_changes:
                changes_by_index[index] = domain_changes

    changes = [
        change
        for index in sorted(changes_by_index)
        for change in changes_by_index[index]
    ]
    return CheckResult(
        changes=changes,
        domains_total=len(domains),
        domains_compared=compared,
        domains_missing_snapshot=missing,
        domains_failed=failed,
        domains_skipped_source=skipped_source,
    )


def build_messages(
    changes: Iterable[WeightChange],
    checked_at: datetime,
    max_chars: int,
    domains_total: int,
) -> List[str]:
    increases, decreases = sort_changes(changes)
    sections = [
        (f"加权数据（总数：{len(increases)}条）", increases),
        (f"降权数据（总数：{len(decreases)}条）", decreases),
    ]
    sections = [(label, items) for label, items in sections if items]
    if not sections:
        return []

    header = f"{checked_at.strftime('%Y-%m-%d %H:%M:%S')} 权重变动通知"
    detected = f"检测域名总数：{domains_total}"
    # Reserve enough room for a page suffix such as （12/12）.
    body_limit = max_chars - len(header) - 20
    pages: List[List[str]] = []
    base_lines = [detected]
    current: List[str] = list(base_lines)

    def fits(lines: Sequence[str]) -> bool:
        return len("\n".join(lines)) <= body_limit

    def flush() -> None:
        nonlocal current
        if len(current) > len(base_lines):
            pages.append(current)
        current = list(base_lines)

    for section_index, (label, items) in enumerate(sections):
        for item_index, change in enumerate(items):
            if item_index == 0:
                prefix = ([REPORT_SEPARATOR] if section_index and current else []) + [label]
            elif len(current) == 1:
                prefix = [f"{label}（续）"]
            else:
                prefix = []

            line = change.render()
            if not fits([*current, *prefix, line]):
                flush()
                prefix = [label if item_index == 0 else f"{label}（续）"]
            current.extend(prefix)
            current.append(line)

    flush()
    total = len(pages)
    messages = []
    for index, body in enumerate(pages, start=1):
        page_header = header if total == 1 else f"{header}（{index}/{total}）"
        messages.append("\n".join([page_header, *body]))
    return messages


def post_webhook_once(url: str, payload: Mapping[str, Any], timeout: int) -> Tuple[bool, str]:
    """Send one webhook request and return its business-level result."""
    try:
        headers = {"Content-Type": "application/json"}
        if requests is not None:
            response = requests.post(url, json=payload, headers=headers, timeout=timeout)
            status_code = response.status_code
            text = response.text
        else:
            response = http_request(
                "POST",
                url,
                headers=headers,
                json_body=payload,
                timeout=timeout,
                retries=0,
            )
            status_code = response.status_code
            text = response.text
    except Exception as exc:
        return False, f"请求异常: {type(exc).__name__}"

    if not 200 <= status_code < 300:
        return False, f"HTTP {status_code}: {text[:500]}"
    try:
        response_body = json.loads(text) if text.strip() else None
    except json.JSONDecodeError:
        response_body = None
    if isinstance(response_body, dict) and response_body.get("code") not in (None, 0):
        return False, f"HTTP {status_code}: {text[:500]}"
    return True, f"HTTP {status_code}: {text[:500]}"


def report_webhook_retry(
    config: Config,
    index: int,
    total: int,
    webhook_url: str,
    retry_succeeded: bool,
    first_detail: str,
    retry_detail: str,
) -> None:
    """Report a retried delivery without recursively retrying the alert webhook."""
    if not config.retry_alert_webhook_url:
        print("[WARN] WEBHOOK_RETRY_ALERT_URL 未配置，无法上报重试结果", file=sys.stderr)
        return
    result = "二次发送成功" if retry_succeeded else "二次发送失败"
    alert_message = "\n".join(
        [
            "Webhook 异常重试结果",
            "服务：权重通知",
            f"主机：{socket.gethostname()}",
            f"目标：{index}/{total}",
            f"地址：{webhook_url}",
            f"结果：{result}",
            f"首次失败：{first_detail}",
            f"二次结果：{retry_detail}",
        ]
    )
    ok, detail = post_webhook_once(
        config.retry_alert_webhook_url,
        {"msg_type": "text", "content": {"text": alert_message}},
        config.request_timeout,
    )
    if not ok:
        print(f"[ERROR] 权重 Webhook 重试结果上报失败: {detail}", file=sys.stderr)


def send_webhook_message(config: Config, message: str) -> bool:
    """Send one message to every URL, retrying each failed delivery once."""
    if not config.webhook_enabled:
        print("[INFO] Webhook 告警开关关闭，跳过发送")
        return False
    if not config.webhook_urls:
        print("[ERROR] WEBHOOK_URLS 未配置", file=sys.stderr)
        return False

    payload = {"msg_type": "text", "content": {"text": message}}
    sent = True
    total = len(config.webhook_urls)
    for index, webhook_url in enumerate(config.webhook_urls, start=1):
        first_ok, first_detail = post_webhook_once(
            webhook_url, payload, config.request_timeout
        )
        if first_ok:
            print(f"[INFO] 权重 Webhook {index}/{total} 发送成功: {first_detail}")
            continue

        print(
            f"[WARN] 权重 Webhook {index}/{total} 首次发送失败: {first_detail}；"
            "2 秒后重试",
            file=sys.stderr,
        )
        time.sleep(WEBHOOK_RETRY_DELAY_SECONDS)
        retry_ok, retry_detail = post_webhook_once(
            webhook_url, payload, config.request_timeout
        )
        if retry_ok:
            print(f"[INFO] 权重 Webhook {index}/{total} 二次发送成功: {retry_detail}")
        else:
            print(
                f"[ERROR] 权重 Webhook {index}/{total} 二次发送失败: {retry_detail}",
                file=sys.stderr,
            )
            sent = False
        report_webhook_retry(
            config,
            index,
            total,
            webhook_url,
            retry_ok,
            first_detail,
            retry_detail,
        )
    return sent


def run_check(config: Config, target_date: Optional[date], dry_run: bool) -> bool:
    checked_at = datetime.now(config.timezone)
    effective_date = target_date or checked_at.date()
    previous_date = effective_date - timedelta(days=1)
    print(
        f"[INFO] 开始权重对比: {previous_date.isoformat()} -> "
        f"{effective_date.isoformat()}"
    )
    result = check_weight_changes(ResourceAPI(config), effective_date, config.api_workers)
    print(
        "[INFO] 对比完成: "
        f"域名总数={result.domains_total}, 已比较={result.domains_compared}, "
        f"缺少快照={result.domains_missing_snapshot}, 查询失败={result.domains_failed}, "
        f"来源或有效性不符={result.domains_skipped_source}, "
        f"变动项={len(result.changes)}"
    )
    if not result.changes:
        if result.domains_failed:
            print("[WARN] 已成功比较的域名没有权重变动；存在查询失败，请检查日志")
        else:
            print("[INFO] 符合比较条件的数据没有权重变动，不发送通知")
        return result.domains_failed == 0

    messages = build_messages(
        result.changes,
        checked_at,
        config.max_message_chars,
        result.domains_total,
    )
    if dry_run:
        for message in messages:
            print("\n[DRY-RUN] 以下消息未发送:\n" + message)
        return result.domains_failed == 0

    sent = True
    for message in messages:
        # Attempt every chunk even if an earlier chunk failed.
        sent = send_webhook_message(config, message) and sent
    return sent and result.domains_failed == 0


def next_run(now: datetime, config: Config) -> datetime:
    candidate = now.replace(
        hour=config.daily_hour, minute=config.daily_minute, second=0, microsecond=0
    )
    if candidate <= now:
        candidate += timedelta(days=1)
    return candidate


def run_daemon(config: Config, dry_run: bool) -> int:
    stopped = threading.Event()

    def stop(_signum: int, _frame: Any) -> None:
        stopped.set()

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    if config.run_on_start:
        try:
            if not run_check(config, None, dry_run):
                print("[ERROR] 启动时权重检查未完全成功", file=sys.stderr)
        except Exception as exc:
            print(f"[ERROR] 启动时权重检查失败: {exc}", file=sys.stderr)

    while not stopped.is_set():
        now = datetime.now(config.timezone)
        scheduled_at = next_run(now, config)
        delay = max(0.0, (scheduled_at - now).total_seconds())
        print(
            f"[INFO] 下次执行时间: {scheduled_at.strftime('%Y-%m-%d %H:%M:%S')} "
            f"({config.timezone_name})"
        )
        if stopped.wait(delay):
            break
        try:
            if not run_check(config, scheduled_at.date(), dry_run):
                print("[ERROR] 定时权重检查未完全成功", file=sys.stderr)
        except Exception as exc:
            print(f"[ERROR] 定时权重检查失败: {exc}", file=sys.stderr)

    print("[INFO] 权重通知服务已停止")
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="每日七项权重变动通知")
    parser.add_argument(
        "--once", action="store_true", help="立即执行一次后退出，供手动运行"
    )
    parser.add_argument(
        "--date", type=date.fromisoformat, help="指定对比日期 YYYY-MM-DD（默认今天）"
    )
    parser.add_argument(
        "--dry-run", action="store_true", help="显示通知内容但不调用 Webhook"
    )
    parser.add_argument(
        "--env-file",
        default=str(Path(__file__).resolve().parent / ".env"),
        help="环境变量文件路径（默认通知程序目录 .env）",
    )
    args = parser.parse_args()
    if args.date and not args.once:
        parser.error("--date 必须和 --once 一起使用")
    return args


def main() -> int:
    args = parse_args()
    load_dotenv(Path(args.env_file))
    try:
        config = Config.from_environment()
    except ValueError as exc:
        print(f"[ERROR] 配置错误: {exc}", file=sys.stderr)
        return 2

    if args.once:
        try:
            return 0 if run_check(config, args.date, args.dry_run) else 1
        except Exception as exc:
            print(f"[ERROR] 权重检查失败: {exc}", file=sys.stderr)
            return 1
    return run_daemon(config, args.dry_run)


if __name__ == "__main__":
    raise SystemExit(main())
