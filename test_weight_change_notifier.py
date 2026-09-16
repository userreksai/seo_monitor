import os
import unittest
from datetime import datetime
from unittest.mock import Mock, patch
from zoneinfo import ZoneInfo

from weight_change_notifier import (
    Config,
    WeightChange,
    build_messages,
    changes_for_domain,
    send_webhook_message,
    sort_changes,
)


class WeightChangeNotifierTests(unittest.TestCase):
    def test_webhook_urls_accept_one_or_multiple_values(self):
        with patch.dict(
            os.environ,
            {
                "ENABLE_WEBHOOK_ALERT": "true",
                "WEBHOOK_URLS": (
                    "https://first.invalid/hook，https://second.invalid/hook,"
                    "https://first.invalid/hook"
                ),
            },
            clear=True,
        ):
            config = Config.from_environment()

        self.assertTrue(config.webhook_enabled)
        self.assertEqual(
            config.webhook_urls,
            ("https://first.invalid/hook", "https://second.invalid/hook"),
        )

        with patch.dict(
            os.environ,
            {"WEBHOOK_URLS": "https://only.invalid/hook"},
            clear=True,
        ):
            single = Config.from_environment()
        self.assertEqual(single.webhook_urls, ("https://only.invalid/hook",))

    def test_legacy_single_webhook_variable_is_not_used(self):
        with patch.dict(
            os.environ,
            {
                "WEBHOOK_URL": "https://legacy.invalid/hook",
                "CERTIFICATE_ALERT_WEBHOOK_URLS": "https://certificate.invalid/hook",
                "TITLE_ALERT_WEBHOOK_URLS": "https://title.invalid/hook",
            },
            clear=True,
        ):
            config = Config.from_environment()
        self.assertEqual(config.webhook_urls, ())

    def test_changes_ignore_missing_boolean_and_unchanged_values(self):
        changes = changes_for_domain(
            "example.com",
            {
                "baidu_pc_weight": 1,
                "baidu_mobile_weight": 1,
                "weight_source": "aizhan",
                "weight_valid": True,
                "sogou_weight": True,
                "bing_weight": 2,
            },
            {
                "baidu_pc_weight": 3,
                "weight_source": "aizhan",
                "weight_valid": True,
                "baidu_mobile_weight": 1,
                "sogou_weight": 2,
                "bing_weight": 2,
            },
        )
        self.assertEqual(len(changes), 1)
        self.assertEqual(changes[0].render(), "主域名：example.com  ---  百度PC权重 1 --> 3")

    def test_changes_are_grouped_and_sorted_by_largest_span(self):
        changes = [
            WeightChange("small-up.com", "baidu_pc_weight", "百度PC权重", 1, 2),
            WeightChange("big-down.com", "baidu_mobile_weight", "百度移动权重", 5, 1),
            WeightChange("big-up.com", "baidu_pc_weight", "百度PC权重", 0, 3),
            WeightChange("small-down.com", "baidu_mobile_weight", "百度移动权重", 2, 1),
        ]

        increases, decreases = sort_changes(changes)
        self.assertEqual([item.domain for item in increases], ["big-up.com", "small-up.com"])
        self.assertEqual([item.domain for item in decreases], ["big-down.com", "small-down.com"])

        message = build_messages(
            changes,
            datetime(2026, 9, 2, 7, 0, tzinfo=ZoneInfo("Asia/Shanghai")),
            12000,
            200,
        )[0]
        self.assertIn("2026-09-02 07:00:00 权重变动通知", message)
        self.assertIn("检测域名总数：200", message)
        self.assertIn("加权数据（总数：2条）", message)
        self.assertIn("降权数据（总数：2条）", message)
        self.assertLess(message.index("加权数据"), message.index("big-up.com"))
        self.assertLess(message.index("big-up.com"), message.index("small-up.com"))
        self.assertLess(message.index("small-up.com"), message.index("------------------------------------------------"))
        self.assertLess(message.index("------------------------------------------------"), message.index("降权数据"))
        self.assertLess(message.index("big-down.com"), message.index("small-down.com"))

    def test_webhook_failure_is_retried_reported_and_does_not_stop_later_groups(self):
        with patch.dict(
            os.environ,
            {
                "ENABLE_WEBHOOK_ALERT": "true",
                "WEBHOOK_URLS": "https://first.invalid/hook,https://second.invalid/hook",
                "WEBHOOK_RETRY_ALERT_URL": "https://alerts.invalid/hook",
            },
            clear=True,
        ):
            config = Config.from_environment()

        client = Mock()
        client.post.side_effect = [
            Mock(status_code=500, text='{"code": 1}'),
            Mock(status_code=200, text='{"code": 0}'),
            Mock(status_code=200, text='{"code": 0}'),
            Mock(status_code=200, text='{"code": 0}'),
        ]
        with (
            patch("weight_change_notifier.requests", client),
            patch("weight_change_notifier.time.sleep") as sleep,
        ):
            self.assertTrue(send_webhook_message(config, "test"))

        sleep.assert_called_once_with(2)
        self.assertEqual(client.post.call_count, 4)
        self.assertEqual(
            [call.args[0] for call in client.post.call_args_list],
            [
                "https://first.invalid/hook",
                "https://first.invalid/hook",
                "https://alerts.invalid/hook",
                "https://second.invalid/hook",
            ],
        )

    def test_webhook_second_failure_is_reported_and_returns_false(self):
        with patch.dict(
            os.environ,
            {
                "ENABLE_WEBHOOK_ALERT": "true",
                "WEBHOOK_URLS": "https://target.invalid/hook",
                "WEBHOOK_RETRY_ALERT_URL": "https://alerts.invalid/hook",
            },
            clear=True,
        ):
            config = Config.from_environment()

        client = Mock()
        client.post.side_effect = [
            Mock(status_code=200, text='{"code":19006,"msg":"internal error"}'),
            Mock(status_code=200, text='{"code":19006,"msg":"internal error"}'),
            Mock(status_code=200, text='{"code":0,"msg":"success"}'),
        ]
        with (
            patch("weight_change_notifier.requests", client),
            patch("weight_change_notifier.time.sleep"),
        ):
            self.assertFalse(send_webhook_message(config, "test"))

        alert_payload = client.post.call_args_list[2].kwargs["json"]
        self.assertIn(
            "地址：https://target.invalid/hook", alert_payload["content"]["text"]
        )
        self.assertIn("结果：二次发送失败", alert_payload["content"]["text"])
        self.assertIn("19006", alert_payload["content"]["text"])

    def test_large_grouped_report_stays_within_message_limit(self):
        changes = [
            WeightChange(
                f"domain-{index:03}.example.com",
                "baidu_pc_weight",
                "百度PC权重",
                0,
                (index % 5) + 1,
            )
            for index in range(60)
        ]
        messages = build_messages(
            changes,
            datetime(2026, 9, 2, 7, 0, tzinfo=ZoneInfo("Asia/Shanghai")),
            500,
            60,
        )

        self.assertGreater(len(messages), 1)
        self.assertTrue(all(len(message) <= 500 for message in messages))
        self.assertTrue(all("检测域名总数：60" in message for message in messages))
        self.assertIn("加权数据（总数：60条）", messages[0])
        self.assertTrue(
            all("加权数据（总数：60条）（续）" in message for message in messages[1:])
        )
        for index in range(60):
            domain = f"domain-{index:03}.example.com"
            self.assertEqual(sum(message.count(domain) for message in messages), 1)


if __name__ == "__main__":
    unittest.main()
