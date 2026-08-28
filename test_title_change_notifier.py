import os
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import patch
from zoneinfo import ZoneInfo

from title_change_notifier import (
    Config,
    TitleChange,
    TitleFailure,
    TitleReport,
    build_messages,
    fetch_report,
    load_window_start,
    save_window_end,
)


class FakeAPI:
    def list_titles(self, status):
        if status == "changed":
            return [
                {
                    "domain": {"id": "507f1f77bcf86cd799439011", "domain": "seo.chinaz.com"},
                    "title": {"changed_at": "2026-08-28T02:00:00Z", "change_count": 1},
                }
            ]
        if status == "failed":
            return [
                {
                    "domain": {"id": "507f1f77bcf86cd799439012", "domain": "123.com"},
                    "title": {
                        "last_attempt_at": "2026-08-28T03:00:00Z",
                        "error_message": "timeout",
                    },
                }
            ]
        raise AssertionError(status)

    def title_history(self, domain_id):
        self.assert_domain_id = domain_id
        return [
            {
                "domain": "seo.chinaz.com",
                "old_title": "SEO综合查询 - 站长工具",
                "new_title": "SEO综合查询",
                "changed_at": "2026-08-28T02:00:00Z",
            },
            {
                "domain": "seo.chinaz.com",
                "old_title": "旧事件",
                "new_title": "不会再次通知",
                "changed_at": "2026-08-26T02:00:00Z",
            },
        ]


class TitleChangeNotifierTests(unittest.TestCase):
    def test_config_reuses_api_settings_but_requires_distinct_webhook(self):
        values = {
            "WEIGHT_ALERT_API_BASE_URL": "http://127.0.0.1:10001",
            "WEIGHT_ALERT_API_TOKEN": "shared-api-token",
            "WEIGHT_ALERT_TIMEZONE": "Asia/Shanghai",
            "WEIGHT_ALERT_DAILY_TIME": "07:00",
            "WEIGHT_ALERT_API_WORKERS": "8",
            "ENABLE_WEBHOOK_ALERT": "true",
            "WEBHOOK_URL": "https://weight.invalid/hook",
            "ENABLE_TITLE_WEBHOOK_ALERT": "true",
            "TITLE_ALERT_WEBHOOK_URL": "https://title.invalid/hook",
        }
        with patch.dict(os.environ, values, clear=True):
            config = Config.from_environment()
        self.assertEqual(config.api_token, "shared-api-token")
        self.assertEqual(config.webhook_url, "https://title.invalid/hook")
        self.assertTrue(config.webhook_enabled)

    def test_fetch_report_only_returns_events_in_window(self):
        report = fetch_report(
            FakeAPI(),
            datetime(2026, 8, 27, tzinfo=timezone.utc),
            datetime(2026, 8, 28, 4, tzinfo=timezone.utc),
            4,
        )
        self.assertEqual(len(report.changes), 1)
        self.assertEqual(report.changes[0].new_title, "SEO综合查询")
        self.assertEqual([item.domain for item in report.failures], ["123.com"])

    def test_message_matches_requested_sections(self):
        report = TitleReport(
            changes=[
                TitleChange(
                    domain="seo.chinaz.com",
                    old_title="SEO综合查询 - 站长工具",
                    new_title="SEO综合查询",
                    changed_at=datetime(2026, 8, 28, 2, tzinfo=timezone.utc),
                )
            ],
            failures=[
                TitleFailure(
                    domain="123.com",
                    attempted_at=datetime(2026, 8, 28, 3, tzinfo=timezone.utc),
                )
            ],
        )
        messages = build_messages(
            report,
            datetime(2026, 8, 28, 11, 43, tzinfo=ZoneInfo("Asia/Shanghai")),
            12000,
        )
        self.assertEqual(len(messages), 1)
        self.assertIn("2026-08-28 11:43:00 网站标题变动通知", messages[0])
        self.assertIn("总数量：2（标题变更 1，获取失败 1）", messages[0])
        self.assertIn("标题发生变更（1）", messages[0])
        self.assertIn(
            "域名：https://seo.chinaz.com --- 标题：SEO综合查询 - 站长工具 --> SEO综合查询",
            messages[0],
        )
        self.assertIn("标题获取失败（1）\n域名：https://123.com", messages[0])
        self.assertNotIn("（无）", messages[0])

    def test_empty_report_does_not_create_a_message(self):
        self.assertEqual(
            build_messages(
                TitleReport(changes=[], failures=[]),
                datetime.now(timezone.utc),
                12000,
            ),
            [],
        )

    def test_failure_domains_do_not_have_blank_lines_between_them(self):
        report = TitleReport(
            changes=[],
            failures=[
                TitleFailure(
                    domain="first.com",
                    attempted_at=datetime(2026, 8, 28, 3, tzinfo=timezone.utc),
                ),
                TitleFailure(
                    domain="second.com",
                    attempted_at=datetime(2026, 8, 28, 3, tzinfo=timezone.utc),
                ),
                TitleFailure(
                    domain="third.com",
                    attempted_at=datetime(2026, 8, 28, 3, tzinfo=timezone.utc),
                ),
            ],
        )
        message = build_messages(
            report,
            datetime(2026, 8, 28, 11, 43, tzinfo=ZoneInfo("Asia/Shanghai")),
            12000,
        )[0]
        self.assertIn(
            "域名：https://first.com\n域名：https://second.com\n域名：https://third.com",
            message,
        )
        self.assertIn("总数量：3（标题变更 0，获取失败 3）", message)
        self.assertIn("标题获取失败（3）", message)
        self.assertNotIn("\n\n域名：", message)

    def test_large_report_is_split_to_webhook_limit(self):
        report = TitleReport(
            changes=[
                TitleChange(
                    domain=f"domain-{index:03}.example.com",
                    old_title="旧标题" * 80,
                    new_title="新标题" * 80,
                    changed_at=datetime(2026, 8, 28, 2, tzinfo=timezone.utc),
                )
                for index in range(20)
            ],
            failures=[],
        )
        messages = build_messages(
            report,
            datetime(2026, 8, 28, 11, 43, tzinfo=ZoneInfo("Asia/Shanghai")),
            500,
        )
        self.assertGreater(len(messages), 1)
        self.assertTrue(all(len(message) <= 500 for message in messages))

    def test_state_file_round_trip(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.dict(
                os.environ,
                {"TITLE_ALERT_STATE_FILE": str(Path(directory) / "state.json")},
                clear=True,
            ):
                config = Config.from_environment()
            checked_at = datetime(2026, 8, 28, 4, tzinfo=timezone.utc)
            save_window_end(config, checked_at)
            self.assertEqual(load_window_start(config, checked_at), checked_at)


if __name__ == "__main__":
    unittest.main()
