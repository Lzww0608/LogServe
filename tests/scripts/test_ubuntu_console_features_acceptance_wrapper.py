import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "ubuntu_console_features_6_10_acceptance.sh"


class UbuntuConsoleFeaturesAcceptanceWrapperTest(unittest.TestCase):
    def test_wrapper_delegates_to_full_console_acceptance_with_feature_result_dir(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("LOGSERVE_CONSOLE_FEATURES_ACCEPTANCE_ID", text)
        self.assertIn("reports/ubuntu-console-features-6-10-$RUN_ID", text)
        self.assertIn("features-6-10-$RUN_ID", text)
        self.assertIn("LOGSERVE_CONSOLE_ACCEPTANCE_DIR", text)
        self.assertIn("ubuntu_console_acceptance.sh", text)
        self.assertIn("logserve_reject_dated_name", text)


if __name__ == "__main__":
    unittest.main()