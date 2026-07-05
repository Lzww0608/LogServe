# Tests text-level contracts in the Ubuntu checkpoint acceptance wrapper.
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "ubuntu_checkpoint_acceptance.sh"


# UbuntuCheckpointAcceptanceWrapperTest verifies the checkpoint wrapper keeps required evidence paths wired.
class UbuntuCheckpointAcceptanceWrapperTest(unittest.TestCase):
    # test_wrapper_keeps_summary_and_send_back_package_on_failure guards summary/package generation after failures.
    def test_wrapper_keeps_summary_and_send_back_package_on_failure(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("checkpoint_acceptance", text)
        self.assertIn("scripts/checkpoint_acceptance.sh", text)
        self.assertIn("write_acceptance_summary", text)
        self.assertIn("package_results", text)
        self.assertIn("acceptance_summary.json", text)
        self.assertIn("ubuntu-checkpoint-acceptance-package.tar.gz", text)


if __name__ == "__main__":
    unittest.main()
