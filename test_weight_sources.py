import unittest
from datetime import date
from unittest.mock import Mock
from weight_change_notifier import changes_for_domain, check_weight_changes


def metric(source="aizhan", valid=True, pc=1, day="2026-09-15"):
    return dict(weight_source=source, weight_valid=valid, baidu_pc_weight=pc,
                baidu_mobile_weight=0, snapshot_date=day + "T00:00:00Z")


class WeightSourceTests(unittest.TestCase):
    def test_only_same_source_valid_values_compare(self):
        for source in ("aizhan", "chinaz"):
            self.assertEqual(len(changes_for_domain("example.com", metric(source), metric(source, pc=0))), 1)
        for previous, current in [
            (metric("chinaz"), metric("aizhan", pc=3)),
            (metric("aizhan"), metric("chinaz", pc=3)),
            (metric(valid=False), metric(pc=3)),
            (metric(), metric(valid=False, pc=3)),
            (metric(source=""), metric(pc=3)),
            (metric(), metric(valid=None, pc=3)),
            (metric(), metric(pc=None)),
        ]:
            with self.subTest(previous=previous, current=current):
                self.assertEqual(changes_for_domain("example.com", previous, current), [])

    def test_missing_optional_weight_never_becomes_zero(self):
        previous, current = metric(), metric()
        current["sogou_weight"] = 2
        self.assertEqual(changes_for_domain("example.com", previous, current), [])

    def test_exact_previous_day_and_source_filter(self):
        api = Mock()
        api.list_active_domains.return_value = [dict(id="one", domain="example.com")]
        api.metrics.return_value = [metric(day="2026-09-14"), metric(pc=3, day="2026-09-16")]
        result = check_weight_changes(api, date(2026, 9, 16), 1)
        self.assertEqual(result.domains_missing_snapshot, 1)
        self.assertFalse(result.changes)
        api.metrics.return_value = [metric("chinaz"), metric(pc=3, day="2026-09-16")]
        result = check_weight_changes(api, date(2026, 9, 16), 1)
        self.assertEqual(result.domains_skipped_source, 1)
        self.assertEqual(result.domains_compared, 0)
        api.metrics.return_value = [metric("chinaz"), metric("chinaz", pc=3, day="2026-09-16")]
        result = check_weight_changes(api, date(2026, 9, 16), 1)
        self.assertEqual(result.domains_compared, 1)
        self.assertEqual(len(result.changes), 1)
