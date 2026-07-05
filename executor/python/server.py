# Python subprocess entrypoint for LogServe worker task and actor execution.
#
# The Go worker keeps this process alive and sends execution requests over
# stdin/stdout. This module intentionally avoids docstrings so user-visible
# __doc__ metadata and sandbox namespace contents do not change during comment
# maintenance.
import ast
import builtins
import hashlib
import json
import os
import struct
import sys
import tempfile
import time
import types
import traceback

# msgpack is optional at import time because JSON line mode is still useful for
# debugging when the dependency is absent; --loop-msgpack fails explicitly below.
try:
    import msgpack
except ImportError:
    msgpack = None

# Keep this bound in sync with the Go worker's maxExecutorFrameBytes; both sides
# reject oversized frames before allocating or writing large payloads.
_MAX_FRAME_BYTES = 16 * 1024 * 1024


# Identity decorator preserves submitted SDK annotations without local registration side effects.
def _identity_decorator(fn=None, **_kwargs):
    # Support both @logserve.task and @logserve.task(...) without changing the wrapped callable.
    if fn is None:
        return lambda actual: actual
    return fn


# Minimal in-memory stand-in for the SDK module while user source is executed.
#
# Task and actor sources can contain @logserve.task/@logserve.actor decorators or
# SDK helper calls. The executor must import and define that code without making
# real control-plane calls, so helpers are inert and decorators are identity
# wrappers.
class _LogServeModule:
    # staticmethod avoids binding a fake module instance as self when user code
    # calls logserve.task or logserve.llm_generate from class/function bodies.
    task = staticmethod(_identity_decorator)
    workflow = staticmethod(_identity_decorator)
    actor = staticmethod(_identity_decorator)
    submit = staticmethod(lambda *args, **kwargs: None)
    submit_workflow = staticmethod(lambda *args, **kwargs: None)
    create_actor = staticmethod(lambda *args, **kwargs: None)
    call_actor = staticmethod(lambda *args, **kwargs: None)
    get_actor_status = staticmethod(lambda *args, **kwargs: None)
    replay_actor = staticmethod(lambda *args, **kwargs: None)
    register_model = staticmethod(lambda *args, **kwargs: None)
    set_scheduling_policy = staticmethod(lambda *args, **kwargs: None)
    submit_llm = staticmethod(lambda *args, **kwargs: None)
    llm_generate = staticmethod(lambda *args, **kwargs: None)
    replay_llm = staticmethod(lambda *args, **kwargs: None)


# Install the fake SDK module into sys.modules before executing user code.
def _install_fake_logserve():
    # Use a real module object so import logserve and from logserve import task follow normal import semantics.
    fake_logserve = types.ModuleType("logserve")
    fake_logserve.task = _identity_decorator
    fake_logserve.workflow = _identity_decorator
    fake_logserve.actor = _identity_decorator
    fake_logserve.submit = lambda *args, **kwargs: None
    fake_logserve.submit_workflow = lambda *args, **kwargs: None
    fake_logserve.create_actor = lambda *args, **kwargs: None
    fake_logserve.call_actor = lambda *args, **kwargs: None
    fake_logserve.get_actor_status = lambda *args, **kwargs: None
    fake_logserve.replay_actor = lambda *args, **kwargs: None
    fake_logserve.register_model = lambda *args, **kwargs: None
    fake_logserve.set_scheduling_policy = lambda *args, **kwargs: None
    fake_logserve.submit_llm = lambda *args, **kwargs: None
    fake_logserve.llm_generate = lambda *args, **kwargs: None
    fake_logserve.replay_llm = lambda *args, **kwargs: None
    # Overwrite any previously imported real SDK module so submitted code always
    # sees the inert executor-local surface for this request.
    sys.modules["logserve"] = fake_logserve


# Only expose imports that have safe executor-side substitutes below.
_ALLOWED_IMPORTS = {"logserve", "os", "time"}

# The time proxy allows user code to sleep or measure time without exposing
# unrelated module attributes.
_SAFE_TIME_MODULE = types.ModuleType("time")
_SAFE_TIME_MODULE.sleep = time.sleep
_SAFE_TIME_MODULE.time = time.time
_SAFE_TIME_MODULE.perf_counter = time.perf_counter

_SAFE_OS_PATH_MODULE = types.ModuleType("posixpath")
_SAFE_OS_PATH_MODULE.exists = os.path.exists
# The os proxy is limited to path.exists for examples that probe temp files; it
# deliberately omits filesystem mutation and environment/process access.
_SAFE_OS_MODULE = types.ModuleType("os")
_SAFE_OS_MODULE.path = _SAFE_OS_PATH_MODULE

_SAFE_IMPORT_MODULES = {
    "os": _SAFE_OS_MODULE,
    "time": _SAFE_TIME_MODULE,
}
# Names in this denylist would bypass the restricted builtins/import policy or
# expose process state that should not be available to submitted functions.
_FORBIDDEN_NAMES = {
    "breakpoint",
    "compile",
    "delattr",
    "dir",
    "eval",
    "exec",
    "getattr",
    "globals",
    "input",
    "locals",
    "setattr",
    "type",
    "vars",
    "__import__",
}


# Import hook used by the sandbox builtins.
#
# It rejects relative imports and returns the safe module proxies instead of the
# real os/time modules. Importing logserve is allowed only after the fake module
# has been installed for the request.
def _limited_import(name, globals=None, locals=None, fromlist=(), level=0):
    if level != 0 or name not in _ALLOWED_IMPORTS:
        raise ImportError(f"imports are disabled in the LogServe executor: {name}")
    # Request handlers install the fake module before executing user source; a missing entry is a request-ordering bug.
    if name == "logserve":
        return sys.modules[name]
    return _SAFE_IMPORT_MODULES[name]


# File I/O is restricted to OS temp roots so examples can exchange scratch files
# without giving submitted source access to the project checkout or user home.
_ALLOWED_FILE_ROOTS = tuple(
    os.path.abspath(root)
    for root in {tempfile.gettempdir(), os.getenv("TMP", ""), os.getenv("TEMP", "")}
    if root
)


# Open a text file only when it lives under one of the allowed temp roots.
def _safe_open(path, mode="r", *args, **kwargs):
    # Binary and update modes are blocked because they complicate auditing and
    # could let user code mutate existing files through a previously opened path.
    if any(flag in mode for flag in ("+", "b")):
        raise PermissionError("executor open only allows one-way text file access")
    target = os.path.abspath(os.fspath(path))
    # commonpath prevents prefix tricks such as C:\TempX matching C:\Temp.
    if not any(os.path.commonpath([root, target]) == root for root in _ALLOWED_FILE_ROOTS):
        raise PermissionError("executor open is restricted to the system temp directory")
    # Delegate only after path normalization so user code cannot smuggle a non-temp path through relative components.
    return builtins.open(target, mode, *args, **kwargs)

# Process-local compiled-code cache keyed by SDK source hashes.
_FUNCTION_CODE_CACHE = {}

# Builtins visible to user code. The set is intentionally small but includes the
# normal data-structure helpers needed by simple task and actor examples.
_SAFE_BUILTINS = {
    "__build_class__": builtins.__build_class__,
    "__import__": _limited_import,
    "abs": abs,
    "all": all,
    "any": any,
    "bool": bool,
    "dict": dict,
    "enumerate": enumerate,
    "Exception": Exception,
    "filter": filter,
    "float": float,
    "int": int,
    "len": len,
    "list": list,
    "map": map,
    "max": max,
    "min": min,
    "open": _safe_open,
    "print": print,
    "range": range,
    "RuntimeError": RuntimeError,
    "set": set,
    "sorted": sorted,
    "str": str,
    "sum": sum,
    "tuple": tuple,
    "TypeError": TypeError,
    "ValueError": ValueError,
    "zip": zip,
}


# Parse, validate, and compile submitted Python source under a synthetic filename.
def _compile_user_source(source, filename):
    # Parse first so validation works on structured syntax rather than brittle
    # string checks, then compile the already-validated tree.
    tree = ast.parse(source, filename=filename, mode="exec")
    _validate_user_ast(tree)
    return compile(tree, filename, "exec")


# Reject AST constructs that would escape the executor's narrow sandbox.
#
# This is a lightweight guardrail, not a complete Python security sandbox: it is
# paired with restricted builtins/imports and the worker-level process timeout.
def _validate_user_ast(tree):
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                if alias.name not in _ALLOWED_IMPORTS:
                    raise ValueError(f"import is not allowed in executor source: {alias.name}")
        elif isinstance(node, ast.ImportFrom):
            # from-import is allowed only for the same top-level safe modules;
            # relative imports would reach files outside the submitted source.
            if node.level != 0 or node.module not in _ALLOWED_IMPORTS:
                raise ValueError(f"import is not allowed in executor source: {node.module or ''}")
        elif isinstance(node, ast.Name):
            if node.id in _FORBIDDEN_NAMES:
                raise ValueError(f"name is not allowed in executor source: {node.id}")
        # Dunder attributes can reach Python internals even when getattr/type are unavailable.
        elif isinstance(node, ast.Attribute):
            if node.attr.startswith("__"):
                raise ValueError(f"dunder attribute access is not allowed in executor source: {node.attr}")


# Build the globals/locals dictionary used for executing user task or actor code.
def _sandbox_namespace(name):
    # The same dictionary is passed as globals and locals so definitions created
    # by exec are immediately visible to later function/class lookups.
    return {
        "__builtins__": _SAFE_BUILTINS,
        "__name__": name,
        "task": _identity_decorator,
        "workflow": _identity_decorator,
        "actor": _identity_decorator,
        # Provide logserve as a global as well as an importable fake module so
        # snippets copied from SDK examples work with or without import logserve.
        "logserve": _LogServeModule,
    }

# Dispatch one decoded executor request and convert exceptions into error replies.
def handle_request(request):
    try:
        if request.get("mode") == "actor":
            return handle_actor(request)
        return handle_task(request)
    # Send tracebacks over the executor protocol so the Go worker can complete the task as failed.
    except Exception:
        return {"ok": False, "error": traceback.format_exc()}


# Execute a submitted Python task function and return its JSON-serializable result.
def handle_task(request):
    source = request.get("function_source") or ""
    function_hash = request.get("function_hash") or ""
    function_name = request["function_name"]
    args_payload = _payload(request.get("args_json") or {})
    args = args_payload.get("args", [])
    kwargs = args_payload.get("kwargs", {})

    # function_name is intentionally required: the worker decides the entrypoint,
    # while this process only resolves it after source validation and execution.
    # Reinstall per request so tests or previous user code cannot leave a mutated
    # logserve module behind for the next execution.
    _install_fake_logserve()
    namespace = _sandbox_namespace("logserve_task")
    code = _code_for_task_source(source, function_hash)
    exec(code, namespace, namespace)
    fn = namespace[function_name]
    result = fn(*args, **kwargs)
    return {"ok": True, "result": result}


# Hash-backed task code is compiled once per source hash and reused only after verification.
def _code_for_task_source(source, function_hash):
    if function_hash:
        cached = _FUNCTION_CODE_CACHE.get(function_hash)
        # A hash-only request is valid only after this long-lived process has seen
        # and cached the corresponding source once.
        if cached is not None and not source:
            return cached
        if not source:
            raise ValueError(f"function source for {function_hash} is not cached")
        # Recheck inside the subprocess so a direct executor caller cannot poison the code cache.
        computed = _source_hash(source)
        if computed != function_hash:
            raise ValueError(f"function hash mismatch: expected {function_hash}, got {computed}")
        if cached is not None:
            # Reuse the compiled object only after verifying any newly supplied
            # source still hashes to the requested function_hash.
            return cached
        code = _compile_user_source(source, f"<logserve-task:{function_hash}>")
        _FUNCTION_CODE_CACHE[function_hash] = code
        return code
    if not source:
        raise ValueError("function_source is required when function_hash is omitted")
    return _compile_user_source(source, "<logserve-task>")


# Compute the SDK-compatible source hash used by worker-side function caching.
def _source_hash(source):
    return "sha256:" + hashlib.sha256(source.encode("utf-8")).hexdigest()


# Execute an actor method against restored state or a newly constructed instance.
def handle_actor(request):
    class_source = request["class_source"]
    class_name = request["class_name"]
    method_name = request["method_name"]
    args_payload = _payload(request.get("args_json") or {})
    args = args_payload.get("args", [])
    kwargs = args_payload.get("kwargs", {})

    state = _payload(request.get("state_json")) if request.get("state_json") else None
    init_payload = _payload(request.get("init_args_json") or {})
    init_args = init_payload.get("args", [])
    init_kwargs = init_payload.get("kwargs", {})

    _install_fake_logserve()
    namespace = _sandbox_namespace("logserve_actor")
    # Actor class code is compiled on every call because actor state is the only
    # persisted Python data; class source remains part of the task request.
    exec(_compile_user_source(class_source, "<logserve-actor>"), namespace, namespace)
    cls = namespace[class_name]
    # Only a truthy persisted state is rehydrated; empty state follows the first-materialization path.
    if state:
        # Rehydrate from persisted __dict__ without calling __init__; init args
        # are only for first materialization when no prior actor state exists.
        obj = cls.__new__(cls)
        obj.__dict__.update(state)
    else:
        obj = cls(*init_args, **init_kwargs)
    result = getattr(obj, method_name)(*args, **kwargs)
    # Persist only the instance dictionary; class code is supplied with each task
    # and actor identity/epoch are owned by the Go control plane.
    return {"ok": True, "result": result, "state": obj.__dict__}


# Normalize JSON envelope fields received from either JSON text or msgpack bytes.
def _payload(raw):
    if raw is None:
        return {}
    if isinstance(raw, (bytes, bytearray)):
        return json.loads(bytes(raw).decode("utf-8")) if raw else {}
    if isinstance(raw, str):
        return json.loads(raw) if raw else {}
    # JSON line mode can already provide decoded dict/list values, so leave them untouched.
    return raw

# Encode a value as compact UTF-8 JSON bytes for the Go worker response fields.
def _json_bytes(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


# Convert a Python execution result into the msgpack reply shape expected by Go.
def _response_for_msgpack(response):
    out = {"ok": bool(response.get("ok"))}
    if response.get("error"):
        out["error"] = response["error"]
    if "result" in response:
        # result_json/state_json preserve raw JSON bytes across msgpack so the Go
        # worker does not have to reverse msgpack's dynamic numeric/list mapping.
        out["result_json"] = _json_bytes(response["result"])
    if "state" in response:
        out["state_json"] = _json_bytes(response["state"])
    return out


# Read exactly size bytes from a binary stream or fail on a short frame.
def _read_exact(stream, size):
    # Pipes can close mid-frame; treat short reads as protocol errors so the Go
    # worker can restart the subprocess instead of decoding partial msgpack.
    data = stream.read(size)
    if len(data) != size:
        raise EOFError("unexpected EOF while reading executor frame")
    return data


# Read one length-prefixed executor frame; None means clean EOF before a header.
def _read_frame(stream):
    header = stream.read(4)
    if not header:
        return None
    if len(header) != 4:
        raise EOFError("short executor frame header")
    (size,) = struct.unpack(">I", header)
    # Reject before allocating the body, matching the Go worker-side frame cap.
    if size > _MAX_FRAME_BYTES:
        raise ValueError(f"executor frame {size} exceeds max {_MAX_FRAME_BYTES}")
    return _read_exact(stream, size)


# Write one length-prefixed executor frame and flush it immediately.
def _write_frame(stream, payload):
    if len(payload) > _MAX_FRAME_BYTES:
        raise ValueError(f"executor frame {len(payload)} exceeds max {_MAX_FRAME_BYTES}")
    stream.write(struct.pack(">I", len(payload)))
    stream.write(payload)
    stream.flush()


# Serve the long-lived msgpack protocol used by the default Go worker path.
def _loop_msgpack():
    if msgpack is None:
        # Diagnostics must go to stderr because stdout is reserved for binary
        # length-prefixed frames in this protocol.
        print("msgpack is required for --loop-msgpack", file=sys.stderr, flush=True)
        return 2
    stdin = sys.stdin.buffer
    stdout = sys.stdout.buffer
    while True:
        frame = _read_frame(stdin)
        if frame is None:
            return 0
        # raw=False decodes msgpack bin fields as bytes and string keys as str,
        # matching the mixed raw-JSON-byte request shape from the Go worker.
        request = msgpack.unpackb(frame, raw=False)
        response = _response_for_msgpack(handle_request(request))
        _write_frame(stdout, msgpack.packb(response, use_bin_type=True))


# Select the executor protocol and run either one-shot or streaming stdin mode.
def main() -> int:
    if len(sys.argv) > 1 and sys.argv[1] == "--loop-msgpack":
        return _loop_msgpack()

    if len(sys.argv) > 1 and sys.argv[1] == "--loop":
        # JSON line mode is kept as a debuggable fallback selected by the worker
        # via LOGSERVE_EXECUTOR_PROTOCOL=json.
        for line in sys.stdin:
            if not line.strip():
                continue
            response = handle_request(json.loads(line))
            print(json.dumps(response, ensure_ascii=False), flush=True)
        return 0

    # One-shot JSON mode is used by direct CLI/debug invocations where the parent
    # process sends a single request and then exits.
    request = json.load(sys.stdin)
    response = handle_request(request)
    print(json.dumps(response, ensure_ascii=False))
    return 0


# Keep process exit handling here so importing this module in tests has no side effects.
if __name__ == "__main__":
    raise SystemExit(main())
