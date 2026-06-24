import functools
import hashlib
import inspect
import json


_TRACE_STACK = []
_REF_KEY = "__step_ref__"


class StepRef:
    def __init__(self, step_id):
        self.step_id = step_id

    def __repr__(self):
        return f"StepRef({self.step_id!r})"


class WorkflowTraceContext:
    def __init__(self):
        self.steps = []
        self._name_counts = {}

    def add_step(self, fn, args, kwargs):
        base = getattr(fn, "__name__", "step")
        count = self._name_counts.get(base, 0) + 1
        self._name_counts[base] = count
        step_id = base if count == 1 else f"{base}_{count}"

        encoded_args, deps_a = _encode_refs(list(args))
        encoded_kwargs, deps_k = _encode_refs(dict(kwargs))
        deps = sorted(set(deps_a + deps_k))
        original = inspect.unwrap(fn)
        source = inspect.getsource(original)
        self.steps.append(
            {
                "step_id": step_id,
                "task_name": base,
                "function_name": base,
                "function_source": source,
                "function_hash": _source_hash(source),
                "args_json": {
                    "args": encoded_args,
                    "kwargs": encoded_kwargs,
                },
                "depends_on": deps,
                "max_attempts": getattr(fn, "_logserve_retries", 3),
                "timeout_ms": getattr(fn, "_logserve_timeout_ms", 30000),
            }
        )
        return StepRef(step_id)

    def add_llm_step(
        self,
        model_name,
        prompt,
        *,
        model_version="v1",
        adapter="mock",
        max_tokens=64,
        step_id=None,
        retries=3,
        timeout_ms=30000,
    ):
        base = step_id or "llm_generate"
        if step_id is None:
            count = self._name_counts.get(base, 0) + 1
            self._name_counts[base] = count
            step_id = base if count == 1 else f"{base}_{count}"

        encoded_args, deps = _encode_refs([prompt])
        self.steps.append(
            {
                "step_id": step_id,
                "task_name": f"llm:{model_name}",
                "function_name": "__logserve_llm__",
                "function_source": "",
                "args_json": {
                    "args": encoded_args,
                    "kwargs": {},
                },
                "depends_on": sorted(set(deps)),
                "max_attempts": retries,
                "timeout_ms": timeout_ms,
                "llm_model_name": model_name,
                "llm_model_version": model_version,
                "llm_adapter": adapter,
                "llm_max_tokens": max_tokens,
            }
        )
        return StepRef(step_id)


def current_trace_context():
    if not _TRACE_STACK:
        return None
    return _TRACE_STACK[-1]


def trace_workflow(fn, *args, **kwargs):
    ctx = WorkflowTraceContext()
    _TRACE_STACK.append(ctx)
    try:
        result = fn(*args, **kwargs)
    finally:
        _TRACE_STACK.pop()
    return ctx, result


def task(fn=None, *, retries=3, timeout_ms=30000):
    def decorate(actual):
        @functools.wraps(actual)
        def wrapper(*args, **kwargs):
            ctx = current_trace_context()
            if ctx is not None:
                return ctx.add_step(wrapper, args, kwargs)
            return actual(*args, **kwargs)

        setattr(wrapper, "_logserve_task", True)
        setattr(wrapper, "_logserve_retries", retries)
        setattr(wrapper, "_logserve_timeout_ms", timeout_ms)
        return wrapper

    if fn is None:
        return decorate
    return decorate(fn)


def workflow(fn=None, *, retries=3, timeout_ms=30000):
    def decorate(actual):
        @functools.wraps(actual)
        def wrapper(*args, **kwargs):
            return actual(*args, **kwargs)

        setattr(wrapper, "_logserve_workflow", True)
        setattr(wrapper, "_logserve_retries", retries)
        setattr(wrapper, "_logserve_timeout_ms", timeout_ms)
        return wrapper

    if fn is None:
        return decorate
    return decorate(fn)


def actor(cls=None, *, snapshot_every=25):
    def decorate(actual):
        setattr(actual, "_logserve_actor", True)
        setattr(actual, "_logserve_snapshot_every", snapshot_every)
        return actual

    if cls is None:
        return decorate
    return decorate(cls)


def encode_json(value):
    encoded, _ = _encode_refs(value)
    return encoded


def _source_hash(source):
    return "sha256:" + hashlib.sha256(source.encode("utf-8")).hexdigest()
def _encode_refs(value):
    deps = []
    if isinstance(value, StepRef):
        return {_REF_KEY: value.step_id}, [value.step_id]
    if isinstance(value, list) or isinstance(value, tuple):
        out = []
        for item in value:
            encoded, item_deps = _encode_refs(item)
            out.append(encoded)
            deps.extend(item_deps)
        return out, deps
    if isinstance(value, dict):
        out = {}
        for key, item in value.items():
            encoded, item_deps = _encode_refs(item)
            out[key] = encoded
            deps.extend(item_deps)
        return out, deps
    json.dumps(value)
    return value, deps
