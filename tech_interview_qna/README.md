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
