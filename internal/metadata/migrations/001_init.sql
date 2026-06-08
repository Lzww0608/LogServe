CREATE TABLE IF NOT EXISTS workflow_instances (
  workflow_id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  input_json JSONB,
  definition_json JSONB,
  output_json JSONB,
  output_ref TEXT,
  error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS task_instances (
  task_id TEXT PRIMARY KEY,
  task_name TEXT NOT NULL,
  status TEXT NOT NULL,
  worker_id TEXT,
  idempotency_key TEXT UNIQUE,
  result_json JSONB,
  error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workers (
  worker_id TEXT PRIMARY KEY,
  address TEXT,
  labels JSONB NOT NULL DEFAULT '{}',
  last_heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_task_instances_status ON task_instances(status);
CREATE INDEX IF NOT EXISTS idx_workers_last_heartbeat ON workers(last_heartbeat_at);

CREATE TABLE IF NOT EXISTS workflow_steps (
  workflow_id TEXT NOT NULL REFERENCES workflow_instances(workflow_id) ON DELETE CASCADE,
  step_id TEXT NOT NULL,
  task_name TEXT NOT NULL,
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  task_id TEXT,
  result_json JSONB,
  result_ref TEXT,
  error TEXT,
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  latency_ms BIGINT,
  PRIMARY KEY (workflow_id, step_id)
);

CREATE INDEX IF NOT EXISTS idx_workflow_steps_status ON workflow_steps(status);

CREATE TABLE IF NOT EXISTS actor_instances (
  actor_id TEXT PRIMARY KEY,
  class_name TEXT NOT NULL,
  status TEXT NOT NULL,
  owner_worker_id TEXT,
  epoch BIGINT NOT NULL DEFAULT 0,
  command_count BIGINT NOT NULL DEFAULT 0,
  snapshot_ref TEXT,
  snapshot_command_count BIGINT NOT NULL DEFAULT 0,
  state_json JSONB,
  init_args_json JSONB,
  snapshot_every INTEGER NOT NULL DEFAULT 25,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS actor_commands (
  actor_id TEXT NOT NULL REFERENCES actor_instances(actor_id) ON DELETE CASCADE,
  call_id TEXT NOT NULL,
  method_name TEXT NOT NULL,
  worker_id TEXT NOT NULL,
  epoch BIGINT NOT NULL,
  args_json JSONB,
  result_json JSONB,
  state_json JSONB,
  command_count BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (actor_id, call_id)
);

CREATE INDEX IF NOT EXISTS idx_actor_instances_owner ON actor_instances(owner_worker_id);
CREATE INDEX IF NOT EXISTS idx_actor_commands_actor_count ON actor_commands(actor_id, command_count);

CREATE TABLE IF NOT EXISTS model_registry (
  name TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT 'v1',
  size_bytes BIGINT NOT NULL DEFAULT 0,
  path TEXT,
  adapter TEXT NOT NULL DEFAULT 'mock',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (name, version)
);

CREATE TABLE IF NOT EXISTS worker_model_cache (
  worker_id TEXT NOT NULL REFERENCES workers(worker_id) ON DELETE CASCADE,
  model_name TEXT NOT NULL,
  model_version TEXT NOT NULL DEFAULT 'v1',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (worker_id, model_name, model_version)
);

CREATE TABLE IF NOT EXISTS llm_requests (
  task_id TEXT PRIMARY KEY REFERENCES task_instances(task_id) ON DELETE CASCADE,
  model_name TEXT NOT NULL,
  model_version TEXT NOT NULL DEFAULT 'v1',
  worker_id TEXT,
  adapter TEXT NOT NULL DEFAULT 'mock',
  cache_hit BOOLEAN NOT NULL DEFAULT false,
  model_load_ms BIGINT NOT NULL DEFAULT 0,
  first_token_ms BIGINT NOT NULL DEFAULT 0,
  total_latency_ms BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_worker_model_cache_model ON worker_model_cache(model_name, model_version);
CREATE INDEX IF NOT EXISTS idx_llm_requests_model ON llm_requests(model_name, model_version);
