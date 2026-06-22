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
- `22_message_queue_kafka_nats_sqs_visibility_timeout_redelivery.md`：消息队列、Kafka、NATS、SQS、visibility timeout、redelivery。
- `23_observability_logging_metrics_tracing_slo_p99_analysis.md`：Observability、日志、指标、Tracing、SLO、p99 分析。
- `24_benchmark_load_testing_ablation_profiling_experiment_credibility.md`：Benchmark、压测、ablation、profiling、实验可信度。
- `25_fault_injection_chaos_engineering_recovery_testing.md`：Fault injection、chaos engineering、恢复测试、故障模型、crash consistency。
- `26_distributed_systems_replication_consensus_cap_quorum_consistency.md`：分布式系统、复制、共识、CAP、quorum、一致性模型。
- `27_network_fundamentals_tcp_http_tls_dns_epoll_connection_management.md`：网络基础、TCP、HTTP、TLS、DNS、epoll、连接管理。
- `28_security_hash_auth_authz_tls_sandbox_supply_chain.md`：安全、哈希、认证、授权、TLS、沙箱、供应链、PII、AEAD、HMAC、Argon2、JWT。
- `29_kubernetes_containers_deployment_resource_isolation_sre.md`：Kubernetes、容器、部署、资源隔离、探针、HPA、发布策略、PDB、leader election、CRD/schema 升级。
- `30_system_design_reliable_task_execution_log_scheduler_multitenancy.md`：系统设计综合、可靠任务执行、append-only log、workflow、actor、LLM scheduler、result reference、metadata replay、source of truth、lease/redelivery、多租户平台。
- `31_actor_dag_state_replay_scheduling_optimization.md`：Actor/DAG、mailbox 串行化、状态机 replay、snapshot、workflow 调度、critical path。
- `32_crc32_sha256_hmac_merkle_interview_boundaries.md`：CRC32、SHA-256、HMAC、Merkle Tree 的定义误区、生产事故、指标设计、正确性与性能边界。
- `33_length_prefix_protobuf_canonical_json_fsync_boundaries.md`：length prefix、protobuf、canonical JSON、fsync 的定义误区、生产事故、指标设计、正确性与性能边界。
- `34_page_cache_rename_wal_lsn_interview_boundaries.md`：page cache、rename atomicity、WAL、LSN 的定义误区、生产事故、指标设计、正确性与性能边界。
- `35_checkpoint_group_commit_compaction_sparse_index_lsm_boundaries.md`：checkpoint、group commit、segment compaction、sparse index、LSM tree 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `36_tombstone_idempotency_key_retry_policy_boundaries.md`：tombstone、idempotency key、retry policy 的定义误区、生产事故、指标设计、正确性与性能边界。
- `37_deadline_lease_fencing_token_lamport_clock_boundaries.md`：deadline、lease、fencing token、Lamport clock、vector clock 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `38_mutex_rwmutex_futex_cas_aba_memory_barrier_false_sharing_interview_boundaries.md`：mutex、RWMutex、futex、CAS、ABA、memory barrier、false sharing 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `39_rcu_lock_free_queue_goroutine_channel_interview_boundaries.md`：RCU、lock-free queue、goroutine、channel 的追问、定义误区、生产事故、指标设计、正确性与性能边界。

- `40_context_cancellation_worker_pool_backpressure_actor_mailbox_interview_boundaries.md`：context cancellation、worker pool、backpressure、actor mailbox 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `41_dag_topological_sort_deterministic_replay_grpc_interview_boundaries.md`：DAG、topological sort、deterministic replay、gRPC 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `42_http2_mtls_subprocess_gil_interview_boundaries.md`：HTTP/2、mTLS、subprocess、GIL 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `43_sandbox_lru_tinylfu_cache_stampede_interview_boundaries.md`：sandbox、LRU、TinyLFU、cache stampede 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `44_kv_cache_continuous_batching_object_store_s3_etag_interview_boundaries.md`：KV cache、continuous batching、object store、S3 ETag 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `45_materialized_view_cqrs_interview_boundaries.md`：materialized view、CQRS 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `46_mvcc_serializable_isolation_kafka_partition_visibility_timeout_interview_boundaries.md`：MVCC、serializable isolation、Kafka partition、visibility timeout 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `47_p99_coordinated_omission_flame_graph_jepsen_interview_boundaries.md`：p99、coordinated omission、flame graph、Jepsen 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `48_linearizability_quorum_raft_crdt_interview_boundaries.md`：linearizability、quorum、Raft、CRDT 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `49_tcp_time_wait_epoll_tls_certificate_kubernetes_readiness_interview_boundaries.md`：TCP TIME_WAIT、epoll、TLS certificate、Kubernetes readiness 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `50_cgroup_hpa_slo_error_budget_interview_boundaries.md`：cgroup、HPA、SLO、error budget 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
- `51_multi_tenancy_disaster_recovery_interview_boundaries.md`：multi-tenancy、disaster recovery 的追问、定义误区、生产事故、指标设计、正确性与性能边界。
