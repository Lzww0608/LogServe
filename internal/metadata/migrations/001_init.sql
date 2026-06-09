CREATE TABLE IF NOT EXISTS workflow_instances (
  workflow_id TEXT PRIMARY KEY,
  workflow_name TEXT,
  status TEXT NOT NULL,
  input_json JSONB,
  definition_json JSONB,
  output_json JSONB,
  output_ref TEXT,
  error TEXT,
  idempotency_key TEXT UNIQUE,
  idempotency_fingerprint TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS task_instances (
  task_id TEXT PRIMARY KEY,
  task_name TEXT NOT NULL,
  status TEXT NOT NULL,
  worker_id TEXT,
  workflow_id TEXT,
  step_id TEXT,
  target_worker_id TEXT,
  actor_id TEXT,
  actor_call_id TEXT,
  actor_epoch BIGINT NOT NULL DEFAULT 0,
  actor_command_seq BIGINT NOT NULL DEFAULT 0,
  task_lease_epoch BIGINT NOT NULL DEFAULT 0,
  llm_model_name TEXT,
  llm_model_version TEXT,
  idempotency_key TEXT UNIQUE,
  idempotency_fingerprint TEXT,
  result_json JSONB,
  error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workers (
  worker_id TEXT PRIMARY KEY,
  address TEXT,
  labels JSONB NOT NULL DEFAULT '{}',
  capacity INTEGER NOT NULL DEFAULT 1,
  running_tasks INTEGER NOT NULL DEFAULT 0,
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
  class_source TEXT,
  status TEXT NOT NULL,
  owner_worker_id TEXT,
  epoch BIGINT NOT NULL DEFAULT 0,
  command_count BIGINT NOT NULL DEFAULT 0,
  snapshot_ref TEXT,
  snapshot_command_count BIGINT NOT NULL DEFAULT 0,
  state_json JSONB,
  init_args_json JSONB,
  snapshot_every INTEGER NOT NULL DEFAULT 25,
  idempotency_key TEXT UNIQUE,
  idempotency_fingerprint TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS actor_commands (
  actor_id TEXT NOT NULL REFERENCES actor_instances(actor_id) ON DELETE CASCADE,
  call_id TEXT NOT NULL,
  method_name TEXT NOT NULL,
  worker_id TEXT NOT NULL,
  epoch BIGINT NOT NULL,
  command_seq BIGINT NOT NULL DEFAULT 0,
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

ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS workflow_name TEXT;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS idempotency_key TEXT UNIQUE;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS workflow_id TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS step_id TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS target_worker_id TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS actor_id TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS actor_call_id TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS actor_epoch BIGINT NOT NULL DEFAULT 0;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS actor_command_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS task_lease_epoch BIGINT NOT NULL DEFAULT 0;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS llm_model_name TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS llm_model_version TEXT;
ALTER TABLE task_instances ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
ALTER TABLE workers ADD COLUMN IF NOT EXISTS capacity INTEGER NOT NULL DEFAULT 1;
ALTER TABLE workers ADD COLUMN IF NOT EXISTS running_tasks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE actor_instances ADD COLUMN IF NOT EXISTS class_source TEXT;
ALTER TABLE actor_instances ADD COLUMN IF NOT EXISTS idempotency_key TEXT UNIQUE;
ALTER TABLE actor_instances ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
