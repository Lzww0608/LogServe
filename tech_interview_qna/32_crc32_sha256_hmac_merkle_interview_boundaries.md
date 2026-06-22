# 32. CRC32、SHA-256、HMAC 与 Merkle Tree 追问链

这组问题不按百科定义铺开，而按面试官最容易追问的方向组织：一句话定义哪里会误导，事故通常从哪里发生，指标怎样设计才看得见长尾，正确性和性能边界分别在哪里。

这四个概念都和“完整性”有关，但完整性不是一个单词能盖住的东西。CRC32 更像可靠性路径里的误码检测；SHA-256 是无密钥的密码学摘要；HMAC 是带共享密钥的消息认证；Merkle Tree 是把大对象或日志集合拆成可局部证明的哈希结构。面试时最忌讳把它们都说成“hash”。

## Q001. 面试官如果只问一个问题检验你是否理解 CRC32，可能会问什么？

我觉得最有效的问题是：如果给每条日志 record 加 CRC32，它能防什么，防不了什么；重启恢复时遇到 CRC mismatch，你会怎么处理？

这个问题能一次看出三件事。第一，你是否知道 CRC32 的目标是发现偶发损坏，比如写入中断、磁盘页读错、网络传输 bit flip、文件尾部半条 record。第二，你是否知道 CRC32 不能防恶意篡改，因为它没有密钥，攻击者能改 payload 就能重新算 CRC。第三，你是否能把算法放回工程流程里，而不是只背“循环冗余校验”。

好的回答会先说清楚覆盖范围。CRC32 应该覆盖哪些 bytes，必须写进格式：record header 是否包括在内，length 是否包括在内，type、version、stream id、payload、padding、compression flag 是否包括在内。只要写入端和读取端对覆盖范围理解不一致，校验值就会变成事故源。CRC32 不是单独一个数字，它和编码格式绑定。

然后讲恢复策略。对 append-only log 来说，如果读取 segment 尾部时遇到 CRC mismatch，常见处理不是继续 replay，也不是立刻把整块盘判死刑，而是停止在最后一条校验通过的 record，截断或隔离损坏尾部，再上报恢复事件。因为尾部损坏很可能来自上次崩溃时的半写入。若 mismatch 出现在 segment 中间，风险更高，要区分介质损坏、并发写坏、程序 bug、版本升级覆盖范围不一致，不能静默跳过。

结合 LogServe，如果 shared log record 将来加 CRC32C，我会把它定义为可靠性校验：用于启动恢复时快速发现半条 record、损坏 payload 或错误 length。它不能证明 record 是 control 写的，也不能证明 worker completion 没被伪造。跨信任边界的完整性要用 HMAC、AEAD tag 或签名。这个边界说清楚，面试官通常就知道你没把 CRC32 当安全哈希。

面试里可以这样收束：CRC32 是便宜、成熟、适合流式处理的误码检测工具。它的价值在“坏了尽早发现”，不是“可信”。如果我只给日志加 CRC32，我解决的是崩溃和介质损坏的可检测性；如果我要防伪造，还得引入带密钥的认证机制。

## Q002. CRC32 的一句话定义是否容易误导，误导点在哪里？

容易误导。最常见的一句话是“CRC32 是一个 32 位 hash/checksum”。这句话不算完全错，但它太粗，会把人带到三个错误方向。

第一个误导是把 CRC32 当成普通哈希。CRC32 的数学本质是 GF(2) 多项式除法的余数。它的生成多项式决定了对哪些错误模式敏感，比如单 bit 错误、短 burst error、低权重错误。哈希表里说的 hash 更关注分布和桶均匀性；密码学 hash 关注原像、第二原像和碰撞阻力。CRC32 的设计目标不是这两者。

第二个误导是把 32 位输出当成唯一身份。32 bit 空间只有 `2^32` 种结果，对单次随机损坏检测很有用，但对海量对象去重或唯一 ID 太短。几十万、上百万对象里出现同 CRC 并不稀奇。把 CRC32 当文件全局 fingerprint 或对象唯一 key，迟早会撞到边界。

第三个误导是把“校验通过”理解成“安全”。CRC32 没有 secret。任何人拿到数据，都能算出匹配的 CRC。它能发现自然噪声，却挡不住主动改数据的人。安全完整性至少需要 HMAC、AEAD tag 或数字签名，因为这些机制把攻击者不知道的密钥或私钥放进了验证条件。

还有一个很实际的误导：`CRC32` 这个名字并不唯一。IEEE CRC32、CRC32C/Castagnoli、Koopman 多项式、不同 init、refin、refout、xorout 都会影响结果。Go 标准库里的多项式常量使用 LSB-first 的反射表示，硬件指令里叫 `CRC32` 的也常常实际对应 CRC32C。面试里只说“我用了 CRC32”，不如说清楚 variant、参数和测试向量。

更稳的一句话是：CRC32 是一种 32 位循环冗余校验码，用固定多项式对输入 bytes 做 GF(2) 运算，主要用于低成本发现传输或存储中的偶发损坏，不提供安全认证，也不适合做唯一身份。

## Q003. CRC32 最常见的生产事故触发条件是什么？

我见过最常见的触发条件不是“CRC32 算法写错”，而是两端算的不是同一串 bytes。覆盖范围、编码、字节序、压缩位置、版本字段、final xor、反射参数，只要有一项在写入端和读取端不一致，线上就会出现校验失败。

典型事故是格式演进。原来 CRC 只覆盖 payload，后来 header 里加了 flags 或 compression type，写入端把 header 纳入 CRC，老读取端仍按旧规则算。或者 record 长度字段从 little-endian 改成 big-endian，校验字段本身的位置变化了，但某个 SDK 还按旧 layout 拼 bytes。结果不是数据坏了，而是协议定义变了却没有版本隔离。

第二类事故是 CRC32 和 CRC32C 混用。名字很像，结果完全不同。x86 SSE4.2 的 `crc32` 指令容易让人误以为可以加速所有 CRC32，实际常用于 Castagnoli 方向的 CRC32C。你拿它去验证 gzip/PNG/IEEE CRC32，当然对不上。这个坑在“为了性能替换实现”的时候很容易出现。

第三类事故是边写边算和落盘顺序没设计好。应用先写 payload，再写 CRC，崩溃发生在中间；或者先写 header length，payload 只写了一半。重启恢复时如果没有明确“遇到尾部 mismatch 就截断”的规则，系统可能反复启动失败，或者更糟，跳过错误继续 replay，制造错误状态。

第四类事故来自可变数据。代码对一个 buffer 算完 CRC 后，又复用了同一个 buffer 改了几个字段；或者压缩前算 CRC，读取端解压后算 CRC；或者 JSON 序列化字段顺序不稳定，写入端和验证端得到不同 bytes。这些都不是 CRC32 的数学问题，是 bytes 边界没有被固定。

排查时我会按这个顺序走：先保存原始 bytes 和期望 CRC，确认算法 variant 和参数；再确认覆盖范围；再检查 endian 和编码；再看是否存在 partial write、并发写、buffer 复用、压缩层位置差异。不要一上来判断磁盘坏，也不要直接关闭校验。CRC mismatch 是信号，信号需要分类。

## Q004. CRC32 的指标应该怎么设计才不会只看平均值？

CRC32 的指标不能只看“平均校验耗时”。平均值会把真正重要的东西盖掉：尾部损坏、某个 segment 频繁 mismatch、某类 record 覆盖范围错、恢复时截断数量异常、p99 计算成本突然升高。

我会先设计正确性指标。至少包括：`crc_mismatch_total`，按 stream、segment、record_type、writer_version、reader_version、reason 分类；`crc_checked_bytes_total`，看实际覆盖了多少数据；`crc_skipped_records_total`，防止某些路径绕过校验；`log_recovery_truncated_bytes_total` 和 `log_recovery_truncated_records_total`，看恢复时丢弃了多少尾部数据；`mid_segment_crc_mismatch_total` 单独告警，因为中段损坏比尾部半写更危险。

然后看动作指标。校验失败后系统做了什么：截断、隔离、重试读取、从副本恢复、停止启动、进入只读模式。一个系统的 CRC 指标如果只记录 mismatch，不记录处理结果，就很难判断风险是否被收住。

性能指标要按路径和大小分桶。写入路径算 CRC、读取路径验证 CRC、恢复路径批量验证 CRC，是三类成本。每类都要看 p50、p95、p99、最大值和 bytes/s。小 record 的成本可能主要是函数调用和内存访问，大 record 的成本可能主要是内存带宽。若有硬件加速，还要分出 accelerated 和 fallback 两条路径。

还要看覆盖率。比如 `crc_enabled_records / total_records`、`crc_verified_on_read_records / read_records`、`crc_algorithm_version`、`records_without_crc_total`。有些事故不是 CRC 报错，而是某条新写入路径忘了写 CRC，指标必须能看见这种沉默失效。

结合 LogServe，如果 shared log 后续加 CRC32C，我会在 dashboard 里放三类视图：校验失败的 stream/segment 分布，恢复时截断了多少 record，CRC 计算对 append/read p99 的影响。面试里可以说：CRC 指标要同时回答“坏了没有”“坏了怎么处理”“校验本身有没有拖慢热路径”，平均耗时只回答了第三个问题的一小部分。

## Q005. CRC32 的正确性边界和性能边界分别是什么？

CRC32 的正确性边界可以从一句话开始：同一组 bytes，用同一组 CRC 参数计算，才能比较结果。这里的“同一组参数”包括 polynomial、init、refin、refout、xorout、输入 bit 顺序、输出编码；“同一组 bytes”包括字段顺序、字节序、压缩前后位置、是否包含 header、是否包含 length、是否包含 padding。

在这个边界内，CRC32 对偶发损坏很有价值。短 burst error、单 bit 错误、很多低权重错误会被发现。超出这个边界，它不提供承诺。它不能保证两个对象不同就一定有不同 CRC，不能阻止攻击者改数据后重算 CRC，也不能证明数据来自谁。把 CRC32 放在日志 record、page、chunk、frame 旁边是合理的；把它单独放进权限、签名、token、防伪造链路里就越界了。

正确性还包括故障处理策略。检测到 mismatch 后怎么做，是格式的一部分。尾部半写可以截断，中段 mismatch 可能要停止恢复或走副本校验，重复 mismatch 要保留证据。只“发现错误”还不够，必须确保错误不会被 replay 到状态机里。对 LogServe 这种 log-first 系统，CRC mismatch 不能被静默跳过，否则 materialized view 可能从坏记录里恢复出错误状态。

性能边界主要受内存带宽、实现方式、数据大小和拷贝次数影响。CRC32/CRC32C 很快，尤其有硬件指令或优化表时，但它不是免费的。对小 record，高 QPS 下函数调用、对象分配、slice 边界检查、锁和系统调用可能比 CRC 运算更显眼；对大对象，内存扫描和缓存污染会成为成本。最差的写法是为了算 CRC 把 payload 复制一遍，再写一遍。

优化要先守住语义。可以流式计算，边编码边更新 CRC；可以用 CRC32C 硬件路径；可以批量处理；可以避免重复扫描 header 和 payload。不能为了快就只算 payload 不算关键 header，也不能在读路径因为 p99 上升就跳过验证。校验覆盖范围是正确性契约，性能优化只能在契约内做。

面试里的边界回答可以这样讲：CRC32 的正确性边界是 bytes 和参数完全一致，并且只承诺误码检测；性能边界是每次校验至少要看一遍被覆盖的数据，真实瓶颈常在内存扫描和拷贝。它适合热路径可靠性校验，但不适合承担安全认证或唯一身份。
## Q006. 面试官如果只问一个问题检验你是否理解 SHA-256，可能会问什么？

我会问：你从网上下载一个二进制文件，页面上也写了 SHA-256。校验一致后，能说明什么，不能说明什么？如果这用于生产发布，你还缺什么？

这个问题比“SHA-256 怎么压缩分组”更能区分工程理解。校验一致只能说明你手里的 bytes 和那个 digest 对得上。它不能自动说明 digest 是可信的，也不能说明发布者身份。如果攻击者同时替换了二进制和同一页面上的 digest，用户仍然会看到“SHA-256 校验通过”。

SHA-256 的价值在于它是强无密钥摘要。给定可信 digest，攻击者很难找另一个不同文件拥有同样 digest，也很难从 digest 反推出原文件。它适合内容寻址、下载完整性、签名前摘要、Merkle Tree 叶子和内部节点、构建产物 manifest。这里的前提是 digest 的来源、算法标识和 bytes 定义可信。

生产发布通常还缺签名和信任链。更稳的做法是发布物有 SHA-256 digest，manifest 被发布方私钥签名，验证者持有可信公钥或通过证书/透明日志/TUF/Sigstore 这类机制建立信任。SHA-256 负责把大文件变成固定长度承诺，签名负责证明“谁认可了这个承诺”。两层不要混在一起。

还要补一句低熵边界。`SHA256(password)` 不安全，不是因为 SHA-256 弱，而是密码空间太小，攻击者可以离线枚举。`SHA256(user_id)`、`SHA256(phone)` 也挡不住字典枚举。哈希函数很强，不代表输入熵足够。

结合 LogServe，如果 result object 用 SHA-256 记录在 manifest 里，这能帮助确认读取到的对象和当初记录的 bytes 一致。但 manifest 本身如果没有来自 shared log 的可信提交、HMAC 或签名保护，攻击者仍可能替换 manifest 和对象。面试里把“digest 对得上”和“来源可信”分开讲，会显得很稳。

## Q007. SHA-256 的一句话定义是否容易误导，误导点在哪里？

常见定义是“SHA-256 是一种安全哈希算法，输出 256 位摘要”。这句话也会误导，因为“安全”两个字容易让人误以为它什么都能保护。

第一个误导是把 SHA-256 当加密。哈希不可逆，不等于加密。加密的目标是有密钥时能解密、无密钥时看不懂；SHA-256 没有解密过程，也不隐藏低熵输入。把手机号、邮箱、状态值直接 SHA-256 后公开，攻击者可以枚举候选值再比对 digest。

第二个误导是把 SHA-256 当认证。SHA-256 没有密钥。任何人都能为任意消息计算 digest，所以它不能证明消息来自谁。要做消息认证，用 HMAC-SHA256；要给第三方验证发布者，用数字签名；要同时保密和认证，用 AEAD。

第三个误导是把 digest 当业务对象身份时忘了上下文。SHA-256 对 bytes 敏感。相同 JSON 语义如果字段顺序、空格、Unicode normalization、浮点格式不同，bytes 就不同，digest 也不同。反过来，如果两个业务对象被错误地 canonicalize 成同一串 bytes，digest 也会一样。哈希前必须定义“被哈希的到底是什么”。

第四个误导是忽略算法标识。只存一个十六进制串，几年后可能不知道它是 SHA-256、SHA-1、BLAKE3、截断 SHA-256，还是带前缀的 domain-separated hash。长期存储的 manifest 应该带 `algorithm`、`digest_length`、`encoding`、`canonicalization_version`，必要时带用途域。

更准确的一句话是：SHA-256 是无密钥的密码学哈希函数，把任意长度 bytes 映射成 256 位 digest，适合在可信 digest 或上层签名保护下做完整性、内容寻址和签名前摘要；它不加密、不认证来源，也不修复低熵输入。

## Q008. SHA-256 最常见的生产事故触发条件是什么？

最常见的是“哈希了错误的 bytes”。团队以为双方在验证同一份内容，实际上一个哈希压缩前数据，一个哈希压缩后数据；一个哈希 JSON 对象的某种序列化结果，另一个用不同语言的默认序列化；一个把换行当 `\n`，另一个在 Windows 路径里读到了 `\r\n`。SHA-256 很稳定，所以这些差异会稳定地造成失败。

发布链路里常见事故是 digest 来源不可信。构建系统产出二进制和 SHA-256 文件，但二者放在同一台未受保护的服务器上。攻击者拿到写权限后一起替换，用户校验仍通过。表面上用了 SHA-256，实际没有建立信任边界。

存储和对象系统里常见事故是 multipart 或分块语义没定义。整体 SHA-256、每块 SHA-256、树形 hash、S3 ETag、压缩后对象 digest，都是不同东西。客户端拿整体 digest 去比对分块组合值，会失败；服务端返回的是加密后对象 digest，客户端按明文算，也会失败。接口必须说清楚 digest 作用于哪一层。

安全设计里常见事故是把 SHA-256 当密码存储或请求签名。`SHA256(password)` 太快，数据库泄露后会被 GPU 离线爆破。`SHA256(secret || message)` 不是标准 MAC，可能遇到 length extension 或拼接歧义。即使没有被立刻打穿，安全评审也很难接受这种自造构造。

还有一种事故来自截断和展示。为了 UI 方便，只显示 digest 前 8 位，然后人工或脚本只比前 8 位。短前缀适合日志关联，不适合作安全验证。校验必须比较完整 digest，或者明确截断长度对应的安全强度，不能随手截。

排查 SHA-256 事故时，我会先拿到双方原始 bytes，确认长度、hex/base64 编码、换行、压缩层、canonicalization version；再确认 digest 来源是否可信；最后看是否把 SHA-256 用在了它不该承担的场景，比如认证、密码存储或防重放。

## Q009. SHA-256 的指标应该怎么设计才不会只看平均值？

SHA-256 指标要分两类：完整性验证指标和计算成本指标。只看平均哈希耗时没有意义，因为大对象、热路径小对象、批量验证、恢复扫描、发布校验的成本完全不同。

完整性指标至少要有 `digest_verify_total`、`digest_mismatch_total`、`digest_missing_total`、`digest_algorithm`、`digest_source`。mismatch 要按对象类型、大小分桶、存储后端、压缩/encryption 状态、producer version、consumer version 分类。这样才能看出是某个 SDK 写错 canonical bytes，还是某个存储层返回了不同内容。

还要看覆盖率。比如 result object 有多少带 SHA-256，manifest 有多少被验证，下载物有多少只校验了长度而没有校验 digest，旧 SHA-1/MD5 路径是否仍在使用。没有覆盖率，系统可能“所有做了校验的请求都成功”，但真正的问题路径从来没校验过。

性能指标要按 bytes 分桶看 p50、p95、p99 和 throughput。1 KB 请求体的 SHA-256 成本和 5 GB 对象的 SHA-256 成本不是一个问题。对大对象，读盘、网络、内存拷贝、压缩解压可能比哈希更贵；对小对象，高 QPS 下对象分配和 canonicalization 可能更贵。指标要拆出 `hash_cpu_time`、`hash_input_bytes`、`body_read_time`、`canonicalization_time`。

失败路径也要有指标。digest mismatch 后是拒绝、重试、从副本恢复、隔离对象，还是降级继续？这些动作要计数。安全链路里还要告警“digest 来自不可信渠道”“算法过旧”“digest 长度被截断”。

结合 LogServe，如果 result object、checkpoint artifact 或 worker binary 用 SHA-256，我会在 dashboard 中展示：对象 digest 覆盖率、验证失败数、按对象大小分桶的验证 p99、manifest 缺 digest 数、旧算法使用数。面试里可以说：SHA-256 指标要看“有没有验证正确的东西”和“验证成本落在哪条路径”，平均耗时只能做粗略背景。

## Q010. SHA-256 的正确性边界和性能边界分别是什么？

SHA-256 的正确性边界是：输入 bytes 被精确定义，算法和编码被精确定义，期望 digest 来自可信来源。满足这些条件时，SHA-256 能给出强内容承诺：想找另一个不同输入撞上同一个 digest，在现实计算能力下不可行；想从 digest 反推出高熵输入，也不可行。

但 SHA-256 不证明来源。digest 如果和数据一起来自同一个可被攻击者修改的地方，完整性价值会大幅下降。它也不保护低熵秘密，不能直接存密码、手机号、短 token。它不处理 canonicalization，业务层必须先把对象变成稳定 bytes。它也不自带防重放，旧的 `(message, digest)` 可以被原样拿来用。

正确性还要求 domain separation。比如同一个系统里既哈希叶子块，又哈希内部节点，又哈希 manifest。最好给不同用途加前缀或结构化编码，避免不同语义的 bytes 落入同一个哈希域。Merkle Tree 里叶子和内部节点区分就是这个思路。

性能边界很直接：SHA-256 要读完整输入。对流式大文件，可以边读边 hash，不必全部读入内存；对对象存储，可以上传时计算，避免事后再扫一遍；对重复验证，可以缓存 digest，但缓存本身要绑定对象 generation、长度、算法和内容版本。不能为了快跳过关键字段，也不能对同一份大对象在多个层重复扫描。

现代 CPU 对 SHA-256 可能有硬件指令支持，吞吐会不错，但它仍比 CRC32C 这类误码检测更重。若场景只是内部 page/record 损坏检测，CRC32C 可能更合适；若场景是发布物、内容寻址、跨信任边界完整性，SHA-256 的成本通常值得付。大规模并行哈希或树形哈希可以考虑 BLAKE3、Merkle chunking 或存储层校验，但要先确认安全和生态要求。

面试里可以这样回答：SHA-256 的正确性依赖可信 digest 和稳定 bytes，不依赖“看起来是同一个对象”；性能上至少扫描一遍输入，瓶颈常在 I/O、内存拷贝和 canonicalization。它是很强的摘要工具，但不是认证、加密、密码存储或业务幂等的替代品。
## Q011. 面试官如果只问一个问题检验你是否理解 HMAC，可能会问什么？

我会问：为什么应该用 `HMAC(key, canonical_request)`，而不是 `SHA256(secret + body)`；你会把哪些字段放进 canonical request，怎么处理重放？

这个问题会逼出 HMAC 的真实边界。HMAC 不是“给 hash 加个 secret”这么随意。标准构造用 inner pad 和 outer pad 把 key 与消息分两层混合，避免很多简单前缀 MAC 的结构问题。自己写 `SHA256(secret + body)`，可能遇到 length extension、拼接歧义、字段遗漏、跨语言编码不一致，后续很难证明安全。

第二层是签什么。只签 body 不够。真实请求里 method、path、query、tenant、timestamp、nonce、body digest、content type、key id、协议版本、业务 idempotency key、lease/attempt 这些字段都可能影响安全语义。攻击者如果不能改 body，但能把同一个 body 从 `/submit` 挪到 `/admin/delete`，或者从 tenant A 挪到 tenant B，签名就失去了上下文。

第三层是防重放。HMAC 验证通过只说明这条消息没被不知道 key 的人改过，不说明它是新的。攻击者可以抓到合法 `(message, tag)` 原样再发一遍。要把 timestamp、nonce、sequence number 或 request id 放进被签名内容，并在服务端做时间窗口和 nonce/idempotency 原子检查。HMAC 保护这些字段不被改，状态机负责判断它们有没有用过。

第四层是比较和错误处理。tag 要严格解析，固定算法、固定长度、固定编码，比较时用 constant-time compare。错误响应不要泄露“key id 不存在、timestamp 过期、tag 前几位对了”这种细节，日志也不能把 secret、完整 token、完整签名串暴露出去。

结合 LogServe，如果 SDK task submission 或 worker complete 用 HMAC，我会签：version、method、path、tenant、workflow_id、task_id、attempt、lease/epoch、timestamp、nonce、body_sha256。control 验签后还要用 shared log 或幂等表处理重复 attempt。HMAC 证明请求没被改，不替代任务状态机。

## Q012. HMAC 的一句话定义是否容易误导，误导点在哪里？

常见说法是“HMAC 是用密钥加盐的哈希”或“HMAC 是加密哈希”。这两种说法都容易误导。

第一，HMAC 不是 salt。salt 通常是公开随机值，用来防止预计算和同密码同 hash；HMAC key 是 secret，泄露后认证语义就没了。把 HMAC key 叫 salt，会让人低估密钥管理、轮换和访问控制的重要性。

第二，HMAC 不加密。它不隐藏消息内容，只生成认证 tag。明文请求体如果包含敏感数据，HMAC 后别人仍然能看到。保密要靠 TLS、存储加密或 AEAD。把 HMAC 叫“加密”会导致系统把敏感 payload 暴露给客户端或日志，以为有 tag 就安全。

第三，HMAC 不等于数字签名。HMAC 使用共享密钥，验证者也能生成 tag。它适合内部服务、webhook、两方共享 secret 的场景，不适合公开发布物和第三方归责。如果要让任何人用公钥验证发布者身份，应该用签名。

第四，HMAC 不自动解决重放。旧消息和旧 tag 仍然合法，除非消息里包含时间、nonce、sequence、request id，并且服务端保存或检查状态。很多线上事故不是 tag 被伪造，而是合法请求被重复处理。

更准确的一句话是：HMAC 是基于密码学哈希和共享密钥构造的消息认证码，用来验证消息完整性和“生成者知道这把 key”；它不保密，不公开验签，也不自动防重放。

## Q013. HMAC 最常见的生产事故触发条件是什么？

HMAC 生产事故最常见的触发条件是 canonicalization 不一致。两边 key 没错，算法也没错，但签名串不是同一串 bytes。一个服务把 query 参数按原顺序签，另一个按字典序验；一个把 header 名转小写，另一个保留大小写；一个对 JSON 做 compact encoding，另一个保留空格；一个代理改了 path 里的 `%2F`。最终就是“偶发签名失败”。

第二类是字段漏签。只签 body，不签 method、path、tenant、timestamp、nonce、content type、body hash。攻击者不一定能改 body，但可以重放到别的资源，或利用代理层路由差异制造跨上下文请求。签名覆盖范围必须跟授权和幂等语义一致。

第三类是重放状态没有原子性。服务端验证 timestamp 和 HMAC 后，再检查 nonce；高并发下两个相同 nonce 请求同时进来，如果检查和写入不是 set-if-absent 或唯一约束，可能都通过。或者服务重启后内存 nonce cache 丢了，旧请求又能通过。HMAC 算法无状态，防重放是系统状态问题。

第四类是 key rotation。客户端已经换到新 key，服务端某个实例还没有刷新；或者服务端同时接受旧 key 和新 key，但没有 key id，导致每次验签都遍历所有 key；再或者旧 key grace window 太长，泄露风险扩大。生产里要有 key id/version、明确的生效时间、撤销状态和指标。

第五类是实现细节。把 HMAC 对象跨 goroutine 复用，内部状态互相污染；tag 用普通字符串比较，产生 timing side channel；失败日志打出完整 canonical string，里面有 token 或 secret；错误路径每次都查远程 KMS，坏签名流量一来就把依赖打爆。

结合 LogServe，如果 worker complete 请求缺少 attempt、lease 或 epoch，即使 HMAC 正确，旧 worker 也可能拿旧请求重放完成事件。真正的修复不是“换更强 hash”，而是把这些状态字段签进去，再让 control 的状态机拒绝旧 attempt。

## Q014. HMAC 的指标应该怎么设计才不会只看平均值？

HMAC 指标最不能只看平均验签耗时。平均值很容易掩盖两类风险：错误请求风暴和状态依赖慢路径。一个系统平时验签 0.2 ms，坏 key id 流量一来每次查 KMS 50 ms，平均值可能到事故很晚才变形。

我会先按失败原因分类：`hmac_verify_success_total`、`hmac_verify_failed_bad_tag_total`、`unknown_key_id_total`、`expired_timestamp_total`、`future_timestamp_total`、`nonce_replay_total`、`canonicalization_error_total`、`malformed_signature_total`。这些原因不能全部混成 401，否则排障时不知道是攻击、时钟漂移、发布不一致还是客户端 bug。

延迟指标要拆层。纯 HMAC 计算、canonical request 构造、body SHA-256、key lookup、key cache miss、nonce set-if-absent、日志写入、完整验签路径分别计时。小请求里 HMAC 本身通常很快，真正慢的是 body 读取、JSON/canonicalization、远程 key store、Redis/DB nonce 写入。

安全指标要看分布和尖峰。按 key id、tenant、client id、source IP、path、错误原因统计失败率；看 p95/p99 和最大值；看单位时间 bad tag 数量；看同一 nonce 的重复次数；看未知 key id 是否集中爆发。重放和攻击通常不是均匀分布，平均失败率不够用。

还要看 key rotation 指标。当前活跃 key 版本、旧 key 命中次数、新 key 命中次数、grace window 内失败数、撤销 key 使用尝试。很多事故发生在轮换期间，指标必须能区分“旧客户端还没升级”和“有人在用已撤销 key”。

结合 LogServe，如果内部 control/worker 请求用 HMAC，dashboard 应该能看到每类验签失败、worker clock skew、nonce replay、key cache miss、验签 p99、错误日志采样率。面试里可以说：HMAC 指标要把安全语义、状态检查和性能路径拆开看，平均验签耗时只适合做背景噪声。

## Q015. HMAC 的正确性边界和性能边界分别是什么？

HMAC 的正确性边界从 key 开始。key 必须有足够熵，按用途隔离，不能把 webhook key、cookie key、日志指纹 key、内部 RPC key 混用。key 要有版本、轮换、撤销和访问控制。只要 key 泄露，HMAC 就不能再证明来源。

第二个边界是 message bytes。HMAC 认证的是 bytes，不是“看起来一样的请求”。canonical request 必须稳定、无歧义、跨语言一致。字段之间要有明确分隔或长度前缀，参与授权、路由、租户、幂等、防重放的字段必须纳入签名。漏签字段就等于把字段交给攻击者或中间层改。

第三个边界是验证动作。tag 长度和编码固定，解析严格，constant-time compare，失败时拒绝处理。timestamp、nonce、sequence、request id 被 HMAC 覆盖后，还要由服务端状态检查新鲜性。HMAC 不能替你记住请求是否来过，也不能保证业务只执行一次。

性能边界通常不在 HMAC 函数本身。HMAC-SHA256 对小消息很快。真实成本常在 body digest、canonicalization、读取大请求体、key lookup、nonce 存储、日志、KMS/Redis/DB 网络。坏签名流量还会放大错误路径成本，所以要先做大小限制、key id allowlist、时间窗口粗筛，再进入重成本验证。

对大 payload，常见做法是流式计算 body SHA-256，再把 body digest 放入 canonical request 做 HMAC。这样避免把整个 body 拼进内存里的签名串。对高 QPS，可以缓存 key，按 tenant/key id 分片 nonce 存储，失败日志采样。不能为了省 nonce 存储就取消防重放，也不能为了快用普通字符串比较 tag。

面试里可以这样回答：HMAC 的正确性依赖 secret key、稳定签名串、完整字段覆盖、严格比较和外部防重放状态；性能上瓶颈多在外围 I/O 和状态检查，不在 HMAC 公式本身。它能认证消息，但不加密，不公开验签，也不替代幂等和分布式状态机。
## Q016. 面试官如果只问一个问题检验你是否理解 Merkle Tree，可能会问什么？

我会问：现在我只给你一个可信 root、某个 chunk、以及一条 proof path，你怎么验证这个 chunk 属于整份对象？这个问题会顺手追问：root 为什么可信，叶子和内部节点怎么编码，左右顺序怎么处理。

回答时可以先讲流程。客户端拿到 chunk 后，先按约定计算 leaf hash。然后沿着 proof path 一层层合并兄弟节点：如果当前节点是左孩子，就 `hash(internal_prefix || current || sibling)`；如果是右孩子，就 `hash(internal_prefix || sibling || current)`。最后算出的 root 必须等于可信 root。只要 hash 强、编码无歧义、root 可信，就能证明这个 chunk 在那棵树的那个位置上。

这个问题的重点不是“树上每个节点都是 hash”这么简单，而是 proof 语义。Merkle proof 证明的是某个叶子、某个 index、某个 tree size 下的包含关系。它不自动证明 root 可信，也不自动证明这份数据来自谁。root 要来自签名 manifest、可信 metadata、区块头、证书透明日志或 shared log 中已经提交的事件。

还要讲 domain separation。叶子节点和内部节点最好使用不同前缀，例如叶子 hash 前加 leaf 标记，内部节点 hash 前加 internal 标记。这样可以避免某段数据被解释成内部节点拼接，或内部节点 bytes 被解释成叶子数据。很多标准化 Merkle 结构都会明确叶子和内部节点的域分离。

再讲左右顺序。`hash(left || right)` 和 `hash(right || left)` 不一样。proof 必须携带方向，或者验证者必须能从 index 推出方向。若实现只给一组兄弟 hash，不说明左右位置，就会出现多个不同结构算出不同 root，验证失败，甚至出现结构混淆风险。

结合 LogServe，Merkle Tree 可以用于大 result object、checkpoint manifest 或 segment set 的局部校验。shared log 里记录可信 root，result store 里保存 chunk 和 proof。worker 或客户端只读取某个 chunk 时，不必下载整份对象，也能确认这个 chunk 属于已提交结果。但前提是 root 的提交本身受 log-first 状态机保护，不能随对象一起被替换。

## Q017. Merkle Tree 的一句话定义是否容易误导，误导点在哪里？

常见定义是“Merkle Tree 是一棵每个节点都是哈希的树”。这句话太松，容易让人忽略真正决定正确性的细节。

第一个误导是忘了 root 的信任来源。Merkle Tree 的所有证明最终都落到 root。如果 root 是攻击者给的，攻击者可以为假数据构造一棵完整的假树。root 必须来自可信渠道，比如签名 manifest、共识提交、透明日志、可信数据库事务或已经认证的控制面事件。

第二个误导是忽略结构定义。叶子怎么切块，块大小是多少，最后一块怎么处理，空树怎么表示，奇数节点是提升、复制还是和空节点合并，内部节点怎么编码，hash 输出怎么编码，这些都会影响 root。两端都说“我们用了 Merkle Tree”，但这些规则不同，root 就对不上。

第三个误导是把 Merkle Tree 当成自动安全。它通常使用 SHA-256、SHA-3、BLAKE2/BLAKE3 这类密码学 hash，但安全性还依赖 domain separation、无歧义编码和 root 保护。没有这些，树形结构可能出现替换、重排、歧义编码、混淆叶子和内部节点等问题。

第四个误导是把它当成性能免费。Merkle Tree 支持局部验证和增量更新，但需要存储中间节点、proof path、tree metadata。小文件只算一个 SHA-256 可能更简单；超大对象、多副本同步、按需下载、审计日志和增量比较才更能体现它的价值。

更准确的一句话是：Merkle Tree 是一种用密码学哈希把有序叶子集合承诺成一个可信 root 的结构，支持 `O(log n)` 的包含证明和局部更新；它的正确性依赖可信 root、固定树规则、无歧义编码和合适的 hash。

## Q018. Merkle Tree 最常见的生产事故触发条件是什么？

最常见的触发条件是树规则没有固定下来。生产者按 4 MiB 切块，消费者按 8 MiB 切块；生产者对奇数节点做 duplicate，消费者直接 promote；生产者叶子 hash 原始 bytes，消费者叶子 hash base64 文本；生产者内部节点拼 hex string，消费者拼原始 digest bytes。每个人都在“算 Merkle Tree”，结果没有一个 root 对得上。

第二类事故是顺序和 index 丢失。Merkle Tree 保护的是有序集合。叶子相同但顺序不同，root 应该不同。若 proof 没带 index、方向或 tree size，验证者可能无法区分“第 3 块属于文件”和“某个相同内容的块属于文件”。对象同步、分块下载、审计日志里，这会造成错误定位或错误接受。

第三类是 root 没受保护。系统把 chunks、tree nodes、root manifest 都放在同一个可写对象桶里，没有签名、HMAC 或 log 提交。攻击者或误操作替换整套对象后，局部 proof 仍然自洽。Merkle Tree 只能证明相对于某个 root 的一致性，不能证明 root 自己可信。

第四类是没有 domain separation。叶子和内部节点都直接 `hash(left || right)` 或 `hash(data)`，没有前缀和长度。某些结构下可能出现歧义，至少会让安全分析困难。工程上不要依赖“应该不会刚好撞上”，而要把叶子、内部节点、manifest、版本字段区分开。

第五类是增量更新不完整。某个 chunk 更新后，只重算了叶子，没有重算到 root；或者缓存了旧的中间节点；或者并发上传多个 chunk 时，manifest root 来自混合版本。结果是单块校验通过，整体 root 不稳定。需要对象 generation、事务提交或 copy-on-write manifest。

结合 LogServe，如果 checkpoint cache 或 result object 做 Merkle manifest，我会把 `tree_version`、`chunk_size`、`hash_algorithm`、`leaf_prefix`、`node_prefix`、`tree_size`、`object_generation` 写进 manifest，并让 root 的接受动作进入 shared log。这样排查时知道 root 是哪套规则算出来的，也知道哪个版本被状态机接受。

## Q019. Merkle Tree 的指标应该怎么设计才不会只看平均值？

Merkle Tree 指标不能只看平均构建时间。真正需要观察的是树规模、proof 成本、局部更新成本、root mismatch 位置、存储放大和验证覆盖率。

构建指标要按对象大小、chunk size、叶子数量、hash 算法分桶。记录 `tree_build_seconds` 的 p50、p95、p99，`tree_leaf_count`，`tree_node_count`，`tree_bytes_hashed_total`，`tree_metadata_bytes`。平均构建时间看不出“大对象 p99 构建拖住提交路径”这种问题。

验证指标要区分整体验证和 proof 验证。整体验证是重算全树，成本接近扫完整对象；proof 验证是 `O(log n)`，成本跟树高和 proof bytes 有关。指标可以有 `proof_verify_seconds`、`proof_sibling_count`、`proof_bytes`、`proof_failure_total`，失败原因按 bad leaf、bad sibling、bad direction、root mismatch、unsupported tree version 分类。

局部更新指标也很重要。记录每次更新多少 leaf、重算多少 internal node、dirty subtree 数、manifest commit 延迟、并发冲突次数。Merkle Tree 的价值之一是局部更新，如果每次小改都重建整棵树，指标会暴露出来。

错误定位要看深度和范围。root mismatch 只是最后结果，排查时更想知道 mismatch 首次出现在哪个 subtree、影响哪些 chunk、是否集中在某个存储节点或上传批次。可以记录 `merkle_mismatch_depth`、`corrupt_leaf_index`、`affected_chunk_range`，并把恢复动作分类：重读、从副本修复、隔离、拒绝 manifest。

还要看覆盖率和版本。多少对象有 Merkle root，多少对象只有单个 digest，多少 proof 因版本太旧无法验证，多少 manifest 缺 chunk size 或算法标识。没有这些指标，系统可能在新路径上有树，旧路径上仍然裸奔。

结合 LogServe，如果大 result object 用 Merkle Tree，我会看：按对象大小分桶的构建 p99、proof 验证 p99、proof bytes p99、root mismatch 数、chunk 修复成功率、manifest commit 延迟、tree metadata 占比。面试里可以说：Merkle Tree 的指标要围绕局部证明和局部修复设计，平均构建时间只是一项成本背景。

## Q020. Merkle Tree 的正确性边界和性能边界分别是什么？

Merkle Tree 的正确性边界首先是可信 root。所有 proof 都只是在说明“这个叶子相对于这个 root 成立”。如果 root 不可信，proof 没有安全意义。root 的来源要么被签名，要么在可信数据库事务里，要么由共识或 shared log 提交，要么来自其他已经认证的 metadata。

第二个边界是树规则固定。叶子顺序、chunk size、最后一块处理、空树规则、奇数节点规则、hash algorithm、节点编码、domain separation、tree size、proof 格式都必须写清楚。任何一项不清楚，跨语言实现都会出问题。长期存储还要把这些规则的 version 放进 manifest，便于升级。

第三个边界是 hash 安全性和编码无歧义。Merkle Tree 不能强于底层 hash。若底层 hash 已不适合碰撞场景，树也会继承风险。叶子和内部节点要做 domain separation，字段要长度前缀或结构化编码，避免 `hash(a || bc)` 与 `hash(ab || c)` 这类拼接歧义。

第四个边界是业务语义。Merkle proof 证明的是内容包含，不证明授权，不证明用户有权读取该 chunk，不证明对象没有过期，也不证明这是最新版本。版本、新鲜性、权限和生命周期仍然由上层 metadata、签名时间、object generation 或访问控制处理。

性能边界则来自三件事：构建要读全部叶子，proof 验证只读一条路径，更新只重算脏叶子到 root 的路径。理论上，构建是 `O(n)`，proof 是 `O(log n)`，单点更新是 `O(log n)`。实际性能还受 chunk size、hash 速度、I/O、并发、metadata 存储、proof 序列化影响。

chunk size 是典型取舍。块太小，叶子多，proof 路径更长，metadata 更多；块太大，局部验证和局部重传粒度变粗。对象存储、备份、日志归档、模型 checkpoint 的合适块大小不一定一样。要根据对象大小分布、访问模式、网络重试成本和存储开销调。

结合 LogServe，如果 Merkle Tree 用在 result object 或 checkpoint manifest，正确性上要把 root 的接受写入 shared log，并绑定 workflow/task/result version；性能上要避免在热路径反复重建大树，可以上传时流式生成叶子，后台构建内部节点，读取局部 chunk 时只验证 proof。面试里可以这样答：Merkle Tree 的强项是局部证明和增量比较，但它不是签名、不是权限系统，也不是免费的哈希加速器。
