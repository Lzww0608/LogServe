import json
import sys
import types
import traceback


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


def handle_request(request):
    try:
        if request.get("mode") == "actor":
            return handle_actor(request)
        return handle_task(request)
    except Exception:
        return {"ok": False, "error": traceback.format_exc()}


def handle_task(request):
    source = request["function_source"]
    function_name = request["function_name"]
    args_payload = _payload(request.get("args_json") or {})
    args = args_payload.get("args", [])
    kwargs = args_payload.get("kwargs", {})

    _install_fake_logserve()
    namespace = {
        "__name__": "logserve_task",
        "task": _identity_decorator,
        "workflow": _identity_decorator,
        "actor": _identity_decorator,
        "logserve": _LogServeModule,
    }
    exec(compile(source, "<logserve-task>", "exec"), namespace, namespace)
    fn = namespace[function_name]
    result = fn(*args, **kwargs)
    return {"ok": True, "result": result}


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
    namespace = {
        "__name__": "logserve_actor",
        "task": _identity_decorator,
        "workflow": _identity_decorator,
        "actor": _identity_decorator,
        "logserve": _LogServeModule,
    }
    exec(compile(class_source, "<logserve-actor>", "exec"), namespace, namespace)
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
    if isinstance(raw, str):
        return json.loads(raw) if raw else {}
    return raw


def main() -> int:
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
