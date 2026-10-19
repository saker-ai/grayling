import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import license_audit


class LicenseAuditTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.inventory = json.loads(license_audit.MANIFEST.read_text())

    def test_checked_inventory_accepts_pinned_apache_evidence(self):
        license_audit.check(self.inventory, self.inventory)
        license_audit.check(self.inventory, self.inventory, release=True)

    def test_version_drift_requires_review(self):
        modified = copy.deepcopy(self.inventory)
        next(row for row in modified["go"] if row["module"] == license_audit.GONACOS)["version"] = "v0.0.0-new"
        with self.assertRaisesRegex(RuntimeError, "changed"):
            license_audit.check(self.inventory, modified)

    def test_missing_gonacos_notice_blocks_release(self):
        modified = copy.deepcopy(self.inventory)
        gonacos = next(row for row in modified["go"] if row["module"] == license_audit.GONACOS)
        gonacos["evidence"] = [entry for entry in gonacos["evidence"] if entry["file"] != "NOTICE"]
        with tempfile.TemporaryDirectory() as directory:
            notices = Path(directory) / "THIRD_PARTY_NOTICES.md"
            notices.write_text(license_audit.notice_text(modified))
            with mock.patch.object(license_audit, "NOTICES", notices):
                with self.assertRaisesRegex(RuntimeError, "LICENSE and NOTICE"):
                    license_audit.check(modified, modified, release=True)

    def test_isc_requires_notice_and_disclaimer(self):
        license_text = b"""Copyright (c) 2025 Example
Permission to use, copy, modify, and distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.
THE SOFTWARE IS PROVIDED AS IS AND THE AUTHOR DISCLAIMS ALL WARRANTIES.
"""
        self.assertEqual(license_audit.license_type(license_text), "ISC")
        self.assertEqual(license_audit.license_type(license_text.replace(
            b"copyright notice and this permission notice appear in all copies", b"NOTICE REMOVED")), "REVIEW")

    def test_notice_tracks_every_runtime_module(self):
        text = license_audit.notice_text(self.inventory)
        for row in self.inventory["go"]:
            if row["runtime"]:
                self.assertIn(f"| {row['module']}", text)


if __name__ == "__main__":
    unittest.main()
