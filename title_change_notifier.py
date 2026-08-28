#!/usr/bin/env python3
"""Send new domain-title changes and failures to a dedicated Lark webhook."""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional, Sequence, Tuple
from urllib import error as urllib_error
from urllib import parse as urllib_parse
from urllib import request as urllib_request
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

try:
    import requests  # type: ignore
except ImportError:  # pragma: no cover - used on servers without requests
    requests = None


REPORT_SEPARATOR = "━━━━━━━━━━━━━━━━━━━━"


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
    body = None
    request_headers = dict(headers or {})
    if json_body is not None:
        body = json.dumps(json_body, ensure_ascii=False).encode("utf-8")
        request_headers.setdefault("Content-Type", "application/json")

    last_error: Optional[Exception] = None
    for attempt in range(retries + 1):
        request = urllib_request.Request(
            url, data=body, headers=request_headers, method=method
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


def first_environment(*names: str, default: str = "") -> str:
    for name in names:
        value = os.getenv(name)
        if value is not None and value.strip():
            return value.strip()
    return default


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


@dataclass(frozen=True)
class Config:
    api_base_url: str
    api_path: str
    api_token: str
    timezone: ZoneInfo
    timezone_name: str
    daily_hour: int
    daily_minute: int
    run_on_start: bool
    request_timeout: int
    max_message_chars: int
    api_workers: int
    webhook_enabled: bool
    webhook_url: str
    state_file: Path
    initial_lookback_hours: int

    @classmethod
    def from_environment(cls) -> "Config":
        timezone_name = first_environment(
            "TITLE_ALERT_TIMEZONE",
            "WEIGHT_ALERT_TIMEZONE",
            "SNAPSHOT_TIMEZONE",
            default="Asia/Shanghai",
        )
        try:
            configured_timezone = ZoneInfo(timezone_name)
        except ZoneInfoNotFoundError as exc:
            raise ValueError(f"TITLE_ALERT_TIMEZONE 无效: {timezone_name}") from exc

        daily_time = first_environment(
            "TITLE_ALERT_DAILY_TIME", "WEIGHT_ALERT_DAILY_TIME", default="07:00"
        )
        try:
            parsed_time = datetime.strptime(daily_time, "%H:%M")
        except ValueError as exc:
            raise ValueError("TITLE_ALERT_DAILY_TIME 必须使用 HH:MM 格式") from exc

        api_base_url = first_environment(
            "TITLE_ALERT_API_BASE_URL",
            "WEIGHT_ALERT_API_BASE_URL",
            default="http://127.0.0.1:10001",
        ).rstrip("/")
        if not api_base_url:
            raise ValueError("TITLE_ALERT_API_BASE_URL 不能为空")

        api_path = first_environment(
            "TITLE_ALERT_API_PATH", default="/api/v1/titles"
        )
        if not api_path.startswith("/"):
            api_path = "/" + api_path

        return cls(
            api_base_url=api_base_url,
            api_path=api_path.rstrip("/"),
            api_token=first_environment(
                "TITLE_ALERT_API_TOKEN", "WEIGHT_ALERT_API_TOKEN", "API_TOKEN"
            ),
            timezone=configured_timezone,
            timezone_name=timezone_name,
            daily_hour=parsed_time.hour,
            daily_minute=parsed_time.minute,
            run_on_start=parse_bool(
                os.getenv("TITLE_ALERT_RUN_ON_START", os.getenv("WEIGHT_ALERT_RUN_ON_START")),
                False,
            ),
            request_timeout=parse_positive_int(
                first_environment(
                    "TITLE_ALERT_REQUEST_TIMEOUT",
                    "WEIGHT_ALERT_REQUEST_TIMEOUT",
                    default="15",
                ),
                "TITLE_ALERT_REQUEST_TIMEOUT",
                1,
                120,
            ),
            max_message_chars=parse_positive_int(
                first_environment(
                    "TITLE_ALERT_MAX_MESSAGE_CHARS",
                    "WEIGHT_ALERT_MAX_MESSAGE_CHARS",
                    default="12000",
                ),
                "TITLE_ALERT_MAX_MESSAGE_CHARS",
                500,
                30000,
            ),
            api_workers=parse_positive_int(
                first_environment(
                    "TITLE_ALERT_API_WORKERS",
                    "WEIGHT_ALERT_API_WORKERS",
                    default="8",
                ),
                "TITLE_ALERT_API_WORKERS",
                1,
                32,
            ),
            webhook_enabled=parse_bool(
                os.getenv("ENABLE_TITLE_WEBHOOK_ALERT"), False
            ),
            webhook_url=first_environment("TITLE_ALERT_WEBHOOK_URL"),
            state_file=Path(
                first_environment(
                    "TITLE_ALERT_STATE_FILE",
                    default="/var/lib/seo-title-alert/state.json",
                )
            ),
            initial_lookback_hours=parse_positive_int(
                first_environment(
                    "TITLE_ALERT_INITIAL_LOOKBACK_HOURS", default="24"
                ),
                "TITLE_ALERT_INITIAL_LOOKBACK_HOURS",
                1,
                720,
            ),
        )


class ResourceAPI:
    def __init__(self, config: Config):
        self.base_url = config.api_base_url
        self.path = config.api_path
        self.timeout = config.request_timeout
        self.headers = {"Accept": "application/json"}
        if config.api_token:
            self.headers["Authorization"] = f"Bearer {config.api_token}"

    def _get(self, path: str, query: Mapping[str, str]) -> Any:
        suffix = urllib_parse.urlencode(query)
        url = f"{self.base_url}{path}"
        if suffix:
            url += "?" + suffix
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
            raise RuntimeError(f"标题 API 返回 HTTP {status_code}: {text[:500]}")
        try:
            return json.loads(text)
        except json.JSONDecodeError as exc:
            raise RuntimeError("标题 API 返回了无效 JSON") from exc

    def list_titles(self, status: str) -> List[Mapping[str, Any]]:
        page = 1
        limit = 100
        records: List[Mapping[str, Any]] = []
        seen = set()
        while True:
            payload = self._get(
                self.path,
                {"q": "", "status": status, "page": str(page), "limit": str(limit)},
            )
            if not isinstance(payload, dict) or not isinstance(payload.get("items"), list):
                raise RuntimeError("标题列表响应缺少 items 数组")
            items = payload["items"]
            for item in items:
                if not isinstance(item, dict):
                    raise RuntimeError("标题列表 items 中存在非对象记录")
                domain_id, _domain = title_identity(item)
                if domain_id not in seen:
                    seen.add(domain_id)
                    records.append(item)
            raw_total = payload.get("total")
            total = raw_total if isinstance(raw_total, int) and raw_total >= 0 else None
            if not items or len(items) < limit or (total is not None and len(records) >= total):
                break
            page += 1
        return records

    def title_history(self, domain_id: str) -> List[Mapping[str, Any]]:
        safe_id = urllib_parse.quote(domain_id, safe="")
        payload = self._get(f"{self.path}/{safe_id}/history", {"limit": "100"})
        if not isinstance(payload, dict) or not isinstance(payload.get("items"), list):
            raise RuntimeError("标题变更历史响应缺少 items 数组")
        result = []
        for item in payload["items"]:
            if not isinstance(item, dict):
                raise RuntimeError("标题变更历史中存在非对象记录")
            result.append(item)
        return result


@dataclass(frozen=True)
class TitleChange:
    domain: str
    old_title: str
    new_title: str
    changed_at: datetime


@dataclass(frozen=True)
class TitleFailure:
    domain: str
    attempted_at: datetime


@dataclass(frozen=True)
class TitleReport:
    changes: List[TitleChange]
    failures: List[TitleFailure]

    def empty(self) -> bool:
        return not self.changes and not self.failures


def parse_api_datetime(value: Any, field_name: str) -> datetime:
    if not isinstance(value, str) or not value.strip():
        raise RuntimeError(f"{field_name} 缺少有效时间")
    normalized = value.strip()
    if normalized.endswith(("Z", "z")):
        normalized = normalized[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(normalized)
    except ValueError as exc:
        raise RuntimeError(f"{field_name} 时间格式无效: {value!r}") from exc
    if parsed.tzinfo is None:
        raise RuntimeError(f"{field_name} 时间缺少时区: {value!r}")
    return parsed.astimezone(timezone.utc)


def title_identity(item: Mapping[str, Any]) -> Tuple[str, str]:
    domain_record = item.get("domain")
    if not isinstance(domain_record, dict):
        raise RuntimeError("标题记录缺少 domain 对象")
    domain_id = domain_record.get("id")
    domain = domain_record.get("domain")
    if not isinstance(domain_id, str) or not domain_id.strip():
        raise RuntimeError("标题记录缺少 domain.id")
    if not isinstance(domain, str) or not domain.strip():
        raise RuntimeError("标题记录缺少 domain.domain")
    return domain_id.strip(), domain.strip()


def title_state(item: Mapping[str, Any]) -> Mapping[str, Any]:
    value = item.get("title")
    if not isinstance(value, dict):
        raise RuntimeError("标题记录缺少 title 对象")
    return value


def changed_candidate(item: Mapping[str, Any]) -> Tuple[str, datetime]:
    domain_id, _domain = title_identity(item)
    changed_at = parse_api_datetime(title_state(item).get("changed_at"), "changed_at")
    return domain_id, changed_at


def parse_change(item: Mapping[str, Any]) -> TitleChange:
    domain = item.get("domain")
    old_title = item.get("old_title")
    new_title = item.get("new_title")
    if not isinstance(domain, str) or not domain.strip():
        raise RuntimeError("标题变更记录缺少 domain")
    if not isinstance(old_title, str) or not isinstance(new_title, str):
        raise RuntimeError(f"{domain} 的标题变更记录缺少原标题或新标题")
    return TitleChange(
        domain=domain.strip(),
        old_title=old_title.strip(),
        new_title=new_title.strip(),
        changed_at=parse_api_datetime(item.get("changed_at"), "changed_at"),
    )


def parse_failure(item: Mapping[str, Any]) -> TitleFailure:
    _domain_id, domain = title_identity(item)
    state = title_state(item)
    error_message = state.get("error_message")
    if not isinstance(error_message, str) or not error_message.strip():
        raise RuntimeError(f"{domain} 的失败记录缺少 error_message")
    return TitleFailure(
        domain=domain,
        attempted_at=parse_api_datetime(state.get("last_attempt_at"), "last_attempt_at"),
    )


def in_window(value: datetime, start: datetime, end: datetime) -> bool:
    return start < value <= end


def fetch_report(
    api: ResourceAPI, start: datetime, end: datetime, workers: int
) -> TitleReport:
    changed_items = api.list_titles("changed")
    candidate_ids = []
    for item in changed_items:
        domain_id, changed_at = changed_candidate(item)
        if in_window(changed_at, start, end):
            candidate_ids.append(domain_id)

    changes: List[TitleChange] = []
    if candidate_ids:
        with ThreadPoolExecutor(max_workers=min(workers, len(candidate_ids))) as pool:
            futures = {pool.submit(api.title_history, domain_id): domain_id for domain_id in candidate_ids}
            for future in as_completed(futures):
                for raw_change in future.result():
                    change = parse_change(raw_change)
                    if in_window(change.changed_at, start, end):
                        changes.append(change)

    unique_changes: Dict[Tuple[str, str, str, str], TitleChange] = {}
    for change in changes:
        key = (
            change.domain.casefold(),
            change.old_title,
            change.new_title,
            change.changed_at.isoformat(),
        )
        unique_changes[key] = change

    failures = []
    for item in api.list_titles("failed"):
        failure = parse_failure(item)
        if in_window(failure.attempted_at, start, end):
            failures.append(failure)

    unique_failures: Dict[Tuple[str, str], TitleFailure] = {}
    for failure in failures:
        unique_failures[(failure.domain.casefold(), failure.attempted_at.isoformat())] = failure

    return TitleReport(
        changes=sorted(
            unique_changes.values(), key=lambda item: (item.changed_at, item.domain.casefold())
        ),
        failures=sorted(
            unique_failures.values(), key=lambda item: (item.attempted_at, item.domain.casefold())
        ),
    )


def domain_url(domain: str) -> str:
    value = domain.strip()
    if value.lower().startswith(("http://", "https://")):
        return value
    return "https://" + value


def truncate(value: str, maximum: int = 500) -> str:
    normalized = " ".join(value.split())
    if len(normalized) <= maximum:
        return normalized
    return normalized[: maximum - 1] + "…"


def build_messages(
    report: TitleReport, checked_at: datetime, max_chars: int
) -> List[str]:
    if report.empty():
        return []

    header = f"{checked_at.strftime('%Y-%m-%d %H:%M:%S')} 网站标题变动通知"
    total = len(report.changes) + len(report.failures)
    summary = (
        f"总数量：{total}（标题变更 {len(report.changes)}，"
        f"获取失败 {len(report.failures)}）"
    )
    content_limit = max_chars - 24
    pages: List[List[str]] = []
    current = [header, summary]

    def rendered_length(lines: Sequence[str]) -> int:
        return len("\n".join(lines))

    def flush() -> None:
        nonlocal current
        if len(current) > 2:
            if rendered_length([*current, REPORT_SEPARATOR]) <= content_limit:
                current.append(REPORT_SEPARATOR)
            pages.append(current)
        current = [header, summary]

    groups = (
        (
            f"标题发生变更（{len(report.changes)}）",
            [
                f"域名：{domain_url(item.domain)} --- 标题：{truncate(item.old_title)} --> {truncate(item.new_title)}"
                for item in report.changes
            ],
        ),
        (
            f"标题获取失败（{len(report.failures)}）",
            [f"域名：{domain_url(item.domain)}" for item in report.failures],
        ),
    )

    for label, entries in groups:
        if not entries:
            continue
        first_in_section = True
        entry_limit = max(
            80,
            content_limit
            - rendered_length(
                [header, summary, REPORT_SEPARATOR, f"{label}（续）"]
            )
            - 1,
        )
        for raw_entry in entries:
            entry = truncate(raw_entry, entry_limit)
            prefix = [REPORT_SEPARATOR, label] if first_in_section else []
            if rendered_length([*current, *prefix, entry]) > content_limit and len(current) > 2:
                flush()
                prefix = [REPORT_SEPARATOR, f"{label}（续）"]
            current.extend([*prefix, entry])
            first_in_section = False
    flush()

    if len(pages) == 1:
        return ["\n".join(pages[0])]
    total = len(pages)
    return [
        "\n".join([f"{header}（{index}/{len(pages)}）", *lines[1:]])
        for index, lines in enumerate(pages, start=1)
    ]


def send_webhook_message(config: Config, message: str) -> bool:
    if not config.webhook_enabled:
        print("[ERROR] ENABLE_TITLE_WEBHOOK_ALERT 未开启", file=sys.stderr)
        return False
    if not config.webhook_url:
        print("[ERROR] TITLE_ALERT_WEBHOOK_URL 未配置", file=sys.stderr)
        return False

    payload = {"msg_type": "text", "content": {"text": message}}
    headers = {"Content-Type": "application/json"}
    try:
        if requests is not None:
            response = requests.post(
                config.webhook_url,
                json=payload,
                headers=headers,
                timeout=config.request_timeout,
            )
            status_code = response.status_code
            text = response.text
        else:
            response = http_request(
                "POST",
                config.webhook_url,
                headers=headers,
                json_body=payload,
                timeout=config.request_timeout,
                retries=1,
            )
            status_code = response.status_code
            text = response.text
        print(f"[INFO] 标题 Webhook 状态码: {status_code}")
        if not 200 <= status_code < 300:
            print(f"[ERROR] 标题 Webhook 响应: {text[:1000]}", file=sys.stderr)
            return False
        try:
            response_body = json.loads(text) if text.strip() else None
        except json.JSONDecodeError:
            response_body = None
        if isinstance(response_body, dict) and response_body.get("code") not in (None, 0):
            print(f"[ERROR] 标题 Webhook 响应: {text[:1000]}", file=sys.stderr)
            return False
        return True
    except Exception as exc:
        print(f"[ERROR] 标题 Webhook 请求失败: {exc}", file=sys.stderr)
        return False


def load_window_start(config: Config, window_end: datetime) -> datetime:
    if not config.state_file.is_file():
        return window_end - timedelta(hours=config.initial_lookback_hours)
    try:
        payload = json.loads(config.state_file.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise RuntimeError(f"无法读取标题通知状态文件 {config.state_file}: {exc}") from exc
    if not isinstance(payload, dict):
        raise RuntimeError(f"标题通知状态文件格式无效: {config.state_file}")
    return parse_api_datetime(payload.get("last_successful_check_at"), "last_successful_check_at")


def save_window_end(config: Config, window_end: datetime) -> None:
    try:
        config.state_file.parent.mkdir(parents=True, exist_ok=True)
        temporary = config.state_file.with_name(config.state_file.name + ".tmp")
        temporary.write_text(
            json.dumps(
                {
                    "last_successful_check_at": window_end.astimezone(timezone.utc)
                    .isoformat()
                    .replace("+00:00", "Z")
                },
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )
        os.chmod(temporary, 0o600)
        os.replace(temporary, config.state_file)
    except OSError as exc:
        raise RuntimeError(f"无法写入标题通知状态文件 {config.state_file}: {exc}") from exc


def run_check(config: Config, dry_run: bool) -> bool:
    checked_at = datetime.now(config.timezone)
    window_end = checked_at.astimezone(timezone.utc)
    window_start = load_window_start(config, window_end)
    if window_start >= window_end:
        window_start = window_end - timedelta(seconds=1)
    print(
        f"[INFO] 查询标题事件区间: {window_start.isoformat()} 至 {window_end.isoformat()}"
    )
    report = fetch_report(ResourceAPI(config), window_start, window_end, config.api_workers)
    print(f"[INFO] 新标题变更: {len(report.changes)}，新检测失败: {len(report.failures)}")
    messages = build_messages(report, checked_at, config.max_message_chars)

    if dry_run:
        if not messages:
            print("[DRY-RUN] 当前时间区间没有需要通知的标题事件")
        for message in messages:
            print("\n[DRY-RUN] 以下消息未发送：\n" + message)
        return True

    if not messages:
        save_window_end(config, window_end)
        print("[INFO] 没有标题变更或失败，不发送通知")
        return True

    sent = True
    for message in messages:
        sent = send_webhook_message(config, message) and sent
    if sent:
        save_window_end(config, window_end)
    return sent


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
            if not run_check(config, dry_run):
                print("[ERROR] 启动时标题通知未完全成功", file=sys.stderr)
        except Exception as exc:
            print(f"[ERROR] 启动时标题通知失败: {exc}", file=sys.stderr)

    while not stopped.is_set():
        now = datetime.now(config.timezone)
        scheduled_at = next_run(now, config)
        delay = max(0.0, (scheduled_at - now).total_seconds())
        print(
            f"[INFO] 下次标题通知时间: {scheduled_at.strftime('%Y-%m-%d %H:%M:%S')} "
            f"({config.timezone_name})"
        )
        if stopped.wait(delay):
            break
        try:
            if not run_check(config, dry_run):
                print("[ERROR] 定时标题通知未完全成功", file=sys.stderr)
        except Exception as exc:
            print(f"[ERROR] 定时标题通知失败: {exc}", file=sys.stderr)

    print("[INFO] 标题通知服务已停止")
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="网站标题变更与失败通知")
    parser.add_argument("--once", action="store_true", help="立即执行一次后退出")
    parser.add_argument(
        "--dry-run", action="store_true", help="显示通知内容但不调用 Webhook 或更新游标"
    )
    parser.add_argument(
        "--env-file",
        default=str(Path(__file__).resolve().parent / ".env"),
        help="环境变量文件路径（默认使用脚本所在目录的 .env）",
    )
    return parser.parse_args()


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
            return 0 if run_check(config, args.dry_run) else 1
        except Exception as exc:
            print(f"[ERROR] 标题通知失败: {exc}", file=sys.stderr)
            return 1
    return run_daemon(config, args.dry_run)


if __name__ == "__main__":
    raise SystemExit(main())
