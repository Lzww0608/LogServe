import sys
import unittest
from pathlib import Path
from unittest import mock


SDK_ROOT = Path(__file__).resolve().parents[1]
if str(SDK_ROOT) not in sys.path:
    sys.path.insert(0, str(SDK_ROOT))

from logserve import client
from logserve.decorators import actor, task, workflow


def add(a, b):
    return a + b


@task
def echo(value):
    return value


@workflow
def echo_workflow(value):
    return echo(value)


@actor
class CounterActor:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value


class CapturingTransport:
    def __init__(self):
        self.calls = []

    def run(self, command, payload=None, **_kwargs):
        self.calls.append((command, payload or {}))
        if command == "submit":
            return {"task_id": "task-1", "status": "SUCCEEDED", "result": 3}
        if command == "workflow-submit":
            return {"workflow_id": "wf-1", "status": "COMPLETED", "result": "ok"}
        if command == "actor-create":
            return {"actor_id": "actor-1", "status": "ACTIVE"}
        raise AssertionError(f"unexpected command {command}")


class ClientIdempotencyTests(unittest.TestCase):
    def test_submit_does_not_auto_dedupe_same_function_and_args(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        self.assertEqual(sdk.submit(add, 1, 2), 3)
        self.assertEqual(sdk.submit(add, 1, 2), 3)

        self.assertEqual(transport.calls[0][1]["idempotency_key"], "")
        self.assertEqual(transport.calls[1][1]["idempotency_key"], "")

    def test_submit_uses_explicit_idempotency_key_only_when_provided(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.submit(add, 1, 2, idempotency_key="same-submit")

        self.assertEqual(transport.calls[0][1]["idempotency_key"], "same-submit")

    def test_workflow_submit_defaults_to_non_idempotent_submission(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.submit_workflow(echo_workflow, "hello")

        self.assertEqual(transport.calls[0][0], "workflow-submit")
        self.assertEqual(transport.calls[0][1]["idempotency_key"], "")

    def test_workflow_step_source_does_not_include_module_imports(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.submit_workflow(echo_workflow, "hello")

        definition = transport.calls[0][1]["definition"]
        step_source = definition["steps"][0]["function_source"]
        self.assertIn("def echo", step_source)
        self.assertNotIn("import sys", step_source)
        self.assertNotIn("from pathlib", step_source)

    def test_actor_source_does_not_include_module_imports(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.create_actor(CounterActor)

        actor_source = transport.calls[0][1]["class_source"]
        self.assertIn("class CounterActor", actor_source)
        self.assertNotIn("import sys", actor_source)
        self.assertNotIn("from pathlib", actor_source)

    def test_module_level_submit_uses_default_client_transport(self):
        transport = CapturingTransport()
        with mock.patch.object(client, "_default_client", return_value=client.LogServeClient(transport=transport)):
            self.assertEqual(client.submit(add, 1, 2, idempotency_key="key-1"), 3)

        self.assertEqual(transport.calls[0][1]["idempotency_key"], "key-1")


if __name__ == "__main__":
    unittest.main()
