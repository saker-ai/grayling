import copy
import json
import unittest

import license_audit


class LicenseAuditTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.inventory = json.loads(license_audit.MANIFEST.read_text())

    def test_checked_inventory_blocks_release_without_gonacos_grant(self):
        license_audit.check(self.inventory, self.inventory)
        with self.assertRaisesRegex(RuntimeError, "gonacos"):
            license_audit.check(self.inventory, self.inventory, release=True)

    def test_version_drift_requires_review(self):
        modified = copy.deepcopy(self.inventory)
        next(row for row in modified["go"] if row["module"] == license_audit.GONACOS)["version"] = "v0.0.0-new"
        with self.assertRaisesRegex(RuntimeError, "changed"):
            license_audit.check(self.inventory, modified)

    def test_notice_tracks_every_runtime_module(self):
        text = license_audit.notice_text(self.inventory)
        for row in self.inventory["go"]:
            if row["runtime"]:
                self.assertIn(f"| {row['module']}", text)


if __name__ == "__main__":
    unittest.main()
