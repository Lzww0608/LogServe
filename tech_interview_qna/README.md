# 通用技术面试题库

这个目录收录项目涉及的通用技术问题，和 `interview_qna/` 里的项目核心内容问答分开维护。

整理规则：

- 每一批题目单独放一个 Markdown 文件。
- 文件名使用两位序号加主题，例如 `01_crc32.md`。
- 内容按“题目 + 回答”组织，优先服务面试口述，不写成百科条目。
- 回答可以展开原理、实现参数、常见误区和工程使用边界。

已整理：

- `01_crc32.md`：CRC32、CRC32C、checksum、端到端校验、密码学哈希。
- `02_binary_record_protocol_schema.md`：二进制 record、framing、schema evolution、magic number、length prefix、varint。
- `03_filesystem_page_cache_fsync_crash_consistency.md`：文件系统、page cache、fsync、rename、journaling、crash consistency。
- `04_wal_append_only_log_redo_undo_recovery.md`：WAL、append-only log、redo/undo、checkpoint、LSN、group commit。
- `05_segment_index_compaction_retention_lsm.md`：segment、index、compaction、retention、LSM tree、SSTable、tombstone。
- `06_idempotency_retry_timeout_dedup_exactly_once.md`：幂等、重试、超时、去重、idempotency key、backoff、jitter、exactly-once 语义。
- `07_sequence_number_lease_epoch_fencing_logical_time.md`：sequence number、lease、epoch、fencing、Lamport clock、vector clock、HLC、逻辑时间。
- `08_mutex_rwmutex_spinlock_futex_lock_implementation.md`：Mutex、RWMutex、spinlock、futex、锁粒度、公平性、可重入性、锁实现原理。
- `09_atomic_cas_memory_model_lock_free_data_structures.md`：atomic operation、CAS、内存模型、memory ordering、ABA、lock-free、wait-free。
- `10_go_concurrency_runtime_channel_context_race.md`：goroutine、G/M/P、work stealing、goroutine leak、channel、context、race detector。
- `11_worker_pool_local_queue_scheduling_backpressure.md`：worker pool、本地队列、调度、backpressure、rate limiting、load shedding、head-of-line blocking。
- `12_actor_model_mailbox_state_machine_snapshot.md`：Actor model、mailbox、状态机、消息顺序、backpressure、actor crash recovery、snapshot replay。
- `13_dag_workflow_engine_topological_sort_scheduling_semantics.md`：DAG、workflow engine、拓扑排序、ready node、step 依赖、结果引用、取消和超时传播。
- `14_serialization_json_protobuf_canonicalization_fingerprint.md`：序列化、JSON、protobuf、Avro、Thrift、canonical JSON、fingerprint、bytes/string、base64。
- `15_grpc_http2_rpc_reliability.md`：gRPC、HTTP/2、RPC 语义、deadline、status code、重试、flow control。
- `16_python_executor_subprocess_ipc_gil_sandbox_boundary.md`：Python executor、subprocess、IPC、stdout/stderr、进程池、GIL、线程与多进程。
- `17_cache_locality_eviction_policy_consistency.md`：缓存局部性、淘汰策略、cache hit、cache consistency、checkpoint cache。
- `18_llm_serving_model_cache_kv_cache_batching_gpu_scheduling.md`：LLM serving、模型缓存、KV cache、batching、GPU 调度。
- `19_object_storage_s3_result_reference_data_lifecycle.md`：对象存储、S3 语义、大对象 result reference、数据生命周期。
- `20_metadata_store_materialized_view_cqrs_event_sourcing.md`：metadata store、materialized view、CQRS、event sourcing。
- `21_database_transactions_isolation_indexes_postgresql_migrations.md`：数据库事务、隔离级别、索引、PostgreSQL、迁移。
