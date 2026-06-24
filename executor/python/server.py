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

try:
    import msgpack
except ImportError:
    msgpack = None

_MAX_FRAME_BYTES = 16 * 1024 * 1024


def _identity_decorator(fn=None, **_kwargs):
    if fn is None:
        return lambda actual: actual
    return fn


class _LogServeModule:
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


def _install_fake_logserve():
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
    sys.modules["logserve"] = fake_logserve



_ALLOWED_IMPORTS = {"logserve", "os", "time"}

_SAFE_TIME_MODULE = types.ModuleType("time")
_SAFE_TIME_MODULE.sleep = time.sleep
_SAFE_TIME_MODULE.time = time.time
_SAFE_TIME_MODULE.perf_counter = time.perf_counter

_SAFE_OS_PATH_MODULE = types.ModuleType("posixpath")
_SAFE_OS_PATH_MODULE.exists = os.path.exists
_SAFE_OS_MODULE = types.ModuleType("os")
_SAFE_OS_MODULE.path = _SAFE_OS_PATH_MODULE

_SAFE_IMPORT_MODULES = {
    "os": _SAFE_OS_MODULE,
    "time": _SAFE_TIME_MODULE,
}
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


def _limited_import(name, globals=None, locals=None, fromlist=(), level=0):
    if level != 0 or name not in _ALLOWED_IMPORTS:
        raise ImportError(f"imports are disabled in the LogServe executor: {name}")
    if name == "logserve":
        return sys.modules[name]
    return _SAFE_IMPORT_MODULES[name]



_ALLOWED_FILE_ROOTS = tuple(
    os.path.abspath(root)
    for root in {tempfile.gettempdir(), os.getenv("TMP", ""), os.getenv("TEMP", "")}
    if root
)


def _safe_open(path, mode="r", *args, **kwargs):
    if any(flag in mode for flag in ("+", "b")):
        raise PermissionError("executor open only allows one-way text file access")
    target = os.path.abspath(os.fspath(path))
    if not any(os.path.commonpath([root, target]) == root for root in _ALLOWED_FILE_ROOTS):
        raise PermissionError("executor open is restricted to the system temp directory")
    return builtins.open(target, mode, *args, **kwargs)
_FUNCTION_CODE_CACHE = {}

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


def _compile_user_source(source, filename):
    tree = ast.parse(source, filename=filename, mode="exec")
    _validate_user_ast(tree)
    return compile(tree, filename, "exec")


def _validate_user_ast(tree):
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                if alias.name not in _ALLOWED_IMPORTS:
                    raise ValueError(f"import is not allowed in executor source: {alias.name}")
        elif isinstance(node, ast.ImportFrom):
            if node.level != 0 or node.module not in _ALLOWED_IMPORTS:
                raise ValueError(f"import is not allowed in executor source: {node.module or ''}")
        elif isinstance(node, ast.Name):
            if node.id in _FORBIDDEN_NAMES:
                raise ValueError(f"name is not allowed in executor source: {node.id}")
        elif isinstance(node, ast.Attribute):
            if node.attr.startswith("__"):
                raise ValueError(f"dunder attribute access is not allowed in executor source: {node.attr}")


def _sandbox_namespace(name):
    return {
        "__builtins__": _SAFE_BUILTINS,
        "__name__": name,
        "task": _identity_decorator,
        "workflow": _identity_decorator,
        "actor": _identity_decorator,
        "logserve": _LogServeModule,
    }

def handle_request(request):
    try:
        if request.get("mode") == "actor":
            return handle_actor(request)
        return handle_task(request)
    except Exception:
        return {"ok": False, "error": traceback.format_exc()}


def handle_task(request):
    source = request.get("function_source") or ""
    function_hash = request.get("function_hash") or ""
    function_name = request["function_name"]
    args_payload = _payload(request.get("args_json") or {})
    args = args_payload.get("args", [])
    kwargs = args_payload.get("kwargs", {})

    _install_fake_logserve()
    namespace = _sandbox_namespace("logserve_task")
    code = _code_for_task_source(source, function_hash)
    exec(code, namespace, namespace)
    fn = namespace[function_name]
    result = fn(*args, **kwargs)
    return {"ok": True, "result": result}


def _code_for_task_source(source, function_hash):
    if function_hash:
        cached = _FUNCTION_CODE_CACHE.get(function_hash)
        if cached is not None and not source:
            return cached
        if not source:
            raise ValueError(f"function source for {function_hash} is not cached")
        computed = _source_hash(source)
        if computed != function_hash:
            raise ValueError(f"function hash mismatch: expected {function_hash}, got {computed}")
        if cached is not None:
            return cached
        code = _compile_user_source(source, f"<logserve-task:{function_hash}>")
        _FUNCTION_CODE_CACHE[function_hash] = code
        return code
    if not source:
        raise ValueError("function_source is required when function_hash is omitted")
    return _compile_user_source(source, "<logserve-task>")


def _source_hash(source):
    return "sha256:" + hashlib.sha256(source.encode("utf-8")).hexdigest()


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
    exec(_compile_user_source(class_source, "<logserve-actor>"), namespace, namespace)
    cls = namespace[class_name]
    if state:
        obj = cls.__new__(cls)
        obj.__dict__.update(state)
    else:
        obj = cls(*init_args, **init_kwargs)
    result = getattr(obj, method_name)(*args, **kwargs)
    return {"ok": True, "result": result, "state": obj.__dict__}


def _payload(raw):
    if raw is None:
        return {}
    if isinstance(raw, (bytes, bytearray)):
        return json.loads(bytes(raw).decode("utf-8")) if raw else {}
    if isinstance(raw, str):
        return json.loads(raw) if raw else {}
    return raw
def _json_bytes(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def _response_for_msgpack(response):
    out = {"ok": bool(response.get("ok"))}
    if response.get("error"):
        out["error"] = response["error"]
    if "result" in response:
        out["result_json"] = _json_bytes(response["result"])
    if "state" in response:
        out["state_json"] = _json_bytes(response["state"])
    return out


def _read_exact(stream, size):
    data = stream.read(size)
    if len(data) != size:
        raise EOFError("unexpected EOF while reading executor frame")
    return data


def _read_frame(stream):
    header = stream.read(4)
    if not header:
        return None
    if len(header) != 4:
        raise EOFError("short executor frame header")
    (size,) = struct.unpack(">I", header)
    if size > _MAX_FRAME_BYTES:
        raise ValueError(f"executor frame {size} exceeds max {_MAX_FRAME_BYTES}")
    return _read_exact(stream, size)


def _write_frame(stream, payload):
    if len(payload) > _MAX_FRAME_BYTES:
        raise ValueError(f"executor frame {len(payload)} exceeds max {_MAX_FRAME_BYTES}")
    stream.write(struct.pack(">I", len(payload)))
    stream.write(payload)
    stream.flush()


def _loop_msgpack():
    if msgpack is None:
        print("msgpack is required for --loop-msgpack", file=sys.stderr, flush=True)
        return 2
    stdin = sys.stdin.buffer
    stdout = sys.stdout.buffer
    while True:
        frame = _read_frame(stdin)
        if frame is None:
            return 0
        request = msgpack.unpackb(frame, raw=False)
        response = _response_for_msgpack(handle_request(request))
        _write_frame(stdout, msgpack.packb(response, use_bin_type=True))


def main() -> int:
    if len(sys.argv) > 1 and sys.argv[1] == "--loop-msgpack":
        return _loop_msgpack()

    if len(sys.argv) > 1 and sys.argv[1] == "--loop":
        for line in sys.stdin:
            if not line.strip():
                continue
            response = handle_request(json.loads(line))
            print(json.dumps(response, ensure_ascii=False), flush=True)
        return 0

    request = json.load(sys.stdin)
    response = handle_request(request)
    print(json.dumps(response, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())