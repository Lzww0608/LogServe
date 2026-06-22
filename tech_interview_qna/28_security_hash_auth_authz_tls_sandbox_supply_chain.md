# 28. 安全：哈希、认证、授权、TLS、沙箱与供应链

这一章回答安全基础问题。面试里这类题最容易被答成几个混在一起的词：加密就是安全、hash 就是校验、签名就是加密、登录成功就等于有权限。真正要讲清楚的是每个机制的安全目标、信任前提、密钥归属和失效边界。

下面的回答主要参考 RFC 4949 的 Internet Security Glossary、RFC 2104 的 HMAC、RFC 5116 的 AEAD、NIST FIPS 180-4 的 Secure Hash Standard、NIST FIPS 186-5 的 Digital Signature Standard，以及 NIST SP 800-38D 对 GCM/GMAC 的规范。结合 LogServe 时，我会把边界说清楚：如果后续给 shared log、snapshot、result object、SDK token、worker 注册或供应链构建增加安全机制，不能只说“用了加密”或“有 hash”。要明确它保护的是完整性、身份、访问权限、机密性，还是回放、篡改、伪造和密钥泄露中的某一类风险。

## Q001. 完整性、认证、授权、机密性分别是什么意思？

完整性、认证、授权、机密性是四个不同目标，不是一组可以互相替代的同义词。

完整性关注“东西有没有被不允许地改过”。这里的东西可以是网络报文、磁盘文件、日志记录、二进制包、配置、镜像，也可以是某个数据库字段。完整性有两层：一层是 accidental corruption，比如磁盘坏块、传输错误、内存翻转；另一层是 malicious tampering，比如攻击者故意改 payload、改金额、改权限字段。checksum 更偏第一层，MAC、数字签名、AEAD 更偏第二层。

认证关注“这个身份或来源是不是真的”。它也分两种常见语义。实体认证是确认对方是谁，比如用户登录、服务端证书、mTLS 客户端证书。数据来源认证是确认这段数据来自谁，比如 MAC 证明“知道共享密钥的一方生成了这个 tag”，数字签名证明“持有私钥的一方签了这段数据”。认证不是授权。用户密码正确，只说明这个用户身份成立，不说明他可以删除生产数据。

授权关注“已经确认身份之后，这个主体能做什么”。它是策略判断和权限执行问题，常见模型包括 ACL、RBAC、ABAC、capability、租户隔离、资源级权限。授权依赖身份，但不等于身份。一个 worker 能注册到 control，不代表它能接所有模型任务；一个 SDK token 能 submit task，不代表它能读所有 workflow 的结果。

机密性关注“没有权限的人看不到内容”。加密是实现机密性的常见工具，但不是唯一条件。密钥管理、访问控制、内存暴露、日志脱敏、备份权限、TLS 终止位置都会影响机密性。密文如果被错误地记录到日志，或者密钥和密文放在同一个对象存储目录里，机密性仍然可能失效。

四者之间的关系可以这样看：

```text
完整性：内容没有被未授权修改。
认证：身份或数据来源可以被确认。
授权：确认身份后，判断能不能做某个动作。
机密性：未授权主体不能读取内容。
```

它们可以组合，但不能互相推出。加密提供机密性，不自动提供完整性；签名提供来源认证和完整性，不自动隐藏内容；登录提供认证，不自动授予所有权限；授权策略允许访问，不代表传输过程保密。

结合 LogServe，如果给 shared log 增加安全设计，我会拆成几条独立目标：log record 要有完整性校验，避免被篡改后 replay 出错误状态；worker 注册要有认证，避免假 worker 接任务；任务、workflow、actor、result object 要有授权，避免跨用户读取；LLM prompt、result、checkpoint、snapshot 如果含敏感内容，要有机密性保护。把这些都叫“安全”没有用，设计时必须逐项落到机制上。

面试里可以这样回答：

```text
完整性是防未授权修改，认证是确认身份或数据来源，授权是判断已经认证的主体能做什么，机密性是防未授权读取。它们经常一起出现，但不能互相替代。比如加密可以隐藏内容，但如果没有认证，攻击者仍可能替换或篡改密文；用户登录成功也不代表他有权限访问所有资源。
```

## Q002. checksum、hash、MAC、signature 的安全目标有什么不同？

这四个东西都可能长得像“一段摘要”或“一段校验值”，但安全目标完全不同。面试里最重要的是说出有没有密钥、能不能防恶意攻击、能不能证明来源，以及谁能验证。

checksum 的目标是发现非恶意错误。CRC32、IP header checksum、简单加和校验都属于这一类。它们适合发现传输错误、存储损坏、随机 bit flip，但不适合防攻击者。攻击者知道算法后，可以改数据再重新算 checksum。checksum 没有密钥，也没有抗碰撞目标，不能当安全完整性机制。

cryptographic hash 的目标是把任意长度输入映射成固定长度 digest，并满足 preimage resistance、second-preimage resistance、collision resistance 这类性质。SHA-256、SHA-384、SHA-3 都属于这个层次。hash 本身没有密钥，所以它不能证明来源。你把文件和 SHA-256 一起放在同一个可被攻击者修改的页面上，攻击者可以同时替换文件和 hash。hash 只有在 digest 来自可信渠道时，才提供实际完整性价值。

MAC 是带密钥的完整性和来源认证机制。HMAC 是典型例子，RFC 2104 定义的 HMAC 用 hash 函数和共享密钥构造消息认证码。验证者必须知道同一个 secret key。MAC 可以证明“生成 tag 的人知道这把共享密钥”，所以它适合服务到服务、客户端和服务端共享密钥的场景。它不能提供公开可验证的不可否认性，因为所有持有共享密钥的人都能生成同样的 MAC。

signature 是非对称密钥机制。签名者用私钥签名，验证者用公钥验证。它提供完整性、数据来源认证，并且在证书、身份绑定、时间戳、审计链成立时，可以支持更强的责任归属。NIST FIPS 186-5 对数字签名的描述也强调：签名可以检测数据的未授权修改、认证签名者身份，并作为第三方向证明的一种证据。它的成本通常高于 MAC，但验证方不需要共享签名私钥。

可以用这张表记：

```text
checksum  ：无密钥，防随机错误，不防恶意篡改。
hash      ：无密钥，防碰撞/二次原像等，digest 必须来自可信渠道。
MAC       ：共享密钥，防篡改，证明知道密钥的一方生成了 tag。
signature ：私钥签名、公钥验证，防篡改，可公开验证签名者身份。
```

还有几个常见误区。第一，`hash(message + secret)` 不等于安全 MAC，很多 hash 有 length extension 等构造风险，应该用 HMAC 或标准 MAC。第二，签名通常签的是 hash 或结构化编码后的摘要，不是随手把字符串拼起来签；否则容易出现字段歧义和 canonicalization 问题。第三，checksum 可以用于性能路径和损坏检测，但不要把 CRC32 当成防篡改。

结合 LogServe，log segment 可以有 CRC 或 checksum 来快速发现磁盘损坏；如果要防止日志被未授权修改，就需要 HMAC、签名或 AEAD tag。对外发布 worker binary 或模型 adapter，适合用数字签名，因为验证者不应该拿到签名私钥。内部 control 到 worker 的请求，如果双方共享密钥，MAC 就足够表达“这条消息来自可信控制面”。

面试里可以这样回答：

```text
checksum 主要防随机错误；hash 是无密钥摘要，只有 digest 本身可信时才提供完整性；MAC 用共享密钥提供完整性和来源认证；signature 用私钥签名、公钥验证，提供公开可验证的完整性和签名者身份。区别的核心是有没有密钥、密钥是否共享、能不能防恶意攻击、验证者是谁。
```

## Q003. 为什么加密不自动等于认证？

加密的基本目标是机密性，也就是让没有密钥的人看不到明文。认证的目标是确认数据来源和内容未被篡改。这两个目标相关，但不是同一个目标。一个算法或协议只说“加密了”，并不等于接收方能知道密文是谁生成的，也不等于密文中途没有被改过。

最直接的例子是流加密或 CTR 模式。密文大致是：

```text
ciphertext = plaintext XOR keystream
```

攻击者即使不知道明文，也可以翻转密文中的某一位，让解密后的明文对应位翻转。这叫 malleability。比如金额、布尔开关、权限位、JSON 里的某些字节，如果协议没有认证 tag，接收方可能会解出被修改过的明文，还以为它是合法数据。

CBC 这类分组模式也有类似问题。攻击者可以修改前一个 ciphertext block 来影响下一个 plaintext block；如果服务端还把 padding 错误、格式错误、MAC 错误区分返回，就可能变成 padding oracle。历史上很多 TLS 和应用层加密漏洞都不是“加密算法完全破了”，而是协议没有正确认证密文，或者错误处理泄露了解密过程的信息。

加密也不证明身份。假设系统里有多个客户端都知道同一把加密密钥，服务端收到一段能解密的密文，只能说明“某个知道密钥的人生成了它”，不一定知道具体是谁。再进一步，如果密钥被泄露，攻击者也能生成能解密的密文。身份认证需要证书、签名、MAC key 分离、token、mTLS、challenge-response 等机制。

加密还不自动防 replay。攻击者可以把昨天截获的密文今天重放一遍。如果协议没有 nonce、sequence number、timestamp、session binding 或日志级幂等，接收方可能把旧命令当新命令执行。认证 tag 能证明密文没被改，但如果没有把序号或上下文绑定进去，仍然可能被重放到错误位置。

正确设计通常是“先定义要认证什么，再加密什么”。现代协议倾向使用 AEAD，把 ciphertext、AAD、nonce、key 绑定在一起。TLS 1.3 也只使用 AEAD cipher suites，不再让应用自己组合裸加密和 MAC。原因很现实：手写“encrypt + hash”或“encrypt + MAC”很容易在顺序、覆盖范围、错误处理、padding、重放上出错。

结合 LogServe，如果将来要加密 result object 或 actor snapshot，不能只把 JSON 加密后放到对象存储就结束。还要认证对象的 workflow_id、actor_id、snapshot_seq、stream offset、content type、key version。否则攻击者可能把一个合法密文挪到另一个 actor 或另一个 workflow 下，让系统解密成功但恢复出错误状态。

面试里可以这样回答：

```text
加密只保证未授权者看不懂明文，不保证密文没有被改，也不保证来源是谁。很多加密模式是可篡改的，攻击者可以改 ciphertext 让 plaintext 按可预测方式变化。认证需要 MAC、signature 或 AEAD tag，并且要把 nonce、序号、协议头、资源身份等上下文一起认证。否则“能解密”不等于“可信”。
```

## Q004. 为什么需要 AEAD？

AEAD 是 Authenticated Encryption with Associated Data。它把两个目标放在一个标准接口里：对 plaintext 提供机密性，对 ciphertext 和 associated data 提供完整性与来源认证。RFC 5116 定义 AEAD 接口时，明确把 key、nonce、plaintext、associated data 作为输入，把 ciphertext 和 authentication tag 作为输出。解密时如果 tag 校验失败，不能释放明文。

AEAD 解决的第一个问题是组合错误。过去很多系统会自己拼“加密 + MAC”，但顺序和覆盖范围很容易写错。MAC-then-encrypt、encrypt-and-MAC、encrypt-then-MAC 在安全性上不是随便等价的；错误处理、padding、长度字段、协议版本、序号如果漏掉，都可能出漏洞。AEAD 给协议一个更硬的边界：未通过认证的密文不应该被当成明文处理。

第二个问题是协议头需要认证但不一定需要加密。很多字段必须明文传输，比如 record type、key id、version、sequence number、routing metadata、tenant id、object name、stream offset。它们不一定敏感，但必须防篡改。AEAD 里的 AAD 正是用来绑定这些明文字段的：字段仍然可见，但任何修改都会导致 tag 验证失败。

第三个问题是上下文绑定。加密一段 payload 还不够，要说明它属于哪条连接、哪个 stream、哪个 sequence、哪个版本、哪个算法。否则攻击者可能复制一段合法密文到另一个上下文里。AEAD 本身不自动理解业务上下文，但它提供 AAD 槽位，让协议把上下文纳入认证。

第四个问题是错误处理。AEAD 的使用方式应该是：先验证 tag，再释放 plaintext。应用不应该在 tag 失败时尝试解析部分明文，也不应该返回能区分“padding 错”“JSON 错”“权限错”的细粒度错误。否则攻击者可能把解密端变成 oracle。

常见 AEAD 包括 AES-GCM、AES-CCM、ChaCha20-Poly1305。它们都要求调用方满足自己的 nonce 规则。AEAD 不是“随便用就安全”的魔法接口；key 生成、nonce 唯一性、AAD 设计、tag 长度、错误处理、rekey 都仍然重要。

结合 LogServe，AEAD 很适合保护日志记录、snapshot、result object 或跨组件消息。例如：

```text
plaintext：真正的 task payload、snapshot body、result data。
AAD：stream id、record offset、event type、workflow id、actor id、key version。
nonce：同一 key 下唯一的 record sequence 或分段计数。
tag：验证 payload 和 AAD 没被篡改。
```

这样设计后，攻击者即使拿到一个合法密文，也不能把 actor A 的 snapshot 挪给 actor B，不能改 event type，不能改 offset。系统解密前先认证，失败就拒绝 replay 或恢复。

面试里可以这样回答：

```text
AEAD 的价值是同时提供加密和认证，并把不加密但必须防篡改的协议头放进 AAD。它减少手工组合 encrypt 和 MAC 的错误，要求解密端在认证失败时不释放明文。需要注意的是，AEAD 仍然依赖正确的 key、nonce、AAD 和错误处理；尤其是 GCM 这类模式，nonce 规则非常硬。
```

## Q005. AES-GCM 的 nonce 重用会有什么风险？

AES-GCM 对 nonce 唯一性非常敏感。同一把 key 下，nonce 不能重复用于两次不同加密。RFC 5116 对 AEAD_AES_128_GCM 的 nonce reuse 说得很重：同 key 下重复 nonce 会破坏这两次调用保护的明文机密性，并破坏该 key 提供的真实性和完整性保护。NIST SP 800-38D 也把 GCM 定义为 AEAD，并强调 IV/nonce 生成和管理是安全边界的一部分。

原因来自 GCM 的两个组成部分。加密部分本质上是 counter mode：AES 生成 keystream，然后和 plaintext XOR。若同 key、同 nonce 重复，counter 初始状态重复，keystream 就重复。攻击者拿到两个密文 `C1` 和 `C2`，可以得到：

```text
C1 XOR C2 = P1 XOR P2
```

如果攻击者知道其中一个明文，另一个明文就能被恢复；即使不知道，也能利用格式、JSON 字段、协议头、固定前缀猜出大量内容。这是典型的 two-time pad 问题。

更严重的是认证也会坏。GCM 的 tag 使用 GHASH，内部有一个 hash subkey。nonce 重用会给攻击者提供足够结构，推导或约束这个认证子密钥；RFC 5116 直接说，攻击者可能恢复用于数据完整性的 internal hash key，后续伪造会变得很容易。也就是说，nonce 重用不只是“泄露一点明文”，而是可能让同一 key 下后续消息都失去认证保护。

生产系统里，nonce 重用通常不是算法工程师故意写的，而是状态管理出错：

```text
进程重启后计数器从 0 开始，但 key 没换。
多实例共享同一把 key，各自独立从 0 分配 nonce。
随机 96-bit nonce 在超大规模下没有碰撞监控和上限规划。
VM snapshot 或容器恢复后回滚了 nonce counter。
把时间戳、请求 id、短随机数拼成 nonce，但没有证明全局唯一。
```

常见缓解方式是让 nonce 结构化且可审计。比如每把 key 分配一个唯一 key id 和 per-node prefix，再用单调递增 counter；或者每条连接、每个 segment、每个 stream 有独立 key，nonce 由 sequence number 派生。计数器要持久化或在重启时换 key。达到计数上限前必须 rekey。多进程、多节点不能共享同一 key 后各自随便从 0 计数。

还要把 AAD 设计好。nonce 唯一性解决的是 GCM 基本安全前提；AAD 解决的是上下文绑定。对 LogServe 来说，如果用 AES-GCM 加密 log segment 或 snapshot，nonce 可以来自 `(key_id, stream_id, segment_id, record_seq)` 的严格唯一映射，AAD 绑定 `stream_id`、`offset`、`event_type`、`workflow_id`、`actor_id`、`key_version`。这样可以同时避免 nonce 重复和密文跨上下文搬运。

如果系统很难保证 nonce 唯一，可以考虑 misuse-resistant AEAD，比如 AES-GCM-SIV 这类设计。但这不是让 nonce 随便重复的许可。更稳的工程原则是：普通 AES-GCM 只在你能严格保证同 key 下 nonce 唯一时使用；做不到，就换模式、换 key 管理方案，或者让成熟加密库替你管理 nonce。

面试里可以这样回答：

```text
AES-GCM 的 nonce 在同一 key 下重复会同时破坏机密性和完整性。加密侧会复用 keystream，攻击者能得到两个明文的 XOR；认证侧可能泄露或约束 GHASH 的内部 hash key，使后续伪造变得容易。工程上要用单调计数、唯一前缀、per-connection/per-segment key、重启换 key、rekey 上限来保证 nonce 不重复，不能靠“应该不会撞”的感觉。
```

## Q006. HMAC 为什么能抵抗简单 hash 无法抵抗的攻击？

HMAC 能抵抗简单 hash 扛不住的攻击，核心原因不是“它用了两次 hash”这么简单，而是它把共享密钥、消息和底层 hash 的迭代结构隔离开了。RFC 2104 对 HMAC 的定义是：

```text
HMAC(K, message) = H((K' XOR opad) || H((K' XOR ipad) || message))
```

这里的 `K'` 是按底层 hash 的 block size 处理后的 key，`ipad` 和 `opad` 是两个固定但不同的填充值。内层 hash 把 key 混入消息计算，外层 hash 再用另一份 key 派生状态认证内层结果。这个结构让攻击者只看到最终 tag，看不到可以继续追加消息的中间状态，也不能把普通 digest 当成新的合法前缀继续扩展。

简单写法最常见的错误是 `Hash(secret || message)`。很多常见 hash，包括 MD5、SHA-1、SHA-256，都属于 Merkle-Damgard 风格的迭代结构。它们会把消息切块处理，最终 digest 本质上是内部链式状态的输出。如果攻击者知道 `Hash(secret || message)`，又能猜到 `secret` 的长度，就可能构造：

```text
message || padding || attacker_controlled_suffix
```

并计算出一个新的 digest，而不需要知道 `secret`。这就是 length extension attack。它对“拿裸 hash 当 MAC”的协议很致命。攻击者不能读出 secret，但可以伪造“原消息后面多了一段”的认证结果。

把 secret 放在后面也不是好设计。`Hash(message || secret)` 虽然不直接暴露同样的 length extension 入口，但它仍然不是标准 MAC。它没有清晰的安全证明，容易被编码歧义、字段拼接歧义、碰撞构造、不同消息边界解释等问题拖进去。比如：

```text
Hash("user=alice" || "role=user" || secret)
Hash("user=alicero" || "le=user" || secret)
```

如果协议没有长度前缀和 canonical encoding，两个不同业务含义可能落到同一串 bytes。HMAC 不能替应用修正所有编码问题，但它至少把 MAC 构造这件事从“自己拼字符串”变成了标准、分析过的算法。

HMAC 的另一个重点是密钥分离。`ipad` 和 `opad` 让内外两层 hash 处在不同域里，不会把同一份 key 直接暴露给同一类输入。内层输出被外层再次认证，攻击者不能自由选择外层 hash 的完整输入，因为外层输入前面有 `(K' XOR opad)`，而这部分未知。RFC 2104 的安全讨论也强调，HMAC 的安全性依赖底层 hash 和压缩函数的一些性质，但它和裸 hash 的碰撞问题不是同一个攻击面。RFC 6151 对 MD5 的更新也提到，MD5 已经不适合需要碰撞阻力的场景，不过 HMAC-MD5 的风险边界和“MD5 用于数字签名”不同；新系统仍然应该选 HMAC-SHA256 或更现代的 MAC，而不是因为 HMAC 结构较强就继续新用 MD5。

还要讲清楚 HMAC 提供什么、不提供什么。它提供消息完整性和数据来源认证，前提是双方共享的 key 没泄露。它不提供机密性，消息仍然是明文。它也不天然防重放：攻击者可以把旧的 `(message, tag)` 原样再发一遍。防重放要把 `timestamp`、`nonce`、`sequence number`、HTTP method、path、body hash、tenant、过期时间等一起放进被认证的消息里。

工程里正确的请求签名通常长这样：

```text
string_to_sign =
  method || "\n" ||
  path || "\n" ||
  canonical_query || "\n" ||
  body_sha256 || "\n" ||
  timestamp || "\n" ||
  nonce || "\n" ||
  tenant_id

tag = HMAC-SHA256(key, string_to_sign)
```

这样签的不是“某段 body 字符串”，而是请求语义。否则攻击者可能把同一段 body 搬到另一个 path、另一个 tenant、另一个时间窗口里复用。

结合 LogServe，如果以后给 SDK task submission 或 worker registration 做签名，我不会写 `sha256(secret + payload)`。我会定义 canonical request，把 `workflow_id`、`actor_id`、`operation`、`deadline`、`nonce`、`body_digest`、`key_id` 放进签名串，再用 HMAC-SHA256。服务端验证时先按 `key_id` 找 key，检查时间窗口和 nonce 是否已使用，再做常量时间比较。这样才能同时处理篡改和重放。

面试里可以这样回答：

```text
简单 hash 没有标准 MAC 语义，尤其是 Hash(secret || message) 会受 Merkle-Damgard length extension 影响，攻击者可能在不知道 secret 的情况下给消息追加内容并伪造 digest。HMAC 用 inner pad 和 outer pad 把 key 混入两层 hash，隐藏可扩展的内部状态，并把内外两层做了域分离。它提供完整性和来源认证，但不加密，也不自动防重放；nonce、时间戳、请求路径和业务上下文要一起被 HMAC 覆盖。
```

## Q007. 密码学哈希需要满足哪些性质？

密码学哈希首先是一个确定性的压缩函数：任意长度输入，输出固定长度摘要。同一串 bytes 在任何机器、任何时间、任何分块方式下都必须得到同一个 digest。这个基础性质看起来普通，但工程上很重要。哈希的输入是 bytes，不是 JSON 对象、字符串语义或业务记录。只要序列化、字符编码、字段顺序、大小写规范化不同，hash 结果就应该不同。

真正的安全性质主要有三个，NIST SP 800-107 也是这样描述 approved hash function 的。

第一是原像阻力。给定一个 digest，攻击者很难找到任意一条消息能 hash 到它。也就是知道 `h`，很难找到 `m` 使得：

```text
Hash(m) = h
```

这就是“单向性”的核心。它让 digest 不容易被反推成原文。但这句话有个边界：如果原文空间很小，比如 6 位数字验证码、常见密码、固定几种状态值，攻击者可以穷举。普通 SHA-256 再强，也救不了低熵输入。所以密码存储不能直接用 `SHA256(password)`。

第二是第二原像阻力。给定一条具体消息 `m1`，攻击者很难找到另一条不同消息 `m2`，让两者 digest 相同：

```text
Hash(m1) = Hash(m2), m1 != m2
```

这个性质常用于文件完整性和内容寻址。比如 LogServe 的 result object 如果已经以 SHA-256 digest 记录在 manifest 里，攻击者很难再找另一个不同对象冒充同一个结果。

第三是碰撞阻力。攻击者很难找到任意两条不同消息 `m1` 和 `m2`，让它们 hash 相同。注意，碰撞阻力比第二原像更强，因为攻击者可以同时选择两边的输入。数字签名特别依赖碰撞阻力：如果攻击者能构造两份 digest 一样但语义不同的文档，就可能让你签正常文档，再把签名挪到恶意文档上。NIST SP 800-107 也把数字签名的 hash 安全强度看作碰撞阻力强度，因为碰撞阻力通常是最短板。

输出长度会直接影响安全强度。对理想的 L-bit hash，原像和第二原像大致有 L bit 安全强度，碰撞阻力受 birthday bound 影响，大致只有 L/2 bit。SHA-256 输出 256 bit，碰撞强度约 128 bit；SHA-1 输出 160 bit，理论生日界是 80 bit，但公开分析和攻击让它在碰撞场景里更弱。

还有一些工程上必须补充的性质。

哈希要有良好的扩散效果，也就是输入微小变化会让输出不可预测地变化。不能有明显线性关系，不能让攻击者通过控制某几个字段来可预测地控制 digest。CRC32 就在这里和密码学哈希分开了：CRC 的变化有线性结构，适合误码检测，不适合对抗者模型。

哈希算法还要有明确的算法标识和版本。存储 digest 时只存裸值是不够的，至少要知道 `algorithm`、`digest_length`、`encoding`、是否截断、是否带 domain separation。否则系统升级时，旧的 SHA-1、新的 SHA-256、截断 SHA-256、BLAKE3 输出可能混在一起，恢复和迁移都会出错。

最后，密码学哈希没有密钥，所以它不证明来源。`SHA256(message)` 只能说明某个 digest 和 message 对应，不能说明是谁认可了 message。要来源认证，用 HMAC 或数字签名；要保密，用加密；要存密码，用 Argon2id、scrypt、bcrypt 这类密码哈希方案。

结合 LogServe，如果要给 log segment 或 snapshot 做内容指纹，我会把 hash 输入定义成稳定二进制编码，而不是直接 hash Go struct 或 JSON map。manifest 里记录 `sha256:<hex>` 这类带算法前缀的字段。跨版本升级时，新对象可以用更强算法，旧对象按旧算法验证，再通过后台迁移或读时重写升级。

面试里可以这样回答：

```text
密码学哈希至少要满足确定性、任意长度输入到固定长度输出、原像阻力、第二原像阻力和碰撞阻力。原像阻力防从 digest 反推任意原文，第二原像阻力防拿一个已知对象找替身，碰撞阻力防攻击者同时构造两份不同对象但 digest 相同。输出长度决定安全强度，碰撞场景通常只有输出位数的一半强度。哈希没有密钥，所以它不能单独提供来源认证，也不能直接用于密码存储。
```

## Q008. MD5 和 SHA-1 为什么不再适合安全场景？

MD5 和 SHA-1 的问题不是“它们算得不准”，而是它们已经不能给现代安全场景需要的碰撞阻力。它们仍然会稳定地产生 digest，但攻击者可以利用已知弱点构造不同输入得到相同 digest。安全场景关心的是主动攻击者，不是正常文件下载时偶然损坏。

MD5 输出只有 128 bit。即使把它当理想 hash，碰撞安全强度也只有大约 64 bit；实际情况更糟，因为 MD5 的碰撞攻击早已成熟。RFC 6151 明确更新了 MD5 的安全考虑：当应用需要碰撞阻力时，不应再使用 MD5，典型例子就是数字签名。历史上还出现过利用 MD5 chosen-prefix collision 伪造 X.509 证书的攻击，这说明它不是“理论上不好看”，而是能打到真实信任链。

SHA-1 输出 160 bit，曾经看起来比 MD5 稳很多。但 SHA-1 的碰撞安全也不够了。NIST SP 800-131A Rev. 2 对 SHA-1 的边界很清楚：用于数字签名生成时，除了 NIST 特定协议指导允许的例外，SHA-1 是 disallowed；数字签名验证只属于 legacy use；非数字签名场景只有在不需要碰撞阻力时才还能谈 acceptable。SHAttered 论文在 2017 年给出了完整 SHA-1 的公开碰撞实例，后续 chosen-prefix collision 研究又进一步压低了攻击门槛。对证书、软件包、签名文档、发布制品来说，这已经足够把 SHA-1 排除在新设计之外。

为什么碰撞这么重要？以签名为例，很多签名流程不是直接签任意长原文，而是签原文的 digest：

```text
signature = Sign(private_key, Hash(document))
```

如果攻击者能准备两份不同文档 `A` 和 `B`，让 `Hash(A) = Hash(B)`，他就可能让你签看起来正常的 `A`，再把签名放到恶意的 `B` 上。验证者只看到 digest 一致和签名有效，不一定知道你当初看的不是 `B`。

软件供应链里风险也类似。包管理器、镜像仓库、release 页面、SBOM、构建缓存、内容寻址系统如果使用弱 hash，攻击者就有机会制造“同 digest 不同内容”的对象。即使攻击成本不是零，供应链攻击的收益足够高，不能拿过时算法赌。

还有一个常见误区：MD5 或 SHA-1 不能用于安全，不等于它们在所有地方都会立即造成漏洞。比如非对抗场景下的历史文件指纹、快速去重预筛、老数据迁移校验，可能还会遇到 MD5。但这类使用必须明确“不提供安全语义”。只要 digest 会参与信任判断、签名、证书、包发布、权限、认证、审计证据，就不要用 MD5 或 SHA-1。

HMAC 场景要稍微精确一点。HMAC 的安全边界和裸 hash 碰撞不完全相同，RFC 6151 也没有说 HMAC-MD5 必须在所有遗留场景立刻停用。但新协议没有必要选择 HMAC-MD5 或 HMAC-SHA1。现实答案很简单：用 HMAC-SHA256、HMAC-SHA384、KMAC、BLAKE2 keyed mode，或者协议已经定义好的现代 MAC。

密码存储也不能用 MD5 或 SHA-1。这里的问题甚至不只碰撞，而是它们太快。攻击者拿到数据库后可以用 GPU/ASIC 每秒尝试海量候选密码。密码存储需要慢、带盐、可调成本，最好还要 memory-hard。`md5(password)`、`sha1(password)`、`sha256(password)` 都不合格。

结合 LogServe，如果要给 worker binary、container image、Python executor 插件或模型 adapter 做供应链校验，我不会接受 “MD5 一致就算可信”。至少要 SHA-256 digest，而且 digest 本身要来自可信渠道；更好的做法是签名发布，比如 Sigstore、cosign、GPG、TUF 这类机制。hash 只能证明内容和 digest 对应，不能证明发布者身份。

面试里可以这样回答：

```text
MD5 和 SHA-1 主要输在碰撞阻力。MD5 的碰撞和 chosen-prefix collision 已经非常成熟，RFC 6151 也明确不建议在需要碰撞阻力的地方使用。SHA-1 虽然输出 160 bit，但公开碰撞和后续 chosen-prefix 攻击已经让它不适合签名、证书、软件发布和供应链校验。它们可以出现在历史兼容或非对抗校验里，但不能参与新的安全决策。新系统用 SHA-256、SHA-384、SHA-3、BLAKE2/BLAKE3，认证场景用 HMAC 或签名。
```

## Q009. 盐 salt 和 pepper 的区别是什么？

salt 和 pepper 都会出现在密码存储里，但它们解决的问题不一样。salt 是每个密码记录自己的随机公开值；pepper 是系统级秘密，通常被多个密码共享，并且不能和密码 hash 放在同一个数据库里。

salt 的目标是让相同密码在不同账户上产生不同 hash，并阻止彩虹表和批量预计算。比如两个用户都用同一个密码，如果没有 salt：

```text
hash(password) = same_digest
```

数据库泄露后，攻击者一眼能看出两个人密码相同，还能对所有账户共用同一张预计算表。有了 salt 后：

```text
hash(password, salt_user_a) != hash(password, salt_user_b)
```

攻击者必须按每个 salt 分别猜。OWASP 的说法很直接：salt 是每个密码唯一的随机字符串，加入 hash 过程后，会让攻击者逐个破解 hash，而不是一次计算去撞整库。NIST 800-63B 也要求密码 verifier 使用适合的密码哈希方案，并存储 salt 和 hash；它给出的 salt 下限是至少 32 bit。工程上通常直接用密码哈希库生成更长的随机 salt，比如 128 bit 或更长，不需要自己发明格式。

salt 不需要保密。它应该和 hash、算法标识、成本参数一起存储，因为登录验证时必须拿同一个 salt 重算。隐藏 salt 没有必要，也不能把 salt 当安全边界。salt 泄露后，攻击者仍然不能预计算通用表，但可以针对某个用户离线爆破。

pepper 是另一层防线。它是系统持有的秘密值，不能放在同一张用户表里。OWASP 把 pepper 描述成 salting 之外的防御层：如果攻击者只拿到数据库备份或 SQL 注入导出的用户表，没有拿到 pepper，就很难离线验证密码猜测。NIST 800-63B 也建议 verifier 额外做一次 keyed hashing 或 encryption operation，并要求这个 secret key 与 password hashes 分开存放，最好放在 HSM、TEE 或类似硬件保护区域。

pepper 常见两种做法。一种是 pre-hash pepper：

```text
stored = PasswordHash(password || pepper, salt, cost)
```

另一种是 post-hash pepper：

```text
inner = PasswordHash(password, salt, cost)
stored = HMAC(pepper, inner)
```

后一种把 pepper 当 HMAC key，边界更清楚，也便于避免直接改变密码哈希算法内部输入语义。但不管哪种，都不要自己随手拼接字符串；要定义 bytes 编码，使用标准 HMAC 或成熟库。

pepper 的缺点也要讲出来。第一，它是共享秘密，一旦泄露，整库这层额外保护同时失效。第二，轮换困难。salt 可以为每个用户独立存在，pepper 换掉后，如果不知道用户明文密码，通常没法直接重新计算密码哈希；常见做法是在用户下次登录成功时升级，或者强制相关用户重置密码。第三，pepper 不能替代 salt。如果所有用户共享同一个 pepper 但没有 salt，相同密码仍然可能暴露相同模式，攻击者拿到 pepper 后也能批量攻击。

这两个概念可以用一句话区分：

```text
salt 是公开的 per-user 随机值，防预计算和相同密码同 hash。
pepper 是私密的 system-wide secret，防数据库单独泄露后的离线爆破。
```

结合 LogServe，如果以后有控制台用户、SDK token secret 或 worker enrollment secret，我会为每个 secret 存独立 salt、算法、成本参数和版本号。pepper 放到 KMS、HSM、Windows DPAPI、云 secret manager 或单独权限域里，而不是写进数据库或配置仓库。备份恢复时也要把 pepper 的恢复流程纳入演练，否则数据库恢复了但 pepper 丢了，用户就都登录不了。

面试里可以这样回答：

```text
salt 是每条密码记录唯一的随机公开值，和 hash 一起存，主要防彩虹表、批量预计算以及相同密码产生相同 hash。pepper 是系统级秘密，多个密码共享，必须和密码数据库分开存，常放在 KMS、HSM、TEE 或 secret manager 里。salt 泄露是预期情况，pepper 泄露是安全事故；salt 易迁移，pepper 轮换通常需要用户登录后重算或强制重置。两者都不能替代慢密码哈希。
```

## Q010. 密码存储为什么应该用 bcrypt、scrypt、Argon2？

密码存储的核心威胁是离线猜测。线上登录可以限流、封禁、MFA、风控；但数据库一旦泄露，攻击者可以在自己的机器上无限尝试候选密码。用户密码的熵通常不高，还会复用、带规律、出现在泄露字典里。这个时候，真正能拖慢攻击者的是每次猜测的成本。

普通 hash 不适合存密码，因为它们太快。SHA-256、SHA-1、MD5 的设计目标都是高效处理消息。高效对文件完整性、签名摘要、Merkle tree 是优点，对密码存储是缺点。攻击者拿到：

```text
stored = SHA256(password || salt)
```

之后，可以用 GPU 或专用硬件高速枚举候选密码。salt 能让他不能一次预计算打全库，但挡不住针对每个用户的高速猜测。密码存储需要的是 password hashing scheme，也可以理解成专门面向低熵 secret 的 KDF。

bcrypt、scrypt、Argon2 的共同点是：带 salt、带成本参数、验证时故意变慢，并且算法和参数可以随硬件变强而升级。NIST 800-63B 要求 password verifier 以抵抗离线攻击的形式存储密码，使用 password、salt、cost factor 作为输入的合适密码哈希方案，并且成本因子应尽可能高但不能影响 verifier 可用性。OWASP 也强调，密码应使用 Argon2id、bcrypt、PBKDF2 这类强慢哈希，而不是 SHA-256 这类快速 hash。

bcrypt 的价值是成熟和自适应成本。Provos 和 Mazières 在 1999 年的 bcrypt 论文里讲得很清楚：用户选择密码的长度和随机性不会随着硬件进步自动提高，所以密码方案必须能提高计算成本。bcrypt 基于 Blowfish 的 expensive key schedule，有 cost factor。cost 增大时，计算成本按指数级上升。它的工程生态很好，适合遗留系统继续使用。但它也有边界：很多实现对输入有 72 byte 限制，对 Unicode、预哈希、版本前缀要谨慎。新系统如果能选 Argon2id，通常不必从 bcrypt 起步。

scrypt 进一步把内存成本放进攻击模型。RFC 7914 说明 scrypt 基于 memory-hard functions，用来增加定制硬件攻击成本。它的参数包括 CPU/Memory cost `N`、block size `r`、parallelization `p`。攻击者如果想并行猜很多密码，不只要算力，还要给每个并行实例配内存带宽和内存容量。参数调得太低，memory-hard 的优势就没了；参数接受外部输入时还要防 DoS，不能让攻击者提交超大参数拖垮服务端。

Argon2 是更现代的选择。RFC 9106 描述 Argon2 为 memory-hard function，并给出 Argon2d、Argon2i、Argon2id 三个变体；其中 Argon2id 是主变体，兼顾 Argon2i 的侧信道防护和 Argon2d 对 GPU/时间-内存权衡攻击的抵抗。Argon2id 的参数包括内存大小 `m`、迭代次数 `t`、并行度 `p`、salt、输出长度。OWASP 当前建议新应用优先用 Argon2id，并给出最低配置参考；真实系统仍要在自己的服务器上压测，把登录延迟、CPU、内存、并发峰值和 DoS 风险一起算进去。

选择这些算法时，不能只写一行“用了 bcrypt”。要把参数、版本和迁移策略一起设计好：

```text
algorithm = argon2id
version = 0x13
memory = 64 MiB
iterations = 3
parallelism = 1
salt = random 128-bit+
pepper_version = 2026-06
hash = ...
```

登录时先按记录里的算法和参数验证。验证成功后，如果发现旧参数太弱，就用新参数重算并更新。这样能平滑迁移，不需要一次性强制所有用户重置密码。失败路径要常量时间化到合理程度，并配合限流，避免把密码哈希变成 CPU 或内存 DoS 入口。

还要注意，慢哈希只保护离线泄露后的猜测成本，不解决所有认证问题。弱密码仍然弱，所以要做泄露密码 blocklist；在线猜测要限流；高风险操作要 MFA；传输要 TLS；会话要安全 cookie 或 token；日志不能记录明文密码。密码哈希是认证系统的一层，不是全部。

结合 LogServe，如果只是本地实验系统，可能没有完整用户体系。但如果以后加 Web 控制台、租户账号、SDK access secret、worker bootstrap secret，就不能把 secret 明文放 SQLite、YAML 或日志里。用户密码用 Argon2id；机器 token 更适合随机生成高熵 secret，然后存储 HMAC 或 password-hash 形式的 verifier；管理员初始密码必须强制首次修改。这样说边界更稳：实验阶段可以没有生产级 IAM，但一旦进入用户认证，就要按离线泄露模型设计。

面试里可以这样回答：

```text
密码存储要防的是数据库泄露后的离线爆破。普通 SHA-256、MD5、SHA-1 太快，攻击者可以用 GPU 大规模猜密码；salt 只能防预计算，不能让单次猜测变贵。bcrypt、scrypt、Argon2id 这类密码哈希算法带 salt 和成本参数，bcrypt 提供可调计算成本，scrypt 和 Argon2id 还引入内存成本，提高 GPU/ASIC 并行破解代价。参数要随硬件升级，验证成功时可重哈希迁移，同时配合限流、MFA、密码 blocklist 和安全传输。
```

## Q011. API token 应该如何存储和轮换？

API token 要先按威胁模型看成 secret，而不是普通配置项。只要一个 token 拿到后就能直接调用接口，它就是 bearer credential：谁持有，谁就能用。RFC 6750 对 bearer token 的安全假设很直接，客户端不需要再证明自己持有额外密钥，所以泄露后的风险主要靠作用域、有效期、撤销和存储边界来控制。

服务端存 token 时，最好不要保存明文 token。常见做法是把 token 分成两段：前缀或 token id 用来定位记录，后半段才是真正的随机秘密。数据库里保存 token id、owner、scope、audience、expires_at、created_at、last_used_at、status、key_version，以及对 secret 部分计算出的 verifier。verifier 可以是 `HMAC(server_pepper, token_secret)`，也可以是适合该场景的慢哈希；机器 token 通常是高熵随机值，不一定需要像用户密码那样用很慢的 Argon2id，但至少不能只存明文。明文 token 只在创建时展示一次，之后系统只能重置或重发新 token，不能“查看旧 token”。

一个比较稳的格式长这样：

```text
-visible prefix-
  lgs_pat_01HZX...        用于识别系统、类型和 token id，泄露后不应直接可用
-secret part-
  256-bit random          只展示一次，客户端保存
-server record-
  token_id
  owner_type / owner_id
  scopes
  audience
  expires_at
  status
  verifier = HMAC(pepper_v3, secret_part)
```

客户端怎么存，要看客户端是什么。后端服务、worker、CI job 应该从 secret manager、KMS、Vault、Kubernetes Secret 加密方案、云 Secret Manager 或受控的运行时注入里取，不要写进 Git、镜像、日志、README、命令行历史。浏览器前端不应该保存长期 API token；如果是用户会话，更常见的是 `HttpOnly`、`Secure`、`SameSite` cookie，加 CSRF 防护和服务端会话或短期 token。移动端要用系统安全存储，比如 Keychain、Android Keystore。CI/CD 场景能用 OIDC federation 换短期云凭证时，不要长期保存云访问密钥。

轮换不要只理解成“改一个字符串”。真正的轮换需要有重叠窗口和可观测性。先发新 token，让客户端切流；系统同时接受旧 token 和新 token 一段时间；通过 `last_used_at` 和审计日志确认旧 token 不再被使用；再撤销旧 token。如果没有重叠窗口，就容易把线上服务打断。如果旧 token 怀疑泄露，流程要更急：缩短重叠窗口甚至立即撤销，同时根据审计日志查它访问过哪些资源。

轮换策略通常分几类：

```text
- 定期轮换：比如 30 天、90 天或按合规要求。
- 事件轮换：人员离职、仓库泄露、日志泄露、供应链事故、权限变更。
- 自动轮换：secret manager 定期生成新版本，应用热加载或滚动重启。
- 使用中轮换：refresh token rotation，每次使用 refresh token 都发一个新 refresh token，旧值立即失效。
```

轮换还要考虑权限和生命周期。token 应该有明确用途，不要一个 token 同时能读写所有资源。一个 SDK token 只能提交任务，就不要让它读所有 workflow 的 result object；一个 worker enrollment token 只能注册指定 worker 池，就不要让它访问元数据表；一个内部 service token 只面向一个 audience，就不要在多个服务间复用。作用域越小，泄露后的爆炸半径越小。

日志和监控要小心。不要记录 `Authorization`、`Cookie`、`X-Api-Key`、查询参数里的 token、异常堆栈中的连接串。为了排查问题，可以记录 token id、前缀、owner、scope、request id，或者记录 `HMAC(log_key, token)` 的短截断值做关联。不要记录原始 token 的 SHA-256，因为攻击者如果拿到候选 token，可以离线对照；用日志专用 HMAC key 更稳。

结合 LogServe，如果以后给 SDK、worker、executor 或管理控制台发 token，我会把 token 当成一等对象管理，而不是放在配置文件里凑合用。数据库只存 verifier 和元数据；每个 token 有 owner、scope、audience、过期时间、最后使用时间；worker 只拿 worker 需要的权限；SDK token 只能访问本租户或本项目的 workflow；日志里只出现 token id 或 HMAC 后的指纹。这样即使某个 token 泄露，也能快速定位、撤销和评估影响。

面试里可以这样回答：

```text
API token 要按 bearer secret 处理。服务端不要存明文，应该存 token id、owner、scope、audience、过期时间、状态和 verifier，明文只在创建时展示一次。客户端不要把 token 写进仓库、镜像、日志和命令行历史，服务端程序应从 secret manager 或受控运行时注入读取。轮换时先发新 token，再让客户端切流，保留短暂重叠窗口，确认旧 token 不再使用后撤销；如果怀疑泄露，就立即撤销并按审计日志评估影响。最重要的是短有效期、最小 scope、可撤销、可审计、可自动化。
```

## Q012. OAuth2 和 API key 的主要差异是什么？

API key 更像一个简单的静态凭证。服务端给调用方一个字符串，调用方每次请求带上它，服务端查表判断这个 key 属于谁、能不能访问。它的优点是实现简单、依赖少、适合服务端到服务端的低复杂度场景。缺点也明显：很多 API key 没有标准化授权流程，没有用户同意页，没有 refresh token，没有统一的 scope 语义，没有标准撤销端点，也经常被当成永久口令使用。

OAuth2 不是“更复杂的 API key”，它是授权框架。RFC 6749 里的核心角色包括 resource owner、client、authorization server、resource server。客户端不是直接拿用户密码访问资源，而是通过授权服务器拿到 access token。这个 token 可以有 scope、lifetime 和其他访问属性。资源服务器看到 token 后，根据 token 的有效性、scope、audience、subject 等信息决定是否允许访问。

差异可以按几个维度记：

```text
API key:
  通常代表某个应用、项目或调用方。
  往往是长期静态 secret。
  权限模型由服务自己定义，粒度经常比较粗。
  撤销和轮换通常是产品自定义接口。

OAuth2:
  用授权服务器签发 token。
  支持授权码、client credentials、device flow 等不同 grant。
  token 可以短期有效，并带 scope、audience、issuer、subject。
  refresh token、revocation、introspection 都有标准扩展。
  适合第三方委托访问和统一身份/授权体系。
```

OAuth2 的强项是委托授权。比如用户允许一个第三方应用读取相册，不需要把自己的用户名密码给第三方；授权服务器发一个只允许读相册、有效期有限、可撤销的 access token。API key 很难自然表达这个过程。它通常只能表达“这个 key 属于某个应用”，而不是“这个用户在这个时间允许这个应用访问这一小部分资源”。

机器到机器场景也可以用 OAuth2。client credentials grant 让服务用自己的 client credential 向授权服务器换 access token。这样资源服务器不需要认识所有客户端的长期 secret，只需要验证授权服务器签发的 token。再配合 RFC 7662 token introspection 或 JWT access token profile，资源服务器可以用统一方式检查 token 的 active 状态、scope、client_id、subject 和 audience。

但 OAuth2 也不是登录协议本身。它解决的是授权，不是完整用户认证语义。很多系统用“OAuth 登录”这个说法，其实需要 OpenID Connect 在 OAuth2 之上提供 ID Token 和用户身份声明。面试里要把这点说清楚：access token 是拿来访问资源的，不等于你能随便把它当用户资料凭证；ID Token 是给 client 识别登录用户的，也不应该拿去调用资源 API。

API key 也不是一定不安全。它适合范围很明确的内部服务、管理脚本、个人访问令牌、简单开发者 API。关键是不要把它做成“永不过期、全局读写、无法审计”的万能钥匙。好的 API key 系统也应该有 scope、过期时间、last used、前缀识别、明文只展示一次、撤销接口和日志脱敏。这样它虽然不是 OAuth2，也能有合理的工程边界。

结合 LogServe，如果只是本地实验或一个内部 SDK，API key 可能足够；但如果变成多租户控制台、第三方集成、用户授权 worker 或外部应用访问 result object，OAuth2/OIDC 这类体系更合适。SDK 可以使用短期 access token；worker 可以使用 client credentials 或 workload identity；资源服务器只根据 token 的 issuer、audience、scope 和 tenant 做授权，不把用户密码或长期密钥分发给每个组件。

面试里可以这样回答：

```text
API key 是简单静态凭证，通常代表一个调用方或项目，实现容易，但委托授权、scope、过期、撤销和审计都要自己设计。OAuth2 是授权框架，客户端通过授权服务器拿短期 access token，token 可以带 scope、audience、lifetime，并能配合 refresh token、revocation、introspection 等机制管理生命周期。OAuth2 更适合第三方委托访问和统一授权；API key 适合简单机器调用，但也必须做最小权限、过期、轮换和日志脱敏。
```

## Q013. JWT 的优缺点是什么？

JWT 的价值在于把一组 claim 放进一个紧凑、URL-safe 的 JSON token 里，并通过 JWS 签名或 JWE 加密保护。实际系统里最常见的是 signed JWT，也就是 header、payload、signature 三段。资源服务器拿到 JWT 后，可以本地验证签名、issuer、audience、expiration 等字段，不一定每次都回授权服务器查状态。这对微服务、边缘网关、跨语言系统和多机房部署很方便。

优点第一是可分布式验证。授权服务器用私钥签发 token，资源服务器通过 JWKS 获取公钥并缓存。只要签名、`iss`、`aud`、`exp` 等验证通过，资源服务器就能判断 token 是否来自可信 issuer，是否发给自己，是否还在有效期内。这个模型减少了中心 introspection 服务的压力，也降低了每次 API 调用的延迟。

优点第二是自包含。JWT 可以携带 subject、tenant、scope、role、session id、token id、认证强度、授权上下文等 claim。资源服务器不必每次查数据库才能知道调用方的大致身份和权限。当然，这也是双刃剑：claim 一旦发出去，在 token 过期前就很难改。

优点第三是互操作性。RFC 7519 定义了 `iss`、`sub`、`aud`、`exp`、`nbf`、`iat`、`jti` 等注册 claim，JOSE 生态定义了 JWS、JWE、JWK、JWA。不同语言和网关都能处理 JWT。对大型系统来说，这比每个服务自定义 token 格式要好维护。

缺点也要直说。第一个缺点是泄露后通常可以被重放。除非 token 被绑定到 mTLS 证书、DPoP key 或其他 proof-of-possession 机制，否则 signed JWT 仍然是 bearer token。攻击者拿到它，在过期前就可能直接调用接口。签名只能证明 token 没被改，不证明当前持有者就是原始客户端。

第二个缺点是撤销困难。JWT 的常见优势是本地验证，不查中心状态；但这也意味着授权服务器想让一个已经签发的 JWT 立刻失效时，资源服务器未必知道。可以加 denylist、`jti` 黑名单、session version、introspection、短有效期、刷新令牌轮换，但这些都会引入状态、缓存和传播延迟。

第三个缺点是验证容易写错。RFC 8725 专门列了 JWT 的最佳实践，比如算法验证、合适算法、验证 issuer/subject/audience、不要信任未验证 claim、不同 JWT 类型使用互斥验证规则。常见事故包括接受 `alg=none`、把 HMAC secret 和 RSA public key 混用、只验签不验 `aud`、不验 `iss`、把 ID Token 当 access token、把一个服务的 token 拿到另一个服务用。

第四个缺点是 JWT 默认不保密。signed JWT 的 payload 只是 base64url 编码，不是加密。日志、浏览器调试工具、代理、错误上报系统都可能看到里面的 claim。如果把邮箱、手机号、权限细节、租户信息、内部资源 id 放进去，就会扩大隐私和信息泄露面。需要机密性时要用 JWE 或者不要把敏感字段放进 token。

第五个缺点是体积和耦合。JWT 比 opaque token 大，放进 HTTP header 会增加流量；claim 设计如果随业务膨胀，网关、后端、客户端会逐渐依赖 token 内部结构，后续改模型很痛苦。JWT 适合放稳定、安全决策需要的最小 claim，不适合当用户资料缓存或权限大杂烩。

结合 LogServe，如果以后要让多个组件验证 SDK 或 worker token，JWT 可以降低中心状态依赖。比如 control plane 签发短期 access token，worker、executor、metadata API 验证 `iss`、`aud`、`exp`、`scope`、`tenant_id`。但 result object 的细粒度权限不要全塞进 JWT，仍应在服务端按 workflow、actor、task ownership 做授权。JWT 可以帮忙传递身份和粗粒度授权，不应该替代资源级授权判断。

面试里可以这样回答：

```text
JWT 的优点是紧凑、自包含、可签名、跨语言、资源服务器可以本地验证，适合分布式系统里的短期 access token。缺点是泄露后通常可重放，默认不保密，claim 可能过期或膨胀，撤销困难，验证规则也容易写错。安全使用时要短有效期、固定算法、验证 iss/aud/exp/nbf、区分 ID Token 和 access token，不把敏感信息放进 payload，并为撤销或会话失效设计额外机制。
```

## Q014. JWT 失效和撤销为什么困难？

JWT 难撤销，根源在于它经常被设计成 self-contained token。资源服务器拿到 token 后，只要能用本地公钥验签，再检查 `exp`、`nbf`、`aud`、`iss`、scope，就认为它有效。这个过程不需要访问授权服务器，也不需要查数据库。性能和可用性上这是优点，但撤销时就变成问题：授权服务器改了状态，资源服务器如果不查状态，就不知道。

过期时间只能解决“最终失效”，不能解决“立刻失效”。假设 JWT 有效期 30 分钟，用户点击退出登录、管理员禁用账号、token 泄露、权限被收回，在剩余 30 分钟里资源服务器仍可能接受这个 token。把有效期调到 5 分钟可以缩短窗口，但不能把窗口变成零；而且有效期越短，刷新频率越高，对客户端体验和授权服务器压力都有影响。

撤销列表是常见方案，但它把 JWT 拉回有状态模型。资源服务器需要检查 `jti` 或 session id 是否在 denylist 里。denylist 要分发到所有资源服务器，缓存要更新，跨机房要同步，还要设置过期清理。撤销列表如果查得太慢，会抵消 JWT 本地验证的优势；如果缓存时间太长，又会留下撤销延迟。

token version 或 session version 也常见。用户表或会话表里保存一个版本号，JWT 里带 `session_version` 或 `token_version`。用户改密码、退出所有设备、管理员禁用时递增版本号，资源服务器查到 JWT 里的版本低于当前版本就拒绝。这个方案适合用户级或会话级撤销，但资源服务器仍然要查状态或缓存状态，对“撤销某一个 token”不如 `jti` 精细。

密钥轮换能让一批 JWT 失效，但太粗。把签名私钥换掉，并让资源服务器不再信任旧公钥，所有旧 token 都会失效。这适合签名密钥泄露或严重事故，不适合用户退出登录。因为它会影响所有使用旧 key 签发的 token，等于大范围强制登出。

introspection 是另一条路。RFC 7662 定义资源服务器向授权服务器查询 token 当前状态，返回 token 是否 active 以及相关元数据。如果每次都 introspect，撤销可以比较及时，但 token 就不再是完全本地验证，授权服务器会成为运行时依赖。工程上常用折中：高风险接口查 introspection，普通接口用短期 JWT 本地验签；或者 JWT 很短期，refresh token 有状态且可撤销。

refresh token 和 access token 要分开看。access token 应该短期，用来调用资源；refresh token 更长期，用来换新 access token，因此必须严格保护、可撤销、最好做 rotation。refresh token rotation 的思路是每次使用 refresh token 都换一个新值，旧值立即失效；如果旧值再次出现，说明可能泄露，可以撤销整条授权链。

还有一个容易忽略的点：JWT 的 claim 已经发出后不会自动更新。用户角色被降级、租户被禁用、项目被归档、资源 owner 变化，这些状态如果只靠 JWT 里的旧 claim，都会在 token 过期前产生过时授权。细粒度权限、资源归属、封禁状态，不适合完全依赖长期 JWT claim。

结合 LogServe，如果以后用 JWT 做 SDK access token，我会把 access token 做短期，比如几分钟到十几分钟；refresh token 或 API key verifier 留在服务端，可撤销、可审计。worker 侧访问具体 task/result 时，不只看 JWT 里的 `scope=worker`，还要检查这个 worker 是否被分配了该 task。需要立即撤销的后台管理操作，要么查服务端状态，要么使用 opaque session/token，而不是只靠长生命周期 JWT。

面试里可以这样回答：

```text
JWT 难撤销是因为它通常自包含，资源服务器本地验签后就接受，不会每次问授权服务器当前状态。exp 只能保证最终过期，不能保证用户退出、权限收回或 token 泄露后立刻失效。要做撤销，就需要短有效期、jti denylist、session/token version、introspection、refresh token rotation 或密钥轮换；这些机制都会引入状态、缓存、传播延迟或更大的运维成本。结论是：JWT 适合短期 access token，不适合承载必须立即撤销的长期会话。
```

## Q015. RBAC 和 ABAC 的区别是什么？

RBAC 是 role-based access control，核心是角色。用户或服务主体被分配到角色，角色再被授予权限。权限不是直接散落在每个用户身上，而是挂在角色上。NIST RBAC 模型里会讨论 user、role、permission、operation、object、session、role hierarchy、constraints 这些概念。它的工程价值很直接：权限管理跟组织结构或职责绑定，审计和授权配置更容易解释。

举个简单例子：

```text
role: admin
  permissions: workflow:read, workflow:delete, user:create

role: developer
  permissions: workflow:read, workflow:submit

subject alice has role developer
```

这种模型适合“按岗位分工”的系统，比如管理员、普通用户、审计员、运维、只读观察员。它也适合后台管理系统的粗粒度权限。缺点是角色容易膨胀：当权限需要同时考虑租户、资源 owner、时间、环境、项目状态、数据分类时，单靠角色会变成 `tenantA_projectB_admin_readonly_temp` 这类组合爆炸。

ABAC 是 attribute-based access control，核心是属性和策略。NIST SP 800-162 对 ABAC 的定义是：授权决策由 subject、object、operation，有时还包括 environment condition 的属性，与策略、规则或关系一起评估得出。也就是说，角色只是 subject 的一个属性，不再是唯一入口。

ABAC 的策略可能长这样：

```text
allow if
  subject.tenant_id == object.tenant_id
  and action in ["read", "write"]
  and object.status != "archived"
  and subject.assurance >= "mfa"
  and request.time within business_hours
```

ABAC 的优势是细。它可以自然表达多租户隔离、资源归属、数据分级、设备可信度、来源网络、认证强度、时间窗口、审批状态。比如“用户只能读自己租户下的 workflow”，“worker 只能写自己领取的 task 的 result”，“管理员只有完成 MFA 后才能导出数据”，这些用 ABAC 表达比用纯角色表达更清楚。

ABAC 的代价是复杂。第一，属性来源必须可信。`tenant_id`、`owner_id`、`device_trust`、`mfa_level` 如果能被调用方随便传，那策略就没意义。第二，策略要可测试、可审计、可解释，否则线上拒绝访问时很难排查。第三，策略引擎和属性查询会带来性能和缓存问题。第四，策略版本变化可能影响大量资源访问，需要灰度、回滚和审计。

所以实际系统常用混合模型。RBAC 管粗粒度职责，ABAC 管上下文和资源边界。比如先判断调用方是不是 `developer` 或 `worker`，再判断 `subject.tenant_id == object.tenant_id`，再判断 action 是否在 scope 内。这样既能保留角色的可管理性，又能避免角色爆炸。

结合 LogServe，RBAC 可以定义 `admin`、`developer`、`worker`、`auditor` 这类角色。ABAC 则用来表达更关键的资源边界：同一个租户、同一个 workflow、actor ownership、task assignment、result object 所属 project、worker pool、环境是 dev 还是 prod。尤其是 worker，不应该因为有 `worker` 角色就能读所有任务；它还必须满足“这个 task 被分配给它”这个属性条件。

面试里可以这样回答：

```text
RBAC 按角色授权，用户或服务拿到角色，角色对应权限，适合岗位清晰、权限相对稳定的系统，优点是简单、可审计，缺点是细粒度场景容易角色爆炸。ABAC 按属性和策略授权，会同时看主体属性、资源属性、动作和环境条件，适合多租户、资源归属、时间、认证强度等动态约束，缺点是属性可信性、策略复杂度、测试和性能成本更高。实际工程常用 RBAC 做粗粒度职责，ABAC 做资源级和上下文级限制。
```

## Q016. 最小权限原则如何落实到服务间调用？

最小权限不是一句“只给必要权限”就结束了，它要落到身份、接口、资源、动作和生命周期上。服务间调用最常见的错误是所有服务共享一个内部 token、一个数据库账号、一个对象存储 AK/SK，或者 mTLS 认证通过后就默认全权访问。这样做开发快，但一旦某个服务被打穿，攻击者就能横向移动。

第一步是给每个服务独立身份。身份可以是 service account、workload identity、SPIFFE ID、mTLS client certificate、OAuth2 client credential 或云 IAM role。不要让 `control`、`worker`、`executor`、`api-gateway` 共用同一套 secret。身份要能在日志和审计里看到：谁在什么时间，以什么身份，对哪个资源做了什么动作。

第二步是画调用矩阵。不是先写代码再补权限，而是先列出来：

```text
subject      action              resource
api          submit_task          workflow in same tenant
worker       claim_task           task in assigned queue
worker       read_payload         task assigned to this worker
worker       write_result         result for assigned task
executor     read_snapshot        actor snapshot for assigned task
executor     write_checkpoint     checkpoint for assigned actor
auditor      read_log             selected tenant, read-only
```

这个矩阵会暴露很多过宽权限。比如 worker 需要写 result，不代表它需要删除 workflow；executor 需要读某个 snapshot，不代表它需要列出所有对象存储 bucket；API 服务需要写 metadata，不代表它应该能读数据库里的 secret 表。

第三步是把权限做成可执行的策略。REST/gRPC 层要按 method 和 resource 做授权；消息队列要按 topic/subject/queue 做 publish/consume 限制；对象存储要按 bucket、prefix、object tag 做限制；数据库要分账号、schema、表、读写权限；secret manager 要按 secret path 和版本授权。网络层的 allowlist 或 service mesh policy 只能说明“能连上”，不能替代业务授权。

第四步是短期凭证和下放权限。服务 A 调服务 B 时，不要把用户的长期 token 原样传遍全链路。可以使用 token exchange、downscoped token、per-request capability、短期签名 URL、临时云凭证。下游只拿到完成当前动作所需的最小权限和最短有效期。对于高风险操作，还可以要求额外的 actor、reason、approval id 或 MFA 级别。

第五步是默认拒绝和逐请求检查。OWASP Authorization Cheat Sheet 强调 deny by default 和每个请求都验证权限。服务间调用也一样：不能因为请求来自内网、来自同一个 Kubernetes 集群、通过了 mTLS，就跳过授权。mTLS 证明对端身份，授权还要问“这个身份能不能对这个资源做这个动作”。

第六步是把权限纳入测试和审计。权限策略要有单元测试、集成测试和回归测试，特别是水平越权：A 租户 worker 能不能读 B 租户 result，worker 能不能提交不是自己领取的 task，普通 SDK token 能不能列出管理员接口。审计日志要记录拒绝事件，因为被拒绝的调用经常是攻击或错误配置的早期信号。

最后是定期收敛权限。服务上线初期经常为了排障给宽权限，后面忘了收回。需要用 last-used 权限分析、IAM access analyzer、审计查询、密钥库存来发现“从没用过的权限”和“已经不属于这个服务的权限”。权限评审不是只看用户账号，机器身份同样要评审。

结合 LogServe，最小权限可以这样落地：SDK token 只能提交或查询自己 tenant 下的 workflow；control plane 可以调度任务，但不能读取 secret 明文；worker 只能 claim 自己队列里的 task，只能写被分配 task 的 result；Python executor 只能访问本次执行需要的输入、snapshot 和临时目录；对象存储凭证按 workflow 或 object prefix 下发短期写入权限。这样 worker 或 executor 被攻击时，不会自然获得全系统读写能力。

面试里可以这样回答：

```text
服务间最小权限要从独立服务身份开始，然后建立 subject-action-resource 调用矩阵，把每个接口、队列、对象存储 prefix、数据库表和 secret path 的权限最小化。mTLS 或网络连通只解决身份和通道，不等于授权；每个请求仍要按资源和动作做 deny-by-default 检查。凭证尽量短期、可下放、可审计，避免共享万能 token。最后要用权限测试、审计日志和定期评审收敛权限，防止服务被打穿后横向移动。
```

## Q017. mTLS 能解决服务身份问题吗？

mTLS 能解决一部分服务身份问题，但不能把身份、授权、隔离和运维全包了。TLS 1.3 的基本目标包括认证、机密性和完整性；普通 HTTPS 一般只认证服务端，客户端匿名。mTLS 则让服务端也验证客户端证书，双方都能证明自己持有对应私钥，并且证书链能被信任根验证。

所以 mTLS 能回答的问题是：当前连接对端是否持有某个证书对应的私钥？这个证书是否由我信任的 CA 或 trust bundle 签发？证书里的 SAN、URI、SPIFFE ID、DNS name 是否符合我期待的身份？如果这些都成立，服务就有了比 IP 地址、固定网络段、共享 token 更强的工作负载身份基础。

但 mTLS 不自动回答“它能做什么”。证书证明的是身份，不是权限。`spiffe://prod/ns/default/sa/worker` 能连到 control plane，不代表它可以读取所有 tenant 的结果。授权仍需要策略：这个 workload identity 能访问哪些 API、哪些 topic、哪些 object prefix、哪些 secret。身份认证和授权判断要分开。

mTLS 也依赖证书签发和身份绑定。如果注册流程很宽，任何 workload 都能拿到 `worker` 证书，那 mTLS 只是把错误身份强认证了一遍。真正难的是 attestation：这个进程是不是预期镜像、预期 service account、预期命名空间、预期节点上运行的 workload。SPIFFE/SPIRE 这类体系的价值就在这里：用 workload attestation 签发短期 SVID，再用 SPIFFE ID 表示工作负载身份。

证书私钥泄露也是现实问题。mTLS 证明“谁持有私钥”，如果私钥被复制到攻击者机器，攻击者也能通过认证。缓解方式包括短期证书、私钥文件权限、硬件或内核密钥保护、sidecar 代持、自动轮换、异常使用检测、证书吊销或快速移除 trust。证书不是一次生成后永久安全。

TLS 终止位置也要明确。如果 mTLS 终止在网关、sidecar 或负载均衡器，后端应用看到的可能只是代理请求。此时有两个选择：代理负责完整授权；或者代理把经过认证的对端身份以受保护方式传给应用。不能让应用盲信任普通 HTTP header 里的 `X-Client-Cert` 或 `X-User`，否则攻击者绕过代理就能伪造身份头。

mTLS 还解决不了用户身份。服务 A 代表用户调用服务 B 时，mTLS 只能说明“服务 A 是谁”，不能说明“最终用户是谁、用户是否同意、用户是否有权限访问该资源”。用户上下文需要 OAuth2 access token、session、JWT、或其他上层机制，并且要防 confused deputy：服务 A 不能因为自己有高权限就替低权限用户访问不该访问的资源。

结合 LogServe，mTLS 适合保护 control、worker、executor、metadata API、object gateway 之间的通道和服务身份。比如 worker 证书里带 SPIFFE ID，control plane 根据 SPIFFE ID 识别 worker pool。但控制面仍要检查这个 worker 是否被允许 claim 某个队列，是否被分配了这个 task，是否能写这个 result object。mTLS 是身份地基，不是完整授权系统。

面试里可以这样回答：

```text
mTLS 可以强认证服务端和客户端，提供加密、完整性和基于证书的工作负载身份，比 IP allowlist 或共享 token 更可靠。但它只证明对端持有某个受信证书，不自动说明它能访问哪些资源。还要解决证书签发、工作负载绑定、私钥保护、轮换、TLS 终止、代理传递身份以及资源级授权。简短说，mTLS 能做服务身份的底座，不能替代 IAM 和授权策略。
```

## Q018. 证书轮换如何避免中断？

证书轮换避免中断的关键是重叠、自动化和分阶段信任。证书不是到期当天才换。到期当天换证书，任何一个客户端缓存、长连接、负载均衡器热加载失败、时钟偏差、trust bundle 未更新，都可能把服务打断。好的轮换流程要提前签发、提前分发、双信任、热加载、观察，再撤旧。

先区分两类轮换：leaf certificate 轮换和 CA/root 轮换。leaf 轮换是服务自己的证书快到期或私钥要换；CA/root 轮换是签发体系本身变了。leaf 轮换通常影响较小，只要客户端信任链没变，服务端加载新证书即可。CA/root 轮换更危险，因为所有对端的 trust bundle 都要先认识新 CA，否则新证书一上线就会被拒绝。

leaf 证书轮换的安全流程一般是：

```text
1. 在旧证书过期前足够早签发新证书。
2. 把新证书和新私钥部署到服务实例。
3. 服务热加载证书，或滚动重启实例。
4. 新连接使用新证书，旧连接自然 drain。
5. 监控 TLS 握手失败、证书剩余有效期、客户端错误。
6. 确认全量实例都使用新证书后，清理旧私钥。
```

如果服务不能热加载，就要滚动重启。滚动重启时配合 readiness probe、连接 drain、负载均衡摘流，避免把正在处理请求的实例直接杀掉。长连接系统还要主动控制最大连接寿命，否则老连接可能一直挂在旧证书时代的状态上；不过 TLS 连接建立后，证书到期通常不影响已经建立的连接，问题主要发生在新握手。

CA/root 轮换要分两阶段，不能一步到位：

```text
阶段 A：扩展信任
  所有客户端和服务端 trust bundle 同时信任 old root 和 new root。
  此时 leaf 仍可由 old root 签发。

阶段 B：切换签发
  新 leaf 改由 new root 或 new intermediate 签发。
  所有对端因为已经信任 new root，所以不会中断。

阶段 C：收缩信任
  等旧 leaf 全部过期或撤销，再从 trust bundle 移除 old root。
```

mTLS 场景更要注意双方。服务端证书换了，客户端要信任；客户端证书换了，服务端也要信任。任何一方 trust bundle 没更新，握手都会失败。服务网格、SPIFFE、cert-manager、ACME 这类自动化系统的价值就是把签发、分发、续期和热加载做成常规流程，而不是人工临时操作。RFC 8555 的 ACME 也是为自动化证书管理设计的标准协议。

轮换还要避免过度证书绑定。客户端如果 pin 了单个 leaf certificate，服务端一换证书就会失败。更稳的是 pin CA、pin 公钥集合，或者用受控 trust bundle。OAuth mTLS 的 certificate-bound access token 场景还要额外注意：token 绑定到某个客户端证书指纹时，证书轮换期间可能需要让新旧证书各自能换到对应 token，或者缩短 token lifetime，避免旧证书过期后 token 还在流通。

监控和演练不能省。至少要监控证书剩余有效期、续期失败、TLS 握手失败率、证书链验证失败、OCSP/CRL 异常、客户端版本分布。轮换 runbook 要能回答：哪个服务持有哪些证书，谁签发，什么时候到期，如何手动续期，如何回滚，旧私钥如何销毁，泄露时如何吊销。

结合 LogServe，如果以后为 control、worker、executor 做 mTLS，我会优先用短期证书和自动续期。worker sidecar 或 agent 持有证书并热加载；control plane 信任一个 trust bundle；CA 轮换先把新 CA 加入所有节点，再切换签发，最后移除旧 CA。任务运行中的长连接通过 drain 平滑退出，不能在证书快过期时强杀 worker。

面试里可以这样回答：

```text
证书轮换避免中断靠提前、重叠和分阶段。leaf 证书要在过期前签发新证书，部署后热加载或滚动重启，让新连接用新证书、旧连接自然 drain。CA/root 轮换要先让所有节点同时信任新旧根，再用新根签发 leaf，等旧证书全部退出后再移除旧根。mTLS 要同时考虑客户端和服务端 trust bundle。配合自动化续期、到期监控、TLS 失败告警和演练，避免到期当天人工救火。
```

## Q019. 日志中泄露 token 会造成什么风险？

日志里泄露 token，风险通常比“普通日志里多了一段字符串”严重得多。token 是凭证，尤其是 bearer token。谁拿到它，谁就可能在有效期内直接调用接口。日志系统又有一个特点：它会被复制、索引、聚合、备份、转发给 SIEM、APM、错误上报、客服排障平台，访问日志的人和系统往往比生产数据库更多。一个 token 进了日志，实际扩散范围很难立刻说清。

最直接的风险是重放。攻击者拿到 access token，就能以该用户、该应用或该服务身份访问资源。如果 token scope 很宽，可能读写大量数据；如果 token 是 refresh token 或长期 API key，风险持续时间会更长。即使是短期 token，也足够完成一次敏感操作、导出数据或换取更长期凭证。

第二个风险是横向移动。内部服务 token 泄露在日志里，攻击者可能从日志平台跳到 API、对象存储、消息队列、secret manager。很多组织对日志平台的访问控制比生产系统宽，因为开发、测试、运维、支持都要查日志。攻击者拿到日志查询权限后，不一定需要打穿服务本身，只要搜索 `Authorization: Bearer`、`X-Api-Key`、`password=`、`token=` 就能收集凭证。

第三个风险是隐私和信息泄露。JWT 即使有签名，payload 也只是编码，不是加密。日志里出现 JWT，就可能暴露 `sub`、`email`、`tenant_id`、`scope`、`role`、`aud`、内部资源 id。攻击者即使不能重放，也能借这些信息做账号枚举、租户识别、权限推断和钓鱼。

第四个风险是事故处置困难。数据库 token 表泄露时，范围比较清楚；日志泄露时，要查所有日志 pipeline：应用本地日志、容器 stdout、节点日志、网关 access log、APM trace、错误上报、数据湖、备份、第三方 SaaS。token 可能已经被压缩归档，删除也不一定能彻底删除。OWASP Logging Cheat Sheet 明确把 session identification values、access tokens、passwords、database connection strings、encryption keys 等列为不应记录的数据。

预防的第一原则是 allowlist logging。只记录明确需要的字段，而不是把整段 request/response、所有 header、所有 query、异常对象直接 dump 出来。默认过滤 `Authorization`、`Cookie`、`Set-Cookie`、`X-Api-Key`、`Proxy-Authorization`、连接串、签名 URL、OAuth code、refresh token、JWT。查询参数尤其危险，因为很多网关和 access log 默认会记录 URL。

第二原则是在进入日志系统前脱敏。不要指望下游 SIEM 再处理，因为原始日志可能已经落盘。应用、网关、日志采集 agent 都要有脱敏规则。为了排障可以保留 token 前缀、token id、credential type、owner、scope，或者记录：

```text
token_fingerprint = truncate(HMAC(log_correlation_key, token), 16 bytes)
```

这样可以关联“同一个 token 发起了哪些请求”，但日志读者拿不到可重放凭证。不要用普通 hash 做指纹，特别是 token 格式可预测或熵不足时，HMAC 更稳。

第三原则是发现后按凭证泄露处理。不是删日志就完了。要立即撤销或轮换相关 token，确认是否有 refresh token，查 token 在泄露窗口内的调用记录，评估访问过的数据，清理日志平台里的副本，增加检测规则。对于无法精准定位的日志泄露，可能需要批量撤销同一时间段签发的 token，或强制用户重新登录。

结合 LogServe，如果 shared log、worker 日志、executor stdout/stderr、trace 或 result object 里出现 token，就会污染恢复和审计链路。尤其是 LogServe 这种以日志和回放为核心的系统，日志保留时间可能很长，token 一旦进入 log segment，就会被备份、复制和 replay。安全设计上要把 secret 脱敏放在写日志前，而不是指望事后扫描。

面试里可以这样回答：

```text
日志中泄露 token 等同于凭证泄露，特别是 bearer token，攻击者拿到后可以在有效期内重放请求。日志还会被集中采集、索引、备份、转发给第三方平台，访问面比生产系统更宽，扩散范围很难控制。JWT 泄露还会暴露 payload 里的用户、租户和权限信息。预防上要禁止记录 Authorization、Cookie、API key、refresh token、签名 URL 等字段，用 allowlist 日志和 HMAC 指纹做关联；发现泄露后要撤销或轮换 token，并按审计记录评估影响。
```

## Q020. 如何做 secret management？

Secret management 的目标不是“找个地方把密钥加密存起来”这么窄。它要覆盖 secret 的完整生命周期：生成、登记、存储、分发、使用、轮换、撤销、审计、备份、恢复和销毁。OWASP Secrets Management Cheat Sheet 提到的核心问题也在这里：API key、数据库凭证、IAM 权限、SSH key、证书经常被硬编码在源码、配置和配置管理工具里，最终变成难以追踪的组织风险。

第一步是定义什么算 secret。密码、API token、refresh token、数据库连接串、私钥、证书私钥、KMS data key、webhook signing secret、OAuth client secret、SSH key、worker bootstrap token、第三方 SaaS key 都算。不要只把“用户密码”叫 secret。对 LogServe 这种系统，SDK token、worker enrollment secret、object storage credential、metadata store password、LLM provider key、signing key、mTLS private key 也都算。

第二步是集中管理，但不是盲目单点。secret manager 要提供加密存储、细粒度访问控制、版本管理、审计日志、自动轮换或轮换 API、高可用、备份恢复。可以是云 Secret Manager、Vault、KMS/HSM 加密的配置系统、Kubernetes 外接 secret store。集中化的好处是知道 secret 在哪里、谁能读、谁读过、怎么轮换；但 secret manager 自己也会成为高价值目标，所以它的 root/admin 凭证、恢复密钥和 break-glass 流程要单独保护。

第三步是最小权限。不是所有工程师都能读所有 secret，也不是所有服务都能读同一个命名空间。secret manager 里的权限应该按 workload、environment、tenant、secret path、version 划分。生产和测试分开，读和写分开，应用读取和管理员轮换分开。OWASP 的建议很现实：能读取 secret 的用户或系统本身就成了泄露路径，因此要在 secret 对象和组件级别做细粒度访问控制。

第四步是运行时分发。不要把 secret bake 进镜像、二进制、前端 bundle 或 Git 仓库。容器场景里，secret 可以由 orchestrator 以文件 volume、tmpfs、sidecar、agent 或运行时 API 注入。环境变量虽然常见，但要知道它可能出现在进程 dump、调试输出、崩溃报告、`/proc`、平台 UI 或子进程环境里；高敏 secret 更适合短生命周期文件、内存注入或由 sidecar 代访问。

第五步是短期和动态。能不用长期静态 secret 就不用。云资源访问可以用 workload identity、OIDC federation、instance role、service account token exchange；数据库可以用动态账号，按需生成、短期有效、自动回收；对象存储可以用短期 signed URL 或 STS credential；服务间身份可以用短期 mTLS SVID。长期 secret 越少，轮换和事故处置越简单。

第六步是轮换设计。每个 secret 要有 owner、用途、依赖服务、创建时间、过期时间、版本、轮换周期、应急联系人。轮换要支持多版本并存：生产者先写新版本，消费者能读当前版本，应用热加载或滚动重启，确认新版本生效后撤旧。数据库密码、签名 key、mTLS CA、JWT signing key、webhook secret 的轮换方式都不同，不能用一套脚本硬套。

第七步是审计和检测。secret manager 要记录谁在什么时候读取了哪个 secret，异常读取要告警。代码仓库、CI 日志、容器镜像、工单、聊天记录、对象存储都要做 secret scanning。CI 要 mask secret，但不能把 mask 当唯一防线；命令输出、测试失败、异常堆栈仍可能泄露。发现 secret 进仓库后，不是删 commit 就行，必须视为泄露并轮换。

第八步是备份和恢复。很多团队只备份数据库，忘了 secret manager。结果灾难恢复时数据有了，解密 key、pepper、数据库密码、证书私钥没了，系统仍然起不来。反过来，备份里的 secret 也要受同等保护，不能把 Vault snapshot 明文放在普通对象存储里。恢复流程要演练，尤其是 KMS/HSM、unseal key、管理员权限丢失时怎么办。

最后，secret management 不能替代应用安全。secret 放进 Vault，不代表应用可以随便打印；KMS 加密，不代表所有服务都能 decrypt；mTLS 私钥短期，不代表 worker 能访问所有任务。它只是把 secret 生命周期管理变得可控。真正的安全还要配合最小权限、输入验证、日志脱敏、供应链安全、审计和故障演练。

结合 LogServe，我会这样设计：开发环境使用本地 `.env` 或配置文件时，明确标记只用于本地并加入 `.gitignore`；生产形态使用 secret manager 注入数据库凭证、对象存储凭证、JWT signing key、worker bootstrap secret、mTLS key；SDK token 只存 verifier；worker 通过短期注册凭证换取工作负载身份；executor 不直接拿全局 secret，而是拿本次任务的短期 capability。shared log 里只记录 secret id、key version 和脱敏指纹，不记录明文。

面试里可以这样回答：

```text
secret management 要管完整生命周期，不只是加密存储。先定义哪些是 secret，再用集中 secret manager 或 KMS/HSM 体系管理存储、访问控制、版本、审计、轮换和恢复。secret 不进 Git、镜像、前端包、日志和命令行历史；运行时通过受控机制注入，权限按服务、环境、路径和版本最小化。优先使用短期动态凭证和 workload identity，长期 secret 必须有 owner、过期、轮换 runbook 和泄露应急流程。备份恢复也要覆盖 secret，否则数据恢复了系统仍可能起不来。
```

## Q021. 环境变量保存 secret 有哪些风险？

环境变量保存 secret 的问题不在于“环境变量一定不安全”，而在于它太容易被当成没有边界的默认方案。它确实方便：容器、CI、systemd、Kubernetes、开发机都能注入环境变量，应用读取也简单。但 secret 一旦进入进程环境，就会跟着进程模型、调试工具、崩溃诊断、子进程继承和平台 UI 一起扩散。很多泄露不是攻击者打破了密码学，而是工程系统把 secret 当普通配置到处展示。

第一个风险是可见面比想象中大。在 Linux 上，同一用户或具备足够权限的进程可能从 `/proc/<pid>/environ`、core dump、调试器、进程快照、容器诊断工具里看到环境变量。平台层也可能记录它：CI job 页面、容器编排控制台、serverless 配置页、systemd unit、Docker inspect、Kubernetes Pod spec、部署审计事件，都可能成为 secret 的副本。即使平台做了 mask，mask 通常只处理 UI 展示，不等于所有下游日志和 artifact 都被清理。

第二个风险是继承。进程创建子进程时，环境变量默认会传给子进程。一个原本只该让 control 进程知道的数据库密码，可能被 executor、shell wrapper、测试脚本、模型下载器、压缩工具、日志采集 agent 继承。LogServe 这种有 worker、本地 executor、Python subprocess 的系统尤其要注意：如果 worker 的环境里带着全局对象存储密钥，用户任务代码就可能间接读到它。

第三个风险是泄露后难追踪。文件型 secret 至少可以有路径、权限、版本和读取审计；secret manager 可以记录谁在什么时候取了哪个 secret。环境变量一旦被注入，应用内部什么时候读取、是否传给子进程、是否打印到异常栈，通常没有独立审计。出了事故后，很难回答“哪些运行实例拿过这个 secret，哪些日志包含它”。

第四个风险是生命周期不清楚。环境变量通常在进程启动时固定下来，轮换时要重启或热更新整个进程。短期 token 放在环境变量里也会变成长生命周期凭证，因为重启策略、进程驻留时间和 token 过期时间经常不一致。更麻烦的是多版本并存：旧实例还拿着旧 key，新实例拿着新 key，验证端如果没有 key version 和 grace window，轮换会变成线上事故。

所以我的取舍是：低敏配置可以用环境变量；本地开发可以临时用 `.env`，但要 `.gitignore`、secret scanning 和明确的环境边界；生产高敏 secret 更适合 secret manager、短生命周期 token、只读文件挂载、tmpfs、sidecar 代理或 workload identity。即使用环境变量，也要避免把它们传给用户代码和子进程，启动日志不能 dump 全量 env，崩溃报告要过滤，CI 要把 secret scope 限到单个 job。

结合 LogServe，我不会让 Python executor 继承 control/worker 的完整环境。worker 启动需要的 bootstrap secret 可以只用于换取短期身份，换完后清理；任务执行需要访问对象存储时，给本次 result object 的短期 capability，而不是把 MinIO root credential 放进环境变量。shared log 里只记录 secret id、key version、脱敏指纹，不记录 secret value。

面试里可以这样回答：

```text
环境变量的问题是扩散面大、继承隐蔽、审计弱、轮换困难。它可能出现在进程环境、调试输出、core dump、CI 页面、容器 inspect、平台配置和子进程环境里。生产高敏 secret 不应默认放环境变量；更好的做法是 secret manager、短期凭证、文件挂载或 sidecar 注入，并限制子进程继承。即使用环境变量，也要禁止启动时 dump env，过滤日志和崩溃报告，泄露后按凭证泄露轮换。
```

## Q022. SSRF、RCE、路径穿越、反序列化漏洞分别是什么？

这四类漏洞的共同点是：攻击者把输入从“数据”变成了“系统行为”。但它们打的边界不同，不能混成一个词。

SSRF 是 Server-Side Request Forgery，服务端请求伪造。攻击者不能直接访问某个内网地址、云 metadata endpoint、管理端口或回环地址，于是诱导你的服务端替他发请求。典型入口是 URL fetch、webhook、图片下载、对象导入、模型拉取、OpenAPI callback、代理接口。风险不只是“打内网 HTTP”。SSRF 可以访问 `169.254.169.254` 这类 metadata 服务，探测内网端口，绕过 IP allowlist，访问只有服务端能访问的 Redis、Elasticsearch、Kubernetes API，甚至通过 gopher/file 等 scheme 触发非 HTTP 协议。防护上不能只做字符串黑名单，要做 scheme allowlist、DNS 解析后 IP 校验、禁止跳转到内网、限制端口、阻断 link-local/private/loopback 地址、设置 egress policy，并把下载器放在低权限网络区。

RCE 是 Remote Code Execution，远程输入导致服务执行了攻击者控制的代码或命令。入口可能是 shell 拼接、模板注入、表达式语言、插件加载、JIT、反序列化 gadget、文件上传后执行、模型或 notebook 执行、CI 脚本注入。RCE 的严重性高，因为它通常直接拿到应用进程权限，再继续读 secret、横向移动、篡改数据。防护上要避免把输入拼进 shell，优先用参数化 API；模板和表达式要用受限模式；插件和用户代码要隔离；运行身份要最小权限；还要有 seccomp、AppArmor、容器/VM、只读文件系统、网络隔离和审计。

路径穿越是 Path Traversal。攻击者通过 `../`、绝对路径、符号链接、编码绕过、Windows drive/path 变体，让程序访问预期目录外的文件。典型例子是下载接口 `GET /files?name=../../etc/passwd`，或者 artifact 解压时 zip-slip 把文件写到目标目录外。路径穿越的坑在 canonicalization：你看到的字符串不等于操作系统最终访问的路径。防护要先解码、规范化、解析真实路径，再检查最终路径是否仍在允许的根目录下；还要拒绝绝对路径、NUL 字节、符号链接逃逸和 archive entry 里的危险路径。

反序列化漏洞是把不可信字节流还原成对象时，触发了非预期行为。危险语言和框架很多：Java 原生序列化、Python pickle、Ruby Marshal、PHP unserialize、某些 YAML loader、.NET BinaryFormatter。问题不是“格式是二进制”，而是反序列化过程可能创建任意类型、调用构造/析构/hook 方法、加载 class，配合 gadget chain 变成 RCE。防护上，不可信输入不要用可执行语义的序列化格式；用 JSON、protobuf、MessagePack 这类数据格式时也要做 schema 和类型校验；必须兼容历史格式时，要做类型 allowlist、签名认证、隔离解析进程和版本迁移。

结合 LogServe，这四类都能找到入口。模型下载 URL、checkpoint source、webhook result reference 可能引入 SSRF；Python executor 本来就执行用户代码，所以要把 RCE 当设计前提而不是异常；result object path、snapshot path、model cache path 要防路径穿越；模型权重、task payload、actor snapshot 如果使用 pickle 或可执行格式，就会有反序列化风险。安全回答要把入口、边界和缓解措施讲清楚。

面试里可以这样回答：

```text
SSRF 是让服务端替攻击者访问本不该访问的地址；RCE 是远程输入导致服务执行攻击者代码；路径穿越是构造路径访问或写入允许目录之外的文件；反序列化漏洞是不可信字节流在还原对象时触发任意类型构造、hook 或 gadget chain。它们都来自信任边界处理错误，但防法不同：SSRF 控制出站网络和 URL 解析，RCE 隔离执行和避免命令拼接，路径穿越检查规范化后的真实路径，反序列化避免可执行格式或做类型 allowlist。
```

## Q023. 执行用户代码时需要防哪些攻击？

执行用户代码时，第一句话应该承认现实：这不是普通输入校验问题，而是主动运行不可信程序。只要用户代码能在你的进程、容器或机器上跑，它就会尝试读文件、打网络、占资源、影响邻居、泄露 secret、拖垮调度器。防护目标不是“让恶意代码变善良”，而是把它关在最小权限、可计量、可中止、可审计的边界里。

最直接的是文件系统攻击。用户代码可能读 `/etc/passwd`、SSH key、cloud credential、Kubernetes service account token、模型缓存、其他租户的 result object；也可能写启动脚本、覆盖依赖、投毒 cache、制造 symlink/hardlink 逃逸。防护要用独立工作目录、只读根文件系统、按任务生成临时目录、禁止挂载宿主敏感路径、限制 volume、清理 symlink，输出文件要做路径规范化和大小限制。

第二类是网络攻击。用户代码可以扫描内网、访问 metadata endpoint、调用控制面 API、打 Redis/PostgreSQL、请求外部 C2 地址，或者借你的出口 IP 做攻击。防护要默认 deny egress，只允许任务声明的目标；拦截 link-local、loopback、private CIDR；对 DNS 和重定向做二次校验；内部控制面 API 不能只靠“在内网”信任，要有 mTLS、token audience、租户和任务级授权。

第三类是资源耗尽。fork bomb、死循环、大内存分配、GPU 独占、磁盘写满、stdout 无限输出、创建过多线程、打开过多文件描述符，都会让 worker 或同机任务受影响。防护要有 CPU quota、memory limit、pids limit、ulimit、磁盘 quota、日志速率限制、执行超时、GPU/MIG 配额、OOM 和 timeout 的清晰状态。只设置 HTTP timeout 不够，执行器要能从外部杀掉任务，并回收进程树。

第四类是权限和身份滥用。用户代码如果继承了 worker 的环境变量、Unix socket、service account、对象存储凭证，就能用平台身份做事。更隐蔽的是共享 cache：一个任务把恶意 package、Triton kernel cache、模型文件、Python wheel 写进共享目录，后续任务加载后执行。防护要把平台 secret 和任务 secret 分开，executor 不继承全量 env，共享 cache 要按信任级别隔离，依赖安装要固定源和 hash。

第五类是逃逸和内核攻击。容器不是强安全边界，特别是 privileged、hostPath、hostNetwork、Docker socket、CAP_SYS_ADMIN、未限制 syscall、老内核漏洞、危险 device mount 都可能让攻击者出容器。强隔离场景更适合 microVM、Kata、gVisor、Firecracker、Wasm sandbox 或专用节点池。容器仍有价值，但要配合 rootless、drop capabilities、seccomp、AppArmor/SELinux、read-only rootfs、no-new-privileges。

第六类是数据外带和侧信道。用户代码可以把 prompt、PII、token、模型输出通过 DNS、HTTP、错误日志、时间通道带出去，也可能通过共享 GPU/CPU cache 推断邻居状态。一般项目不一定能防所有硬件侧信道，但至少要隔离租户、限制网络、避免共用敏感 cache、把日志和输出当数据出口审计。

结合 LogServe，我会把 Python executor 当成不可信执行面。control 和 shared log 不直接运行用户代码；worker 只拿本次任务需要的短期 capability；executor 在独立进程或容器里运行，有 timeout、资源限制、工作目录和日志限制；stdout/stderr 入日志前脱敏；任务完成只允许写 result store 中分配的对象 key。这样即使用户代码恶意，影响也尽量停在单个任务边界内。

面试里可以这样回答：

```text
执行用户代码要防文件读取和写入逃逸、内网访问和 SSRF、资源耗尽、凭证继承、共享缓存投毒、容器逃逸、数据外带和审计绕过。设计上应把用户代码放到独立执行边界里，默认最小权限，限制 CPU、内存、磁盘、进程数、网络和运行时间；平台 secret 不继承给 executor；输出路径和日志要受控。容器可以做隔离层，但高风险场景要考虑 microVM、gVisor、Kata 或 Wasm 这类更强边界。
```

## Q024. 容器逃逸的基本风险是什么？

容器逃逸指进程突破容器隔离，影响宿主机或其他容器。它的根本原因是容器共享宿主内核。namespace、cgroup、capabilities、seccomp、LSM 能把进程隔开，但它们不是虚拟机那种完整硬件边界。只要配置给得太宽，或者内核、runtime、挂载点、设备暴露有漏洞，容器内的攻击者就可能摸到宿主。

最常见的风险来自危险配置。`--privileged` 基本等于把大量能力交出去；挂载 Docker socket 等于允许容器控制宿主 Docker；hostPath 把宿主文件系统暴露进容器；hostNetwork 让网络边界消失；CAP_SYS_ADMIN 权限过大；挂载 `/proc`、`/sys`、设备目录、Kubernetes service account token，都可能把容器从应用隔离变成宿主控制面入口。很多逃逸不是高深漏洞，而是为了方便调试把边界打开了。

第二类是内核和 runtime 漏洞。容器里的进程仍在调用宿主内核 syscall。如果内核存在可利用漏洞，攻击者可以从容器权限升级到宿主。containerd、runc、overlayfs、cgroups、netfilter、eBPF、GPU driver 这类组件也可能成为攻击面。机器学习任务还会用到 CUDA、NVIDIA runtime、共享驱动和大块设备访问，这些都比普通 Web 服务更难收紧。

第三类是横向影响。即使没有完全拿到宿主 root，攻击者也可能读取同节点其他容器数据、投毒共享卷、抢占 CPU/GPU、写满磁盘、访问节点上的 metadata 或 kubelet API。多租户平台里，逃逸风险不只是一台机器的问题，而是租户隔离、凭证、日志、模型缓存、对象存储访问都会被牵连。

防护要从配置做起：不使用 privileged，drop capabilities，启用 seccomp、AppArmor 或 SELinux，rootless 或 non-root 用户运行，read-only rootfs，限制 hostPath 和 device，关闭不必要的 service account token，禁止挂载容器 runtime socket，限制 egress，及时升级内核和 runtime。Kubernetes 里可以用 Pod Security Standards、admission control、OPA/Kyverno 这类策略把危险配置拦在提交阶段。

如果执行的是不可信用户代码，仅靠容器一般不够。容器适合隔离普通服务依赖和部署环境；强对抗场景更适合 microVM、专用节点池、gVisor/Kata、Wasm runtime，或者把不同租户放到不同物理/虚拟机边界。这个判断很关键：不要把“用了容器”说成“已经沙箱化”。

结合 LogServe，如果 worker executor 需要跑用户 Python 或加载用户模型，我会把它和 control/logd 分开部署。executor 容器不挂宿主源码、不挂 Docker socket、不带全局 secret，结果只写到分配的 result path；高风险任务单独节点池或 microVM；模型 cache 按租户隔离，避免一个任务投毒后影响另一个任务。容器逃逸的风险要按执行内容分级，不是统一一个 Dockerfile 就解决。

面试里可以这样回答：

```text
容器逃逸的核心风险是容器进程突破 namespace、cgroup 和权限限制，影响宿主机或其他容器。常见原因包括 privileged、hostPath、Docker socket、hostNetwork、过大的 Linux capabilities、内核或 runtime 漏洞、设备和驱动暴露。防护要 drop capabilities、启用 seccomp/LSM、非 root、只读文件系统、限制挂载和网络、升级内核/runtime，并用 admission policy 阻止危险配置。运行不可信代码时，容器通常只是基础隔离，高风险场景要考虑 microVM 或专用节点。
```

## Q025. 镜像供应链攻击如何发生？

镜像供应链攻击不是只发生在 registry 被黑的时候。它覆盖从源码、依赖、构建脚本、CI、基础镜像、打包、签名、推送、拉取到部署的整个链路。攻击者只要控制其中一个环节，就可能把恶意代码带进最终容器镜像，而且它看起来还像一次正常发布。

第一类是源头依赖被污染。Dockerfile 里 `apt install`、`pip install`、`npm install`、`go install` 如果不固定版本和来源，就会拉到被劫持、typosquatting、dependency confusion 或被维护者账号攻破的包。基础镜像也一样，`FROM python:latest`、`FROM ubuntu:latest` 会随时间变化。今天构建的内容和下个月构建的内容可能不同，排查时很难复现。

第二类是构建环境被污染。CI runner 拿着源码、registry token、签名权限、云凭证，权限很高。攻击者如果能改 GitHub Actions workflow、偷 runner token、投毒构建 cache、篡改 build script、利用 pull request 注入，就可以在镜像里放后门，或者直接推送带后门的 tag。更隐蔽的是缓存污染：构建速度优化留下的 layer cache、package cache、compiler cache，可能跨分支、跨项目、跨信任级别复用。

第三类是镜像发布和分发被替换。tag 是可变引用，`myapp:prod` 可以被重新指向；registry 账号被盗后可以覆盖镜像；镜像 digest 如果不校验，部署系统只知道 tag，不知道实际 bytes。中间人攻击在 HTTPS 和 registry auth 下少一些，但内部 registry、镜像代理、离线导入包仍可能被篡改。

第四类是构建脚本主动下载二进制。很多 Dockerfile 会 `curl | sh`，或者下载某个 release tarball、模型权重、插件、CLI。只验证 HTTPS 不够，因为 HTTPS 只保护传输，不证明发布者身份，也不证明这个文件对应你审计过的版本。至少要固定 URL、版本、SHA-256 或签名；更好是验证发布者签名、SLSA provenance 和构建者身份。

第五类是部署策略放大了问题。集群如果允许任意镜像来源、允许 latest、允许未签名镜像、允许 privileged、允许镜像里带 root 用户和包管理器，一个被污染的镜像进入环境后很容易横向移动。镜像扫描能发现已知 CVE，但很难发现业务后门、恶意初始化脚本和凭证窃取逻辑。

防护要分层：固定基础镜像 digest；锁定依赖版本和 hash；CI 最小权限，trusted runner 与 untrusted PR 分离；构建过程生成 SBOM 和 provenance；镜像签名并在 admission 阶段验证；部署使用 digest 而不是 mutable tag；registry 权限分环境；定期重建和扫描；发布流程要求双人审批或受保护分支。SLSA 这类框架的价值就在于把“谁构建了什么、从哪些输入构建、在哪个平台构建”变成可验证信息。

结合 LogServe，worker 镜像、control 镜像、Python executor 镜像和模型 adapter 镜像都要分开看。executor 镜像风险最高，因为它运行用户任务和依赖；control 镜像风险在于拿着调度和凭证权限。我的设计会固定基础镜像 digest，CI 产出镜像 digest、SBOM、构建 provenance，部署时只允许来自内部 registry 且签名通过的 digest。模型权重不应该在 Docker build 中随手 curl 下来，应该走独立的 artifact 校验和访问控制。

面试里可以这样回答：

```text
镜像供应链攻击可以发生在依赖、基础镜像、构建脚本、CI runner、缓存、registry、tag 和部署策略任何一环。攻击者可能投毒依赖、篡改 workflow、偷推送凭证、覆盖 tag，或者让 Dockerfile 下载恶意二进制。防护要固定基础镜像 digest 和依赖 hash，隔离不可信 PR 构建，生成 SBOM/provenance，镜像签名，部署时验证签名并使用 digest，不把 latest 或未签名镜像直接进生产。
```

## Q026. SBOM 解决什么问题？

SBOM 是 Software Bill of Materials，软件物料清单。它解决的不是“自动让软件安全”，而是“我到底用了什么”这个基础可见性问题。没有 SBOM，出了一个 OpenSSL、log4j、xz、glibc、protobuf、base image 漏洞时，团队只能靠 grep、包管理器列表、镜像扫描和人工记忆去猜哪些系统受影响。SBOM 把组件、版本、供应商、依赖关系、许可证、hash、来源、构建信息以机器可读格式记录下来，让影响分析可自动化。

第一层价值是漏洞响应。安全公告出来后，扫描系统可以拿 CVE/CPE/purl/Git commit/hash 去匹配 SBOM，快速回答“哪些镜像、二进制、服务、客户环境包含这个组件”。没有 SBOM，响应会慢很多，尤其是静态链接、vendored dependency、容器多层镜像、模型 serving 镜像、CLI 工具打包进镜像这类场景。

第二层价值是依赖治理。SBOM 能暴露直接依赖和传递依赖，也能发现重复版本、废弃包、未知来源包、许可证冲突、未维护组件。它让供应链从“构建完才扫描”前移到“构建时记录证据”。但要注意，SBOM 的准确性取决于生成时机和工具能力。源码级 SBOM、构建级 SBOM、镜像级 SBOM看到的东西不同；动态下载的插件和运行时加载的模型如果没被记录，SBOM 就不完整。

第三层价值是采购和合规。客户、审计、监管可能要求供应商说明组件组成、许可证、漏洞状态、加密资产、第三方服务。SPDX 和 CycloneDX 是常见格式，前者在许可证和软件清单领域很常见，后者在安全、服务、漏洞、SaaSBOM、ML-BOM 等扩展上更活跃。格式不是重点，重点是能被工具消费、能和 artifact 绑定、能随版本归档。

SBOM 解决不了几个问题。它不能证明构建过程没被篡改，这需要 provenance、签名、可重复构建或受保护构建平台。它也不能证明组件没有未知漏洞，只能帮助发现已知风险。它不能替代依赖升级策略，不能替代运行时隔离，也不能替代供应商信任评估。一个 SBOM 如果没有签名、没有和镜像 digest 绑定、没有随版本保存，事故时价值会打折。

结合 LogServe，我会为 control、worker、logd、Python SDK、executor 镜像分别生成 SBOM。Go module、Python package、基础镜像、系统包、模型 adapter 依赖都要覆盖。每次 release 把 SBOM 绑定到镜像 digest 和 Git commit，和 provenance、签名一起保存。以后某个依赖曝漏洞，才能快速判断是 control 面受影响、executor 受影响，还是只在开发工具链里出现。

面试里可以这样回答：

```text
SBOM 解决的是软件组成可见性问题。它记录一个 artifact 包含哪些组件、版本、来源、依赖关系、hash、许可证等信息，方便漏洞响应、依赖治理、合规和客户审计。SBOM 不等于安全保证，它不能证明构建没被篡改，也不能发现未知漏洞；它需要和镜像 digest、签名、provenance、扫描和升级流程配合。没有 SBOM，供应链事故时很难快速回答哪些服务受影响。
```

## Q027. 依赖锁文件和可重复构建有什么安全价值？

依赖锁文件的价值是把“这次构建解析到了哪些依赖”固定下来。没有 lockfile，`npm install`、`pip install`、`cargo build`、`go mod tidy` 可能因为新版本发布、解析策略变化、镜像源差异、yanked package、平台标签不同而得到不同依赖。安全上，这会让审计和回滚都变弱：你以为构建的是同一份源码，实际依赖已经变了。

锁文件能降低几类风险。第一是意外升级风险。一个传递依赖发布了带漏洞的新版本，没锁定时会被自动拉入。第二是 dependency confusion 和 typosquatting 的影响面。如果 lockfile 记录了精确版本、来源和 integrity hash，攻击者即使发布同名高版本包，也不容易被解析进来。第三是审计证据。代码评审能看到依赖变化，不会把风险藏在构建日志里。

但锁文件不是银弹。它锁的是包版本和校验，不一定锁构建脚本行为；很多包安装时会跑 postinstall、setup.py、build.rs；有些依赖会下载平台特定二进制；有些 registry metadata 会变化。锁文件也不能证明包本身可信，只能证明你拿到的是锁定的那一份。真正严肃的环境会配合私有镜像源、hash 校验、离线缓存、禁用危险 install script 或把构建放进受控沙箱。

可重复构建进一步解决“同一源码和同一依赖，能不能得到同一 artifact”的问题。如果构建结果可重复，别人可以独立构建并比对 hash，发现发布包是否被构建环境或发布流程篡改。它还让事故排查更清楚：源码、依赖、编译器、构建参数、时间戳、路径、locale、随机数、压缩顺序都会影响输出；可重复构建要求把这些非确定因素收紧。

安全价值体现在两个方向。对生产者，它减少“构建机被投毒但源码没变”的隐蔽空间。对消费者，它提供验证路径：我不只相信发布者上传的二进制，还可以用公开源码和声明的构建环境复现。可重复构建和 SLSA provenance 是互补关系：provenance 说明 artifact 怎么构建，可重复构建让别人能检查这个说明是否站得住。

结合 LogServe，Go 依赖有 `go.sum`，Python 依赖如果进入 executor 镜像，应该有锁定版本和 hash。镜像构建要固定基础镜像 digest，不用 `latest`。release 产物要记录 Git commit、构建参数、Go/Python 版本、镜像 digest、SBOM 和 provenance。这样未来如果 worker binary 或 SDK wheel 被怀疑篡改，可以回到相同输入重建和比对。

面试里可以这样回答：

```text
锁文件固定依赖解析结果，减少意外升级、依赖混淆和不可审计变更。可重复构建则要求同一源码、依赖和构建环境产出相同 artifact，方便独立验证发布物是否被构建或发布链路篡改。它们不能证明依赖没有漏洞，也不能替代签名和 provenance，但能把供应链从“相信构建机器”推进到“可以复查输入和输出”。
```

## Q028. 如何验证下载的二进制或模型权重未被篡改？

验证下载物不能只说“用 HTTPS”。HTTPS 保护传输通道，不能证明发布者身份是否符合预期，也不能证明下载的对象就是项目发布的那个对象。一个二进制、容器镜像、模型权重或 adapter 包，至少要验证完整性、来源、版本、用途和加载安全。

最基础是校验 hash，但 hash 必须来自可信渠道。项目 release 页面给的 SHA-256 如果和文件在同一个可被攻击者同时修改的页面上，价值有限；如果 hash 在签名 release、包管理器 metadata、透明日志、独立公告渠道或你自己的锁文件里，价值就高。校验时要固定算法，避免 MD5/SHA-1 这类已不适合安全场景的 hash；比较时用工具输出的完整 digest，不要只看前几位。

更强的是签名。二进制可以用 GPG、minisign、cosign、Sigstore 这类方式验证发布者签名；容器镜像可以验证 digest 和签名；模型权重也可以作为 blob 签名。关键不是“有签名文件”，而是信任锚是否正确：公钥从哪里来、是否绑定发布者身份、是否轮换、是否撤销、是否有透明日志或证书链。很多团队下载 `.sig` 后用同目录的公钥验证，这只能证明攻击者同时替换了三件东西。

再往上是 provenance 和 attestation。它回答“这个 artifact 是由哪个源码、哪个 commit、哪个构建平台、哪些参数构建出来的”。对于开源依赖，能验证 SLSA provenance、GitHub OIDC identity、构建 workflow、subject digest，比只看文件 hash 更稳。模型权重也可以记录训练代码、数据版本、转换脚本、量化参数、基础模型、许可证和导出环境。不是所有模型都能公开完整训练数据，但至少要有可审计的来源链。

模型权重还有额外问题。`.pt`、`.pth`、pickle、TorchScript、custom operator 可能在加载时执行代码或触发 native extension；即使权重文件未被篡改，也可能本来就不该被信任。更安全的格式如 safetensors 限制了可执行语义，但仍要验证大小、shape、dtype、metadata、模型架构匹配和资源消耗。下载前还要防 SSRF、路径穿越、zip-slip 和解压炸弹。

实际流程可以这样做：固定下载 URL 和版本；使用 allowlist 域名；下载到临时目录；限制大小和 Content-Type；校验 digest；验证签名和 signer identity；检查 provenance/attestation；解压或加载前做路径和格式检查；在隔离环境里第一次加载；把最终 artifact 复制进内部 artifact store，并用内部 digest 引用。生产环境不要每次启动都从公网动态拉最新文件。

结合 LogServe，模型 checkpoint 和 worker binary 都应该进入 result/object store 或内部 registry 后再被调度使用。shared log 记录的是 artifact digest、签名状态、key id、来源和版本，不记录“某个 URL”。worker 只加载已经通过验证的 digest；如果要支持用户上传模型，先进入隔离扫描和转换流程，再进入可执行环境。

面试里可以这样回答：

```text
验证下载物要分层：先固定版本和来源，用 SHA-256 这类强 hash 校验完整性；再验证发布者签名，确认公钥或签名身份来自可信渠道；更进一步验证 provenance，确认 artifact 来自预期源码、commit 和构建平台。模型权重还要看格式是否会执行代码，优先用安全格式，加载前检查大小、shape、metadata，并在隔离环境中试加载。HTTPS 只是传输保护，不能单独证明 artifact 没被篡改。
```

## Q029. 模型文件也可能成为供应链风险吗？

会，而且模型文件的风险经常被低估。很多人把模型权重看成“大矩阵”，以为它和图片、CSV 一样只是数据。实际工程里，模型 artifact 可能包含可执行序列化、custom code、tokenizer、config、pre/post-process 脚本、native extension、量化 kernel、Triton cache、LoRA adapter、prompt template。加载模型往往比读取普通配置复杂得多。

第一类风险是加载时执行代码。Python pickle、某些 PyTorch checkpoint、TorchScript、custom operator、model hub 的 remote code，都可能在反序列化或初始化时执行逻辑。攻击者不一定要篡改推理输出，只要让加载器跑一段代码，就能读环境变量、访问网络、写文件或挖凭证。PyTorch 官方安全说明也把不可信模型当成不可信代码看待，这个边界非常重要。

第二类风险是依赖和 companion files。一个模型仓库不只有权重，还可能有 `requirements.txt`、自定义 tokenizer、`modeling_*.py`、配置文件、vocab、merges、generation config、后处理脚本。依赖安装和 remote code 执行本身就是供应链入口。很多模型服务为了兼容生态，会允许 `trust_remote_code=true` 之类选项；这在生产环境要非常谨慎，至少要进入代码审计和镜像构建流程，而不是在线动态执行。

第三类风险是资源攻击。模型文件可以声明巨大 tensor、异常 shape、压缩炸弹、恶意 metadata、触发 OOM 的结构，导致加载时内存爆炸、GPU 显存耗尽、服务启动卡死。即使格式不执行代码，也可能造成拒绝服务。对多租户 serving 来说，一个恶意模型把 worker 拉死，就会影响其他租户。

第四类风险是数据和行为风险。模型可能记忆训练数据，包含 PII、商业秘密或版权内容；也可能被后门训练，在特定 trigger 下输出攻击者想要的结果。这个风险和文件篡改不同：artifact 本身可能就是“真实发布”的，但不符合你的安全要求。供应链验证只能证明来源，不能证明模型行为安全。

防护要把模型纳入 artifact 管理。来源 allowlist、digest、签名、SBOM/ML-BOM、许可证、基础模型、训练/微调来源、转换脚本、评测记录都要记录。格式上优先选择不执行代码的权重格式；禁用或审批 remote code；依赖安装离线化；首次加载在沙箱里做大小、shape、dtype、metadata、内存预算和推理 smoke test；通过后复制到内部模型 registry，用 digest 而不是外部 URL 引用。

结合 LogServe，model registry 不能只保存 `model_name` 和 `path`。至少要保存 artifact digest、格式、来源、签名状态、大小、adapter 类型、是否需要 remote code、资源预算、验证状态。worker cache 里按 digest 命名，避免同名模型覆盖；shared log 记录加载的是哪个 digest。用户上传模型要走隔离扫描，不能让 worker 直接从公网 hub 拉取并执行。

面试里可以这样回答：

```text
模型文件当然可能是供应链风险。很多模型 artifact 不只是权重，还可能包含 pickle、TorchScript、custom code、tokenizer、依赖和后处理脚本，加载时可能执行代码；即使不执行代码，也可能通过异常 shape、巨大 tensor 或压缩结构造成资源耗尽。防护上要把模型纳入 artifact registry，验证 digest、签名、来源和许可证，优先使用不执行代码的格式，禁用或审批 remote code，首次加载放在沙箱里，并按 digest 缓存和调度。
```

## Q030. 如何处理 PII 的加密、脱敏、访问审计？

PII 处理要先做数据分类。名字、邮箱、手机号、地址、身份证件、设备标识、IP、cookie、精确位置、面部图像、支付标识、健康信息、用户 prompt 和模型输出里的个人信息，都可能是 PII 或敏感个人信息。不要等到写代码时再判断；字段进入系统前就要有 classification、purpose、retention、owner 和访问路径。

加密解决的是机密性。传输中用 TLS，存储中用 envelope encryption：数据用 data key 加密，data key 由 KMS/HSM 或主密钥保护。高敏字段可以做字段级加密或 tokenization；对象存储和数据库的磁盘加密只是底线，因为应用层和数据库管理员仍可能看到明文。密钥要有版本、轮换、访问控制和审计。加密设计还要考虑查询需求：如果业务需要按邮箱查找，不能直接把邮箱随机加密后还指望等值查询；可以用单独的 HMAC blind index，但要理解频率分析和低熵字段枚举风险。

脱敏解决的是最小暴露。展示给客服、日志、BI、测试环境、工单、错误报告的数据，不应该是完整明文。常见方法有 mask、截断、泛化、hash/HMAC 指纹、tokenization、匿名化和假名化。这里要区分：mask 主要防肉眼泄露；HMAC 指纹适合关联同一主体但不暴露原文；匿名化如果做得不好仍可重识别；假名化仍然通常算个人数据，因为有办法还原或关联。

访问审计解决的是责任和检测。每次读取高敏 PII，要记录主体、服务、租户、字段类别、目的、时间、结果、请求来源和审批上下文。审计日志不要记录 PII 明文；记录对象 id、字段类别、脱敏指纹就够。审计要能回答“谁看过这个用户的数据”“某个服务为什么批量读取”“某个 break-glass 操作是否合规”。只记录写操作不够，读操作才是 PII 泄露的高频路径。

权限模型要按用途收紧。工程师默认不该有生产 PII 明文读权限；服务账号只读业务需要的字段；后台查询要审批和水印；导出要限量、过期、加密和可追踪；测试环境使用脱敏数据。高敏字段最好做 purpose-based access control：同一个用户对象，风控服务可以看某些字段，推荐服务不一定能看，排障人员需要临时授权。

结合 LogServe，PII 可能出现在 task input、workflow payload、LLM prompt、LLM result、actor state、snapshot、result object、stdout/stderr 和 trace 里。shared log 是 append-only 的，一旦写入 PII，后续删除和脱敏会很难。所以要在 SDK 或 control 接入层就做字段分类和脱敏策略：日志里只保存引用、分类标签、加密 blob 或脱敏摘要；result store 做对象级加密和租户授权；审计记录谁读取了 prompt/result/snapshot。

面试里可以这样回答：

```text
PII 处理先做数据分类和目的限制。传输用 TLS，存储用 KMS/HSM 支持的 envelope encryption，高敏字段可以字段级加密或 tokenization；日志、BI、客服和测试环境用脱敏或 HMAC 指纹，避免明文扩散。访问审计要覆盖读操作，记录谁、什么时候、以什么目的访问了哪类数据，但审计日志本身不存 PII 明文。权限要按租户、服务、字段和用途最小化，导出和 break-glass 需要审批、过期和追踪。
```

## Q031. GDPR 删除请求和 append-only log 会产生什么矛盾？

append-only log 的设计目标是不可变、可回放、可审计。GDPR 删除请求关注的是数据主体在特定条件下要求删除个人数据的权利。矛盾就在这里：日志系统希望历史事件不要改，隐私合规要求某些个人数据在目的消失、同意撤回或违法处理时被删除。你不能简单说“我们是 append-only，所以删不了”，也不能随手改历史日志让恢复语义崩掉。

第一种冲突是物理删除和回放完整性。workflow、actor、LLM 请求如果把 PII 直接写进 log record，后续 replay 依赖这些字段恢复状态。删除字段后，旧事件可能无法重放；不删除又可能违反删除请求。尤其是 LogServe 这种以 shared log 为状态源的系统，日志既是审计材料，又是恢复材料，PII 进入日志就很难处理。

第二种冲突是备份、索引和派生数据。即使主 log 做了删除，PII 可能还在 compacted segment、snapshot、result object、cache、搜索索引、metrics label、trace、dashboard snapshot、备份和冷归档里。GDPR 处理不是删一行数据库，而是处理所有复制和派生位置。删除请求还要求身份核验、范围判断和例外判断，比如法律义务、公共利益、法律主张等场景可能允许保留部分记录。

第三种冲突是审计和不可抵赖。安全审计日志有时必须保留，防止攻击者通过删除请求抹掉入侵痕迹。正确做法通常不是在审计日志里保留明文 PII，而是在设计时做数据最小化：审计日志保存事件、主体 pseudonymous id、tenant、action、time、policy decision、脱敏指纹，而不是邮箱、手机号、prompt 原文。这样既能审计，又降低删除压力。

工程上有几种缓解。最重要的是不要把可删除 PII 直接放进不可变日志。日志里保存稳定内部 id、对象引用、加密 blob id、key version、hash/HMAC 指纹；PII 明文放在可删除的数据存储里。删除请求到来时，删除或匿名化明文对象，撤销加密 key，更新 materialized view，让 replay 得到“数据已删除”的状态。对历史 log，可以保留事件骨架，但去掉可识别内容。

如果历史上已经写入 PII，就需要补救流程：定位所有 log segment 和派生物，评估是否能做 redaction segment、tombstone、crypto-shredding、物理重写和重新签名；确认备份保留周期；记录删除处理证据；对无法立即改写的归档制定访问冻结和生命周期。这里要和法务/合规一起定义，因为 GDPR Article 17 有条件和例外，不是任何请求都无条件物理擦除所有痕迹。

结合 LogServe，我会把 shared log 设计成不承载 PII 明文。task payload 大对象放 result store，log 只存 object ref、content hash、classification、tenant 和 key version；LLM prompt/result 如果敏感，进入加密对象；snapshot 也走对象存储和字段级脱敏。删除时删除对象或销毁 data key，再追加 `PersonalDataErased` 事件让 materialized view 收敛。这样 append-only 语义和删除请求不会直接互相撞车。

面试里可以这样回答：

```text
GDPR 删除请求和 append-only log 的矛盾在于：日志希望历史不可变、可回放，删除请求要求在满足条件时删除个人数据。解决思路不是随意改历史，也不是拒绝删除，而是在设计时避免把 PII 明文写进不可变日志。日志保存事件骨架、对象引用、脱敏指纹和 key version；PII 明文放在可删除或可 crypto-shredding 的存储里。删除时删除对象、销毁密钥或追加删除事件，并处理索引、snapshot、缓存和备份的生命周期。
```

## Q032. 安全审计日志本身如何防篡改？

审计日志的威胁模型要先说清楚。它要防普通应用 bug、恶意用户、被攻陷的业务服务、内部人员，还是要防拿到日志管理员权限的人？不同强度的防篡改方案差别很大。最弱的是“应用自己写数据库表”；最强会走 WORM 存储、外部时间戳、签名、透明日志和独立安全域。面试里不要把“有日志”说成“可审计”。

第一步是追加写和权限隔离。业务服务只应该有 append 权限，没有 update/delete 权限。日志写入服务和业务数据库分离，日志存储账号分离，生产工程师默认不能改历史日志。日志采集链路要尽快把事件送到独立安全域，避免攻击者拿下应用后先删本地日志。对于本地缓冲，也要限制权限和保留短窗口。

第二步是完整性链。每条日志记录可以包含上一条记录的 hash，形成 hash chain；按时间窗口生成 Merkle root 或 segment digest；每个 segment 关闭时签名或 HMAC。这样篡改单条记录、删除中间记录、重排记录都会被发现。HMAC 适合单组织内部验证，数字签名适合让审计方在不拿写入密钥的情况下验证。密钥要放 KMS/HSM，不能和应用放一起。

第三步是时间和序列。审计日志要有单调序列号、可信时间来源、写入时间和事件发生时间的区分。攻击者常见手法是延迟写入、回填旧时间、删除一段窗口。单调 append offset、segment id、时间戳签名、外部 timestamp service 可以帮助发现缺口。分布式系统里还要记录节点 id、trace id、request id，避免多源日志合并后顺序含糊。

第四步是不可变存储和保留策略。对象存储的 object lock/WORM、合规保留、版本化、跨账号复制、冷归档可以提高删除和覆盖成本。数据库也可以用 append-only 表加审计触发器，但对高价值审计日志，最好复制到业务系统之外。保留策略要同时满足安全调查和隐私最小化，不能无限期保存所有明文。

第五步是验证和告警。防篡改机制如果从不验证，就只是摆设。要定期重算 hash chain、验证签名、检查 segment 连续性、检查写入速率异常、检查日志源心跳。日志突然停止、某个服务日志量归零、sequence 缺口、签名失败，都要告警。还要演练：假设某段日志被删，系统能否发现并定位范围。

结合 LogServe，shared log 自身就有 append-only 结构，但这不等于安全审计日志。为了防篡改，可以给每个 stream 的 record 加 sequence 和 previous digest，segment close 时生成 segment manifest，manifest 用 KMS key 签名，再复制到只追加对象存储。control、worker、executor 的安全事件单独进入 audit stream，业务服务只有 append capability。replay 时验证 digest 链，dashboard 展示 audit gap。

面试里可以这样回答：

```text
审计日志防篡改要做权限隔离、追加写、完整性链、签名和独立存储。业务服务只允许 append，不能 update/delete；每条日志带 sequence 和 previous hash，segment 关闭后生成 Merkle root 或 digest 并签名；日志尽快复制到独立安全域或 WORM/object-lock 存储。还要定期验证 hash chain、签名和连续性，对日志中断、缺口、签名失败告警。日志本身也要最小化，不能为了审计长期保存 PII 明文。
```

## Q033. 如何设计 rate limit 防止资源滥用？

Rate limit 的目标不是简单地“每秒最多 N 次”。它要保护共享资源，避免单个用户、租户、IP、token、任务类型或下游服务把系统拖垮。一个好设计要先定义被保护的资源：请求数、CPU、内存、GPU、队列长度、数据库连接、对象存储 I/O、LLM token、模型加载次数、日志写入量、失败重试次数，这些都可能需要不同的限流维度。

常见算法有 token bucket、leaky bucket、fixed window、sliding window、并发上限和配额。token bucket 允许短突发，适合 API；leaky bucket 平滑输出，适合保护下游；sliding window 比 fixed window 更不容易在窗口边界被打爆；并发上限适合长任务；每日/月度配额适合成本控制。实际系统通常会组合：每秒速率、同时运行任务数、排队任务数、每日 token budget、单请求最大 payload。

限流 key 要谨慎。只按 IP 容易误伤 NAT 后的用户，也防不住分布式攻击；只按 user 又挡不住匿名入口；只按 token 又挡不住一个租户创建大量 token。更稳的是分层：global、region、tenant、user、API key、IP、endpoint、resource class。内部服务也要限流，因为 bug 或重试风暴比外部攻击更常见。

返回行为也重要。HTTP 可以返回 429 和 `Retry-After`；异步任务系统可以拒绝入队、延迟调度、降级、排队或 load shed。不要让限流失败变成无限重试。客户端 SDK 要有指数退避和 jitter；服务端要区分“超过速率”“超过并发”“超过成本预算”“队列满”，否则调用方不知道怎么恢复。

还要防绕过。攻击者会换 IP、换 token、制造高成本低频请求、构造大 payload、触发昂贵查询、让任务失败后自动重试、用不同 endpoint 打同一个下游。限流不能只数请求，还要按成本计量。例如 LLM 请求要看 input/output token、模型大小、是否 cold load、GPU 秒；对象存储要看 bytes；日志写入要看 record count 和 payload bytes。

状态存储要和系统规模匹配。单机内存限流简单但多实例下不准确；Redis/central quota 准确一些但会引入延迟和单点；本地近似限流加全局配额适合高吞吐路径。高并发下要避免每个请求都抢同一把锁；可以分片计数、批量同步、使用原子操作或令牌预取。限流系统本身也要降级，否则 Redis 抖动时全站不可用。

结合 LogServe，我会按 tenant/user 限制 task submit QPS、排队长度、运行中 task 数、workflow step 数、actor mailbox 长度、LLM token budget、模型加载频率、result object bytes 和 log append bytes。worker 拉取任务也要限速，避免 control 重启后所有 worker 同时抢任务。失败重试要有 retry budget，不能让一个坏任务无限占满队列。

面试里可以这样回答：

```text
rate limit 要先保护具体资源，不只是限制请求数。可以组合 token bucket、sliding window、并发上限和成本配额，按 global、tenant、user、token、IP、endpoint、resource class 分层。对高成本请求要按 CPU/GPU 秒、LLM token、bytes、队列长度和重试次数计费。超过限制时返回 429/Retry-After 或异步拒绝入队，并要求客户端退避加 jitter。多实例下要权衡本地近似和集中配额，限流系统本身也要能降级。
```

## Q034. 如何防止多租户之间的数据泄露？

多租户隔离先要明确隔离对象：身份、元数据、业务数据、缓存、日志、指标、对象存储、任务队列、模型缓存、GPU/CPU 资源、备份、管理员工具。很多泄露不是数据库少了一个 `tenant_id` 这么简单，而是某个旁路系统忘了租户边界，比如搜索索引、导出任务、trace、dashboard、error report、cache key。

第一层是身份和授权。每个请求必须有租户上下文，tenant 不能只来自客户端传参；服务端要从 token、mTLS identity、session 或 API key 解析出主体和租户，再做 policy decision。跨租户访问必须是显式授权，比如组织管理员、支持人员 break-glass、共享项目；不能靠“传了另一个 tenant_id”就访问。

第二层是数据模型。共享表模式下，所有表、索引、查询、更新、删除、后台任务都要带 tenant predicate，最好有数据库 Row Level Security 或统一 query builder 执行。独立 schema 或独立数据库隔离更强，但成本和运维复杂度更高。对象存储 key 也要包含租户前缀，并在服务端授权，不能把 bucket URL 当权限。

第三层是缓存和队列。缓存 key 必须包含 tenant id 和权限相关维度；不要用裸 `user_id`、`workflow_id`、`model_name` 当全局 key。队列消息要带 tenant，并在 worker 取到任务后再次校验 capability。模型缓存要按租户或信任级别隔离，特别是用户上传模型、LoRA adapter、prompt cache、KV cache 这类可能含数据的缓存。

第四层是日志、指标和可观测性。日志里不能把其他租户的 payload、prompt、result、token 打出来；trace baggage 和 metric label 不要塞 PII。Dashboard 查询必须带租户过滤。导出和报表是高风险点：批量导出要有租户边界、审批、限量和水印。测试环境也要防止把生产全量数据复制过去后给所有开发者看。

第五层是资源隔离和侧信道。共享 worker、GPU、磁盘和网络会带来资源干扰，也可能通过缓存、临时文件、模型 cache、日志文件泄露。强隔离租户可以用独立节点池、独立 namespace、独立 KMS key、独立 object prefix/bucket、独立数据库 schema。普通隔离也至少要有 quota、防止 noisy neighbor、清理临时目录、限制同机共享敏感 cache。

第六层是测试。多租户系统一定要有越权测试：IDOR、tenant id 参数篡改、缓存串租户、后台任务漏过滤、导出漏过滤、管理 API 漏过滤、对象存储直连绕过。只靠代码评审很难覆盖。可以用两个测试租户构造相同 id、不同行为，跑集成测试和安全测试。

结合 LogServe，tenant id 要进入 task、workflow、actor、LLM request、result object、audit log 和 materialized view。shared log stream 命名、object key、cache key 都要包含 tenant 或用内部权限映射；worker 拿到任务时只拿该任务的短期 capability；dashboard API 默认只查当前租户。actor id 和 workflow id 即使全局唯一，也不能替代授权检查。

面试里可以这样回答：

```text
防多租户泄露要把租户上下文贯穿身份、授权、数据、缓存、队列、日志、对象存储和后台任务。请求中的 tenant 不能由客户端随便传，要从认证主体解析并做服务端授权；数据库查询、对象 key、缓存 key、队列消息都要带租户边界；日志、trace、导出和 dashboard 也要过滤。高隔离租户可以用独立 schema、KMS key、bucket、namespace 或节点池。最后要用两个以上测试租户专门测 IDOR、缓存串租户和后台任务漏过滤。
```

## Q035. 如何为内部控制面 API 做认证和授权？

内部控制面 API 不能只靠“在内网”保护。内网里有被攻陷的业务服务、调试机器、CI runner、员工 VPN、横向移动的攻击者。控制面 API 又通常有高权限：注册 worker、分配任务、读写日志、发放 token、更新模型、触发重试、查看租户数据。它需要和外部 API 一样严肃的认证和授权，有时还要更严。

认证先解决“调用者是谁”。服务间调用可以用 mTLS，把客户端证书中的 SPIFFE ID、service account、workload identity 映射成服务身份；也可以用短期 OAuth2 client credential token、JWT access token 或云 workload identity。人访问控制面要走 SSO/MFA，不要共享 admin token。worker bootstrap 要分两步：初始 enrollment secret 只用于注册，注册后换取短期工作负载身份。

认证材料必须有 audience、issuer、过期时间和绑定关系。一个给 metrics API 的 token 不能拿来调用 scheduler；一个 staging worker 不能注册到 prod control；一个 tenant-scoped token 不能访问全局管理 API。mTLS 也不是只看证书没过期，要校验证书链、SAN/SPIFFE ID、trust domain、revocation/rotation 和服务名绑定。

授权要按动作和资源做，而不是“认证通过就是内部服务”。控制面 API 可以拆成 `TaskSubmit`、`TaskPoll`、`TaskComplete`、`WorkerRegister`、`ReadLog`、`AppendLog`、`ReadResult`、`UpdateModel`、`AdminReplay` 等 action，每个 action 绑定 resource、tenant、environment、scope、caller identity。worker 只能 poll 分配给自己的队列、complete 自己 lease 的任务；SDK client 只能 submit/read 自己租户的任务；dashboard 只能读当前租户 view；管理员高危操作要审批或 break-glass。

还要有防重放和会话约束。控制面请求可以带 request id、timestamp、nonce 或使用 TLS channel 保护；任务完成事件要带 lease id、epoch、attempt，避免旧 worker 或旧 token 写回结果。LogServe 现有的 actor epoch fencing 思路可以推广到 worker lease 和 task completion：不是谁拿到 task id 都能写完成，而是必须持有当前 lease/capability。

审计和限流也属于控制面安全。每个控制面决策要记录 caller、action、resource、tenant、policy result、reason、request id。失败的授权也要记录，用于发现扫描和横向移动。内部 API 要做 rate limit，防止 bug 服务打爆控制面。错误信息不要泄露其他租户资源是否存在。

结合 LogServe，我会让 control API 使用 mTLS 或短期 JWT。worker 注册时拿到 workload identity；poll 返回任务和本次 result object capability；complete 时必须带 task lease、attempt 和 worker epoch；ReadLog/ReadResult 按 tenant 和 role 授权；管理 API 单独暴露在受控网络和 SSO 后面。shared log append 也要按 stream/action 授权，不允许任何内部服务随便写任意 stream。

面试里可以这样回答：

```text
内部控制面 API 不能只信内网。认证上用 mTLS、workload identity 或短期 OAuth/JWT，人访问走 SSO/MFA；token 和证书要校验 issuer、audience、过期、环境和服务身份。授权按 action/resource/tenant/scope 做，worker 只能操作自己 lease 的任务，SDK 只能访问自己租户资源，管理员高危操作要审批。请求还要有防重放、epoch/lease fencing、审计日志和内部限流。
```
## Q036. AEAD 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

AEAD 是 Authenticated Encryption with Associated Data，核心目标是同时提供机密性、完整性和数据来源认证，并允许一部分关联数据只认证、不加密。它主要解决安全性问题，同时也改善正确性和可维护性。性能不是它的首要目标，但很多 AEAD 算法像 AES-GCM、ChaCha20-Poly1305 在工程上也能做到很高吞吐。

传统做法常常把 encryption 和 MAC 分开组合，比如先 AES-CBC 加密，再 HMAC。问题在于组合顺序、密钥分离、padding 错误处理、MAC 覆盖范围、错误返回差异都容易出错。AEAD 把“加密”和“认证”合到一个明确接口里：输入 key、nonce、plaintext、AAD，输出 ciphertext 和 tag；解密时只有 tag 验证通过，才返回 plaintext。调用方不需要自己发明 encrypt-then-MAC，也不需要决定哪些字段参与 MAC。

关联数据是 AEAD 很容易被忽略的部分。AAD 不被加密，但会被认证。典型用途是协议头、record type、tenant id、key id、version、sequence number、content type、object metadata。这些字段接收方需要明文读取，但不能被攻击者悄悄改掉。比如 LogServe 的 result object 如果加密，object id、workflow id、tenant id、content type、key version 可以作为 AAD；这样攻击者不能把一个租户的密文挪到另一个租户上下文里通过验证。

说它解决正确性，是因为接口强迫调用方处理几个不变量：同一 key 下 nonce 不能重复；tag 验证失败不能返回 plaintext；AAD 必须和加密时一致；key 和算法要有版本。只要库设计得好，调用方少犯一类“忘记 MAC”“MAC 没覆盖 header”“先解密再校验”的错误。

说它改善可维护性，是因为 AEAD 把加密封装成一个稳定抽象。上层代码只关心 seal/open，不关心块模式、padding、MAC 顺序。以后从 AES-GCM 换到 ChaCha20-Poly1305 或换 key version，接口不会大改。但这不意味着可以不懂密码学边界；nonce 管理、key 管理、随机数和错误处理仍然要由系统设计保证。

结合 LogServe，AEAD 适合保护 result object、checkpoint、snapshot、敏感 task payload、LLM prompt/result 这类需要存储或传输的内容。它不能替代访问控制。一个 worker 如果拿到了 decrypt capability，AEAD 不能阻止它读取明文；它只能保证密文没被篡改，且没有密钥的人看不到内容。

面试里可以这样回答：

```text
AEAD 的核心目标是把机密性、完整性和来源认证放在一个接口里，并支持 AAD 这种只认证不加密的上下文字段。它主要解决安全性，也减少组合加密和 MAC 时的正确性错误。典型接口是 key、nonce、plaintext、AAD 输入，输出 ciphertext/tag；解密时 tag 不通过就不能返回明文。它不能替代授权和密钥管理，nonce 唯一性仍然是系统必须保证的不变量。
```

## Q037. AEAD 的典型适用场景和不适用场景分别是什么？

AEAD 适合“我需要隐藏内容，同时要防篡改”的场景。网络协议 record、数据库字段、对象存储 blob、cookie/session、备份、消息队列 payload、本地缓存文件、模型 checkpoint、日志中的敏感 payload，都可以考虑。只要攻击者可能读到或改动密文，AEAD 就比单纯加密或单纯 hash 更贴近需求。

典型场景之一是传输层和消息层。TLS 1.3 record 使用 AEAD 思路保护应用数据；应用自己的消息如果跨不完全可信的队列或存储，也可以用 AEAD 做端到端保护。比如 control 把任务 payload 放进队列，队列服务本身不应该看到明文，worker 拿到密文后再解密，tenant id、task id、schema version 可以作为 AAD。

第二个场景是对象存储。S3/MinIO 这类对象存储可能有服务端加密，但应用层 AEAD 能把信任边界收得更紧：对象服务只能存 bytes，不能解密内容；对象 key、版本、租户、content type 作为 AAD，防止密文被换上下文。对 LogServe 的 result object、actor snapshot、LLM prompt/result，这个模型很自然。

第三个场景是本地持久化。checkpoint cache、磁盘上的 token cache、worker local model metadata、临时结果，如果机器被误配置、备份泄露或普通用户读到文件，AEAD 至少能保护内容和篡改检测。这里要注意 nonce 持久化和 key version，不然重启后很容易踩重复 nonce。

不适用场景也要讲清楚。AEAD 不适合密码存储，密码存储要用 Argon2、bcrypt、scrypt 这类慢哈希/KDF，而不是可逆加密。AEAD 不适合公开完整性验证，如果任何人都要验证发布物来源，应该用数字签名；AEAD 的验证者必须有共享密钥。AEAD 不适合只要防随机损坏的高性能路径，CRC/checksum 更便宜。AEAD 也不适合解决授权：它不能判断用户是否该访问某个对象，只能让没有密钥的人打不开。

还要避免把大流式数据随便塞进单次 AEAD。很多 AEAD 接口需要一次性处理完整 plaintext/ciphertext，超大文件要做分块，每块有独立 nonce、chunk number 和 AAD，并认证整体长度或 manifest。否则中间块删除、重排、截断可能不容易被上层发现。

面试里可以这样回答：

```text
AEAD 适合需要同时保密和防篡改的数据，比如网络 record、队列消息、对象存储 blob、session、checkpoint、snapshot 和敏感 payload。AAD 可以放租户、版本、对象 id、序列号等不加密但不能被改的上下文。它不适合密码存储，不适合公开验签，不适合只防随机损坏，也不能替代授权。大文件要分块并认证 chunk 序号、长度和 manifest，不能简单把 AEAD 当无限流。
```

## Q038. AEAD 和相近概念最容易混淆的边界在哪里？

最容易混淆的是 AEAD、普通加密、MAC、数字签名、TLS、KMS 和访问控制。它们都和安全有关，但目标不同。

AEAD 和普通加密的边界在完整性。普通加密只强调没有密钥看不到明文，很多模式本身可篡改。AEAD 要求密文或 AAD 被改后验证失败，不返回明文。面试里如果只说“AES 加密了所以安全”，会被继续追问用的是 ECB、CBC、CTR、GCM，是否有 tag，nonce 怎么管。

AEAD 和 MAC 的边界在机密性。MAC 只认证消息，不隐藏内容；AEAD 同时加密 plaintext 并认证 ciphertext/AAD。如果 payload 本来就可以公开，只要防篡改，用 HMAC 或签名可能更合适。反过来，如果 payload 有敏感内容，只做 HMAC 不能防泄露。

AEAD 和数字签名的边界在验证者和责任归属。AEAD 通常使用对称密钥，能解密/验证的人也能伪造新的密文。数字签名用私钥签、公钥验，适合发布物、审计证据、跨组织验证。你不能用 AEAD 证明“只有发布者签了这个模型权重”，因为所有持有共享密钥的人都能生成有效 tag。

AEAD 和 TLS 的边界在层次。TLS 保护连接上的传输，数据到达 TLS 终止点后就是明文。如果网关终止 TLS，再把请求写到队列、日志、对象存储，TLS 不再保护后续链路。应用层 AEAD 可以做端到端或存储保护。反过来，已经在可信单跳 TLS 内传输的短期内部数据，不一定要再做应用层 AEAD，除非存储或中间系统不可信。

AEAD 和 KMS 的边界在职责。KMS 管 key、做 envelope encryption、记录 decrypt 调用，AEAD 是数据加密算法或模式。KMS 不会自动替你选择正确 AAD，也不会替你保证 nonce 唯一。很多云 SDK 封装了 AEAD，但系统的不变量仍要理解。

AEAD 和授权的边界更重要。拥有密钥就能解密，不代表业务允许。没有密钥打不开，也不代表审计和租户隔离完成。权限判断要在发放密钥、capability 或 decrypt 服务时做，而不是指望密文自己懂租户。

结合 LogServe，我会这样划分：TLS 用于 SDK/control/worker 传输；AEAD 用于 result object 和 snapshot 内容；HMAC 用于日志指纹或内部消息认证；签名用于 release artifact 和审计 segment manifest；KMS 管 key；授权系统决定谁能拿到 decrypt capability。每个机制只承担自己的边界。

面试里可以这样回答：

```text
AEAD 容易和普通加密、MAC、签名、TLS、KMS、授权混淆。普通加密不一定防篡改，MAC 不保密，签名可公开验证而 AEAD 通常是共享密钥，TLS 只保护连接不保护落盘后的数据，KMS 管密钥不自动解决 nonce/AAD，授权决定谁能拿密钥或解密能力。AEAD 的位置是：对某段数据同时做保密和认证，并把上下文放进 AAD。
```

## Q039. AEAD 在高并发场景下可能出现哪些隐藏问题？

高并发下最危险的是 nonce 重复。同一个 key 下，很多 AEAD 算法要求每次加密 nonce 唯一；AES-GCM 一旦 nonce 重用，机密性和完整性都可能严重受损。单线程里用递增计数器很简单，多 worker、多进程、多节点、崩溃重启后就麻烦了。两个实例如果从同一个计数器起步，或者随机 nonce 熵不足，都会出事。

第二个问题是 key/nonce 状态竞争。为了避免重复 nonce，系统可能维护一个全局 counter。如果每次加密都抢同一把锁，高并发下会变成瓶颈；如果为了性能把 counter 分片，又要确保分片前缀唯一。常见做法是 key + instance id + local counter，或者从 KMS/协调服务分配 nonce range，但要处理 range 持久化和重启回收。

第三个问题是随机数质量和阻塞。并发生成 nonce、data key、salt 时依赖 CSPRNG。现代系统通常没问题，但容器冷启动、大量短命进程、错误使用 `math/rand`、自己拼时间戳，都可能引入低熵。nonce 不一定需要保密，但需要唯一；key 必须不可预测。把时间戳毫秒 + goroutine id 当 nonce 是很危险的。

第四个问题是错误处理造成侧信道或降级。高并发服务为了吞吐，可能在 tag 验证失败后仍返回部分解析错误，或者把解密失败和授权失败区分得过细，让攻击者做 oracle 探测。AEAD open 应该是原子语义：验证失败就没有 plaintext。上层不要先解析明文头再校验 tag；需要明文路由的信息应放 AAD 或外部 metadata。

第五个问题是内存和拷贝。AEAD seal/open 对大 payload 可能产生额外分配；Go 里如果每次请求都创建 cipher、分配 buffer、从 KMS 拉 key，会带来 CPU 和 GC 压力。正确做法是复用 key schedule 或 cipher.AEAD 对象，但不要复用会产生数据竞争的 mutable buffer。AAD 和 plaintext buffer 的生命周期也要小心，避免明文在内存中停留太久或被日志打印。

第六个问题是多租户 key 管理。所有租户共用一把 key，会放大 blast radius；每个对象一把 key，又会把 KMS 打爆。常见折中是 tenant key 或 data key envelope encryption，加缓存和短 TTL。高并发下 key cache 要有访问控制，不要因为缓存命中就绕过授权。

结合 LogServe，如果多个 worker 并发写加密 result object，我会避免“全局随机拼 nonce”的随意做法。可以为每个 data key 分配唯一 object id，把 object id/segment id/chunk number 纳入 nonce 或 AAD；大对象按 chunk 加密，每个 chunk 有 chunk index；key version 和 tenant id 进 AAD。cipher 对象可以复用，nonce 分配必须有明确不变量。

面试里可以这样回答：

```text
AEAD 高并发下最大坑是同一 key 下 nonce 重复，特别是 AES-GCM。多进程、多节点、崩溃重启会让简单 counter 失效；随机 nonce 也要有足够熵。还要注意 nonce 分配锁竞争、CSPRNG 使用、tag 验证失败的原子错误处理、大 payload 的内存分配、KMS/key cache 压力和多租户 blast radius。设计时要明确 key、instance、counter、chunk、AAD 的不变量。
```

## Q040. AEAD 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

崩溃重启对 AEAD 最大的威胁仍然是 nonce 状态丢失。进程内 counter 如果没有持久化，重启后从 0 开始，同一 key 下就可能复用 nonce。尤其是批处理、日志加密、对象分块上传这类场景，重试很自然；如果重试重新加密同一 plaintext 却复用 nonce，安全边界会被破坏。

第二个边界是“加密成功但写入失败”。假设系统先生成 nonce 和 ciphertext，然后写对象存储时超时。客户端不知道对象是否已经写入，于是重试。重试如果使用同一个 nonce 和同一个 plaintext，从密码学角度不一定比第一次更坏，因为密文相同；但如果实现重新生成不同 plaintext 元数据、部分内容变化或同 nonce 加密不同内容，就危险。更稳的是让对象写入具备幂等 id：同一个 object id、chunk id、attempt 对应确定 nonce 和内容，或者失败后废弃整个 data key。

第三个边界是部分写入。大对象分块时，前几块写成功，后几块失败；重试时如果 chunk nonce 分配不稳定，可能覆盖、重排或截断。每个 chunk 的 AAD 应包含 object id、chunk index、total chunks 或 manifest digest。最后再认证 manifest，确保接收方能发现缺块、重复块、重排和截断。

第四个边界是 tag 验证失败后的状态更新。解密失败不能更新 materialized view、不能推进 offset、不能把对象标记为已处理。否则攻击者可以用坏密文制造状态跳跃。处理逻辑应该是：读取密文，验证 tag，得到明文，校验业务 schema，再提交状态。任何失败都要停在可重试或隔离状态。

第五个边界是 key rotation 和重试交错。加密时用 key v1，重试时系统默认 key 已切到 v2；如果 metadata 没记录 key version，解密端会失败。正确做法是 ciphertext metadata 明确记录 algorithm、key id/version、nonce、AAD schema version。轮换期间解密端支持旧 key，写入端使用新 key，过了保留期再撤旧。

第六个边界是超时导致的未知结果。KMS decrypt 超时、对象存储写超时、控制面提交超时，都可能出现“实际成功但调用方以为失败”。这和分布式系统幂等问题一样，需要 request id、object generation、conditional write、lease/epoch，而不是靠重试次数猜。

结合 LogServe，result object 和 snapshot 加密要和 log-first 语义配合。可以先写对象，再在 shared log 里提交对象 digest、nonce/key metadata 和 result ref；如果 log append 失败，对象可以被 GC；如果对象写失败，不提交完成事件。worker 重试同一 task attempt 时，要么复用同一 result object id 和确定 nonce，要么生成新 attempt id，并保证旧 attempt 不会被错误接受。

面试里可以这样回答：

```text
AEAD 在崩溃、重启和重试里主要暴露 nonce 状态、部分写入、key rotation 和未知结果问题。进程内 counter 重启后不能从零再用；分块对象要把 chunk index、object id、总长度或 manifest 放进 AAD；tag 验证失败不能推进状态；ciphertext metadata 要记录 algorithm、key id/version、nonce 和 AAD schema。超时后重试要靠幂等 object id、conditional write、lease/epoch，而不是随手重新加密。
```

## Q041. AEAD 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

AEAD 的瓶颈要看数据大小和系统形态。小消息高 QPS 时，固定开销、nonce 分配、对象创建、KMS/key cache、锁竞争可能比加密本身更明显；大对象时，CPU 加密吞吐、内存拷贝和 I/O 更重要；远程 KMS 或对象存储参与时，网络延迟可能远大于 AEAD 算法成本。

CPU 方面，AES-GCM 在有 AES-NI、ARM Crypto Extension、PCLMULQDQ 的机器上非常快；没有硬件加速时，ChaCha20-Poly1305 可能更稳定。性能问题经常不是“AEAD 慢”，而是每次请求重新初始化 cipher、频繁派生 key、使用没有硬件加速的算法，或者对小 payload 做过多封装。

内存方面，seal/open 可能复制 plaintext、ciphertext、tag、AAD。大对象如果一次性读入内存再加密，会造成峰值内存和 GC 压力。分块流式处理能把内存压下来，但要设计 chunk nonce 和 manifest 认证。Go 里还要注意 buffer 复用和明文生命周期，避免为了省分配引入数据竞争或把明文复用到日志里。

锁竞争主要来自 nonce 分配、key cache、metrics、KMS client 和对象池。一个全局 mutex 保护 counter，在 100k QPS 下会很显眼。可以用 per-key shard、预分配 nonce range、atomic counter、每实例唯一前缀 + 本地 counter。key cache 也要分片，避免所有请求抢同一个 map lock。

I/O 和网络常常是主瓶颈。对象存储写入、数据库读取、KMS decrypt、secret manager 拉 key、日志写入、跨 AZ 网络，都会比本地 AEAD 慢。优化前要 profile。很多系统把 KMS 放在每次请求路径上，结果瓶颈不是 AES，而是远程 KMS QPS 和 p99。

结合 LogServe，如果加密 result object，瓶颈很可能是对象存储 I/O 和 worker 本地磁盘，而不是 AEAD 算法。小的 log record 如果每条都单独 KMS 解密，会把 KMS 打爆；更好的做法是 data key 缓存、按对象/segment 加密、批量写入和异步验证。benchmark 要分开测纯 AEAD、buffer 拷贝、KMS、对象存储和 end-to-end。

面试里可以这样回答：

```text
AEAD 性能瓶颈不固定。小消息高 QPS 常见瓶颈是固定开销、nonce/key cache 锁、cipher 初始化和远程 KMS；大对象常见瓶颈是 CPU、内存拷贝和 I/O；跨网络对象存储或 KMS 时网络 p99 可能压过算法成本。优化要先 profile，分开看纯加密吞吐、buffer 分配、nonce 分配、key 获取和存储写入。不要为了省锁破坏 nonce 唯一性。
```

## Q042. AEAD 的 correctness test、stress test 和 benchmark 应该分别测什么？

Correctness test 要测密码学接口的不变量。给定 key、nonce、plaintext、AAD，seal 后 open 能还原；改 ciphertext 任意 bit 要失败；改 tag 要失败；改 AAD 要失败；改 nonce 要失败；空 plaintext、空 AAD、大 payload、不同算法、不同 key version 都要覆盖。还要用标准 test vector，避免自己实现或封装时字节序、tag 拼接、nonce 长度搞错。

Correctness 还要测错误处理。tag 验证失败不能返回部分 plaintext，不能更新状态，不能吞掉错误后返回空明文。AAD schema 要稳定：加密时用的 tenant id、object id、version，解密时必须一致。分块加密要测缺块、重排、重复、截断、错误 total length 和错误 manifest digest。

Stress test 测并发和异常。多 goroutine、多 worker、多进程同时加密，检查 nonce 是否重复；模拟崩溃重启，确认 counter/range 不回退；模拟 KMS 超时、对象存储部分写、任务重试、key rotation，确认不会用旧 nonce 加密新内容。还要跑 race detector 或等价工具，查 key cache、buffer pool、nonce allocator 是否有数据竞争。

Stress test 还要测资源边界。超大 payload、很多小 payload、恶意 AAD 长度、错误 key id 洪泛、tag failure 洪泛、KMS 限流、key cache miss 风暴，都可能让系统退化。安全系统常见问题是正常路径很好，攻击者发送大量坏密文后 CPU 全耗在解密和日志告警上。

Benchmark 要分层。第一层测纯 AEAD seal/open 的 ns/op、B/op、allocs/op、吞吐 MB/s；第二层测封装开销，包括 metadata、AAD 构造、buffer 分配、base64/JSON；第三层测 KMS/key cache；第四层测对象存储或日志写入；最后测 end-to-end。否则一个“AEAD 很慢”的结论可能实际是对象存储慢。

Benchmark 还要按 payload size 分桶：128B、1KB、16KB、1MB、64MB 的瓶颈完全不同。并发度也要变化：1、CPU 核数、2 倍核数、实际 worker 数。要分别报告成功解密和失败 tag 的成本，因为攻击流量可能大多是失败。

结合 LogServe，我会为 result object encryption 写 test vector 和篡改测试；为 chunked snapshot 写缺块/重排/截断测试；为 nonce allocator 写并发唯一性和重启测试；为 worker 写 KMS 超时和 retry 测试。benchmark 则分成纯 AEAD、加密对象写入、本地 checkpoint cache、shared log metadata 追加和完整 task 完成路径。

面试里可以这样回答：

```text
correctness test 测 seal/open、标准 test vector、篡改 ciphertext/tag/AAD/nonce 必失败、失败不返回明文、分块缺失重排截断能发现。stress test 测多线程 nonce 唯一性、崩溃重启、重试、key rotation、KMS 超时、坏密文洪泛和数据竞争。benchmark 分层测纯 AEAD、封装分配、nonce/key cache、KMS、存储 I/O 和端到端，并按 payload size 和并发度分桶。
```

## Q043. 如果要求从零实现一个简化版 AEAD，你会先定义哪些不变量？

面试里如果被要求“从零实现简化版 AEAD”，我会先澄清：真实生产不应该自己造密码算法，只能用成熟库和标准算法。所谓从零实现，适合考察接口设计和不变量，而不是手写 AES 或 Poly1305。先把这个边界说清楚，比直接写玩具算法更靠谱。

第一个不变量是同一 key 下 nonce 唯一。无论是随机 nonce、counter nonce，还是 instance prefix + counter，都必须证明不会重复。要定义 nonce 长度、生成方式、持久化方式、崩溃后恢复方式、多实例分配方式。只要这个不变量说不清，AES-GCM 这类算法就不能安全使用。

第二个不变量是 encrypt-then-authenticate 或直接使用标准 AEAD 原语。简化版接口可以封装 `Seal(keyID, plaintext, aad) -> envelope` 和 `Open(envelope, aad) -> plaintext`，内部调用库的 AES-GCM/ChaCha20-Poly1305。不要自己组合 CBC、padding、HMAC，除非题目明确要求解释组合顺序。即便组合，也必须 key separation，MAC 覆盖 algorithm、key id、nonce、ciphertext、AAD。

第三个不变量是验证失败不释放明文。Open 必须先验证 tag，失败时返回统一错误；不能把部分 plaintext 返回给上层，不能让错误类型泄露 padding/tag/parse 的细节，不能推进业务状态。任何解密后的 schema parse 都发生在认证通过之后。

第四个不变量是上下文绑定。AAD 应该覆盖 tenant id、object id、record type、version、key id、chunk index、content type。这样密文不能被搬到另一个对象、另一个租户、另一个协议上下文里复用。没有 AAD 的 AEAD 仍然安全地保护 bytes，但少了工程上下文绑定，容易出现替换攻击。

第五个不变量是 envelope 自描述。密文要带 algorithm、key id/version、nonce、tag、AAD schema version、maybe chunk metadata。不要把这些信息藏在外部配置里，否则轮换和恢复会失败。key id 可以明文，因为它不是 secret；真正的 key 由 KMS/secret manager 管。

第六个不变量是密钥生命周期。key 随机生成，按用途分离；不能拿用户密码直接当 AEAD key；rotation 期间新写用新 key，旧读支持旧 key；撤销时明确哪些对象受影响。测试环境 key 和生产 key 分开，日志不能打印 key 或 plaintext。

结合 LogServe，我会把简化版设计成 object encryption helper：输入 tenant、object id、content type、plaintext，内部申请 data key、生成 nonce、用 AAD 绑定 metadata，输出 envelope 和 ciphertext。shared log 只记录 envelope metadata 和 object digest，读取时必须拿到 tenant 授权后由 decrypt service 打开。

面试里可以这样回答：

```text
我不会从零造真实密码算法，只会定义简化 AEAD 封装并调用标准库。先定义不变量：同一 key 下 nonce 唯一；验证失败不返回明文；AAD 绑定 tenant、object id、version、record type；envelope 记录 algorithm、key id、nonce、tag 和 schema version；key 随机生成、按用途分离、可轮换；大对象分块时 chunk index 和 manifest 也要认证。这些不变量比手写加密轮函数更重要。
```

## Q044. AEAD 的常见误用是什么，误用后通常会产生什么线上症状？

最严重误用是 nonce 重复。同一 key 下重复 nonce，尤其是 AES-GCM，会破坏机密性并可能让攻击者伪造 tag。线上症状不一定立刻明显：系统仍然能正常解密，日志没有错误，但安全性已经失效。只有在审计 nonce、发现重复，或者出现异常明文泄露/伪造时才暴露。这个问题危险就在于它静默。

第二个误用是忽略 AAD。把 tenant、object id、version、record type 放在未认证 metadata 里，攻击者可能把密文从一个上下文挪到另一个上下文。线上症状可能是“偶发解密成功但业务对象不对”“跨租户对象引用异常”“旧版本 ciphertext 被新逻辑接受”。如果没有审计，很难定位到是上下文未绑定。

第三个误用是 tag 验证失败后仍处理数据。有些开发者为了调试，会在解密失败时打印 plaintext buffer，或者先解密再验证，再 parse header。线上症状是坏密文触发奇怪 parser 错误、panic、状态推进，甚至日志里出现敏感片段。正确语义是认证失败就没有明文。

第四个误用是自己拼算法。比如 AES-CBC 不带 MAC、AES-CTR + hash、`hash(secret + ciphertext)`、同一 key 同时做 encryption 和 MAC、MAC 不覆盖 nonce/AAD/algorithm。线上症状可能是 padding oracle、bit flipping、篡改未检测、跨协议攻击。使用标准 AEAD 库就是为了少踩这些坑。

第五个误用是 key 管理混乱。所有环境共用 key，所有租户共用 key，key 写在 env 或镜像里，轮换时没有 key id，删除旧 key 后历史数据打不开。线上症状是批量解密失败、回滚失败、跨环境数据可读、事故 blast radius 过大。

第六个误用是错误处理和重试没有幂等。写对象超时后重试，nonce/counter 状态回退；分块上传失败后 chunk index 错乱；key rotation 期间 metadata 不完整。线上症状是偶发 `authentication failed`、只有重启后出现解密失败、某些 snapshot 无法恢复。

第七个误用是把 AEAD 当万能安全。数据加密了，但 decrypt key 给了所有 worker；日志里打印明文；授权检查在解密之后才做；密文和 key 放同一个 bucket。线上症状是权限绕过和数据泄露，但团队误以为“我们用了加密所以没问题”。

结合 LogServe，最需要盯的是 result object、snapshot 和 model cache 的 nonce/key/AAD。比如 snapshot 加密时不把 actor id 和 snapshot sequence 放进 AAD，攻击者可能替换同租户内不同 actor 的 snapshot。worker 如果拿全局 decrypt key，租户隔离也会失效。AEAD 只能保护 bytes，不能替代 capability。

面试里可以这样回答：

```text
AEAD 常见误用包括 nonce 重复、AAD 漏掉上下文、tag 失败还返回或处理明文、自己拼 CBC/CTR/HMAC、key id/rotation 混乱、大对象分块不认证顺序，以及把 AEAD 当授权。线上症状可能是静默安全失效、偶发 authentication failed、重启后历史数据打不开、跨对象替换、坏密文触发 parser 错误、日志泄露明文。最危险的是 nonce 重复，因为系统可能看起来仍然正常。
```

## Q045. AEAD 在单机和分布式环境中的语义有什么差异？

AEAD 的密码学语义在单机和分布式里不变：同一 key 下 nonce 唯一，tag 验证通过才返回明文，AAD 必须一致。变化的是这些不变量由谁保证。在单机里，一个进程可以维护 counter、key cache 和本地文件；在分布式里，多个节点、多个进程、多个区域、崩溃重启、重试和并发写入会让不变量难很多。

单机环境最简单的设计是本地 key + 单调 counter + 本地持久化 metadata。只要进程控制所有写入，并且 crash recovery 不回退 counter，nonce 唯一性可控。测试也容易：杀进程、重启、检查 counter。缺点是扩展性和可用性有限，key 存在单机上也有风险。

分布式环境下，nonce 分配要全局唯一或按 key 分片唯一。可以给每个节点分配唯一 prefix，再用本地 counter；也可以为每个对象生成独立 data key，让 nonce 空间局部化；也可以由协调服务分配 range。每种方案都要处理节点身份复用、range 持久化、时钟不可靠、重试、旧节点恢复。不要用“当前时间 + 随机数”糊弄，概率风险和实现 bug 都会积累。

分布式还多了 key 分发和授权。哪个服务能拿到 data key？KMS decrypt 是否每次都做授权？key cache 怎么过期？租户迁移、区域灾备、备份恢复时旧 key 是否可用？如果只把 key 分发给所有 worker，AEAD 的隔离价值会下降。更好的方式是 envelope encryption：对象用 data key，data key 由 tenant/master key 包裹；decrypt capability 按请求授权。

数据复制也改变了边界。密文对象可能复制到多个区域，metadata log 可能落后，key rotation 可能不同步。解密端要依赖 ciphertext envelope 自描述，而不是依赖“当前配置”。AAD 里的 region、tenant、object id、version 要稳定。跨区域恢复时，如果 KMS key 或 key material 没有恢复，密文也打不开。

分布式里的失败更复杂。一个 worker 加密并上传对象成功，但 complete event 追加失败；另一个 worker 重试同一任务；旧 worker 延迟写回；control failover 后重复调度。AEAD 本身不会解决这些，需要任务 attempt、lease、epoch、object generation 和 conditional write 配合，确保只有被接受的结果进入状态机。

结合 LogServe，单机实验可以先用本地 key 管理和对象级 data key；如果扩到多节点，就要把 key 管理、nonce 分配、object id、worker lease 和 shared log 提交统一起来。我的原则是：AEAD envelope 自描述，data key scoped 到 tenant/object，nonce scoped 到 object/chunk，control 用 log-first 记录哪个 digest 被接受，旧 attempt 的密文即使存在也不会被读取。

面试里可以这样回答：

```text
AEAD 密码学语义不因单机或分布式改变，变的是不变量的实现成本。单机可以用本地 counter 和 key cache；分布式要解决多节点 nonce 唯一、key 分发、KMS 授权、key cache、区域复制、重试和旧 worker 写回。常见做法是每对象 data key、节点唯一前缀加 counter、envelope 自描述、AAD 绑定 tenant/object/chunk，并用 lease/epoch/conditional write 处理分布式失败。AEAD 不替代分布式一致性。
```
## Q046. HMAC 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

HMAC 的核心目标是消息认证：接收方用共享密钥验证一段消息是否由知道这把密钥的一方生成，并且内容没有被篡改。它主要解决安全性问题，也能改善正确性和可维护性。性能通常很好，因为它基于 hash 函数，开销可预测，实现成熟。

HMAC 不是“加密”。它不隐藏消息内容，只产生一个 tag。消息本身如果是明文，任何能看到消息的人仍然能读内容。HMAC 也不是普通 hash。普通 SHA-256 没有密钥，攻击者改了消息后可以重新算 digest；HMAC 使用 secret key，攻击者不知道 key，就不能为篡改后的消息生成有效 tag。

HMAC 解决的典型问题是“我收到的 webhook、内部 API 请求、日志记录、下载 manifest、cookie 内容，是不是被改过”。例如一个 webhook provider 和你的服务共享 secret，provider 对 payload、timestamp、request path 计算 HMAC，你验证通过后才处理。攻击者即使截获 payload，也不能改金额或用户 id 后重新签出合法 tag。

它对正确性的帮助在于接口简单：明确 canonical message、key、hash algorithm、tag encoding、比较方式。只要规范写清楚，就能避免自己发明 `SHA256(secret + message)` 这种不安全拼法，避免漏掉 method/path/timestamp 这类上下文。HMAC 还能防止很多 hash length extension 问题，因为它的构造不是简单拼接 secret 和消息。

它对可维护性的帮助来自稳定性。HMAC 的概念很老，库支持广，跨语言一致性好。只要把 canonicalization 做好，Go、Python、Java 都能生成相同 tag。相比手写签名格式，HMAC 更容易测试和排障。

结合 LogServe，HMAC 可以用于内部消息认证、审计日志链、token 指纹、对象 manifest、webhook 回调，不适合用来保护 prompt 或 result 的机密性。比如日志里记录 `HMAC(log_key, token)` 的截断值，可以关联同一个 token 的请求，又不暴露 token 明文。shared log record 如果要防篡改，也可以用 per-segment HMAC，但验证方需要共享密钥。

面试里可以这样回答：

```text
HMAC 的核心目标是用共享密钥做消息完整性和来源认证。它主要解决安全性：攻击者不知道 key，就不能为篡改后的消息生成有效 tag。HMAC 不加密内容，也不提供公开验签；它适合 webhook、内部请求、manifest、日志链、token 指纹等场景。相比普通 hash，它多了密钥；相比签名，它要求验证者也持有共享密钥。
```

## Q047. HMAC 的典型适用场景和不适用场景分别是什么？

HMAC 适合双方或多方已经共享 secret，并且只需要验证完整性和来源的场景。比如 webhook 签名、内部服务请求签名、API request signing、cookie/session 防篡改、下载 manifest、审计日志 hash chain、对象 metadata、幂等 key 指纹、token 指纹。它的前提是验证方可以安全持有同一把 key。

Webhook 是很典型的例子。第三方平台发送 payload 时，把 timestamp、body、path 计算 HMAC 放到 header。你的服务验证 timestamp 防重放，验证 HMAC 防篡改。这里不需要隐藏 payload，因为它通过 TLS 传输，主要担心有人伪造回调或改内容。

内部请求签名也适合 HMAC。服务 A 调服务 B 时，如果不想上完整 PKI 或签名体系，可以共享 per-service secret，对 method、path、body digest、timestamp、nonce 计算 HMAC。B 验证 tag、时间窗口和 nonce 后处理。缺点是密钥分发和轮换要做得好，多个服务共享同一 key 会扩大风险。

日志和指纹场景也适合。直接 hash 邮箱、token、身份证号这类低熵或可枚举字段不安全，攻击者可以字典枚举。用 HMAC 加一个只有服务知道的 key，可以生成稳定指纹用于关联，同时避免离线枚举。注意 HMAC 指纹仍可能是个人数据或敏感关联信息，不能随便公开。

不适用场景首先是需要保密时。HMAC 不加密，明文仍然可见。如果 payload 敏感，要用 AEAD 或 TLS/存储加密。第二是不适合公开验证。软件发布、模型权重、容器镜像、审计证明给第三方看，应该用数字签名；HMAC 验证者也能伪造 tag，无法做强责任归属。第三是不适合密码存储。密码存储需要 Argon2/bcrypt/scrypt 这类慢哈希；HMAC 太快，除非作为 pepper 层的一部分。

还要注意不适合跨大规模不完全信任组织共享。共享密钥越多，越难轮换和归责。一把 key 给十个服务，tag 只能证明“十个服务之一生成”，不能证明具体是谁。如果需要细粒度归责，用每服务 key 或非对称签名。

结合 LogServe，我会把 HMAC 用在日志 token fingerprint、worker registration request、内部 callback、防篡改 manifest 上；不会用它加密 result object，也不会用它发布 worker binary。发布物应该签名，敏感对象应该 AEAD，密码应该 Argon2。

面试里可以这样回答：

```text
HMAC 适合已有共享密钥、只需完整性和来源认证的场景，比如 webhook、内部请求签名、cookie 防篡改、manifest、审计日志链、token/PII 指纹。它不适合保密，因为不加密；不适合公开验签，因为验证者也能伪造；不适合密码存储，因为太快。共享 key 的范围越大，归责和轮换越难，所以生产里要按服务或用途分 key。
```

## Q048. HMAC 和相近概念最容易混淆的边界在哪里？

HMAC 最常和 hash、checksum、signature、AEAD、password hashing 混淆。边界一旦混掉，设计就会出现“看起来有校验，实际没有安全目标”的问题。

HMAC 和 hash 的边界是密钥。SHA-256(message) 任何人都能算，适合内容寻址、去重、非恶意完整性、可信 digest 对比。HMAC(key, message) 只有知道 key 的人能算，适合防恶意篡改。把 SHA-256 叫“签名”是常见错误；没有密钥就不能认证来源。

HMAC 和 checksum 的边界是对抗性。CRC32、Adler32、IP checksum 适合发现随机错误，速度快，但攻击者可以轻松构造新 checksum。HMAC 抵抗不知道 key 的攻击者。日志 segment 可以同时有 CRC 和 HMAC：CRC 快速发现损坏，HMAC 检查篡改。

HMAC 和数字签名的边界是对称/非对称。HMAC 的验证者和生成者共享 key，所以验证者也能伪造。数字签名只有私钥持有者能签，公钥验证者不能伪造，适合发布物、跨组织证明和不可抵赖。内部系统为了性能用 HMAC 可以，公开分发就不够。

HMAC 和 AEAD 的边界是机密性。HMAC 只认证，不隐藏；AEAD 既加密又认证。把 token payload HMAC 后放给客户端，客户端仍能看到 payload；如果 payload 包含敏感信息，就要加密或不要放客户端。JWT 的 JWS 就是签名/认证而不是加密，JWE 才是加密。

HMAC 和 password hashing 的边界是速度和盐。HMAC 很快，不适合直接存用户密码。密码存储要用慢、可调成本、带 salt 的 KDF。HMAC 可以作为 pepper 的一层，但不能替代 Argon2。

还有一个常见混淆是“HMAC 能不能防重放”。HMAC 本身不能。攻击者如果截获一条完整消息和 tag，可以原样重放。防重放要把 timestamp、nonce、sequence、request id 放进被 HMAC 覆盖的消息，并在服务端记录或限制窗口。HMAC 只保证这几个字段没被改。

面试里可以这样回答：

```text
HMAC 和 hash 的区别是有没有密钥；和 checksum 的区别是能不能防恶意篡改；和签名的区别是 HMAC 是共享密钥，验证者也能伪造；和 AEAD 的区别是 HMAC 不保密；和密码哈希的区别是 HMAC 太快，不能直接存密码。HMAC 本身也不防重放，必须把 timestamp、nonce、sequence 等字段纳入 MAC 并在服务端检查。
```

## Q049. HMAC 在高并发场景下可能出现哪些隐藏问题？

HMAC 算法本身通常不是高并发瓶颈，隐藏问题更多在 key 管理、canonicalization、重放防护、对象复用和日志放大。

第一类问题是 HMAC 对象复用错误。很多语言的 HMAC/hash 对象是有内部状态的，不是线程安全的。为了省分配，把同一个 HMAC instance 放到全局多 goroutine 复用，会导致 tag 错乱、数据竞争，甚至把一个请求的内容混进另一个请求。正确做法是每次创建，或者用 pool 但取出后 reset，且同一实例只在一个 goroutine 使用。

第二类问题是 canonicalization 不稳定。高并发系统里请求可能经过不同网关、不同语言 SDK、不同 JSON 序列化器。Header 大小写、URL encoding、query 参数顺序、JSON 字段顺序、空格、换行、Unicode normalization、body gzip，都可能让同一业务请求在两端算出不同 HMAC。线上症状是少量请求签名失败，和地域、SDK 版本、代理有关。签名规范必须非常具体，最好对 body bytes 或 canonical form 签名，而不是对“逻辑对象”随手序列化。

第三类是重放状态的并发一致性。HMAC 只认证消息，不防重放。你把 timestamp/nonce 放进去后，服务端还要检查 nonce 是否用过。高并发下，同一个 nonce 的两个请求同时到达，如果检查和写入不是原子操作，可能都通过。需要用唯一约束、原子 set-if-absent、短窗口缓存或 sequence 单调检查。

第四类是 key cache 和轮换。每个请求都查 secret manager 会慢；缓存 key 又要处理过期、撤销、版本并存。高并发下轮换瞬间，旧 key 和新 key 都可能存在。验证端一般要按 key id 查对应 key，或者在短 grace window 内尝试当前和前一版本，但不能无限尝试所有历史 key，否则会放大攻击成本。

第五类是失败流量放大。攻击者可以发送大量错误 tag 请求，让服务不停计算 HMAC、打日志、查 key、查 nonce。HMAC 计算便宜，但如果每次失败都写一条大日志或查远程 KMS，就会被拖垮。要在边缘做大小限制、key id allowlist、速率限制，失败日志采样。

结合 LogServe，如果 SDK 请求用 HMAC 签名，要固定 canonical string，例如 method、path、body SHA-256、tenant、timestamp、nonce；验证时先做大小和时间窗口检查，再查 key；nonce set-if-absent 要按 tenant/key id 原子写入。worker 内部如果用 HMAC 生成 token 指纹，HMAC 对象不能跨 goroutine 共享 mutable state。

面试里可以这样回答：

```text
HMAC 高并发问题通常不是算法慢，而是实现细节：HMAC 对象有状态不能跨线程共享；canonicalization 在不同 SDK/网关之间不一致会导致偶发签名失败；nonce 防重放检查要原子；key cache 要处理轮换和撤销；大量坏 tag 请求可能放大日志、KMS 和查询成本。设计时要固定签名规范、按 key id 查 key、失败日志采样，并给内部状态做并发安全。
```

## Q050. HMAC 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

HMAC 本身是无状态计算，但围绕它的防重放和 key rotation 是有状态的。崩溃、重启、超时、重试主要会暴露这些状态的边界。

第一个边界是 nonce/timestamp 防重放状态丢失。服务端如果只在内存里记录最近 nonce，重启后攻击者可以重放窗口内旧请求。对于低风险 webhook，短时间内存窗口可能能接受；对于资金、权限、任务完成这类高风险操作，需要持久化 request id、幂等 key、sequence 或数据库唯一约束。timestamp 只能限制窗口，不能单独防重放。

第二个边界是客户端重试。客户端请求超时，不知道服务端是否处理成功，于是重发同一 body 和 HMAC。如果服务端把 nonce 标记为已用后才处理业务，重试会被拒绝，客户端无法拿到结果；如果处理业务后才标记 nonce，崩溃时又可能重复执行。更好的做法是把 request id/idempotency key 纳入 HMAC，服务端用幂等表记录处理中、成功或失败，重试返回同一结果。

第三个边界是签名时间窗口和时钟漂移。重启后机器时间错误、NTP 跳变、容器时间异常，会让大量请求签名过期或未来时间。验证端要允许小范围 clock skew，但不能太宽；太宽会扩大重放窗口。内部系统可以用 sequence 或 lease epoch 减少对 wall clock 的依赖。

第四个边界是 key rotation。请求用旧 key 签名，服务端刚切新 key；或者服务端刚撤旧 key，客户端重试旧请求。需要 key id/version、grace window 和双写/双验策略。没有 key id 时，服务端只能试多个 key，既慢又容易出错。撤销高风险 key 时可以立即拒绝旧 key，但要接受相应的失败和重放处理。

第五个边界是部分提交。比如 LogServe worker 完成任务时发送 HMAC 请求，control 验签成功并写 shared log，但响应超时；worker 重试，control 必须识别同一 attempt completion，而不是写两条冲突完成事件。HMAC 保证请求没被改，不能保证业务幂等。

结合 LogServe，任务完成、worker 心跳、SDK submit 都应该有 request id、attempt、lease、timestamp，且这些字段在 HMAC 覆盖范围内。control 验签后用 idempotency table 或 shared log 里的唯一语义处理重试。重启后防重放状态不能只靠内存，至少关键路径要落到 metadata store 或 log。

面试里可以这样回答：

```text
HMAC 自身无状态，但防重放和轮换有状态。崩溃重启会丢内存 nonce，超时重试会让同一签名请求重复到达，时钟漂移会影响 timestamp 窗口，key rotation 会造成新旧 key 交错。解决方式是把 request id、timestamp、nonce、attempt、lease 等字段纳入 HMAC，再用幂等表、唯一约束或 log 语义处理重试；key 要带版本并支持受控 grace window。
```

## Q051. HMAC 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

纯 HMAC 计算一般很快，特别是 HMAC-SHA-256 在现代 CPU 上吞吐很高。真实性能瓶颈通常来自外围：消息 canonicalization、JSON 序列化、body 读取、key 查找、nonce 存储、日志和网络。

CPU 瓶颈会出现在大 payload 或高 QPS 场景。比如每个请求都对 50MB body 做 HMAC，CPU 和内存带宽都会明显；如果只是几百字节的 canonical string，HMAC 本身通常不是问题。可以对 body 先计算 SHA-256 digest，再把 digest 放进 canonical string 做 HMAC，但要确保 digest 算法和 body bytes 定义清楚。

内存瓶颈来自拷贝。为了签名把整个 request body 读到内存、转字符串、base64、JSON canonicalize，会造成 GC 和峰值内存。更好的方式是 streaming hash/HMAC，或在网关层限制 body size。不要为了 HMAC 把大对象在内存中复制多份。

锁竞争来自 key cache、nonce cache、metrics 和对象池。每个请求验签都抢同一个 key map lock，或者所有 nonce 写同一个 Redis key，会在高并发下退化。可以按 key id/tenant 分片，nonce 存储用 set-if-absent 加 TTL，指标异步聚合。HMAC 对象池如果用全局锁，也可能得不偿失。

I/O 和网络来自 secret manager、数据库、Redis、日志系统。每次验签都远程取 key，或者每次 nonce 都写强一致数据库，延迟会远大于 HMAC。通常会缓存 key，nonce 防重放用短窗口 KV，关键操作再落持久化幂等表。失败日志不要同步写大对象。

结合 LogServe，如果内部 API 用 HMAC，我预期瓶颈会在 body digest、metadata store 幂等写、nonce set-if-absent 和日志，而不是 HMAC-SHA-256。benchmark 要分别测纯 HMAC、canonicalization、key cache 命中/未命中、nonce check、完整 RPC 验签路径。

面试里可以这样回答：

```text
HMAC 算法本身通常不是瓶颈。小消息下瓶颈多在 canonicalization、key cache、nonce 防重放、日志和网络；大 payload 下 CPU、内存带宽和 body 拷贝会明显。不要每次远程取 key，也不要为了签名复制整个 body 多次。优化要分层测纯 HMAC、body digest、序列化、key/nonce 存储和端到端 RPC。
```

## Q052. HMAC 的 correctness test、stress test 和 benchmark 应该分别测什么？

Correctness test 先测标准 test vector。HMAC 很容易跨语言使用，必须确保 key、message、hash algorithm、tag encoding、hex/base64、截断长度都一致。用 RFC 测试向量可以防止实现把 key 处理、block size、inner/outer pad 搞错。

然后测篡改：改 message 任意 bit 要失败，改 timestamp、method、path、query、body digest、tenant、nonce、key id 都要失败；改 tag 编码大小写、padding、截断长度要按规范处理。还要测常量时间比较，至少不要用普通字符串比较直接暴露早停时间；很多库提供 constant-time compare。

Canonicalization 是 correctness 的重点。测试要覆盖 query 参数顺序、URL encoding、重复 header、header 大小写、JSON 字段顺序、空 body、gzip body、Unicode、换行差异。最好的测试是用多语言 SDK 生成签名互验，防止 Go 端和 Python 端各自“正确”但不兼容。

Stress test 测并发和异常。多线程验签不能共享 mutable HMAC 状态；nonce set-if-absent 在并发重放下只能有一个成功；key rotation 时旧 key 和新 key 的窗口符合预期；服务重启后关键幂等状态仍在；大量坏签名不会打爆日志或 key store。还要跑 race detector 检查 key cache 和对象池。

Benchmark 要分成纯 HMAC、canonical string 构造、body digest、key lookup、nonce check、完整请求验签。按消息大小分桶，按并发度分桶，分别报告成功签名、错误 tag、未知 key id、过期 timestamp 的成本。错误路径很重要，因为攻击流量通常走错误路径。

结合 LogServe，我会为 SDK submit 和 worker complete 各放一组 test vector：同一个请求在 Go/Python 生成一致签名；改 attempt、lease、tenant、body digest 都失败。stress 里并发重放同一个 completion，只能有一个进入 accepted 状态，其余返回幂等结果或重复请求错误。

面试里可以这样回答：

```text
correctness test 用标准 HMAC 向量，并测 message、AAD 式上下文字段、tag、编码和截断篡改都失败；重点测 canonicalization，覆盖 query、header、JSON、Unicode、空 body 和多语言互验。stress test 测并发验签、nonce 原子防重放、key rotation、重启后幂等状态、大量坏签名和数据竞争。benchmark 分层测纯 HMAC、body digest、canonicalization、key lookup、nonce check 和端到端。
```

## Q053. 如果要求从零实现一个简化版 HMAC，你会先定义哪些不变量？

真实生产不应该自己手写 HMAC，直接用标准库。面试里的“简化版 HMAC”可以用来说明我理解它的结构：HMAC 不是 `hash(key + message)`，而是用 inner pad 和 outer pad 把 key 和消息按 hash block size 组合，避免简单前缀 MAC 的 length extension 等问题。

先定义第一个不变量：key 是 secret，有足够熵，按用途分离。webhook key、internal API key、log fingerprint key、cookie key 不能混用。key 可以比 hash block size 长，长 key 先 hash；短 key pad 到 block size。这些由标准 HMAC 算法处理，但系统层要负责生成和轮换。

第二个不变量：message 必须是唯一、稳定、无歧义的 bytes。不能说“签 JSON 对象”，而要说“签 UTF-8 canonical JSON bytes”或“签 method + newline + path + newline + body_sha256”。字段之间要有长度前缀或明确分隔，避免 `ab|c` 和 `a|bc` 这类歧义。参与安全决策的字段都必须被 HMAC 覆盖。

第三个不变量：验证必须使用 constant-time compare，并且失败错误不要泄露太多细节。未知 key id、tag 错误、消息过期可以在内部分类，但外部返回尽量统一。否则攻击者可以做枚举和探测。

第四个不变量：防重放字段要纳入消息，但状态检查在 HMAC 之外完成。timestamp、nonce、sequence、request id 被签名后不能被改；服务端还要检查时间窗口、nonce 是否用过、sequence 是否单调。HMAC 只能保护这些字段不被改，不能自动记住它们。

第五个不变量：tag 长度和编码固定。HMAC 输出可以截断，但截断长度要有明确安全边界，不能随客户端传。编码用 hex 还是 base64url 要固定，比较前做严格解析，不能接受多个等价格式导致绕过。

如果真要描述简化构造，可以写成：

```text
if len(key) > blockSize: key = H(key)
key = rightPadWithZero(key, blockSize)
inner = H((key XOR ipad) || message)
tag = H((key XOR opad) || inner)
```

但我会强调这只是教学，生产用库。

结合 LogServe，我会为 request signing 定义 canonical string，而不是直接 HMAC JSON。比如：version、method、path、tenant、timestamp、nonce、body_sha256、lease_id、attempt，每个字段长度前缀编码。这样后续加字段时可以升级 version，不破坏旧客户端。

面试里可以这样回答：

```text
我不会在生产手写 HMAC，但会先定义不变量：key 随机且按用途分离；message 是稳定、无歧义的 bytes；所有参与授权和幂等的字段都被覆盖；tag 固定算法、长度和编码；验证用 constant-time compare；timestamp/nonce/request id 被签名，但防重放状态由服务端原子检查。简化结构是 H((K xor opad) || H((K xor ipad) || message))，不要写成 hash(key + message)。
```

## Q054. HMAC 的常见误用是什么，误用后通常会产生什么线上症状？

最常见误用是把普通 hash 当 HMAC。比如 `SHA256(secret + message)`、`SHA256(message + secret)`、`MD5(secret + message)`。有些构造会受 length extension 或拼接歧义影响，即使没有被立刻攻击，也很难安全分析。线上症状通常是安全评审不过，或者被构造出“原消息签名能扩展到新消息”的漏洞。

第二个误用是 canonicalization 不完整。只签 body，不签 method/path/query；只签 JSON 对象，不规定序列化；不签 timestamp 和 nonce；不签 tenant 或 resource id。攻击者可能把同一 body 换到另一个 API、另一个租户、另一个路径。线上症状是签名看似通过，但业务上下文被替换。

第三个误用是比较 tag 用普通字符串比较。普通比较可能早停，理论上造成 timing side channel。很多 Web 场景里利用难度不一定低，但既然标准库有 constant-time compare，就不该偷懒。更常见的实际问题是编码解析宽松：接受大小写混杂、自动补 padding、截断 tag、忽略非法字符，导致绕过空间变大。

第四个误用是 key 太弱或共享范围太大。拿用户密码、短字符串、配置名当 HMAC key；所有服务、所有租户、所有环境共用同一 key；key 放进前端或移动端。线上症状是 key 泄露后全局失守，无法定位谁伪造，轮换影响巨大。

第五个误用是不防重放。请求有 HMAC，但没有 timestamp、nonce、sequence 或 request id。攻击者抓到一条合法请求后原样重发，HMAC 仍然通过。线上症状是重复扣款、重复任务提交、重复 webhook 处理。解决靠幂等和防重放状态，不是换 hash 算法。

第六个误用是日志泄露。验签失败时把 secret、canonical string、完整 token、payload 打进日志；或者为了排障打印 expected tag 和 received tag。线上症状是日志系统里出现可重放凭证或签名材料。日志最多记录 key id、tag 前缀、请求 id、失败原因分类。

结合 LogServe，若 worker complete 只对 body 做 HMAC，不签 lease/attempt/worker epoch，旧 worker 可能拿旧 body 伪造完成；若 SDK submit 不签 tenant，代理层误路由可能造成跨租户。HMAC 误用的症状常常不是“签名失败”，而是“签名成功但上下文错了”。

面试里可以这样回答：

```text
HMAC 常见误用包括用 hash(secret+message) 代替 HMAC、签名字段不完整、canonicalization 不稳定、普通字符串比较 tag、key 太弱或全局共用、不带 timestamp/nonce 导致重放、失败日志泄露 secret。线上症状可能是偶发签名失败、跨路径/跨租户重放、重复提交、key 泄露后全局失守，或者安全评审发现 length extension 和拼接歧义。
```

## Q055. HMAC 在单机和分布式环境中的语义有什么差异？

HMAC 的算法语义在单机和分布式中不变：同样的 key 和 message 产生同样的 tag，验证者持有同一 key 就能验证。差异在密钥分发、重放状态、时钟、canonicalization 和归责。

单机环境里，key 可以从本地 secret store 读取，nonce 防重放状态可以放内存或本地数据库，canonicalization 只有一个服务版本，排查简单。只要进程重启后状态处理得当，语义比较可控。

分布式环境里，多个实例都要验证同一种请求。key 必须一致地分发和轮换；key cache 可能有延迟；某些实例接受旧 key，某些实例只接受新 key，就会产生灰度期故障。最好在签名里带 key id/version，验证端按版本取 key，并定义旧 key 的明确 grace window。

防重放状态也变复杂。请求可能打到任意实例，nonce 不能只存在某台机器内存里。需要集中 KV、数据库唯一约束、分区 sequence，或者把请求路由到固定 shard。否则攻击者把同一请求发到两个实例，两个实例都没见过 nonce，就都处理了。

时钟和窗口也更难。多实例时钟漂移会导致一部分实例认为请求过期，一部分认为正常。内部系统如果能使用 sequence、lease、epoch，会比只靠 wall clock 稳。外部 webhook 则要设合理时间窗口，并监控时钟同步。

归责在分布式里也更重要。如果多个服务共享同一 HMAC key，tag 只能证明“某个持 key 的服务”签了消息。要定位具体服务，应按 service/tenant/environment 分 key，或者改用非对称签名。HMAC 不适合跨组织的强归责。

结合 LogServe，如果多个 control 实例验证 worker 请求，key cache、nonce store、lease state 必须共享或可一致查询。worker signing key 应按 worker 或 worker pool 分配；tenant SDK key 按租户分配；request id/nonce 存在 metadata store 或 shared log 中，避免多 control 实例重复接受。

面试里可以这样回答：

```text
HMAC 算法语义不变，分布式难在 key 分发、轮换、重放状态和归责。多实例要用 key id/version 和明确 grace window；nonce/request id 不能只放本机内存，要用集中 KV、唯一约束或分片 sequence；时钟窗口要考虑漂移；共享 key 范围越大，越难判断是谁签的。HMAC 只能证明持有共享 key 的某方生成了 tag，不能自动提供分布式幂等和责任归属。
```
## Q056. Argon2 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

Argon2 的核心目标是让密码猜测变贵。它主要解决安全性问题：当数据库里的 password hash 泄露后，攻击者不能低成本用 GPU/ASIC 大规模离线爆破用户密码。它不是为了让登录更快，恰恰相反，它通过可调 CPU、内存和并行度成本，让每次猜测都付出足够代价。

普通 hash 太快。SHA-256、SHA-3、BLAKE2 适合完整性、内容寻址、签名预处理，但不适合直接存密码。用户密码通常熵低，攻击者拿到 hash 后可以离线尝试字典、规则、泄露密码库和暴力组合。hash 越快，攻击者每秒能猜越多。Argon2 这类 password hashing/KDF 的目标就是故意慢，并且吃内存，让并行硬件的优势下降。

Argon2 有几个参数：memory cost、time cost、parallelism、salt、输出长度。salt 要每个密码唯一，防止同密码同 hash，也防止预计算彩虹表。memory cost 决定每次计算占多少内存，time cost 决定迭代次数，parallelism 决定内部并行度。参数不是越大越好，要在安全和可用性之间测出来：登录接口不能被自己拖垮，但攻击成本要足够高。

Argon2 主要用于密码存储，也可用于从低熵密码派生密钥。它不解决传输安全，不解决 token 存储，不解决消息认证，不解决数据加密本身。很多面试回答会说“密码加密”，这句话不准确。密码存储通常不是加密，因为系统不应该能解密得到原密码；它保存的是带 salt 和参数的不可逆 verifier。

正确性上，Argon2 的重点是参数和编码。存储结果要包含算法版本、参数、salt 和 hash，常见格式类似 `$argon2id$v=19$m=...,t=...,p=...$salt$hash`。以后参数升级时，新登录可以 rehash；旧 hash 仍能验证。不要只存裸 hash bytes，否则无法知道当年用什么参数。

结合 LogServe，如果需要存 SDK 用户密码或控制台登录密码，应使用 Argon2id 这类 password hash。API token 不应该用 Argon2 逐个验证高频请求，因为 token 本身应有高熵，可以存 HMAC/SHA-256 verifier 并做常量时间比较；Argon2 用在低熵人类密码上更合适。

面试里可以这样回答：

```text
Argon2 的核心目标是让离线猜密码变贵，主要解决密码存储的安全性问题。它通过可调 CPU、内存和并行度成本，让攻击者即使拿到 password hash，也不能廉价地用 GPU/ASIC 大规模爆破。它不是加密，不用于消息认证，也不适合高频 API token 验证。存储时要保存算法版本、参数、salt 和 hash，后续登录时可以按新参数 rehash。
```

## Q057. Argon2 的典型适用场景和不适用场景分别是什么？

Argon2 最典型的场景是存用户密码。注册或修改密码时，系统生成随机 salt，用 Argon2id 根据配置参数计算 hash，把算法、版本、参数、salt、hash 存进数据库。登录时取出这些参数重新计算并常量时间比较。数据库泄露后，攻击者只能离线猜，不能直接还原密码。

第二个场景是从人类输入的低熵秘密派生密钥。比如本地加密工具用用户 passphrase 保护私钥或备份文件，可以用 Argon2id 派生加密 key。这里要更谨慎，因为参数太低会被爆破，参数太高会让低端设备无法解锁。还要有 KDF 参数版本，方便以后升级。

第三个场景是 pepper 体系的一部分。密码 hash 本身有 per-user salt；pepper 是服务端额外 secret，可以在 Argon2 前后加入，比如先 HMAC-pepper(password) 再 Argon2，或 Argon2 后再 HMAC。pepper 放在 KMS/secret manager，不和数据库放一起。这样数据库单独泄露时，攻击者还缺 pepper。但 pepper 泄露或轮换会带来运维复杂度，不是所有系统都必须加。

不适用场景首先是高熵随机 token。API key、session token、refresh token 如果是 128/256 bit 随机值，本身不可猜，存 SHA-256/HMAC verifier 通常足够。用 Argon2 验证每个 API 请求会引入巨大 CPU/内存开销，容易被 DoS。高熵 token 的核心是生成足够随机、只显示一次、存 verifier、可撤销和限 scope。

第二个不适用场景是消息完整性和签名。Argon2 不替代 HMAC、数字签名、AEAD。它是 password hashing/KDF，不是 MAC。第三个不适用场景是普通文件完整性。文件 digest 用 SHA-256；发布物验证用签名；不要用 Argon2 算文件校验。

第四个不适用场景是需要可逆的 secret 存储。数据库密码、OAuth client secret、私钥如果应用需要原文使用，要用 secret manager/KMS 加密存储和访问控制，而不是 Argon2。Argon2 是不可逆 verifier，算完拿不回原文。

结合 LogServe，我会把 Argon2 用在控制台用户登录或本地管理密码，不会用它处理每次 SDK task submit 的 API token。SDK token 应是随机高熵，数据库存 HMAC verifier；如果泄露，撤销并轮换。LLM provider key、MinIO credential 则走 secret manager，不用 Argon2。

面试里可以这样回答：

```text
Argon2 适合存人类密码，或从低熵 passphrase 派生本地加密密钥；也可以和 pepper 组合降低数据库单独泄露的风险。它不适合高频 API token 验证，高熵随机 token 存 HMAC/SHA-256 verifier 更合适；也不适合消息认证、文件完整性、发布物签名或可逆 secret 存储。判断标准是输入是不是低熵且需要抗离线猜测。
```

## Q058. Argon2 和相近概念最容易混淆的边界在哪里？

Argon2 最容易和 hash、HMAC、加密、salt、pepper、KDF、token verifier 混淆。先讲边界，很多安全设计就清楚了。

Argon2 和普通 hash 的边界是成本模型。SHA-256 要快，Argon2 要慢且占内存。普通 hash 适合文件完整性、内容寻址、签名预处理；Argon2 适合低熵密码。把密码 `SHA256(password)` 存库，数据库一泄露就会被高速爆破。

Argon2 和 HMAC 的边界是目标。HMAC 用共享密钥认证消息，速度快，适合请求签名和指纹；Argon2 用 salt 和成本参数处理密码，故意慢，适合抗离线猜测。HMAC 可以用于 pepper 或 token 指纹，但不能替代 password hashing。

Argon2 和加密的边界是可逆性。加密后有 key 可以解密；Argon2 hash 后不能还原密码。密码存储应该是 verifier，不应该能解密出原密码。说“密码加密存储”通常是不精确的，正确说法是“使用带 salt 的 password hash 存储”。

Salt 和 pepper 也常混。salt 是每条密码记录随机、公开存储，目标是防同密码同 hash、防预计算；pepper 是系统级 secret，存 KMS/secret manager，不和密码表一起泄露。salt 不需要保密，但必须唯一随机；pepper 需要保密，但轮换复杂。

Argon2 和通用 KDF 的边界也要看输入熵。HKDF 适合从高熵密钥材料派生多个子密钥，速度快；Argon2 适合低熵密码，成本高。不要用 HKDF 直接处理用户密码，也不要用 Argon2 处理每个网络包的 key schedule。

Argon2 和 token verifier 的边界是熵和频率。随机 API token 如果足够长，攻击者无法枚举，存 HMAC 或 SHA-256 verifier 即可；用户密码熵低，要 Argon2。把所有 token 都用 Argon2 存，会让正常 API 验证很贵，还可能被恶意请求打爆。

结合 LogServe，用户控制台密码走 Argon2id；API token 走高熵随机 + verifier；内部 HMAC key 走 secret manager；对象加密 key 走 KMS/HKDF/AEAD。四类东西不要互相代替。

面试里可以这样回答：

```text
Argon2 和普通 hash 的区别是成本，Argon2 故意慢且吃内存；和 HMAC 的区别是 HMAC 认证消息、Argon2 抗密码猜测；和加密的区别是 Argon2 不可逆；salt 是公开的 per-password 随机值，pepper 是系统级 secret；HKDF 适合高熵 key 派生，Argon2 适合低熵密码。高熵 API token 通常不需要 Argon2，存 verifier 即可。
```

## Q059. Argon2 在高并发场景下可能出现哪些隐藏问题？

Argon2 的设计就是消耗 CPU 和内存，所以高并发下最大问题是资源耗尽。每次登录如果配置 64MiB 内存，100 个并发登录就可能占用数 GB 内存；再加上 CPU 时间，认证服务很容易被正常高峰或恶意请求打满。参数单看一次计算合理，不代表并发下合理。

第一类隐藏问题是登录 DoS。攻击者可以对登录接口发送大量不存在用户或错误密码请求。如果系统对每次请求都跑完整 Argon2，就会消耗大量 CPU/内存。为了防用户枚举，很多系统会对不存在用户也做类似耗时处理，但这会增加 DoS 面。需要配合 IP/user/device/tenant rate limit、验证码或风险控制、队列隔离、并发上限。

第二类是内存竞争和 OOM。Argon2 的 memory cost 是每次计算的工作内存，不是服务总内存。并发过高时，容器会 OOM，被 Kubernetes 重启，登录服务抖动。要根据实例内存、并发上限、p99 延迟和业务峰值调参数，并通过 semaphore 限制同时 Argon2 计算数。超出并发时排队或快速失败，不要让内核 OOM killer 决定。

第三类是参数升级造成容量突变。安全团队把 memory/time cost 翻倍，单次 benchmark 看起来只是从 100ms 到 250ms，但高峰并发下吞吐可能腰斩。参数升级要灰度，按用户登录逐步 rehash，不要启动时批量重算所有用户密码。

第四类是多租户不公平。一个租户被密码喷洒攻击，可能耗尽全局 Argon2 worker pool，影响其他租户登录。要按租户/来源做配额和隔离，至少要有全局上限和 per-tenant 上限。管理后台和用户登录也可以分池。

第五类是实现误用。每次请求生成 salt 后再验证旧密码是不对的；验证必须使用存储记录里的 salt 和参数。并发注册/改密时，要确保新 hash 原子替换旧 hash，session/token 失效策略清楚。不要把 Argon2 放到高频 API token 验证路径，否则攻击者不需要猜密码也能消耗大量内存。

结合 LogServe，如果控制台登录使用 Argon2，我会把它放在独立 auth 服务或至少独立 goroutine pool，设置并发上限和 rate limit。SDK token 验证不走 Argon2，避免每次 task submit 都消耗几十 MiB。参数调整要在压测环境测 p95/p99 和 OOM 风险。

面试里可以这样回答：

```text
Argon2 高并发最大问题是 CPU 和内存耗尽。memory cost 是每次计算的内存，并发登录会线性放大；攻击者可以用错误密码或不存在用户制造登录 DoS。要根据实例容量设置参数，用 semaphore 限制同时计算数，配合 IP/user/tenant rate limit，参数升级要灰度，不能把 Argon2 放进高频 API token 验证路径。不存在用户处理还要平衡防枚举和 DoS。
```

## Q060. Argon2 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

Argon2 是确定性计算：同样的 password、salt、参数会得到同样结果。崩溃重启不会像 AEAD 那样带来 nonce 重复问题，但会暴露注册、改密、rehash、登录超时和参数迁移的状态问题。

第一个边界是注册和改密的原子性。用户提交新密码，服务算 Argon2 hash，写数据库时崩溃。必须保证数据库里要么还是旧 hash，要么是完整的新 hash，不能写半条。存储格式要包含 algorithm、version、m/t/p、salt、hash，不能把参数和 hash 分开写到可能不同步的字段里。

第二个边界是 rehash on login。参数升级后，用户用旧参数 hash 登录成功，系统想顺手用新参数重算并更新。这个更新如果失败，不能影响本次登录；如果多个登录并发发生，要避免旧参数覆盖新参数。可以用 compare-and-swap：只有当前 hash 仍是旧值时才更新。

第三个边界是登录超时。Argon2 计算已经开始，客户端超时断开，服务端是否继续算？如果所有断开的请求都继续跑，会浪费资源；如果中途取消，需要实现支持上下文取消或把计算放进可控 worker pool。很多库的 Argon2 调用本身不一定可中断，所以并发上限更重要。

第四个边界是失败计数和锁定。密码验证超时或服务崩溃时，是否增加失败次数？如果在 Argon2 前增加，攻击者可以让合法用户被锁；如果在成功写入失败计数前崩溃，攻击者可以绕过限次。一般要把认证结果、失败计数、锁定策略放进事务或明确状态机。

第五个边界是 pepper 和 KMS。Argon2 前后如果用了 pepper，KMS/secret manager 超时会让登录失败。重启后 pepper key cache 为空，瞬时登录延迟上升；pepper rotation 时旧 hash 需要继续验证或要求用户重登改密。pepper 丢失会导致密码 verifier 无法验证，这比普通参数升级严重。

第六个边界是备份恢复。恢复旧数据库后，密码 hash 参数、pepper version、用户改密时间可能回退。用户最近修改的密码可能失效，旧密码复活，这是安全事故。认证数据恢复要和审计、session revocation、password changed timestamp 一起考虑。

结合 LogServe，如果有管理账号，改密事件可以写入审计日志；数据库更新用事务；rehash on login 用 CAS；auth 服务设置计算池，超时请求不无限堆积。pepper key version 记录在 hash metadata 中，恢复演练要验证旧版本 pepper 是否可用。

面试里可以这样回答：

```text
Argon2 不像 AEAD 有 nonce 问题，但崩溃和重试会影响注册、改密、rehash 和失败计数。密码记录要原子写入完整的算法、参数、salt、hash；登录成功后的参数升级要 CAS，失败不能影响本次登录；Argon2 计算要有并发上限，客户端超时不能无限消耗资源；失败计数和锁定要有事务语义；如果使用 pepper，还要处理 KMS 超时、轮换和备份恢复。
```

## Q061. Argon2 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

Argon2 的性能瓶颈主要来自 CPU 和内存，而且内存是它故意设计出来的成本。memory-hard 的意义就是让攻击者不能只靠大量并行计算核心廉价爆破；每次猜测都要占一定内存带宽和容量。对服务端来说，这也意味着登录吞吐受 CPU、内存带宽和并发内存上限约束。

CPU 影响 time cost 和并行度。time cost 越高，计算时间越长；parallelism 可以利用多核，但也可能和服务端整体并发争抢 CPU。登录服务通常不希望单次验证吃满所有核，所以参数要按实例规格和并发模型测出来。

内存影响更直接。memory cost 乘以并发数，就是最低工作集。容器 memory limit、Go/Python runtime 内存、其他请求处理、TLS、数据库连接都要一起算。Argon2 导致 OOM 时，症状可能不是“登录慢”，而是实例重启、连接中断、p99 飙升。

锁竞争通常不是 Argon2 算法内部的主问题，但会出现在认证服务外围：用户记录查询、失败计数更新、rate limit、KMS pepper key cache、worker pool semaphore。登录风暴下，这些共享状态可能先成为瓶颈。

I/O 和网络来自数据库读取密码记录、写失败计数、读取 pepper、审计日志、风控调用。单次 Argon2 如果配置 100ms，数据库 5ms 看起来小；但如果 KMS pepper 每次远程取 80ms，总延迟就会翻倍。key cache 和本地配置能减少远程依赖，但要处理轮换。

Benchmark Argon2 参数不能只测单次。要测并发 1、并发等于 CPU 核数、并发等于预期峰值、错误密码洪泛、不存在用户、pepper cache miss、数据库慢查询。指标要看 p50/p95/p99、CPU、RSS、OOM、GC、队列等待时间。

结合 LogServe，我会单独给 auth 路径容量规划，不让 Argon2 计算和 worker 执行任务抢同一个资源池。实验系统如果只是本地 demo，可以用较低参数；生产说明里要写清楚参数需要按硬件和登录峰值调优，不能复制示例值。

面试里可以这样回答：

```text
Argon2 的主要瓶颈是 CPU 和内存，尤其是 memory cost 乘以并发数。锁竞争、I/O 和网络多来自外围的用户记录查询、失败计数、rate limit、pepper/KMS 和审计日志。参数要按硬件压测，不能只看单次耗时；要测峰值并发、错误密码洪泛、cache miss 和 OOM 风险。通常需要 Argon2 worker pool 或 semaphore 控制同时计算数。
```

## Q062. Argon2 的 correctness test、stress test 和 benchmark 应该分别测什么？

Correctness test 先用官方或库提供的 test vector，确认同样 password、salt、参数、版本、输出长度得到同样 hash。还要测试 PHC string 解析和生成：algorithm、version、memory、time、parallelism、salt、hash 都能正确保存和读取。

密码验证测试要覆盖正确密码成功、错误密码失败、相同密码不同 salt 得到不同 hash、参数变化后旧 hash 仍能验证、需要 rehash 的判断正确、hash 格式损坏时安全失败。比较 hash 要用常量时间比较或库提供的验证函数，避免普通字符串比较。

边界测试包括空密码、长密码、Unicode 密码、很长 salt、非法参数、过低参数、过高参数、版本不支持、并行度为 0、内存不足。Unicode 特别容易忽略：系统要明确密码按什么 bytes 编码。通常直接用用户输入的 UTF-8 bytes，不要做不一致的 normalization，除非产品规范明确。

Stress test 主要测登录洪泛和资源保护。大量错误密码、同一用户并发登录、不存在用户请求、注册/改密并发、rehash on login 并发、pepper/KMS 超时、数据库慢、服务重启，都要覆盖。目标是确认不会 OOM，不会把失败计数写乱，不会让旧 hash 覆盖新 hash。

Stress 还要测 rate limit 与防枚举。不存在用户响应不能明显比错误密码快太多，否则容易枚举；但也不能让不存在用户无限消耗 Argon2。可以使用 dummy hash 或固定成本策略，再配合限流。这个平衡要通过测试验证。

Benchmark 要测参数选择。记录 memory cost、time cost、parallelism、机器规格、并发度、p50/p95/p99、RSS、CPU、吞吐。不要只报告“单次 120ms”。还要测不同硬件：开发机、CI、生产容器、低配节点结果差距很大。参数应该有目标，比如普通登录 p95 控制在 200-500ms，认证服务在峰值下不 OOM。

结合 LogServe，如果以后加管理登录，我会放一组 auth tests：密码创建、验证、错误、rehash、改密 CAS、pepper missing、并发登录限流。benchmark 不放在普通单元测试里，但要有脚本记录参数和机器。

面试里可以这样回答：

```text
correctness test 用 Argon2 test vector，测 PHC string 编解码、正确/错误密码、不同 salt、旧参数验证、rehash 判断、坏格式安全失败和 Unicode bytes。stress test 测错误密码洪泛、不存在用户、并发改密、rehash 竞争、KMS/pepper 超时、重启和 OOM 保护。benchmark 用不同 memory/time/parallelism 和并发度测 p50/p95/p99、CPU、RSS、吞吐，参数选择要基于生产硬件。
```

## Q063. 如果要求从零实现一个简化版 Argon2，你会先定义哪些不变量？

我会先说明边界：生产系统不应该自己实现 Argon2 算法，应该使用经过审计的库。面试里所谓“简化版”，我会把重点放在密码存储封装和不变量，而不是手写内存填充函数。

第一个不变量是每个密码都有唯一随机 salt。salt 不需要保密，但必须足够随机，不能用 user id、邮箱、时间戳。相同密码在不同用户或同一用户改密后，也应该得到不同 hash。salt 和 hash 一起存储。

第二个不变量是参数自描述。存储字符串必须包含 algorithm、version、memory cost、time cost、parallelism、salt、hash。验证时使用记录里的旧参数；登录成功后如果参数低于当前策略，再 rehash。不能把当前全局参数拿来验证所有历史密码。

第三个不变量是输入 bytes 明确。密码字符串要按固定编码转 bytes，通常 UTF-8。不要在不同平台做不同 normalization；如果产品要求 normalization，注册和登录必须完全一致，并记录策略版本。否则用户会遇到“同样看起来的密码登录失败”。

第四个不变量是比较和错误处理安全。验证结果用常量时间比较或库验证函数；错误返回不要区分“用户不存在”“hash 格式错”“密码错”给外部；内部日志不记录密码、salt+hash 可以记录但也要谨慎，因为 hash 泄露就是离线攻击入口。

第五个不变量是资源上限。简化封装也要拒绝不合理参数，防止数据库里被写入 `m=1TiB` 让登录 OOM。解析 PHC string 后要检查 memory/time/parallelism 在允许范围内。认证服务要有并发上限。

第六个不变量是迁移和轮换。旧算法如 bcrypt/scrypt/PBKDF2 可以验证后升级到 Argon2id；pepper 如果存在，要有 version；改密要原子替换旧 verifier；session 失效策略清楚。

结合 LogServe，我会实现一个 `PasswordVerifier` 抽象，接口是 `HashPassword(password) -> encoded`、`VerifyPassword(password, encoded) -> ok, needsRehash`。内部只调用标准 Argon2id 库，外部看不到参数细节。API token 不走这个接口，避免误用。

面试里可以这样回答：

```text
我不会生产手写 Argon2 算法，只会实现密码存储封装。先定义不变量：每条密码唯一随机 salt；存储格式自描述，包含算法、版本、m/t/p、salt、hash；输入 bytes 编码固定；验证用常量时间比较；外部错误统一；解析参数有上限防 OOM；登录成功可按新参数 rehash；改密原子替换。核心是把低熵密码变成可迁移、可验证、抗离线猜测的 verifier。
```

## Q064. Argon2 的常见误用是什么，误用后通常会产生什么线上症状？

第一种误用是参数太低。开发时为了测试快，把 memory/time cost 设得很小，最后带到生产。线上症状不明显，登录很快，系统也稳定，但数据库泄露后攻击者爆破成本很低。这类问题通常在安全评审或事故后才被发现。

第二种误用是参数太高但没有容量控制。安全上看更强，生产里登录 p99 飙升、认证服务 OOM、错误密码攻击把服务打挂。Argon2 参数必须和 rate limit、并发上限、实例资源一起设计。只讲“越大越安全”是不负责任的。

第三种误用是 salt 错误。所有用户共用 salt、用 user id 当 salt、salt 太短、改密不换 salt。症状是相同密码 hash 相同，方便攻击者聚类和预计算。salt 应该随机生成并随 hash 存储。

第四种误用是只存 hash，不存参数。上线时用一套参数，半年后升级参数，结果旧用户无法验证或无法判断是否需要 rehash。正确做法是 PHC string 或等价自描述格式。

第五种误用是把 pepper 放错位置。pepper 写在代码、环境变量、镜像、同一个数据库，或者日志里打印。pepper 如果和 hash 一起泄露，价值就没了；如果 pepper 丢失，所有密码都无法验证。使用 pepper 要有 KMS、版本和恢复策略。

第六种误用是拿 Argon2 存高频 token。每次 API 请求都跑几十 MiB 内存的 Argon2，攻击者发一批随机 token 就能打爆认证服务。高熵 token 不需要这种成本，存 HMAC verifier 更合理。

第七种误用是把密码先做不稳定处理。比如 trim 空格、大小写转换、Unicode normalization 不一致，导致用户设置的密码和登录时 bytes 不同。症状是部分用户偶发无法登录，尤其是含中文、emoji、组合字符的密码。

结合 LogServe，最容易踩的是把 Argon2 用到 SDK token 或 worker token 验证上。worker 心跳和 task submit 是高频路径，不应该用 password hash；管理用户登录才适合。参数要通过压测确定，而不是复制网上示例。

面试里可以这样回答：

```text
Argon2 常见误用包括参数太低、参数太高但无限并发、salt 共用或可预测、只存 hash 不存参数、pepper 和数据库放一起或无恢复策略、拿 Argon2 验证高频随机 token、密码编码/normalization 不一致。症状可能是安全性静默变弱、登录 p99 飙升、认证服务 OOM、用户偶发无法登录、参数升级困难或 token 接口被 DoS。
```

## Q065. Argon2 在单机和分布式环境中的语义有什么差异？

Argon2 的验证语义在单机和分布式环境中一样：用存储的 salt 和参数对输入密码计算，和存储 hash 比较。差异在资源调度、参数一致性、pepper 分发、rehash 并发和全局防护。

单机环境里，密码表、参数策略、pepper、失败计数都在一个进程或一套数据库附近。排查直接，容量也容易估算。缺点是单点和扩展有限，攻击登录接口时会直接占满这台机器。

分布式环境里，多实例都要使用同一套密码策略，但验证旧 hash 时必须尊重记录里的历史参数。新参数发布要灰度，否则一部分实例认为需要 rehash，一部分实例还按旧策略；这一般不影响验证，但会造成重复 rehash 和写竞争。用 `needsRehash` + CAS 可以处理。

Pepper 分发更复杂。所有 auth 实例要能拿到当前 pepper 和必要旧版本 pepper；KMS/secret manager 抖动会影响登录；pepper cache 要有 TTL 和轮换通知。多区域部署时，还要考虑某一区域是否允许解密/验证所有用户，还是按区域隔离 key。

失败计数和锁定策略必须全局一致。用户连续错误密码可能打到不同实例，如果失败计数只存在本机内存，就绕过锁定。需要集中存储、分片计数或风控系统。rate limit 也要按用户、IP、租户在全局范围内生效，至少不能轻易通过换实例绕过。

资源隔离在分布式里更重要。一个租户或攻击源不能耗尽所有 auth 实例的 Argon2 worker pool。可以按租户做限流，认证计算池和业务请求池分开，必要时给管理后台和用户登录分不同队列。多区域还要防止某一区域被攻击后全局重试风暴。

结合 LogServe，如果只是单机实验，Argon2 可以在控制面本地使用；多节点后，auth 逻辑应集中或明确共享用户表、pepper、失败计数和参数策略。LogServe 的核心 worker/task 路径不应依赖 Argon2，否则扩展后认证成本会拖垮调度。

面试里可以这样回答：

```text
Argon2 算法语义不变，分布式难在参数策略、pepper 分发、失败计数、rehash 并发和资源隔离。每条 hash 要自带旧参数，新策略只影响新 hash 或登录后 rehash；rehash 更新用 CAS；pepper 通过 KMS/secret manager 分发并支持版本；失败计数和限流不能只放本机内存；Argon2 计算池要按租户和实例容量做上限。它不适合放在高频分布式请求路径上。
```
## Q066. JWT 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

JWT 的核心目标是用一种紧凑、URL-safe、JSON-based 的格式携带 claims，并可通过 JWS 签名或 JWE 加密进行保护。它本身不是“登录系统”，也不是“权限系统”。它只是 token 格式。真正的安全来自签名/加密算法、issuer、audience、subject、过期时间、key 管理、撤销策略和服务端授权。

JWT 主要解决的是跨服务传递身份和声明的工程问题：资源服务不一定每次都回认证中心查 session，可以本地验证 token 签名，读取 `sub`、`iss`、`aud`、`exp`、`scope`、`tenant` 等 claims。这样性能和可用性会更好，特别是在微服务和 API gateway 场景。但安全性不是自动获得的，错误使用 JWT 反而很危险。

从性能角度，JWT 的好处是自包含和本地验证。资源服务验证签名后就能做初步鉴权，不需要每次访问数据库或 introspection endpoint。缺点也来自自包含：token 一旦签发，在过期前通常都有效；用户退出、权限变更、账号禁用、密钥泄露时，撤销会变复杂。很多系统为了性能选择较长有效期，安全风险就上升。

从正确性角度，JWT 要求验证者严格检查算法、签名、issuer、audience、expiration、not-before、subject、token type。只验证签名不够。一个给 service A 的 token 不能拿给 service B；一个 ID token 不能当 access token；一个来自 staging issuer 的 token 不能进 production。RFC 8725 重点提醒的就是这些混淆和替换攻击。

从可维护性角度，JWT 有标准 claim、header、JWK/JWKS、kid、库生态，跨语言方便。但这也导致团队容易把业务状态塞进 token：角色、权限、套餐、租户、邮箱、昵称、配置。token 越大，泄露信息越多，权限变更越难及时生效。

结合 LogServe，JWT 可以用于 SDK 调 control、dashboard 用户会话、worker 短期身份，但我会把它当 access token 格式，而不是完整授权模型。control 验证 JWT 后，还要查 tenant、workflow、task、actor、result object 的授权。worker token 要短期、带 audience、scope、worker id、epoch，不允许一个 token 读所有 stream。

面试里可以这样回答：

```text
JWT 的核心目标是用紧凑 JSON token 携带 claims，并通过签名或加密保护。它主要解决跨服务传递身份声明和本地验证的工程问题，带来性能和可用性收益，但安全取决于严格验证。验证 JWT 不能只看签名，还要检查 alg、iss、aud、exp、nbf、sub、token type、scope 和 key。JWT 不等于授权系统，资源级权限仍要服务端判断。
```

## Q067. JWT 的典型适用场景和不适用场景分别是什么？

JWT 适合短生命周期 access token，尤其是资源服务需要本地验证、认证中心不能在每个请求同步参与的场景。API gateway 验证 token，后端服务根据 claims 和本地 policy 做授权，这是常见用法。OAuth2/OIDC 生态里，JWT 也常作为 access token 或 ID token 格式，但两者用途不同，不能混用。

第二个适用场景是服务间短期身份。内部 workload identity 系统可以给服务签发短期 JWT，audience 指向目标服务，scope 限制动作，过期时间很短。目标服务通过 JWKS 缓存公钥本地验证。这样比长期共享 API key 更容易轮换和限制 blast radius。

第三个适用场景是边缘到内部传递认证结果。网关完成用户认证后，给内部服务传一个短期、受众明确、只包含必要 claims 的 token。内部服务不需要理解所有登录方式，但仍能验证网关签名和 token audience。这里要小心：内部服务不能盲信任任意客户端传来的 JWT，必须信任指定 issuer。

不适用场景首先是需要强即时撤销的长会话。如果业务要求用户退出、改密、管理员禁用后立即失效，纯 stateless JWT 会很难。可以用短 access token + refresh token + server-side session/revocation list，或者对高风险操作做 introspection。不要把 30 天有效的 bearer JWT 当万能 session。

第二个不适用场景是存敏感信息。JWS 签名的 JWT 只是 base64url 编码，不加密；任何拿到 token 的人都能读 payload。不要把密码、API key、PII、内部权限细节、商业数据放进去。需要保密时用 JWE 或不要放 token。

第三个不适用场景是大规模动态权限。用户权限频繁变化、策略依赖实时资源状态、ABAC 规则复杂时，把权限快照塞 JWT 会很快过期。资源服务应该根据 token 里的主体和租户，再查最新 policy 或缓存短 TTL policy。

第四个不适用场景是浏览器里不受保护的长期 bearer token。JWT 如果放 localStorage，XSS 后会被直接拿走；放 cookie 也要考虑 HttpOnly、Secure、SameSite、CSRF。JWT 不是防 XSS/CSRF 的机制。

结合 LogServe，SDK access token 可以是短期 JWT，scope 包含 submit/read/cancel，audience 是 control API。dashboard 可以用服务端 session 或短期 JWT + refresh 流程。worker token 短期绑定 worker id 和 epoch。不要把 LLM prompt、result、object storage credential 放进 JWT。

面试里可以这样回答：

```text
JWT 适合短生命周期 access token、服务间身份、网关向后端传递认证结果，前提是 issuer/audience/scope/expiration 明确。它不适合长时间 stateless session、需要即时撤销的高风险场景、承载敏感数据、承载频繁变化的大权限快照，也不能替代浏览器安全。JWS payload 只是编码不是加密，敏感内容不要放进去。
```

## Q068. JWT 和相近概念最容易混淆的边界在哪里？

JWT 最容易和 session、OAuth2、OIDC、access token、ID token、JWS、JWE、cookie、API key 混淆。JWT 是格式，不是协议，也不是会话策略。OAuth2 是授权框架，OIDC 是身份层，JWT 可以被它们用作 token 格式，但不是必然。

JWT 和 session 的边界在状态。传统 session 通常是客户端保存 session id，服务端保存状态；JWT 常被用作自包含 token，服务端本地验证。JWT 可以实现 session，session 也可以不用 JWT。选择 JWT 不等于无状态就一定更好，撤销、权限变更、token 泄露都要处理。

JWT 和 OAuth2 的边界在层次。OAuth2 定义授权流程、角色、token endpoint、scope、refresh token 等；access token 可以是 JWT，也可以是不透明字符串。资源服务如果收到 opaque token，可能需要 introspection；收到 JWT，则可以本地验证。不要说“我们用了 JWT 所以就是 OAuth2”。

JWT 和 OIDC ID token 的边界很重要。ID token 是给客户端证明用户身份的，audience 通常是客户端；access token 是给资源服务器访问 API 的。把 ID token 拿去调 API 是常见错误。资源服务器应该接受 audience 指向自己的 access token，而不是任何签名正确的 token。

JWS 和 JWE 的边界在保密性。JWS 签名保证完整性和来源，payload 可读；JWE 加密才隐藏 payload。大多数人说 JWT 时其实指 signed JWT/JWS。看到三段 `header.payload.signature`，不要以为 payload 被加密。

Cookie 和 JWT 也不是同一层。Cookie 是浏览器存储和自动发送机制，可以装 session id，也可以装 JWT。JWT 放 cookie 仍要处理 CSRF；JWT 放 localStorage 要处理 XSS。API key 则通常是随机 opaque secret，服务端查 verifier 或数据库；JWT 是结构化 claims token。API key 简单但难表达细粒度 claims，JWT 表达力强但撤销复杂。

结合 LogServe，我会在文档里明确：SDK bearer 可以是 opaque token 或 JWT；如果是 JWT，它只是认证材料，授权仍在 control。dashboard login 可以用 session，不必强行 JWT。worker 身份 token 如果用 JWT，必须和 access token 类型区分，避免 cross-JWT confusion。

面试里可以这样回答：

```text
JWT 是 token 格式，不是 OAuth2、OIDC 或 session 本身。OAuth2 access token 可以是 JWT，也可以是 opaque；OIDC ID token 不能当 API access token；JWS 是签名不保密，JWE 才加密；cookie 只是浏览器传 token 的容器；API key 通常是 opaque secret。JWT 的常见错误就是签名正确就信，忽略 token type、issuer、audience 和用途边界。
```

## Q069. JWT 在高并发场景下可能出现哪些隐藏问题？

高并发下，JWT 的优势是本地验签，缺点是密钥、撤销、解析和缓存会成为新的复杂点。很多线上问题不是 JWT 算法慢，而是 JWKS 获取、kid 选择、错误缓存、权限变更延迟和大 token 传播成本。

第一类问题是 JWKS/key cache。资源服务通常从 issuer 的 JWKS endpoint 拉公钥，按 `kid` 选择 key。高并发下如果 cache miss 时所有请求同时拉 JWKS，会形成 thundering herd；如果 issuer 暂时不可用，所有新 kid token 都失败。需要有缓存、singleflight、后台刷新、旧 key grace window、失败降级策略。不要每个请求都远程取 key。

第二类是签名算法成本。HMAC 类 HS256 验证快，但共享密钥分发风险大；RSA 验签相对慢，ECDSA/EdDSA 成本和库实现有关。高 QPS API gateway 上，验签 CPU 会变成可见成本。可以在边缘统一验证后转发内部短期身份，但后端是否还要二次验证取决于信任边界。不要为了性能关闭签名验证。

第三类是 token 过大。JWT 放太多 claims，会增加每个请求 header 大小，影响带宽、代理限制、日志、HTTP/2 header compression 风险和缓存。很多 ingress/proxy 对 header 有大小限制。高并发下，一个 8KB token 每秒 10 万请求就是很实际的网络和 CPU 成本。token 里只放必要 claims。

第四类是撤销和权限变更延迟。高并发服务本地验证 JWT，不查中心状态，性能好；但用户禁用、租户权限变化、key 泄露、token 被盗时，所有资源服务要等 token 过期或拿到撤销状态。短 access token、refresh token、revocation list、session version、user token version、introspection 缓存都可以用，但会引入状态和缓存一致性问题。

第五类是并发下的错误放大。攻击者发送大量伪造 token、未知 kid、巨大 token、嵌套 JSON、压缩或奇怪编码，可能消耗解析、base64、JWKS 查询和日志。验证流程要先限制 token 大小、header/payload JSON 大小、允许算法集合和 issuer，再做昂贵操作。失败日志要采样，不能把完整 token 打出来。

第六类是多服务验证规则不一致。服务 A 检查 audience，服务 B 忘了；网关允许某算法，后端库默认接受另一个；某服务没检查 `typ` 或 token_use。高并发微服务里，这会表现为“同一个 token 在某些 API 能用，在某些 API 不能用”，更严重时是跨服务混淆攻击。最好有统一验证库和配置。

结合 LogServe，control API 可以本地验证短期 JWT，但 JWKS/key cache 要后台刷新；worker token 的 audience 必须是 control，不允许 dashboard token 调 worker API；token payload 不放大对象结果；权限变化通过短 TTL 和 server-side scope/version 检查处理。高并发 task submit 时，JWT 验证不应每次打远程 issuer。

面试里可以这样回答：

```text
JWT 高并发隐藏问题包括 JWKS cache miss 风暴、验签 CPU、token 过大导致 header/网络成本、撤销和权限变更延迟、坏 token 洪泛放大解析和日志成本、多服务验证规则不一致。解决上要限制 token 大小，固定允许算法和 issuer，缓存并后台刷新 JWKS，短 access token，必要时用撤销/version 状态，统一验证库。不要每个请求远程取 key，也不要为了性能跳过 audience/签名验证。
```

## Q070. JWT 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

JWT 自身是 bearer token，服务重启不会改变 token 有效性。真正的边界在 key cache、撤销状态、refresh 流程、时钟、重试幂等和权限版本。

第一个边界是 key cache 冷启动。服务重启后 JWKS cache 为空，如果第一批请求都触发远程拉取，会造成启动风暴；如果 issuer 不可用，新实例可能无法验签。需要在启动时预热 key，或允许使用持久化/上次有效 JWKS，后台刷新并有过期策略。旧 key 不能无限保留，但也不能轮换瞬间让所有旧 token 失效。

第二个边界是时钟。JWT 的 `exp`、`nbf`、`iat` 依赖验证端时间。机器重启后时钟错误、NTP 漂移、容器宿主时间异常，会导致 token 大量“过期”或“尚未生效”。验证端通常允许很小 clock skew，但不能太大。太大相当于延长 token 有效期。

第三个边界是撤销状态丢失。如果系统用内存 blacklist、jti cache、user token version cache，重启后可能忘记已撤销 token。高风险撤销状态要有持久化来源，内存只能是缓存。否则用户退出或管理员禁用后，重启会让旧 token 复活。

第四个边界是 refresh token 重试。客户端刷新 access token 时超时，不知道服务端是否签发了新 refresh token。如果 refresh token rotation 使用不当，重试可能被当成重放攻击，或者旧 refresh token 被错误保留。需要 refresh token family、reuse detection、幂等窗口和明确的客户端行为。

第五个边界是权限变更和长 token。用户权限被撤销后，已经签发的 JWT 仍带旧 scope。服务重启不会帮助。解决方式是短 access token、token version、policy version、resource server 二次检查，或者对高风险 API 做 introspection。不同操作可以有不同要求：读普通状态可以接受几分钟延迟，删除生产数据不能。

第六个边界是业务重试。JWT 验证通过只说明调用者身份成立，不说明请求幂等。客户端 submit task 超时后用同一个 JWT 重试，control 仍需要 idempotency key；worker complete 超时后重试，仍需要 attempt/lease。不要把 JWT 的 `jti` 和业务 request id 混用，除非协议明确。

结合 LogServe，control 重启后要能继续验证 worker/SDK token，JWKS 或 HMAC key 缓存有预热和后台刷新；撤销状态或 token version 在 metadata store；task submit 的幂等由 idempotency key，不由 JWT；worker complete 的幂等由 task attempt 和 lease，不由 JWT。

面试里可以这样回答：

```text
JWT 在重启和重试中的问题主要是 key cache、时钟、撤销状态和 refresh 流程。服务重启后 JWKS cache 为空会触发拉取风暴；时钟漂移会影响 exp/nbf；内存 blacklist 重启会丢撤销；refresh token rotation 遇到超时要处理幂等和重放；权限变化不会让已签发 JWT 自动消失。业务重试仍要 idempotency key、lease、attempt，JWT 只证明身份和声明。
```

## Q071. JWT 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

JWT 性能瓶颈通常不只在验签算法。它可能来自 CPU、内存分配、key/JWKS cache、网络、日志和下游授权查询。要看 token 类型、算法、QPS、claims 大小和验证策略。

CPU 上，HMAC 验证通常便宜，RSA 验签更贵，ECDSA/EdDSA 依库和硬件而定。API gateway 每秒验证几十万 token 时，算法选择会影响核心数。不要为了 CPU 改用 HS256 并把共享 secret 分发给所有服务；这会牺牲密钥边界。更常见的优化是边缘验证、后端使用受控内部身份，或缓存验证结果短时间，但缓存要绑定 token digest 和过期时间。

内存和分配来自 base64url 解码、JSON parse、claims map、字符串转换和大 token。很多库把 claims 解析成动态 map，会有不少分配。高 QPS 服务可以使用固定结构、限制 claim 大小、避免把完整 token 复制到日志或 context。token 太大还会增加 header 处理和 GC 压力。

锁竞争来自 JWKS/key cache、验证结果 cache、撤销列表、metrics。一个全局 key map lock 在高并发下会抖；JWKS refresh 如果没有 singleflight，会被未知 kid 打爆。撤销列表每次查集中 Redis，也可能成为瓶颈。可以用分片缓存、本地 TTL、后台刷新和 negative cache，但要小心撤销延迟。

I/O 和网络主要来自远程 JWKS、introspection、revocation check、policy check。纯 stateless JWT 本地验证最快，但撤销和动态授权弱；每次 introspection 最准确，但网络 p99 高、认证中心压力大。折中方案是短 JWT + 本地验证 + 对高风险操作做状态检查，或者缓存 introspection 结果。

日志也是常见性能和安全问题。坏 token 洪泛时，如果每次都记录完整 token 和 stack trace，日志系统会先爆。应该记录 token fingerprint、issuer、kid、失败分类、request id，完整 token 不进日志，失败采样。

结合 LogServe，task submit 的热路径要避免远程 introspection；短期 JWT 本地验证后，再用本地/metadata cache 做 scope 和 tenant 授权。对于取消 workflow、读取敏感 result、管理 worker 这类高风险 API，可以增加 token version 或中心状态检查。benchmark 要分开测验签、claims parse、JWKS cache、授权 policy、完整 RPC。

面试里可以这样回答：

```text
JWT 性能瓶颈可能来自验签 CPU、JSON/base64 解析分配、token 过大、JWKS/key cache 锁、远程 JWKS/introspection/revocation 网络、授权查询和失败日志。HMAC 快但共享密钥边界差，RSA/ECDSA 成本更高但更适合公钥验证。优化要限制 token 大小，缓存并后台刷新 key，统一验证库，避免每请求远程取 key；高风险操作再做状态检查。完整 token 不进日志。
```

## 参考资料

- IETF RFC 4949, [Internet Security Glossary, Version 2](https://www.rfc-editor.org/rfc/rfc4949.html)
- IETF RFC 2104, [HMAC: Keyed-Hashing for Message Authentication](https://www.rfc-editor.org/rfc/rfc2104.html)
- IETF RFC 6151, [Updated Security Considerations for the MD5 Message-Digest and the HMAC-MD5 Algorithms](https://www.rfc-editor.org/rfc/rfc6151.html)
- IETF RFC 5116, [An Interface and Algorithms for Authenticated Encryption](https://www.rfc-editor.org/rfc/rfc5116.html)
- IETF RFC 8446, [The Transport Layer Security (TLS) Protocol Version 1.3](https://www.rfc-editor.org/rfc/rfc8446.html)
- IETF RFC 6749, [The OAuth 2.0 Authorization Framework](https://www.rfc-editor.org/rfc/rfc6749.html)
- IETF RFC 6750, [The OAuth 2.0 Authorization Framework: Bearer Token Usage](https://www.rfc-editor.org/rfc/rfc6750.html)
- IETF RFC 7009, [OAuth 2.0 Token Revocation](https://www.rfc-editor.org/rfc/rfc7009.html)
- IETF RFC 7662, [OAuth 2.0 Token Introspection](https://www.rfc-editor.org/rfc/rfc7662.html)
- IETF RFC 7519, [JSON Web Token (JWT)](https://www.rfc-editor.org/rfc/rfc7519.html)
- IETF RFC 8725, [JSON Web Token Best Current Practices](https://www.rfc-editor.org/rfc/rfc8725.html)
- IETF RFC 8705, [OAuth 2.0 Mutual-TLS Client Authentication and Certificate-Bound Access Tokens](https://www.rfc-editor.org/rfc/rfc8705.html)
- IETF RFC 8555, [Automatic Certificate Management Environment (ACME)](https://www.rfc-editor.org/rfc/rfc8555.html)
- IETF RFC 5280, [Internet X.509 Public Key Infrastructure Certificate and CRL Profile](https://www.rfc-editor.org/rfc/rfc5280.html)
- IETF RFC 9068, [JSON Web Token (JWT) Profile for OAuth 2.0 Access Tokens](https://www.rfc-editor.org/rfc/rfc9068.html)
- IETF RFC 7914, [The scrypt Password-Based Key Derivation Function](https://www.rfc-editor.org/rfc/rfc7914.html)
- IETF RFC 9106, [Argon2 Memory-Hard Function for Password Hashing and Proof-of-Work Applications](https://www.rfc-editor.org/rfc/rfc9106.html)
- NIST FIPS 180-4, [Secure Hash Standard (SHS)](https://csrc.nist.gov/pubs/fips/180-4/upd1/final)
- NIST FIPS 186-5, [Digital Signature Standard (DSS)](https://csrc.nist.gov/pubs/fips/186-5/final)
- NIST SP 800-107 Rev. 1, [Recommendation for Applications Using Approved Hash Algorithms](https://nvlpubs.nist.gov/nistpubs/Legacy/SP/nistspecialpublication800-107r1.pdf)
- NIST SP 800-131A Rev. 2, [Transitioning the Use of Cryptographic Algorithms and Key Lengths](https://csrc.nist.gov/pubs/sp/800/131/a/r2/final)
- NIST SP 800-63B, [Authentication and Authenticator Management](https://pages.nist.gov/800-63-4/sp800-63b.html)
- NIST SP 800-38D, [Recommendation for Block Cipher Modes of Operation: Galois/Counter Mode (GCM) and GMAC](https://csrc.nist.gov/pubs/sp/800/38/d/final)
- NIST SP 800-57 Part 1 Rev. 5, [Recommendation for Key Management: Part 1 - General](https://csrc.nist.gov/pubs/sp/800/57/pt1/r5/final)
- NIST SP 800-162, [Guide to Attribute Based Access Control (ABAC) Definition and Considerations](https://csrc.nist.gov/pubs/sp/800/162/upd2/final)
- NIST CSRC, [Role Based Access Control](https://csrc.nist.gov/projects/role-based-access-control)
- NIST SP 800-207, [Zero Trust Architecture](https://csrc.nist.gov/pubs/sp/800/207/final)
- SPIFFE, [SPIFFE Overview](https://spiffe.io/docs/latest/spiffe-about/overview/)
- SPIFFE, [SPIFFE Concepts](https://spiffe.io/docs/latest/spiffe-about/spiffe-concepts/)
- OWASP Cheat Sheet Series, [Authorization Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html)
- OWASP Cheat Sheet Series, [Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
- OWASP Cheat Sheet Series, [Secrets Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html)
- OWASP Cheat Sheet Series, [JSON Web Token for Java Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/JSON_Web_Token_for_Java_Cheat_Sheet.html)
- OWASP Cheat Sheet Series, [Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)
- Niels Provos and David Mazières, [A Future-Adaptable Password Scheme](https://www.usenix.org/legacy/events/usenix99/provos/provos.pdf)
