# 29. Kubernetes、容器、部署、资源隔离与 SRE

这一章回答 Kubernetes、容器、部署、资源隔离和 SRE 相关问题。面试里这类题很容易被答成对象名清单：Pod、Deployment、Service、Ingress、PV。真正要讲清楚的是边界：容器隔离不是虚拟机隔离；request 是调度承诺，limit 是运行时约束；readiness 管流量，liveness 管重启；滚动发布改变的是副本集合，不会自动解决协议和数据兼容。

结合 LogServe 时，我会按仓库里的运行示例来讲。当前 Kubernetes manifests 包含 `logd`、`control` 和两个 `worker` Deployment，`logd` 使用 PVC 保存 append-only log。这个部署文件适合说明运行形态，但项目实验结论仍来自单机 Ubuntu、3 worker、mock LLM 和 file-backed checkpoint cache。回答时要守住这个边界：可以说系统已经有 Kubernetes 运行示例，不能说它已经完成多节点生产级验证。

## Q001. 容器和虚拟机的隔离边界有什么不同？

**回答：**

容器和虚拟机最根本的差别在内核边界。虚拟机通常有自己的 guest kernel，通过 hypervisor 和虚拟硬件运行。容器没有自己的内核，它和宿主机、同一台机器上的其他容器共享 host kernel，只是在进程视图、文件系统视图、网络视图、用户视图和资源用量上做隔离。

容器常见隔离手段来自 Linux：namespace 负责把全局资源包装成隔离视图，比如 PID、mount、network、IPC、UTS、user namespace；cgroup 负责限制和统计资源，比如 CPU、memory、PIDs、I/O；seccomp 限制系统调用；capabilities 把 root 权限拆小；AppArmor、SELinux 这类 LSM 负责强制访问控制。它们合起来让一个进程“看起来”像在自己的运行环境里，但系统调用最终还是进宿主机内核。

虚拟机的隔离边界更厚。一个 VM 里的进程先进入 guest kernel，再通过虚拟化层访问宿主资源。攻击者要从 VM 逃逸，通常要突破 guest kernel、虚拟设备或 hypervisor 边界。容器逃逸的路径更直接：如果容器里有高权限、挂了宿主路径、保留了危险 capabilities，或者内核本身有漏洞，攻击面会落在共享宿主内核上。

这不是说容器“不安全”。容器的价值在于轻量、启动快、镜像分发方便、资源密度高、开发和运行环境一致。只是它的安全边界不是 VM 级别。面试里我会避免说“容器就是轻量虚拟机”。更准确的说法是：容器是进程级隔离和资源控制，虚拟机是机器级隔离和独立内核。

还有一个工程差别：VM 里的内核参数、文件系统、系统服务更完整，适合强隔离、多租户、不同内核版本的场景；容器更适合同一内核上运行多个应用进程，配合镜像和编排系统做部署。如果是强不可信代码执行，比如开放给外部用户的沙箱，单靠普通容器不够，通常要叠加 gVisor、Kata Containers、Firecracker、独立 VM 或更严格的 seccomp/LSM 策略。

结合 LogServe，worker 会执行 Python task 或 LLM 调用逻辑。如果这些任务来自可信项目代码，容器隔离加上最小权限、只读根文件系统、非 root 用户、限制 capabilities 是合理的运行边界。如果要跑外部用户提交的任意 Python 代码，那就不能只说“放进容器就安全”。需要把系统调用、网络访问、文件访问、CPU/memory/PID 上限、超时、输出大小和宿主路径挂载全部收紧。

面试里可以这样回答：

```text
虚拟机有独立 guest kernel，隔离边界在 hypervisor 和虚拟硬件层；容器共享宿主机内核，主要靠 namespace 做视图隔离，靠 cgroup 做资源控制，再配合 seccomp、capabilities、AppArmor/SELinux 限制权限。容器更轻，但它不是 VM 级隔离。对不可信代码，普通容器通常要叠加更强沙箱。
```

## Q002. namespace 和 cgroup 分别解决什么问题？

**回答：**

namespace 解决“看见什么”的问题，cgroup 解决“能用多少”的问题。这个区分很重要，因为很多容器问题不是“隔离有没有”，而是某个维度隔离了，另一个维度没有隔离。

namespace 把宿主机的全局资源变成进程自己的局部视图。PID namespace 让容器里的进程看到自己的 PID 树，容器里的 1 号进程不一定是宿主机的 1 号进程；mount namespace 让容器看到自己的文件系统挂载点；network namespace 让容器有自己的网卡、路由表、端口空间；UTS namespace 隔离 hostname；IPC namespace 隔离 System V IPC 和 POSIX message queue；user namespace 可以让容器内 UID 0 映射成宿主机上的非 root UID。namespace 的效果是：进程以为自己在一个独立系统里。

cgroup 不负责隐藏资源，它负责资源会计、限制和优先级。CPU cgroup 可以限制 CPU quota 或权重；memory cgroup 限制内存使用，超过后触发 OOM；pids cgroup 限制进程数量，防止 fork bomb；I/O 控制器可以限制块设备读写；cgroup v2 还提供统一层级和更一致的资源模型。cgroup 的效果是：进程即使看见机器有很多资源，也只能按分配额度使用。

举个简单例子：一个容器通过 PID namespace 只能看到自己内部的进程，但如果没有 pids cgroup，它仍可能疯狂 fork，把宿主机进程表打满。另一个例子，容器通过 mount namespace 看到自己的根文件系统，但如果你把 `/var/run/docker.sock` 或宿主机目录挂进去，namespace 并不会神奇地保护宿主资源。挂载点本身就是你暴露出去的边界。

Kubernetes 里的 Pod 会把这些机制包装起来。Pod 内多个容器通常共享 network namespace，所以它们看到同一个 Pod IP，也可以通过 `localhost` 互相访问；它们可以共享 volumes；默认情况下不共享 PID namespace，除非显式设置。资源 request 和 limit 最终也会落到 kubelet 和容器运行时创建的 cgroup 上。Pod 是 Kubernetes 的抽象，namespace/cgroup 才是 Linux 运行时的底层机制。

面试里常见误区有两个。第一，把 namespace 当作安全边界的全部。它只是视图隔离，还要看 capabilities、seccomp、LSM、rootfs 是否只读、宿主路径是否挂载。第二，把 cgroup 当作调度器。cgroup 限制单机资源使用，不负责决定 Pod 放到哪台机器；Kubernetes scheduler 根据 request、亲和性、污点容忍等信息做调度。

结合 LogServe，如果 worker 容器要限制 Python executor 的影响范围，namespace 只能让它看不到宿主机的大部分进程和网络；cgroup 才能限制它最多吃多少 CPU、内存和进程数。还要配合应用层超时，因为 cgroup 不知道某个 workflow step 的业务 deadline。

面试里可以这样回答：

```text
namespace 做资源视图隔离，让进程看到自己的 PID、网络、挂载、IPC、hostname、用户空间；cgroup 做资源会计和限制，控制 CPU、内存、PIDs、I/O 等用量。namespace 管“你看见什么”，cgroup 管“你能用多少”。容器隔离通常要两者一起用，再叠加权限和系统调用限制。
```

## Q003. Docker image layer 会如何影响构建和分发？

**回答：**

Docker image 不是一个大 tar 包，而是一组内容寻址的只读层。Dockerfile 里的 `FROM`、`RUN`、`COPY`、`ADD` 等指令通常会产生层或影响构建缓存。镜像运行成容器时，运行时再叠加一个可写层。这个模型直接影响构建速度、分发速度、镜像大小和安全扫描。

第一，layer 可以复用。多个镜像如果共享相同基础层，节点拉取时只需要下载一次。比如十个服务都基于同一个 Go runtime base image，基础层已经在节点上，后续只拉应用层会快很多。CI 构建也是类似，前面的层如果输入没变，可以命中 cache，后面的层才重新执行。

第二，layer 也会放大错误写法。比如你在一个 `RUN` 里下载 500MB 临时文件，下一层再 `rm -rf` 删除，最终镜像历史里仍保留前一层的内容。删除只是让上层的文件系统视图看不到它，不会把底层 layer 的字节抹掉。正确做法是在同一个 `RUN` 里下载、使用、清理，或者用 multi-stage build 只把最终二进制拷到运行镜像。

第三，Dockerfile 顺序会影响 cache 命中。稳定、变化少的步骤应该放前面，变化频繁的应用源码放后面。Go 项目常见写法是先复制 `go.mod`、`go.sum` 下载依赖，再复制源码构建。否则每改一行代码，依赖下载层也失效，CI 时间会被拉长。

第四，layer 会影响分发和回滚。镜像 tag 是可变引用，`latest` 或 `prod` 可以被重新指向不同 digest。生产部署最好记录 immutable digest，避免同一个 tag 在不同节点拉到不同内容。Kubernetes 的 `imagePullPolicy`、节点本地缓存和 registry 可用性也会影响发布行为。一个看起来只是“改镜像”的问题，最后可能变成每个节点拉取时间不同、启动时间不同、回滚镜像找不到的问题。

第五，layer 还影响安全。密钥、token、私有 npmrc、pip.conf 如果进入某一层，即使后面删除，也可能被镜像历史或 registry 中的 layer 恢复出来。构建密钥要走 build secret，不要 `COPY` 进镜像。安全扫描也往往按 layer 和文件系统最终视图分析，基础镜像太旧会带来大量 CVE 噪音。

结合 LogServe，`deployments/Dockerfile` 用同一个镜像承载 `logserve-logd`、`logserve-control`、`logserve-worker` 和 Python executor。这个方式对本地 kind/minikube 运行很方便，但如果后续走生产化，可以考虑把构建层和运行层拆开，减少镜像体积，固定基础镜像 digest，避免把测试数据、模型 checkpoint、开发脚本或本地凭证打进镜像。worker 需要的模型或 checkpoint 不应该随便塞进应用镜像，否则每次模型变更都会变成一次大镜像分发。

面试里可以这样回答：

```text
镜像 layer 让构建缓存和跨镜像分发复用变得高效，但也会让错误写法留下历史字节。频繁变化的文件放太前面会打穿 cache；先添加再删除大文件不会真正减小底层 layer；密钥进入 layer 后不能靠后续删除解决。生产里要用 multi-stage build、固定镜像 digest、优化 Dockerfile 顺序，并把模型或大数据和应用镜像分开管理。
```

## Q004. Kubernetes Pod 的最小调度单元意味着什么？

**回答：**

Pod 是 Kubernetes 里最小的调度单元，意思是 scheduler 调度的是整个 Pod，而不是 Pod 里的单个容器。一个 Pod 一旦被绑定到某个 Node，Pod 里的所有普通容器都在这台 Node 上运行，共享同一个 Pod 生命周期、Pod IP 和一组 volumes。你不能让同一个 Pod 里的 A 容器跑在 node-1，B 容器跑在 node-2。

这带来几个直接后果。

第一，资源 request 会按 Pod 聚合。调度器看的是 Pod 内所有容器 request 的总量，再判断某个节点是否放得下。如果一个 sidecar request 过高，主容器即使很轻，也会让整个 Pod 难以调度。反过来，如果 sidecar 没有 request，它在运行时吃掉 CPU 和内存，会影响主容器，还可能让 Pod 的 QoS 等级变差。

第二，Pod 内容器适合放强耦合组件，而不是随便打包。典型 sidecar 是日志代理、service mesh proxy、本地缓存代理、配置热更新器。它们和主容器需要共享网络、共享 volume、共同生命周期。两个可以独立扩缩容、独立发布、独立失败的服务，不应该因为“部署方便”塞进同一个 Pod。

第三，Pod IP 是 Pod 级别，不是容器级别。Pod 内多个容器共享 network namespace，可以用 `localhost` 互相访问。端口冲突也发生在 Pod 内：两个容器不能同时监听同一个 Pod IP 上的同一端口。Service 选择的是 Pod endpoints，不知道你内部哪个容器才是真正服务请求，除非端口定义清楚。

第四，Pod 是相对短命的。Deployment 滚动更新、节点驱逐、镜像升级、探针失败都会导致 Pod 被删除重建。Pod 名字、IP、容器可写层都不能当成持久身份。需要稳定身份时，通常要用 StatefulSet、PVC、Headless Service 或应用自己的成员管理。

结合 LogServe，`control`、`logd`、`worker` 被拆成不同 Deployment 是合理的，因为它们职责不同、扩缩容和故障模式不同。`worker` 里面的 Python executor 可以作为同容器进程或 sidecar 处理，这取决于你要不要独立重启、独立限流、独立观测。如果 Python executor 崩溃会拖垮 worker，拆成 sidecar 也不一定解决语义问题，因为 Pod 仍然是共同调度和共同生命周期；应用层还得处理任务重试、lease 和幂等。

面试里可以这样回答：

```text
Pod 是 Kubernetes 调度和生命周期管理的最小单位。调度器把整个 Pod 放到一个节点上，Pod 内容器共享网络、Pod IP、volume 和生命周期，资源 request/limit 也会共同影响调度和运行。适合放强耦合 sidecar，不适合把本来能独立扩缩容的服务硬塞到一起。
```

## Q005. Deployment、StatefulSet、DaemonSet 适合什么场景？

**回答：**

这三个控制器都能管理 Pod，但它们表达的运行语义不同。选错控制器，后面会在发布、存储、服务发现和故障恢复上付成本。

Deployment 适合无状态或弱状态服务。它通过 ReplicaSet 维持副本数，支持滚动更新、回滚、扩缩容。典型场景是 HTTP API、control plane 无状态实例、前端服务、普通 worker。Deployment 的 Pod 名字和 IP 不稳定，副本之间没有固定顺序，也没有稳定存储身份。应用必须能接受任意副本被替换。

StatefulSet 适合需要稳定身份的服务。它给 Pod 分配稳定 ordinal，比如 `db-0`、`db-1`，通常配合 Headless Service 得到稳定 DNS，也能为每个 ordinal 绑定稳定 PVC。它还支持有序创建、删除和更新。典型场景是数据库、ZooKeeper/etcd 这类 quorum 系统、有成员编号的复制系统、需要固定 shard 身份的服务。StatefulSet 不是“有状态就一定用”，它解决的是稳定网络身份和稳定存储身份。

DaemonSet 适合每个节点都要跑一个副本的节点级组件。比如日志采集器、监控 agent、CNI 插件、CSI node plugin、节点本地缓存、安全 agent。DaemonSet 会随着节点加入而创建 Pod，节点移除时清理 Pod。它不是用来跑普通业务副本的，因为它的副本数由节点数决定，不由业务 QPS 决定。

结合 LogServe，当前 manifests 里 `logd`、`control`、`worker-a`、`worker-b` 都是 Deployment。作为本地运行示例可以接受。若要推进生产语义，`worker` 通常还是 Deployment，因为 worker 副本本身应当可替换，任务恢复依赖 shared log、lease 和幂等。`logd` 如果只是单副本演示，用 Deployment + PVC 可以跑起来；如果要做真正的复制日志服务，就要重新设计成员、复制、leader、fencing、存储和升级策略，可能会走 StatefulSet 或直接使用成熟存储系统。`control` 如果变多副本，要解决 leader election、队列一致性、metadata store 和事件写入幂等，不能只把 `replicas` 从 1 改成 3。

面试里可以这样回答：

```text
Deployment 管可替换副本，适合无状态服务和普通 worker；StatefulSet 管稳定 ordinal、稳定 DNS 和每副本 PVC，适合数据库、quorum 和有固定成员身份的服务；DaemonSet 按节点部署，适合日志、监控、网络、存储插件等节点级 agent。选择控制器要看身份、存储、发布顺序和副本数语义。
```
## Q006. ConfigMap 和 Secret 的区别是什么？

**回答：**

ConfigMap 和 Secret 都是把配置数据交给 Pod 使用的 Kubernetes API 对象。它们的差别不在“能不能挂载成文件”这种表面能力，而在数据类型、权限语义和安全预期。

ConfigMap 用来放非敏感配置。比如服务端口、feature flag、日志级别、配置模板、普通环境变量。它可以作为环境变量注入，也可以挂载成文件。ConfigMap 适合把镜像和环境配置分离，让同一个镜像在 dev、staging、prod 里使用不同配置。它不应该放密码、token、私钥。

Secret 用来放敏感配置。比如数据库密码、TLS 私钥、registry pull secret、service account token、API key。Secret 的 `data` 字段通常是 base64 编码，这只是传输和 YAML 表达形式，不是加密。Secret 的价值在于 Kubernetes 对它有专门对象类型、RBAC 可以单独授权、kubelet 可以把它以 volume 或 env 的方式注入 Pod，一些发行版或托管集群也会对 Secret 启用静态加密和审计策略。

两者都有几个共同边界。第一，环境变量注入不会随对象更新自动改变，容器通常要重启才能拿到新值。volume 挂载的 ConfigMap/Secret 可以被 kubelet 更新，但更新有同步延迟，应用还要自己重新加载文件。第二，单个对象大小有限，不适合塞大配置、大证书链或模型文件。第三，把 Secret 注入环境变量有泄露风险，进程 dump、错误日志、debug 页面、`/proc` 访问都可能暴露它；挂载成文件并设置文件权限通常更好。

Secret 还要考虑权限最小化。能 `get secrets` 的主体通常就能拿到明文数据，至少能拿到 base64 后的内容再解码。对 Secret 的 RBAC 要比普通 ConfigMap 严。不要为了方便给所有 service account 授权读 namespace 下全部 Secret。也不要把 Secret 写入应用日志、错误返回、metrics label 或 crash dump。

结合 LogServe，普通配置如 `LOGSERVE_CONTROL_ADDR`、`LOGSERVE_METADATA_STORE`、worker 模型列表可以放 ConfigMap 或 Deployment args/env；S3 access key、OpenAI-compatible adapter key、数据库密码、TLS 私钥要放 Secret。更进一步，如果 task payload 或 LLM prompt 本身含敏感内容，不能以为“Secret 管好了就安全”，还要管日志、result object、snapshot 和 shared log 里的数据边界。

面试里可以这样回答：

```text
ConfigMap 放非敏感配置，Secret 放密码、token、私钥这类敏感数据。Secret 的 base64 不是加密，真正的安全来自 RBAC、etcd 静态加密、最小权限、避免日志泄露和合理挂载方式。两者都可以通过 env 或 volume 注入，但 env 不会自动热更新，volume 更新也要应用自己重新加载。
```

## Q007. Kubernetes Secret 默认是否加密？

**回答：**

默认不要把 Kubernetes Secret 当成“已经加密保存”。Secret 在 API 里用 base64 表示，不等于加密。Kubernetes 可以配置 encryption at rest，把 Secret 等资源在写入 etcd 前加密；也可以接 KMS provider，把数据加密密钥交给外部 KMS 管理。但这需要集群配置。不同托管集群可能默认启用不同策略，面试里要先讲 Kubernetes 的语义，再说明要检查具体集群。

如果没有开启静态加密，Secret 进入 etcd 时就是可由 etcd 存储层读取的数据。攻击者拿到 etcd 备份、磁盘快照、apiserver 高权限、能读 Secret 的 RBAC 权限，都可能取出内容。即使开启静态加密，能通过 Kubernetes API `get secret` 的用户仍然能读到解密后的 Secret，因为 apiserver 会负责解密后返回。静态加密保护的是存储介质和 etcd 层，不是授权绕过的万能药。

Secret 的安全链条至少包括几层。第一，启用 etcd encryption at rest，优先使用受管 KMS 或合理的 key rotation。第二，RBAC 最小化，应用 service account 只读自己需要的 Secret。第三，限制能创建 Pod 的权限，因为能创建 Pod 的人可能把 Secret 挂进自己 Pod 里读出来。第四，避免 Secret 以环境变量形式进入日志和 crash dump。第五，管好备份、审计和 Secret 轮换。

还有一个细节：很多人只盯着 Secret 对象本身，却忘了 Secret 被消费后的路径。TLS 私钥挂进容器后，容器内进程能读；应用把数据库连接串打印到启动日志里，Secret 就进了日志系统；worker 把带 key 的配置写进 result object，Secret 就进了对象存储。Secret 对象的保护只是第一站。

结合 LogServe，如果以后把 S3 credentials、数据库密码、LLM adapter token 放进 Kubernetes Secret，要在文档里明确：Secret 默认 base64 不代表加密；生产部署需要启用 Secret 静态加密，限制 worker/control 的 service account 权限，并检查日志和 dashboard 不输出敏感值。当前仓库 manifests 是运行示例，没有这套完整安全配置，不能把它描述成生产 Secret 管理方案。

面试里可以这样回答：

```text
Kubernetes Secret 的 base64 不是加密。Secret 是否在 etcd 中加密，取决于集群是否配置了 encryption at rest 或 KMS provider。即使开启静态加密，拥有 API 读取权限的人仍能拿到解密后的 Secret，所以还要做 RBAC 最小化、限制 Pod 创建权限、轮换密钥、避免日志和备份泄露。
```

## Q008. liveness probe、readiness probe、startup probe 的区别是什么？

**回答：**

这三个 probe 解决的是不同问题。最短的区分是：liveness 决定要不要重启容器，readiness 决定要不要接流量，startup 决定启动阶段先别急着用 liveness 杀它。

liveness probe 检查进程是否还活在可恢复状态。如果 liveness 连续失败，kubelet 会按容器的 restart policy 重启容器。它适合检测死锁、主循环卡死、内部状态不可恢复这类“重启可能有用”的问题。它不适合检测下游数据库慢、某个外部 API 超时、临时高负载。把这些放进 liveness，等于用重启处理外部抖动。

readiness probe 检查 Pod 是否应该被 Service 选为 endpoint。readiness 失败时，Pod 不一定重启，但会从负载均衡目标里摘掉。它适合表达“我现在不能接新请求”：比如启动后还在加载模型、还在 warm cache、还没连上必要依赖、正在 drain、队列积压过深、线程池已满。readiness 的失败会直接影响流量路由。

startup probe 用来处理慢启动。配置了 startup probe 后，liveness 和 readiness 在启动探针成功前不会干预容器。它适合 JVM 冷启动、模型加载、数据迁移、首次构建缓存这种启动时间可能很长的场景。没有 startup probe 时，很多人会把 liveness 的 initialDelaySeconds 配得很大，但这会让真正运行期故障也晚发现。startup probe 可以把“启动慢”和“运行期死掉”拆开。

probe 本身可以用 HTTP、TCP、exec、gRPC 等方式实现。HTTP probe 最常见，但要注意返回码语义。TCP probe 只能说明端口能连，不代表业务可用。exec probe 成本较高，命令卡住会拖慢节点。gRPC probe 适合 gRPC 服务，但服务端要实现健康检查语义。

结合 LogServe，`control` 的 readiness 可以检查是否能访问 `logd`、metadata store 是否初始化、是否已 bootstrap 必要状态；liveness 应该更窄，只检查 control 主进程是否进入不可恢复状态。`worker` 的 readiness 可以反映 executor pool、模型 cache warmup 或与 control 的注册状态；liveness 不应该因为某个 task 超时就杀整个 worker。`logd` 的 readiness 要关心数据目录可写和恢复完成，liveness 不要因为一次 fsync 抖动就触发重启风暴。

面试里可以这样回答：

```text
liveness 失败会让 kubelet 重启容器，适合检测不可恢复卡死；readiness 失败会把 Pod 从 Service endpoints 里摘掉，适合控制是否接流量；startup 用来保护慢启动，在启动成功前先不让 liveness/readiness 误伤容器。三者不能混用，尤其不要用 liveness 表达下游依赖慢。
```

## Q009. readiness 配置错误会造成什么流量风险？

**回答：**

readiness 直接影响 Service endpoint，所以它配置错了，表现出来就是流量错路由。错法大致有两类：太宽松和太严格。

太宽松时，Pod 还没准备好就开始接请求。常见场景是应用端口已经 listen，但内部还没完成初始化：数据库连接池没建好，缓存还没 warm，模型还在加载，schema migration 状态未知，worker 还没注册成功。TCP probe 在这种场景尤其容易误判，因为端口可连不代表业务能处理请求。结果是滚动发布期间新 Pod 提前进 endpoints，用户请求打到半初始化实例，出现 5xx、超时或错误结果。

太严格时，Pod 明明可以服务，却因为某个非关键依赖短暂失败而被摘流量。比如 readiness 检查了一个可降级的推荐服务、可重试的对象存储、某个偶发慢查询；依赖一抖，所有 Pod 同时 readiness 失败，Service endpoints 变少甚至变空，入口层开始返回 503。更糟的是，剩下的少数 ready Pod 承担更多流量，也可能被打到不 ready，形成级联摘除。

readiness 还有滚动发布风险。Deployment 更新时，Kubernetes 会根据新 Pod 是否 ready 来推进 rollout。如果 readiness 太早成功，旧 Pod 被删得太快，新版本实际还没能力接流量；如果 readiness 长时间失败，rollout 卡住，`maxUnavailable` 和 `maxSurge` 的配置会决定此时可用副本数和资源压力。`minReadySeconds` 可以要求 Pod 连续 ready 一段时间后才算可用，用来过滤刚启动后的短暂成功。

一个容易忽略的问题是 readiness 和依赖检查的范围。我的习惯是把 readiness 分成“本实例能否接新请求”和“关键依赖是否满足最低服务条件”。不是所有下游都应该进入 readiness。可以降级的功能应该由应用返回降级结果或局部错误，不要让整个 Pod 从 endpoints 消失。依赖健康检查还要有 timeout 和缓存，不能让 probe 自己把应用打挂。

结合 LogServe，worker readiness 如果绑定到“当前没有任何任务失败”，那就是错的。任务失败是业务结果，不代表 worker 不能接新任务。更合理的是检查 worker 是否注册到 control、executor pool 是否还能接收任务、必要模型或 checkpoint 是否达到声明状态。control readiness 如果每次都做重型 log replay 或全量 metadata 检查，也会把健康检查变成负载源。`logd` readiness 可以关注 append 路径是否可用，但不要因为某个非关键 metrics exporter 异常摘掉主服务。

面试里可以这样回答：

```text
readiness 太宽松会让未初始化实例提前接流量，滚动发布时容易 5xx；太严格会因为非关键依赖抖动把健康 Pod 摘掉，甚至 endpoints 变空造成 503。readiness 应该表达“这个实例现在能不能接新请求”，检查要轻量、有 timeout，并区分关键依赖和可降级依赖。
```

## Q010. liveness 配置过激会造成什么故障放大？

**回答：**

liveness 过激最常见的后果是重启风暴。一个实例本来只是短暂慢了、GC 停顿了、CPU 被 throttling 了、下游依赖卡住了，liveness probe 连续失败后 kubelet 把容器杀掉。容器重启会带来冷启动、连接重建、缓存丢失、队列重新均衡、更多下游请求。系统还没缓过来，更多 Pod 被 liveness 杀掉，故障就被放大了。

过激 liveness 的危险在于它把“慢”和“死”混在一起。服务 p99 升高时，健康检查请求也可能超时；但这时杀进程不一定有帮助。对 JVM、Go、Python 这类 runtime，短暂 STW、内存压力、线程池耗尽、GIL 下的阻塞都可能让 probe 慢。你如果把 timeout 设得很小、failureThreshold 设得很低，就等于让 kubelet 参与制造抖动。

另一个问题是 liveness 检查了外部依赖。数据库慢、对象存储慢、DNS 慢、认证服务慢，这些问题重启当前 Pod 通常解决不了。反而会让所有实例一起重启，打出连接风暴。正确做法是用 readiness 摘流量，用 circuit breaker、timeout、限流、降级保护业务，用告警和自动扩容处理容量问题。liveness 只处理“这个进程已经坏到不重启不行”。

还要注意 CrashLoopBackOff。liveness 杀掉容器后，如果容器启动又很慢、startup probe 没配、依赖仍然不可用，Pod 会反复重启。Kubernetes 会退避重启间隔，但服务容量已经下降，滚动发布或自动扩容也会被拖慢。很多线上事故的根因不是应用第一次慢，而是过激健康检查把慢实例变成了不可用实例。

设计 liveness 时，我会问几个问题：这个失败能靠重启恢复吗？probe 是否只检查本进程内部状态？timeout 是否覆盖正常 GC 和负载抖动？是否有 startup probe 保护慢启动？readiness 是否已经能把不适合接流量的实例摘掉？是否能从 metrics 看到 liveness restart 的原因，而不是只看到容器重启次数？

结合 LogServe，`worker` 执行某个 Python task 卡住，不应该立刻由 liveness 杀掉整个 worker，除非 worker 主循环或 executor 管理线程失控。任务级超时、lease 过期、redelivery、幂等才是主要恢复机制。`control` 如果因为 `logd` 短暂慢而 liveness 失败，重启 control 只会增加 bootstrap 压力。`logd` 如果因为一次磁盘 fsync 慢就被杀，append-only log 的恢复压力会更大。

面试里可以这样回答：

```text
过激 liveness 会把慢实例杀成不可用实例，引发冷启动、缓存丢失、连接风暴和 CrashLoopBackOff。liveness 只应该检测重启能修复的进程级故障，不要检查外部依赖或普通业务延迟。慢启动用 startup probe，不能接流量用 readiness，容量和依赖问题用限流、降级、扩容和告警处理。
```
## Q011. resource request 和 limit 的区别是什么？

**回答：**

request 和 limit 是 Kubernetes 资源模型里最容易混淆的一对。request 主要影响调度和资源预留，limit 主要影响运行时上限。简单说：request 是“我至少需要多少，调度器按这个给我找位置”；limit 是“我最多能用多少，超过会被限制或杀掉”。

CPU request 会影响 Pod 放到哪台 Node。scheduler 会看 Node 上已经被请求的 CPU 总量，再判断新 Pod 是否能放下。这里算的是 request，不是当前真实 CPU 使用率。一个 Pod CPU request 写得太大，可能明明集群还有很多空闲 CPU，也调度不上去；写得太小，调度器会把过多 Pod 塞到同一台机器，运行时互相抢。

CPU limit 是运行时上限。容器超过 limit 时通常不会被杀，而是被 cgroup CFS quota throttling，表现为请求变慢、p99 抖动、goroutine 或线程排队。CPU 是可压缩资源，超过上限的后果是等，不是立刻死亡。

memory request 也影响调度。scheduler 会按 request 估算节点是否有足够内存。memory limit 是硬上限，超过后容器内进程可能被 OOM kill。内存不是可压缩资源，不能像 CPU 一样慢慢等。写太低会频繁 OOM，写太高会降低集群装箱率，还可能让节点内存压力时 eviction 行为变复杂。

request/limit 还决定 QoS class。所有容器 CPU 和 memory request 等于 limit 时，Pod 是 Guaranteed；有 request 但不完全相等时是 Burstable；没有 request 和 limit 时是 BestEffort。节点内存压力下，BestEffort 和低优先级 Burstable 更容易被驱逐。QoS 不会让应用天然可靠，但它会影响节点压力时谁先被牺牲。

工程上，我不建议为了“防止超用”无脑给所有服务设置很紧的 CPU limit。对延迟敏感服务，紧 CPU limit 很容易制造 throttling。更好的做法是给合理 CPU request，配合 HPA、节点容量规划和监控；CPU limit 是否设置要看隔离要求。内存 limit 通常更需要设置，因为没有上限的内存泄漏会拖垮节点，但它必须基于实际峰值、GC 行为、cache 策略和数据大小估算。

结合 LogServe，`control` 的 request 应该覆盖队列调度、metadata view 和 gRPC 请求处理；`worker` 的 request 要考虑 Python executor pool、LLM mock/vLLM 调用、checkpoint cache；`logd` 要考虑 append/read/recovery 和 page cache 的关系。当前 manifests 没有写 resources，作为示例能跑，但生产化时不能缺 request/limit，否则 scheduler 和 eviction 都缺少明确依据。

面试里可以这样回答：

```text
request 用于调度和资源预留，limit 用于运行时限制。CPU 超过 limit 通常被 throttling，表现为延迟变高；memory 超过 limit 会触发 OOM kill。request/limit 还影响 Pod QoS。生产里要基于负载画像设置 request，内存 limit 要谨慎估峰值，CPU limit 对延迟敏感服务可能带来 p99 抖动。
```

## Q012. CPU limit 会如何导致 throttling？

**回答：**

CPU limit 在 Linux cgroup 里通常落到 CFS quota。它不是说容器独占某些 CPU core，而是在一个调度周期内给容器一定 CPU 时间。比如 limit 是 `500m`，可以理解为平均最多半个 CPU；如果 CFS period 是 100ms，那么这个 cgroup 在一个 period 里大约只能用 50ms CPU 时间。用完后，即使机器上还有空闲 CPU，也可能被 throttling 到下个 period。

throttling 的表现不是进程崩溃，而是“明明没有错误，但请求变慢”。服务处理一个请求需要 CPU burst 时，短时间超过 quota，就会被挂起等待下个 period。对 Go、Java、Python 都可能影响明显。Go runtime 可能看到 `GOMAXPROCS` 和实际可用 CPU 时间不匹配；Java GC 或 JIT 在 quota 下变慢；Python 多进程 worker 被限制后队列等待变长。应用层看到的是 p95/p99 升高、timeout 增加、HPA 反应滞后。

CPU throttling 也会误导 autoscaling。如果 HPA 按 CPU utilization 扩容，而容器被 limit 限住，使用率可能长期接近 100%，但每个 Pod 已经被限速。扩容能缓解总吞吐，但如果 request/limit 配置不合理，新的 Pod 仍然被同样 quota 卡住。另一种情况是 CPU request 太低，HPA 的 utilization 以 request 为分母，轻微真实负载就显示很高，导致过早扩容。

排查时要看几个信号。容器级 metrics 里通常有 `container_cpu_cfs_throttled_periods_total`、`container_cpu_cfs_throttled_seconds_total` 这类指标。还要对比 CPU usage、request、limit、应用延迟、GC pause、队列等待。只看 `kubectl top pod` 不够，因为 top 展示当前使用量，不一定展示被 throttle 的等待时间。

工程处理有几种选择。对延迟敏感的在线服务，可以只设置 CPU request，不设置 CPU limit，依靠节点容量、HPA 和优先级控制竞争；对批处理、多租户或不可信 workload，可以保留 limit，但要按真实 CPU burst 设置，避免把 limit 贴着平均值。还可以减少单请求 CPU burst、降低并发、增加 worker pool 背压，让服务不要在一个 period 内打满 quota。

结合 LogServe，`control` 的调度路径如果被 CPU throttling，会表现为 worker poll 延迟、task lease 分配慢、workflow step ready 到 start 的时间变长。`worker` 被 throttle 会导致 Python executor 启动慢、task complete 事件晚写、LLM mock first-token 延迟变高。此时重启 Pod 没有意义，应该先看 CPU quota、request/limit、队列等待和 throttling metrics。

面试里可以这样回答：

```text
CPU limit 会被 cgroup CFS quota 实现。容器在一个调度周期内用完 quota 后会被 throttling，通常不崩溃，但延迟会上升。它最容易影响 p99、GC、线程池和队列等待。排查时看 cfs throttled periods/seconds、CPU usage、request/limit 和应用延迟。延迟敏感服务不一定要设置很紧的 CPU limit。
```

## Q013. memory limit 超过后会发生什么？

**回答：**

memory limit 和 CPU limit 的后果完全不同。CPU 超了可以等，内存超了通常要杀。容器进程的内存使用超过 cgroup 限制后，内核会在这个 cgroup 内触发 OOM，选择一个进程杀掉。Kubernetes 里常见表现是容器终止原因 `OOMKilled`，退出码通常是 137，也就是被 SIGKILL 杀掉。

内存限制包括的不只是你在代码里显式申请的 heap。RSS、匿名内存、部分 page cache、tmpfs `emptyDir`、mmap、线程栈、运行时元数据都可能计入 cgroup。很多人看到应用 heap 没到 limit，就以为不会 OOM，这是错的。比如把大文件写到 memory-backed `emptyDir`，或者在 `/dev/shm` 里放大对象，也可能把容器打到 OOM。

超过 memory limit 时，Kubernetes 不会像 CPU 一样“慢下来”。容器被杀后是否重启，取决于 Pod restart policy 和控制器。Deployment 管理的 Pod 通常会重启容器或重建 Pod。应用如果没有持久化状态和幂等语义，重启就可能丢内存队列、丢本地缓存、重复执行请求。

还有一种相关但不同的情况：节点内存压力 eviction。节点整体内存不足时，kubelet 可能驱逐 Pod。此时 Pod 状态可能是 Evicted，而不是单个容器 OOMKilled。驱逐顺序会考虑 QoS、Priority、资源使用超过 request 的程度。BestEffort 和超 request 很多的 Burstable 更危险。排查时要分清是 cgroup limit OOM，还是 node pressure eviction。

设置 memory limit 的难点是它既要保护节点，又不能贴着平均值。应用内存有峰值：启动加载、请求 fan-out、反序列化、大结果聚合、GC 周期、缓存 warmup、compaction/backfill 都会抬高内存。limit 贴着平稳状态，发布或流量尖峰时就会 OOM。反过来，limit 太高而 request 太低，会导致 scheduler 过度装箱，节点压力时更容易集体抖动。

结合 LogServe，容易吃内存的地方包括 workflow payload 解析、大 result 缓冲、actor snapshot、control materialized view、log replay、worker-local model/checkpoint cache、Python executor 进程。worker 的缓存尤其要设上限和淘汰策略，不能只依赖 Kubernetes memory limit 当最后刹车。因为 OOM kill 发生时，任务可能已经执行一半，仍要靠 task lease、attempt、idempotency key 和 replay 恢复语义兜住。

面试里可以这样回答：

```text
memory limit 是硬约束。容器超过 cgroup 内存限制后会触发 OOM kill，Kubernetes 通常显示 OOMKilled，退出码常见 137。内存不仅包括应用 heap，还可能包括 RSS、tmpfs emptyDir、mmap、线程栈和部分 page cache。还要区分容器 OOM 和节点内存压力驱逐。内存 limit 要基于峰值和缓存策略设置，不能贴着平均值。
```

## Q014. OOMKilled 如何排查？

**回答：**

排查 OOMKilled 先不要急着加内存。第一步是确认到底发生了什么：看 Pod 状态、容器 last state、exit code、reason、restart count 和 Events。`OOMKilled` 通常说明容器内 cgroup OOM；如果是 `Evicted`，就要看节点 memory pressure、ephemeral storage pressure 或 kubelet eviction 事件。这两条路径的处理不一样。

第二步看时间线。OOM 是在启动时、流量高峰、发布后、某个 batch/backfill、某个大请求、GC 之后，还是运行很久后慢慢涨上去。启动时 OOM 可能是模型、索引、配置或缓存一次性加载；流量高峰 OOM 可能是并发乘以单请求内存；发布后 OOM 可能是新版本引入了额外缓存或对象复制；慢慢涨通常要怀疑泄漏、未清理 map、goroutine/thread 泄漏、对象池失控。

第三步拆内存来源。应用 heap 只是其中一块。Go 服务要看 heap、stack、goroutine、GC、`pprof`；Java 看 heap、metaspace、direct memory、thread stack；Python 看进程 RSS、子进程、native extension、对象引用；容器还要看 tmpfs、page cache、mmap 文件、日志缓冲、本地缓存。Kubernetes metrics 里的 working set 可以作为入口，但不能替代 runtime profile。

第四步看资源配置。memory request 是否太低导致节点上同类 Pod 过度集中？limit 是否贴着平时峰值？Pod QoS 是 Guaranteed、Burstable 还是 BestEffort？同节点上是否有其他 Pod 抢内存？`emptyDir` 是否使用 memory medium？是否有 `LimitRange` 给了默认 limit，应用作者自己并不知道？

第五步看恢复语义。OOM 不是只影响一个进程，它会打断业务流程。请求是否幂等？任务是否会 redeliver？部分写入能否恢复？本地缓存丢失后是否会引发冷启动风暴？如果服务重启后需要从头 replay 很多日志，OOM 可能变成连续重启。排查时要同时处理根因和重启后的恢复压力。

结合 LogServe，我会按模块查。`control` OOM：看 task/workflow/actor/model materialized view 是否无限增长，bootstrap/replay 是否一次性读太多 log record，metadata clone 是否放大内存。`worker` OOM：看 executor pool 并发、Python 子进程 RSS、模型 checkpoint cache、result 缓冲、LLM 输出大小。`logd` OOM：看读路径是否把 segment 或 stream 全量加载，benchmark payload 是否异常。修复上优先做边界：限制 payload/result/snapshot 大小，流式读取，cache LRU，executor 并发上限，profile 后再调 request/limit。

面试里可以这样回答：

```text
先确认是容器 OOMKilled 还是节点 eviction，再看 OOM 时间线、应用 profile、容器 working set、tmpfs/page cache/mmap、资源 request/limit 和 QoS。不要只加内存，要找是启动峰值、并发放大、缓存无界、泄漏还是配置过低。还要检查 OOM 后任务、请求和本地缓存的恢复语义，避免重启后继续放大故障。
```

## Q015. HPA 根据 CPU 扩容有什么局限？

**回答：**

HPA 按 CPU 扩容看起来简单，但它有几个天然局限。第一个是指标分母问题。CPU utilization 通常按当前 CPU usage 除以 CPU request 计算。如果容器没有设置 CPU request，HPA 很难计算目标利用率；如果 request 写得太小，利用率会虚高，轻微负载就扩容；如果 request 写得太大，利用率会偏低，扩容太慢。

第二个局限是 CPU 不一定代表真实瓶颈。很多服务瓶颈在队列长度、下游数据库、锁竞争、磁盘 I/O、网络等待、对象存储、外部 API、GPU、模型加载或单请求内存。CPU 低不代表服务健康。比如 LogServe worker 在等对象存储或模型 checkpoint，CPU 可能很低，但请求已经排队；control 卡在 metadata lock 或 logd append p99，CPU 也不一定很高。

第三个局限是滞后。HPA 依赖 metrics pipeline，采样、聚合、决策、创建 Pod、镜像拉取、启动、readiness 成功都需要时间。突发流量到来时，CPU 先升高，HPA 后扩容，新 Pod 再冷启动。对短尖峰或队列型任务，等 CPU 指标触发时，backlog 可能已经堆起来了。

第四个局限是 CPU limit 和 throttling 会干扰判断。Pod 被 CPU limit throttling 时，CPU usage 可能被压在 limit 附近，延迟已经很差，但 HPA 只看到平均 CPU。扩容可能有帮助，也可能掩盖单 Pod limit 太紧的问题。相反，如果 Pod 因 I/O 等待导致 CPU 不高，HPA 不会扩容，但用户已经超时。

第五个局限是平均值掩盖不均衡。HPA 通常看 Deployment 维度的平均指标。某些 Pod 热点、某些 worker 缓存 miss、某些分片 backlog 很高，平均 CPU 可能仍在目标值附近。按平均 CPU 扩容无法解决分片倾斜和 cache locality 问题。

更稳的做法是把 CPU HPA 当基础保护，再引入业务指标。在线服务看 RPS、in-flight、队列等待、p95/p99、错误率；队列消费者看 backlog per replica、oldest message age、processing duration；LLM serving 看 GPU utilization、KV cache、batch queue、cold start；control plane 看 scheduler queue depth 和 lease 分配延迟。扩容指标要和用户痛点更接近。

面试里可以这样回答：

```text
CPU HPA 依赖 request，request 配错会误判；CPU 也不一定是瓶颈，I/O、锁、队列、下游、GPU 和冷启动都可能让服务慢但 CPU 不高。HPA 还有 metrics 和 Pod 启动滞后，平均 CPU 会掩盖热点。生产里通常把 CPU 作为基础指标，再配合队列长度、in-flight、延迟、错误率或自定义业务指标。
```
## Q016. 基于队列长度扩容需要注意什么？

**回答：**

基于队列长度扩容比基于 CPU 更接近很多异步系统的真实压力，但它也不能只看一个 raw backlog。队列长度本身没有单位感，1000 条消息可能是轻任务，也可能是 1000 个大模型推理。更好的指标是 backlog per replica、oldest message age、入队速率、处理速率、单任务耗时和目标延迟。

第一，要把队列长度和处理能力联系起来。一个消费者每秒处理 10 条消息，10 个消费者每秒 100 条。当前 backlog 10000，按 100 条每秒清完要 100 秒。如果 SLO 要 30 秒内处理完，就需要更多消费者，或者减少单任务耗时。这个思路比“队列超过 1000 就扩容”靠谱。

第二，要考虑消息耗时分布。队列里如果混着短任务和长任务，平均长度会误导扩容。长任务占住 worker 后，短任务可能排在后面。需要按任务类型分队列、分优先级，或者扩容指标区分 task class。LogServe 里普通 task、actor task、LLM task 的耗时和资源都不同，把它们放在一个队列长度指标里会丢信息。

第三，要考虑 redelivery 和 visibility timeout。消费者扩容太快，如果下游变慢，任务超时后被重复投递，队列长度可能越扩越大。这个时候瓶颈不是消费者不够，而是下游数据库、对象存储、模型服务或锁竞争。盲目扩容会把下游打穿。队列扩容要配合最大并发、下游限流、幂等和退避。

第四，要控制扩容速度和缩容速度。扩容太慢会让 backlog 长时间堆积；扩容太快会造成镜像拉取风暴、冷启动风暴和下游连接风暴。缩容太快会杀掉正在处理长任务的消费者，触发重试。常见策略是扩容更积极，缩容更保守，并使用稳定窗口、cooldown、graceful termination。

第五，要处理 scale-to-zero 的冷启动。事件驱动系统可以把副本缩到 0，但第一个请求会付出镜像拉取、Pod 创建、readiness、缓存 warmup 的成本。对低延迟任务，最好保留最小副本数，或者预热关键缓存。

结合 LogServe，如果按队列扩容 worker，不应该只看全局 pending task 数。要分别看普通 task backlog、actor mailbox backlog、LLM backlog、每类任务平均耗时、worker executor pool 利用率、模型 cache hit/cold start、control poll 延迟。LLM 任务还要考虑模型 locality：扩了一个没有模型缓存的 worker，不一定立刻提升吞吐，可能先制造 checkpoint fetch。

面试里可以这样回答：

```text
队列扩容要看 backlog per replica、oldest message age、入队/出队速率和任务耗时，而不是只看 raw queue length。还要处理 visibility timeout、redelivery、幂等、下游容量、扩缩容冷却和长任务 graceful shutdown。不同任务类型最好分指标，避免短任务、长任务和冷启动混在一个队列长度里。
```

## Q017. 滚动发布如何避免不兼容版本同时存在？

**回答：**

滚动发布的本质是让新旧版本在一段时间内同时存在。Deployment 的 `maxSurge`、`maxUnavailable` 可以控制替换速度，但它不理解你的协议、数据库 schema、消息格式和缓存内容。所以避免不兼容版本共存，主要靠应用兼容设计，不靠 Kubernetes 自动兜底。

第一层是接口兼容。新版本服务端要能处理旧客户端请求，旧版本服务端最好能忽略新客户端的可选字段。HTTP/gRPC API 要避免直接删除字段、改变字段语义、把可选字段改成必填。protobuf 这类 schema 要保留 field number，不复用已删除字段编号。JSON 要能容忍未知字段。

第二层是数据兼容。数据库 schema 变化要走 expand-contract。先加 nullable 列或新表，让旧代码不受影响；新代码双写或兼容读；完成 backfill；确认所有旧版本下线；最后再收紧约束、删除旧列。不能先改成新 schema 再滚动应用，否则旧 Pod 还在运行时就可能读写失败。

第三层是消息兼容。队列里的消息可能被旧版本写入、新版本消费，也可能反过来。消息 schema 要版本化，消费者要兼容旧消息，生产者不能突然发旧消费者不能解析的格式。对 LogServe 这种 shared log 系统尤其重要：log record 一旦写入，就会被 replay。新字段要可选，旧 replay 逻辑要能跳过未知字段，新 replay 逻辑要能读旧记录。

第四层是行为兼容。比如新版本改变了任务幂等 key、leader election 逻辑、cache key、锁名、外部对象路径，即使 API 字段没变，也可能和旧版本互相踩。滚动发布前要列出 shared state：数据库、队列、对象存储、缓存、锁、lease、日志格式、metrics label。共享状态是最容易被忽略的兼容面。

第五层是发布控制。如果确实不能新旧共存，就不要用普通滚动发布硬上。可以用蓝绿发布，先把新环境全量拉起但不接生产流量；也可以用 canary，把很小一部分流量切给新版本；更复杂的场景用 feature flag 控制写新格式的时间点。Deployment strategy 只是机械替换 Pod，不是兼容性策略。

结合 LogServe，我会特别关注 log-first 语义。`TaskSubmitted/Started/Completed`、workflow/actor/LLM 事件格式变化时，要先让所有读路径支持新旧格式，再放开写新格式。actor `command_seq`、worker epoch、idempotency key、result ref 这些字段不能随便改语义。否则滚动期间 control、worker、dashboard、replay 工具看到的状态会不一致。

面试里可以这样回答：

```text
滚动发布天然会让新旧版本共存。避免不兼容要靠应用协议、数据库 schema、消息格式和共享状态的前后兼容，而不是只靠 maxSurge/maxUnavailable。常用方法是 expand-contract、可选字段、双读双写、feature flag、先升级读者再升级写者。不能共存的变更应改用蓝绿、金丝雀或停机迁移。
```

## Q018. 蓝绿发布和金丝雀发布有什么区别？

**回答：**

蓝绿发布和金丝雀发布都在降低发布风险，但它们控制风险的方式不同。蓝绿发布是两套环境切换，金丝雀发布是小流量逐步放量。

蓝绿发布通常有 blue 和 green 两套相对完整的环境。当前生产流量在 blue，新版本部署到 green。green 预热、检查、跑 smoke test 后，一次性把流量切过去。如果出问题，再把流量切回 blue。它的优点是切换和回滚快，环境边界清楚；缺点是资源成本高，数据库和外部状态不容易复制。只要 blue 和 green 共享同一个数据库，schema 不兼容仍然会影响回滚。

金丝雀发布是让一小部分流量先进入新版本，比如 1%、5%、10%、50%、100%。过程中观察错误率、延迟、资源、业务指标，再决定是否继续放量或回滚。它的优点是风险暴露范围小，适合真实生产流量验证；缺点是需要流量切分能力、指标判断和自动/人工决策。普通 Kubernetes Deployment 只能滚动替换 Pod，不等同于按用户、比例或 header 做 canary。

流量控制能力通常来自 Ingress controller、Service Mesh、Gateway API、云负载均衡、Argo Rollouts、Flagger 之类的发布控制器。它们能按比例、header、cookie、用户分组、region 或服务版本切流。没有流量层支持时，所谓 canary 可能只是“少起几个新 Pod”，但 kube-proxy 或 Service 默认负载均衡不一定按你想要的比例分配真实业务流量。

两者都不能自动解决状态兼容。蓝绿如果共享数据库，green 写了新字段，blue 回滚后可能读不懂。金丝雀如果新版本写入新消息，旧消费者可能失败。发布方式降低的是流量暴露风险，不是数据兼容风险。schema、消息和共享状态仍要先设计。

结合 LogServe，如果只是更新 stateless worker 逻辑，可以用金丝雀先让少量 worker 接一小部分任务，观察 task failure、lease timeout、executor latency、cache miss。如果更新 `logd` 日志格式或 control 的 replay 语义，金丝雀要更谨慎，因为 shared log 是全局状态，一旦新版本写入不兼容记录，旧版本就算没有接流量也可能被影响。此时先做兼容读写，再谈发布策略。

面试里可以这样回答：

```text
蓝绿发布是准备两套环境，把流量从旧环境一次性切到新环境，回滚就是切回去；金丝雀发布是把少量真实流量逐步导入新版本，边观察边放量。蓝绿切换快但资源成本高，金丝雀风险暴露小但依赖流量治理和指标判断。两者都不替代 schema 和消息兼容设计。
```

## Q019. Pod graceful termination 如何设计？

**回答：**

Pod graceful termination 的目标不是“等一等再杀”，而是让实例从接收流量、处理请求、写状态、释放资源这几个动作里有序退出。设计不好时，滚动发布、节点 drain、缩容都会变成请求中断、重复执行和状态损坏。

典型流程是：Pod 被删除后，Kubernetes 给它设置 deletionTimestamp，EndpointSlice/Service 会逐步把它从可用 endpoints 中移除；kubelet 开始终止容器，执行 preStop hook，然后向容器发送 SIGTERM；应用在 termination grace period 内退出；超时后 kubelet 发送 SIGKILL。实际流量停止不是瞬间完成，负载均衡器、Ingress、连接复用、客户端重试都可能有延迟。

应用要主动配合。第一步，收到 SIGTERM 后立即停止接收新请求，或者让 readiness 变 false。第二步，继续处理已经开始的请求，给它们一个小于 terminationGracePeriodSeconds 的 deadline。第三步，停止从队列拉取新任务，释放 lease 或把未开始任务归还。第四步，flush 日志、metrics、trace 和关键状态。第五步，关闭连接池和后台 goroutine。最后退出进程。

对 HTTP/gRPC 服务，还要处理长连接。HTTP keep-alive、gRPC streaming、worker poll 长连接都可能让旧 Pod 在被摘 endpoint 后继续收到请求。服务端需要 drain：停止接受新 stream，允许旧 stream 在 deadline 内完成，必要时返回明确错误让客户端重试。客户端也要有 retry 和 deadline，不能无限等待一个正在终止的实例。

对队列消费者，graceful termination 更关键。收到 SIGTERM 后不要再拉新消息；正在处理的消息如果能在剩余时间完成，就完成并 ack；如果不能，就尽快释放或让 visibility timeout 到期，但要保证任务幂等。硬杀会让消息 redelivery，业务必须能处理重复执行。

结合 LogServe，worker 终止时应该停止 poll 新任务，等待本地 executor pool 在 deadline 内完成；未开始任务可以不 lease 或让 lease 过期；已开始任务的完成事件要带 attempt/worker/epoch，control 侧要拒绝过期完成。control 终止时要停止接收新 submit/poll，flush 已写 log 后的 metadata 更新，重启后从 shared log bootstrap。logd 终止时要停止新 append，flush segment，保证恢复路径能识别最后一条完整 record。

面试里可以这样回答：

```text
graceful termination 要先从流量摘除，再停止接新请求，处理或中止已有请求，释放 lease，flush 状态和日志，最后退出。SIGTERM 不是业务协议，应用必须自己处理 drain。队列消费者要停止拉新任务，正在处理的任务要么完成 ack，要么依赖 visibility timeout 和幂等重试。terminationGracePeriodSeconds 要覆盖真实清理时间。
```

## Q020. preStop hook 和 terminationGracePeriodSeconds 有什么作用？

**回答：**

preStop hook 是容器终止前由 kubelet 调用的钩子，terminationGracePeriodSeconds 是 Pod 从开始终止到被强制 SIGKILL 之间的宽限时间。它们经常一起出现，但职责不同。

preStop 可以是 exec、HTTP 或 sleep 这类动作。常见用途是通知应用开始 drain、从外部注册中心摘除、给负载均衡传播留一点时间、触发本地清理脚本。需要注意，preStop 运行在终止宽限时间内，它不是额外赠送的一段无限时间。hook 太慢会吃掉应用处理 SIGTERM 的时间。hook 失败也不应该成为唯一退出逻辑，因为容器最终仍会被终止。

terminationGracePeriodSeconds 是总预算。Pod 删除开始后，kubelet 按这个时间等待容器退出。时间到了，仍未退出的进程会被 SIGKILL。SIGKILL 不能被应用捕获，所以未 flush 的日志、未写完的状态、未 ack 的任务都可能丢在半路。这个值不能随便设成默认 30 秒就完事，也不能为了“安全”设成很大。太短会打断业务，太长会拖慢发布和节点 drain。

一个常见但有争议的写法是 preStop `sleep 10`。它的意图是等 endpoints 变化传播，让入口层不再发新请求。这个方法有时有效，但它只是经验性缓冲，不是严格保证。更好的做法是应用收到终止信号后主动让 readiness false、停止接新请求，并在负载均衡层配置合理的连接 draining。sleep 可以作为补充，但不要把它当唯一机制。

设计这两个参数时，要按业务最长安全退出时间来估算。HTTP API 可能只需要几秒到十几秒；长任务 worker 可能需要更复杂的 checkpoint 或任务释放，不能简单把 grace period 设成一小时。长任务如果真的可能跑很久，应该做可中断、可重试、可 checkpoint，而不是靠 Pod 删除时硬等。

结合 LogServe，worker 的 preStop 可以调用本地管理接口让 worker 进入 draining：停止 poll，标记 readiness false，等待 executor pool 完成短任务。terminationGracePeriodSeconds 要小于 task lease 设计，或者至少和 lease timeout、retry 语义协调。control 的 preStop 可以停止接收新任务并 flush 关键状态。logd 的 terminationGracePeriodSeconds 要给 fsync/segment close 足够时间，但最终恢复仍要靠 append-only record 边界，而不是靠永远不被杀。

面试里可以这样回答：

```text
preStop 是终止前执行的钩子，可用于 drain、摘注册、短暂等待负载均衡传播；terminationGracePeriodSeconds 是整个优雅退出的时间预算，超时后 kubelet 会 SIGKILL。preStop 会消耗这个预算，不能写得太慢。真正可靠的做法是应用收到终止后停止接新请求、处理已有请求、释放任务和 flush 状态，而不是只靠 sleep。
```
## Q021. Service、Ingress、Gateway API 的职责是什么？

**回答：**

Service、Ingress、Gateway API 都和流量入口有关，但层次不同。Service 解决集群内部稳定访问和 Pod 负载均衡，Ingress 解决较传统的 HTTP/HTTPS 入口路由，Gateway API 则提供更细的角色分离和更丰富的流量模型。

Service 给一组 Pod 一个稳定抽象。Pod 会变，IP 会变，Service 的 DNS 名和 ClusterIP 相对稳定。Service 通过 selector 选择 Pod，再通过 Endpoints/EndpointSlice 把流量送到 ready endpoints。ClusterIP 适合集群内部访问，NodePort 暴露节点端口，LoadBalancer 通常让云厂商创建外部负载均衡。Service 是 Kubernetes 网络的基础抽象，但它主要是 L4 负载均衡，不负责复杂 HTTP 路由。

Ingress 是 HTTP/HTTPS 路由规则。它可以按 host、path 把外部请求转到不同 Service，也可以配置 TLS 终止。Ingress 本身只是 API 对象，需要 Ingress Controller 才会真正生效。不同 controller 支持的 annotation 差异很大，这也是 Ingress 长期的问题：核心 API 简单，很多能力靠厂商特定 annotation 扩展。

Gateway API 是更现代的流量 API。它把基础设施角色和应用角色拆开：GatewayClass 表达某类网关实现，Gateway 表达一个实际入口，HTTPRoute/TCPRoute/TLSRoute 等表达路由规则。它比 Ingress 更适合多团队、多 namespace、跨服务共享网关的场景，也更容易表达 listener、route binding、traffic policy 这类能力。Gateway API 不是 Service 的替代品，它通常仍然把流量转到 Service 或后端 endpoints。

一个常见误区是把 Service 当作“服务发现 + 健康检查 + L7 网关 + 灰度系统”的合集。Service 只知道 endpoints 和端口，不理解业务版本、用户分组、header、认证、熔断、重试预算。Ingress/Gateway/Service Mesh 才更接近 L7 流量治理，但也要看具体实现。

结合 LogServe，`logserve-control` 和 `logserve-logd` 当前用普通 Service 暴露集群内 gRPC 端口，这适合 worker 访问 control/logd。是否需要 Ingress 或 Gateway，要看是否要把 SDK API 暴露到集群外、是否需要 TLS、认证、路径路由或多租户入口。`logd` 这类内部存储服务一般不应该直接暴露到公网；control API 即使暴露，也要先处理认证、授权、限流和审计。

面试里可以这样回答：

```text
Service 给一组 Pod 提供稳定 DNS/IP 和 L4 负载均衡；Ingress 定义 HTTP/HTTPS host/path 路由，需要 Ingress Controller 实现；Gateway API 用 GatewayClass、Gateway、Route 把基础设施入口和应用路由拆开，能力比 Ingress 更细。Service 是基础访问抽象，复杂 L7 流量治理通常在 Ingress/Gateway/Service Mesh 层做。
```

## Q022. Headless Service 适合什么场景？

**回答：**

Headless Service 是 `clusterIP: None` 的 Service。它不分配普通 ClusterIP，也不通过 kube-proxy 做一个统一虚拟 IP。DNS 查询时通常直接返回后端 Pod 或 endpoints 的地址。它适合客户端需要知道每个后端实例身份的场景。

最典型场景是 StatefulSet。StatefulSet 配合 Headless Service 可以给每个 Pod 稳定 DNS，比如：

```text
web-0.web.default.svc.cluster.local
web-1.web.default.svc.cluster.local
```

这种稳定名字对数据库、quorum 系统、分片系统很有用。成员之间可以知道“我是 0 号副本”“我要连 1 号副本”，而不是只连一个随机负载均衡地址。

Headless Service 也适合客户端自己做负载均衡或连接管理的系统。比如某些数据库 driver、gRPC client-side load balancing、服务发现客户端、需要感知所有 endpoints 的代理。客户端拿到多个 A/AAAA 记录后，可以按自己的策略连接、重试、健康检查。

它不适合所有服务。普通 stateless HTTP API 通常用 ClusterIP Service 就够了。使用 Headless Service 后，你把更多责任交给客户端：连接哪个 endpoint、endpoint 失效怎么处理、DNS TTL 怎么处理、是否缓存旧地址、是否均衡流量。客户端写不好，可能出现长时间连到已删除 Pod、流量倾斜、重试风暴。

结合 LogServe，如果 `logd` 只是单实例内部服务，普通 Service 更简单。若以后把 logd 设计成多副本 quorum 或分片日志服务，Headless Service + StatefulSet 才有价值，因为成员身份和稳定网络名会成为协议的一部分。worker 一般不需要 Headless Service，control 应该通过调度和注册表知道活跃 worker，而不是让其他组件直接依赖每个 worker Pod DNS。

面试里可以这样回答：

```text
Headless Service 不分配 ClusterIP，DNS 直接暴露后端 endpoints。它适合 StatefulSet、数据库、quorum、分片和客户端自带负载均衡的场景，尤其需要每个 Pod 稳定 DNS 身份时。普通无状态服务通常用 ClusterIP Service 更简单；Headless 会把连接选择和失败处理责任交给客户端。
```

## Q023. 持久化服务为什么需要 PersistentVolume？

**回答：**

Pod 的生命周期是短的，容器可写层也是短的。Pod 被删除、重建、调度到另一台节点后，本地容器层里的数据通常就没了。PersistentVolume 和 PersistentVolumeClaim 解决的是存储生命周期和 Pod 生命周期解耦的问题。

PersistentVolume 是集群里的存储资源，可能来自本地盘、云盘、NFS、Ceph、CSI driver 等。PersistentVolumeClaim 是应用对存储的请求：需要多大容量、什么访问模式、什么 StorageClass。Pod 通过 PVC 挂载存储。这样 Pod 重建后，只要 PVC 还在，数据卷可以重新挂回新 Pod。

持久化服务需要 PV，不只是为了“数据不丢”。它还提供调度约束和运维边界。比如云盘通常只能挂到一个可用区里的一个节点，Kubernetes 要知道 PVC 和节点拓扑，才能把 Pod 调到合适位置。StorageClass 负责动态创建卷，reclaim policy 影响 PVC 删除后底层卷保留还是删除。备份、快照、扩容、访问模式也都围绕 PV/PVC 管理。

没有 PV 的有状态服务会有几个问题。数据库重启后数据丢失；append-only log Pod 迁移后看不到旧 segment；对象存储本地目录丢失；缓存可以丢，但如果你把缓存当事实来源，就会出事故。临时数据、可重建缓存可以放 emptyDir；事实来源、不可重算结果、日志、数据库文件要放持久卷或外部存储。

PV 也不是魔法。它不能替代复制、备份和一致性协议。单块 PVC 挂给单副本数据库，Pod 重建时数据还在，但节点、磁盘、云盘、文件系统仍可能故障。多副本数据库还要考虑写入一致性、leader、fencing、快照、恢复演练。PV 提供持久介质，不提供数据库语义。

结合 LogServe，`logd` 保存 append-only shared log，所以当前 k8s 示例给它挂了 `logserve-logstore` PVC，这是正确方向。worker 的 model/checkpoint cache 如果只是性能缓存，可以丢；如果缓存里有唯一副本，就必须改成 PV 或对象存储。result store、actor snapshot、workflow 大结果更适合本地/S3-compatible object store 边界，而不是塞进 Pod 可写层。

面试里可以这样回答：

```text
Pod 和容器可写层是短生命周期的，持久化服务需要 PV/PVC 把数据生命周期从 Pod 生命周期里拆出来。PVC 让 Pod 重建或迁移后还能重新挂载数据，也让调度器理解存储拓扑和访问模式。但 PV 只提供持久介质，不替代复制、备份、快照和一致性协议。
```

## Q024. StatefulSet 的稳定网络标识有什么价值？

**回答：**

StatefulSet 的稳定网络标识让副本不只是“几个可替换 Pod”，而是有固定身份的成员。`app-0`、`app-1`、`app-2` 这些 ordinal 会伴随 Pod 重建保留，配合 Headless Service 可以得到稳定 DNS。对很多有状态系统来说，身份就是协议的一部分。

数据库和 quorum 系统需要稳定身份。比如成员 0、1、2 各自有日志、投票权、数据目录和复制位置。如果 Pod 每次重建都换一个随机名字，成员管理会很麻烦。稳定 DNS 让其他成员能用固定名字连接它，稳定 PVC 让这个 ordinal 的数据目录跟着它走。

分片系统也需要稳定身份。假设 shard 0 归 `worker-0`，shard 1 归 `worker-1`，重建后仍希望 `worker-0` 拿回 shard 0 的本地状态。StatefulSet 的 ordinal 可以作为外部成员配置的一部分。Deployment 的副本没有这个语义，任何一个 Pod 都是可替换的。

有序发布也是价值之一。StatefulSet 默认更强调有序创建、更新和删除。对集群数据库来说，你可能不希望所有成员同时换版本；按 ordinal 逐个更新更容易保持 quorum。但有序不代表自动安全，schema、协议和数据兼容仍然要设计。

不要把稳定网络身份误解成“Pod 永远在同一台机器”。StatefulSet Pod 可能被调到别的 Node，IP 也可能变化；稳定的是 Pod 名字、DNS 名和 PVC 绑定关系。若应用还依赖本地 NVMe、NUMA、GPU 或节点级缓存，就要额外用 node affinity、local PV、拓扑约束或应用层恢复。

结合 LogServe，当前 `logd` 是单副本 Deployment + PVC，作为演示简单直接。如果后续把 logd 做成多副本复制日志，稳定成员 ID、稳定 PVC、启动顺序、leader election 和 fencing 会变得重要，StatefulSet 才更合适。`control` 多副本如果只是无状态 API，可以用 Deployment；如果每个副本持有固定 shard 或调度分片，才需要稳定身份设计。

面试里可以这样回答：

```text
StatefulSet 的稳定网络标识给每个副本固定 ordinal 和 DNS，通常还配合每副本 PVC。它适合数据库、quorum、分片和需要固定成员身份的系统。稳定的是身份和存储绑定，不是永远固定在某台节点。它降低成员管理复杂度，但不自动解决协议兼容、复制一致性和故障恢复。
```

## Q025. Pod 被驱逐时本地缓存会发生什么？

**回答：**

Pod 被驱逐后，本地缓存通常要按“会丢”来设计。容器可写层会随容器消失；`emptyDir` 跟 Pod 生命周期绑定，Pod 删除后也会删除；内存缓存当然会丢。Pod 如果被重新调度到另一台节点，新 Pod 看不到旧节点上的本地文件和 page cache。只有挂载的 PV、外部对象存储、数据库、远端 cache 才能跨 Pod 生命周期保留。

驱逐有多种原因。节点内存压力、磁盘压力、PID 压力、节点 drain、优先级抢占、资源不足都可能导致 Pod 退出。自愿驱逐通常会走 Eviction API，可能受 PDB 约束；节点压力下的驱逐是 kubelet 为保护节点做的，本地缓存保不住。

本地缓存丢失不一定是坏事，前提是它真的是缓存。缓存的定义是：丢了可以从事实来源重建，只是慢一点。如果 Pod 本地 cache 里有唯一的业务结果、未上传的 snapshot、还没写入 shared log 的状态，那它就不是缓存，而是未持久化数据。驱逐时这类数据会丢，恢复语义会破。

缓存丢失的主要风险是冷启动风暴。节点 drain 或扩容后，很多新 Pod 同时启动，全部 cache miss，同时去对象存储、数据库、模型仓库拉数据。下游被打爆，readiness 迟迟不成功，HPA 继续扩容，故障扩大。缓存系统要有预热、限流、退避、并发下载控制和 fallback。

结合 LogServe，worker-local checkpoint cache 在当前实验里是性能优化，不是事实来源。worker 被驱逐后，新 worker 可以从 `model-source-dir` 或对象存储重新获取 checkpoint，只是 cold request 变慢。actor state、workflow state、task status 的事实来源应该是 shared log、metadata store 或 object store。`logd` 的 append-only log 不能放在 ephemeral local cache 里，所以 k8s 示例挂了 PVC。

排查时也要看 eviction reason。`kubectl describe pod` 的 Events、Node conditions、kubelet 日志、容器 last state、ephemeral storage usage 都有价值。很多缓存目录写在容器层或 emptyDir 里，增长太快还会触发 ephemeral storage pressure，把 Pod 自己驱逐掉。

面试里可以这样回答：

```text
Pod 被驱逐后，容器可写层、emptyDir 和内存缓存通常都会丢；重新调度到别的节点后也看不到旧节点本地数据。只有 PV 或外部存储能跨 Pod 生命周期保留。缓存必须能从事实来源重建，否则它就是未持久化状态。缓存丢失还会带来冷启动风暴，需要预热、限流和下游保护。
```
## Q026. 本地缓存和 Kubernetes 调度如何协同？

**回答：**

本地缓存和 Kubernetes 调度的矛盾在于：缓存希望 workload 留在“有数据的地方”，调度器希望按资源、亲和性、可用区、污点、优先级把 Pod 放到合适节点。两者要协同，不能只靠某一个层面。

第一种方式是节点亲和性。给有特定硬件、磁盘、模型、数据集的节点打 label，Pod 用 nodeSelector 或 node affinity 选择这些节点。比如 GPU 节点、带 NVMe 的节点、预热了某类模型的节点。required affinity 是硬约束，调度不上就 Pending；preferred affinity 是软偏好，放不到理想节点也能运行。缓存场景通常优先用 preferred，避免缓存 miss 变成不可调度。

第二种方式是拓扑约束和反亲和。多个副本不要全部落在同一台节点或同一个 zone，避免节点故障时缓存和服务一起没了。topology spread constraints、pod anti-affinity 可以帮助分散副本。但约束太复杂会拖慢调度，甚至让 Pod 长期 Pending。缓存命中和高可用之间要取平衡。

第三种方式是把缓存状态暴露给应用层调度。Kubernetes 默认 scheduler 并不知道某个 worker 里已经缓存了 `model-A:v1`。它只知道 Pod、Node、资源和 labels。LogServe 这类系统如果要按模型 cache 命中调度任务，应用层 scheduler 要维护 worker cache view，然后把任务分给已有缓存的 worker。Kubernetes 负责把 worker Pod 放到节点，LogServe control 负责把 LLM task 放到合适 worker，这是两个调度层。

第四种方式是预热和守护进程。可以用 init container 预拉模型或数据，用 DaemonSet 做节点级缓存代理，用 image pre-pull 降低镜像冷启动，用 warmup Job 在发布前准备缓存。要注意预热也会打下游，必须限速。预热失败时，应用要么不 ready，要么允许冷启动 fallback。

第五种方式是承认缓存会丢。任何调度协同都不能假设本地缓存永远存在。节点重启、驱逐、镜像升级、磁盘清理都会让缓存失效。系统要记录 cache hit/miss、cold start latency、checkpoint fetch latency、缓存大小和淘汰原因。没有观测，缓存调度很容易变成玄学。

结合 LogServe，已有 locality-aware 和 predicted-latency 调度思路：control 根据 worker model cache 上报选择更合适的 worker。这属于应用层调度。Kubernetes 层可以继续做 worker Pod 的资源 request、anti-affinity、节点标签和本地盘约束。两层不能互相替代：Kubernetes 不知道单个 LLM task 的模型版本，LogServe 也不应该直接绕过 Kubernetes 去假设 Pod 永远在某个节点。

面试里可以这样回答：

```text
本地缓存要和 Kubernetes 调度协同：节点标签和 affinity 让 Pod 倾向于有资源/缓存的节点，anti-affinity 和 topology spread 避免副本集中；应用层 scheduler 维护实际 cache view，把任务分给命中缓存的 worker；预热、限流和 fallback 处理冷启动。缓存只能提升性能，不能成为唯一事实来源。
```

## Q027. node affinity 和 pod anti-affinity 解决什么部署问题？

**回答：**

node affinity 解决“Pod 应该去哪些节点”的问题，pod anti-affinity 解决“Pod 不应该和哪些 Pod 放太近”的问题。它们一个面向节点属性，一个面向 Pod 分布。

node affinity 基于节点 label。比如 `disk=ssd`、`gpu=true`、`zone=us-east-1a`、`workload=llm`。required node affinity 是硬规则，不满足就不调度；preferred node affinity 是软规则，调度器会尽量满足。它适合把需要特殊硬件、特殊内核参数、特殊本地盘、特殊网络环境的 Pod 放到对应节点。

pod anti-affinity 基于已有 Pod 的 label 和拓扑域。比如同一个 Deployment 的副本不要放在同一个 `kubernetes.io/hostname`，或者同一个服务的多个副本尽量跨 zone。这样节点宕机、机架故障、zone 故障时，不会一次性打掉所有副本。它也可以避免资源热点，比如多个 CPU 密集型副本不要挤在一起。

pod affinity 是相反方向：希望某些 Pod 靠近。比如应用和本地 cache proxy 放同一节点，或者计算任务靠近数据服务。但 affinity 容易制造故障相关性，不能为了低延迟把所有关键组件都堆到一台机器。

这些规则的风险是约束过度。required affinity 太多会让 Pod Pending；pod anti-affinity 在大集群里可能增加调度计算成本；节点 label 如果维护不准，调度结果就会错。工程上常见做法是关键硬件用 required，性能偏好用 preferred；高可用分散用 topology spread constraints 或适度 anti-affinity；再配合 PDB、资源 request 和监控。

结合 LogServe，`logd` 和 `control` 最好不要和所有 worker 强绑定在同一节点，否则一个节点问题会同时影响控制面和执行面。worker 副本可以用 anti-affinity 分散；带模型 cache 的 worker 可以用 node affinity 倾向于有本地 SSD 或 GPU 的节点；如果 `logd` 使用本地 PV，那么它会受到 PV 拓扑约束，不是随便调到任意节点都能挂上数据。

面试里可以这样回答：

```text
node affinity 根据节点标签选择节点，适合硬件、zone、本地盘、GPU 等约束；pod anti-affinity 根据已有 Pod 标签把副本分散到不同主机或拓扑域，降低相关故障和资源热点。required 是硬约束，preferred 是软偏好。约束太强会 Pending，约束太弱会集中风险。
```

## Q028. PDB 如何影响可用性？

**回答：**

PDB，全称 PodDisruptionBudget，用来限制自愿中断时同时不可用的 Pod 数。它保护的是 eviction 类操作，比如节点 drain、集群升级、管理员主动驱逐。PDB 不阻止节点突然宕机、内核崩溃、容器 OOM、硬件故障这类非自愿中断。

PDB 有两种常见写法：`minAvailable` 和 `maxUnavailable`。`minAvailable: 2` 表示匹配的 Pod 至少要保持 2 个可用；`maxUnavailable: 1` 表示最多允许 1 个不可用。可用性判断依赖 Pod readiness，所以 readiness 配置错了，PDB 的效果也会偏。一个 Pod readiness 失败，就可能被视为不可用，从而阻塞后续 drain。

PDB 的价值在于让维护操作更温和。比如一个有 5 个副本的 API，PDB 设置 `maxUnavailable: 1`，节点 drain 时不会一次驱逐掉多个副本。集群升级、节点替换、自动缩容都要尊重这个预算，避免管理员操作直接打穿服务容量。

PDB 也可能造成运维阻塞。单副本服务如果设置 `minAvailable: 1`，节点 drain 时这个 Pod 不能被自愿驱逐，升级会卡住。副本数不足、readiness 长期失败、资源不够创建替代 Pod，都会让 eviction 被拒绝。PDB 不是越严格越好，它要和副本数、Deployment strategy、HPA、集群容量一起设计。

还有一个边界：PDB 不保证有足够容量。它只会拒绝自愿驱逐，不会帮你自动扩容节点，也不会让 Pending Pod 突然调度成功。如果集群没有资源创建替代副本，PDB 只能阻止进一步驱逐，不能恢复已经不足的容量。

结合 LogServe，`control` 当前单副本即使设置 PDB，也只是阻止自愿 drain，不会实现高可用。要让 control 高可用，必须先解决多副本一致性、leader election、队列和 metadata 语义。worker 多副本可以设置 PDB，避免节点维护时一次性驱逐太多执行能力。`logd` 单副本加 PVC 的场景，PDB 也只能减少维护误伤，不能抵抗节点故障或存储故障。

面试里可以这样回答：

```text
PDB 限制自愿中断时同时不可用的 Pod 数，常用于节点 drain 和集群升级。它不阻止节点宕机、OOM、硬件故障等非自愿中断。PDB 太松保护不足，太严会阻塞运维；单副本 minAvailable=1 只能阻止自愿驱逐，不等于高可用。PDB 要和副本数、readiness、HPA 和容量规划一起看。
```

## Q029. 多副本服务如何处理 leader election？

**回答：**

多副本服务如果只有一个副本能执行某类写操作，就需要 leader election。Kubernetes 里常见做法是使用 Lease 对象：每个候选实例尝试更新同一个 Lease，成功续约的一方在租约期内当 leader。其他实例观察 Lease，发现租约过期或 holder 改变后再竞选。

leader election 的关键不是“谁抢到 leader”，而是“旧 leader 失联后不会继续产生危险写入”。网络分区、GC pause、CPU throttling、apiserver 短暂不可用都可能让旧 leader 没及时续约。新 leader 接管后，旧 leader 可能还在运行一小段时间。如果外部系统只认“进程觉得自己是 leader”，就可能 split brain。

所以 leader election 要配合 fencing。Lease 的 resourceVersion、renewTime、leaseDuration、leader identity 可以告诉大家谁当前持有租约，但写关键状态时还要带 epoch 或 fencing token。下游存储、任务队列、日志系统要拒绝旧 epoch 写入。没有 fencing，leader election 只能降低概率，不能严格防止旧主继续写。

参数也要谨慎。leaseDuration 太短，短暂抖动就频繁切主；太长，真实 leader 死亡后恢复慢。renewDeadline 和 retryPeriod 要结合 apiserver p99、网络延迟、GC pause 和业务恢复时间设置。leader 切换期间要有指标和日志，不能只看到“服务还活着”。

有些服务可以避免单 leader。比如每个 shard 一个 leader，或者用幂等任务队列让多个 worker 并发消费，或者把一致性委托给数据库/etcd/Kafka。不是所有多副本都要自己写 leader election。能用成熟协调系统就不要在应用里临时拼一个锁。

结合 LogServe，worker ownership 已经有 `owner_worker_id + epoch` 的思想，用来拒绝旧 worker 对 actor 状态的完成。control 如果变多副本，也需要类似 fencing：哪个 control 负责调度，写 shared log 和 metadata 时如何防旧 leader，task lease 如何避免重复发放。`logd` 如果多副本，要更复杂，因为 append-only log 的顺序和持久化是系统事实来源，不能只用一个 Kubernetes Lease 就宣称一致。

面试里可以这样回答：

```text
多副本 leader election 通常用 Kubernetes Lease 或外部一致性存储。候选实例续约 Lease，持有者当 leader；但真正安全还要 fencing token/epoch，让下游拒绝旧 leader 写入。leaseDuration 太短会抖动，太长会恢复慢。leader election 只能选主，不能替代写路径幂等、一致性和 split-brain 防护。
```

## Q030. Kubernetes 中如何安全滚动升级 CRD 或数据库 schema？

**回答：**

安全升级 CRD 或数据库 schema 的核心是同一句话：先让所有读者能读新旧格式，再让写者写新格式，最后才删除旧格式。直接改 schema 然后滚动应用，是最容易出事故的方式。

CRD 升级要关注 API 版本、served/storage version、conversion 和客户端兼容。常见做法是先在 CRD 里增加新版本，同时继续 serve 旧版本；如果不同版本字段结构变化，提供 conversion webhook，把旧对象和新对象互相转换；更新 controller 和客户端，让它们能读旧版本对象，也能处理新版本字段；确认所有写路径都迁移后，再做 storage version migration；最后才停止 serve 旧版本或删除旧字段。删除字段前要确认没有旧 controller、旧 kubectl 插件、旧自动化脚本还在使用它。

数据库 schema 升级通常走 expand-contract。第一步 expand：只做兼容性增加，比如新增 nullable 列、新表、新索引、默认值安全的字段，不破坏旧代码。第二步应用发布：新代码兼容读旧字段和新字段，必要时双写。第三步 backfill：把历史数据补齐，过程要限速、可暂停、可重试。第四步切读：新代码从新字段读，保留旧字段一段时间。第五步 contract：确认旧版本全部下线，再加 NOT NULL、唯一约束、删除旧列或旧表。

索引和约束也要注意。大表加索引可能锁表或拖垮数据库，生产里要用在线索引、并发索引或影子表策略。加唯一约束前要先清理脏数据。改列类型可能重写全表。migration 不是只看 SQL 能不能执行，还要看锁、回滚、复制延迟、连接池和应用超时。

滚动升级期间要有发布顺序。通常是：先部署兼容 reader，再启用双写，再 backfill，再切读，再清理。feature flag 很有用，因为它能把“代码已部署”和“行为已开启”拆开。监控要覆盖错误率、慢查询、conversion webhook 延迟、controller reconcile error、schema version、backfill 进度、旧字段读写次数。回滚也要提前想清楚：如果新版本已经写了旧版本读不懂的数据，回滚就不是简单 `kubectl rollout undo`。

结合 LogServe，shared log 的事件 schema 和数据库 schema 一样敏感。比如给 `TaskCompleted` 增加 result ref 字段，要先让 replay 逻辑能处理字段缺失，再让新 worker 写字段；如果要改变 actor snapshot 格式，要保留旧 snapshot 的读取路径；如果 metadata store 加新索引或字段，要保证 control 重启时仍能从 shared log bootstrap。CRD/database 的安全升级思路可以直接迁移到 log record schema：新增优先，兼容读取，延迟删除，回滚可行。

面试里可以这样回答：

```text
CRD 和数据库 schema 安全升级都要先兼容读，再改变写，最后清理旧格式。CRD 要处理 served/storage version、conversion webhook、storage migration 和旧客户端；数据库走 expand-contract：加字段或表、双读双写、backfill、切读、确认旧版本下线后再删字段或加硬约束。发布和回滚要靠 feature flag、监控和明确顺序，不靠一次滚动更新赌运气。
```

## Q031. 如何设计容器镜像的最小权限运行？

**回答：**

最小权限运行要从两个层面一起看：镜像里有什么，运行时允许它做什么。只把镜像做小，不等于最小权限；把容器设成非 root，也不等于最小权限。比较稳的做法是先把进程需要的文件、用户、端口、写目录、系统调用、Linux capabilities、ServiceAccount 权限都列出来，然后逐项收紧。

镜像构建阶段先减少攻击面。Go 服务可以用 multi-stage build，只把最终二进制、必要 CA 证书、时区数据和少量运行配置拷到运行镜像。能用 distroless、scratch 或精简基础镜像时，不要把编译器、包管理器、shell、curl、gcc、调试工具和测试数据留在生产镜像里。镜像里不要写入私钥、token、npmrc、pip.conf 或云凭据；这些内容一旦进过某个 layer，后面再删除也不能当作真正清理。

运行用户要固定。Dockerfile 里用 `USER 10001:10001`，Kubernetes 里再用 `runAsNonRoot: true`、`runAsUser`、`runAsGroup` 做兜底。不要依赖镜像里的用户名解析；生产里更可靠的是数值 UID/GID。需要写文件时，只给特定目录写权限，比如 `/tmp`、`/var/run/app` 或挂载的 emptyDir/PVC，不要让整个根文件系统可写。`readOnlyRootFilesystem: true` 能把很多意外写入提前暴露出来。

Linux capabilities 要默认全丢，再按需加回。大多数业务进程不需要 `CAP_SYS_ADMIN`、`CAP_NET_ADMIN`、`CAP_SYS_PTRACE` 这类高危能力。常见配置是 `capabilities.drop: ["ALL"]`，只在确实需要绑定低端口、设置网络参数或做特定内核操作时加最小集合。`allowPrivilegeEscalation: false` 很重要，它会阻止进程通过 setuid、file capability 等路径拿到比父进程更高的权限。`privileged: true`、`hostPID`、`hostNetwork`、`hostIPC`、hostPath、挂载 Docker socket 都要当作例外审批，而不是普通配置。

系统调用和内核安全配置也要落地。Linux 节点上应使用 `seccompProfile: RuntimeDefault`，再用 AppArmor 或 SELinux profile 收紧文件和进程访问。很多逃逸不是应用代码直接发生的，而是容器拿到了过宽的系统调用面、宿主路径或内核能力。Pod Security Standards 里的 Restricted profile 是一个很好的基线：非 root、禁止提权、限制 capabilities、限制 host namespace 和危险 volume，然后再根据应用需要打开少数例外。

Kubernetes 权限要分开处理。容器里的 root 权限和访问 Kubernetes API 的 RBAC 不是一回事。Pod 默认挂载的 ServiceAccount token 如果用不到，就关掉 `automountServiceAccountToken`；如果要访问 API，就给独立 ServiceAccount，只授予所需 namespace、resource、verb。很多线上事故不是容器逃逸，而是 Pod 里的应用凭据能 list secrets、patch deployments 或读全 namespace 配置。

还要考虑可观测和运维边界。最小镜像通常没有 shell，排障不能靠 `kubectl exec sh`。这不是坏事，但要准备好日志、指标、健康检查、ephemeral container 调试策略和镜像 SBOM。生产镜像少工具，调试镜像和临时容器可以有工具；两者不要混在一起。

结合 LogServe，`logd`、`control`、`worker` 这类组件都应该用非 root 运行。`logd` 只需要写自己的 append-only log 目录或 PVC，不应该拿宿主机路径；`worker` 如果执行 Python task，要额外限制网络、文件写入、CPU、内存、PID 数和超时。mock LLM 或本地 checkpoint cache 可以挂只读模型源目录和独立 cache 目录，不能把本地开发凭据打进镜像。

面试里可以这样回答：

```text
最小权限不是只选一个小镜像，而是把镜像内容、运行用户、文件系统、capabilities、seccomp/LSM、ServiceAccount/RBAC、volume 和网络访问一起收紧。默认非 root、只读 rootfs、drop ALL capabilities、禁止 privilege escalation、使用 RuntimeDefault seccomp、避免 host namespace 和 hostPath，需要写的目录单独挂载。Kubernetes API 权限用独立 ServiceAccount 和最小 RBAC。调试能力通过日志、指标和 ephemeral container 补，不把 shell 和凭据留在生产镜像里。
```

## Q032. 如何收集容器 stdout/stderr 日志？

**回答：**

容器日志的默认路径应该是 stdout 和 stderr。应用把结构化日志写到标准输出，容器运行时负责捕获这两个流，kubelet 按 CRI 日志格式和节点文件组织它们，`kubectl logs` 再通过 kubelet 暴露出来。这个链路的好处是应用不用关心节点文件路径，也不用在容器里自己做日志轮转。

节点上通常会有两层路径：运行时保存真实日志文件，Kubernetes 再通过 `/var/log/pods` 和 `/var/log/containers` 建软链接，方便节点日志采集器按 Pod、namespace、container 名字采集。DaemonSet 形式的 agent，比如 Fluent Bit、Vector、Filebeat、OpenTelemetry Collector，会在每个节点上读取这些文件，补上 Kubernetes 元数据，然后转发到 Elasticsearch、Loki、ClickHouse、对象存储或其他日志平台。

应用侧最好直接输出一行一个事件。JSON 日志更容易机器解析，但要控制字段基数和大小。`trace_id`、`request_id`、`workflow_id`、`actor_id` 这类字段有价值；把整段 payload、完整二进制、巨大 stack 或用户敏感数据写进去，会把日志系统拖垮。多行日志要谨慎，异常栈可以保留，但采集器要有 multiline 合并规则，否则一条错误会拆成几十条事件。

stderr 不应该简单等同于错误级别。很多运行时和框架会把 warning、panic、启动信息写 stderr。更可靠的是日志内容里带 `level` 字段。stdout/stderr 可以作为来源维度，不能替代业务日志级别。反过来，也不要把所有错误吞掉只写文件；Kubernetes 默认工具和节点采集器最容易拿到的是标准流。

有些老应用只能写文件，这时有两个办法。更简单的是把应用配置改成写 `/dev/stdout` 或 `/dev/stderr`。改不了时，可以加 sidecar tail 文件并把内容转到自己的 stdout。这个方案能兼容遗留程序，但会多一份节点存储消耗和一个容器资源开销，还要处理文件轮转、断点、重复读取和 sidecar 退出顺序。能直接写标准流时，没必要绕一圈。

日志收集还要考虑背压。stdout 写入最终会落到运行时和节点文件，如果日志量暴涨，节点磁盘、采集 agent、网络出口、后端索引都会出问题。线上要设置 kubelet 日志轮转、采集器限速、丢弃策略和告警。应用也要限制高频日志，比如每次重试都打印一整段响应体，会让故障期间日志量反过来加重故障。

结合 LogServe，`worker` 执行 Python task 时要把子进程 stdout/stderr 分开捕获：一份进入任务结果或调试输出，一份作为运行日志进入容器 stdout/stderr。这里要有大小上限和截断策略，不能让一个失控脚本无限写日志撑爆节点。`logd` 自己的 append-only log 是业务事实来源，不要和容器运行日志混为一谈；容器日志服务于排障，shared log 服务于恢复和重放。

面试里可以这样回答：

```text
容器应用优先把日志写 stdout/stderr，运行时捕获后交给 kubelet，节点日志 agent 以 DaemonSet 方式读取 /var/log/containers 或 CRI 日志文件，补 Kubernetes 元数据后转发到日志后端。应用应输出结构化日志，控制字段基数、大小和敏感信息。老应用写文件时，优先改到 /dev/stdout；改不了再用 sidecar tail。日志链路必须有轮转、限速、采集失败告警和背压策略。
```

## Q033. sidecar 模式适合哪些横切能力？

**回答：**

sidecar 适合放和主容器强耦合、和业务逻辑弱耦合的横切能力。它和主容器在同一个 Pod 里，共享网络命名空间，可以用 `localhost` 通信，也可以共享 volume。这个位置很适合做本地代理、日志转发、证书刷新、配置热更新、轻量缓存、指标导出、流量拦截这类能力。

日志是经典场景。主容器写文件，sidecar tail 文件并转 stdout；或者 sidecar 运行一个小型采集器，把本 Pod 的日志转到外部系统。监控也类似，sidecar 可以暴露主进程内部指标、做协议转换，或者把本地统计推给 collector。安全场景里，sidecar 可以刷新短期证书、代理 mTLS、注入密钥文件、做本地授权检查，但它不应该成为万能保险箱；主容器和 sidecar 共享 Pod 边界，任何一方被攻破都会影响同一个 Pod。

服务网格代理也是 sidecar 的典型用法。Envoy 这类代理和应用跑在同一个 Pod，通过 iptables、eBPF 或运行时配置接管进出流量，实现 mTLS、重试、熔断、路由、流量镜像、指标和 tracing。这样应用代码少改，但每个 Pod 多了代理资源、启动顺序、配置下发和排障复杂度。

sidecar 还适合本地辅助服务。比如配置文件 watcher、模型文件预热器、本地缓存代理、对象存储上传器、checkpoint 同步器。判断标准是：主容器离开它还能不能独立表达业务语义。如果答案是不能，可能它只是主应用的一部分；如果答案是能，但加上 sidecar 后能复用一套横切能力，sidecar 才合适。

不适合放进 sidecar 的东西也很明确。需要独立扩缩容的服务，不要塞进同一个 Pod；失败模式和主应用不同的组件，不要为了部署方便强绑；CPU 或内存波动很大的任务，不要和主容器抢同一个 Pod 的资源预算；需要独立发布、独立回滚、独立 SLO 的模块，通常应该拆成服务或 Job。

sidecar 的坑主要来自生命周期和资源。老版本 Kubernetes 里 sidecar 只是普通容器，启动和退出顺序不好控制；新式 restartable sidecar 改善了这一点，但仍要考虑 readiness、termination、资源 request/limit、QoS 等级。sidecar 没有 request 时，调度器低估 Pod 成本；sidecar readiness 设计错了，整个 Pod 会迟迟不 Ready。

结合 LogServe，可以把模型预热、日志转发、证书刷新做成 sidecar，但不要把 `control` 和 `worker` 塞进一个 Pod。它们的扩缩容和故障模式不同。Python executor 如果只是 worker 内部执行引擎，放同容器或子进程更直接；如果要强隔离、独立重启、独立资源统计，再考虑 sidecar 或独立 Pod。

面试里可以这样回答：

```text
sidecar 适合日志采集、指标导出、服务网格代理、证书刷新、配置热更新、本地缓存、文件同步这类横切能力。它的优势是和主容器共享网络和 volume，能在不改主业务代码的情况下增强能力。边界是：需要独立扩缩容、独立发布、独立 SLO 或失败模式不同的组件不要硬塞进 sidecar。使用时要给 sidecar 配 request/limit、readiness 和退出策略，否则会引入调度、资源和排障问题。
```

## Q034. 服务网格解决什么问题，又引入什么复杂度？

**回答：**

服务网格解决的是服务间通信的横切治理问题。微服务多了以后，每个服务都要处理 TLS、身份认证、重试、超时、熔断、限流、灰度路由、指标、访问日志、trace 传播。如果这些能力全写在业务 SDK 里，语言栈、版本、配置和行为很难统一。服务网格把这些能力下沉到数据平面代理和控制平面配置里，让应用少写一部分网络治理代码。

最常见的数据平面是 sidecar proxy。每个 Pod 旁边跑一个代理，进出流量经过它；控制平面负责下发服务发现、证书、路由、策略和 telemetry 配置。这样可以做服务到服务的 mTLS，按服务身份做授权，按 header、权重、版本做流量切分，统一记录请求指标和 trace。对多语言系统来说，这一点很有吸引力。

它也能把发布策略做细。比如 canary、蓝绿、流量镜像、故障注入、按用户或 header 路由。没有服务网格也能做这些，但通常要靠 Ingress、Gateway、SDK、应用配置或负载均衡器拼起来。网格的价值在于把 east-west traffic 的治理统一化，尤其是集群内部服务互调很多的时候。

复杂度也很实在。第一个成本是资源和延迟。每个 Pod 多一个代理，CPU、内存、连接数、启动时间都会增加；高 QPS 和长连接场景要评估代理性能。第二个成本是故障面。原本请求从 A 到 B，现在变成 A app -> A proxy -> B proxy -> B app，中间还有证书、xDS 配置、iptables/eBPF 转发和控制平面。排障时要同时看应用日志、proxy 日志、proxy 配置、证书状态和策略命中。

第三个成本是语义冲突。网格可以配置重试，但业务请求是否幂等，只有业务知道；网格可以熔断，但熔断阈值和服务容量要和应用 SLO 对齐；网格可以做超时，但应用内部 deadline、数据库超时、RPC 超时如果不一致，会出现上游已经放弃、下游还在工作的情况。把网络治理下沉后，业务边界反而要讲得更清楚。

第四个成本是组织和升级。服务网格通常牵涉平台团队、安全团队和业务团队。根证书轮换、代理版本升级、控制平面升级、策略发布、命名空间注入、异常豁免都要有流程。一个配置错误可能影响一整片服务。小系统一上来就上网格，经常是用十倍复杂度解决一个还没出现的问题。

结合 LogServe，如果只是本地实验或少量组件互调，Service、Ingress/Gateway、应用层 timeout/retry 和指标已经够用。等到 `control`、`worker`、`logd`、对象存储、模型服务和外部调用变多，需要统一 mTLS、流量治理和跨服务 telemetry 时，再引入网格更合理。即使引入，也不能把任务幂等、lease、epoch、shared log 恢复语义交给网格；网格只治理通信，不理解 LogServe 的业务状态机。

面试里可以这样回答：

```text
服务网格把服务间通信的 mTLS、身份、路由、重试、超时、熔断、限流、灰度和观测从业务代码下沉到代理和控制平面。它适合多语言、多服务、east-west 流量多、需要统一安全和治理的系统。代价是每个 Pod 多代理，增加资源、延迟、配置、证书、控制平面和排障复杂度。网格不能替代业务幂等、事务、任务 lease 和数据一致性设计，重试和超时还要和业务语义对齐。
```

## Q035. 如何在 Kubernetes 中调试网络连通性？

**回答：**

Kubernetes 网络排障要按路径分层，不要一上来就猜 CNI。一次请求通常会经过客户端 Pod、DNS、Service、EndpointSlice、kube-proxy 或 eBPF datapath、目标 Pod、NetworkPolicy、节点路由、Ingress/Gateway 或外部负载均衡。排障顺序应该从最接近现象的地方开始，逐层排除。

第一步看对象是否存在、状态是否正常。`kubectl get pod -o wide` 看 Pod IP、Node、Ready、重启次数；`kubectl describe pod` 看 Events、探针、容器端口、CNI 分配错误；`kubectl get svc` 看 Service type、ClusterIP、port、targetPort；`kubectl get endpointslice -l kubernetes.io/service-name=xxx` 看 Service 有没有选中后端 Pod。很多故障只是 selector 标签错了、targetPort 写错了、Pod 没 Ready，所以 EndpointSlice 为空。

第二步从集群内部发起测试。用一个临时 debug Pod 或 ephemeral container 执行 `nslookup service.namespace.svc.cluster.local`、`curl -v`、`nc -vz`。先测 Service DNS，再测 Service ClusterIP，再直接测 Pod IP 和目标端口。如果 DNS 不通，去查 CoreDNS、Pod `/etc/resolv.conf`、namespace 后缀和 NetworkPolicy 对 DNS 的限制。如果 Service IP 不通但 Pod IP 通，重点查 Service、kube-proxy/eBPF、EndpointSlice、端口映射。如果 Pod IP 也不通，重点查目标进程监听、NetworkPolicy、CNI 路由和节点网络。

第三步确认应用真的在监听。`kubectl exec` 进目标容器，查看 `ss -lntp` 或应用日志，确认监听地址是不是 `0.0.0.0` 而不是 `127.0.0.1`。容器里监听 localhost，只能被同一个 Pod 内的容器访问，Service 转发进来会失败。还要确认协议，TCP、UDP、HTTP、gRPC、TLS 都可能被误判成“网络不通”。比如 TLS 握手失败不是三层连通性问题。

第四步查策略。NetworkPolicy 默认不一定生效，取决于 CNI 是否支持；一旦有 ingress/egress policy，Pod 选择器、namespaceSelector、端口和 DNS egress 都可能影响连通。Service mesh 也会加入一层策略，mTLS 模式、AuthorizationPolicy、Sidecar resource、Envoy listener 都可能拦截请求。这里要分清是 Kubernetes NetworkPolicy 拦了，还是网格代理拒绝了。

第五步查节点和数据平面。传统 kube-proxy 集群要看 iptables/ipvs 规则、kube-proxy 日志、conntrack 表是否满；eBPF CNI 要看对应工具，比如 Cilium 的 endpoint、policy、service、hubble flow。跨节点不通时，看 Node 路由、MTU、封包、云安全组、防火墙和 overlay/underlay 配置。间歇性失败时，conntrack、DNS 缓存、Endpoint 更新延迟、Pod readiness 抖动都要纳入排查。

Ingress/Gateway 路径要单独看。外部请求失败时，先确认 Ingress/Gateway 对象、listener、route、backendRef、证书、负载均衡器健康检查，再回到 Service 和 Pod。很多“外部访问不通”不是集群内部网络问题，而是云 LB 安全组、证书 SNI、Host header、路径 rewrite 或网关控制器没有成功下发配置。

结合 LogServe，可以按 `control -> logd`、`control -> worker`、`worker -> object/model source` 这些调用链逐段测。先在 `control` Pod 内解析 `logd` Service DNS，再打 `logd` 的 Service port，再直连 Endpoint Pod IP。若直连 Pod IP 成功但 Service 不通，查 Service selector 和 EndpointSlice；若 Service 成功但业务超时，查应用 deadline、logd 队列、PVC I/O 和探针，而不是继续盯着 CNI。

面试里可以这样回答：

```text
Kubernetes 网络排障按层走：Pod 是否 Running/Ready，Service 是否存在，selector 是否选中 Pod，EndpointSlice 是否有地址，DNS 是否能解析，Service ClusterIP 是否能访问，Pod IP 和端口是否直连成功，NetworkPolicy 或 mesh 策略是否拦截，kube-proxy/eBPF/CNI/节点路由是否正常。命令上用 get/describe、exec debug pod、nslookup、curl/nc、endpointslice、日志和必要时 tcpdump。先定位断在哪一层，再查对应组件。
```
## Q036. namespace 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

这里要先澄清名词。容器语境里的 namespace 通常指 Linux namespace；Kubernetes 里的 Namespace 是 API 对象分组。两者名字一样，层级完全不同。Linux namespace 是内核机制，把 PID、mount、network、IPC、UTS、user、cgroup、time 这类全局资源包装成局部视图。Kubernetes Namespace 是集群资源命名和配额边界，用来把 Deployment、Service、ConfigMap 这类对象按团队或项目分开。

Linux namespace 的核心目标是视图隔离。它让进程以为自己有独立的进程树、网络栈、挂载点、hostname 或用户 ID 映射。这个目标首先服务于正确性和可维护性：不同容器看到的资源不混在一起，进程不会随便看到宿主机的 PID，端口空间可以在不同 network namespace 里重复使用，mount 视图可以单独组织。安全性是它的一个结果，但不是完整安全边界。

它对性能的直接帮助不大。namespace 本身不是让程序跑得更快的机制。它有时会提高部署密度，因为多个应用可以在同一内核上隔离运行；但 CPU、内存、I/O 的限制不是 namespace 做的，是 cgroup 做的。把 namespace 说成性能优化工具，面试里很容易被追问到露馅。

安全方面要讲得克制。namespace 能减少可见面和误操作面，比如容器里看不到宿主大部分进程、网络接口和挂载点。但它不限制资源用量，不自动删除 capabilities，不自动禁止危险系统调用，也不阻止你把宿主目录挂进去。真正的容器安全通常是 namespace、cgroup、capabilities、seccomp、LSM、只读 rootfs、非 root 用户和最小挂载一起工作。

Kubernetes Namespace 的目标更偏可维护性和治理。它解决名字冲突、团队隔离、RBAC 作用域、ResourceQuota、LimitRange、NetworkPolicy 选择范围这些问题。它不等于租户强隔离。默认情况下，不同 Kubernetes namespace 里的 Pod 仍可能互相访问，除非你配置 NetworkPolicy 或网格策略；集群级资源，比如 Node、StorageClass、PV，也不属于某个 namespace。

结合 LogServe，如果问 Linux namespace，我会说它让 `worker`、`logd`、`control` 在容器里有各自的进程、网络和文件系统视图。如果问 Kubernetes Namespace，我会说可以把实验环境、开发环境和演示环境拆开，配不同 quota、RBAC 和 NetworkPolicy。两者都不能单独证明系统安全，只是隔离设计的一部分。

面试里可以这样回答：

```text
Linux namespace 的核心目标是资源视图隔离，让进程看到自己的 PID、mount、network、IPC、hostname、user 映射等局部视图。它主要解决正确性和可维护性问题，也提供安全基础，但不是完整安全边界；资源限制要靠 cgroup，权限收紧要靠 capabilities、seccomp、LSM 等机制。Kubernetes Namespace 则是 API 资源的命名、配额和权限作用域，不能和 Linux namespace 混为一谈。
```

## Q037. namespace 的典型适用场景和不适用场景分别是什么？

**回答：**

Linux namespace 适合需要进程级隔离但不想启动完整虚拟机的场景。容器就是最典型用法：给应用独立 PID 视图、rootfs 视图、网络栈和 hostname，让多个应用共享同一个宿主内核，同时减少互相干扰。CI sandbox、构建隔离、临时测试环境、轻量任务执行器、网络实验环境，也经常用 namespace 做基础。

network namespace 很适合隔离端口和路由。不同容器可以都监听 `0.0.0.0:8080`，因为它们在不同网络命名空间里；CNI 再用 veth、bridge、路由或 eBPF 把它们接入集群网络。mount namespace 适合给进程一个独立文件系统视图，比如只挂载应用目录、只读 rootfs、临时工作目录。PID namespace 适合让容器里的进程树独立，便于生命周期管理。

user namespace 适合降低 root 风险。容器内的 UID 0 可以映射为宿主机上的普通 UID，这样容器里看起来是 root，宿主机上并不是 root。这个机制对 rootless container 很关键。不过 user namespace 的文件权限、挂载、capabilities 语义比较复杂，不能只写一行配置就以为所有问题都解决了。

不适用场景也要明确。第一，强多租户和不可信代码执行不能只靠普通 namespace。开放给外部用户的任意代码执行，一般还要 VM、microVM、gVisor、Kata、Firecracker、严格 seccomp、网络隔离和资源限制。第二，需要不同内核版本或内核模块的 workload，不适合只靠容器，因为所有容器共享宿主内核。第三，资源公平性不是 namespace 的职责，要靠 cgroup、调度器和限流。

Kubernetes Namespace 适合团队、环境、项目、成本中心、权限边界的组织。比如 `dev`、`staging`、`prod` 分开，或者每个团队一个 namespace，再配 RBAC、quota、limit range、NetworkPolicy。不适合用 namespace 区分同一个应用的不同版本；这种情况通常用 label、Deployment、Service、Gateway route 或发布系统管理。Kubernetes 官方文档也提醒，不需要为了少量用户或稍微不同的资源滥建 namespace。

结合 LogServe，Linux namespace 适合把 Python executor 和宿主隔开，但如果要跑外部用户提交的任意 Python，普通容器不够。Kubernetes Namespace 适合把本地演示、压测实验和集成测试资源分开，比如给压测 namespace 限定 CPU/memory quota，避免影响其他组件。

面试里可以这样回答：

```text
Linux namespace 适合容器、CI sandbox、构建隔离、轻量任务执行、网络实验等场景，目标是隔离进程看到的 PID、文件系统、网络、用户和 IPC 视图。不适合单独承担强多租户、不可信代码、资源公平和不同内核需求。Kubernetes Namespace 适合按团队、环境和项目组织 API 资源、RBAC、quota 和 policy，不适合替代 label 做版本区分，也不等于默认网络安全边界。
```

## Q038. namespace 和相近概念最容易混淆的边界在哪里？

**回答：**

最容易混淆的是 namespace 和 cgroup。namespace 管“看见什么”，cgroup 管“能用多少”。一个容器可以有独立 PID namespace，但如果没有 pids cgroup，仍可能 fork 太多进程拖垮宿主。一个进程可以被 memory cgroup 限制，但它看到的 `/proc`、网络接口和挂载点可能还是宿主视图。两者经常一起用，但职责不重叠。

第二个边界是 namespace 和 chroot。chroot 只是改变进程看到的根目录，不隔离 PID、网络、IPC、用户 ID，也不限制资源。mount namespace 能提供更完整的挂载视图隔离，还能控制挂载传播，但它也不是文件权限系统。你把宿主机敏感目录挂进容器，namespace 不会替你判断这个目录该不该暴露。

第三个边界是 namespace 和 VM。容器 namespace 共享宿主内核，VM 通常有独立 guest kernel。容器启动快、密度高，但内核攻击面共享；VM 隔离更厚，成本也更高。面试里说“容器是轻量虚拟机”容易被追问。更准确的说法是容器是受 namespace/cgroup/权限约束的进程集合。

第四个边界是 Linux namespace 和 Kubernetes Namespace。Linux namespace 是内核对象，影响进程运行时看到的资源。Kubernetes Namespace 是 API 对象作用域，影响对象名称、RBAC、quota 和策略选择。一个 Pod 在 `prod` namespace 里运行，不代表它有一个叫 `prod` 的 Linux namespace；它仍然由容器运行时创建 PID、mount、network 等内核 namespace。

第五个边界是 namespace 和安全策略。Network namespace 让容器有独立网络栈，但并不自动拒绝访问别的 Pod；Kubernetes NetworkPolicy 或网格授权策略才负责网络访问控制。User namespace 做 UID/GID 映射，但不等于删除 capabilities 或禁用危险 syscall。PID namespace 隔离进程视图，但如果容器开了 `hostPID: true`，这个隔离就被主动关闭了。

第六个边界是 namespace 和调度。Kubernetes scheduler 不根据 Linux namespace 做资源放置，它看的是 Pod spec、request、node label、taint、affinity 等信息。namespace 不决定 Pod 放在哪台机器，也不决定 CPU share。把调度失败归因于 namespace，通常方向是错的。

结合 LogServe，`worker` 的容器 namespace 可以限制它看到的进程和文件系统，但 task 调度仍然由 LogServe control 和 Kubernetes scheduler 共同决定。`tech_interview_qna` 里这类题如果不先把两种 namespace 分清，后面的安全、性能和可用性讨论都会混掉。

面试里可以这样回答：

```text
namespace 最容易和 cgroup、chroot、VM、Kubernetes Namespace、NetworkPolicy、RBAC 混淆。namespace 负责运行时资源视图隔离；cgroup 负责资源用量；chroot 只是文件根目录；VM 有独立内核；Kubernetes Namespace 是 API 资源作用域；NetworkPolicy/RBAC 才是访问控制。namespace 是容器隔离的基础，不是所有隔离问题的答案。
```

## Q039. namespace 在高并发场景下可能出现哪些隐藏问题？

**回答：**

namespace 在高并发下的问题通常不是“namespace 查询慢”，而是创建、销毁、引用和关联资源的成本被放大。容器快速扩缩容、短任务高频启动、CI 大量并发 job、serverless 冷启动，都可能频繁创建 PID、mount、network、user namespace。每次创建都伴随进程、挂载表、veth、路由、iptables/eBPF 状态、cgroup、日志目录等一串工作。

network namespace 的成本最容易被忽视。每个 Pod 通常有独立网络 namespace，CNI 要创建 veth、分配 IP、配置路由、更新 datapath。如果短时间启动上千个 Pod，瓶颈可能出现在 IPAM、CNI 插件、iptables 规则更新、eBPF map 更新、conntrack 或节点网络设备上。应用看见的是 Pod Pending 或启动慢，根因可能是网络命名空间和 CNI 初始化排队。

mount namespace 也会出问题。容器启动时要准备 rootfs、overlay、volume mount、projected volume、secret/configmap、subPath 等。挂载很多、层很多、volume 很多时，启动时间会变长。高并发创建和销毁还容易暴露 mount propagation、文件句柄残留、volume unmount 失败。表现可能是 Pod 卡在 Terminating、节点上有残留挂载点、后续同名路径无法清理。

PID namespace 下要注意 PID 1 的职责。容器里 1 号进程如果不回收子进程，高并发短子进程会堆出 zombie；如果信号处理不好，终止时子进程留下来，重启和退出会变慢。容器里的进程数还要配合 pids cgroup，否则 PID namespace 只让它看不到宿主进程，不会阻止它把宿主进程表打满。

namespace 本身也有数量限制和生命周期问题。Linux 通过 `/proc/sys/user/max_*_namespaces` 暴露每类 namespace 的数量上限。高并发创建时可能打到这些限制，`clone` 或 `unshare` 返回 ENOSPC。某个 namespace 即使没有活跃进程，也可能被打开的 `/proc/pid/ns/*` fd、bind mount、子 namespace 或 proc/mqueue mount pin 住，导致资源迟迟不释放。

user namespace 在高并发下还会碰到 UID/GID 映射、文件属主和 overlayfs 兼容性问题。rootless 容器大量创建时，subuid/subgid 分配、idmap mount、文件系统权限都会变成排查点。权限错误不一定在容器启动时暴露，可能在应用第一次写 volume 时才出现。

结合 LogServe，如果 worker 设计成每个 task 都新建一个完整容器或 namespace，高并发时启动成本会很高。更现实的方案是复用 worker 进程或 Pod，在应用层做 task 级隔离、超时、输出上限和资源限制；只有不可信任务才升级到更重的 sandbox。这样能避免把每个 workflow step 都变成一次容器冷启动。

面试里可以这样回答：

```text
namespace 高并发问题主要来自创建销毁和周边资源：netns 会放大 CNI、IPAM、veth、路由、iptables/eBPF、conntrack 成本；mount namespace 会放大 overlay、volume、subPath、unmount 成本；PID namespace 要处理 PID 1 回收和信号；namespace 可能被 fd 或 bind mount pin 住而不释放；数量上限打满会导致 clone/unshare 失败。短任务高并发时，盲目每任务一个容器会把启动和清理成本放大。
```

## Q040. namespace 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

namespace 的生命周期和进程生命周期紧密相关，但不是简单的“进程死了 namespace 就一定消失”。一般情况下，最后一个成员进程退出后 namespace 可以被回收；可如果还有打开的 namespace fd、bind mount、子 namespace、proc mount 或其他引用，它会继续存在。崩溃和强杀场景下，残留引用会让资源清理变得复杂。

PID namespace 最典型的问题是 1 号进程退出。容器里的 PID 1 退出，容器通常就结束；它的信号处理和子进程回收方式和普通进程不完全一样。应用把 shell 脚本当 entrypoint，又不 `exec` 真正进程，或者不处理 SIGTERM，滚动更新和超时终止时就可能出现退出慢、子进程没收干净、grace period 用完后被 SIGKILL 的情况。

network namespace 在重启时会暴露连接语义。Pod 重建通常意味着新的 network namespace、新的 veth、新的 Pod IP。旧连接会断，conntrack 和上游连接池可能还保留旧状态一段时间。应用层如果没有重试、deadline 和连接刷新，会把这类运行时重建看成随机网络错误。Service 能屏蔽 Pod IP 变化，但不能让旧 TCP 连接自动迁移。

mount namespace 在崩溃后要关注写入和卸载。容器进程被杀时，用户态缓冲、临时文件、emptyDir、projected volume、subPath mount 都可能处于中间状态。namespace 只决定视图，不提供文件系统事务。写入一致性还是要靠 fsync、rename、WAL、应用协议和存储系统。把 mount namespace 当成“崩溃隔离”，这个理解是错的。

user namespace 的边界在重启后也会出现。UID/GID 映射变了，旧文件属主、volume 权限、rootless 配置可能和新容器不匹配。尤其是持久卷里已经写了某个映射下的文件，新版本改了 `runAsUser` 或 user namespace 策略后，应用可能突然无权读写旧数据。

超时和重试会放大清理问题。一个任务超时后，如果只杀主进程，没有杀同 namespace 里的子进程或进程组，就可能留下后台进程继续写文件、占端口或持有锁。重试任务启动后看到的是同一个 volume 或外部系统，残留进程可能和新任务并发写，造成重复输出或状态破坏。namespace 本身不理解“这个业务任务已经超时”。

结合 LogServe，Python executor 如果用子进程隔离，超时时要杀进程组，必要时配合 PID namespace 或容器边界；但更重要的是任务 lease、epoch 和输出提交幂等。即使 namespace 清理慢，只要旧任务不能提交新 epoch 的结果，业务状态就不会被污染。namespace 清理负责资源，业务 fence 负责正确性。

面试里可以这样回答：

```text
namespace 在崩溃和重启时的边界是生命周期引用、PID 1 语义、网络连接断开、挂载清理和 UID/GID 映射。最后一个进程退出后 namespace 通常可回收，但 fd、bind mount、子 namespace 等引用会 pin 住它。Pod 重建会创建新 netns 和新连接语义，旧 TCP 不能迁移。超时重试时要杀进程组并清理残留，namespace 不懂业务 deadline，业务正确性还要靠 lease、epoch 和幂等提交。
```
## Q041. namespace 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

namespace 本身通常不是运行时热路径里的大开销。进程已经在某个 namespace 里运行后，很多访问就是查当前 task 的 namespace 指针，再进入对应资源视图。真正的性能瓶颈大多来自 namespace 周边资源，而不是“namespace 这个抽象”本身。

创建和销毁阶段可能消耗 CPU 和锁。`clone`、`unshare`、`setns`、容器运行时创建 sandbox、CNI 初始化、挂载 rootfs、写 cgroup、设置权限，这些操作需要内核和用户态协作。短任务高频启动时，CPU 时间和内核锁竞争会明显。你看到的可能是 kubelet、containerd、runc、CNI 插件 CPU 升高，而不是业务进程 CPU 升高。

network namespace 的瓶颈经常落在网络和内核数据结构上。Pod 网络要创建 veth、路由、邻居表、iptables/ipvs/eBPF 规则和 conntrack 状态。高连接数或高 Pod churn 会让 conntrack、iptables 更新、eBPF map 操作、CNI IPAM 成为瓶颈。服务延迟抖动时，根因可能是网络 namespace 对应的数据平面，而不是应用代码。

mount namespace 的瓶颈偏 I/O 和挂载管理。overlayfs 层多、镜像层大、volume 多、projected volume 多、subPath 多时，容器启动会变慢。镜像拉取、解压、目录创建、挂载、权限修正都可能占用磁盘 I/O。清理阶段 unmount 卡住，也会让 Pod 长时间 Terminating。

PID namespace 的瓶颈通常来自进程数量和 proc 相关操作。大量短进程创建退出，会带来进程表、PID 分配、子进程回收和 `/proc` 扫描成本。PID 1 不回收子进程时，zombie 堆积会让问题更明显。这里只靠 namespace 解决不了，要配 pids cgroup 和进程管理。

内存成本通常体现在每个 namespace 关联的内核对象、网络状态、mount table、进程结构和控制平面缓存上。单个 namespace 的内存不一定大，但大规模 Pod 和短生命周期任务会把小成本叠起来。Kubernetes 场景还要加上 apiserver、kubelet、runtime、CNI 的对象和状态。

所以回答瓶颈时不能只选一个。长期运行的容器，namespace 开销通常不在主路径；高频创建销毁时，CPU、锁和 I/O 会出现；网络密集场景里，network namespace 周边的数据平面和 conntrack 更常见；大量 volume 时，mount namespace 的挂载和存储 I/O 更明显。

面试里可以这样回答：

```text
namespace 本身通常不是热路径瓶颈。瓶颈来自周边资源：高频创建销毁时是 CPU、内核锁、runtime 和 CNI 开销；netns 场景常见瓶颈是 veth、路由、iptables/eBPF、conntrack 和网络数据面；mount namespace 常见瓶颈是 overlay、volume、挂载和磁盘 I/O；PID namespace 受进程创建、回收和 pids 限制影响。长期稳定运行的容器里，namespace 的直接开销通常很小。
```

## Q042. namespace 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

correctness test 要验证隔离语义是否正确。PID namespace 里，容器内只能看到自己的进程树，PID 1 语义正确，子进程能被回收。mount namespace 里，挂载点、只读 rootfs、volume 可见性和 mount propagation 符合预期。network namespace 里，端口空间隔离、路由、DNS、Service 访问和 NetworkPolicy 行为符合预期。user namespace 里，UID/GID 映射、文件权限、capabilities 作用域要测清楚。

correctness 还要测负向路径。容器不能看到宿主敏感进程，不能写只读根文件系统，不能访问未挂载目录，不能绑定不该绑定的宿主端口，不能用被 drop 的 capability 做内核操作。很多安全配置只测“应用能启动”是不够的，要测“越界操作确实失败”。

stress test 测高并发和异常。并发创建和销毁大量 namespace，观察失败率、残留进程、残留 veth、残留 mount、残留 IP、Terminating 卡住、CNI 超时、containerd/runc 错误。还要加上崩溃和强杀：创建一半杀 runtime，Pod 终止中杀主进程，网络初始化失败后重试，volume unmount 失败后恢复。目标是证明清理路径不会在压力下漏资源。

stress test 也要测资源上限。把 `/proc/sys/user/max_*_namespaces`、pids limit、节点 ephemeral storage、IPAM 地址池、conntrack 表压到接近上限，看系统是优雅拒绝、排队、告警，还是随机失败。对 Kubernetes 来说，还要观察 apiserver、kubelet、CNI、scheduler 和日志采集器是否被 Pod churn 拖垮。

benchmark 测具体成本，不要只测一个“容器启动时间”。可以拆成：`clone/unshare/setns` 成本、容器 runtime 创建成本、CNI add/delete 成本、mount/overlay 准备成本、Pod 从创建到 Ready 的 p50/p95/p99、网络直连延迟、Service 访问延迟、吞吐、conntrack 压力下延迟。这样才能知道瓶颈在内核、runtime、网络还是存储。

benchmark 还要区分冷启动和热启动。镜像已缓存和未缓存差别很大；CNI 预热、DNS 缓存、节点已有基础镜像、containerd snapshotter 类型都会影响结果。测试报告里要写明节点规格、内核版本、容器运行时、CNI、Kubernetes 版本、镜像大小、volume 类型和并发度。否则数字很难复现。

结合 LogServe，如果要验证 Python executor 的隔离，可以做三类测试：correctness 测任务不能读宿主路径、不能无限 fork、不能越过输出目录；stress 测大量 task 超时、崩溃、重试后没有残留进程和文件；benchmark 测每任务新 sandbox、复用 worker、同进程执行三种方案的延迟和吞吐差异。

面试里可以这样回答：

```text
correctness test 测隔离语义和越界失败：PID、mount、network、user namespace 是否按预期隔离。stress test 测高并发创建销毁、崩溃、强杀、CNI 失败、unmount 失败、资源上限和残留清理。benchmark 拆开测 clone/unshare/setns、runtime 创建、CNI add/delete、mount/overlay、Pod Ready 延迟、网络延迟和吞吐。测试要区分冷启动/热启动，并记录内核、runtime、CNI、镜像和并发度。
```

## Q043. 如果要求从零实现一个简化版 namespace，你会先定义哪些不变量？

**回答：**

从零实现简化版 namespace，第一步不是写 API，而是定义隔离对象和不变量。最核心的不变量是：每个进程在任意时刻对每类资源最多属于一个当前 namespace；进程对资源的解析必须经过它所属的 namespace；没有显式授权，进程不能观察或修改别的 namespace 里的对象。

第二个不变量是命名空间内唯一性和跨命名空间可重复。比如简化 PID namespace 里，PID 在同一个 namespace 内唯一；不同 namespace 可以出现相同 PID，但它们指向不同内部对象。简化 network namespace 里，端口绑定在同一个 namespace 内不能冲突；不同 namespace 可以都绑定 8080。这个不变量能解释 namespace 的主要价值。

第三个不变量是生命周期和引用计数。namespace 只要还有成员进程、打开句柄、子 namespace 或挂载引用，就不能被释放；最后一个引用消失后必须清理内部资源。释放必须幂等，重复清理不能造成 double free 或把别的 namespace 的资源删掉。崩溃恢复时也要能扫描并清理孤儿对象。

第四个不变量是权限检查。创建、加入、修改 namespace 都要经过 capability 或策略判断。不能让普通进程随意加入宿主 namespace，也不能让低权限进程通过创建 user namespace 绕过已有限制。权限要在 namespace 层和具体资源层都检查，比如加入 netns 后仍不能凭空获得管理网络设备的能力。

第五个不变量是父子和映射关系。PID namespace、user namespace 这类有层级或映射语义的对象，要定义父 namespace 如何看到子对象，子 namespace 如何映射 UID/GID，父级限制如何约束子级。这里最容易出安全洞。比如子 namespace 创建不应该逃过父级数量限制，UID 映射不应该越过允许范围。

第六个不变量是并发安全。创建、加入、退出、销毁、枚举资源可能并发发生。必须定义锁顺序、引用获取顺序和状态机。常见状态可以是 creating、active、dying、dead。进程加入 dying namespace 应该失败，销毁时要阻止新引用进入，同时等待旧引用退出。

如果做最小可用版本，我会先实现一个“命名表 + 成员进程 + 引用计数 + 权限检查”的框架，再实现 PID 或 network 里的一个小资源。不要一开始就试图实现所有 namespace 类型。每一种资源都有自己的细节，框架不变量先稳定，扩展才有基础。

面试里可以这样回答：

```text
简化版 namespace 先定义不变量：进程访问资源必须经过所属 namespace；同一 namespace 内名字唯一，跨 namespace 可重复；没有授权不能观察或修改别的 namespace；namespace 有引用计数和明确生命周期；创建、加入、销毁都要做权限检查；父子和 UID/GID 映射不能绕过父级限制；并发状态机要防止加入 dying namespace、重复释放和资源泄漏。先实现框架，再扩展具体资源类型。
```

## Q044. namespace 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

最常见的误用是把 namespace 当成完整安全边界。只要进了容器，就以为应用安全了，于是保留 root、privileged、`CAP_SYS_ADMIN`、hostPath、Docker socket、hostNetwork、hostPID。结果是容器可以操作宿主资源，攻击面几乎回到宿主机。线上症状可能是节点文件被改、容器能看见宿主进程、网络规则被改、其他 Pod 受到影响。

第二种误用是混淆 Kubernetes Namespace 和网络隔离。把服务放进不同 namespace，就以为互相不能访问。默认情况下，这个假设不成立。没有 NetworkPolicy 或网格授权，不同 namespace 的 Pod 仍可能通过 Service DNS 或 Pod IP 通信。症状是测试环境服务误连生产依赖，或者一个 namespace 的漏洞横向访问到另一个 namespace。

第三种误用是把 namespace 当资源限制。PID namespace 不限制进程数量，network namespace 不限制连接数，mount namespace 不限制磁盘写入，Kubernetes Namespace 也不自动限制 CPU/memory。没有 cgroup、ResourceQuota、LimitRange、ephemeral storage limit，资源滥用仍会影响节点。症状是 fork bomb、磁盘写满、conntrack 爆、节点内存压力和 Pod 被驱逐。

第四种误用是随意共享宿主 namespace。为了排障或省事开 `hostNetwork`、`hostPID`、`hostIPC`，结果端口冲突、进程可见、IPC 互通、安全边界变薄。hostNetwork 下 DNS 策略、端口绑定和 NetworkPolicy 行为都可能变化。症状是 Pod 调度到某些节点后端口冲突，或者策略明明配置了却没有按预期限制流量。

第五种误用是 PID 1 处理不当。容器里用 shell 脚本启动多个后台进程，不处理 SIGTERM，不回收子进程。滚动更新时 Pod 迟迟不退出，任务被 SIGKILL，僵尸进程堆积，日志最后一段丢失。这个问题看起来像 Kubernetes 不稳定，本质是容器进程模型没设计好。

第六种误用是 user namespace 配了一半。容器内 UID 映射和 volume 文件属主不匹配，开发环境能跑，生产挂 PVC 后写不了；或者 rootless 下某些挂载、网络、capability 行为和 rootful 不同，应用启动到一半失败。症状通常是 `permission denied`、只在特定节点失败、重启后才暴露。

结合 LogServe，危险误用是为了让 worker 访问模型或日志目录，把宿主项目根目录和 Docker socket 直接挂进去。这样 Python task 的边界就被打穿了。更好的做法是只挂必要的模型只读目录、任务工作目录和输出目录，并用 cgroup、超时和输出大小上限控制资源。

面试里可以这样回答：

```text
namespace 常见误用包括：把它当完整安全边界，混淆 Kubernetes Namespace 和网络隔离，把 namespace 当资源限制，随意开启 hostNetwork/hostPID/hostPath，PID 1 不处理信号和子进程，user namespace 与 volume 权限不匹配。线上症状是节点资源被影响、跨 namespace 误访问、fork bomb 或磁盘写满、端口冲突、Pod Terminating 卡住、permission denied 和容器逃逸风险增大。
```

## Q045. namespace 在单机和分布式环境中的语义有什么差异？

**回答：**

Linux namespace 的语义是单机内核语义。一个 PID namespace、network namespace 或 mount namespace 只存在于某台机器的内核里。它不能跨节点延伸，也不会让两个节点上的容器共享同一个内核对象。分布式系统里看到的“命名空间”，通常是控制平面或服务发现层的抽象，不是同一个 Linux namespace。

在单机上，namespace 直接影响系统调用看到的资源。进程调用 `getpid`、`mount`、`bind`、读 `/proc`、查看网络接口，结果都由它所在的 namespace 决定。你可以用 `nsenter` 进入某个进程的 namespace 排障，也可以通过 `/proc/pid/ns/*` 判断两个进程是否在同一个 namespace。这个判断只对本机有意义。

在 Kubernetes 里，每个 Pod 的 Linux namespace 仍然由所在 Node 创建。Pod A 在 node-1，Pod B 在 node-2，它们的 network namespace 是两个不同内核里的对象。Kubernetes 通过 CNI、Service、DNS、EndpointSlice、kube-proxy/eBPF 或云网络，把这些本地 namespace 接入一个集群网络。应用看到的是 Pod IP 和 Service，底层不是一个跨机器的巨大 network namespace。

Kubernetes Namespace 的语义才是集群级 API 语义。它作用在 apiserver 对象上，Deployment、Service、ConfigMap 等名字在 namespace 内唯一，RBAC 和 quota 可以按 namespace 配置。这个语义是分布式控制平面维护的，和节点内核 namespace 不同。删除一个 Kubernetes Namespace 会触发其中对象的级联清理，但不会直接等同于销毁某个 Linux namespace；真正的 Pod sandbox 清理仍发生在各节点 kubelet 和 runtime 上。

分布式环境还会多出传播延迟和最终一致性。Service selector 变化后，EndpointSlice 更新、kube-proxy/eBPF datapath 更新、DNS 缓存刷新都需要时间。一个 Pod 的 readiness 变了，不代表所有客户端立刻停止访问旧 endpoint。单机 namespace 的可见性变化通常是内核内同步；Kubernetes 命名和路由变化要经过控制平面和节点代理传播。

安全语义也不同。单机 Linux namespace 保护的是进程视图和宿主资源；分布式 Kubernetes Namespace 保护的是 API 操作作用域和资源组织。跨 namespace 网络隔离、服务身份、mTLS、审计和租户隔离要靠 NetworkPolicy、RBAC、Admission、ServiceAccount、网格或外部身份系统组合。只创建 namespace 不能声明“分布式安全隔离完成”。

结合 LogServe，单机实验里容器 namespace 能说明 worker 和宿主的运行边界；上 Kubernetes 后，还要说明 `logd`、`control`、`worker` 分布在不同 Pod 和节点时，服务发现、Endpoint 更新、Pod 重建、PVC 绑定和任务 lease 如何工作。Linux namespace 解决的是节点内隔离，LogServe 的 shared log 和 epoch 解决的是分布式语义里的状态正确性。

面试里可以这样回答：

```text
Linux namespace 是单机内核对象，只在某台 Node 上隔离进程看到的 PID、网络、挂载、用户等资源；它不跨节点。Kubernetes 通过 CNI、Service、DNS、EndpointSlice 和节点 datapath 把各节点上的 Pod 连接成集群网络。Kubernetes Namespace 是集群 API 资源作用域，负责名字、RBAC、quota 和对象组织。分布式环境还要考虑控制平面传播延迟、Endpoint 更新、DNS 缓存和策略下发，不能把单机 namespace 语义直接当成集群语义。
```
## Q046. cgroup 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

cgroup 的核心目标是把一组进程组织成层级，并对它们做资源统计、限制、优先级和控制。它解决的不是“进程看见什么”，而是“这组进程能用多少资源、用了多少资源、超过限制后怎么办”。CPU、memory、pids、I/O 这些控制器是容器资源隔离的基础。

它首先解决稳定性和资源治理问题。一个容器如果没有 memory limit，内存泄漏可能拖垮整台节点；没有 pids limit，fork bomb 可能耗尽进程表；没有 CPU 权重或 quota，某个 workload 可能压低同节点其他服务的可用 CPU。cgroup 让系统能把故障约束在一个进程组或 Pod 内，而不是任由它扩散。

正确性方面要分清层次。cgroup 不保证业务语义正确，不知道某个请求是否幂等，也不知道 workflow 是否超时。它能保证资源规则被执行，比如内存达到硬上限后触发 cgroup 内 OOM，CPU quota 达到后节流，pids 达到后 fork 失败。这是运行环境正确性，不是业务正确性。

性能方面，cgroup 既能保护系统，也可能伤害性能。CPU limit 会带来 throttling，延迟型服务在高峰时可能 p99 变差；memory limit 太紧会频繁回收或 OOM；I/O limit 太紧会让写 WAL、刷日志、加载模型变慢。cgroup 不是自动优化器，它只是执行资源策略。策略写错了，性能会更差。

安全方面，cgroup 提供的是资源滥用防护，属于 DoS containment。它不能阻止进程读不该读的文件，不能隐藏宿主资源，也不能替代 seccomp、capabilities、LSM 或 RBAC。它能限制一个恶意或失控进程最多消耗多少 CPU、内存、进程数和 I/O，这对多租户很重要，但不是完整安全模型。

可维护性方面，cgroup 的价值很大。Kubernetes 的 requests/limits、QoS、eviction、Pod 资源统计，最终都要落到节点和容器运行时的 cgroup 上。SRE 能通过 cgroup 指标知道哪个 Pod 触发了 OOM、CPU throttling、pids 耗尽或 I/O 压力。没有这些边界，排障只能看整机指标，定位会很粗。

结合 LogServe，`worker` 运行 Python task 和 LLM 调用时，cgroup 能限制 CPU、memory、pids 和临时文件 I/O 的影响范围。它不能保证 task 不重复提交结果，不能保证 actor 状态一致；这些要靠 shared log、lease、epoch 和幂等。cgroup 做资源边界，LogServe 的协议做业务边界。

面试里可以这样回答：

```text
cgroup 的核心目标是按层级组织进程，并对 CPU、内存、进程数、I/O 等资源做统计、限制和优先级控制。它主要解决资源稳定性、可维护性和 DoS 防护问题，也提供运行环境层面的规则正确性。它不隐藏资源视图，不保证业务正确性，也不是完整安全边界。Kubernetes 的 requests/limits、QoS 和资源指标最终都要落到节点 cgroup 上执行。
```

## Q047. cgroup 的典型适用场景和不适用场景分别是什么？

**回答：**

cgroup 的典型场景是容器资源隔离。每个 Pod 或容器有自己的 CPU、memory、pids、I/O 约束，节点上多个 workload 才能共存。没有 cgroup，一个低优先级批处理任务可能把 CPU 和内存吃光，影响控制面、日志采集器和在线服务。容器编排系统离不开 cgroup。

第二个场景是多租户和任务执行。CI job、在线判题、构建系统、数据处理任务、用户脚本执行器，都要限制单个任务能用多少资源。即使任务来自可信用户，也会有 bug。cgroup 可以让一个任务超限后被节流、OOM 或 fork 失败，而不是拖垮宿主机。

第三个场景是容量规划和可观测。cgroup 提供按进程组的 CPU 使用、内存使用、OOM、I/O、pids 等统计。SRE 可以按 Pod 或服务聚合这些指标，判断 request 是否过低、limit 是否过紧、某个版本是否引入内存泄漏、CPU throttling 是否导致尾延迟上升。没有 cgroup，很多指标只能到节点级别。

第四个场景是优先级和隔离策略。CPU weight 可以让不同服务在竞争时按权重分配；memory.high 可以让 cgroup 在达到硬上限前先进入回收和节流；pids.max 能限制进程数量；I/O 控制器能降低批任务对磁盘的影响。这些机制适合把在线服务、批任务、系统守护进程分层管理。

不适用场景也要说清楚。cgroup 不适合做访问控制。它不能决定进程能不能读某个文件、访问某个 socket、调用某个 syscall。它也不适合做网络 ACL；网络 egress/ingress 策略要看 NetworkPolicy、iptables/eBPF、service mesh 或防火墙。cgroup 可以参与流量整形或 BPF 分类，但这不是它最常见的容器资源语义。

cgroup 也不适合表达业务优先级的全部语义。比如 LogServe 里某个 workflow step 更紧急，cgroup 只能给它所在 worker 更多 CPU 或更少限制，不能理解 DAG 依赖、重试预算、任务 deadline、模型缓存命中率。业务调度器仍然要存在。

还有一个不适用点：cgroup 不能替代容量。给所有 Pod 都写 limit，不会让节点 magically 多出资源。request 写得太低，调度器会过度装箱；limit 写得太紧，运行时会节流或 OOM。cgroup 是执行边界，容量规划和负载控制仍然要做好。

面试里可以这样回答：

```text
cgroup 适合容器资源隔离、多租户任务执行、CI/批处理资源限制、按 Pod 的资源观测、CPU/memory/pids/I/O 优先级控制。不适合做文件权限、系统调用、网络访问控制或业务调度语义。它也不能替代容量规划，request 和 limit 配错会带来过度装箱、CPU throttling、OOM 和尾延迟问题。
```

## Q048. cgroup 和相近概念最容易混淆的边界在哪里？

**回答：**

第一条边界还是 cgroup 和 namespace。namespace 隔离视图，cgroup 限制资源。进程可以看不到宿主 PID，但仍然不受 CPU 限制；也可以被 CPU limit 限制，但仍然看见宿主网络。容器通常两者都需要：namespace 让它像在独立环境里运行，cgroup 防止它用光机器资源。

第二条边界是 cgroup 和 Kubernetes scheduler。scheduler 用 requests 判断 Pod 放在哪个 Node；kubelet 和 runtime 用 cgroup 执行 limit。request 是调度承诺，limit 是运行时约束。一个容器 request 500m、limit 2 CPU，调度时按 500m 放置，运行时最多可用到 2 CPU。把 request 当 limit，或者把 limit 当 scheduler 唯一依据，都会误解 Kubernetes 资源模型。

第三条边界是 cgroup 和 ResourceQuota/LimitRange。ResourceQuota 是 Kubernetes API 层的配额，限制某个 namespace 可以创建多少总 request、limit、对象数。LimitRange 可以给容器默认 request/limit 或限制范围。它们是准入和治理规则，不是内核执行机制。真正限制进程 CPU 和内存的仍然是节点 cgroup。

第四条边界是 cgroup 和 ulimit。ulimit 是进程资源限制，比如打开文件数、栈大小、最大进程数等，作用方式和继承规则不同。cgroup 管一组进程的聚合资源，适合 Pod/容器；ulimit 更像单进程或进程树级别的传统限制。两者可以叠加，但不要互相替代。

第五条边界是 cgroup 和优先级调度。CPU weight、cpu.max、cpuset 都能影响 CPU 分配，但它们不是业务调度器。内核不知道哪个请求是 VIP，也不知道哪个 workflow step 接近 deadline。应用层调度仍要按业务队列、deadline、租户、缓存命中和重试预算做决策。

第六条边界是 cgroup 和安全沙箱。cgroup 能限制资源滥用，不能阻止越权访问。一个进程即使用了很少 CPU，也可能通过过宽的 capability 改网络规则；一个容器即使 memory limit 很低，也可能读到错误挂载的 secret。安全要靠权限、系统调用、文件系统和身份控制一起做。

结合 LogServe，Kubernetes scheduler 决定 worker Pod 放在哪个节点，cgroup 限制 worker 运行时资源，LogServe control 决定 task 分给哪个 worker。三层都叫“调度”或“限制”会乱；面试里要把节点放置、资源执行、业务分配分开讲。

面试里可以这样回答：

```text
cgroup 容易和 namespace、Kubernetes scheduler、ResourceQuota/LimitRange、ulimit、业务调度器和安全沙箱混淆。namespace 管视图，cgroup 管资源；scheduler 用 request 做放置，cgroup 执行 limit；Quota/LimitRange 是 API 准入治理；ulimit 是传统进程限制；业务调度器理解 deadline 和优先级；安全沙箱负责权限和 syscall。cgroup 只是资源控制层。
```

## Q049. cgroup 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，cgroup 最常见的问题是策略和实际负载不匹配。CPU limit 太紧，很多容器同时到达 quota，内核开始 throttling。应用日志里可能没有错误，节点 CPU 也可能没有满，但请求 p99 明显上升。原因是单个 cgroup 被限住了，不能使用节点剩余 CPU。延迟型服务尤其容易踩这个坑。

内存 cgroup 的问题更直接。大量并发请求、批量加载模型、缓存升温、日志缓冲、临时对象堆积，都会把 memory usage 推到上限。达到 `memory.max` 一类硬限制后，内核会在 cgroup 内触发 OOM；达到较软边界时，可能先进入回收和节流。应用看到的是随机进程被杀、容器重启、请求中断，根因是并发峰值和 limit 不匹配。

pids cgroup 在高并发短进程场景很重要。每个请求 fork 一个进程、Python executor 并发启动子进程、shell 脚本没有收子进程，都可能打到 pids limit。好处是它防止宿主机进程表被耗尽；坏处是应用没处理 fork 失败时，会出现奇怪的 `resource temporarily unavailable`、任务启动失败或部分请求失败。

I/O 控制器和存储也会暴露问题。多个容器同时写 WAL、日志、临时文件、模型缓存，节点磁盘队列会上升。cgroup 能做一定 I/O 权重或限制，但 Kubernetes 默认对块 I/O 的表达没有 CPU/memory 那么常用。很多系统只配了 CPU/memory limit，结果高并发时真正瓶颈在磁盘和 ephemeral storage。

cgroup 层级和 churn 也会带来开销。大量短生命周期 Pod 创建销毁，kubelet、runtime、systemd 或 cgroupfs 要频繁创建目录、写控制文件、迁移进程、读取统计。metrics agent 高频读取 cgroup 指标，也可能给节点带来额外开销。规模大时，监控本身要控制采样频率和标签基数。

另一个隐藏问题是指标解释。容器 CPU 使用率低，不代表没有被 throttling；节点内存还有空闲，不代表某个 cgroup 不会 OOM；Pod 被 OOMKilled，不代表节点内存一定耗尽。高并发排障要看 cgroup 维度的 throttled periods、OOM events、memory.current、pids.current、I/O 延迟，而不能只看整机指标。

结合 LogServe，高并发 workflow 会让 worker 同时跑多个 Python task 或模型调用。CPU limit 太紧会让任务超时，超时又触发重试，重试再放大负载。memory limit 太紧会杀掉 worker，导致 lease 过期和任务重放。这里要把 cgroup 指标和 LogServe 的任务重试、deadline、epoch 指标放在一起看。

面试里可以这样回答：

```text
cgroup 高并发隐藏问题包括 CPU quota throttling 导致 p99 上升、memory limit 触发 cgroup OOM、pids limit 导致 fork 失败、I/O 和 ephemeral storage 成为真实瓶颈、大量 Pod churn 带来 cgroup 创建销毁和指标采集开销。排障不能只看节点 CPU/内存，要看 cgroup 级 throttling、OOM、memory.current、pids.current、I/O 延迟和应用重试放大。
```

## Q050. cgroup 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

cgroup 在崩溃场景下最先暴露的是 OOM 语义。内存超限后，内核会在 cgroup 内选择进程杀掉。被杀的可能是主进程，也可能是子进程，取决于 OOM 选择和配置。Kubernetes 看到容器主进程退出后，会把状态标成 OOMKilled 并按 restartPolicy 处理。应用层如果只看到连接断开或任务失败，要能把它和资源超限关联起来。

重启后，cgroup 统计通常会重置或换成新的容器 cgroup。旧容器的 CPU、内存、I/O 计数不一定还在当前路径里。监控系统要及时 scrape 或保留历史，否则重启后的现场会丢。很多 OOM 问题排查慢，就是因为只看当前容器状态，没看 last state、事件和历史 cgroup 指标。

超时场景下，cgroup 不理解业务 deadline。CPU throttling 或内存回收可能让任务跑慢，应用 deadline 到了以后开始重试，但旧任务可能还在 cgroup 里继续运行，继续占资源。除非应用取消上下文、杀进程组或 kubelet 终止容器，否则 cgroup 只是限制资源，不会替你结束业务任务。

重试会放大资源问题。一次请求因为 CPU throttling 超时，上游重试三次，worker 同时处理旧请求和新请求，cgroup 更容易到达 CPU 或内存上限。这个反馈环会把小问题放成大故障。SRE 需要把 timeout、retry、concurrency limit 和 cgroup limit 一起设计，不能只在资源层设硬上限。

pids 和子进程清理也是边界。任务超时只杀父进程，子进程留在同一个 cgroup 里继续跑，就会造成资源泄漏。容器退出时 runtime 通常会清理 cgroup 内进程，但如果应用长期运行、只取消某个 task，就要由应用自己管理进程组、session 或 executor 池。cgroup 可以帮你发现 pids.current 异常，但不会理解哪个子进程属于哪个业务任务。

节点重启或 kubelet 重启时，cgroup 恢复还涉及 runtime 和 systemd。kubelet 要重新发现已有容器，清理死亡容器，恢复统计。短时间内指标可能缺口，Pod 状态也可能滞后。对强依赖资源统计的控制器，要能接受这些抖动。

结合 LogServe，worker OOM 后任务不能直接判定成功或失败，要靠 shared log 里的提交记录和 lease/epoch 判断。旧 worker 被杀，新的 worker 重放任务时，cgroup 只保证旧进程资源释放；是否重复执行、是否重复提交结果，靠 LogServe 的幂等和 fencing 设计。资源层和业务层要各自负责自己的边界。

面试里可以这样回答：

```text
cgroup 在崩溃和重启场景下暴露 OOM 选择、统计重置、last state 丢失、子进程残留和 kubelet/runtime 恢复问题。超时和重试时，cgroup 不知道业务 deadline，只会节流或限制资源；旧任务可能继续占资源，重试会放大负载。应用要主动取消、杀进程组、限制并发，并用幂等和 fencing 处理重放。监控要保留 OOM、throttling、pids 和 last state 历史。
```
## Q051. cgroup 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

cgroup 的瓶颈取决于你启用了哪个控制器，以及 workload 的形态。CPU 控制器的典型瓶颈是 throttling。`cpu.max` 或 Kubernetes CPU limit 设得太低时，容器在一个调度周期内用完 quota，就会被节流。节点整体 CPU 可能还有空闲，但这个 cgroup 不能继续跑。对低延迟服务来说，这会直接反映到 p99 和超时率。

内存控制器的瓶颈来自回收和 OOM。达到较软边界时，分配路径可能进入 direct reclaim，应用线程自己花时间回收内存；达到硬限制且无法回收时，触发 cgroup 内 OOM。内存问题不只是“够不够”，还包括 page cache、匿名内存、socket buffer、临时对象、语言运行时 GC。Go、JVM、Python 在容器内都要关注运行时对 cgroup memory limit 的感知。

I/O 瓶颈常被低估。WAL、日志、模型加载、checkpoint、临时文件、容器日志都可能竞争节点磁盘。cgroup I/O 控制器能做权重或限制，但在 Kubernetes 日常使用里，I/O 隔离往往没有 CPU/memory 那么完整。最终症状是 fsync 慢、日志写慢、容器启动慢、PVC 响应慢，而不是 CPU 或内存指标异常。

锁竞争更多出现在高 churn 和指标采集场景。频繁创建销毁 cgroup、迁移进程、启停短生命周期容器、监控系统高频读取大量 cgroup 文件，都会增加内核和用户态管理开销。大规模节点上，cgroup 层级太深、对象太多、scrape 频率太高，会让控制面和节点代理变慢。

网络不是传统 cgroup 的主要控制面，但也不能完全排除。网络带宽控制常常通过 tc、eBPF、CNI 或 QoS 策略实现，和 cgroup 可以有关联但不是 Kubernetes CPU/memory 那样的标准路径。网络延迟问题更多来自 CNI、conntrack、Service datapath、网格代理和应用连接池，不要一看到 Pod 有 limit 就把网络问题归因给 cgroup。

综合看，在线服务最常见的是 CPU throttling 和内存回收/OOM；存储型服务常见 I/O；大规模短任务和监控密集场景会看到锁竞争和管理开销；网络问题通常要先查网络栈和代理。面试里能把这些条件说出来，比简单回答“CPU 或内存”更可靠。

结合 LogServe，`logd` 更容易受 I/O 和 fsync 影响，`worker` 更容易受 CPU、memory 和 pids 影响，模型缓存和 checkpoint 会涉及磁盘和内存双重压力。不同组件的 cgroup 指标要分开看，不能用一套 limit 套所有 Pod。

面试里可以这样回答：

```text
cgroup 瓶颈按控制器和 workload 看：CPU limit 会导致 throttling 和 p99 上升；memory limit 会触发 reclaim、GC 压力和 cgroup OOM；I/O 控制不足会让 WAL、日志、checkpoint、模型加载变慢；高频创建销毁和高频 scrape 会带来管理开销和锁竞争。网络通常不是 cgroup 的主瓶颈，更多要查 CNI、conntrack、Service datapath 和代理。
```

## Q052. cgroup 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

correctness test 要先验证限制是否真的生效。CPU limit 下，进程不能长期超过 quota；memory limit 下，超过硬上限会在 cgroup 内触发 OOM 或分配失败；pids limit 下，fork 到上限后失败而不是拖垮宿主；I/O 限制下，吞吐或权重符合预期。还要验证层级约束：子 cgroup 的使用不能突破父 cgroup 预算。

correctness 也要测统计准确性。CPU usage、throttled periods、memory.current、memory.peak、oom events、pids.current、io.stat 这些指标应该和实际负载大致吻合。监控系统依赖这些指标做告警和容量分析，如果读错路径、混了 cgroup v1/v2 或容器重启后关联错 cgroup，结论会错。

stress test 测极端负载和异常恢复。并发启动很多容器，快速创建销毁 cgroup；让一批任务同时冲 CPU、同时分配内存、同时 fork、同时写磁盘；在高压下杀容器、杀 kubelet、重启 runtime、节点 drain。观察是否出现资源泄漏、残留 cgroup、进程未清理、指标丢失、OOM 风暴、Pod 状态不一致。

stress test 还要测混部。在线服务和批任务放同一节点，批任务 CPU 打满、内存接近上限、写大量日志，看在线服务的 p99、错误率和 throttling 是否仍在预算内。cgroup 的价值就是混部时限制损害范围，只测单个容器跑满没有太大意义。

benchmark 要拆维度。CPU benchmark 测不同 quota、不同并发、不同 period 下的吞吐和 p99；memory benchmark 测工作集大小、GC、page cache、reclaim、OOM 前后的行为；I/O benchmark 测 fsync、顺序写、随机读写、日志写入和 checkpoint；pids benchmark 测短进程创建吞吐和上限行为。还要记录节点内核、cgroup v1/v2、runtime、Kubernetes 版本和 QoS class。

对 Kubernetes，要单独测 request/limit 配置效果。request 不等于 limit，Guaranteed、Burstable、BestEffort 的 QoS 行为不同，节点压力下的 eviction 顺序也不同。测试要覆盖低 request 高 limit、无 limit、request=limit、sidecar 无 request 等常见配置，否则上线后调度和运行时表现会和预期不一致。

结合 LogServe，可以为 worker 做资源测试矩阵：不同 task 并发、不同 CPU limit、不同 memory limit、不同输出大小和不同模型 cache 命中率。看 task latency、timeout、retry、OOMKilled、CPU throttling、checkpoint fetch latency。这样才能知道是业务调度问题，还是 cgroup 策略太紧。

面试里可以这样回答：

```text
correctness test 测 CPU、memory、pids、I/O 限制和层级约束是否生效，指标是否读准。stress test 测大量 cgroup 创建销毁、CPU/内存/fork/I/O 冲击、runtime/kubelet 重启、节点 drain、混部场景和资源泄漏。benchmark 拆成 CPU quota 对吞吐/p99 的影响、memory reclaim/OOM、I/O 延迟、pids 创建吞吐，并记录 cgroup v1/v2、runtime、QoS、request/limit 配置。
```

## Q053. 如果要求从零实现一个简化版 cgroup，你会先定义哪些不变量？

**回答：**

简化版 cgroup 的第一个不变量是树结构。每个 cgroup 除根之外有且只有一个父节点，不能形成环；进程属于某个叶子或允许的内部节点；删除 cgroup 前必须保证没有活跃进程和子节点，或者定义明确的迁移/清理规则。树结构不稳，资源层级就无法推导。

第二个不变量是资源记账不能丢。进程加入、迁出、退出，资源使用都要正确 charge 和 uncharge。CPU 时间、内存页、进程数、I/O 统计不能变成负数，也不能因为并发退出和迁移重复扣减。资源统计可以有采样误差，但硬限制相关的计数必须保守正确。

第三个不变量是层级限制。子 cgroup 的资源使用必须计入父 cgroup；父级限制不能被子级拆分绕过。比如父 cgroup memory limit 是 1GiB，下面开十个子 cgroup，不代表总共能用 10GiB。pids、memory、I/O 这类资源都要考虑父子传播。

第四个不变量是限制动作明确。CPU 超限是节流还是降权，memory 超限是回收、阻塞、返回错误还是 OOM kill，pids 超限是 fork 失败，I/O 超限是排队或限速。每个控制器都要定义超过限制后的动作，不能让调用方猜。动作还要可观测，至少能记录事件和计数。

第五个不变量是并发安全。资源 charge、limit 更新、进程迁移、cgroup 删除可能同时发生。要定义锁顺序和状态机，比如 active、freezing、dying、dead。更新 limit 时要处理当前使用量已经超过新 limit 的情况：是拒绝更新，还是先设置新限制再回收/杀进程。Linux cgroup v2 的一些接口就很重视这种语义。

第六个不变量是权限和委派。谁能创建子 cgroup，谁能移动进程，谁能改 limit，谁能读统计，要有明确规则。多租户环境里，如果允许用户管理自己的子树，必须保证他们不能越权修改父级控制器，也不能通过创建子树逃过父级预算。

第七个不变量是可观测性。没有指标和事件，cgroup 只会变成“进程莫名变慢或被杀”。简化实现至少要暴露 current、max、events、throttled、oom、pids 等状态，方便上层调度器和运维系统判断资源策略是否合理。

面试里可以这样回答：

```text
简化版 cgroup 先定义树结构、进程归属、资源 charge/uncharge、层级限制、超限动作、并发状态机、权限委派和可观测指标。不变量包括：树不能成环；子级使用计入父级；资源计数不为负不重复扣；删除前无活跃引用；limit 更新和进程迁移并发安全；用户不能通过子 cgroup 绕过父级限制；超限后节流、回收、OOM 或失败的动作明确且可观测。
```

## Q054. cgroup 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

最常见的误用是给延迟敏感服务设置过低 CPU limit。很多人以为 limit 是“保护服务”，结果服务高峰时被 throttling。节点 CPU 还有空闲，Pod 却到 quota 不能继续运行，p99 上升、请求超时、重试增加。对在线服务，有时只设置合理 request、不设置 CPU limit，反而比硬限 CPU 更稳，前提是集群有容量治理和隔离策略。

第二种误用是 memory limit 按平均值配置。内存峰值、GC、page cache、批量请求、模型加载都可能超过平均值。limit 太紧时，容器随机 OOMKilled，应用看起来像不稳定。更糟的是 OOM 后上游重试，新的 Pod 冷启动，又占更多内存，形成重启循环。

第三种误用是忘记 pids limit。容器有 PID namespace 但没有 pids cgroup，脚本 bug 或 fork bomb 仍可能耗尽宿主进程表。症状是节点上新进程创建失败，健康检查、ssh、日志 agent 都受影响。pids limit 不需要太紧，但应该有上限。

第四种误用是 sidecar 不配资源。主容器 request/limit 配好了，日志 sidecar、mesh proxy、exporter 没配或配太低。调度器低估 Pod 实际资源，运行时 sidecar 抢主容器 CPU/memory，或者代理被 OOM 导致业务流量断。服务网格场景尤其要给 proxy 资源预算。

第五种误用是 request 写得太低。调度器按 request 装箱，request 低于真实长期使用量，会把太多 Pod 放到同一节点。平时看着没问题，高峰时节点压力、eviction、CPU 争抢一起出现。limit 只能阻止单个容器超过上限，不能修正错误装箱带来的整体容量不足。

第六种误用是把 cgroup 当业务隔离。比如一个 worker 里跑多个租户任务，只给 worker Pod 一个 cgroup，却没有 task 级并发、取消、输出上限和公平队列。结果一个任务用光 worker 的内存，影响同 Pod 其他任务。cgroup 边界在 Pod 或容器层，业务内多租户还要应用自己做隔离。

结合 LogServe，worker 的 CPU/memory limit 要和单个 task 并发、Python executor 进程数、模型 cache 大小一起设计。`logd` 如果写 WAL 或 append-only log，不能只看 CPU，要看 PVC I/O 和 fsync 延迟。把所有组件套一个默认 100m/128Mi，很容易得到一堆看似随机的超时和 OOM。

面试里可以这样回答：

```text
cgroup 常见误用包括：给在线服务 CPU limit 太低导致 throttling；memory limit 按平均值配导致 OOMKilled；忘记 pids limit 导致 fork bomb；sidecar 或 mesh proxy 不配资源；request 低估导致节点过度装箱；把 Pod 级 cgroup 当成业务租户隔离。线上症状是 p99 上升、超时重试、重启循环、节点压力、代理 OOM、任务互相影响和排障时节点指标看似正常但 Pod 已被限住。
```

## Q055. cgroup 在单机和分布式环境中的语义有什么差异？

**回答：**

cgroup 的执行语义是单机内核语义。某个进程组能用多少 CPU、内存、pids、I/O，由它所在节点的内核和 cgroup 层级执行。它不能跨节点聚合执行，也不能保证“整个服务在全集群最多用多少 CPU”。跨节点的资源治理是 Kubernetes scheduler、ResourceQuota、HPA/VPA、Cluster Autoscaler、业务限流和容量管理共同完成的。

在单机上，cgroup 直接影响进程运行。CPU quota 到了就节流，memory limit 到了就回收或 OOM，pids 到了 fork 失败。这些动作和业务副本数无关，也不需要控制平面参与。即使 apiserver 挂了，已运行容器的 cgroup limit 仍然由节点内核继续执行。

在 Kubernetes 里，Pod 被调度到某个节点后，kubelet 和容器运行时创建对应 cgroup。request 参与调度和 QoS，limit 变成 cgroup 约束。不同节点上的同一 Deployment 副本，各自由所在节点执行 cgroup。某个节点 CPU 空闲，不会自动借给另一个节点上被 CPU limit throttling 的 Pod。

分布式环境还多了控制平面语义。ResourceQuota 限制 namespace 可以提交多少资源请求，LimitRange 提供默认值或范围，HPA 根据指标调副本数，VPA 建议或调整 request，Cluster Autoscaler 加节点。它们看起来都在“管资源”，但都不是 cgroup 的单机硬执行。最终每个容器的运行时上限仍要落到本节点。

分布式系统的故障语义也不同。一个 Pod 因 cgroup OOM 在 node-1 重启，Deployment 会在同节点或其他节点拉起替代副本；Service 会通过 Endpoint 更新把流量转走；上游会看到连接断开或超时。cgroup 只负责触发资源边界，集群负责重建和路由，业务负责重试和幂等。

多租户场景里，单机 cgroup 只能约束节点内资源。全局公平性需要更高层调度。例如一个租户在十个节点各跑一个 Pod，每个 Pod 都没超 cgroup，但总量可能超过租户预算；这要靠 namespace quota、队列配额、作业调度器或账单系统控制。反过来，ResourceQuota 通过了，也不代表单个节点一定有足够资源运行 Pod。

结合 LogServe，单机实验里 worker cgroup 限制可以证明资源边界；上 Kubernetes 后，还要证明 scheduler request 合理、worker 副本分布合理、HPA 或手动扩缩容不会让 shared log、object store、metadata store 成为瓶颈。cgroup 是节点局部约束，LogServe 的全局吞吐还取决于调度、存储和重试控制。

面试里可以这样回答：

```text
cgroup 是单机内核执行机制，只限制所在节点上的进程组资源。Kubernetes 把 Pod 调度到节点后，由 kubelet/runtime 创建 cgroup 执行 limit；request、ResourceQuota、LimitRange、HPA、VPA、Cluster Autoscaler 是分布式控制面或容量治理机制。一个节点的空闲 CPU 不会借给另一个节点上被 throttling 的 Pod。全局租户预算和业务吞吐要靠更高层调度、配额和限流完成。
```
## Q056. readiness probe 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

readiness probe 的核心目标是告诉 Kubernetes：这个 Pod 现在能不能接收业务流量。它不负责重启容器，也不负责判断进程是否活着。readiness 失败时，Pod 会被标记为 unready，Service 对应的 EndpointSlice 会把它从可服务 endpoint 中移除或标记为 not ready，新的流量不应该再转给它。

它首先解决的是可用性和发布正确性问题。一个进程启动了，不代表它已经加载完配置、连上依赖、完成 warmup、建立缓存、跑完迁移兼容检查。没有 readiness，Deployment 可能在应用真正可用前就把流量打进来，用户看到连接失败或 500。readiness 把“容器进程已启动”和“服务可接流量”拆开。

readiness 也影响滚动发布。Deployment 更新时，新的 Pod 只有 Ready 后才算可用，旧 Pod 才能按策略继续缩减。readiness 设计好，可以避免新版本还没准备好就替换旧版本；设计错了，会导致 rollout 卡住，或者更糟，提前把坏版本放进流量路径。

性能方面，readiness 间接保护系统。应用过载时可以主动返回 not ready，把新流量从自己身上摘掉，配合限流和负载均衡让实例恢复。不过它不是细粒度负载均衡器，也不应该每次队列稍微长一点就抖动。频繁 Ready/Unready 会造成 endpoint churn、连接重建和上游重试，反而更差。

安全方面，readiness 作用有限。它可以避免未初始化的实例接流量，比如证书没加载、密钥没准备好、授权缓存没同步。但它不是安全控制面。访问控制仍然要靠认证、授权、NetworkPolicy、mTLS 和业务策略。

可维护性方面，readiness 很有价值。它把服务状态显式暴露给平台，SRE 可以从 `READY`、Events、EndpointSlice、rollout status 看出为什么流量没进某个 Pod。一个好的 readiness endpoint 应该解释“我能否接请求”，而不是把所有内部状态都塞进去。

结合 LogServe，`control` Ready 应该表示它能接收调度请求、连接 metadata/shared log，并且必要的后台循环已经启动。`logd` Ready 应该表示 append 路径可写、已有 log segment 可访问。`worker` Ready 应该表示能接任务、executor 池可用、必要模型或 fallback 路径准备好。它不应该因为某个可选模型冷启动就永久 unready。

面试里可以这样回答：

```text
readiness probe 的目标是判断 Pod 是否能接业务流量。失败时 Kubernetes 不重启容器，而是把 Pod 标记为 unready，并从 Service 可服务 endpoint 中移除。它主要解决可用性、滚动发布和运行状态可维护性问题，也能间接做过载摘流。它不等于 liveness，不负责重启，也不是安全控制面或细粒度负载均衡器。
```

## Q057. readiness probe 的典型适用场景和不适用场景分别是什么？

**回答：**

典型场景之一是启动准备。应用进程已经启动，但还在加载配置、建立数据库连接池、恢复本地缓存、加载模型、执行轻量自检。这段时间 liveness 可以通过，readiness 应该失败，避免流量过早进入。启动慢的应用还可以配 startup probe，避免 liveness 过早杀掉它。

第二个场景是依赖或本地能力缺失。比如服务必须写本地 WAL，磁盘只读了；必须连接 metadata store，连接池完全不可用；必须加载证书，证书还没准备好。这些会让服务无法正确处理请求，可以让 readiness 失败。但这里要谨慎：如果每个实例都因为同一个下游短暂抖动而 unready，可能把整个服务从流量里摘空。

第三个场景是优雅下线。Pod 收到终止信号后，可以先让 readiness 失败，停止接新请求，再等待连接 draining 和正在处理的请求完成。Kubernetes 的 terminationGracePeriod、preStop hook、应用 shutdown 流程要配合起来。readiness 不是唯一的 draining 机制，但它是很重要的信号。

第四个场景是过载保护。实例本地队列过长、线程池耗尽、事件循环严重滞后、关键 executor 不可用时，可以短暂返回 unready，让新流量转给其他副本。这个方案要有滞后和恢复阈值，避免一会儿 Ready 一会儿 Unready。很多系统会把 readiness 和应用内限流结合，而不是直接把每次压力波动暴露给 Kubernetes。

不适用场景包括深度依赖巡检。readiness 不应该每秒去查所有下游数据库、对象存储、第三方 API、消息队列、模型服务。这样做会制造额外负载，也会把外部依赖的短暂抖动放大成全服务摘流。更合理的是检查本实例处理请求所需的本地关键条件，同时让真正请求路径处理依赖失败和降级。

另一个不适用场景是触发重启。readiness 失败不会重启容器。如果应用死锁、事件循环卡死、主进程无法响应，应该由 liveness 或外部 watchdog 处理。把 readiness 当重启按钮，结果是 Pod 一直 Running 但不接流量，问题可能被隐藏很久。

也不要用 readiness 表达业务功能开关。某个可选功能下游挂了，不代表整个服务不能接所有请求。把可选依赖放进 readiness，会让服务过度脆弱。可以在业务层返回降级结果、按路由限流，或者暴露单独的功能健康指标。

结合 LogServe，worker readiness 可以检查执行队列是否接收新任务、executor 池是否还有容量、基础配置是否可用；不应该每次 readiness 都拉一次模型源或访问所有对象存储路径。模型缺失可以让特定任务走冷加载或拒绝该模型任务，不一定让整个 worker unready。

面试里可以这样回答：

```text
readiness 适合启动准备、关键本地能力缺失、优雅下线、实例过载摘流和滚动发布保护。不适合做全链路深度巡检、第三方依赖心跳、重启触发器或业务功能开关。探针应该回答“这个实例现在能不能接新流量”，检查要轻量、局部、稳定，并配合 shutdown、限流和降级策略。
```

## Q058. readiness probe 和相近概念最容易混淆的边界在哪里？

**回答：**

最容易混淆的是 readiness 和 liveness。readiness 管接不接流量，liveness 管要不要重启。readiness 失败时容器继续运行，只是从 Service endpoint 里摘掉；liveness 失败时 kubelet 会按 restartPolicy 重启容器。把临时下游抖动放进 liveness，会造成重启风暴；把死锁只放进 readiness，会让坏实例一直挂着。

第二个边界是 readiness 和 startup probe。startup probe 管启动期保护。应用启动很慢时，可以让 startup probe 先接管，成功后 liveness/readiness 再正常工作。没有 startup probe，很多人会把 liveness initialDelaySeconds 设得很大，导致真实死锁发现很慢。readiness 可以在启动期失败，但它不应该替代 startup probe 对慢启动的保护。

第三个边界是 readiness 和应用健康接口。很多团队写一个 `/health`，同时给 readiness、liveness、外部 LB、监控使用。这样容易混。更好的做法是拆成 `/ready`、`/live`、`/startup` 或带不同检查级别的 endpoint。`/ready` 可以检查接流量条件，`/live` 只检查进程是否需要重启，监控可以有更深的 diagnostic endpoint。

第四个边界是 readiness 和负载均衡。readiness 是二值信号，不是权重系统。Ready 的 Pod 都可能被 Service 转发，Kubernetes 不会因为某个 Pod 只剩 20% 容量就自动给它少分流，除非你引入额外负载均衡或服务网格策略。需要按负载权重调流量时，应该在应用层、网格、Gateway 或专用负载均衡器做。

第五个边界是 readiness 和 PDB/rollout。PDB 依赖 Pod readiness 判断可用 Pod 数，Deployment rollout 也依赖 readiness。readiness 过于严格，会阻塞 drain 和发布；过于宽松，会让坏 Pod 被算作可用。这里的 ready 不只是一个健康检查结果，它会影响运维动作。

第六个边界是 readiness 和业务依赖。是否检查数据库、对象存储、消息队列，要看这个实例能否在依赖异常时仍提供部分能力。如果所有实例都依赖同一个数据库，把数据库短暂抖动放入 readiness，可能让所有 Pod 同时 unready，导致 Service 没 endpoint。更稳的做法是本地快速检查加业务请求中的超时、熔断、降级。

结合 LogServe，`logd` 的 `/ready` 可以检查 append 路径和当前 segment 状态；`/live` 只检查主循环没有死锁；深度检查，比如扫描所有历史 segment 或验证对象存储，可以放到诊断接口或后台任务。不要把一个重操作探针挂到 kubelet 每几秒调用一次。

面试里可以这样回答：

```text
readiness 和 liveness 的边界是接流量还是重启；readiness 和 startup 的边界是运行期接流量还是启动期保护；readiness 和负载均衡的边界是二值摘流还是按权重分流；readiness 和监控诊断的边界是轻量接流量判断还是深度巡检。readiness 还会影响 Deployment、EndpointSlice 和 PDB，所以过严会阻塞发布，过松会放坏流量。
```

## Q059. readiness probe 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，readiness 最大的问题是抖动。实例在压力下偶尔超时，readiness 失败，被摘出流量；剩余实例承受更多流量，也开始失败；过一会儿前一个实例恢复 Ready，又被打满。这个 Ready/Unready 循环会造成 endpoint churn、连接重建、上游重试和负载振荡。

第二个问题是探针本身参与竞争。readiness handler 如果和业务请求共用线程池、连接池、锁或事件循环，高峰时它可能排在业务请求后面超时。Kubelet 看到探针失败，把 Pod 摘流；但真正的问题不是服务不能工作，而是健康检查没有独立快速返回能力。Node 上很多 Pod 同时探测，也会形成额外流量。

第三个问题是全局依赖检查放大故障。如果 readiness 每次都查数据库、Redis、对象存储、模型服务，高并发时探针请求和业务请求一起打下游。下游稍慢，探针失败，Pod unready，上游重试，更多实例查同一个下游。这个反馈环会把一个下游抖动放大成全服务不可用。

第四个问题是 endpoint 更新和连接 draining 不同步。readiness 失败后，EndpointSlice 更新、kube-proxy/eBPF、网格代理、客户端连接池都需要时间感知。已经建立的连接可能继续发请求，客户端也可能缓存旧 endpoint。应用不能假设返回 unready 后立刻没有流量进来。优雅下线时仍要处理一段过渡流量。

第五个问题是发布时容量误判。滚动升级时，新 Pod readiness 通过得太早，但缓存未预热、JIT 未完成、连接池还很小，刚接流量就慢；或者 readiness 太慢，Deployment 认为新副本不可用，发布卡住。高并发系统要把 readiness 和 warmup、minReadySeconds、maxUnavailable、maxSurge 一起调。

第六个问题是探针参数太激进。`periodSeconds` 太短、`timeoutSeconds` 太短、`failureThreshold` 太低，会把短暂 GC、CPU throttling、网络抖动都当成不可用。参数太宽又会让坏实例继续接流量。合适的参数要根据应用 p99、GC pause、节点压力和期望摘流时间设置。

结合 LogServe，worker 的 readiness 如果检查队列长度，不能队列稍长就失败，否则调度器会把任务转给其他 worker，引发抖动。可以用高水位/低水位、最小不 Ready 时间、并发上限和控制面负载反馈。`logd` readiness 也不要每次 fsync 一个测试记录，这会把探针变成写路径负载。

面试里可以这样回答：

```text
readiness 高并发问题包括 Ready/Unready 抖动、探针和业务请求竞争线程池或连接池、深度依赖检查放大下游故障、EndpointSlice 和连接 draining 存在传播延迟、发布时缓存未预热却过早 Ready、探针参数太激进导致误摘流。解决思路是轻量探针、独立快速路径、高低水位、失败阈值、warmup、限流和业务降级配合。
```

## Q060. readiness probe 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

崩溃后，readiness 的语义取决于容器是否还在运行。主进程退出时，Pod 会进入重启流程，readiness 自然失败；但如果只是某个后台线程、连接池、executor 池或子进程崩了，主进程还活着，readiness handler 必须能反映关键能力是否丢失。否则 Pod 看起来 Ready，实际请求进来就失败。

重启时，readiness 要处理初始化窗口。新容器启动后，依赖连接、缓存恢复、leader 状态、WAL replay、模型加载都可能还没完成。readiness 应该在这些关键步骤完成前失败。这里要避免两种极端：过早 Ready 导致错误流量；过晚 Ready 导致发布慢、容量不足。startup probe、readiness 和应用 warmup 要配套。

超时场景下，要区分探针超时和业务超时。readiness handler 自己必须很快，最好只读本地状态，不做长阻塞调用。如果 handler 因为锁竞争或下游阻塞超时，kubelet 会把实例摘掉。探针超时本身也是一种信号，但它应该说明本实例的事件循环或关键状态访问已经异常，而不是说明外部依赖慢。

重试场景下，readiness 会影响流量转移，但不会取消旧请求。Pod 变 unready 后，上游新请求可能转向其他实例，旧请求仍在原实例执行。上游如果同时重试，原实例和新实例可能并发处理同一个业务操作。业务层必须有幂等 key、lease、epoch 或去重，不能指望 readiness 保证“摘流后旧请求不存在”。

终止场景也很关键。Pod 收到 SIGTERM 后，应用通常先切 readiness 为 false，然后停止接新请求，等待 inflight 完成，最后退出。preStop hook、terminationGracePeriodSeconds、负载均衡器健康检查周期、连接池 keepalive 都要一起考虑。如果 grace period 太短，readiness 还没传播完，容器就被 SIGKILL，用户仍会看到断连。

依赖恢复场景容易产生反复。数据库短暂不可用，Pod readiness 失败；数据库恢复后，所有 Pod 同时 Ready，大量请求和缓存重建一起打回来。可以加恢复抖动、warmup、限流和连接池逐步放量。readiness 只是门，不是流量坡度控制器。

结合 LogServe，worker 重启后不能立刻 Ready，至少要恢复 worker identity、连接 control/logd、准备 executor 池，并明确旧任务 lease 如何处理。`logd` 重启后要先完成 segment 恢复和 append 位置确认。readiness 通过只说明能接新流量，不说明历史任务已经完全处理完；任务恢复仍以 shared log 为准。

面试里可以这样回答：

```text
readiness 在崩溃重启时要反映关键能力是否恢复，而不只是主进程是否活着。启动期要等配置、连接、replay、warmup 完成后再 Ready；探针自身要轻量，避免因下游慢而超时；Pod unready 不会取消旧请求，重试仍要靠业务幂等；终止时要先置 unready、drain inflight，再退出，并考虑 Endpoint 传播和连接池缓存延迟。
```
## Q061. readiness probe 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

readiness probe 的理想实现应该几乎没有性能瓶颈。它应该读内存里的状态位、最近一次后台检查结果、队列长度或本地组件状态，然后快速返回。真正出问题时，瓶颈通常不是探针协议，而是探针做了不该做的重操作。

CPU 瓶颈通常来自 handler 和业务共用事件循环或线程池。服务高峰时，业务请求把 CPU 打满，readiness handler 排队，kubelet 等不到响应就判失败。Node CPU 还可能因为 CPU limit throttling 让探针超时。探针超时看起来像服务不可用，但根因可能是 CPU 预算不足或 handler 没有独立轻量路径。

内存瓶颈比较少见，但也会出现。readiness 如果每次构造大对象、扫描大 map、读取大配置、序列化复杂 JSON，在高频探测下会制造 GC 压力。很多语言的健康检查 endpoint 写得随意，平时没问题，Pod 数量和探测频率上来后就变成一批小而密的分配。

锁竞争是 readiness 常见隐患。handler 为了判断状态去拿全局锁，而业务请求、配置 reload、连接池刷新、任务调度也拿同一把锁。高峰时探针卡在锁上，返回超时。更好的做法是把健康状态写入原子变量或只读快照，readiness 不进入复杂临界区。

I/O 瓶颈来自错误的检查方式。每次 readiness 都读磁盘、fsync、访问 PVC、列目录、检查日志 segment、打开证书文件，都会把 kubelet 的周期性探针变成稳定 I/O 负载。对存储型服务，可以由后台线程定期检查 I/O 状态并缓存结果，探针只读缓存。

网络瓶颈最常见于深度依赖检查。readiness 每次都打数据库、Redis、对象存储、下游 HTTP，会引入网络延迟、连接池竞争和下游压力。下游慢时，本来业务也许可以降级，readiness 却直接失败，把实例摘掉。除非某个依赖是所有请求的绝对前置条件，并且检查非常轻量，否则不要把网络调用放在高频 readiness 路径里。

结合 LogServe，`logd` readiness 不应该每次写真实 log record；可以由后台维护 `appendPathHealthy` 和 `lastAppendError`。`worker` readiness 不应该同步加载模型；可以看 executor 池是否可接任务、队列高水位和最近一次 control 心跳。这样探针成本固定，不随模型大小和下游状态波动。

面试里可以这样回答：

```text
readiness probe 理想上只读本地轻量状态。性能瓶颈通常来自误设计：CPU 被业务请求或 CPU limit 抢占，handler 超时；每次分配大对象引发 GC；探针拿全局锁造成锁竞争；每次读盘或 fsync 带来 I/O；每次访问数据库/对象存储造成网络和下游压力。设计上应使用原子状态、后台检查缓存、短超时和独立快速路径。
```

## Q062. readiness probe 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

correctness test 要测状态转换。应用启动前应 not ready，初始化完成后 Ready；关键本地能力失效后 not ready，恢复后 Ready；收到终止信号后应尽快 not ready；readiness 失败不应触发容器重启。还要确认 Service EndpointSlice 里的 ready 状态和 Pod readiness 一致。

correctness 还要测边界条件。依赖短暂失败时是否按设计摘流，后台检查结果过期时是否保守返回 not ready，配置 reload 中是否避免半初始化状态 Ready，队列超过高水位是否不接新流量，低于低水位后是否恢复。不能只测 HTTP 200/500，要测状态机。

stress test 测高并发和抖动。让业务请求打满线程池、CPU 接近 limit、GC pause、下游变慢、队列堆积，同时让 kubelet 周期性探测。观察 readiness 是否误失败、是否频繁 Ready/Unready、EndpointSlice 是否大量 churn、上游是否出现重试风暴。还要模拟滚动发布、节点 drain 和 Pod 终止，看摘流和 draining 是否顺序正确。

stress test 还应该测全局依赖故障。比如数据库慢 30 秒，所有 Pod 的 readiness 会不会同时失败；对象存储抖动时，worker 会不会全部 unready；恢复后是否所有实例同时 Ready 并打爆下游。这里不是为了让 readiness 永远成功，而是验证系统是否会把局部故障放大。

benchmark 要测探针成本和传播时间。探针 handler 的 p50/p95/p99 延迟、分配次数、CPU 消耗、锁等待、I/O 次数都要看。还要测状态变化到 EndpointSlice 更新、到 kube-proxy/eBPF/mesh 生效、到客户端停止访问旧 endpoint 的时间。readiness 的真实效果不是 handler 返回 503 那一刻结束的。

发布 benchmark 也有价值。新 Pod 从容器启动到 Ready 的时间分布，Ready 后第一批请求的延迟，缓存 warmup 完成时间，minReadySeconds 对可用容量的影响，都能帮助设置 rollout 参数。没有这些数据，`initialDelaySeconds` 和 `failureThreshold` 经常是拍脑袋。

结合 LogServe，可以做测试：断开 logd PVC 或让 append 失败，`logd` 是否 not ready；worker 队列到高水位，是否停止接新任务；control 收到 SIGTERM，是否先 not ready 再 drain；大量 workflow 并发时 readiness 是否因锁竞争误失败。benchmark 则看 readiness handler 在高负载下是否仍是毫秒级、零或低分配。

面试里可以这样回答：

```text
correctness test 测启动、恢复、故障、终止、EndpointSlice 状态和“readiness 失败不重启”这些语义。stress test 测高并发、CPU throttling、GC、下游慢、队列堆积、滚动发布、drain 和全局依赖故障下是否抖动或摘空。benchmark 测 handler 延迟、分配、锁等待、I/O、状态到 endpoint 生效的传播时间，以及 Pod 启动到 Ready 和 Ready 后首批请求延迟。
```

## Q063. 如果要求从零实现一个简化版 readiness probe，你会先定义哪些不变量？

**回答：**

简化版 readiness probe 先定义状态机。状态至少包括 starting、ready、not_ready、draining、stopped。任何时刻只能处于一个明确状态，状态转换要有原因和时间戳。比如 starting 只能在初始化完成后进入 ready；ready 可以因为本地关键能力失败进入 not_ready；收到终止信号后进入 draining，draining 不再回到 ready，除非你明确支持取消终止。

第二个不变量是探针只回答接新流量能力。它不负责重启，不负责深度诊断，不负责业务功能矩阵。返回 ready 的含义是“可以接收符合当前路由的普通请求”；返回 not ready 的含义是“不要再给我新请求”。正在处理的旧请求不受这个返回值自动影响。

第三个不变量是检查路径轻量且有界。readiness handler 不能无限等待锁、网络、磁盘或下游。所有检查要有短超时，最好只读本地原子状态或后台检查缓存。状态缓存要有 TTL，过期后按设计选择保守失败或降级成功，不能返回陈旧成功还不留痕迹。

第四个不变量是滞后和防抖。一次短暂失败不一定立即摘流，一次短暂成功也不一定立即恢复。可以定义 `failureThreshold`、`successThreshold`、高低水位和最小状态保持时间。没有防抖，高并发场景下 readiness 会变成振荡源。

第五个不变量是和 shutdown 协调。进入 draining 后，应立即让 readiness 失败，停止接受新请求，等待 inflight 完成或超时，然后退出。grace period 结束时必须能强制终止。探针状态、连接关闭、队列停止接收、后台任务取消要在同一个 shutdown 状态机里。

第六个不变量是可观测。每次状态变化要记录原因，比如 `init_not_done`、`queue_high_watermark`、`append_path_unhealthy`、`draining`。只暴露 200/503 很难排查。指标要有当前状态、状态持续时间、失败原因、探针延迟、状态切换次数。

如果把它接到 Kubernetes，还要定义外部契约：HTTP 200 表示 ready，非 2xx/3xx 或超时表示失败；探针端口要在容器内稳定监听；Endpoint 更新有传播延迟，应用不能把 readiness 当作立即停止所有流量的保证。

面试里可以这样回答：

```text
简化版 readiness 先定义状态机：starting、ready、not_ready、draining、stopped。核心不变量是只回答能否接新流量；检查路径轻量有界；状态有 TTL 和失败原因；用阈值和高低水位防抖；进入 draining 后不接新请求并等待 inflight；readiness 失败不触发重启；状态变化和探针延迟可观测。Kubernetes 下还要承认 endpoint 传播延迟。
```

## Q064. readiness probe 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

第一种误用是把 readiness 做成全依赖巡检。每次探针都查数据库、Redis、对象存储、消息队列、第三方 API。下游稍慢，所有 Pod 同时 unready，Service 没有可用 endpoint；上游开始重试，下游更慢。线上症状是故障范围被 readiness 放大，原本只是依赖抖动，最后变成整个服务不可访问。

第二种误用是把 readiness 和 liveness 复用同一个深度 `/health`。某个可恢复依赖失败后，readiness 失败是合理的，liveness 失败却会重启容器。结果是下游抖动时应用集体重启，缓存丢失，连接池重建，故障更重。反过来，如果死锁只让 readiness 失败，容器不重启，坏实例长期挂着。

第三种误用是探针太重。readiness handler 读大文件、扫目录、跑 SQL、拿全局锁、做 fsync、加载模型。平时看不出来，高峰时探针超时，Pod 被摘流。线上症状是 READY 列频繁变化，Events 里大量 probe timeout，但应用日志不一定有业务错误。

第四种误用是永远返回成功。为了让发布不卡，有人把 `/ready` 写成只要进程活着就 200。结果新 Pod 还没初始化就接流量，滚动发布把坏版本推进线上；终止时也不摘流，用户请求被打到马上退出的 Pod。症状是发布期间 5xx、连接重置、冷启动延迟飙升。

第五种误用是参数拍脑袋。`timeoutSeconds: 1`、`periodSeconds: 1`、`failureThreshold: 1` 对很多 JVM、Go GC、高负载节点都太激进；`initialDelaySeconds` 设很大又会掩盖真实不可用。参数应该基于探针 p99、启动时间、warmup、期望摘流速度和故障恢复策略。

第六种误用是把 readiness 当流量权重。队列稍长就 unready，队列稍短就 ready，没有高低水位和最小保持时间。流量在副本间来回摆，所有实例都不稳定。更好的做法是应用内限流、队列上限、负载均衡权重或网格策略，readiness 只做粗粒度摘流。

结合 LogServe，错误做法是 worker `/ready` 每次都检查所有模型 checkpoint 是否在本地，或者 logd `/ready` 每次都写入真实 shared log。前者会让模型冷启动变成 Pod 不可用，后者会把探针混入业务事实来源。应改成本地状态位、后台检查和明确失败原因。

面试里可以这样回答：

```text
readiness 常见误用包括全依赖巡检、和 liveness 复用深度健康检查、探针太重、永远返回成功、参数太激进或太宽、把 readiness 当流量权重。症状是全服务被摘空、重启风暴、READY 频繁抖动、发布时 5xx、连接重置、冷启动延迟上升和故障被探针放大。正确做法是轻量、本地、稳定、可观测，并和 liveness/startup 分开。
```

## Q065. readiness probe 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，readiness 通常只是一个进程内状态判断。比如本地进程返回 `/ready=200`，调用方马上可以按这个结果决定是否发请求。传播路径短，参与者少。你可以把它理解成本地负载均衡或进程管理器的一项健康状态。

Kubernetes 里，readiness 是分布式控制信号。kubelet 周期性探测容器，把结果写成 Pod condition；EndpointSlice controller 或相关控制循环根据 Pod readiness 更新 endpoint；节点上的 kube-proxy、eBPF datapath、网格代理、Ingress/Gateway、客户端连接池再逐步感知变化。这个过程不是瞬时的。

因此，Pod 返回 unready 后，短时间内仍可能收到请求。原因包括旧 Endpoint 还没传播到所有节点、客户端复用长连接、网格代理配置还没更新、外部负载均衡器健康检查周期更长。应用要设计成 unready 后停止接新任务，但仍能处理一段过渡流量或优雅拒绝。

Ready 也不是全局容量保证。一个 Pod Ready，只表示 kubelet 认为它可接流量，不表示所有上游都已经把流量分给它，也不表示缓存已经完全热、连接池已经达到稳态。发布系统要配合 minReadySeconds、warmup、流量逐步放量和监控，不能只看 READY 变成 1/1 就认为容量恢复。

分布式环境里，readiness 还影响其他控制器。Deployment rollout、PDB、HPA 间接指标、Service endpoint、Gateway 后端可用性都可能受它影响。单机健康检查失败最多影响本进程；Kubernetes readiness 抖动会造成 endpoint churn，并把压力传给其他副本。

多集群或服务网格下，传播路径更长。一个集群内 Pod unready，跨集群服务发现、全局负载均衡、DNS、网格控制面可能都有缓存。SRE 需要知道每一层的健康检查来源和刷新周期，否则会看到“Pod 已经 unready，但外部流量还在进来”的现象。

结合 LogServe，单机运行时 `worker` 是否接任务可以由 control 直接感知；Kubernetes 里 `worker` readiness 只影响 Service 流量，不一定等同于 LogServe control 的任务调度视图。control 最好维护自己的 worker heartbeat、lease 和 capacity view，把 Kubernetes readiness 当基础信号之一，而不是唯一调度依据。

面试里可以这样回答：

```text
单机 readiness 通常是本进程能否接请求的本地判断；Kubernetes readiness 是分布式信号，要经过 kubelet、Pod condition、EndpointSlice、节点 datapath、网格或负载均衡器传播。unready 后仍可能有旧连接和缓存流量，Ready 后也不代表全局容量马上恢复。它还会影响 Deployment、PDB 和 Service endpoint，所以需要考虑传播延迟、draining、warmup 和控制器联动。
```
## Q066. liveness probe 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

liveness probe 的核心目标是判断容器是否已经进入无法自我恢复的坏状态，需要 kubelet 重启。它回答的问题不是“能不能接新流量”，而是“这个进程继续留着是否没有意义”。如果 liveness 连续失败，kubelet 会杀掉容器并按 Pod 的 restartPolicy 拉起新实例。

它主要解决可用性和可维护性问题。典型坏状态包括进程死锁、主事件循环卡住、内部 worker 全部永久阻塞、关键后台线程退出后无法恢复、服务端口不再响应。没有 liveness，这类进程可能一直 Running，但永远不处理请求。liveness 给平台一个自动恢复入口。

正确性方面要非常谨慎。重启只能恢复进程状态，不能修复业务语义。请求是否重复执行、任务是否幂等、写入是否提交、lease 是否过期、旧实例是否还能继续写，这些都不是 liveness 能解决的。liveness 触发重启后，应用还要靠持久化日志、事务、幂等 key、epoch 和恢复逻辑保证业务状态正确。

性能方面，liveness 不是性能优化工具。进程慢、下游慢、CPU throttling、GC pause、数据库超时，不一定应该重启。把性能波动写进 liveness，最容易造成重启风暴。重启会丢缓存、断连接、触发冷启动，还可能让负载转移到其他副本，整体性能更差。

安全方面，liveness 基本不提供安全隔离。它可以在进程异常时重启，减少某些失控状态的持续时间，但不能阻止攻击、越权访问或数据泄漏。安全仍靠最小权限、网络策略、认证授权、seccomp/LSM、审计和密钥管理。

可维护性方面，liveness 的价值是把“假活”实例暴露出来。SRE 可以从重启次数、Events、probe failure 日志判断进程是否经常卡死。一个好的 liveness endpoint 应该非常保守，只在进程确实需要重启时失败。宁愿用 readiness 摘流，也不要用 liveness 重启所有短暂不健康。

结合 LogServe，`worker` liveness 可以检查主进程事件循环、executor 管理线程或本地 RPC server 是否还能响应；不要因为某个 task 超时、某个模型下载慢、logd 短暂不可达就让 liveness 失败。`logd` liveness 可以检查主循环没有死锁，但 append 路径短暂不可写更适合 readiness 或错误告警，是否重启要看它是否能通过内部恢复处理。

面试里可以这样回答：

```text
liveness probe 判断容器是否进入不可自愈的坏状态，需要 kubelet 重启。它解决假活、死锁、事件循环卡死、关键线程永久失效这类可用性和可维护性问题。它不负责接流量，那是 readiness；不负责慢启动，那是 startup；也不保证业务正确性。liveness 失败应非常保守，否则性能抖动或下游故障会被放大成重启风暴。
```

## Q067. liveness probe 的典型适用场景和不适用场景分别是什么？

**回答：**

典型适用场景是进程假活。进程还在，端口还占着，但内部主循环已经卡死；HTTP server accept 不了新连接；gRPC server 线程池永久阻塞；后台调度线程 panic 后没有恢复；内部队列锁死，所有请求都卡住。这些状态下，继续等待通常没有价值，重启能恢复到干净状态。

第二个场景是不可恢复的内部错误。应用发现核心状态机坏了、必须重建内存状态；某个必要后台组件退出且无法重新启动；事件循环检测到长时间没有 tick；内部 watchdog 判断所有执行器都无响应。这些可以让 liveness 失败，交给 kubelet 重启。

第三个场景是进程级健康。HTTP liveness 可以返回固定轻量响应，TCP liveness 可以检查端口是否能建立连接，gRPC liveness 可以用健康检查协议。检查越简单越好。liveness 不是给你做全链路诊断的地方，它应该证明“进程主体还活着并能调度基本工作”。

不适用场景包括外部依赖失败。数据库挂了、对象存储慢了、下游服务 500 了，不应该直接让 liveness 失败。重启本服务不会修复下游，反而会制造更多连接重建和缓存丢失。外部依赖影响接流量时，用 readiness、熔断、降级、限流和告警处理。

慢启动也不适合 liveness 直接处理。应用启动要加载大模型、恢复 WAL、跑缓存预热，应该用 startup probe 或足够合理的启动保护。没有 startup probe 时，liveness 过早检查会在应用还没准备好时把它杀掉，形成 CrashLoopBackOff。

业务错误不适合 liveness。某类请求持续失败、某个租户配置错误、某个 task 超时，不代表整个进程需要重启。把业务失败接到 liveness，会让单个输入触发容器重启，造成可用性和安全风险。业务错误应该在请求路径、队列、任务状态和告警里处理。

结合 LogServe，单个 workflow step 执行失败、某个 Python task 超时、某个 worker 没有模型缓存，都不该触发 liveness。worker 主循环卡死、无法响应 control heartbeat、本地 executor 管理器永久死锁，才是 liveness 的候选。这样才能避免任务级错误升级成 Pod 级重启。

面试里可以这样回答：

```text
liveness 适合检测进程假活、死锁、主事件循环卡住、关键后台组件不可恢复、端口无法响应这类重启能解决的问题。不适合检测外部依赖失败、业务请求失败、慢启动、临时过载、队列积压或某个任务超时。外部依赖和过载用 readiness/降级/限流，慢启动用 startup probe，业务失败用业务状态和幂等恢复。
```

## Q068. liveness probe 和相近概念最容易混淆的边界在哪里？

**回答：**

liveness 和 readiness 的边界最重要。liveness 失败会重启容器，readiness 失败会摘流。实例还能恢复但暂时不能接新请求时，用 readiness；实例已经卡死或内部状态坏到无法恢复时，用 liveness。下游数据库慢了，通常不是 liveness；应用线程池永久死锁，才可能是 liveness。

liveness 和 startup probe 也常混。startup probe 是启动保护，成功前 liveness 和 readiness 的某些检查不应该过早杀进程。慢启动应用如果没有 startup probe，liveness initialDelaySeconds 往往被设得很大，结果启动后真实死锁也要等很久才发现。更清晰的做法是 startup 管启动窗口，liveness 管运行期假活。

liveness 和进程管理器也要分清。Kubernetes 可以重启容器，但应用内部仍然应该管理自己的 goroutine、线程、子进程和连接池。不能把所有内部错误都丢给 kubelet。频繁重启会损失现场，也可能隐藏内存泄漏、死锁和资源泄漏的根因。liveness 是最后一道自动恢复，不是正常控制流。

liveness 和 watchdog 有交集。应用内部 watchdog 可以检测事件循环 tick、worker heartbeat、锁等待时间，然后更新一个本地健康状态；liveness probe 读取这个状态。这样比在 probe handler 里做复杂检查更稳。边界是：watchdog 负责判断内部是否卡死，liveness 负责把这个判断交给 kubelet 执行重启。

liveness 和监控告警也不同。监控可以做深度检查、趋势分析、错误率告警、依赖健康分析；liveness 只能做非常小心的重启触发。很多应该报警的事情不应该重启，比如错误率升高、下游慢、缓存命中率低、磁盘快满。把告警条件写进 liveness 会把可诊断问题变成重启噪声。

liveness 和资源限制也不要混。CPU throttling 或 memory pressure 可能导致 liveness 超时，但重启不一定解决，甚至会更糟。资源压力应该通过 requests/limits、扩容、限流、负载削峰处理。只有资源压力导致进程进入不可恢复假活，才考虑 liveness 失败。

结合 LogServe，可以让内部 watchdog 记录 worker 主循环最后 tick、executor 池 heartbeat、control heartbeat。liveness endpoint 只读这些本地时间戳和状态。如果 logd 短暂慢，worker 应该 readiness 降低或业务重试；如果 worker 主循环 5 分钟没有 tick，liveness 再失败更合理。

面试里可以这样回答：

```text
liveness 和 readiness 的边界是重启还是摘流；和 startup 的边界是运行期假活还是启动期保护；和 watchdog 的边界是内部判断还是交给 kubelet 执行重启；和监控的边界是自动恢复触发还是诊断告警。liveness 不应承载外部依赖、业务错误、慢启动和资源压力的全部判断，否则会把可恢复问题变成重启风暴。
```

## Q069. liveness probe 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，liveness 最大的风险是假失败。业务流量把 CPU、线程池、事件循环或连接池打满，liveness handler 没有独立执行能力，探针超时。kubelet 认为容器不活，重启它。重启后缓存没了、连接断了、队列重放了，其他副本承担更多流量，也开始探针超时，最后变成重启风暴。

第二个风险是 CPU throttling。Pod 设置了较低 CPU limit，高峰时容器频繁被节流。liveness 周期到了，但进程拿不到足够 CPU 响应探针，连续失败后被重启。节点整体 CPU 可能没满，SRE 如果只看节点指标会很困惑。排查时要看 container CPU throttled periods 和 probe timeout 时间是否重合。

第三个风险是探针和业务共用锁。liveness 为了检查状态去拿全局锁，而高并发请求正持有这把锁做慢操作。探针卡住后失败。更糟的是，锁竞争本来只是局部慢，liveness 重启会中断所有 inflight 请求。liveness 的检查路径应该尽量无锁或只读原子快照。

第四个风险是依赖检查误入 liveness。高并发下数据库、Redis、对象存储本来就更慢，如果 liveness 也查这些依赖，就会在下游压力最大时触发上游重启。重启不会修复下游，只会放大连接风暴和冷启动。外部依赖应该从 liveness 里拿掉。

第五个风险是参数和规模不匹配。Pod 数很多时，kubelet 会周期性发大量探针；`periodSeconds` 太短、`timeoutSeconds` 太低、`failureThreshold` 太小，会让短暂 GC、STW、网络抖动、节点压力变成重启。高并发系统要按真实 p99、GC、CPU limit、节点负载和恢复目标设置参数。

第六个风险是重启后的恢复负载。容器重启会导致连接池重建、缓存冷启动、模型重新加载、WAL replay、leader election 或 worker 重新注册。这些动作本身消耗资源。在高并发时，多个实例因为 liveness 同时重启，会把系统推到更坏状态。需要配合 PDB、rollout 策略、重启告警、退避和过载保护。

结合 LogServe，worker 高并发执行任务时，liveness 不能和任务执行池共用同一条拥塞路径。可以让 liveness 只读一个由主循环定期更新的 atomic heartbeat。如果任务执行慢但主循环还在调度、还能取消任务和响应 control，就不要重启 worker；如果主循环停止 tick、executor 管理器无法回收任务，再让 liveness 失败。

面试里可以这样回答：

```text
liveness 高并发隐藏问题包括业务流量挤占探针导致假失败、CPU limit throttling 造成 probe timeout、探针拿全局锁被请求阻塞、把数据库等依赖检查放进 liveness、参数太激进、多个实例同时重启带来冷启动和连接风暴。设计上要让 liveness 路径轻量、局部、少锁，不查外部依赖，用 startup/readiness 分担启动和摘流语义，并监控 throttling 与 probe failure 的相关性。
```