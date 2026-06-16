# 十一、分布式系统原理与生产化追问：安全、多租户与运维

这一组问题要回答得克制一点。LogServe 当前重点在 log-first runtime、workflow、actor 和 LLM 调度，安全能力还不是完整生产级实现。面试时可以坦白说：现在的 Python executor、gRPC、shared log、Kubernetes manifest 更偏实验环境；如果接入真实用户任务，第一批要补的就是代码沙箱、网络隔离、租户边界、密钥管理和运维审计。

## Q796. 执行用户 Python 源码的最大安全风险是什么？

最大风险是远程代码执行失控。LogServe 的 worker 会把用户提交的 Python source 交给 Python executor 执行。只要这段代码不是完全可信，就可能读宿主机文件、访问内网服务、扫描网络、消耗 CPU 和内存、写爆磁盘，甚至偷取环境变量里的密钥。

当前项目里的 Python runner 更像实验执行器。它适合验证 `@task`、`@workflow`、`@actor` 的链路，不适合直接运行不可信用户代码。尤其是长驻 Python 进程会带来两个额外问题：一个任务可能污染全局状态，影响后续任务；用户代码如果导入恶意包或修改运行时环境，worker 很难完全清理。

生产化时，我会把用户代码当成不可信代码处理。最低要求是容器隔离、非 root 用户、只读文件系统、资源限制、网络策略、seccomp/AppArmor。更严格的场景可以考虑 gVisor、Firecracker 或 WASM。平台不能只靠 Python 层的限制，因为 Python 解释器不是安全沙箱。

## Q797. 如何防止用户代码读取宿主机文件？

核心做法是让用户代码看不到宿主机文件。

在 Kubernetes 里，worker 不应该把宿主机路径直接挂进执行容器。不要给用户任务容器挂 `hostPath`，也不要把 Docker socket、kubeconfig、SSH key、云厂商凭据挂进去。容器文件系统尽量设成 read-only，只挂载一个临时工作目录，并且这个目录按 task 或 tenant 隔离。

还要把运行身份降下来。用户代码用非 root UID/GID 执行，drop Linux capabilities，禁止 privilege escalation。配合 seccomp 和 AppArmor/SELinux，限制 `mount`、`ptrace`、加载内核模块这类危险能力。

如果执行器继续用长驻 Python runner，也至少要把 runner 放在受限容器里。更好的设计是每个任务一个短生命周期 sandbox，或者按 tenant 维护隔离 runner pool。这样即使任务读取本地文件，也只能看到平台允许的目录。

日志和结果目录也要小心。`/var/lib/logserve/logstore`、模型 cache、MinIO 凭据、control 配置都不能暴露给用户代码。用户任务能看到的路径应该是白名单，不是“除了少数目录都能看”。

## Q798. 如何防止用户代码访问内网元数据服务？

这要靠网络隔离，不能指望用户代码自觉。

云环境里最危险的是实例元数据服务，例如 `169.254.169.254`。如果用户代码能访问它，就可能拿到节点或 Pod 的临时凭据。第一步是在网络层封掉这类地址。Kubernetes 里可以用 NetworkPolicy、CNI egress policy、iptables 或 eBPF，把任务容器访问 link-local metadata IP 的流量直接拒绝。

第二步是限制出网。用户任务默认不应该拥有任意 egress。比较稳的做法是走出网代理，代理上做域名和端口 allowlist。比如只允许访问用户声明的 API endpoint，不允许扫内网网段，不允许访问 Kubernetes service CIDR。

第三步是云厂商侧配置。比如要求 IMDSv2、限制 metadata hop limit、给节点和 Pod 使用最小权限 IAM。就算某个网络规则漏了，拿到的凭据也不应该有大权限。

LogServe 里 worker 需要访问 control、logd、对象存储和模型源，这些平台流量应该和用户代码流量分开。用户代码不应该直接连 logd 或 control。它只通过 executor 协议返回结果，由 worker 代为完成平台写入。

## Q799. 如何隔离不同 tenant 的任务和日志？

多租户隔离要从命名、权限、资源和存储四层做。

命名上，所有对象都带 `tenant_id`。stream 可以从 `task:<task_id>` 改成 `tenant:<tenant_id>:task:<task_id>`，workflow、actor、LLM stream 也一样。object store 的 key 前缀也带 tenant，例如 `tenants/<tenant_id>/workflows/...`。这样至少不会在查询和清理时混在一起。

权限上，所有 RPC 都从 token 里解析 tenant。用户只能提交自己 tenant 的任务，只能读自己 tenant 的 workflow、actor、result 和 replay log。管理员 API 另走单独权限。

资源上，tenant 要有 quota：最大并发 task、最大 queue depth、最大 workflow fan-out、actor mailbox backlog、LLM token 预算、对象存储容量、日志写入速率。没有 quota 的多租户系统，很容易被一个租户的 fan-out workflow 拖垮。

存储上，metadata store 要按 tenant 建索引或分区。shared log 可以逻辑分 namespace，也可以进一步分 shard。对象存储最好配合 bucket policy 或 prefix policy。加密时也应使用 tenant 级别的 key，避免一个 tenant 的数据泄漏影响全局。

## Q800. shared log 中是否可能泄漏 prompt、结果、密钥？

可能。当前 shared log 的 payload 是 bytes，里面会放 `TaskSubmitted` 的 TaskSpec、workflow definition、step 结果、actor state、LLM event 等内容。TaskSpec 里可能有函数源码和参数，LLM 参数里可能有 prompt，结果较小时也可能 inline 写进事件。只要用户把密钥放进参数、prompt 或源码，shared log 就会记录下来。

所以生产里要给 log payload 定规则。

第一，密钥不进入 log。用户只提交 secret reference，例如 `secret://tenant-a/openai-key`。worker 执行时通过授权的 secret manager 读取，日志里只保留引用。

第二，prompt 和结果要分级。调试环境可以保留明文；生产默认应该脱敏或加密。对敏感 prompt，log 里只放 hash、长度、token count 和 result_ref。

第三，大结果继续走 object store，不要塞进 log。当前项目已有 result ref 机制，这是正确方向。

第四，Dashboard 和 replay API 要做权限控制。shared log 能解释状态，不代表每个人都能看原始 payload。

一句话：shared log 是恢复基础，但不能把它当成无风险的审计文本库。

## Q801. 如何给 payload 加密？谁持有密钥？

我会用 envelope encryption。每条记录或每个 stream 用数据密钥 DEK 加密 payload，DEK 再由 KMS 里的主密钥 KEK 加密。日志 record 里保存 `key_id`、加密后的 DEK、nonce、算法和密文。logd 只负责存 bytes，不需要理解明文。

密钥粒度可以按风险选择：

- 每 tenant 一个 key，管理简单。
- 每 stream 一个 key，隔离更细。
- 每对象一个 key，适合高敏感结果和 snapshot。

谁持有密钥很重要。最好不要让 logd 持有明文密钥。control plane 或专门的 crypto service 根据调用者权限向 KMS 请求解密 DEK，再解 payload。worker 如果要读取任务参数，也只能拿到自己执行所需的那部分明文。

加密范围也要权衡。stream_id、event_type、seq 通常需要明文，否则 logd 很难索引和读取。payload 可以加密。idempotency_key 如果包含业务信息，也应该改成 hash 后的 key，避免从日志元数据里泄漏。

密钥轮换时，新写入使用新 key，旧记录保留旧 key id。后台可以做 re-encryption，但这不是第一步必须做的。

## Q802. 如何实现 RBAC：谁能提交任务、查看结果、replay log？

RBAC 要落在 API 层，而不是只靠前端隐藏按钮。

可以先定义几类角色：

- submitter：提交 task、workflow、actor call，查询自己提交的对象。
- developer：查看本 tenant 的运行状态、错误、部分日志和结果。
- operator：查看 worker、queue、backpressure、scheduler 状态，做重试和取消。
- auditor：只读审计事件，可以 replay 但不能修改状态。
- admin：管理 tenant、配额、密钥、模型注册和系统配置。

每个 RPC 都要标注权限。`SubmitTask`、`SubmitWorkflow` 属于 submit 权限；`GetTaskStatus`、`GetWorkflowStatus` 属于 read 权限；`ReplayWorkflow`、`ReplayActor`、`ReadLog` 风险更高，需要 replay 或 audit 权限；`SetSchedulingPolicy`、backpressure 配置、model registry 修改应归 operator/admin。

实现上，gRPC 加认证拦截器。token 里有 user_id、tenant_id、roles。control plane 在处理请求前做鉴权，并把 tenant_id 写入对象 metadata 和 log payload。logd 如果对外暴露 `ReadLog`，也要鉴权；更稳的是 logd 只对 control 暴露内部接口，用户所有读写都走 control 的授权路径。

RBAC 还要配合审计。谁 replay 了哪条 stream，谁下载了哪个 result_ref，都要留下记录。

## Q803. 如何做审计和不可抵赖？

审计要记录“谁在什么时候做了什么”，不可抵赖要防止事后篡改或抵赖。

LogServe 已经有 append-only shared log，这是很好的基础，但业务事件日志和安全审计日志最好分开。业务日志用于 replay 状态，审计日志用于追踪用户和管理员行为。审计事件可以写入 `audit:<tenant_id>` 或独立 audit log：

```text
AuditEvent {
  actor_user_id
  tenant_id
  action
  resource_type
  resource_id
  request_id
  source_ip
  timestamp_ms
  result
}
```

不可抵赖需要更强的完整性保护。可以给审计日志加 hash chain：每条记录包含上一条记录 hash。也可以定期把当前 root hash 写到外部可信位置，比如对象存储 WORM bucket、KMS 签名日志、甚至另一个独立审计系统。

管理员操作尤其要审计。比如修改 backpressure、调整 scheduler policy、删除对象、变更 RBAC、轮换密钥、触发 replay。这些不是普通运行事件，排查事故时很关键。

注意一点：审计日志不等于把所有 payload 明文写进去。审计记录可以保存 hash 和引用，避免审计系统本身变成新的泄漏源。

## Q804. 如何防止恶意用户提交超大 payload 打爆 logd？

入口要限流，logd 也要自保。

第一，在 SDK 和 control 层限制请求大小。TaskSpec、workflow definition、args_json、function_source、LLM prompt 都要有最大字节数。超过限制直接拒绝，错误要明确。

第二，结果大小也要限制。当前项目已有 `resultInlineThreshold`，大结果写 object store，log 里只放 result_ref。生产里还要限制单个 result object 大小、单 workflow 总结果大小、单 tenant 每日写入量。

第三，logd 层自己要设置最大 record size。不能只相信 control。即使有人绕过 control 直接打 logd，logd 也应该拒绝超大 payload。

第四，加 backpressure。队列深度、last log append latency、log disk usage、object store latency 都可以作为拒绝新任务的信号。恶意用户持续提交时，还要按 tenant rate limit，不要影响其他 tenant。

第五，function_source 不适合无限传。长期方案是代码包或镜像引用，log 里记录 artifact digest，而不是每次把大段源码写进 log。

## Q805. 如何限制 workflow fan-out 防止资源滥用？

workflow fan-out 是典型滥用入口。一个用户提交一个 DAG，如果瞬间展开成几千个 step，就会打满 control queue、log append、worker executor 和对象存储。

限制要放在几个地方。

提交时先检查 definition。限制最大 step 数、最大边数、最大 fan-out 宽度、最大嵌套层级、单 step 参数大小。超限的 workflow 不进入 log。

运行时加并发上限。比如每个 workflow 同时最多运行 N 个 step，每个 tenant 同时最多运行 M 个 workflow step。ready step 可以先放在 workflow ready set，按 token 慢慢发 task。

还要限制 retry 放大。一个大 fan-out workflow 如果每个 step 都 retry 3 次，实际任务数会翻倍。配额计算要按 worst-case attempts 算，而不是只看原始 step 数。

LLM step 还要单独管。LLM 请求消耗模型 cache、显存和 token 预算，不能和普通 CPU step 用同一套 quota。

如果用户确实需要大规模 fan-out，平台应该要求显式申请更高 quota，并把这个 workflow 标记成 batch workload，用低优先级队列执行。

## Q806. 如何对 actor mailbox 做 per-tenant quota？

actor mailbox 的风险是堆积。一个 tenant 可以对大量 actor 或单个热点 actor 提交很多 command，导致 control metadata、队列和 worker actor pool 被占满。

quota 可以分几层：

- tenant 级：一个 tenant 全部 actor pending command 总数。
- actor 级：单个 actor mailbox 最大长度。
- 方法级：某些昂贵 method 限制并发或频率。
- 字节级：pending command 参数总大小。

提交 actor call 时，control 在写 `ActorCommandSubmitted` 之前先检查 quota。如果超过限制，直接返回 backpressure 或 quota exceeded。注意这里要遵守 log-first：被拒绝的请求可以写审计日志，但不应该写成业务 command，否则 replay 会认为它曾经进入 mailbox。

执行完成后释放 quota。失败、超时、取消也要释放。为了防止 control crash 后 quota 计数不准，quota view 要能从 actor stream 和 task stream 重建。

单 actor 热点不能只靠 quota 解决。quota 是保护系统，热点本身还要靠 sharding、batching 或业务建模调整。

## Q807. 如何处理用户任务依赖供应链风险？

用户 Python 代码通常会依赖 pip 包。供应链风险包括恶意包、typosquatting、依赖劫持、安装脚本执行恶意命令、包版本漂移。

我会避免 worker 在运行任务时直接 `pip install`。更安全的流程是离线构建 artifact：

1. 用户提交依赖声明。
2. 构建服务在受控环境里解析依赖，生成 lockfile。
3. 扫描包漏洞和许可证。
4. 构建容器镜像或 zip artifact。
5. 记录 digest，任务只引用 digest。

运行时 worker 只拉取已经审核过的 artifact，不访问公网包仓库。这样任务执行链路更可控，也方便复现。

还要支持 allowlist 和 denylist。核心环境里的基础包由平台维护；用户自带依赖放在隔离环境里。对高风险包、native extension、安装脚本，要更严格。

LogServe 当前通过源码传递任务，适合 demo。生产化时应该把“源码传输”升级为“构建产物引用 + digest 校验”。

## Q808. 如何做容器镜像签名和代码签名？

镜像签名的目标是确认 worker、control、logd 和用户执行镜像没有被替换。可以用 cosign 这类工具给镜像 digest 签名。部署时 admission controller 校验签名，不通过就拒绝 Pod 启动。

代码签名适用于用户提交的任务包。构建服务生成 artifact 后，计算 digest，并用平台私钥签名。TaskSpec 里不要存一大段代码，而是存：

```text
artifact_ref
artifact_digest
signature
builder_id
```

worker 拉取 artifact 后先校验 digest 和签名，再执行。这样可以防止对象存储里的包被替换，也能追溯是谁构建的。

还要记录构建环境。比如基础镜像 digest、依赖 lockfile、构建时间、扫描结果。出了事故时，不能只知道“跑了某个任务”，还要知道它到底用了哪份代码和依赖。

Kubernetes 里可以配合镜像准入策略，只允许来自受信仓库、带有效签名、通过漏洞扫描阈值的镜像运行。

## Q809. 如何管理 secret，不让其进入 shared log？

原则很简单：日志里只放 secret reference，不放 secret value。

用户提交任务时，如果需要 API key、数据库密码、对象存储凭据，SDK 只允许传 `secret_ref`。例如：

```text
secret://tenant-a/openai-api-key
```

control 把这个引用写进 TaskSpec 或 workflow definition。worker 执行前，拿自己的身份和 task 的 tenant_id 去 secret manager 请求临时解密。secret manager 检查 worker 是否被授权执行该 tenant 的任务，再返回短期凭据。

worker 拿到 secret 后也不能把它写回日志。stdout/stderr 要做脱敏，异常堆栈也要扫描常见 secret 格式。任务结果如果包含 secret，要么拒绝写入，要么脱敏。

在 Kubernetes 里，平台自身 secret 用 Kubernetes Secret 或外部 secret operator 管理。不要放在 ConfigMap、命令行参数或普通日志里。Compose 文件里的明文 MinIO/PostgreSQL 密码只能算本地实验配置，不能照搬到生产。

## Q810. 如果部署到 Kubernetes，哪些组件需要 StatefulSet？哪些是 Deployment？

当前 manifests 里 `logserve-logd` 用的是 Deployment 加 PVC，实验环境可以跑。生产里我更倾向把有稳定身份和持久存储要求的组件改成 StatefulSet。

logd 需要 StatefulSet。它有本地 logstore 数据目录，需要稳定网络身份和 PVC。如果未来做多副本 Raft logd，每个副本都要有固定 ordinal 和独立 PVC。

PostgreSQL、MinIO、NATS JetStream 也属于有状态组件。实验 compose 里它们直接作为服务启动；Kubernetes 里更适合使用成熟 operator 或托管服务，而不是手写简单 Deployment。

control plane 通常可以是 Deployment。当前项目默认单 control；生产化如果做 leader election，可以多副本 Deployment，但同一时间只有 leader 负责调度写路径。

worker 适合 Deployment 或 Job/DaemonSet，取决于定位。普通 CPU worker 用 Deployment。需要每个节点一个、有本地模型 cache 或 GPU 亲和的 worker，可以用 DaemonSet 或带 node selector 的 Deployment。LLM worker 如果绑定 GPU 和本地盘，可能需要更稳定的 placement。

Dashboard/API gateway 这类无状态组件也用 Deployment。

## Q811. logd 的数据目录如何使用 PVC？

logd 的数据目录保存 segment log、index、retention metadata。它不能放在 Pod 临时盘里。Pod 重启后如果数据目录丢了，shared log 就丢了，metadata view 也无法完整 replay。

Kubernetes 里应该给 logd 挂 PVC，例如当前 manifest 里的 `/var/lib/logserve/logstore`。PVC 至少要满足：

- ReadWriteOnce，单副本 logd 独占写。
- 使用可靠 StorageClass，不要用容易丢数据的临时盘。
- 容量按日志增长和 retention 规划。
- 打开监控，关注磁盘使用率、IO latency、inode。

如果 logd 未来做 Raft，多副本各自有 PVC。不能多个 logd Pod 共享同一个 ReadWriteMany 目录来写同一份 segment，这会破坏日志文件的一致性。

备份上，PVC snapshot 只是底层快照。最好配合 logd 自己的 checkpoint 或 segment 边界，确保恢复时不会拿到不一致的文件状态。恢复后仍要走 logstore recovery，扫描 segment、重建 index、处理 partial tail。

## Q812. worker 的 model cache 应该用 emptyDir、hostPath 还是 PVC？

要看 cache 的目标。

`emptyDir` 最简单，Pod 删除后 cache 消失。它适合实验和便宜模型，优点是生命周期清楚，不会留下脏数据。缺点是每次调度到新 Pod 都可能 cold start。

`hostPath` 可以保留节点本地 cache。Pod 重启或重新调度到同一节点时，模型还在，适合 LLM checkpoint cache。风险是隔离差，权限要管好；不同 tenant 的模型不能随便放在同一个无权限边界的目录里。Kubernetes 里更推荐用 local PV 表达这类本地盘，而不是裸 hostPath。

PVC 适合需要跨 Pod 保留的数据，但对模型 cache 不一定最好。很多网络存储吞吐和延迟不如本地 SSD，模型加载会慢。PVC 更适合存 source checkpoint 或共享 artifact，不一定适合热 cache。

我的选择是：实验用 `emptyDir`；单节点或固定节点 LLM worker 用 local PV/hostPath 作为 cache；生产模型源放对象存储或专门模型仓库；cache 只是可丢的加速层，不作为唯一数据来源。

## Q813. 如何设计 rolling upgrade，避免正在执行任务丢失？

先让 worker 支持 draining。

升级 worker 前，Pod 收到终止信号后不要立刻退出。它先停止 PollTask，不再接新任务；已经 dispatch 到本地 executor 的任务继续跑，或者在超时内尝试完成。完成后正常写 TaskCompleted/TaskFailed，再退出。如果超过 terminationGracePeriod，任务可能被杀，control 依赖 lease timeout 做 redelivery。

actor worker 更麻烦。它还拥有 actor ownership。draining 时要停止接新 actor command，等待 mailbox 清空，或者主动释放 ownership，让其他 worker 以更高 epoch 接管。旧 worker 后续 completion 会被 epoch fencing 拒绝。

control plane 升级时，当前项目可以靠 log bootstrap 恢复 metadata view。生产里多副本 control 要有 leader election，新 leader 接管前先从 log catch up，再开放调度。

logd 升级最敏感。单副本 logd 升级期间系统写入不可用。多副本 Raft logd 可以滚动升级 follower，再切 leader。升级前要确认所有 committed log 都落盘，升级后跑 recovery 和一致性检查。

对客户端来说，rolling upgrade 期间要接受短暂重试。SDK 必须使用 idempotency key，避免 gRPC 超时后重复创建对象。

## Q814. 如何做配置变更审计？

配置变更要走 control API，不要让人直接改环境变量或手工 patch 运行中状态。

LogServe 里像 scheduler policy、backpressure、redelivery timeout、tenant quota、model registry、RBAC、secret policy 都属于需要审计的配置。每次变更应写一条配置事件，例如：

```text
ConfigChanged {
  key
  old_value_hash
  new_value_hash
  changed_by
  reason
  timestamp_ms
}
```

敏感值不要明文写入审计日志，记录 hash 或引用即可。比如 secret 轮换记录 key id 和版本号，不记录 secret value。

配置生效也要可 replay。当前项目已经把 backpressure 和 scheduler policy 这类配置写到 system stream，并能 bootstrap 恢复，这是好方向。生产里可以扩展成统一 config stream，control 重启后从 config stream 重建当前配置。

还要支持审批和回滚。面试里可以说：我不希望生产配置只存在某个 Pod 的 env 里。配置本身也应该事件化，这样才知道谁改了、改了什么、什么时候生效、能不能回滚。

## Q815. 如何实现灾难恢复演练？

灾难恢复不能只写文档，要定期演练。

LogServe 的核心恢复对象有三类：shared log、metadata store、object store。shared log 是 source of truth；metadata store 是 materialized view；object store 保存大结果和 actor snapshot。三者都要纳入备份。

演练可以按步骤做：

1. 在测试环境运行一批 workflow、actor、LLM 请求，生成已知结果。
2. 备份 logstore 数据目录、PostgreSQL、MinIO bucket。
3. 模拟故障：删除 metadata 表、替换 control 节点、重建 worker、恢复 logd PVC。
4. 从备份恢复 logstore 和 object store。
5. 启动 logd，再启动 control，让 `BootstrapFromLog` 重建 metadata view。
6. 跑一致性检查：workflow replay 与 DB 状态一致，actor get() 返回预期值，result_ref 能读到对象，dashboard 统计正常。

还要测部分失败。比如 metadata 丢了但 log 和 object store 还在；object store 丢了但 log 里有 result_ref；logstore 有 partial tail；某个 actor snapshot 丢失。不同故障的恢复能力不一样，要写清楚。

最后产出两类指标：RPO 和 RTO。RPO 表示最多会丢多少数据，RTO 表示多久恢复服务。当前单机实验只能证明功能路径，不能证明生产级灾备。生产化要把备份、恢复、校验和演练报告自动化。
