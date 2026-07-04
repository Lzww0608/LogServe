# High-level Python SDK client surface for submitting tasks, workflows, actors, and LLM requests.
import hashlib
import inspect
import json
import os
from pathlib import Path
import subprocess
import textwrap

from .decorators import StepRef, current_trace_context, trace_workflow


# ActorHandle is the user-facing proxy returned after actor creation.
class ActorHandle:
    # Store actor identity and optionally pin calls to the creating client.
    def __init__(self, actor_id, client=None):
        self.actor_id = actor_id
        self._client = client

    # Submit one actor method call through the bound or default client.
    def call(self, method_name, *args, timeout_ms=60000, idempotency_key=None, **kwargs):
        client = self._client or _default_client()
        return client.call_actor(
            self.actor_id,
            method_name,
            *args,
            timeout_ms=timeout_ms,
            idempotency_key=idempotency_key,
            **kwargs,
        )

    # Convert arbitrary attribute access into actor method calls.
    def __getattr__(self, name):
        # Keep private Python attributes local instead of turning them into RPC calls.
        if name.startswith("_"):
            raise AttributeError(name)

        # Late-bind the missing attribute name as the remote actor method.
        def method(*args, **kwargs):
            return self.call(name, *args, **kwargs)

        return method


# LogServeClient wraps the selected control transport with Python-friendly APIs.
class LogServeClient:
    # Resolve the control-plane address and transport once per client instance.
    def __init__(self, address=None, transport=None):
        self.address = address or os.environ.get("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052")
        self.transport = transport or _default_transport(self.address)

    # Submit a decorated or plain Python function and wait for its task result.
    def submit(self, fn, *args, idempotency_key=None, **kwargs):
        if getattr(fn, "_logserve_workflow", False):
            return self.submit_workflow(fn, *args, idempotency_key=idempotency_key, **kwargs)
        # Capture only the original function source so wrappers/decorators do not leak into worker code.
        source = textwrap.dedent(inspect.getsource(inspect.unwrap(fn)))
        payload = {
            "task_name": getattr(fn, "__name__", "task"),
            "function_name": getattr(fn, "__name__", "task"),
            "function_source": source,
            "function_hash": _source_hash(source),
            "args": list(args),
            "kwargs": kwargs,
            "idempotency_key": idempotency_key or "",
        }
        output = self.transport.run("submit", payload)
        if output.get("status") != "SUCCEEDED":
            raise RuntimeError(output.get("error", "task failed"))
        return output.get("result")

    # Create an actor from class source and return a handle for future calls.
    def create_actor(self, cls, *args, snapshot_every=None, idempotency_key=None, **kwargs):
        source = textwrap.dedent(inspect.getsource(cls))
        if snapshot_every is None:
            snapshot_every = getattr(cls, "_logserve_snapshot_every", 25)
        payload = {
            "class_name": getattr(cls, "__name__", "Actor"),
            "class_source": source,
            "init_args": list(args),
            "init_kwargs": kwargs,
            "idempotency_key": idempotency_key or "",
            "snapshot_every": snapshot_every,
        }
        output = self.transport.run("actor-create", payload)
        if output.get("status") not in ("ACTIVE", "UNAVAILABLE"):
            raise RuntimeError(output.get("error", "actor creation failed"))
        return ActorHandle(output["actor_id"], self)

    # Register model metadata used by scheduling, replay output, and dashboard views.
    def register_model(self, name, version="v1", size_bytes=0, path="", adapter="mock"):
        payload = {
            "name": name,
            "version": version,
            "size_bytes": size_bytes,
            "path": path,
            "adapter": adapter,
        }
        return self.transport.run("model-register", payload)

    # Change the control-plane scheduling policy through the active transport.
    def set_scheduling_policy(self, policy):
        return self.transport.run("scheduler-policy", {"policy": policy})

    # Submit one LLM task and wait for the resulting task status.
    def submit_llm(
        self,
        model_name,
        prompt,
        *,
        version="v1",
        max_tokens=64,
        adapter="mock",
        idempotency_key=None,
    ):
        payload = {
            "model_name": model_name,
            "model_version": version,
            "prompt": prompt,
            "max_tokens": max_tokens,
            "adapter": adapter,
            "idempotency_key": idempotency_key or "",
        }
        output = self.transport.run("llm-submit", payload)
        if output.get("status") != "SUCCEEDED":
            raise RuntimeError(output.get("error", "llm request failed"))
        return output

    # Actor calls reuse the task completion shape but expose only the method result to SDK callers.
    def call_actor(self, actor_id, method_name, *args, timeout_ms=60000, idempotency_key=None, **kwargs):
        payload = {
            "actor_id": actor_id,
            "method_name": method_name,
            "args": list(args),
            "kwargs": kwargs,
            "idempotency_key": idempotency_key or "",
            "timeout_ms": timeout_ms,
        }
        output = self.transport.run("actor-call", payload)
        if output.get("status") != "SUCCEEDED":
            raise RuntimeError(output.get("error", "actor call failed"))
        return output.get("result")

    # Trace a workflow function into a DAG definition, submit it, and wait for completion.
    def submit_workflow(self, fn, *args, idempotency_key=None, **kwargs):
        definition = _build_workflow_definition(fn, args, kwargs)
        payload = {
            "workflow_name": getattr(fn, "__name__", "workflow"),
            "definition": definition,
            "idempotency_key": idempotency_key or "",
        }
        output = self.transport.run("workflow-submit", payload)
        if output.get("status") != "COMPLETED":
            raise RuntimeError(output.get("error", "workflow failed"))
        return output.get("result")

    # Fetch the materialized state for one task.
    def get_task_status(self, task_id):
        return self.transport.run("status", {"task_id": task_id})

    # Fetch the materialized state for one workflow.
    def get_workflow_status(self, workflow_id):
        return self.transport.run("workflow-status", {"workflow_id": workflow_id})

    # Rebuild workflow state from log records for consistency checks.
    def replay_workflow(self, workflow_id):
        return self.transport.run("workflow-replay", {"workflow_id": workflow_id})

    # Fetch the materialized state for one actor.
    def get_actor_status(self, actor_id):
        return self.transport.run("actor-status", {"actor_id": actor_id})

    # Rebuild actor state from log records and snapshots.
    def replay_actor(self, actor_id):
        return self.transport.run("actor-replay", {"actor_id": actor_id})

    # Rebuild LLM execution metrics from recorded LLM events.
    def replay_llm(self, task_id):
        return self.transport.run("llm-replay", {"task_id": task_id})


# CLIControlTransport is the fallback transport that shells out to logservectl.
class CLIControlTransport:
    # Commands in this set accept JSON on stdin, matching logservectl wire behavior.
    _PAYLOAD_COMMANDS = {
        "submit",
        "workflow-submit",
        "model-register",
        "scheduler-policy",
        "llm-submit",
        "backpressure-set",
        "actor-create",
        "actor-call",
    }

    # Commands in this map take one identifier flag instead of stdin JSON.
    _FLAG_COMMANDS = {
        "status": ("--task-id", "task_id"),
        "workflow-status": ("--workflow-id", "workflow_id"),
        "workflow-replay": ("--workflow-id", "workflow_id"),
        "actor-status": ("--actor-id", "actor_id"),
        "actor-replay": ("--actor-id", "actor_id"),
        "llm-replay": ("--task-id", "task_id"),
    }

    # Dispatch the SDK transport command to the equivalent logservectl invocation.
    def run(self, command, payload=None, **_kwargs):
        payload = payload or {}
        if command in self._PAYLOAD_COMMANDS:
            return _run_payload_command(command, payload)
        if command in self._FLAG_COMMANDS:
            flag, field = self._FLAG_COMMANDS[command]
            return _run_json_command(_ctl_command() + [command, flag, payload[field]])
        if command == "dashboard-snapshot":
            return _run_json_command(_ctl_command() + [command])
        raise ValueError(f"unsupported control command {command!r}")


# Submit a function through the process-wide default client.
def submit(fn, *args, idempotency_key=None, **kwargs):
    return _default_client().submit(fn, *args, idempotency_key=idempotency_key, **kwargs)


# Create an actor through the process-wide default client.
def create_actor(cls, *args, snapshot_every=None, idempotency_key=None, **kwargs):
    return _default_client().create_actor(
        cls,
        *args,
        snapshot_every=snapshot_every,
        idempotency_key=idempotency_key,
        **kwargs,
    )


# Register model metadata through the process-wide default client.
def register_model(name, version="v1", size_bytes=0, path="", adapter="mock"):
    return _default_client().register_model(name, version=version, size_bytes=size_bytes, path=path, adapter=adapter)


# Scheduler policy updates use the process-wide default client for parity with other helpers.
def set_scheduling_policy(policy):
    return _default_client().set_scheduling_policy(policy)


# Submit an LLM request through the process-wide default client.
def submit_llm(model_name, prompt, *, version="v1", max_tokens=64, adapter="mock", idempotency_key=None):
    return _default_client().submit_llm(
        model_name,
        prompt,
        version=version,
        max_tokens=max_tokens,
        adapter=adapter,
        idempotency_key=idempotency_key,
    )


# Generate text immediately or add an LLM step when tracing a workflow.
def llm_generate(
    model_name,
    prompt,
    *,
    version="v1",
    max_tokens=64,
    adapter="mock",
    step_id=None,
    retries=3,
    timeout_ms=30000,
):
    ctx = current_trace_context()
    if ctx is not None:
        return ctx.add_llm_step(
            model_name,
            prompt,
            model_version=version,
            adapter=adapter,
            max_tokens=max_tokens,
            step_id=step_id,
            retries=retries,
            timeout_ms=timeout_ms,
        )
    return submit_llm(
        model_name,
        prompt,
        version=version,
        max_tokens=max_tokens,
        adapter=adapter,
    ).get("result")


# Module-level actor calls are convenience wrappers around the default client transport.
def call_actor(actor_id, method_name, *args, timeout_ms=60000, idempotency_key=None, **kwargs):
    return _default_client().call_actor(
        actor_id,
        method_name,
        *args,
        timeout_ms=timeout_ms,
        idempotency_key=idempotency_key,
        **kwargs,
    )


# Submit a workflow through the process-wide default client.
def submit_workflow(fn, *args, idempotency_key=None, **kwargs):
    return _default_client().submit_workflow(fn, *args, idempotency_key=idempotency_key, **kwargs)


# Fetch task state through the process-wide default client.
def get_task_status(task_id):
    return _default_client().get_task_status(task_id)


# Fetch workflow state through the process-wide default client.
def get_workflow_status(workflow_id):
    return _default_client().get_workflow_status(workflow_id)


# Replay workflow state through the process-wide default client.
def replay_workflow(workflow_id):
    return _default_client().replay_workflow(workflow_id)


# Fetch actor state through the process-wide default client.
def get_actor_status(actor_id):
    return _default_client().get_actor_status(actor_id)


# Replay actor state through the process-wide default client.
def replay_actor(actor_id):
    return _default_client().replay_actor(actor_id)


# Replay LLM state through the process-wide default client.
def replay_llm(task_id):
    return _default_client().replay_llm(task_id)


# Build the JSON workflow definition by executing the function under trace mode.
def _build_workflow_definition(fn, args, kwargs):
    ctx, result = trace_workflow(fn, *args, **kwargs)
    # A workflow must end at a traced step so the control plane has a result node.
    if not isinstance(result, StepRef):
        raise TypeError("workflows must return the result of a @task call")

    steps = []
    for step in ctx.steps:
        step = dict(step)
        step["function_source"] = textwrap.dedent(step["function_source"])
        if step["function_source"]:
            step["function_hash"] = _source_hash(step["function_source"])
        steps.append(step)

    workflow_source = textwrap.dedent(inspect.getsource(inspect.unwrap(fn)))
    return {
        "workflow_name": getattr(fn, "__name__", "workflow"),
        "function_source": workflow_source,
        "function_hash": _source_hash(workflow_source),
        "args_json": {"args": list(args), "kwargs": kwargs},
        "steps": steps,
        "result_step_id": result.step_id,
        "max_attempts": getattr(fn, "_logserve_retries", 3),
        "timeout_ms": getattr(fn, "_logserve_timeout_ms", 30000),
    }




# Hash source text into the same sha256-prefixed identifier expected by control-plane metadata.
def _source_hash(source):
    return "sha256:" + hashlib.sha256(source.encode("utf-8")).hexdigest()


# Construct a fresh default client for module-level convenience functions.
def _default_client():
    return LogServeClient()


# Choose gRPC when dependencies are available, otherwise fall back to logservectl.
def _default_transport(address):
    mode = os.environ.get("LOGSERVE_SDK_TRANSPORT", "auto").lower()
    if mode == "cli":
        return CLIControlTransport()
    try:
        # Import lazily so the CLI transport works even when grpcio is not installed.
        from .grpc_client import GrpcControlTransport

        return GrpcControlTransport(address)
    except ImportError as exc:
        if mode == "grpc":
            raise RuntimeError("Python gRPC transport requires grpcio and protobuf") from exc
        return CLIControlTransport()


# Run a stdin-JSON logservectl command and decode its JSON stdout.
def _run_payload_command(command, payload):
    completed = subprocess.run(
        _ctl_command() + [command],
        cwd=_repo_root(),
        input=json.dumps(payload, ensure_ascii=False),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError(completed.stderr.strip())
    return json.loads(completed.stdout)


# Run a flag-only logservectl command and decode its JSON stdout.
def _run_json_command(command):
    completed = subprocess.run(
        command,
        cwd=_repo_root(),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError(completed.stderr.strip())
    return json.loads(completed.stdout)


# Resolve the logservectl command, allowing LOGSERVE_CLI to override go run.
def _ctl_command():
    cli = os.environ.get("LOGSERVE_CLI")
    if cli:
        return [cli]
    return ["go", "run", str(_repo_root() / "cmd" / "logservectl")]


# The CLI fallback runs from the repository root so go run can resolve local packages.
def _repo_root():
    return Path(__file__).resolve().parents[3]
