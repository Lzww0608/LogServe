# Tests text-level contracts in the Ubuntu console acceptance wrapper.
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "ubuntu_console_acceptance.sh"


# UbuntuConsoleAcceptanceWrapperTest uses text checks to pin the expensive console acceptance orchestration contract.
class UbuntuConsoleAcceptanceWrapperTest(unittest.TestCase):
    # test_docker_runtime_installs_executor_requirements ensures the worker image carries Python executor dependencies.
    def test_docker_runtime_installs_executor_requirements(self):
        text = (ROOT / "deployments" / "Dockerfile").read_text(encoding="utf-8")

        self.assertIn("COPY executor/python/requirements.txt /tmp/logserve-executor-requirements.txt", text)
        self.assertIn("pip install --no-cache-dir -r /tmp/logserve-executor-requirements.txt", text)

    # test_wrapper_covers_console_build_compose_probe_and_packaging guards local gates, Compose runtime, probe, and package order.
    def test_wrapper_covers_console_build_compose_probe_and_packaging(self):
        text = SCRIPT.read_text(encoding="utf-8")

        self.assertIn("go test -count=1 ./cmd/logserve-web ./internal/webapi", text)
        self.assertIn("web_npm_ci", text)
        self.assertIn("web_build", text)
        self.assertIn("compose build", text)
        self.assertIn("compose up -d postgres nats minio logd control web worker", text)
        self.assertIn("scripts/console_http_probe.py", text)
        self.assertIn("scripts/summarize_console_acceptance.py", text)
        self.assertIn("console-acceptance-package.tar.gz", text)
        self.assertIn("--exclude './console.env'", text)

        # The wrapper should mark Compose as started immediately after compose up is attempted so cleanup can run.
        compose_up = 'run_step docker_compose_up "$RESULT_DIR/docker_compose_up.log" compose up -d postgres nats minio logd control web worker'
        self.assertIn(compose_up + "\n      COMPOSE_STARTED=1", text)
        self.assertNotIn(compose_up + '\n      if [ "$LAST_STEP_CODE" -eq 0 ]; then\n        COMPOSE_STARTED=1', text)

        # The wrapper writes a pre-package summary, records packaging, then rewrites the final summary including package evidence.
        first_summary = text.index("write_acceptance_summary\npackage_results")
        final_summary = text.index("package_results\nwrite_acceptance_summary")
        self.assertLess(first_summary, final_summary)


if __name__ == "__main__":
    unittest.main()
