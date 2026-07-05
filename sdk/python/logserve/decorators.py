# Workflow decorators record Python calls as LogServe task, workflow, actor, and LLM metadata.
#
# Decorators must preserve ordinary Python execution while recording a DAG only when
# trace_workflow has pushed a process-local WorkflowTraceContext.
import functools
import hashlib
import inspect
import json


# A simple process-local trace stack; workflow definitions are expected to trace synchronously.
# It is intentionally not thread-local because SDK tracing is a short-lived build step,
# not a concurrent workflow execution engine.
_TRACE_STACK = []
# Marker key used to serialize StepRef dependencies inside JSON args.
_REF_KEY = "__step_ref__"


# StepRef is the placeholder value returned by traced task calls inside workflows.
class StepRef:
    # Store the workflow step id represented by this dependency reference.
    def __init__(self, step_id):
        self.step_id = step_id

    # Show the referenced step id in debugging output.
    def __repr__(self):
        return f"StepRef({self.step_id!r})"


# WorkflowTraceContext accumulates a JSON DAG while a workflow function is executed.
class WorkflowTraceContext:
    # Track emitted steps and per-name counters for stable duplicate step ids.
    def __init__(self):
        self.steps = []
        # Counts are per workflow trace, preventing duplicate step ids without global state.
        self._name_counts = {}

    # Record one @task invocation as a workflow step instead of executing it.
    def add_step(self, fn, args, kwargs):
        # Duplicate Python function names get stable suffixed step ids within one workflow.
        base = getattr(fn, "__name__", "step")
        count = self._name_counts.get(base, 0) + 1
        self._name_counts[base] = count
        step_id = base if count == 1 else f"{base}_{count}"

        # StepRef values embedded in args become JSON markers plus depends_on edges.
        encoded_args, deps_a = _encode_refs(list(args))
        encoded_kwargs, deps_k = _encode_refs(dict(kwargs))
        deps = sorted(set(deps_a + deps_k))
        original = inspect.unwrap(fn)
        # Capture the unwrapped function source so worker execution receives user code, not SDK wrappers.
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

    # Record one llm_generate call as a synthetic workflow step.
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

        # The prompt itself may depend on prior StepRef outputs, so encode it through the same dependency path.
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


# Active workflow tracing is process-local and only exists during trace_workflow.
def current_trace_context():
    if not _TRACE_STACK:
        return None
    return _TRACE_STACK[-1]


# Execute a workflow function while collecting task and LLM step references.
def trace_workflow(fn, *args, **kwargs):
    ctx = WorkflowTraceContext()
    # A stack supports nested tracing defensively, although normal workflow definitions trace synchronously.
    _TRACE_STACK.append(ctx)
    try:
        result = fn(*args, **kwargs)
    finally:
        # Always pop on user-code exceptions so a failed trace cannot leak into
        # later ordinary function calls in the same process.
        _TRACE_STACK.pop()
    return ctx, result


# Decorate a function as a LogServe task and make it traceable in workflows.
def task(fn=None, *, retries=3, timeout_ms=30000):
    # Support both @task and @task(...) call styles.
    def decorate(actual):
        # During workflow tracing, return a StepRef instead of running user code.
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


# Decorate a function as a workflow entrypoint.
def workflow(fn=None, *, retries=3, timeout_ms=30000):
    # Support both @workflow and @workflow(...) call styles.
    def decorate(actual):
        # Preserve call behavior while marking the callable for workflow submission.
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


# Decorate a class as a LogServe actor definition.
def actor(cls=None, *, snapshot_every=25):
    # Attach actor metadata without replacing the class object.
    def decorate(actual):
        setattr(actual, "_logserve_actor", True)
        setattr(actual, "_logserve_snapshot_every", snapshot_every)
        return actual

    if cls is None:
        return decorate
    return decorate(cls)


# Encode Python values and StepRef placeholders into JSON-compatible objects.
def encode_json(value):
    # Public encoding drops the dependency list; submit paths keep dependencies through WorkflowTraceContext.
    encoded, _ = _encode_refs(value)
    return encoded


# Hash source text with the same sha256-prefix convention used by the client.
def _source_hash(source):
    return "sha256:" + hashlib.sha256(source.encode("utf-8")).hexdigest()


# Recursively replace StepRef values with dependency markers and collect dependencies.
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
    # Dict keys are left unchanged because the workflow JSON contract only rewrites values that reference steps.
    if isinstance(value, dict):
        out = {}
        for key, item in value.items():
            encoded, item_deps = _encode_refs(item)
            out[key] = encoded
            deps.extend(item_deps)
        return out, deps
    # Validate that non-reference leaves are JSON serializable before returning them.
    json.dumps(value)
    return value, deps
