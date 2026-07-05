# Tests text-level contracts in the Ubuntu PostgreSQL async acceptance wrapper.
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "ubuntu_postgres_async_acceptance.sh"


# UbuntuAcceptanceWrapperTest checks wrapper text because the shell script is too expensive to execute in unit tests.
class UbuntuAcceptanceWrapperTest(unittest.TestCase):
    # test_python_setup_failure_disables_compose_compare_but_keeps_summary preserves failure packaging behavior.
    def test_python_setup_failure_disables_compose_compare_but_keeps_summary(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("if ! setup_python; then", text)
        self.assertIn("PREREQ_OK=0", text)
        self.assertIn("Skipping postgres_async_compare because prerequisite_check failed", text)
        self.assertIn("write_acceptance_summary", text)
        self.assertIn("package_results", text)


if __name__ == "__main__":
    unittest.main()
