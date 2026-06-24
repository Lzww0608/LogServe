import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "ubuntu_project_acceptance.sh"


class UbuntuProjectAcceptanceWrapperTest(unittest.TestCase):
    def test_wrapper_runs_core_and_subsuite_acceptance_paths(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("go_test_physical_compaction", text)
        self.assertIn("go_race_logstore", text)
        self.assertIn("bash scripts/run_experiment.sh", text)
        self.assertIn("bash scripts/ubuntu_checkpoint_acceptance.sh", text)
        self.assertIn("bash scripts/ubuntu_postgres_async_acceptance.sh", text)
        self.assertIn("scripts/summarize_ubuntu_project_acceptance.py", text)
        self.assertIn("package_results", text)
        self.assertIn("ubuntu-project-acceptance-package.tar.gz", text)


if __name__ == "__main__":
    unittest.main()