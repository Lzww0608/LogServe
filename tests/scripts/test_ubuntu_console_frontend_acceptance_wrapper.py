# Tests delegation from the frontend/admin console acceptance wrapper.
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "ubuntu_console_frontend_acceptance.sh"


# UbuntuConsoleFrontendAcceptanceWrapperTest pins the frontend/admin wrapper delegation contract.
class UbuntuConsoleFrontendAcceptanceWrapperTest(unittest.TestCase):
    # test_wrapper_delegates_to_full_console_acceptance_with_frontend_result_dir checks canonical frontend result naming.
    def test_wrapper_delegates_to_full_console_acceptance_with_frontend_result_dir(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("LOGSERVE_CONSOLE_FRONTEND_ACCEPTANCE_ID", text)
        self.assertIn("reports/ubuntu-console-frontend-$RUN_ID", text)
        self.assertIn("frontend-$RUN_ID", text)
        self.assertIn("LOGSERVE_CONSOLE_ACCEPTANCE_DIR", text)
        self.assertIn("ubuntu_console_acceptance.sh", text)
        self.assertIn("logserve_reject_dated_name", text)


if __name__ == "__main__":
    unittest.main()