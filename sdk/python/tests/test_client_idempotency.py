# Regression tests for SDK idempotency semantics and source extraction boundaries.
import sys
import unittest
from pathlib import Path
from unittest import mock


SDK_ROOT = Path(__file__).resolve().parents[1]
# Load the checkout SDK directly so these regression tests do not depend on package installation.
if str(SDK_ROOT) not in sys.path:
    sys.path.insert(0, str(SDK_ROOT))

from logserve import client
from logserve.decorators import actor, task, workflow


# add is a plain function used to verify standalone task submission behavior.
def add(a, b):
    return a + b


# echo is a decorated task used as the workflow step in source extraction tests.
@task
def echo(value):
    return value


# echo_workflow returns a StepRef so workflow submission can build a one-step DAG.
@workflow
def echo_workflow(value):
    return echo(value)


# CounterActor is a small actor fixture for class-source extraction tests.
@actor
class CounterActor:
    # Initialize deterministic actor state for source extraction assertions.
    def __init__(self):
        self.value = 0

    # Mutate actor state so the fixture resembles a real actor method.
    def inc(self):
        self.value += 1
        return self.value


# CapturingTransport records SDK transport calls without starting LogServe services.
class CapturingTransport:
    # Store captured command/payload pairs for each assertion.
    def __init__(self):
        self.calls = []

    # Emulate the small subset of transport commands needed by these tests.
    def run(self, command, payload=None, **_kwargs):
        # Capture the SDK-level transport contract before any CLI or gRPC encoding
        # can obscure idempotency keys or extracted source payloads.
        self.calls.append((command, payload or {}))
        # Return successful terminal states because these tests focus on emitted payloads, not polling behavior.
        if command == "submit":
            return {"task_id": "task-1", "status": "SUCCEEDED", "result": 3}
        if command == "workflow-submit":
            return {"workflow_id": "wf-1", "status": "COMPLETED", "result": "ok"}
        if command == "actor-create":
            return {"actor_id": "actor-1", "status": "ACTIVE"}
        raise AssertionError(f"unexpected command {command}")


# ClientIdempotencyTests locks down retry and source-shaping behavior in the SDK.
class ClientIdempotencyTests(unittest.TestCase):
    # Verify repeated submits stay non-idempotent unless the caller supplies a key.
    def test_submit_does_not_auto_dedupe_same_function_and_args(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        self.assertEqual(sdk.submit(add, 1, 2), 3)
        self.assertEqual(sdk.submit(add, 1, 2), 3)

        self.assertEqual(transport.calls[0][1]["idempotency_key"], "")
        self.assertEqual(transport.calls[1][1]["idempotency_key"], "")
        self.assertTrue(transport.calls[0][1]["function_hash"].startswith("sha256:"))

    # Verify explicit task idempotency keys pass through unchanged.
    def test_submit_uses_explicit_idempotency_key_only_when_provided(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.submit(add, 1, 2, idempotency_key="same-submit")

        self.assertEqual(transport.calls[0][1]["idempotency_key"], "same-submit")

    # Verify workflow submits default to an empty idempotency key.
    def test_workflow_submit_defaults_to_non_idempotent_submission(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.submit_workflow(echo_workflow, "hello")

        self.assertEqual(transport.calls[0][0], "workflow-submit")
        self.assertEqual(transport.calls[0][1]["idempotency_key"], "")

    # Verify traced step source contains the function body without module imports.
    def test_workflow_step_source_does_not_include_module_imports(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.submit_workflow(echo_workflow, "hello")

        # Source extraction should stop at the decorated callable and avoid capturing this test module prologue.
        definition = transport.calls[0][1]["definition"]
        step = definition["steps"][0]
        step_source = step["function_source"]
        self.assertTrue(step["function_hash"].startswith("sha256:"))
        self.assertIn("def echo", step_source)
        self.assertNotIn("import sys", step_source)
        self.assertNotIn("from pathlib", step_source)

    # Verify actor class source extraction does not capture module imports.
    def test_actor_source_does_not_include_module_imports(self):
        transport = CapturingTransport()
        sdk = client.LogServeClient(transport=transport)

        sdk.create_actor(CounterActor)

        actor_source = transport.calls[0][1]["class_source"]
        self.assertIn("class CounterActor", actor_source)
        self.assertNotIn("import sys", actor_source)
        self.assertNotIn("from pathlib", actor_source)

    # Verify module-level helpers delegate through the default client path.
    def test_module_level_submit_uses_default_client_transport(self):
        transport = CapturingTransport()
        # Patch the factory rather than module globals so the test matches the real
        # module-level helper path without depending on a singleton client.
        with mock.patch.object(client, "_default_client", return_value=client.LogServeClient(transport=transport)):
            self.assertEqual(client.submit(add, 1, 2, idempotency_key="key-1"), 3)

        self.assertEqual(transport.calls[0][1]["idempotency_key"], "key-1")


if __name__ == "__main__":
    unittest.main()
