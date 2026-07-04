# Direct Python gRPC transport for the LogServe control-plane API.
import json
import os
import time

import grpc

from ._generated import control_pb2 as pb


# GrpcControlTransport adapts SDK transport commands onto ControlService RPCs.
class GrpcControlTransport:
    # Create or reuse a plaintext gRPC channel and optional bearer metadata.
    def __init__(self, address="127.0.0.1:50052", *, channel=None, poll_interval_s=0.1, api_token=None):
        self.address = address
        # The Go servers use application-level bearer auth; transport TLS is not configured here.
        self.channel = channel or grpc.insecure_channel(address)
        self.poll_interval_s = poll_interval_s
        token = api_token if api_token is not None else os.environ.get("LOGSERVE_API_TOKEN", "")
        self._metadata = _auth_metadata(token)
        self._rpc = _ControlRpc(self.channel, self._metadata)
    # Dispatch the same command names used by CLIControlTransport to RPC helpers.
    def run(self, command, payload=None, timeout=None):
        payload = payload or {}
        if command == "submit":
            return self._submit_task(payload, timeout=timeout)
        if command == "status":
            return self._task_status(payload["task_id"], timeout=timeout)
        if command == "workflow-submit":
            return self._submit_workflow(payload, timeout=timeout)
        if command == "workflow-status":
            return self._workflow_status(payload["workflow_id"], timeout=timeout)
        if command == "workflow-replay":
            return self._workflow_replay(payload["workflow_id"], timeout=timeout)
        if command == "model-register":
            return self._register_model(payload, timeout=timeout)
        if command == "scheduler-policy":
            return self._set_scheduling_policy(payload, timeout=timeout)
        if command == "llm-submit":
            return self._submit_llm(payload, timeout=timeout)
        if command == "llm-replay":
            return self._llm_replay(payload["task_id"], timeout=timeout)
        if command == "actor-create":
            return self._create_actor(payload, timeout=timeout)
        if command == "actor-call":
            return self._call_actor(payload, timeout=timeout)
        if command == "actor-status":
            return self._actor_status(payload["actor_id"], timeout=timeout)
        if command == "actor-replay":
            return self._actor_replay(payload["actor_id"], timeout=timeout)
        if command == "backpressure-set":
            return self._set_backpressure(payload, timeout=timeout)
        if command == "dashboard-snapshot":
            return self._dashboard_snapshot(timeout=timeout)
        raise ValueError(f"unsupported control command {command!r}")

    # Submit a task request and wait until the task reaches a terminal state.
    def _submit_task(self, payload, timeout=None):
        args_json = _json_bytes({"args": payload.get("args", []), "kwargs": payload.get("kwargs", {})})
        resp = self._rpc.SubmitTask(
            pb.SubmitTaskRequest(
                task_name=payload.get("task_name", ""),
                function_name=payload.get("function_name", ""),
                function_source=payload.get("function_source", ""),
                args_json=args_json,
                idempotency_key=payload.get("idempotency_key", ""),
                function_ref=payload.get("function_ref", ""),
                function_hash=payload.get("function_hash", ""),
            ),
            timeout=timeout,
        )
        return self._wait_task(resp.task_id, timeout=timeout)

    # Fetch and normalize one task status response.
    def _task_status(self, task_id, timeout=None):
        resp = self._rpc.GetTaskStatus(pb.GetTaskStatusRequest(task_id=task_id), timeout=timeout)
        return _task_status_dict(resp)

    # Submit a workflow definition and wait until the workflow reaches a terminal state.
    def _submit_workflow(self, payload, timeout=None):
        resp = self._rpc.SubmitWorkflow(
            pb.SubmitWorkflowRequest(
                workflow_name=payload.get("workflow_name", ""),
                definition_json=_json_bytes(payload.get("definition", {})),
                idempotency_key=payload.get("idempotency_key", ""),
            ),
            timeout=timeout,
        )
        return self._wait_workflow(resp.workflow_id, timeout=timeout)

    # Fetch and normalize one workflow status response.
    def _workflow_status(self, workflow_id, timeout=None):
        resp = self._rpc.GetWorkflowStatus(pb.GetWorkflowStatusRequest(workflow_id=workflow_id), timeout=timeout)
        return _workflow_status_dict(resp)

    # Replay workflow state and include the metadata consistency flag.
    def _workflow_replay(self, workflow_id, timeout=None):
        resp = self._rpc.ReplayWorkflow(pb.ReplayWorkflowRequest(workflow_id=workflow_id), timeout=timeout)
        out = _workflow_status_dict(resp.replayed)
        out["consistent_with_metadata"] = resp.consistent_with_metadata
        return out

    # Convert SDK model payloads into ModelInfo and return normalized metadata.
    def _register_model(self, payload, timeout=None):
        resp = self._rpc.RegisterModel(
            pb.RegisterModelRequest(
                model=pb.ModelInfo(
                    name=payload.get("name", ""),
                    version=payload.get("version", "") or "v1",
                    size_bytes=payload.get("size_bytes", 0),
                    path=payload.get("path", ""),
                    adapter=payload.get("adapter", "") or "mock",
                )
            ),
            timeout=timeout,
        )
        return _model_dict(resp.model)

    # Parse a user policy string and return the accepted scheduling policy name.
    def _set_scheduling_policy(self, payload, timeout=None):
        resp = self._rpc.SetSchedulingPolicy(
            pb.SetSchedulingPolicyRequest(policy=_parse_scheduling_policy(payload.get("policy", ""))),
            timeout=timeout,
        )
        return {"policy": _scheduling_policy_name(resp.policy)}

    # Submit an LLM request as a task and wait for terminal task status.
    def _submit_llm(self, payload, timeout=None):
        resp = self._rpc.SubmitLLM(
            pb.SubmitLLMRequest(
                model_name=payload.get("model_name", ""),
                model_version=payload.get("model_version", "") or "v1",
                prompt=payload.get("prompt", ""),
                max_tokens=payload.get("max_tokens", 0),
                adapter=payload.get("adapter", ""),
                idempotency_key=payload.get("idempotency_key", ""),
            ),
            timeout=timeout,
        )
        return self._wait_task(resp.task_id, timeout=timeout)

    # Replay LLM execution and cache metrics for one task.
    def _llm_replay(self, task_id, timeout=None):
        resp = self._rpc.ReplayLLM(pb.ReplayLLMRequest(task_id=task_id), timeout=timeout)
        return {
            "task_id": resp.task_id,
            "model_name": resp.model_name,
            "model_version": resp.model_version,
            "worker_id": resp.worker_id,
            "cache_hit": resp.cache_hit,
            "model_load_ms": resp.model_load_ms,
            "checkpoint_fetch_ms": resp.checkpoint_fetch_ms,
            "first_token_ms": resp.first_token_ms,
            "total_latency_ms": resp.total_latency_ms,
            "cache_used_bytes": resp.cache_used_bytes,
            "cache_capacity_bytes": resp.cache_capacity_bytes,
            "eviction_count": resp.eviction_count,
            "events": [_llm_event_dict(event) for event in resp.events],
        }

    # Create an actor and normalize ownership and epoch metadata.
    def _create_actor(self, payload, timeout=None):
        resp = self._rpc.CreateActor(
            pb.CreateActorRequest(
                class_name=payload.get("class_name", ""),
                class_source=payload.get("class_source", ""),
                init_args_json=_json_bytes(
                    {"args": payload.get("init_args", []), "kwargs": payload.get("init_kwargs", {})}
                ),
                idempotency_key=payload.get("idempotency_key", ""),
                snapshot_every=payload.get("snapshot_every", 0),
            ),
            timeout=timeout,
        )
        return {
            "actor_id": resp.actor_id,
            "status": _actor_status_name(resp.status),
            "owner_worker_id": resp.owner_worker_id,
            "epoch": resp.epoch,
        }

    # Submit an actor command and normalize the task-like response.
    def _call_actor(self, payload, timeout=None):
        resp = self._rpc.CallActor(
            pb.CallActorRequest(
                actor_id=payload.get("actor_id", ""),
                method_name=payload.get("method_name", ""),
                args_json=_json_bytes({"args": payload.get("args", []), "kwargs": payload.get("kwargs", {})}),
                idempotency_key=payload.get("idempotency_key", ""),
                timeout_ms=payload.get("timeout_ms", 0),
            ),
            timeout=timeout,
        )
        return {
            "actor_id": resp.actor_id,
            "call_id": resp.call_id,
            "status": _task_status_name(resp.status),
            "result": _decode_json(resp.result_json),
            "error": resp.error,
            "epoch": resp.epoch,
        }

    # Fetch and normalize one actor status response.
    def _actor_status(self, actor_id, timeout=None):
        resp = self._rpc.GetActorStatus(pb.GetActorStatusRequest(actor_id=actor_id), timeout=timeout)
        return _actor_status_dict(resp)

    # Replay actor state and expose both full and snapshot replay counters.
    def _actor_replay(self, actor_id, timeout=None):
        resp = self._rpc.ReplayActor(pb.ReplayActorRequest(actor_id=actor_id), timeout=timeout)
        out = _actor_status_dict(resp.replayed)
        out["consistent_with_metadata"] = resp.consistent_with_metadata
        out["full_replay_commands"] = resp.full_replay_commands
        out["snapshot_replay_commands"] = resp.snapshot_replay_commands
        return out

    # Update scheduling pressure thresholds through the control plane.
    def _set_backpressure(self, payload, timeout=None):
        resp = self._rpc.SetBackpressure(
            pb.SetBackpressureRequest(
                queue_high_watermark=payload.get("queue_high_watermark", 0),
                redelivery_timeout_ms=payload.get("redelivery_timeout_ms", 0),
                log_append_slow_ms=payload.get("log_append_slow_ms", 0),
            ),
            timeout=timeout,
        )
        return {
            "queue_high_watermark": resp.queue_high_watermark,
            "redelivery_timeout_ms": resp.redelivery_timeout_ms,
            "log_append_slow_ms": resp.log_append_slow_ms,
        }

    # Dashboard normalization intentionally exposes only fields consumed by SDK callers.
    def _dashboard_snapshot(self, timeout=None):
        resp = self._rpc.GetDashboardSnapshot(pb.GetDashboardSnapshotRequest(), timeout=timeout)
        return {
            "queue_depth": resp.queue_depth,
            "queue_high_watermark": resp.queue_high_watermark,
            "redelivery_timeout_ms": resp.redelivery_timeout_ms,
            "scheduling_policy": _scheduling_policy_name(resp.scheduling_policy),
            "last_log_append_ms": resp.last_log_append_ms,
            "log_append_slow_ms": resp.log_append_slow_ms,
            "compactable_log_records": getattr(resp, "compactable_log_records", 0),
            "compactable_log_bytes": getattr(resp, "compactable_log_bytes", 0),
        }

    # Poll task status until success, failure, or the caller-provided timeout.
    def _wait_task(self, task_id, timeout=None):
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            # Reuse the same RPC timeout value per poll; the outer deadline caps total wait time.
            status = self._task_status(task_id, timeout=timeout)
            if status["status"] in ("SUCCEEDED", "FAILED"):
                return status
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"task {task_id} did not finish before timeout")
            time.sleep(self.poll_interval_s)

    # Poll workflow status until completion, failure, or the caller-provided timeout.
    def _wait_workflow(self, workflow_id, timeout=None):
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            # Reuse the same RPC timeout value per poll; the outer deadline caps total wait time.
            status = self._workflow_status(workflow_id, timeout=timeout)
            if status["status"] in ("COMPLETED", "FAILED"):
                return status
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"workflow {workflow_id} did not finish before timeout")
            time.sleep(self.poll_interval_s)


# _ControlRpc builds low-level unary callables without generated grpc stubs.
class _ControlRpc:
    # Bind each ControlService RPC path to a serializer/deserializer pair.
    def __init__(self, channel, metadata=None):
        self.SubmitTask = _unary(channel, metadata, "SubmitTask", pb.SubmitTaskRequest, pb.SubmitTaskResponse)
        self.GetTaskStatus = _unary(channel, metadata, "GetTaskStatus", pb.GetTaskStatusRequest, pb.GetTaskStatusResponse)
        self.SubmitWorkflow = _unary(channel, metadata, "SubmitWorkflow", pb.SubmitWorkflowRequest, pb.SubmitWorkflowResponse)
        self.GetWorkflowStatus = _unary(
            channel, metadata, "GetWorkflowStatus", pb.GetWorkflowStatusRequest, pb.GetWorkflowStatusResponse
        )
        self.ReplayWorkflow = _unary(channel, metadata, "ReplayWorkflow", pb.ReplayWorkflowRequest, pb.ReplayWorkflowResponse)
        self.RegisterModel = _unary(channel, metadata, "RegisterModel", pb.RegisterModelRequest, pb.RegisterModelResponse)
        self.SetSchedulingPolicy = _unary(
            channel, metadata, "SetSchedulingPolicy", pb.SetSchedulingPolicyRequest, pb.SetSchedulingPolicyResponse
        )
        self.SubmitLLM = _unary(channel, metadata, "SubmitLLM", pb.SubmitLLMRequest, pb.SubmitLLMResponse)
        self.ReplayLLM = _unary(channel, metadata, "ReplayLLM", pb.ReplayLLMRequest, pb.ReplayLLMResponse)
        self.SetBackpressure = _unary(
            channel, metadata, "SetBackpressure", pb.SetBackpressureRequest, pb.SetBackpressureResponse
        )
        self.GetDashboardSnapshot = _unary(
            channel, metadata, "GetDashboardSnapshot", pb.GetDashboardSnapshotRequest, pb.DashboardSnapshot
        )
        self.CreateActor = _unary(channel, metadata, "CreateActor", pb.CreateActorRequest, pb.CreateActorResponse)
        self.CallActor = _unary(channel, metadata, "CallActor", pb.CallActorRequest, pb.CallActorResponse)
        self.GetActorStatus = _unary(channel, metadata, "GetActorStatus", pb.GetActorStatusRequest, pb.GetActorStatusResponse)
        self.ReplayActor = _unary(channel, metadata, "ReplayActor", pb.ReplayActorRequest, pb.ReplayActorResponse)


# Build outgoing bearer-token metadata when LOGSERVE_API_TOKEN is configured.
def _auth_metadata(api_token):
    if not api_token:
        return None
    return (("authorization", f"Bearer {api_token}"),)


# Build one unary-unary callable for a ControlService method path.
def _unary(channel, default_metadata, method, request_cls, response_cls):
    call = channel.unary_unary(
        f"/logserve.v1.ControlService/{method}",
        request_serializer=request_cls.SerializeToString,
        response_deserializer=response_cls.FromString,
    )

    # Merge default auth metadata with per-call metadata before invoking gRPC.
    def invoke(request, *, timeout=None, metadata=None):
        merged_metadata = default_metadata
        if metadata:
            merged_metadata = tuple(default_metadata or ()) + tuple(metadata)
        return call(request, timeout=timeout, metadata=merged_metadata)

    return invoke

# Encode compact UTF-8 JSON bytes expected by protobuf request fields.
def _json_bytes(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


# Decode optional protobuf JSON bytes into Python values.
def _decode_json(data):
    if not data:
        return None
    return json.loads(data.decode("utf-8"))


# Convert TaskStatus protobuf responses into stable SDK dictionaries.
def _task_status_dict(resp):
    return {
        "task_id": resp.task_id,
        "status": _task_status_name(resp.status),
        "result": _decode_json(resp.result_json),
        "error": resp.error,
        "worker_id": resp.worker_id,
        "created_at_ms": resp.created_at_ms,
        "updated_at_ms": resp.updated_at_ms,
    }


# Convert workflow status protobuf responses into stable SDK dictionaries.
def _workflow_status_dict(resp):
    return {
        "workflow_id": resp.workflow_id,
        "workflow_name": resp.workflow_name,
        "status": _workflow_status_name(resp.status),
        "result": _decode_json(resp.result_json),
        "result_ref": resp.result_ref,
        "error": resp.error,
        "steps": [_workflow_step_dict(step) for step in resp.steps],
        "created_at_ms": resp.created_at_ms,
        "updated_at_ms": resp.updated_at_ms,
        "completed_at_ms": resp.completed_at_ms,
        "latency_ms": resp.latency_ms,
    }


# Convert one workflow step protobuf message into an SDK dictionary.
def _workflow_step_dict(step):
    return {
        "step_id": step.step_id,
        "task_name": step.task_name,
        "status": _workflow_step_status_name(step.status),
        "attempts": step.attempts,
        "task_id": step.task_id,
        "result": _decode_json(step.result_json),
        "result_ref": step.result_ref,
        "error": step.error,
        "started_at_ms": step.started_at_ms,
        "completed_at_ms": step.completed_at_ms,
        "latency_ms": step.latency_ms,
    }


# Convert actor status protobuf responses into stable SDK dictionaries.
def _actor_status_dict(resp):
    return {
        "actor_id": resp.actor_id,
        "class_name": resp.class_name,
        "status": _actor_status_name(resp.status),
        "owner_worker_id": resp.owner_worker_id,
        "epoch": resp.epoch,
        "command_count": resp.command_count,
        "snapshot_ref": resp.snapshot_ref,
        "snapshot_command_count": resp.snapshot_command_count,
        "state": _decode_json(resp.state_json),
        "created_at_ms": resp.created_at_ms,
        "updated_at_ms": resp.updated_at_ms,
    }


# Convert one LLM event protobuf message into an SDK dictionary.
def _llm_event_dict(event):
    return {
        "event_type": event.event_type,
        "timestamp_ms": event.timestamp_ms,
        "task_id": event.task_id,
        "model_name": event.model_name,
        "model_version": event.model_version,
        "worker_id": event.worker_id,
        "cache_hit": event.cache_hit,
        "model_load_ms": event.model_load_ms,
        "checkpoint_fetch_ms": event.checkpoint_fetch_ms,
        "first_token_ms": event.first_token_ms,
        "total_latency_ms": event.total_latency_ms,
        "cache_used_bytes": event.cache_used_bytes,
        "cache_capacity_bytes": event.cache_capacity_bytes,
        "eviction_count": event.eviction_count,
    }


# Convert ModelInfo protobuf messages into SDK dictionaries.
def _model_dict(model):
    return {
        "name": model.name,
        "version": model.version,
        "size_bytes": model.size_bytes,
        "path": model.path,
        "adapter": model.adapter,
    }


# Map task status enum values to stable CLI-compatible strings.
def _task_status_name(status):
    return {
        pb.TASK_STATUS_QUEUED: "QUEUED",
        pb.TASK_STATUS_RUNNING: "RUNNING",
        pb.TASK_STATUS_SUCCEEDED: "SUCCEEDED",
        pb.TASK_STATUS_FAILED: "FAILED",
    }.get(status, "UNSPECIFIED")


# Map workflow status enum values to stable CLI-compatible strings.
def _workflow_status_name(status):
    return {
        pb.WORKFLOW_STATUS_RUNNING: "RUNNING",
        pb.WORKFLOW_STATUS_COMPLETED: "COMPLETED",
        pb.WORKFLOW_STATUS_FAILED: "FAILED",
    }.get(status, "UNSPECIFIED")


# Map workflow step status enum values to stable CLI-compatible strings.
def _workflow_step_status_name(status):
    return {
        pb.WORKFLOW_STEP_STATUS_SCHEDULED: "SCHEDULED",
        pb.WORKFLOW_STEP_STATUS_STARTED: "STARTED",
        pb.WORKFLOW_STEP_STATUS_SUCCEEDED: "SUCCEEDED",
        pb.WORKFLOW_STEP_STATUS_FAILED: "FAILED",
    }.get(status, "UNSPECIFIED")


# Map actor status enum values to stable CLI-compatible strings.
def _actor_status_name(status):
    return {
        pb.ACTOR_STATUS_ACTIVE: "ACTIVE",
        pb.ACTOR_STATUS_UNAVAILABLE: "UNAVAILABLE",
    }.get(status, "UNSPECIFIED")


# Parse user-facing scheduling policy aliases into protobuf enum values.
def _parse_scheduling_policy(value):
    normalized = (value or "").replace("-", "_").upper()
    if normalized in ("", "LOCALITY_AWARE"):
        return pb.SCHEDULING_POLICY_LOCALITY_AWARE
    if normalized == "RESOURCE_ONLY":
        return pb.SCHEDULING_POLICY_RESOURCE_ONLY
    if normalized == "PREDICTED_LATENCY":
        return pb.SCHEDULING_POLICY_PREDICTED_LATENCY
    raise ValueError(f"unknown scheduling policy {value!r}")


# Map scheduling policy enum values to stable CLI-compatible strings.
def _scheduling_policy_name(policy):
    return {
        pb.SCHEDULING_POLICY_RESOURCE_ONLY: "RESOURCE_ONLY",
        pb.SCHEDULING_POLICY_LOCALITY_AWARE: "LOCALITY_AWARE",
        pb.SCHEDULING_POLICY_PREDICTED_LATENCY: "PREDICTED_LATENCY",
    }.get(policy, "UNSPECIFIED")
