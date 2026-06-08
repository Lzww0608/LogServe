import hashlib
import inspect
import json
import os
import subprocess
import textwrap
from pathlib import Path

from .decorators import StepRef, current_trace_context, trace_workflow


class ActorHandle:
    def __init__(self, actor_id):
        self.actor_id = actor_id

    def call(self, method_name, *args, timeout_ms=60000, **kwargs):
        return call_actor(self.actor_id, method_name, *args, timeout_ms=timeout_ms, **kwargs)

    def __getattr__(self, name):
        if name.startswith("_"):
            raise AttributeError(name)

        def method(*args, **kwargs):
            return self.call(name, *args, **kwargs)

        return method


def submit(fn, *args, **kwargs):
    if getattr(fn, "_logserve_workflow", False):
        return submit_workflow(fn, *args, **kwargs)
    source = textwrap.dedent(inspect.getsource(inspect.unwrap(fn)))
    payload = {
        "task_name": getattr(fn, "__name__", "task"),
        "function_name": getattr(fn, "__name__", "task"),
        "function_source": source,
        "args": list(args),
        "kwargs": kwargs,
    }
    payload["idempotency_key"] = _idempotency_key(payload)
    output = _run_ctl("submit", payload)
    if output.get("status") != "SUCCEEDED":
        raise RuntimeError(output.get("error", "task failed"))
    return output.get("result")


def create_actor(cls, *args, snapshot_every=None, **kwargs):
    source = _module_source(cls) or textwrap.dedent(inspect.getsource(cls))
    if snapshot_every is None:
        snapshot_every = getattr(cls, "_logserve_snapshot_every", 25)
    payload = {
        "class_name": getattr(cls, "__name__", "Actor"),
        "class_source": source,
        "init_args": list(args),
        "init_kwargs": kwargs,
        "snapshot_every": snapshot_every,
    }
    output = _run_ctl("actor-create", payload)
    if output.get("status") not in ("ACTIVE", "UNAVAILABLE"):
        raise RuntimeError(output.get("error", "actor creation failed"))
    return ActorHandle(output["actor_id"])


def register_model(name, version="v1", size_bytes=0, path="", adapter="mock"):
    payload = {
        "name": name,
        "version": version,
        "size_bytes": size_bytes,
        "path": path,
        "adapter": adapter,
    }
    return _run_ctl("model-register", payload)


def set_scheduling_policy(policy):
    return _run_ctl("scheduler-policy", {"policy": policy})


def submit_llm(model_name, prompt, *, version="v1", max_tokens=64, adapter="mock", idempotency_key=""):
    payload = {
        "model_name": model_name,
        "model_version": version,
        "prompt": prompt,
        "max_tokens": max_tokens,
        "adapter": adapter,
        "idempotency_key": idempotency_key,
    }
    output = _run_ctl("llm-submit", payload)
    if output.get("status") != "SUCCEEDED":
        raise RuntimeError(output.get("error", "llm request failed"))
    return output


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


def call_actor(actor_id, method_name, *args, timeout_ms=60000, **kwargs):
    payload = {
        "actor_id": actor_id,
        "method_name": method_name,
        "args": list(args),
        "kwargs": kwargs,
        "timeout_ms": timeout_ms,
    }
    output = _run_ctl("actor-call", payload)
    if output.get("status") != "SUCCEEDED":
        raise RuntimeError(output.get("error", "actor call failed"))
    return output.get("result")


def submit_workflow(fn, *args, **kwargs):
    definition = _build_workflow_definition(fn, args, kwargs)
    payload = {
        "workflow_name": getattr(fn, "__name__", "workflow"),
        "definition": definition,
        "idempotency_key": _stable_hash(definition),
    }
    output = _run_ctl("workflow-submit", payload)
    if output.get("status") != "COMPLETED":
        raise RuntimeError(output.get("error", "workflow failed"))
    return output.get("result")


def get_task_status(task_id):
    command = _ctl_command() + ["status", "--task-id", task_id]
    return _run_json_command(command)


def get_workflow_status(workflow_id):
    command = _ctl_command() + ["workflow-status", "--workflow-id", workflow_id]
    return _run_json_command(command)


def replay_workflow(workflow_id):
    command = _ctl_command() + ["workflow-replay", "--workflow-id", workflow_id]
    return _run_json_command(command)


def get_actor_status(actor_id):
    command = _ctl_command() + ["actor-status", "--actor-id", actor_id]
    return _run_json_command(command)


def replay_actor(actor_id):
    command = _ctl_command() + ["actor-replay", "--actor-id", actor_id]
    return _run_json_command(command)


def replay_llm(task_id):
    command = _ctl_command() + ["llm-replay", "--task-id", task_id]
    return _run_json_command(command)


def _build_workflow_definition(fn, args, kwargs):
    ctx, result = trace_workflow(fn, *args, **kwargs)
    if not isinstance(result, StepRef):
        raise TypeError("Phase 2 workflows must return the result of a @task call")

    module_source = _module_source(fn)
    steps = []
    for step in ctx.steps:
        step = dict(step)
        if module_source:
            step["function_source"] = module_source
        else:
            step["function_source"] = textwrap.dedent(step["function_source"])
        steps.append(step)

    return {
        "workflow_name": getattr(fn, "__name__", "workflow"),
        "function_source": module_source or textwrap.dedent(inspect.getsource(inspect.unwrap(fn))),
        "args_json": {"args": list(args), "kwargs": kwargs},
        "steps": steps,
        "result_step_id": result.step_id,
        "max_attempts": getattr(fn, "_logserve_retries", 3),
        "timeout_ms": getattr(fn, "_logserve_timeout_ms", 30000),
    }


def _module_source(fn):
    module = inspect.getmodule(inspect.unwrap(fn))
    path = getattr(module, "__file__", None)
    if not path:
        return ""
    try:
        return Path(path).read_text(encoding="utf-8")
    except OSError:
        return ""


def _run_ctl(command, payload):
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


def _ctl_command():
    cli = os.environ.get("LOGSERVE_CLI")
    if cli:
        return [cli]
    return ["go", "run", str(_repo_root() / "cmd" / "logservectl")]


def _repo_root():
    return Path(__file__).resolve().parents[3]


def _idempotency_key(payload):
    stable = json.dumps(
        {
            "function_name": payload["function_name"],
            "function_source": payload["function_source"],
            "args": payload["args"],
            "kwargs": payload["kwargs"],
        },
        sort_keys=True,
        ensure_ascii=False,
    )
    return hashlib.sha256(stable.encode("utf-8")).hexdigest()


def _stable_hash(value):
    stable = json.dumps(value, sort_keys=True, ensure_ascii=False)
    return hashlib.sha256(stable.encode("utf-8")).hexdigest()
