# 33. length prefix、protobuf、canonical JSON 与 fsync 追问链

这组问题围绕四个容易被说成“我懂”的基础点：length prefix、protobuf、canonical JSON、fsync。它们看上去分别属于网络、序列化、签名和文件系统，但面试追问经常落在同一件事上：你有没有把边界、不变量、异常路径和指标说清楚。

## Q001. 面试官如果只问一个问题检验你是否理解 length prefix，可能会问什么？

我会问：TCP 连接上连续发两条 protobuf 消息，接收端为什么不能按 `read()` 次数切消息？你会怎样设计 length prefix parser，让它处理半包、粘包、超大 length 和连接超时？

这个问题比“length prefix 是什么”更能测出经验。TCP 给的是字节流，不给消息边界。发送端写一次，不代表接收端读一次；接收端一次可能读到半个 length，也可能读到一条半消息。length prefix 的作用是让接收端先读出长度，再按状态机凑齐 payload，凑不齐就继续等，凑多了就把剩余 bytes 留给下一帧。

好的回答会先定义 frame 格式。比如：固定 4 字节 big-endian length，表示 payload bytes 长度；length 不包含自己；后面跟 payload；如果还有 flags、checksum、compression，这些字段在 header 中有固定位置。gRPC over HTTP/2 的消息层就是一个典型例子：每条消息前面有 1 字节压缩标志和 4 字节大端 message length，DATA frame 边界和 gRPC message 边界没有必然关系。

然后讲状态机。读取阶段至少有两个状态：`read_length` 和 `read_payload`。`read_length` 阶段只能用小 buffer，直到凑满 length 字段；解析 length 后立刻检查 `max_frame_size`、整数溢出、负值或 varint 超长；通过后再进入 `read_payload`。payload 没凑满不能交给上层，连接关闭时要返回 truncated frame，而不是把半条消息当完整消息。

再讲资源保护。length 来自对端，不可信。不能一看到 2GB length 就分配 2GB buffer；不能让对端发完 length 后慢慢拖着 payload，占住内存和连接；不能让大量连接都卡在半条 frame 上。要有最大长度、读超时、连接级和进程级内存预算、backpressure、限流和错误指标。

结合 LogServe，如果 SDK gRPC 或内部二进制 log record 用 length prefix，我会把它看成 framing，不是业务提交语义。它说明这条 frame 到哪里结束，不说明 payload 没坏，不说明任务可以提交，不说明消息幂等。日志 record 还需要 checksum/CRC、commit marker、LSN 和恢复规则；RPC 请求还需要 deadline、request id、认证和幂等键。

## Q002. length prefix 的一句话定义是否容易误导，误导点在哪里？

一句话说“length prefix 就是在消息前面放一个长度字段”，容易让人以为这个字段只要写出来就完事了。真正的问题都藏在“长度是谁的长度、这个长度可信不可信、长度读坏了怎么办”。

第一个误导是没有定义覆盖范围。length 到底表示 payload 长度、header 后剩余长度，还是整个 frame 长度？是否包含压缩后的 bytes？是否包含 checksum？是否包含 padding？一个系统里如果不同语言 SDK 对这个问题理解不同，表现就是偶发解码错位。

第二个误导是把 length prefix 当安全边界。length 是输入的一部分，攻击者可以填任意值。它不能防篡改，不能防恶意大包，也不能证明 payload 完整。它只能告诉 parser 应该尝试读多少 bytes。真正的保护来自最大长度、超时、配额、checksum/MAC 和协议错误处理。

第三个误导是忽略流式场景。length prefix 很适合“我知道一条消息总长度”的场景。对实时流、无限日志 tail、未知长度上传，单个完整 length prefix 可能不合适。更常见的是 chunked framing，每个 chunk 有自己的长度和状态，或者交给 HTTP/2 / QUIC 这类传输层做流控制。

第四个误导是把 varint 和 length prefix 混成一层。varint 是整数编码方式，length prefix 是 framing 语义。length 可以用固定 4 字节，也可以用 varint。用了 varint 不代表协议有完整的 frame 状态机；固定 4 字节也不代表协议粗糙。关键是边界和异常处理。

更准确的一句话是：length prefix 是一种在字节流上标出下一段消息长度的 framing 机制；它必须和最大长度、状态机、超时、错误处理、checksum/认证和业务幂等一起设计，不能单独承担可靠性或安全性。

## Q003. length prefix 最常见的生产事故触发条件是什么？

最常见的是 parser 过早相信 length。对端发一个很大的长度，服务端马上分配 buffer，结果 OOM 或 GC 打满；对端发合法长度但慢慢发送 payload，连接堆积成 slowloris；对端发半个 length 后断开，状态机把残留 bytes 错当下一帧，后面全错位。

第二类事故是整数和边界处理。4 字节无符号长度被读进有符号 `int32`，超过 `2GB` 后变负；32 位进程里 `header_len + payload_len` 溢出；varint length 没限制最大字节数，对端一直发 continuation bit；`length == 0`、最大值、刚好超过上限这些边界没有测。线上一旦遇到坏输入，parser 不是拒绝，而是进入奇怪状态。

第三类是覆盖范围变更。某个版本把 length 从“payload 长度”改成“frame 剩余长度”，或者新增 compression flag 后 length 表示压缩后大小，旧客户端仍按解压后大小理解。协议没 version，错误就会表现成随机 EOF、decode error、checksum mismatch 或下游 protobuf 解析失败。

第四类是读循环写错。很多新人会写 `read(conn, lenBuf)` 后直接解析，忘了网络 read 不保证读满。文件读取也一样，尤其是从管道、socket、TLS stream、HTTP/2 DATA stream 里读 frame。正确实现要 `readFull` 或显式状态机。

第五类是异常路径缺指标。所有 frame error 都打成一条“decode failed”，没有区分 truncated length、length too large、timeout waiting payload、checksum mismatch、schema decode failed。排障时只能抓包，无法从指标判断是攻击流量、网络断开，还是版本升级不兼容。

LogServe 的 shared log 如果采用 `length + payload + crc`，恢复时最危险的是最后一条半写 record。正确做法是识别出“length 未写完”“payload 未写完”“CRC 不通过”，停止在最后一个完整 record，而不是继续扫描猜下一条。length prefix 是恢复入口，不是恢复证明。

## Q004. length prefix 的指标应该怎么设计才不会只看平均值？

length prefix 的指标要围绕 frame 状态机和资源占用设计。只看平均 frame decode latency 基本没用，因为正常路径太快，事故都在坏 length、慢 payload、大包和半包上。

我会先记录 frame size 分布：`frame_size_bytes` 的 p50、p95、p99、max，按 message type、tenant、method、producer version 分桶。平均大小很容易骗人，一个 200MB frame 足够拖垮 worker，但平均值可能仍然很低。

第二类是 parser 状态指标：当前处于 `read_length`、`read_payload` 的连接数；半成品 frame 持有的 bytes；等待 payload 的最长时间；每连接 pending frame 数；总 pending frame bytes。slowloris 和 backpressure 问题会先体现在这些状态上。

第三类是错误分类：`length_too_large_total`、`length_varint_too_long_total`、`length_overflow_total`、`truncated_length_total`、`truncated_payload_total`、`payload_timeout_total`、`frame_checksum_mismatch_total`、`decode_after_frame_failed_total`。这些计数要按对端、版本、入口和 frame type 维度打标签，但标签不能爆炸。

第四类是资源指标。解析前分配了多少内存，payload 读取复制了几次，buffer pool 命中率，拒绝超大 frame 节省了多少 bytes，连接因 frame 违规关闭了多少。高并发下，length prefix 的风险不是 CPU，而是内存、连接和队列占用。

第五类是端到端影响。frame 解析 p99 是否和业务请求 p99 同步升高；大 frame 是否占住 worker；是否出现 head-of-line blocking；超限 frame 是否触发重试风暴。指标要能把 framing 问题和业务问题接起来。

面试里可以这样说：length prefix 指标不只看解码耗时，要看 frame size 分布、半成品 frame、超限和截断错误、payload 等待时间、内存占用和关闭连接原因。平均值会把真正危险的长尾藏起来。

## Q005. length prefix 的正确性边界和性能边界分别是什么？

length prefix 的正确性边界是 frame 定义一致，并且 parser 不信任 length。定义一致包括字节序、长度字段宽度、是否 varint、长度覆盖范围、压缩前后语义、header 版本、checksum 覆盖范围。parser 不信任 length，意味着解析后先检查上限和溢出，再决定读取和分配。

它只能保证消息边界。它不保证 payload 能被 protobuf 或 JSON 正确解析，不保证 payload 没被篡改，不保证请求没有重复，不保证业务可以提交。很多事故来自把 frame 完整误认为业务完整。frame 完整只是“这一段 bytes 到齐了”，业务层仍要做 schema validation、checksum、认证、幂等和状态机检查。

性能边界一般不在 length 字段本身。解析 4 字节或几个 varint byte 很便宜。真正成本在 payload 读取、buffer 分配、内存复制、TLS/HTTP2 解帧、protobuf/JSON 解析、压缩解压、日志和下游处理。优化 length prefix 时，不要盯着那几个字节，而要减少不必要拷贝，限制大包，做 buffer 复用和 backpressure。

还有一个边界是流控。知道 length 后，系统可以做预检查和预留预算，但不能让每个连接都独占完整 buffer。大消息应该考虑流式读取、chunking、临时文件、对象存储引用，或者在协议层直接拒绝。把所有 payload 一次性读进内存，是 length prefix 最常见的性能陷阱。

结合 LogServe，普通 task payload 可以 length-prefixed；大 result 应该走 result reference 或对象存储，不该塞进单个 frame。shared log record 的 length prefix 应该服务恢复扫描和边界定位，不能替代 CRC、fsync 策略和 durable offset。
## Q006. 面试官如果只问一个问题检验你是否理解 protobuf，可能会问什么？

我会问：一个已经上线的 protobuf 字段删掉以后，为什么不能把 field number 分给新字段？如果旧日志里还有老消息，新服务会怎样解析？

这个问题能直接测出你是否理解 protobuf 的核心身份不是字段名，而是 field number 和 wire type。binary protobuf 写进消息的是字段号、wire type 和 payload，不写字段名。字段名改了，二进制解析不一定受影响；字段号复用了，同一段历史 bytes 就可能被解释成新的业务含义。

举个简单例子。老版本里 `field 7` 是 `email`，新版本删了它，后来有人把 `field 7` 复用成 `is_admin` 或 `quota_level`。历史消息、队列积压消息、备份文件、旧客户端请求重新进入系统时，新代码可能把旧字段读成新字段。wire type 不兼容时可能跳过或报错；wire type 兼容时更危险，因为它可能“正常解析”，但业务含义错了。

所以删除字段时要 `reserved` 旧 field number，必要时也 reserve 旧字段名。protobuf 官方 best practices 明确说不要复用 tag number，删除字段后 reserve。这个规则不是洁癖，它是在保护历史数据、滚动升级、回滚和日志重放。

这个问题还会引出 unknown fields。旧 reader 看到新字段，可以跳过或保留 unknown fields，这让滚动升级有缓冲。但 unknown fields 不是业务扩展系统。旧业务不理解新语义，只是尽量不破坏它。转成 ProtoJSON、手写对象拷贝、日志脱敏、只复制已知字段，都可能把 unknown fields 洗掉。

结合 LogServe，如果 workflow event、actor command、task completion 用 protobuf 作为 payload，field number 就是长期协议契约。shared log 里的历史事件可能多年后被 replay，不能因为当前代码“没人用这个字段”就复用编号。面试里把“上线后字段号不可回收”讲清楚，比背 protobuf 体积小更重要。

## Q007. protobuf 的一句话定义是否容易误导，误导点在哪里？

常见定义是“protobuf 是 Google 的高效二进制序列化格式”。这句话没错，但太容易把 protobuf 说成“更快的 JSON”。真正重要的是 schema-first、field number、wire compatibility 和生成代码。

第一个误导是把 protobuf 当 JSON 压缩版。JSON 里字段名在 payload 中，没 schema 也能大致读；protobuf binary 里只有字段号和 wire type，没有 `.proto` 或 descriptor，基本看不出业务含义。protobuf 的优势不是把 JSON 变小，而是用稳定 schema 和字段号管理长期协议。

第二个误导是把 protobuf 当自描述格式。单条 protobuf bytes 通常不携带完整 schema。长期存储时，如果只保存 bytes，不保存 message type、schema version、descriptor set 或 schema registry 引用，未来可能读不懂。对日志、对象存储、事件流来说，envelope 里要有 type/version/schema id。

第三个误导是把 protobuf 当 canonical bytes。官方文档明确提醒，protobuf serialization 不是 canonical；deterministic serialization 也不能保证跨语言、跨版本、跨构建稳定。map 顺序、unknown fields、字段重复表示、runtime 版本都可能影响 bytes。直接 `hash(proto.Marshal(msg))` 当幂等 key、签名输入或长期 fingerprint，很脆。

第四个误导是把 protobuf 当安全机制。二进制不可读不等于加密。没有 TLS、HMAC、AEAD 或签名，protobuf bytes 可以被复制、重放、篡改。解析器也可能被超大 message、深嵌套、重复字段、压缩炸弹拖垮。

更准确的一句话是：protobuf 是 schema-first 的跨语言序列化系统，用 field number 和 wire type 编码结构化消息，适合长期演进的内部协议；它不是自描述 JSON，不是 canonical fingerprint 格式，也不是安全边界。

## Q008. protobuf 最常见的生产事故触发条件是什么？

最常见的是 schema 演进破坏兼容。复用 field number、随意改字段类型、删除字段不 reserve、把 repeated 改 scalar、改默认值语义、把 binary 安全的变更误认为 ProtoJSON 也安全。这类事故在灰度、回滚、队列积压和日志 replay 时最容易暴露。

第二类是 presence 误解。proto3 的隐式 scalar 默认值让人分不清“没设置”和“明确设置为默认值”。`retry_enabled=false` 到底是用户关闭了重试，还是旧客户端没有这个字段？如果业务语义需要区分，应该使用 `optional`、oneof、wrapper 或显式状态字段。否则上线后会出现旧客户端请求被新逻辑误判。

第三类是把 protobuf bytes 当 canonical。客户端超时后重试，服务端用 `hash(raw_protobuf_bytes)` 当幂等 key。不同语言 SDK、map 字段顺序、unknown fields、deterministic 选项差异都可能让同一业务请求生成不同 bytes，幂等失效。更稳的是基于业务字段构造 canonical request 或显式 idempotency key。

第四类是热路径重复转换。入口 protobuf 解析成对象，中间为了日志转 ProtoJSON，下游又 marshal，审计再转 JSON，失败时把完整 payload 打日志。单次 protobuf 很快，多层转换后 CPU、分配、GC、PII 风险都上来。

第五类是大消息。protobuf 不等于无限大小。巨大的 repeated 字段、map、bytes、嵌套 message 会造成内存峰值、解析时间和重试成本。尤其是和压缩叠加时，传输层看起来不大，解压后 protobuf 很大，worker 可能被拖垮。

结合 LogServe，Python SDK 和 Go control 之间用 protobuf 时，最需要防的是版本偏移和幂等漂移。`workflow_id + step_id + input_hash` 这种语义 key 比 raw protobuf bytes 更稳；event envelope 要记录 schema version 和 message type；热路径日志不要把完整 ProtoJSON 打出来。

## Q009. protobuf 的指标应该怎么设计才不会只看平均值？

protobuf 指标要把编解码、兼容性、资源和错误语义拆开。只看平均 marshal/unmarshal 耗时，会错过大消息、特定字段、反射路径、ProtoJSON 转换和 schema 版本不兼容。

第一类是 size 分布。记录 encoded bytes、decoded object 大小估算、repeated/map 长度、bytes/string 字段大小、嵌套深度。按 message type、method、producer version、consumer version 分桶。一个 message 平均 2KB 不代表没有 50MB 的异常请求。

第二类是编解码延迟和分配。分别看 binary parse、binary marshal、ProtoJSON parse/marshal、descriptor/dynamic message 路径、generated code 路径、compression 前后。指标要包括 p50/p95/p99、allocs、GC、反射命中次数。protobuf 的优势通常在 generated binary path，动态反射和 JSON path 可能完全不同。

第三类是兼容性错误。unknown field 数量、unknown enum 数量、discard unknown 次数、required/presence 相关错误、schema version mismatch、field number reserved violation、decode error by wire type。上线灰度时，这些指标比平均耗时更早暴露问题。

第四类是转换和日志。多少请求被转成 ProtoJSON，多少失败 payload 被截断，多少日志因为 message 太大被采样，多少 decode error 缺 schema id。很多线上成本不是 protobuf 本身，而是为了可观测性把它转成文本。

第五类是业务后果。protobuf decode fail 后是拒绝请求、进死信队列、触发重试，还是降级跳过字段？大消息是否导致 worker queue 堵塞？schema 不兼容是否集中在某个 SDK 版本？指标要接到重试率、p99、内存峰值和错误码上。

面试里可以这样答：protobuf 指标不要只看平均解析耗时，要看 message size 长尾、字段规模、unknown fields、schema 版本、ProtoJSON 转换、分配/GC、decode error 分类和下游重试。真正的事故常常是兼容性和资源边界，不是单次 varint 解码慢。

## Q010. protobuf 的正确性边界和性能边界分别是什么？

protobuf 的正确性边界是 schema 契约。field number 一旦上线就不能复用；wire-unsafe 类型变更不能灰度混跑；删除字段要 reserve；需要区分缺失和默认值时要显式 presence；长期存储要保存 message type 和 schema version；跨语言要跑 golden test。没有这些，protobuf 只是把错误压成不可读的 bytes。

第二个正确性边界是“binary 兼容”和“业务兼容”不同。旧 reader 能跳过新字段，不代表旧业务理解新语义；新 reader 能读旧字段，不代表默认值符合业务预期。协议变更要写清楚旧客户端、旧服务端、回滚、队列积压、日志 replay 的行为。

第三个边界是 canonical。protobuf binary 适合传输和存储结构化数据，不适合直接做跨版本签名输入或语义 fingerprint。如果要做幂等、签名、缓存 key，应投影到明确字段集合，或者定义专门 canonical form。deterministic serialization 可以减少局部不稳定，但不能把它升级成长期安全承诺。

性能边界主要来自消息规模、对象分配、反射、转换和 I/O。小消息 generated code 路径很快；大 repeated/map、深嵌套、巨型 bytes 会推高内存和 GC；动态 descriptor 解析比生成代码慢；ProtoJSON 和压缩可能比 binary 编解码贵得多。

优化时先减少转换次数，再限制消息大小和嵌套深度，最后再考虑对象池、arena、zero-copy。对象池如果 reset 不干净，会把上个请求字段带到下个请求；zero-copy 如果让 buffer 生命周期变复杂，可能引入更隐蔽的 bug。性能优化不能破坏 schema 不变量。

结合 LogServe，protobuf 适合 SDK/control 的结构化 RPC 和内部事件 payload，但大 result 不该塞进 protobuf 消息里；应走 result reference。对需要 replay 的 shared log event，要把 schema version、event type 和关键业务 id 放在 envelope 中，避免未来只剩一串不可解释 bytes。
## Q011. 面试官如果只问一个问题检验你是否理解 canonical JSON，可能会问什么？

我会问：两个 JSON 文本字段顺序不同、空白不同、数字写法不同，但业务上表示同一个对象。你要对它签名，签名前到底应该签哪串 bytes？

这个问题能把 canonical JSON 的核心逼出来：签名、哈希、幂等 key、内容寻址都只认识 bytes，不认识“看起来一样的 JSON 对象”。普通 JSON 允许 object key 顺序不同、空白不同、字符串转义不同、数字格式不同。canonical JSON 的目标是把同一个 JSON 值收敛成唯一字节序列。

回答时要先说规则来源。RFC 8785 的 JSON Canonicalization Scheme 规定了若干关键点：不输出 token 间空白；字符串和数字按确定规则序列化；object 属性按规则排序；输出 UTF-8；字符串数据不做 Unicode normalization，而是保留原始 code points。它不是“把 key 排序一下”这么简单。

然后讲输入边界。canonicalizer 应该先 parse JSON，并按协议拒绝或处理重复 object key、非法数字、NaN/Infinity、超出安全数值模型的输入。否则不同 parser 对同一段 JSON 的理解可能不一致。签名前必须先确定输入是协议允许的 JSON 值。

再讲 schema 边界。canonical JSON 只解决“JSON 值到 bytes”的确定性，不告诉你某个字符串是不是时间、某个 base64 文本是不是 bytes、某个数字是不是 int64。业务类型仍然要靠 schema 或协议定义。签名时也要决定只签 body，还是把 method、path、tenant、timestamp、nonce 一起纳入签名域。

结合 LogServe，如果 SDK 请求要用 JSON 做 HMAC 输入，我不会直接签“客户端原始 JSON 字符串”。我会先按规范 canonicalize，或者更稳地定义 canonical request：method、path、tenant、body_digest、timestamp、nonce、idempotency key。canonical JSON 解决 body 表示稳定，不替代防重放和授权。

## Q012. canonical JSON 的一句话定义是否容易误导，误导点在哪里？

常见定义是“canonical JSON 就是把 JSON 的 key 排序”。这句话很误导，因为 key 排序只是其中一部分，而且不是最危险的部分。

第一个误导是忽略数字。JSON 数字没有固定文本形式，`1`、`1.0`、`1e0` 在某些业务里可能等价，但作为 bytes 完全不同。RFC 8785 采用确定的数字序列化规则，并受 I-JSON 数值模型约束。自己写 canonical JSON 时，如果数字规则没定义，跨语言签名会很快出问题。

第二个误导是忽略字符串。转义形式不同也会改变 bytes；Unicode normalization 更麻烦。JCS 不会把视觉上相似但 code points 不同的字符串强行归一。业务如果希望对用户输入做 NFC normalization，要在 canonical JSON 之前定义，而不是指望 canonicalizer 自动处理。

第三个误导是把 canonicalization 当 schema validation。canonical JSON 不会判断字段是否缺失、类型是否符合业务、字符串是不是合法时间、数组长度是否超限。它只把允许的 JSON 值转成稳定 bytes。业务校验仍然单独做。

第四个误导是把 canonical JSON 当安全协议。它能提供稳定签名输入，但不负责密钥、签名算法、防重放、权限和错误处理。把 body canonicalize 后做 SHA-256，只是得到了稳定 digest；要认证来源还要 HMAC 或签名。

更准确的一句话是：canonical JSON 是一组把 JSON 值确定性序列化为 UTF-8 bytes 的规则，用来支撑哈希、签名、fingerprint 和跨语言比较；它不是单纯 key 排序，不是 schema validation，也不是安全协议本身。

## Q013. canonical JSON 最常见的生产事故触发条件是什么？

最常见的是各端“自定义 canonical”但规则不一样。Go 端排序 map key 后用默认 JSON encoder，Python 端用另一个浮点格式，JavaScript 端按自己的 `JSON.stringify` 行为处理字符串和数字。小样例都能过，一遇到浮点、Unicode、转义字符、空对象、嵌套数组，就开始签名失败。

第二类是重复 key。JSON 文本里可以出现重复属性名，不同 parser 的处理方式可能不同：保留第一个、保留最后一个、报错。签名系统如果不拒绝重复 key，攻击者可能让签名方和验签方看到不同语义。canonical JSON 输入层必须把这个边界说清楚。

第三类是数字范围。很多 JSON 库把数字读成 double，超过安全整数范围后会丢精度。签名前后如果一个端保留大整数文本，另一个端转成 double 再输出，digest 就变了，甚至业务值也变了。需要大整数时，应在 schema 中用 string 或固定编码表示。

第四类是把 canonical JSON 用在热路径大 payload 上。每次请求 parse、递归排序、重新序列化、再 hash，宽对象和深对象会带来 CPU 与内存长尾。系统一开始为了签名稳定引入 canonicalization，后来把所有请求都走这条路径，p99 被排序和分配拖高。

第五类是签名域不完整。body canonicalize 了，但 method、path、query、tenant、timestamp 没签。攻击者不能改 body，却能把 body 放到另一个上下文里重放。canonical JSON 只管 body bytes，协议签名域要另外定义。

排查时我会先保存签名前 canonical bytes，而不是只保存原始 JSON。对比双方 canonical bytes、数字输出、字符串转义、key 排序、重复 key 策略和 Unicode code points。没有这些证据，签名失败很难靠肉眼看原始 JSON 排出来。

## Q014. canonical JSON 的指标应该怎么设计才不会只看平均值？

canonical JSON 指标要看输入形状和失败原因，不能只看平均 canonicalize 耗时。真正拖垮系统的通常是宽对象、深对象、大数组、数字/Unicode 边界和签名失败重试。

我会记录 `canonicalize_seconds` 的 p50、p95、p99、max，并按 payload size、object key 数、最大嵌套深度、数组元素数、字段类型分桶。排序成本和 key 数有关，深度会影响递归和栈，payload bytes 只是粗略指标。

第二类是失败分类：重复 key、非法 JSON、NaN/Infinity、数字超范围、Unicode 错误、unsupported type、schema validation failed、signature mismatch after canonicalization。签名失败要区分“canonical bytes 不一致”和“密钥/tag 错误”，否则安全和兼容问题会混在一起。

第三类是输出指标。canonical bytes 大小、hash 输入 bytes、body digest 计算耗时、canonical bytes 缓存命中率。对同一个 body 重复签名时，可以缓存 digest，但缓存要绑定原始 bytes、schema version 和 canonicalization version。

第四类是版本指标。多少请求使用 JCS，多少使用旧的自定义排序，多少 SDK 版本还在旧规则，多少验签失败来自旧规则。canonicalization 一旦跨语言，就要把规则版本当协议版本看。

结合 LogServe，如果 SDK 请求、manifest 或 result metadata 使用 canonical JSON，我会监控 canonicalize p99、payload shape、签名失败原因、SDK version 分布和 body_digest mismatch。平均耗时只能说明正常样本，不能说明最危险的边界样本。

## Q015. canonical JSON 的正确性边界和性能边界分别是什么？

canonical JSON 的正确性边界是：双方使用同一套规范，同一套输入限制，同一套 schema 前置规则，并签同一份 canonical bytes。只要其中一个环节不一致，签名和 fingerprint 就会漂移。

它不解决 JSON 之外的问题。字段类型、必填项、时间格式、bytes 表示、业务默认值、权限、防重放，都不属于 canonical JSON 本身。它也不自动把不同 Unicode 表示归一，不自动保护低熵字段，不自动证明来源。安全协议要在它外面加 HMAC、签名、timestamp、nonce、key id 和错误处理。

性能边界主要来自 parse、排序、重新序列化和内存分配。对象越宽，排序越贵；对象越深，递归和临时结构越多；payload 越大，I/O 和拷贝越明显。它通常不是数据面高频大 payload 的最佳默认格式。

优化要从使用边界入手。只对需要签名、幂等、审计、fingerprint 的小型控制面对象 canonicalize；大对象签 digest 或 manifest；重复请求缓存 body digest；避免在日志、指标、重试路径反复 canonicalize 同一对象。不要为了性能改规则，否则所有旧签名都会失效。

面试里可以这样回答：canonical JSON 的正确性建立在固定规范和固定输入域上，性能成本来自把自由 JSON 变成确定 bytes 的过程。它适合控制面和签名面，不适合无脑铺到所有大流量数据面。
## Q016. 面试官如果只问一个问题检验你是否理解 fsync，可能会问什么？

我会问：你写一个新文件 `tmp`，写完后 `fsync(tmp)`，再 `rename(tmp, final)`。机器随后断电。重启后 `final` 一定存在且内容正确吗？如果不一定，还缺哪一步？

这个问题能测出你是否区分文件内容和目录项。`fsync(file_fd)` 主要同步文件数据和恢复读取该文件所需的元数据，比如 size、block mapping。新文件名、rename 结果、unlink 这类名字到 inode 的映射属于目录。只 fsync 文件，不一定保证父目录项持久化。

更稳的发布协议通常是：写 temp 文件，检查 write 返回；`fsync(temp_fd)`；`rename(temp, final)`；打开父目录并 `fsync(dir_fd)`。第一步同步内容，rename 提供运行时原子替换，目录 fsync 让名字映射在崩溃后也更可靠。跨目录 rename 时，源目录和目标目录都可能要考虑。

这个问题还会引出 `write()` 和 `fsync()` 的区别。`write()` 返回成功通常只是内核接受了数据，可能还在 page cache。错误也可能延迟到后续 `fsync()` 或 `close()` 才出现，比如 ENOSPC、EDQUOT、EIO。调用了 `fsync()` 也必须检查返回值，失败不能继续向上层 ack 成功。

还要讲设备层。`fsync()` 的语义要穿过文件系统、journal、块层、设备 cache。底层如果有易失 write cache，需要 flush/FUA 或等价机制。设备或虚拟化层如果虚假报告 flush 完成，应用层无法靠多调一次 `fsync()` 修复。

结合 LogServe，shared log 的 `always/batch/interval fsync` 真正影响的是 ack 语义：是写进 page cache 就返回，还是 batch fsync 成功后返回。面试里要说清楚“成功返回”对应哪个持久化点。否则吞吐数字再漂亮，也不知道崩溃后会丢什么。

## Q017. fsync 的一句话定义是否容易误导，误导点在哪里？

常见定义是“fsync 把文件刷到磁盘”。这句话太粗，误导点很多。

第一个误导是“文件”不等于“文件名”。`fsync(file_fd)` 处理文件内容和文件自身必要元数据，不一定同步父目录项。新建、rename、unlink 的持久化边界通常要看目录 fsync。很多配置文件、manifest、checkpoint 发布事故就出在这里。

第二个误导是“磁盘”不等于稳定介质。数据可能经过 page cache、文件系统 journal、块层队列、设备缓存、RAID 控制器、虚拟化层、云盘服务。`fsync()` 请求的是持久化边界，但最终是否真的落到非易失介质，依赖整条栈正确处理 flush 和错误。

第三个误导是“刷盘成功”等于“事务成功”。`fsync()` 不知道应用 record 边界，不知道一条 WAL 是否完整，不知道 protobuf payload 是否可解析，不知道 checksum 是否通过。应用层要有 length、CRC、commit marker、LSN、manifest 或恢复协议。

第四个误导是以为 `fsync()` 只同步本次 write。实际同步范围和文件、文件系统实现、脏页、元数据事务有关。一个小文件 fsync 可能等待 journal commit；一个 fdatasync 在文件扩展时也可能需要同步 size 和 block mapping。它不是简单的“把这 4KB 立刻写下去”。

更准确的一句话是：`fsync(fd)` 要求操作系统把该文件到某个点为止的脏数据和必要元数据推进到稳定存储，并等待结果；它不自动同步父目录项，不提供应用级事务，也依赖底层存储正确实现持久化语义。

## Q018. fsync 最常见的生产事故触发条件是什么？

最常见的是 ack 时机和 fsync 策略说不清。系统写 WAL 后先返回成功，后台每 1 秒 fsync。压测吞吐很好，断电后发现最近一秒“已经成功”的请求没了。问题不在 interval fsync 本身，而在系统把 page cache 接受误报成 durable commit。

第二类是忘记 fsync 目录。写 temp 文件、fsync 文件、rename 成功，进程以为发布完成；断电后 final 文件名没出现，或者仍指向旧文件。这个事故在 manifest、配置文件、SSTable、checkpoint、segment rollover 中很常见。

第三类是忽略 fsync 错误。磁盘满、配额满、I/O 错误可能在 `fsync()` 才返回。代码如果只检查 write，不检查 fsync，或者 fsync 失败后仍推进 durable offset，就会把未持久化数据当成已提交数据。

第四类是 fsync storm。每个请求线程都自己写一点、自己 fsync，设备看到大量小同步和 flush，吞吐下降，p99 飙升。更糟的是多个组件同时 checkpoint、compaction、WAL commit，所有前台请求都被后台写回拖住。

第五类是测试环境给了错觉。tmpfs、容器 overlay、开发机 SSD、云盘、ext4/xfs、不同挂载参数、是否有掉电保护，都会影响结果。只在一台机器上跑一次正常测试，证明不了 crash consistency。需要 crash test、满盘测试、I/O error 注入和恢复扫描。

结合 LogServe，shared log benchmark 里 always fsync 慢、batch/interval 快，这个结果符合预期。但面试时不能只报吞吐，要说清楚每种策略的承诺：已 ack 记录是否保证恢复，最多丢哪个窗口，fsync 失败如何广播给等待者，恢复时如何截断坏尾巴。

## Q019. fsync 的指标应该怎么设计才不会只看平均值？

fsync 指标最忌讳只看平均延迟。fsync 的伤害通常在 p99、p999、最大值和错误路径上。平均 2ms 的系统，可能每隔几分钟有一次 800ms flush，把用户请求打穿。

应用层要记录 `fsync_seconds` histogram，至少有 p50、p95、p99、p999、max；按文件类型区分 WAL、manifest、segment、checkpoint、目录 fsync；记录每次 fsync 覆盖的 bytes、record 数、等待者数、durable LSN 推进量。这样才能看出 group commit 是否真的摊薄了成本。

错误指标也必须有：EIO、ENOSPC、EDQUOT、EINTR、timeout、close-time error。fsync 失败后影响了多少 pending ack，是否进入只读模式，是否停止推进 durable offset，都要计数。只记录成功延迟，会把最关键的正确性信号漏掉。

系统层要看 dirty page 和 writeback。包括 dirty bytes、writeback bytes、block device queue depth、await/util、journal commit latency、flush/FUA 次数、checkpoint/compaction 同期事件。fsync p99 升高时，要能判断是设备慢、journal 拥堵、后台 checkpoint，还是应用层锁持有太久。

还要看策略指标。always/batch/interval 模式下，batch size、batch wait time、fsync interval drift、已 ack 未 fsync bytes、未 ack pending bytes、durability lag。对于 interval 策略，最重要的是风险窗口，而不是单次 fsync 平均多快。

结合 LogServe，我会在 dashboard 中展示 append throughput、fsync p99、durable offset lag、batch 覆盖 record 数、fsync error 数、recovery truncated records。面试里可以说：fsync 指标要同时回答“同步慢在哪里”“已承诺但未持久的窗口有多大”“失败后系统有没有停止撒谎”。

## Q020. fsync 的正确性边界和性能边界分别是什么？

fsync 的正确性边界是本地持久化边界，不是分布式提交边界。单机上，`fsync()` 成功通常意味着该文件相关数据和必要元数据已经被推进到稳定存储，前提是文件系统、块层和设备守约。它不代表副本已经同步，不代表 quorum commit，不代表远端对象存储已经持久化。

第二个边界是对象范围。文件 fsync 不等于目录 fsync；fdatasync 不等于 fsync；msync 不等于完整文件发布；rename 原子性不等于持久性。应用要按自己的发布协议选择同步对象：WAL 文件、manifest 文件、父目录、segment 文件、索引文件，不能只看到一个 fd 就调用 fsync 结束。

第三个边界是应用事务。fsync 不知道一条 record 是否完整，也不知道多文件更新是否原子。要靠 WAL、checksum、length prefix、commit marker、manifest、recovery scanner 把“哪些 bytes 属于已提交状态”定义出来。fsync 只是把某些 bytes 更可靠地留到崩溃后。

性能边界来自同步持久化的物理成本。fsync 要等待脏页写回、元数据提交、journal commit、设备 flush，任何一层都可能慢。频繁小 fsync 会破坏合并，增加 flush 次数；后台写回和 checkpoint 会制造尾延迟；云盘还会受网络和后端复制影响。

优化方式不是简单删 fsync，而是改变协议：group commit、批量 fsync 后 ack、预分配 segment、顺序写 WAL、减少目录发布频率、把大对象放到对象存储、把可丢数据和不可丢数据分级。每个优化都要同步修改对外承诺。

LogServe 的边界可以这样讲：当前单机实验用 fsync 策略验证 shared log 的可靠性/吞吐取舍，不宣称多机强持久化。若要生产化，需要把本地 fsync、复制、quorum、故障域、恢复扫描和用户 ack 语义串起来。面试里能把这个边界讲清楚，比简单说“我们调用了 fsync”更有说服力。
