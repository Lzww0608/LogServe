# 14. 序列化、JSON、protobuf、canonicalization 与 fingerprint

这一组问题看起来像是在比较几种格式，其实面试里更容易追问的是确定性：同一份逻辑数据，为什么在不同语言、不同运行时、不同版本里会变成不同字节？如果这些字节又被拿去做 hash、签名、去重 key、schema id 或 fingerprint，差异就会变成真实故障。

可以先抓住这几条主线：

```text
serialization:
  把带有结构和类型语义的数据变成可存储、可传输的表示。
encoding:
  把某一层的值映射成字节或字符，比如 UTF-8、base64、varint、little-endian。
canonicalization:
  给同一份逻辑值规定唯一或尽量稳定的输出形式。
fingerprint:
  对某个明确的字节序列或 canonical form 做短摘要，用来比较、索引、去重或签名。
main risk:
  如果没有先定义 canonical form，就直接 hash 序列化结果，得到的通常不是语义 fingerprint，而是某次实现细节的 fingerprint。
```

这份回答参考了一手规范和官方文档，包括 RFC 8259 JSON、RFC 8785 JSON Canonicalization Scheme、RFC 4648 Base-N Encoding、RFC 3339 时间戳、protobuf 官方 encoding / ProtoJSON / 非 canonical 序列化说明、Apache Avro 规范和 Apache Thrift IDL / protocol 说明。

## Q001. 序列化和编码的区别是什么？

**回答：**

序列化解决的是“把结构化数据变成可传输或可存储的表示”。编码解决的是“某一层的符号或值怎么映射成字节或字符”。编码经常是序列化的一部分，但二者不是同一个层次。

比如有一个对象：

```text
User{
  id: 123,
  name: "张三",
  active: true
}
```

把它变成 JSON：

```json
{"id":123,"name":"张三","active":true}
```

这是序列化。它决定了字段名写不写、布尔值怎么表示、对象用什么结构承载、缺失字段怎么处理。这个 JSON 文本再用 UTF-8 变成网络上的字节，这是编码。UTF-8 不知道 `id` 是用户 id，也不知道 `active` 是业务状态，它只负责把字符映射成字节。

protobuf 里也一样。`.proto` 决定了消息结构、字段号、字段类型、是否 repeated、是否 map。把一条消息写成 protobuf wire format 是序列化；其中整数用 varint，`float` 用 IEEE 754 32 位，小端固定宽度写出，`string` 和 `bytes` 用 length-delimited record，这些是编码细节。

面试里可以这样说：

```text
序列化关注结构和语义边界：哪些字段存在、字段类型是什么、字段顺序是否有意义、版本怎么演进。编码关注具体表示：字符集、字节序、varint、base64、压缩、转义。把 base64 当成序列化是常见误区；base64 只是把 bytes 变成 ASCII 字符串，并没有描述这些 bytes 代表什么对象。
```

这个区别在做 fingerprint 时很关键。你要问清楚自己 hash 的是什么：

```text
hash(业务语义)
hash(canonical JSON bytes)
hash(某个库这一次输出的 JSON 字符串)
hash(protobuf binary bytes)
hash(schema canonical form)
```

这几个不是一回事。最后两个字节序列只要有一点不同，fingerprint 就会不同；但业务语义可能完全相同。

所以设计协议时，我会把层次写清楚：

```text
logical value:
  业务对象，比如 user、event、workflow step。
serialization format:
  JSON、protobuf、Avro、Thrift、自定义 binary record。
value encoding:
  varint、fixed32、fixed64、UTF-8、base64、RFC3339 timestamp。
transport framing:
  length prefix、HTTP body、Kafka message、WAL record。
integrity / identity:
  checksum、hash、signature、fingerprint。
```

如果这些层次混在一起，后面改一个空格、字段顺序或浮点输出格式，都可能把“数据没变”变成“hash 变了”。

## Q002. JSON 的优缺点是什么？

**回答：**

JSON 的优点很直接：简单、文本化、跨语言、容易排查。RFC 8259 把 JSON 定义为轻量的文本数据交换格式，它能表达 object、array、string、number、boolean 和 null。这个集合不复杂，所以几乎所有语言都有成熟解析器，HTTP API、配置文件、日志、控制面请求都很喜欢用它。

它适合这些场景：

```text
human-facing:
  人要能直接看、复制、调试。
public API:
  客户端语言很多，不能要求所有人装同一套 binary codec。
control plane:
  数据量不大，可读性比极致压缩更重要。
logs / audit:
  希望 grep、jq、浏览器、curl 都能直接处理。
schema-light integration:
  双方还在快速演进，不想一开始就绑定强 IDL。
```

但 JSON 的缺点也正来自这些优点。

第一，JSON 本身不是强 schema 格式。它只告诉你这里是 object、array、number 或 string，不告诉你这个 string 是 UUID、timestamp、base64 bytes，还是普通文本。类型约束、必填字段、枚举范围、默认值、版本兼容，都要靠 JSON Schema、OpenAPI、业务文档或代码约定补上。

第二，数字容易出跨语言问题。RFC 8259 允许实现限制可接受数字的范围和精度，并提醒超过 IEEE 754 binary64 能力的数字会造成互操作风险。最典型的是 int64。`9007199254740993` 在一些 JavaScript 路径里会丢精度，因为它超过了 `2^53 - 1` 的精确整数范围。很多 API 因此把 int64、uint64、decimal 金额写成字符串。

第三，object 成员顺序没有业务语义。JSON object 是 name/value 集合，不应该依赖 key 顺序。不同语言的 map、dict、object 输出顺序可能不同。对人来说这只是排版差异；对 hash、签名、fingerprint 来说，这是完全不同的输入字节。

第四，重复 key 很危险。规范建议 object 里的 name 唯一；如果不唯一，不同解析器可能保留最后一个、报错、保留全部，行为不可预期。所以安全协议、签名协议、配置解析都应该拒绝重复 key，而不是让“谁覆盖谁”变成隐含规则。

第五，JSON 没有原生 bytes、date、time、decimal。bytes 通常要 base64；timestamp 通常要约定 RFC 3339 字符串或 epoch number；decimal 要用字符串或 scaled integer。每个约定都要写清楚，否则客户端会各写各的。

第六，体积和 CPU 成本不低。字段名重复出现，数字要做文本转换，字符串要转义，bytes 要 base64。小请求里这点成本不重要；高吞吐日志、存储层、跨数据中心复制、大批量指标上报，就会明显。

面试可以这样收束：

```text
JSON 的强项是开放互操作和可调试性，不是稳定字节表示和强类型。只要数据要拿去签名、去重、做 fingerprint，或者涉及 int64、decimal、bytes、timestamp，我就不会只说“用 JSON”结束，而会继续规定 canonicalization、数字范围、时间格式和二进制字段表示。
```

## Q003. protobuf 的优缺点是什么？

**回答：**

protobuf 的核心优势是：schema 明确、编码紧凑、解析快、跨语言代码生成成熟，schema evolution 的工程规则也比较清楚。

protobuf binary wire format 不写字段名，而写字段号和 wire type。小整数可以用 varint，常用字段可以使用较小字段号来节省空间，字符串、bytes、子消息和 packed repeated 都走 length-delimited 形式。这让 protobuf 比 JSON 更适合高频 RPC、内部服务通信、持久化小消息、移动端弱网络和日志中的结构化 payload。

它的优点可以按工程角度讲：

```text
compact:
  字段号替代字段名，小整数 varint，重复数字可以 packed。
typed:
  .proto 文件定义字段类型，生成代码减少手写解析错误。
compatible:
  新字段通常可以被旧 reader 跳过，旧字段也能被新 reader 读默认值或 unknown fields。
fast:
  二进制解析少了很多字符串扫描和数字文本转换。
tooling:
  protoc、gRPC、反射、lint、breaking-change 检查生态成熟。
```

但 protobuf 也有明显边界。

第一，它不是自描述格式。拿到一段 protobuf bytes，如果没有 `.proto` 或 descriptor，你只能看到字段号和 wire type，很难知道业务含义。抓包排障不如 JSON 直观。

第二，schema 演进必须守纪律。字段号一旦上线就不能随意改；删除字段后要 `reserved` 掉字段号和字段名；不要复用字段号。官方文档说得很直白：复用字段号会让 wire-format 解码变得歧义，轻则解析失败，重则数据损坏或敏感数据泄露。

第三，默认值和 presence 容易误解。proto3 早期的隐式 presence 让 scalar 字段无法区分“没传”和“传了默认值”。比如 bool 字段读到 `false`，可能是调用方明确写了 false，也可能是字段缺失后返回默认值。现在可以用 `optional` 改善，但老 schema 里这类坑很多。

第四，protobuf binary 序列化不是 canonical。官方文档明确提醒：deterministic serialization 也不等于 canonical serialization；字段顺序、unknown fields、库版本、语言实现、构建参数都可能影响输出字节。也就是说，不要默认 `hash(proto.Marshal(msg))` 是一个跨语言、跨版本稳定的语义 fingerprint。

第五，protobuf 的 JSON 映射和 binary wire format 不是同等能力。ProtoJSON 可读性更好，但官方文档也说明它不支持 unknown fields，字段名和枚举名会出现在 JSON 里，因此重命名字段和删除字段更容易成为 breaking change。换句话说，binary protobuf 的演进规则不能原封不动搬到 ProtoJSON。

面试里可以这样说：

```text
protobuf 适合服务间高频结构化通信，尤其是双方可以共享 .proto 并接受代码生成的场景。它比 JSON 更紧凑、更强类型，也更适合长期演进。但 protobuf 不是可读格式，也不是天然 canonical 格式。如果消息要签名、做 fingerprint、当缓存 key，我会单独设计 canonical 表示，或者只 hash 业务上选定的字段集合。
```

一个很实用的判断是：

```text
public debugging API:
  JSON 往往更方便。
internal hot path RPC:
  protobuf 往往更合适。
long-term archival with writer schema:
  Avro 这类格式可能更合适。
cryptographic signing:
  先定义 canonical form，再谈 hash。
```

## Q004. Avro、Thrift、protobuf 的 schema evolution 模型有什么差异？

**回答：**

这三个格式都支持演进，但思路不一样。最短版可以这样记：

```text
protobuf:
  字段号是 wire identity，靠稳定 field number 和 unknown fields 做兼容。
Avro:
  writer schema 和 reader schema 同时参与解析，靠 schema resolution 做兼容。
Thrift:
  字段 id 和 requiredness 很关键，靠稳定 field id、optional/default 约定做软版本演进。
```

protobuf 的演进是字段号中心。wire format 里主要识别的是 field number 和 wire type，而不是字段名。添加新字段通常是安全的，旧程序不认识就跳过或保留为 unknown field；删除字段时要保留字段号，不让后人复用。危险操作包括改字段号、复用字段号、把不兼容类型硬改成同一个字段号、把业务含义完全换掉。

所以 protobuf 的规则很像数据库 schema 的“列 id 不能乱动”：

```proto
message User {
  string name = 1;
  int64 created_at_ms = 2;
  reserved 3;
  reserved "old_status";
}
```

字段名可以影响生成代码和 ProtoJSON，但 binary wire format 真正稳定的是字段号。

Avro 的模型更强调 writer schema。Avro binary data 本身不写字段名和类型信息，所以读取数据必须知道写入时使用的 schema。Avro 规范要求文件或系统保存 writer schema；读取时可以再提供 reader schema，然后按 schema resolution 规则对齐。比如 record 字段按名字匹配，reader 多出来的字段如果有 default 就用 default，writer 多出来而 reader 没有的字段会被忽略；int 可以提升为 long、float 或 double。

这带来一个很不一样的特点：Avro 天生适合数据文件、日志归档、schema registry、数据湖这类场景。你可以保存每批数据的 writer schema，几年后用新的 reader schema 读取旧数据。Avro 还定义了 Parsing Canonical Form 和 schema fingerprint，用来判断两个 schema 对 reader 是否等价，以及用较短 id 标识 schema。

Thrift 更像服务 IDL 和 RPC 体系。Thrift struct 字段有 field id、类型、名字和 requiredness。协议结构里字段携带 field type 和 field id。演进时最重要的是保持 field id 稳定，新字段尽量 optional，谨慎使用 required。Thrift 官方 IDL 文档也提醒，required 字段缺失时读操作应该失败，这会显著限制软版本演进；如果以后想删除 required 字段或改成 optional，旧新版本之间就不兼容。

可以把差异放成一张口述表：

```text
identity:
  protobuf 主要靠 field number。
  Avro record 字段解析时主要靠字段名，且需要 writer schema。
  Thrift 主要靠 field id，requiredness 影响兼容性。

schema location:
  protobuf 通常由程序编译进来，payload 不带完整 schema。
  Avro 要让 reader 能拿到 writer schema，文件或 schema registry 经常一起出现。
  Thrift 通常由客户端和服务端共享 IDL，payload 不带完整 IDL。

unknown / extra fields:
  protobuf binary 可以跳过或保留 unknown fields，但 ProtoJSON 不支持 unknown fields。
  Avro writer 有、reader 没有的字段会被忽略。
  Thrift reader 通常跳过不认识的 field id，但 required 字段缺失会失败。

defaults:
  protobuf default 更多是读取 API 的默认值，presence 要单独注意。
  Avro default 是 reader schema 补旧数据缺失字段的重要机制。
  Thrift default 在不同实现里有历史差异，optional + isset 更稳。
```

面试里不要只说“它们都支持 schema evolution”。更好的说法是：

```text
protobuf 的兼容性建立在 field number 不复用上；Avro 的兼容性建立在 writer schema 和 reader schema resolution 上；Thrift 的兼容性建立在 field id 稳定和 requiredness 克制上。真正落地时，三者都需要 CI 做 breaking-change 检查，因为规范只给规则，不会阻止人乱改语义。
```

## Q005. 为什么 map 的遍历顺序会影响 fingerprint？

**回答：**

因为 fingerprint 最后处理的是字节序列，而 map 的逻辑语义通常不包含顺序。同一个 map，如果序列化时 key 输出顺序不同，字节就不同，hash 也会不同。

最小例子：

```json
{"a":1,"b":2}
```

和：

```json
{"b":2,"a":1}
```

对 JSON 语义来说，这两个 object 通常表示同一组 name/value mapping。但如果直接做 SHA-256：

```text
sha256('{"a":1,"b":2}')
sha256('{"b":2,"a":1}')
```

结果一定不同。fingerprint 不会理解“这两个 JSON object 语义相同”，它只看字节。

很多语言的 map 遍历顺序本来就不承诺稳定。Go 甚至故意让 map iteration 呈现随机化特征，防止程序依赖它。即使某个语言现在看起来按插入顺序输出，也不能把它当成跨语言协议规则。数据库、消息队列、缓存服务、前端 JavaScript、后端 Go、Python 脚本，只要有一处序列化顺序不同，hash 就变了。

protobuf 也有类似问题。protobuf map 在 wire format 上接近 repeated entry，官方 encoding 文档说明 map 序列化时不保证保留顺序。再加上 protobuf 本身字段序列化顺序也不是稳定语义，直接 hash protobuf bytes 做跨版本 fingerprint 很脆。

面试可以这样答：

```text
map 的顺序不属于业务值，但序列化输出必须线性排列。fingerprint 是对线性字节做摘要，所以只要 map key 的输出顺序不固定，同一份逻辑数据就会产生多个 fingerprint。解决办法不是祈祷运行时顺序稳定，而是定义 canonicalization，比如按明确的 key 比较规则递归排序，再输出无歧义的字节。
```

工程上要补几个细节：

```text
key sort rule:
  字符串按什么比较？UTF-8 bytes、Unicode code point、UTF-16 code unit、locale？
numeric key:
  如果 JSON object key 全是 string，数字 key 是按数字排还是按字符串排？
recursive:
  只排顶层不够，嵌套 object / map 也要排。
array:
  array 顺序通常有语义，不能为了稳定 hash 随便排序。
duplicates:
  JSON 重复 key 要拒绝，否则 canonicalization 前就已经有歧义。
```

这也是 canonical JSON、Avro Parsing Canonical Form、schema fingerprint 存在的原因。它们都在回答同一个问题：哪些差异是无关差异，哪些差异会改变可解析语义。

## Q006. canonical JSON 解决什么问题？

**回答：**

canonical JSON 解决的是“同一份 JSON 数据如何得到稳定字节表示”的问题。没有 canonicalization 时，下面这些差异都会影响 hash 或签名：

```text
字段顺序:
  {"a":1,"b":2} vs {"b":2,"a":1}
空白:
  {"a":1} vs { "a" : 1 }
字符串转义:
  "A" vs "\u0041"
数字写法:
  1.0 vs 1 vs 1e0
Unicode:
  原样字符、转义形式、孤立 surrogate、规范化形式。
```

RFC 8785 定义的 JSON Canonicalization Scheme，也就是 JCS，大致做了几件事：

```text
1. 输入限制在 I-JSON 这类更稳的 JSON 子集。
2. 输出 token 之间不写空白。
3. 字符串按 ECMAScript JSON.stringify 兼容规则输出。
4. 数字按 ECMAScript 的确定性规则输出，并要求 JSON number 能表达为 IEEE 754 double。
5. object 属性递归排序，排序基于未转义属性名的 UTF-16 code unit。
6. 最终输出 UTF-8 bytes。
```

所以 canonical JSON 的目标不是“更好看”，而是“可重复”。它让签名方、验签方、缓存方、去重方对同一份 JSON 值拿到同一串 bytes。

一个面试回答可以这样说：

```text
canonical JSON 主要服务 hash、签名、去重和一致性比较。普通 JSON 只规定语法，不规定唯一输出；canonical JSON 补上输出规则：去空白、定字符串转义、定数字格式、定 object key 排序、定 UTF-8 输出。这样 hash 的对象就从“某个库随手打印出来的 JSON 字符串”变成了“规范定义的 JSON 字节”。
```

但它也有边界。

第一，canonical JSON 不会帮你发明 schema。`"123"` 是字符串还是 int64，`"2026-06-16T12:00:00Z"` 是时间还是普通文本，`"YWJj"` 是 base64 bytes 还是普通字符串，都要由上层协议定义。

第二，它不适合所有数字。JCS 要求 JSON number 能表达为 IEEE 754 double。如果你有 int64 id、uint64 offset、任意精度 decimal、金额，最好用字符串或 scaled integer，并把规则写在 schema 里。

第三，它不做 Unicode normalization。也就是说，视觉上很像的两个字符串，如果底层 Unicode code points 不同，canonical JSON 也会保留差异。签名前最好决定：系统到底是保留原始字符串，还是在业务层先做 NFC 等规范化。

第四，它不能修复输入歧义。重复 key、非法 Unicode、超范围数字、业务字段缺失，这些应该在 canonicalization 之前就拒绝。

实际工程里，我会把流程写成：

```text
parse JSON
reject duplicate keys and invalid Unicode
validate schema and numeric ranges
normalize business values if protocol requires
canonicalize to bytes
hash / sign canonical bytes
```

少一步都可能留下绕过空间。

## Q007. 浮点数序列化为什么容易导致跨语言不一致？

**回答：**

浮点数的问题在于：内存里通常是二进制浮点，JSON 里通常是十进制文本，业务上又经常把它当成精确数。三层语义不一致，跨语言就容易出问题。

先看 JSON。JSON number 是十进制语法，允许整数、小数和指数形式，但它没有规定接收方必须用什么内部类型。一个语言可能解析成 binary64，一个语言可能解析成 BigDecimal，一个语言可能对大数保留字符串再延迟处理。同一个输入：

```json
0.1000000000000000055511151231257827021181583404541015625
```

有的系统会保留很多十进制精度，有的系统会四舍五入成最接近的 double。再重新序列化，文本就可能变短、变成指数形式，或者保留不同位数。

RFC 8259 也提醒过，`1E400` 或特别长的圆周率小数这类数字暗示生成方期待接收方有更大范围或更高精度，互操作会出问题。ProtoJSON 因此把 int64、uint64 默认输出成字符串，避免大整数被 JavaScript 风格 binary64 路径吞掉精度。

再看浮点特殊值。标准 JSON 不允许 NaN 和 Infinity。protobuf binary 里的 `float` / `double` 可以承载 IEEE 754 bit pattern；ProtoJSON 则把 `"NaN"`、`"Infinity"`、`"-Infinity"` 作为特殊字符串形式。不同格式对特殊值的态度不同，桥接时很容易不一致。

还有一个经常被忽略的点是 `-0.0`。在数学上 `-0.0 == +0.0` 通常为 true，但 IEEE 754 里它们 bit pattern 不同。protobuf 文档也提到，scalar float/double 设为 `+0` 可能不序列化，而 `-0` 被认为是不同值并会序列化。这种差异如果进入 hash，就会很难排查。

面试可以这样说：

```text
浮点数跨语言不一致，主要不是“大家不会算小数”，而是二进制浮点、十进制文本、特殊值、舍入算法和输出最短表示之间没有天然唯一答案。只要把浮点文本拿去做 fingerprint，就必须指定 canonical number serialization；如果业务需要精确值，比如金额、比例阈值、版本号，我会避免用 float，改用 decimal 字符串或 scaled integer。
```

工程建议很朴素：

```text
money:
  用 decimal 或 cents 这种 scaled integer，不用 float。
score / probability:
  明确精度，比如保留 6 位小数，或者用 fixed-point。
scientific data:
  明确 NaN、Infinity、-0、rounding 和文本输出算法。
hash / signature:
  不要 hash 默认 JSON 输出，先用规范的 canonicalization。
protobuf binary:
  同一个 bit pattern 的编码清楚，但跨版本 canonical fingerprint 仍然不能只靠默认 Serialize。
```

## Q008. 时间戳、时区和精度如何影响序列化一致性？

**回答：**

时间戳最容易混淆三件事：

```text
instant:
  全球时间线上的一个点，比如 UTC epoch nanos。
local date-time:
  某个本地日历时间，比如 2026-06-16 10:00:00，没有唯一 instant。
date:
  只有日期，没有时分秒和时区，比如 2026-06-16。
```

这三种值不能随便互换。比如：

```text
2026-06-16T12:00:00+08:00
2026-06-16T04:00:00Z
```

它们表示同一个 instant，但字符串不同。如果直接对文本做 hash，fingerprint 不同。如果协议说 fingerprint 表示 instant，那就应该先归一化到一种表示，比如 UTC `Z` 加固定小数位，或者 epoch microseconds。

RFC 3339 给了互联网时间戳的常见文本格式：完整日期、`T`、完整时间、可选小数秒和 `Z` 或数值 offset。它还指出，如果都使用同一时区表示、同样的 fractional second 位数，字符串排序才会得到时间顺序。这句话对 canonicalization 很重要：同样是合法 RFC 3339，不等于文本可直接排序，也不等于 hash 稳定。

精度也会影响一致性。一个系统用毫秒：

```text
1718524800123
```

另一个系统用微秒：

```text
1718524800123456
```

第三个系统用纳秒：

```text
1718524800123456789
```

如果没有字段名或 schema 说明单位，整数本身没有办法告诉你它是哪种精度。更麻烦的是截断和四舍五入：`123456789 ns` 转成毫秒，到底是 `123 ms` 还是 `124 ms`？不同语言默认可能不同。

Avro 规范把这个问题拆得很清楚：`timestamp-millis`、`timestamp-micros`、`timestamp-nanos` 表示全球时间线上的 instant；`local-timestamp-*` 表示本地时间，不绑定具体时区。protobuf 的 `Timestamp` 在 ProtoJSON 里用 RFC 3339 字符串，生成时总是 Z-normalized，并使用 0、3、6 或 9 位小数。这些都是在把“时间”说清楚。

面试里可以这样答：

```text
时间戳一致性要先定语义，再定格式。语义上先区分 instant、local timestamp 和 date；格式上再规定时区归一化、精度单位、舍入方式、是否允许 leap second、是否允许 offset。否则同一个时间点可以有多个合法字符串，同一个字符串在不同时区环境里也可能被解释成不同 instant。
```

实际协议里我会这样选：

```text
跨系统事件时间:
  用 epoch micros/nanos，字段名写清单位；或者 RFC3339 UTC Z，固定 fractional digits。
用户本地日程:
  存 local date-time 加 timezone id，比如 Asia/Shanghai，而不是只存 UTC instant。
日期字段:
  用 date，不要偷偷用当天零点 timestamp。
fingerprint:
  先归一化时间表示，再 hash。
logs:
  人读用 RFC3339，机器排序最好另有 epoch 字段。
```

还要注意运行时细节。比如 Go 的 `time.Time` 可能带 monotonic clock 信息，打印和 marshal 通常不会带出去，但比较时可能影响行为。协议层最好只序列化 wall-clock instant 和明确精度，不要把运行时内部状态混进去。

## Q009. bytes 和 string 在协议中应该如何区分？

**回答：**

最简单的原则是：`string` 是文本，`bytes` 是不透明字节。文本要满足字符编码约束，字节不应该被当成字符解释。

protobuf 官方类型表说得很清楚：`string` 必须包含 UTF-8 或 7-bit ASCII 文本，`bytes` 可以包含任意字节序列。Avro 也把 `string` 定义为 Unicode 字符序列，把 `bytes` 定义为 8-bit unsigned bytes 序列。Thrift protocol 里也区分 `T_STRING` 和 `T_BINARY`。

这不是命名洁癖，而是避免真实问题。

如果把二进制数据塞进 string，会遇到这些坑：

```text
UTF-8 validation:
  随机 bytes 可能不是合法 UTF-8。
normalization:
  文本系统可能做 Unicode normalization，binary 一做就坏。
escaping:
  JSON、日志、SQL、shell 都可能转义或截断控制字符。
length:
  字符数、code point 数、UTF-16 code unit 数、字节数不是一回事。
security:
  NUL、控制字符、不可见字符可能绕过展示和比较逻辑。
```

反过来，如果把文本设计成 bytes，也会损失语义。调用方不知道用 UTF-8、GBK、Latin-1 还是压缩后的文本；搜索、排序、大小写折叠、校验、日志展示都会麻烦。文本字段应该明确是文本，并规定编码。现代协议里一般就是 UTF-8。

面试可以这样说：

```text
string 和 bytes 的区别不是底层都能不能变成 byte array，而是协议承诺不同。string 承诺这是合法文本，可以按字符语义处理；bytes 承诺这是原始八位字节，不做字符解释。只要字段可能是 hash、签名、图片、压缩块、加密块、WAL record、protobuf 子消息、文件内容，就应该用 bytes。只要字段是用户名字、JSON path、错误消息、枚举名、时间字符串，就应该用 string。
```

在 JSON 里没有原生 bytes，所以通常要用 base64 字符串承载：

```json
{
  "payload_b64": "AQIDBA==",
  "sha256": "base64-or-hex-digest",
  "encoding": "base64"
}
```

字段名最好别只叫 `payload`，否则读者不知道它是原文、base64、hex 还是压缩后的数据。对外协议可以明确：

```text
payload:
  human-readable text, UTF-8 JSON string
payload_bytes:
  raw bytes in binary protocol
payload_b64:
  base64 encoding of raw bytes in JSON
```

还有一个细节：不要把“看起来能打印”当成 string 的标准。UUID、trace id、hex digest 虽然由 ASCII 字符组成，但语义上可能更接近标识符文本；如果它们要人工复制和日志检索，用 string 可以。如果它们是定长原始 digest 并参与二进制比较，用 bytes 更稳。看业务语义，不只看表面字符。

## Q010. base64 编码在 JSON 中的成本是什么？

**回答：**

base64 的主要成本是体积、CPU、内存和 canonical 表示。

体积最好算。RFC 4648 的 base64 把 3 个 8-bit 输入字节，也就是 24 bit，拆成 4 个 6-bit 组，再映射成 4 个可打印字符。所以大块数据会从 `N` bytes 变成大约 `ceil(N / 3) * 4` 个字符，接近 33.3% 体积膨胀。JSON 里还要再加一对引号和字段名。如果在 JavaScript 或某些运行时里以 UTF-16 字符串保存在内存里，内存占用可能更高。

举个简单例子：

```text
3 bytes  -> 4 base64 chars
30 MB    -> about 40 MB base64 text
```

如果这个字段还要进日志、消息队列、数据库、对象缓存，成本会一路放大。

CPU 成本也真实存在。编码时要把 bytes 转成字符，解码时要把字符还原成 bytes，还要校验 alphabet、padding、长度。小 token、小图片、小签名无所谓；大文件、大量二进制 payload 放在 JSON 里，就会把 API server 变成 base64 编解码器。

第三个成本是流式处理差。二进制协议或对象存储可以边读边写；JSON parser 经常要先拿到完整字符串，再 base64 decode，内存峰值可能同时包含 JSON 文本、base64 字符串和 decoded bytes。对几十 MB 甚至更大的 payload，这个很痛。

第四个成本是 canonicalization。RFC 4648 讨论了 padding、非 alphabet 字符、换行和 base64/base64url 字母表。标准 base64、URL-safe base64、有 padding、无 padding、带不带换行，可能都能解出同样 bytes，但文本不同。签名或 fingerprint 如果覆盖的是 base64 文本，就必须规定唯一写法。

面试里可以这样答：

```text
base64 让 JSON 能承载 bytes，但代价是约三分之一体积膨胀、额外 CPU、额外内存、流式处理困难，以及 padding 和 alphabet 带来的 canonical 表示问题。小字段可以接受，比如 nonce、digest、短证书片段；大 payload 不建议直接塞 JSON，最好放对象存储或二进制协议，JSON 里只放 ref、size、content-type 和 checksum。
```

常见设计可以这样分：

```text
small opaque bytes:
  JSON base64 可以，明确 standard base64 还是 base64url，是否带 padding。
large binary:
  放 S3/MinIO/object store，JSON 放 uri、length、sha256。
streaming RPC:
  用 binary framing、multipart、gRPC stream，而不是一个巨大 JSON string。
signature input:
  hash decoded bytes，或者规定 base64 canonical form 后 hash 文本，二者必须写清。
logs:
  避免直接打印大 base64，最多打印长度和 digest。
```

还有一个安全边界：解码前先检查长度上限。base64 文本长度可以推导最大 decoded size，不要等 decode 以后才发现请求能撑爆内存。

## Q011. protobuf unknown fields 的保留语义有什么用？

**回答：**

protobuf unknown fields 的作用，是让旧代码在不理解新字段的情况下，仍然尽量不破坏这条消息。它服务的是滚动升级、代理转发、存储再读、日志重放这类场景。

先看一个简单版本：

```proto
// v1
message Event {
  string id = 1;
}

// v2
message Event {
  string id = 1;
  string trace_id = 2;
}
```

如果 v2 producer 发出一条带 `trace_id` 的 binary protobuf，v1 consumer 解析时不认识字段号 `2`。protobuf 会把它当成 unknown field。v1 业务代码访问不到 `trace_id`，但只要它用 protobuf 消息对象原样转发、合并或重新序列化，这个字段通常还会被带出去。这样 v2 receiver 仍然能看到 `trace_id`。

这对分布式系统很重要。真实上线不是全量瞬间升级，而是：

```text
new producer -> old gateway -> old queue consumer -> new storage reader
```

中间某些节点可能还没有新 schema。如果这些旧节点只是做鉴权、路由、限流、打日志、写队列，不需要理解新字段，保留 unknown fields 就能减少升级顺序约束。否则旧节点只要 parse 再 serialize 一次，新字段就没了。

面试里可以这样答：

```text
unknown fields 是 protobuf forward compatibility 的缓冲层。旧程序不认识新字段，但可以保留它们，避免在代理、队列、日志重放或滚动升级过程中把未来版本的数据洗掉。它不是让旧业务理解新语义，只是让旧路径尽量不破坏新字段。
```

官方文档里有两个边界要讲清楚。

第一，保留主要说的是 binary protobuf。把 proto 转成 JSON 会丢 unknown fields；用 TextFormat 做交换也不稳。protobuf 官方建议需要保留 unknown fields 时使用 binary，并用 `CopyFrom()`、`MergeFrom()` 这类 message-oriented API，而不是自己遍历已知字段再拼一个新消息。

第二，unknown fields 不等于安全字段。旧代码不知道它们是什么，也不会做业务校验。比如一个旧 gateway 转发了未来版本里的 `is_admin`、`billing_plan`、`retention_policy`，它保留了字节，不代表它理解这些字段的权限含义。真正做授权和业务判断的一侧，仍然要用自己理解的 schema 校验。

还有几个常见坑：

```text
field-by-field copy:
  手写 new_msg.id = old_msg.id 会丢 unknown fields。
JSON bridge:
  proto -> JSON -> proto 会丢 unknown fields。
privacy:
  unknown fields 可能包含旧服务没意识到的敏感数据，日志和审计要小心。
bloat:
  长链路反复 merge 可能让 unknown payload 变大，尤其是代理层不设大小限制时。
canonical hash:
  unknown fields 是否参与 fingerprint 必须写清楚。参与会让未来字段影响旧 hash；不参与又可能让语义变化没有反映到 hash。
```

所以 unknown fields 的正确定位是：它帮助协议演进，但不替代版本治理。字段号仍然不能复用，删除字段仍然要 `reserved`，跨 JSON 边界仍然要重新考虑兼容性。

## Q012. JSON Schema 适合做什么，不适合做什么？

**回答：**

JSON Schema 适合描述 JSON 数据的结构、类型和局部约束。它能回答这些问题：

```text
这个值是不是 object？
哪些字段必填？
某个字段是不是 string / integer / array？
数组最少几个元素，最多几个元素？
字符串长度和 pattern 是什么？
数字范围是什么？
未知字段能不能出现？
某个字段是否必须符合另一个 schema？
```

JSON Schema 官方文档把它描述成用来注解和验证 JSON 文档的 vocabulary。2020-12 validation 规范里，`type`、`enum`、`const`、`minimum`、`maxLength`、`required`、`properties`、`items` 这些都是验证词汇。它很适合做 API request/response 契约、配置文件校验、事件 payload 校验、文档生成、表单生成、SDK 生成的输入之一。

一个典型 schema 可以这样写：

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["id", "amount_cents"],
  "properties": {
    "id": { "type": "string", "minLength": 1 },
    "amount_cents": { "type": "integer", "minimum": 0 },
    "tags": {
      "type": "array",
      "items": { "type": "string" },
      "maxItems": 20
    }
  },
  "additionalProperties": false
}
```

这类校验很有价值。它能在业务逻辑之前挡住明显坏输入，也能让错误信息更靠近边界。配置文件尤其需要这个：应用启动时就知道哪个字段缺了、哪个类型错了，不要等运行中某条路径触发空值。

但 JSON Schema 不适合承担所有语义。

第一，它不是授权系统。`role` 字段是 `"admin"` 这种格式正确，不代表调用方有权限设置它。权限要看身份、资源、操作和上下文。

第二，它不是业务事务规则引擎。比如“退款金额不能超过已支付金额”“结束时间必须晚于数据库里已有的开始时间”“同一用户每天最多创建 10 个任务”，这些需要查状态、查数据库、看时间窗口，JSON Schema 只能表达很有限的本地约束。

第三，它不是 canonicalization。schema 可以说字段类型和范围，但不会规定 JSON object key 的输出顺序、数字文本格式、空白、字符串转义。要做签名和 fingerprint，仍然需要 canonical JSON 或自定义规范化。

第四，它不是数据迁移工具。`default` 在 JSON Schema 里是 annotation，规范说它可以提供默认 JSON 值，但不等于 validator 必须把缺失字段写进实例。很多人以为 schema 里写了 `"default": 10`，请求里缺字段时服务端就自动得到 10。这要看具体 validator 或框架，协议不能含糊。

第五，它不能保证跨语言业务类型一致。JSON 只有 number，没有 int32、int64、decimal 的完整区分。你可以用 `type: integer`、`minimum`、`maximum` 收窄范围，但金额、ID、时间戳、base64 bytes 的语义仍然要在协议文档里写清楚。

面试可以这样收束：

```text
JSON Schema 是边界校验和契约描述工具，不是业务正确性的全部。它适合做结构、类型、范围、必填字段和未知字段控制；不适合替代权限、跨资源业务规则、数据迁移、canonicalization 和加密签名规则。
```

我通常会把它放在这层：

```text
parse JSON:
  先保证语法是 JSON。
schema validation:
  再保证结构和局部约束对。
business validation:
  再查权限、状态、数据库一致性。
canonicalization / persistence:
  最后再做规范化、签名、存储或派发。
```

这个顺序清楚，后面排错会容易很多。

## Q013. schema validation 应该发生在客户端还是服务端？

**回答：**

两边都应该做，但服务端必须是权威校验点。客户端校验是为了体验和减少无效请求，服务端校验是为了安全和数据一致性。

客户端校验有明显好处。表单里少填了必填字段、数字超范围、字符串太长、日期格式错了，前端可以马上提示，不必等一次网络往返。CLI、SDK、移动端也一样，越早告诉调用方越好。公开 API 还可以把 JSON Schema 给客户端，让它们生成类型、生成表单或做本地预校验。

但客户端校验不能被信任。原因很简单：请求可以绕过客户端。

```text
curl 可以直接打接口。
旧版本 SDK 可能没更新 schema。
浏览器 JS 可以被修改。
移动端 App 可能长期不升级。
内部服务也可能发错请求。
攻击者会故意构造客户端不会发出的 payload。
```

所以服务端仍然要 parse、schema validate、business validate。服务端应该把所有外部输入都当成不可信，包括来自“自己公司另一个服务”的请求。内部系统也会有版本漂移、bug 和重放流量。

面试里可以这样答：

```text
客户端校验负责早反馈，服务端校验负责真实边界。客户端可以复用 schema 来提升体验，但服务端不能因为客户端已经校验过就跳过校验。所有会影响持久化、权限、计费、调度、资源消耗的字段，都必须在服务端按当前 schema 和业务规则重新检查。
```

服务端校验还要分层。不要把所有校验都塞到 controller 里，也不要把所有错误都变成一个 `invalid request`。

```text
transport layer:
  body size、content-type、超时、压缩限制。
parser layer:
  JSON/protobuf 语法、嵌套深度、数字范围、UTF-8。
schema layer:
  required、type、enum、min/max、additionalProperties。
business layer:
  权限、状态机、配额、唯一性、跨字段规则。
storage layer:
  数据库约束、外键、唯一索引、事务隔离。
```

客户端和服务端共用 schema 时，也要处理版本问题。服务端可能支持 `v1` 和 `v2` 两套 schema；客户端可能还在用旧 schema。比较稳的做法是：

```text
request declares version:
  URL、header、media type 或 envelope 里带版本。
server chooses schema:
  按版本选择 validator，不让客户端自己决定校验规则。
errors are structured:
  返回字段路径、错误码、约束名，方便客户端展示。
schema is tested:
  CI 检查 breaking changes，至少覆盖新增 required 字段、类型变化、默认值变化。
```

还有一个容易忽略的点：schema validation 本身也有成本。复杂 JSON Schema 可能有递归 `$ref`、`oneOf`、`anyOf`、`patternProperties`、`unevaluatedProperties`。如果 schema 或 payload 来自不可信方，validator 也要设置超时、引用解析策略和资源限制。不要让“校验输入”这件事变成新的 DoS 入口。

## Q014. 反序列化不可信输入有哪些安全风险？

**回答：**

反序列化不可信输入的风险不只是“解析失败”。严重的时候，它可以变成远程代码执行、权限绕过、内存耗尽、CPU 打满、敏感字段被覆盖，或者把系统带进一个业务上不该出现的状态。

要先区分两类格式。

第一类是数据格式，比如 JSON、protobuf、Avro、CBOR。它们主要表达数据。风险通常集中在 parser bug、资源耗尽、类型混淆、未知字段处理、默认值误解和业务校验缺失。

第二类是对象序列化格式，比如 Java native serialization、Python pickle、Ruby Marshal、.NET BinaryFormatter 一类。它们不只是数据，还可能携带对象类型、构造流程、回调方法、反射入口。OWASP 反序列化指南明确提醒，很多语言原生反序列化机制的功能可以被攻击者重新利用，造成 DoS、访问控制绕过或 RCE。

常见风险可以这样讲：

```text
remote code execution:
  对象反序列化触发 gadget chain、构造器、readObject、反射调用或魔术方法。
resource exhaustion:
  超大 payload、深层嵌套、巨大数组、重复字段、压缩炸弹让 CPU、内存或栈耗尽。
type confusion:
  反序列化成比预期更宽的类型，或者动态类型字段接受了危险实现类。
trusted field clobbering:
  攻击者设置本应由服务端生成的字段，比如 is_admin、tenant_id、owner_id、created_at。
parser differential:
  网关、服务端、签名组件对重复 key、数字范围、Unicode、未知字段的解释不同。
logic bypass:
  缺失字段走默认值，旧 schema 忽略新字段，或者 unknown fields 在某个桥接点丢失。
information exposure:
  错误信息、日志、unknown fields 或 debug dump 泄露内部字段。
```

面试可以这样答：

```text
反序列化不可信输入时，我不会只问“能不能 parse”。我会问解析过程会不会执行代码，payload 能不能消耗过多资源，字段能不能覆盖服务端信任值，不同组件对同一输入的解释是否一致，以及反序列化后的对象有没有再经过 schema 和业务校验。
```

防护上也要分层：

```text
avoid dangerous formats:
  外部边界不要用 pickle、Java native serialization 这类可恢复对象图的格式。
allowlist types:
  如果必须用对象反序列化，只允许预期类型，不接受任意类名。
authenticate before deserialize when possible:
  OWASP 建议对已知消息做签名，未通过认证的消息不要进入危险反序列化路径。
set resource limits:
  body size、解压后大小、嵌套深度、数组长度、字符串长度、字段数量、parse timeout。
validate after parse:
  schema validation 只能做结构约束，业务字段还要按权限和状态机检查。
normalize before trust:
  拒绝重复 key、非法 UTF-8、超范围 number，避免 parser differential。
```

对 JSON/protobuf 这类数据格式，也不能松懈。它们通常不会因为字段本身直接执行代码，但仍然可以制造 DoS、绕过签名、污染缓存 key，或者让默认值和 unknown fields 造成语义差异。

## Q015. 反序列化炸弹是什么？

**回答：**

反序列化炸弹是指输入看起来不大，但解析、解压或展开之后会消耗远超预期的资源。它的目标通常不是拿到正确对象，而是让服务端 CPU、内存、磁盘、栈或线程池被耗尽。

最容易理解的是压缩炸弹。请求体可能只有几 MB，声明 `Content-Encoding: gzip`，但解压后变成几 GB。应用如果先全部解压到内存，再解析 JSON，就可能直接 OOM。

JSON 也有类似炸弹：

```json
[[[[[[[[[[[[[[[[[[[[0]]]]]]]]]]]]]]]]]]]]
```

这个例子不大，但如果嵌套深度达到几万层，递归 parser 或递归 validator 可能栈溢出。另一个方向是巨大数组：

```json
{"ids":[1,1,1,1,1, ... millions of entries ...]}
```

如果业务在每个元素上做数据库查询、正则匹配或权限检查，真正被打爆的可能不是 JSON parser，而是后面的业务逻辑。

还有一些更隐蔽的形式：

```text
entity expansion:
  XML 里的 billion laughs 是经典例子。
recursive schema validation:
  复杂 $ref、oneOf、anyOf 和 annotation 收集可能让 validator 成本很高。
hash collision:
  大量特制 key 触发哈希表退化，老运行时尤其危险。
string / regex bomb:
  超长字符串配合灾难性回溯正则。
protobuf nested messages:
  大量嵌套 message 或 packed repeated 字段让内存和递归深度失控。
base64 expansion:
  JSON 里短文本不一定短，base64 decode 后还有一份 bytes。
```

面试里可以这样说：

```text
反序列化炸弹的本质是输入大小和处理成本不成比例。攻击者不一定利用 parser 漏洞，只要让系统按正常逻辑展开、递归、校验、解压、分配或查询，就能把资源耗尽。
```

防护不是只设一个 body size。要看每一层的放大倍数：

```text
transport:
  限制原始 body 大小、连接时间、读取速率。
decompression:
  限制解压后大小和压缩比，支持流式解压。
parser:
  限制嵌套深度、字段数量、字符串长度、数字长度。
schema validator:
  限制 $ref 解析、正则复杂度、总验证时间。
business:
  限制数组长度、批量操作大小、每请求数据库查询数。
storage/logging:
  不把完整恶意 payload 原样打进日志。
```

RFC 8259 明确允许 JSON parser 对可接受文本大小、最大嵌套深度、数字范围和精度、字符串长度与字符内容设置限制。这个点面试时可以直接说：限制不是违反 JSON 规范，而是生产系统必须做的工程边界。

## Q016. 为什么要限制嵌套深度和数组长度？

**回答：**

限制嵌套深度和数组长度，是为了把输入的处理成本控制在可预测范围内。没有这些限制，攻击者可以用合法格式制造非法成本。

嵌套深度主要影响递归和栈。很多 parser、validator、业务遍历器都会递归处理 object、array、message。深度太大时，哪怕每层只有一个元素，也会让调用栈变深，或者让迭代式 parser 维护很大的状态栈。

比如：

```json
{"a":{"a":{"a":{"a":{"a":1}}}}}
```

深度是 5。把它变成深度 5000，就算总字节数不算夸张，也可能让递归解析、JSON Pointer 定位、schema validation、日志格式化全部变慢甚至崩掉。

数组长度影响的是总工作量。一个字段叫 `ids`，类型是 array，单个元素只是 integer。看起来很温和，但如果允许一百万个元素，后面的成本会被放大：

```text
内存:
  parser 要存数组，业务要建 slice/list，可能还要去重 map。
CPU:
  每个元素都要校验、转换、授权、查缓存。
数据库:
  一个请求可能变成百万级查询或一个超大 IN 条件。
日志:
  错误时把数组打印出来，日志系统也会被拖下水。
响应:
  返回结果也可能是同等规模，形成二次放大。
```

面试可以这样答：

```text
嵌套深度限制保护 parser、validator 和递归业务逻辑；数组长度限制保护内存、CPU、数据库和下游服务。它们不是随便加的安全开关，而是协议成本模型的一部分。一个 API 如果没有 maxDepth、maxItems、maxStringLength、maxBodyBytes，就很难说明自己面对恶意输入时的资源上界。
```

这些限制最好写进 schema 和网关配置里，而不是散落在业务代码中。

```json
{
  "type": "object",
  "properties": {
    "ids": {
      "type": "array",
      "items": { "type": "string", "minLength": 1, "maxLength": 64 },
      "minItems": 1,
      "maxItems": 1000,
      "uniqueItems": true
    }
  },
  "required": ["ids"],
  "additionalProperties": false
}
```

JSON Schema 能表达 `maxItems`、`maxLength`、`maxProperties` 这类约束，但最大嵌套深度通常要靠 parser 或 validator 配置额外控制。protobuf、Avro、Thrift 也一样，schema 只能表达一部分边界，解码器本身还要有 recursion limit、message size limit、string/bytes limit。

最后还要考虑错误返回。超限时不要返回含完整 payload 的错误，也不要为了构造漂亮错误而继续深度遍历。边界检查的目的就是尽早停止。

## Q017. fingerprint 应该基于原始 payload 还是规范化后的语义对象？

**回答：**

这要看 fingerprint 想回答哪个问题。

如果问题是“这串输入字节有没有变”，fingerprint 应该基于原始 payload。比如文件完整性、WAL record 校验、对象存储 ETag、上传去重、审计证据保存。原始 bytes 里一个空格、字段顺序、换行、base64 padding 变了，就说明 payload 真的变了。此时 hash 原始 payload 是合理的。

如果问题是“这份业务数据的语义是否相同”，fingerprint 应该基于规范化后的语义对象。比如两个 JSON：

```json
{"a":1,"b":2}
```

和：

```json
{ "b" : 2, "a" : 1 }
```

原始 bytes 不同，但很多业务会认为它们是同一个对象。要让它们得到同一个 fingerprint，就要 parse、校验、去掉无关差异，再按 canonical form 输出后 hash。

面试里可以这样答：

```text
raw payload fingerprint 适合完整性和审计，semantic fingerprint 适合去重、幂等和业务等价判断。最危险的是没有说清楚目标，直接把某个库输出的 JSON 或 protobuf bytes 拿去 hash，然后以为它代表业务语义。
```

两个方案的取舍很不一样。

```text
hash(raw bytes):
  优点是简单、可重放、和传输内容完全绑定。
  缺点是格式无关差异也会改变 hash，比如空白、字段顺序、转义形式。

hash(canonical semantic object):
  优点是能忽略无关表示差异，更适合幂等 key 和业务去重。
  缺点是必须定义 parse、validate、default、normalization、排序和输出规则。
```

规范化也不是一句“按语义 hash”就结束。要把这些问题写清楚：

```text
unknown fields:
  protobuf unknown fields 参与还是不参与？
defaults:
  缺失字段和显式默认值是否等价？
numeric:
  1、1.0、1e0 是否等价？int64 是否按字符串处理？
timestamp:
  +08:00 和 Z-normalized 是否等价？
string:
  Unicode normalization 做不做？
array:
  array 顺序是否有语义？能不能排序？
field filtering:
  server-generated 字段、trace_id、request_id、created_at 是否参与？
version:
  fingerprint 算法本身怎么版本化？
```

我通常会建议同时保留两个值：

```text
payload_digest:
  对原始 bytes 做 cryptographic digest，用于完整性、审计和排障。
semantic_fingerprint:
  对经过 schema validation 和 canonicalization 的业务对象做 hash，用于幂等、去重和缓存。
```

这样排查时不会互相替代。payload 变了但 semantic fingerprint 没变，说明只是表示差异；semantic fingerprint 也变了，说明业务值发生了变化。

## Q018. hash fingerprint 和 cryptographic digest 的区别是什么？

**回答：**

两者都可以是 hash 输出，但设计目标不同。

fingerprint 通常是工程里的短标识。它的目标是快速比较、索引、去重、分桶或发现大概率变化。比如 schema fingerprint、content fingerprint、cache key suffix、Bloom filter 输入。它可能很短，比如 32 bit、64 bit、128 bit，也可能使用非密码学 hash，比如 CRC32、xxHash、MurmurHash。它追求速度和低碰撞概率，但通常不承诺能抵抗恶意构造。

cryptographic digest 是密码学摘要。NIST FIPS 180-4 这类标准定义的 SHA-256、SHA-512 关注的是抗碰撞、抗原像、抗第二原像。它适合完整性检查、签名输入、证书、软件发布校验、内容寻址系统里的安全边界。这里攻击者会主动找碰撞或伪造。

可以这样对比：

```text
fingerprint:
  用于工程识别，假设碰撞是概率问题，不一定面对攻击者。
cryptographic digest:
  用于安全边界，假设攻击者会主动构造输入。
checksum:
  用于偶然损坏检测，比如 CRC，不适合防篡改。
MAC / signature:
  用于认证来源和防篡改，需要密钥或私钥。
```

面试可以这样说：

```text
hash fingerprint 更像“快速身份标签”，cryptographic digest 更像“安全摘要”。如果输入来自不可信方，或者结果用于权限、签名、制品校验、内容寻址、跨租户缓存隔离，就不能用非密码学 fingerprint 代替 cryptographic digest。再往上，如果要证明消息来自某个主体，还需要 HMAC 或数字签名，单纯 digest 没有认证能力。
```

一个实际例子：

```text
schema cache:
  64-bit fingerprint 可能够用，冲突后可以再比完整 schema。
artifact download:
  用 SHA-256 digest，下载后校验完整 bytes。
API request idempotency:
  可以用 canonical semantic object 的 SHA-256 截断值，但要有版本和冲突处理。
authentication token:
  不只 hash，要用签名或 MAC。
```

还有一个常见误区：使用 cryptographic hash 不等于协议安全。如果 hash 的输入没有 canonicalization，攻击者可能利用字段顺序、重复 key、Unicode、数字格式让签名方和验签方看到不同语义。如果 hash 没有 domain separation，不同用途的相同 bytes 还可能混淆。更稳的做法是：

```text
digest_input = "LogServe-SemanticFingerprint-v1\n" + canonical_bytes
digest = SHA-256(digest_input)
```

这里的前缀不是装饰，它把算法版本和用途写进输入，避免同一个 digest 被拿到别的协议里误用。

## Q019. 如何避免字段顺序变化导致 fingerprint 变化？

**回答：**

核心办法是：不要直接 hash 未规定顺序的序列化输出。先定义 canonicalization，再 hash canonical bytes。

JSON object、很多语言的 map、protobuf map 都不应该把遍历顺序当成语义。要避免字段顺序影响 fingerprint，可以按这个流程做：

```text
1. parse payload，得到结构化值。
2. 拒绝重复 key、非法 UTF-8、超范围数字这类歧义输入。
3. 按 schema 校验字段和类型。
4. 对 object/map 的 key 使用明确规则递归排序。
5. 对数字、字符串、时间、bytes 使用明确输出规则。
6. 输出无空白、无歧义的 canonical bytes。
7. 对 canonical bytes 做 hash。
```

对 JSON，RFC 8785 JCS 是一个现成参考。它要求去掉 token 间空白，按确定规则序列化字符串和数字，并递归排序 object 属性。它还要求输入适配 I-JSON，比如不能有重复属性名，数字要能表达为 IEEE 754 double。只要你的业务能接受这些限制，JCS 比自己拍脑袋定义排序规则更稳。

对 protobuf，要更谨慎。protobuf 官方明确说 deterministic serialization 不是 canonical serialization。它可以让同一个进程或同一套库更稳定，但不保证跨语言、跨版本、unknown fields、map 表示都得到唯一语义字节。尤其是 map 和 unknown fields，直接 hash binary protobuf bytes 很容易把实现细节混进 fingerprint。

面试可以这样答：

```text
字段顺序变化导致 fingerprint 变化，是因为 hash 看的是线性字节，不理解 map 的无序语义。解决办法是把“语义对象到字节”的规则固定下来，递归排序 object/map key，并规定数字、字符串、时间和 bytes 的输出方式。JSON 可以参考 JCS；protobuf 如果要语义 fingerprint，通常要把参与 hash 的字段投影成自己的 canonical form，而不是依赖默认 Marshal。
```

工程上我会补几条规则：

```text
array 不随便排序:
  array 顺序通常有业务意义，除非 schema 明确说它是 set。
sort key 要稳定:
  字符串按 UTF-8 bytes、Unicode code point 还是 JCS 的 UTF-16 code unit，要写清楚。
unknown fields 要明确:
  参与 hash 会让未来字段改变旧 fingerprint；不参与 hash 可能忽略未来语义。
defaults 要明确:
  缺失字段和显式默认值是否等价，必须由 schema 版本决定。
schema version 要进入输入:
  同一份 canonical bytes 在不同 schema 下可能含义不同。
```

还可以用测试把规则固定住。准备几组语义相同但字段顺序不同的 payload，要求 fingerprint 相同；再准备几组语义不同但表示很接近的 payload，要求 fingerprint 不同。没有这类 golden tests，canonicalization 很容易在重构时被破坏。

## Q020. 如何处理默认值变化导致的兼容性问题？

**回答：**

默认值变化是 schema evolution 里很容易被低估的一类 breaking change。字段类型没变，字段号没变，API 也没报错，但缺失字段的语义变了。

比如旧版本里：

```text
retry_enabled 缺失 => false
```

新版本改成：

```text
retry_enabled 缺失 => true
```

老数据没有这个字段。新 reader 一读，行为就变了：以前不会重试的任务，现在开始重试。这个变化不体现在 payload bytes 里，只体现在 reader 的解释规则里，所以特别隐蔽。

不同格式里默认值的含义还不一样。

protobuf proto3 的隐式 scalar 字段没有 presence。`bool retry_enabled = 1;` 读出来是 `false` 时，你不知道是发送方明确写了 false，还是字段缺失后得到默认 false。官方现在推荐对需要区分“没设置”和“设置为默认值”的标量字段使用 `optional`。这对兼容性很关键。

JSON Schema 的 `default` 是 annotation，不是强制填充规则。规范建议 default 值本身应该符合 schema，但没有要求 validator 自动把 default 写进实例。所以你不能只改 schema 的 `default`，就假设所有服务端、客户端、SDK 都会得到同样行为。

Avro 的 reader schema default 常用于读取旧数据缺失字段。它很实用，但也意味着 reader 版本会影响旧数据解释。如果你改了 reader default，旧文件没变，读出来的业务对象却变了。

面试里可以这样说：

```text
默认值是协议语义的一部分，不是文档注释。处理默认值变化时，最稳的是不要原地改变已有字段的默认语义；需要新行为时新增字段或新增版本，把旧数据的解释规则保留下来，并在读写路径中显式处理迁移。
```

常见处理办法有几类。

第一，写入时物化默认值。也就是说，producer 在写数据时把当时的默认值写进 payload，而不是让未来 reader 猜。

```json
{
  "retry_enabled": false
}
```

这样未来默认值变成 true，老数据仍然明确是 false。

第二，增加新字段，不改旧字段语义。

```proto
message JobPolicy {
  optional bool retry_enabled = 1;
  optional bool retry_enabled_v2 = 2;
}
```

这看起来不优雅，但比悄悄改变旧字段含义安全。稳定后可以在更大的版本迁移里清理。

第三，引入 schema 或语义版本。

```json
{
  "schema_version": 2,
  "retry_policy": "default"
}
```

reader 根据版本解释缺失字段，而不是用当前代码里的全局默认值解释所有历史数据。

第四，做离线迁移。把老数据读出来，按旧规则补齐字段，再写回新格式。这样线上 reader 就不用长期背负太多历史分支。

第五，测试默认值兼容性。准备老 payload，用新代码读；准备新 payload，用旧代码读；检查行为而不只是检查 parse 成功。

实际落地时，我会把默认值分成三类：

```text
wire default:
  格式层默认值，比如 proto3 scalar 的零值。
schema default:
  schema 或 reader 用来补缺失字段的值。
business default:
  产品语义上的默认行为，比如是否自动重试。
```

这三者不要混成一个词。很多兼容性事故就是因为大家都说“默认值”，但一个人在说 wire，一个人在说 schema，另一个人在说业务策略。

## Q021. 如何给长期存储的数据做 schema migration？

**回答：**

长期存储的数据做 schema migration，第一步不是写迁移脚本，而是先承认一个事实：数据活得比代码久。今天的 reader、SDK、业务默认值、字段名字，几年后都会变；但对象存储、日志、事件流、备份、审计文件里的 bytes 还在。

我一般会把长期数据 migration 拆成几层：

```text
stored bytes:
  原始 payload，可能是 JSON、protobuf、Avro、压缩块或加密块。
schema identity:
  schema id、版本号、fingerprint、writer schema 或 content-type。
reader path:
  当前代码如何把旧 bytes 解释成当前业务对象。
rewrite path:
  是否要把旧数据重写成新格式。
audit path:
  如何证明迁移前后数据没有丢、没有乱改。
```

长期存储里最重要的是保存“写入时的解释规则”。Avro 的设计很典型：数据读取时可以同时使用 writer schema 和 reader schema，规范里还有 schema resolution 规则。Avro object container file 还会把 schema 放进文件 metadata。这样十年后的 reader 不需要猜旧字段是什么，它可以按旧 writer schema 读出，再按新 reader schema 做解析。

protobuf 不会把完整 `.proto` 放进消息本身，所以长期存储时通常要额外保存：

```text
schema_version:
  业务版本，比如 event_v3。
descriptor set:
  对应的 FileDescriptorSet 或 schema registry id。
codec:
  protobuf/json/avro + 是否压缩 + 是否加密。
writer metadata:
  producer 名称、版本、生成时间。
```

JSON 更需要显式版本。JSON payload 看得懂，不代表语义清楚。`"status": "1"` 是字符串、枚举名、旧系统编码，还是临时状态？几年后没人会记得。长期 JSON 数据最好有 `schema_version`、`type`、`created_at`、`producer`，并把对应 schema 放进 registry 或代码仓库。

面试里可以这样答：

```text
长期数据 migration 的核心不是“把所有旧数据一次性改成新数据”，而是保证每一条旧数据都能找到写入时的 schema，并且新 reader 有可测试的升级路径。真正迁移时，可以先做读路径兼容，再灰度回填，最后收紧写路径和删除旧 reader 分支。
```

一个稳妥流程通常是：

```text
1. inventory:
  列出所有存储位置、格式、schema 版本、数据量、保留期限、访问频率。
2. compatibility design:
  判断是新增字段、重命名、拆字段、改类型，还是改变业务语义。
3. reader first:
  新代码先能读旧数据和新数据，不急着重写所有历史数据。
4. dual write / versioned write:
  新写入带明确版本，必要时短期双写旧字段和新字段。
5. backfill:
  后台批量扫描旧数据，按旧 schema 解析，按新 schema 写回或写到新表/新 bucket。
6. verification:
  抽样比对、全量计数、checksum、业务不变量、失败重试和迁移审计日志。
7. cleanup:
  等旧 reader、旧 producer、旧数据保留窗口都过去，再删除旧分支和旧字段。
```

不要把 schema migration 和业务 migration 混在一起。字段重命名是 schema migration；把 `status=PENDING` 的旧订单改成 `status=READY` 是业务 migration。前者看格式和兼容性，后者看业务状态机和审计。两者可以同一批跑，但设计和回滚方式不同。

长期存储还要考虑“不可重写”的数据。审计日志、append-only event log、合规归档，通常不应该原地改。可以在读时升级，或者写一条新的 correction / superseding event，而不是篡改原始事件。对于这类数据，保留原始 payload digest 很有用：迁移后的投影可以变，但原始证据还在。

## Q022. 在线迁移和读时迁移有什么 trade-off？

**回答：**

在线迁移通常指后台任务主动扫描旧数据，把它重写或回填成新格式。读时迁移是数据被访问时才升级：reader 发现旧版本，按旧 schema 读出来，再转换成新对象，有时还会顺手写回新格式。

两种方式没有绝对好坏，主要看数据量、访问模式、停机窗口、回滚要求和一致性要求。

在线迁移的优点是可控。你可以安排速率、分片、批量大小、重试策略、监控指标和验证流程。迁移完成后，线上读路径可以变简单，不需要长期背很多旧版本兼容代码。它适合这些场景：

```text
高访问频率数据:
  热数据如果每次读都做转换，会把成本压到用户请求上。
需要强约束:
  新索引、新唯一约束、新字段必须一次性准备好。
数据规模可控:
  可以在可接受时间内扫描完。
迁移可验证:
  能做全量计数、抽样 diff、checksum 或业务不变量检查。
```

它的缺点也明显。后台回填会占用数据库、对象存储、队列和网络资源。迁移脚本一旦有 bug，可能批量污染数据。迁移时间长时，新旧 writer 还在同时写入，容易出现“双轨数据”。回滚也麻烦：你要么保留旧数据备份，要么能从新格式反推旧格式。

读时迁移的优点是懒。没有被访问的数据不用动，长尾冷数据不会占用迁移窗口。它适合冷数据很多、访问分布极不均匀、或者不能轻易批量重写的存储。比如用户几年没打开的历史配置，读时迁移很合理。

但读时迁移会把复杂度放到请求路径上：

```text
latency:
  第一次读旧数据会变慢。
tail risk:
  很久没人读的旧版本可能多年后才暴露 bug。
code burden:
  reader 要长期支持多个旧版本。
write-back race:
  多个请求同时读旧数据并尝试写回，可能互相覆盖。
observability:
  迁移进度不明显，必须额外统计 version 分布。
```

面试可以这样答：

```text
在线迁移把成本集中在后台，读路径更干净，但需要强监控、限速、回滚和验证。读时迁移把成本摊到访问时，适合冷数据和不可一次性扫描的数据，但会增加请求延迟和长期兼容负担。实际系统经常混用：热数据先在线回填，冷数据保留读时迁移兜底。
```

比较稳的组合是：

```text
phase 1:
  新 reader 能读旧版本和新版本。
phase 2:
  新 writer 只写新版本，或者短期双写。
phase 3:
  在线迁移热数据和最近数据。
phase 4:
  读时迁移冷数据，命中后写回。
phase 5:
  用指标确认旧版本读写量接近 0，再删除旧代码。
```

这里有一个判断标准很实用：如果旧 reader 分支会影响安全、计费、权限或状态机，就不要让它长期存在。读时迁移看起来省事，但长期留一套旧语义在生产路径里，迟早会变成没人敢删的风险。

## Q023. 序列化格式中是否应该存类型名？

**回答：**

是否存类型名，要看这个类型名是协议类型，还是运行时类名。前者经常需要，后者通常危险。

协议类型名是稳定的、受 schema 管理的名字，比如：

```text
com.example.payment.PaymentCaptured
type.googleapis.com/google.rpc.Status
event_type = "order.created.v2"
schema_id = 1024
content_type = "application/x-protobuf; message=OrderCreated"
```

这类名字是协议的一部分。它帮助消费者选择 schema、选择 decoder、做路由、做权限控制和做演进管理。Avro named type 有 fullname 和 alias；protobuf 的 `Any` 也会保存 type URL，里面放的是被嵌入消息的类型标识。事件系统里保存 `event_type` 或 `schema_id` 很常见。

运行时类名是另一回事，比如：

```text
com.foo.internal.UserEntity
org.hibernate.proxy.CustomerProxy
java.util.HashMap
```

把这种名字放进外部 payload，会把协议绑死在某个语言、包名、框架和实现细节上。更糟的是，某些对象反序列化机制会根据类名加载类、构造对象、调用回调。这正是很多反序列化漏洞的入口。

面试里可以这样答：

```text
序列化格式里可以存稳定的协议类型标识，比如 event_type、schema_id、protobuf Any type URL；不要把运行时类名当成协议。类型名一旦进入长期数据和跨语言接口，就要按公开协议治理，不能随代码重构随便改包名。
```

存类型名也有 trade-off。

优点：

```text
self-describing:
  consumer 拿到 envelope 后知道该用哪个 schema。
routing:
  broker、日志系统、事件处理器可以按类型分发。
evolution:
  type + version 可以清楚表达协议族。
debugging:
  人看 payload 时更容易判断是什么事件。
```

缺点：

```text
coupling:
  类型名会变成长期兼容承诺。
security:
  不能让不可信输入决定加载哪个类。
privacy:
  类型名本身可能暴露业务内部结构。
size:
  每条消息都写长字符串会增加开销。
```

工程上我更喜欢 envelope 里放稳定字段：

```json
{
  "type": "payment.captured",
  "version": 2,
  "schema_id": "paycap-v2-2026-06",
  "encoding": "protobuf",
  "payload_b64": "..."
}
```

如果系统追求效率，可以把 `type + version` 映射成短整数 `schema_id`。调试工具再通过 registry 把 `schema_id` 展开成人类可读名字。不要为了省几个字节直接省掉类型信息，也不要为了“方便反射”把类名暴露到协议里。

## Q024. 跨语言 SDK 如何保证序列化语义一致？

**回答：**

跨语言 SDK 保证序列化一致，靠的不是“大家都按文档理解”，而是同一份 schema、同一套 canonical 规则、同一批测试向量和明确的兼容性检查。

最基本的是 schema 单一来源。protobuf 用 `.proto`，Avro 用 `.avsc` 或 IDL，JSON API 用 JSON Schema / OpenAPI。SDK 不应该各自手写字段和默认值。生成代码、手写封装、文档都应该从同一份 schema 派生。

第二是把语义写到协议层，而不是写到某个语言的习惯里。

```text
integer:
  int64 在 JSON 里是 number 还是 string？
timestamp:
  RFC3339 UTC Z，还是 epoch millis？
bytes:
  binary 协议用 bytes，JSON 用 base64url 还是 standard base64？
map:
  fingerprint 时 key 按什么规则排序？
defaults:
  缺失字段和显式默认值是否等价？
unknown fields:
  SDK 转发时保留还是丢弃？
```

这些规则如果没写清，语言差异一定会漏出来。JavaScript 的 number、Java 的 `BigDecimal`、Go 的 `time.Time`、Python 的 `datetime`、Rust 的 enum、C# 的 nullable，各自都有坑。

第三是 golden tests。每个 SDK 都要用同一批输入，得到同一批输出。测试不能只测“能解析”，还要测 bytes、canonical JSON、错误行为和边界值。

```text
encode tests:
  logical object -> expected bytes / expected canonical JSON
decode tests:
  bytes / JSON -> expected logical object
round-trip tests:
  old payload -> new SDK decode -> encode -> expected compatibility behavior
error tests:
  duplicate key、unknown enum、超大 int64、非法 UTF-8、缺失 required 字段
cross-language tests:
  Go encode -> Java decode -> Python encode -> compare canonical form
```

第四是版本矩阵。新 SDK 和旧服务端、旧 SDK 和新服务端都要测。protobuf 官方 best practices 里提醒，客户端和服务端不会刚好同时更新，也可能回滚。这个判断非常现实。SDK 发布时最好跑：

```text
SDK N   <-> server N
SDK N   <-> server N-1
SDK N-1 <-> server N
SDK old payload -> SDK new decode
SDK new payload -> SDK old decode, if compatibility is promised
```

面试可以这样说：

```text
跨语言一致性靠协议治理，不靠口头约定。schema 要单一来源；数字、时间、bytes、默认值、unknown fields、canonicalization 要写成规范；每个 SDK 都跑同一批 golden vectors；CI 做跨语言 encode/decode 矩阵。只要某个语言可以自己解释语义，就迟早会和别的语言不一致。
```

还有一个现实建议：SDK 对外暴露的 API 可以符合本语言习惯，但 wire 层必须收敛到同一语义。比如 Python 可以用 `datetime`，Go 可以用 `time.Time`，但最终 wire 必须都是 UTC epoch micros 或 RFC3339 UTC 字符串。方便开发者和保证协议一致，不是一回事。

## Q025. 如何设计 golden test 验证序列化兼容性？

**回答：**

golden test 的目标是固定协议行为。它不是普通单元测试，不只是测函数现在能不能跑，而是把“这些输入必须永远这样编码、这样解码、这样报错”写成证据。

一套好的 golden test 至少包括四类文件：

```text
logical cases:
  人类可读的对象，比如 JSON/YAML，表达业务值。
wire cases:
  期望的 binary bytes、canonical JSON bytes 或 base64/hex dump。
schema versions:
  v1、v2、v3 的 schema，以及兼容性说明。
expected outcomes:
  encode 输出、decode 输出、是否报错、错误码、是否保留 unknown fields。
```

目录可以这样设计：

```text
testdata/serialization/
  schemas/
    event_v1.proto
    event_v2.proto
  cases/
    001_minimal/
      input.json
      expected.pb.hex
      expected.canonical.json
      expected.decode.json
    002_unknown_field/
      input.pb.hex
      expected.reencoded.pb.hex
    003_invalid_duplicate_key/
      input.json
      expected_error.json
```

case 不要只放正常值。序列化兼容性的 bug 往往在边界值里：

```text
numbers:
  0、-0、最大 int32、最大 int64、超过 2^53 的整数、decimal 字符串。
strings:
  空字符串、中文、emoji、组合字符、控制字符、非法 UTF-8。
time:
  UTC、带 offset、毫秒/微秒/纳秒、闰秒策略、夏令时边界。
bytes:
  空 bytes、随机 bytes、base64 padding、base64url。
maps:
  不同 key 顺序、重复 key、大小写接近的 key。
schema evolution:
  新字段、删除字段、reserved field、unknown fields、默认值变化。
security:
  深层嵌套、超长数组、超大字符串、压缩炸弹样本的安全替身。
```

面试里可以这样答：

```text
golden test 要测 wire-level 稳定性，也要测 semantic-level 兼容性。每个语言 SDK 都读取同一批 test vectors：给定逻辑对象，编码结果必须一致；给定历史 bytes，解码结果必须一致；给定非法输入，错误行为必须一致。这样协议变更时，CI 会告诉你到底是故意升级，还是无意破坏兼容性。
```

golden test 还要区分 deterministic 和 canonical。比如 protobuf 默认 binary 输出不适合承诺跨语言 canonical bytes。那就不要写一个错误的测试要求“所有语言 Marshal bytes 完全相同”。更合理的是：

```text
protobuf binary:
  decode 后比较语义对象；对需要稳定的场景另定义 canonical projection。
canonical JSON:
  比较完整 bytes，因为 canonicalization 本来就是协议承诺。
schema fingerprint:
  比较 parsing canonical form 或明确算法输出。
```

测试更新也要有纪律。golden 文件一旦变化，PR 里要说明：

```text
是 schema 版本升级？
是 bug 修复？
是否仍能读取旧 golden payload？
是否需要迁移历史数据？
是否影响签名、fingerprint、缓存 key 或幂等 key？
```

如果团队允许随手 `update snapshots`，golden test 很快就失去意义。协议测试最怕的就是“测试跟着实现一起漂”。

## Q026. 为什么日志或事件中保存完整请求可能带来隐私风险？

**回答：**

完整请求很诱人，因为排障方便。线上出问题时，看到原始 body、headers、query、用户上下文，确实能少猜很多。但它的风险也很直接：请求里经常有不该长期保存的数据。

常见敏感内容包括：

```text
Authorization header:
  bearer token、API key、session cookie。
credentials:
  密码、验证码、一次性 token、OAuth code。
PII:
  姓名、手机号、邮箱、身份证号、地址、IP、设备标识。
financial:
  银行卡、账户、账单、交易细节。
health / government identifiers:
  医疗、政务、未成年人或脆弱群体相关数据。
internal secrets:
  数据库连接串、签名密钥、加密 key、webhook secret。
business confidential:
  合同、报价、模型输入、客户文件内容。
```

OWASP Logging Cheat Sheet 明确建议，这类数据通常不应直接记录到日志里，而应该删除、脱敏、哈希或加密。它还特别提到 access tokens、session identification values、敏感个人数据、密码、连接串、加密密钥、银行卡数据等。

完整请求保存到事件里还有几个额外风险。

第一，保留期限变长。API 请求本来可能只在内存里停留几毫秒；一旦写进日志、事件流、对象存储、备份和数据湖，就可能保存几个月甚至几年。

第二，访问面变大。能查业务数据库的人不一定能看原始请求；但日志平台、搜索平台、告警平台、离线分析系统可能有另一套权限。完整请求进入这些系统后，敏感数据的复制范围会扩大。

第三，删除困难。用户要求删除个人信息时，结构化数据库可以按 user_id 找；日志里的完整请求可能散落在压缩文件、备份、索引、冷存储里，查找和删除成本很高。

第四，调试字段会被滥用。今天为了排查临时加的 `log full request`，明天可能变成默认行为。事故往往不是没人知道风险，而是没人把临时代码关掉。

面试可以这样答：

```text
完整请求日志的本质问题是数据最小化失败。它把认证凭据、PII、业务机密和用户内容从短生命周期请求路径搬进了长期、可搜索、多人可访问的日志系统。排障价值是真的，但应该通过字段级白名单、脱敏、采样和受控调试开关来拿，而不是默认保存完整请求。
```

更稳的做法是记录“足够排障”的摘要：

```text
request_id:
  用于串联链路。
actor / tenant:
  记录内部 id，不记录原始 token。
operation:
  API 名、event type、schema version。
shape:
  payload size、字段是否存在、数组长度、错误字段路径。
digest:
  对原始 payload 做 SHA-256，用于确认是否同一请求，但不暴露内容。
redacted preview:
  只显示白名单字段，敏感字段固定写 "<redacted>"。
```

真正需要保存完整 payload 时，要把它当成敏感数据：单独权限、短保留期、加密、审计访问、采样、工单审批，以及明确的自动删除机制。

## Q027. 如何在事件 payload 中处理 PII 脱敏？

**回答：**

事件 payload 里的 PII 脱敏，最重要的是先做分类，再决定保留什么。不要等到日志系统里再靠正则“抢救”。正则可以兜底，但不应该是主设计。

我会先把字段分成几类：

```text
not sensitive:
  event_type、schema_version、status、count、region 这类普通字段。
direct identifiers:
  姓名、手机号、邮箱、身份证号、银行卡、精确地址。
indirect identifiers:
  IP、设备 id、user agent、地理位置、生日、公司名。
secrets:
  token、cookie、password、private key、connection string。
user content:
  用户输入文本、上传文件、prompt、评论、工单内容。
```

处理策略也不一样。

```text
drop:
  完全不需要的字段直接不写。
mask:
  需要人工识别但不能暴露全量，比如 phone = "138****1234"。
hash:
  需要关联同一主体，但不需要还原，比如 HMAC(email)。
tokenize:
  需要后续受控还原，保存 token，映射表放到受保护系统。
encrypt:
  需要保留原文但严格控制访问，字段级加密并审计解密。
generalize:
  精确位置变城市，生日变年龄段，时间戳降精度。
```

面试里可以这样说：

```text
事件脱敏最好在 producer 或边界层做白名单，而不是在日志平台黑名单过滤。payload schema 要标注字段敏感级别，默认不记录未知字段；需要关联分析时用 HMAC 这类带密钥的稳定 pseudonym，不要直接用普通 hash 处理手机号或邮箱，因为低熵 PII 很容易被字典反查。
```

一个事件可以设计成这样：

```json
{
  "event_type": "user.login_failed",
  "schema_version": 3,
  "tenant_id": "t_123",
  "user_ref": "hmac_sha256:v1:7b3a...",
  "ip_prefix": "203.0.113.0/24",
  "reason": "bad_password",
  "password": "<redacted>"
}
```

这里 `user_ref` 不是明文邮箱，`ip_prefix` 不是完整 IP，`password` 固定不保存。这样安全团队还能按用户、租户和原因分析失败登录，但不会拿到原始密码或完整身份信息。

还要注意两个坑。

第一，脱敏要在序列化前做，或者至少在进入事件总线前做。等 payload 已经进入 Kafka、对象存储、日志采集 agent，再说“下游会脱敏”，风险已经扩散。

第二，脱敏规则要版本化。字段改名、新增 nested object、新增 `metadata` map，都会绕过旧规则。最好在 schema 里标注：

```text
pii: direct | indirect | secret | none
redaction: drop | mask | hmac | encrypt
```

CI 可以检查：新增字段如果没有敏感级别标注，就不允许合并。运行时也可以对未知字段采取拒绝或丢弃策略，避免 “extra” map 变成 PII 下水道。

## Q028. 压缩前签名和压缩后签名有什么区别？

**回答：**

签名保护的是字节，不是你脑子里的对象。压缩前签名和压缩后签名，签的字节不同，验证语义也不同。

假设原文是：

```text
payload = canonical_json_bytes
compressed = gzip(payload)
```

压缩前签名是：

```text
signature = Sign(payload)
send compressed + signature
```

验证方要先解压，再对解压后的 `payload` 验签。它保护的是原始语义字节。不同压缩等级、不同 gzip timestamp、不同压缩库输出，只要解压后 payload 一样，签名仍然可以通过。

压缩后签名是：

```text
signature = Sign(compressed)
send compressed + signature
```

验证方先对收到的压缩 bytes 验签，再解压。它保护的是传输中的压缩表示。任何压缩参数变化都会让签名变化，即使解压后的 payload 完全相同。

面试可以这样答：

```text
压缩前签名保护解压后的内容，适合“内容语义一致即可”的场景；压缩后签名保护压缩后的传输 bytes，适合“收到的每一个字节都必须和发送方产生的一样”的场景。两者都可以成立，但协议必须写清验证顺序、签名输入、压缩算法和压缩参数。
```

两种方式还有安全和工程差异。

压缩后签名的一个优点是：可以先验签再解压。这样未认证的压缩炸弹不会进入解压器，DoS 风险低一些。但它要求签名方和验证方对压缩 bytes 完全一致，长期兼容更麻烦。

压缩前签名的优点是：签名和压缩实现解耦。你可以换压缩等级、换算法、重新打包对象，只要内容不变，签名还有效。缺点是验证方通常要先解压再验签，所以必须有解压大小限制、压缩比限制和流式处理。

JWS 的模型可以帮助理解：JWS signing input 包含 protected header 和 payload 的 base64url 表示，本质上就是明确规定“签哪些 octets”。JWE 则有 `zip` header，表示 plaintext 在加密前压缩。不同标准不是随便排列步骤，而是在协议里把每一步的输入写死。

实际设计时我会这样选：

```text
content signature:
  canonicalize -> sign canonical bytes -> optionally compress for transport。
transport package signature:
  compress/package -> sign final package bytes。
untrusted large payload:
  优先先认证再解压，或者使用 detached digest + 安全解压限制。
long-term archive:
  同时保存 content digest 和 package digest，分别用于语义完整性和归档完整性。
```

不要只说“我们对 payload 签名”。要说清楚：payload 是压缩前还是压缩后，是 canonical JSON 还是 protobuf bytes，是加密前还是加密后，header 是否参与签名。

## Q029. 加密数据还能否被压缩？

**回答：**

一般来说，加密后的数据很难再有效压缩。好的加密输出应该接近随机，压缩算法靠发现重复模式、频率偏斜和结构冗余来省空间；密文里这些模式应该被隐藏掉。所以常规顺序是先压缩，再加密。

但“先压缩再加密”也不是永远安全。RFC 8725 对 JWT 的建议很直接：不要压缩加密输入，因为压缩后的长度可能泄露明文信息。CRIME、BREACH 这类攻击的核心思路就是观察压缩后密文长度，反推出秘密和攻击者可控文本之间的相似性。

面试里可以这样答：

```text
加密后通常压不动，因为密文应该像随机字节；压缩要放在加密前才有效。但压缩前加密会引入长度侧信道，特别是同一个压缩上下文里混合了攻击者可控文本和秘密。安全协议不能只按“压缩率最高”排序，要看攻击模型。
```

什么时候可以压缩再加密？

```text
offline archive:
  数据不包含攻击者可控反射输入，主要目标是省存储。
large internal batch:
  输入来源可信，长度侧信道不可被外部反复观察。
object storage:
  单个对象压缩后加密，访问者看不到可控 probe 的长度反馈。
```

什么时候要谨慎甚至禁止？

```text
web response:
  响应里同时有 CSRF token/session secret 和攻击者可控内容。
interactive API:
  攻击者可以反复提交 probe，并观察密文长度。
shared compression context:
  多用户或多请求共享字典/上下文。
encrypted token:
  小 token 压缩收益不大，长度泄露风险更不值。
```

还有一个现实问题：压缩会改变错误处理。解密后再解压，如果没有限制解压大小，仍然可能遇到压缩炸弹。即使密文通过认证，也不代表解压后大小合理。协议里要写：

```text
compression algorithm:
  gzip、zstd、deflate，版本和参数。
max compressed size:
  输入密文或压缩块大小上限。
max decompressed size:
  解压后大小上限。
max ratio:
  压缩比过高直接拒绝。
authenticated metadata:
  原始长度、算法、content type 要参与认证。
```

如果数据已经加密，还想节省带宽，通常不要指望再压缩密文。应该回到源头：压缩明文前的结构化数据，或者换更紧凑的明文格式，比如 protobuf、Avro、CBOR，再按安全模型决定是否压缩。

## Q030. 如何设计可调试又高效的 payload 格式？

**回答：**

可调试和高效不是非此即彼。比较好的设计是分层：外层 envelope 可读、可路由、可排障；内层 payload 可以用高效二进制；旁边配套 schema registry、debug tool 和 golden test。

一个实用结构可以这样：

```json
{
  "type": "workflow.step.completed",
  "version": 3,
  "schema_id": "wf-step-completed-v3",
  "encoding": "protobuf",
  "compression": "zstd",
  "payload_ref": "inline",
  "payload_b64": "...",
  "payload_sha256": "..."
}
```

外层 JSON 的作用是让人和基础设施看懂：这是什么事件、哪版 schema、用什么编码、是否压缩、payload 摘要是什么。内层 protobuf 或 Avro 负责效率：字段号、二进制数值、紧凑数组、明确类型。这样 broker、日志系统、死信队列和排障工具不需要解完整 payload，也能做基本路由和索引。

面试里可以这样答：

```text
我会把 payload 格式拆成 envelope 和 body。envelope 保存 type、version、schema_id、encoding、compression、trace_id、payload digest 这些调试和路由字段；body 用 protobuf/Avro 这类高效格式。再提供 CLI 或 Web 工具根据 schema_id 解码 body。这样线上路径高效，排障时也不需要对着一串 base64 猜。
```

几个设计点很关键。

第一，schema 必须可找。只有 `payload_b64` 没用，调试工具还要知道 schema。可以用：

```text
schema_id:
  指向 registry。
schema_fingerprint:
  防止 registry 内容漂移。
descriptor:
  必要时随归档保存 writer schema 或 descriptor set。
```

第二，payload 要有 digest。digest 可以让你判断对象存储里的大 payload 有没有被改，也能在日志里引用同一份内容而不打印原文。对调试来说，`payload_sha256` 经常比完整 payload 更有用。

第三，大 payload 不要强行 inline。小事件可以 inline base64；大对象放对象存储：

```json
{
  "payload_ref": "s3://bucket/key",
  "payload_size": 4823912,
  "payload_sha256": "..."
}
```

第四，错误和死信要保留足够上下文。至少包括解析失败原因、schema_id、producer、request_id、payload size、digest。是否保留原始 payload，要按敏感级别和保留策略决定。

第五，提供调试工具，而不是要求人手工 decode：

```text
payloadctl inspect event.json
payloadctl decode --schema-id wf-step-completed-v3 payload.bin
payloadctl canonicalize input.json
payloadctl diff old.bin new.bin --schema-id ...
```

工具要能显示字段路径、unknown fields、默认值来源、bytes 长度、时间戳解释和 redaction 状态。没有工具的二进制格式，很快会被团队绕回 JSON。

最后要把效率边界写清楚：

```text
hot path:
  binary body，避免重复字段名和大 JSON number/text 转换。
control plane:
  JSON envelope，便于 grep、审计和手工排障。
privacy:
  envelope 不放 PII；body 根据 schema 做字段级脱敏或加密。
compatibility:
  schema evolution 规则、golden test、旧版本 reader 都要存在。
observability:
  记录 type、version、size、digest、decode_error，而不是默认记录完整 payload。
```

一个 payload 格式如果只能高效但没人看得懂，排障成本会转嫁给所有人；如果只可读但巨大、慢、没有 schema，规模上来以后也会出问题。分层通常是最稳的折中。

## Q031. canonical JSON 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？
**回答：**

canonical JSON 的核心目标是把同一个 JSON 语义值转换成唯一、稳定、可重复的字节序列。它不是为了“排版统一”，也不是为了“让 JSON 更快”，而是为了让 hash、签名、fingerprint、幂等 key、内容寻址和跨语言比较有一个共同的输入。

普通 JSON 的自由度很高：object 字段顺序可以不同，空白可以不同，字符串可以用不同转义形式，数字也可能被不同运行时格式化成不同文本。对业务阅读来说，这些差异可能不重要；但对摘要和签名来说，一个字节不同就是完全不同的输入。canonical JSON 的价值就是把这些“表示层差异”收敛掉。

RFC 8785 的 JSON Canonicalization Scheme，也就是 JCS，把这个目标落到了规则上：

```text
不输出多余空白；
对象属性递归排序；
数组顺序保持不变；
字符串和数字按 ECMAScript JSON 序列化规则输出；
最终输出 UTF-8；
拒绝重复属性名、非法 Unicode、NaN、Infinity 等无法稳定互操作的输入。
```

所以如果在“正确性、性能、安全性、可维护性”里选主目标，我会说 canonical JSON 首先解决的是正确性和互操作性。正确性不是说“业务字段一定合法”，而是说同一个语义对象在不同机器、不同语言、不同时间得到同一个 canonical bytes。它和安全性关系很近，因为签名和摘要必须建立在稳定字节上；但 canonical JSON 本身不是签名算法，也不负责密钥管理、防重放、权限校验或 schema validation。

它通常不是性能优化。canonicalization 往往要 parse、排序、重新 serialize，再 hash，热路径里会增加 CPU 和内存开销。它能提升可维护性，但这是明确规则之后带来的副作用：排障、跨语言测试、历史重算会更容易。没有签名、摘要、去重、幂等、审计这类需求时，只为了“规范一点”引入 canonical JSON，收益通常不够。

面试里可以这样概括：

```text
canonical JSON 的主目标是确定性字节表示，主要解决 correctness/interoperability。它让语义相同但字段顺序、空白、转义形式不同的 JSON 得到同一个 fingerprint。它对安全协议很重要，因为签名前必须先明确到底签哪串字节；但它不是安全协议本身，也不是性能优化，更不是 schema validation。
```

## Q032. canonical JSON 的典型适用场景和不适用场景分别是什么？
**回答：**

canonical JSON 适合的场景有一个共同点：系统仍然希望使用 JSON 作为可读、可传、可审计的格式，但又需要稳定字节表示。它更像控制面、审计面、签名面、幂等面工具，不太适合作为所有高吞吐数据面的默认处理步骤。

典型适用场景包括：

```text
数字签名和验签:
  WebHook、配置文件、策略文档、供应链元数据、审计记录都需要签名方和验签方看到同一串 bytes。

内容寻址和 fingerprint:
  工作流定义、规则对象、feature flag、实验配置，只要语义相同就应该得到同一个 id。

幂等和去重:
  API gateway、任务队列、事件处理系统可以用 canonical digest 降低字段顺序变化导致的重复提交。

跨语言 golden test:
  Go、Java、Python、Node.js SDK 对同一批输入生成同一份 canonical bytes 和 digest。

长期可审计文档:
  JSON 保持人类可读，canonical bytes 用于版本比较、签名、审计和缓存 key。
```

不适用场景也要讲清楚。

第一，不适合超大、高频、低延迟 payload 的每次请求处理。canonical JSON 要递归排序对象 key。宽对象会有 `O(k log k)` 排序成本，深对象会递归放大，再加上 parse 和重新序列化，高并发下 CPU 和 GC 压力会很明显。

第二，不适合表达任意精度数字。JCS 这类方案通常要求 JSON number 能被 IEEE 754 double 表达。`int64`、`uint64`、decimal 金额、大整数、纳秒时间戳，如果要求精确互操作，应该用 string 或结构化对象表达。

第三，不适合替代业务语义归一化。canonical JSON 会保留数组顺序，不会知道某个数组是不是集合；也不会把邮箱转小写、删除默认值、补业务字段或判断枚举是否合法。这些要由 schema 和业务规则定义。

第四，不适合需要原始字节身份的场景。法务取证、raw request 签名、底层协议调试，可能关心客户端实际发来的每个字节。这时 fingerprint 应该基于 raw payload，而不是 canonicalized object。

第五，不适合无边界处理不可信输入。攻击者可以构造极深嵌套、极宽对象、超长 key 或大量重复字段，逼服务消耗 CPU 和内存。canonical JSON 仍然需要 size、depth、string length、key count 和超时限制。

面试里可以说：

```text
canonical JSON 适合签名、摘要、幂等、内容寻址、跨语言测试和可审计配置；不适合大规模热路径数据传输、任意精度数字、流式超大 payload、schema validation、业务 normalization，也不适合必须保留原始字节身份的场景。它解决的是表示确定性，不是所有 JSON 问题。
```

## Q033. canonical JSON 和相近概念最容易混淆的边界在哪里？
**回答：**

canonical JSON 最容易和 minified JSON、deterministic serializer、JSON Schema、业务 normalization、hash/signature、protobuf deterministic serialization 混在一起。边界说不清，设计时就很容易把一件事的保证误用到另一件事上。

第一，canonical JSON 不是 minified JSON。minify 只是去掉空白，例如把格式化文本压成一行；但字段顺序、数字格式、字符串转义、重复字段、非法 Unicode 等问题仍然存在。canonical JSON 通常还要求递归排序对象属性，并限制输入集合。

第二，canonical JSON 不等于“sort keys 的 JSON.stringify”。很多语言库支持按 key 排序输出，但未必符合 RFC 8785。例如排序依据可能是 UTF-8 字节，也可能是语言默认字符串比较；数字格式化、Unicode surrogate、NaN/Infinity、重复 key 的行为也可能不同。只说“我们排了 key”不是完整 canonicalization 规范。

第三，canonical JSON 不是 JSON Schema。JSON Schema 判断实例是否符合结构和约束，例如字段是否必填、类型是否正确、枚举是否合法；canonical JSON 判断一个已经接受的 JSON 值怎样确定性输出成 bytes。一个 JSON 可以通过 schema，但因为重复 key 或非法 Unicode 不能 canonicalize；也可以被 canonicalize，但业务字段完全不合法。

第四，canonical JSON 不是业务语义归一化。例如：

```json
{"email":"Alice@Example.com"}
{"email":"alice@example.com"}
```

canonical JSON 不会自动认为这两个邮箱相同。再比如：

```json
{"tags":["a","b"]}
{"tags":["b","a"]}
```

如果 `tags` 在业务上是集合，需要业务层定义排序和去重规则；canonical JSON 只知道 JSON array 是有序的，不能自作主张。

第五，canonical JSON 不是 hash，也不是签名。正确链路通常是：

```text
parse and validate
canonicalize
hash canonical bytes
sign digest or compare digest
```

canonicalization 只产出稳定 bytes；hash 和签名负责摘要、认证、完整性和不可抵赖性。密钥管理、防重放、过期时间、权限边界都不是 canonical JSON 自动提供的。

第六，它也不能直接套用 protobuf 的确定性序列化语义。protobuf 官方明确提醒 deterministic serialization 不是 canonical serialization，尤其在跨语言、跨版本、unknown fields、map 表示上不能随便把二进制 bytes 当语义 fingerprint。JSON canonicalization 和 protobuf deterministic serialization 是两个格式里的不同保证。

第七，重复字段名是重要边界。RFC 8259 说 object 成员名应该唯一，但很多 parser 会接受重复 key，并采用 first wins、last wins 或保留全部的不同策略。做签名和 fingerprint 时，重复 key 最稳的处理是拒绝，因为不同解析结果可能导致安全绕过或跨语言漂移。

面试里可以这样收束：

```text
canonical JSON 的边界是表示层确定性。它不是 minify，不是 schema validation，不做业务语义归一化，也不是 hash/signature。凡是字段合法性、默认值、数组是否当集合、大小写是否等价、PII 是否脱敏，都要在 schema 或业务层定义。
```

## Q034. canonical JSON 在高并发场景下可能出现哪些隐藏问题？
**回答：**

canonical JSON 在小样例里只是 parse、sort、serialize；但在高并发服务里，它会变成 CPU、内存、锁竞争和资源边界问题。

第一个隐藏问题是重复计算。一个请求可能在网关、鉴权、幂等、事件发布、审计日志里各做一次 canonicalization。每次都要 parse、递归排序、重新输出、hash。宽对象的排序是 `O(k log k)`，嵌套多时还会递归放大。最后线上看到的不是“canonical JSON 慢”，而是 p99 延迟和 CPU 使用率突然上升。

第二个问题是内存和 GC。常见实现会同时持有 raw bytes、解析后的 DOM、排序 key 列表、canonical output buffer、hash 输入或 digest。大 payload 或高并发下，短命对象很多，GC 压力会上来。Go、Java、Node.js、Python 都会遇到，只是表现不同。

第三个问题是可变对象竞态。canonicalization 必须基于稳定快照。如果一个线程正在 canonicalize，另一个线程还在修改 map、slice、dict 或对象字段，就可能输出不稳定，甚至出现 data race 或 panic。正确做法是输入对象不可变，或者 canonicalization 前复制出只读快照。

第四个问题是共享状态竞争。为了省分配，有些实现会复用全局 buffer、encoder、hasher、schema registry、LRU cache 或对象池。单线程 benchmark 看起来很好，高并发下可能变成锁热点；如果共享 encoder 不是线程安全的，还可能串数据。优化必须通过 race test、mutex profile 和压测验证。

第五个问题是 cache stampede。如果大配置对象的 canonical bytes 或 digest 被缓存，服务重启、配置刷新或缓存失效时，大量请求同时 miss，会一起做昂贵计算。可以用 singleflight、预热、版本化 cache key、最大 payload 限制来缓解。

第六个问题是不可信输入导致资源耗尽。极深嵌套、极宽对象、超长字段名、巨型字符串、重复 key 洪泛，都会让 canonicalization 消耗大量 CPU 和内存。RFC 8259 允许实现限制 JSON 文本大小、嵌套深度、数字范围、字符串长度；线上入口也应该这么做。

第七个问题是局部优化破坏规范。有些团队为了快，只排序顶层 key，不处理数组里的对象；或者用语言默认排序规则，导致跨语言不一致。这种错误在简单单测里不明显，灰度到多语言 SDK 后才表现为间歇性验签失败、缓存 miss 或幂等失效。

工程上我会这样控制：

```text
canonicalization 尽量只做一次，后续传 digest 或 canonical bytes；
输入先限制 size、depth、key count、string length；
基于不可变对象或快照处理；
避免共享可变 encoder 和 hasher；
缓存要有版本，并防 cache stampede；
metrics 拆 parse、sort、serialize、hash、reject reason；
用并发压测和 race test 验证实现。
```

## Q035. canonical JSON 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？
**回答：**

canonical JSON 一旦参与签名、幂等、去重、审计或内容寻址，它就不只是一个本地函数，而是业务状态机的一部分。崩溃、重启、超时和重试会把“digest 什么时候产生、和副作用怎么对齐、版本是否稳定”这些边界暴露出来。

第一，要看 canonical bytes 和 digest 是否被原子地产生并持久化。比如服务算出 digest 后写幂等表，刚写完 digest 还没完成业务副作用就崩溃。重试时如果只看到 digest，可能误判请求已经完成。更稳的做法是记录清楚状态：

```text
RECEIVED:
  收到 raw payload，还没通过校验。
CANONICALIZED:
  得到 canonical bytes 和 digest。
COMMITTED:
  digest 记录和业务副作用在同一事务边界内完成，或者有明确补偿。
```

第二，超时可能发生在副作用之后。客户端没收到响应就重试，如果 fingerprint 稳定，服务可以识别同一请求；如果 fingerprint 依赖 raw JSON 字段顺序，客户端重试时换了序列化顺序，就可能被当成新请求，造成重复创建、重复扣款或重复发事件。

第三，重启可能带来版本漂移。新版本 canonicalization 对数字、Unicode、排序、错误处理、默认值的策略如果变了，同一个 payload 的 digest 会变。长期存储或签名系统应该把算法版本写入 envelope、数据库记录和签名上下文，例如：

```json
{
  "canonicalization": "jcs-rfc8785-v1",
  "schema_version": 4,
  "hash": "sha-256",
  "payload_digest": "..."
}
```

第四，缓存冷启动会造成延迟尖峰。运行时可能缓存 schema、descriptor、canonical bytes 或 digest。重启后缓存失效，高流量同时进来会触发大量重算。缓存还要带版本号，否则灰度发布期间可能拿旧规则结果服务新规则请求。

第五，非法输入的错误语义必须稳定。重复 key、NaN、Infinity、非法 Unicode、过大数字，不能这个版本接受、下个版本拒绝；也不能一个服务 last wins，另一个服务 first wins。重试链路里这种差异会被放大。

第六，raw payload 是否保留要权衡。排障时可能需要知道客户端原始发了什么，但完整保存 raw payload 可能带来隐私风险。常见折中是保存 raw digest、canonical digest、size、schema version、reject reason、trace id，而不是默认记录完整原文。

面试里可以说：

```text
canonical JSON 在 crash/retry 场景里暴露的核心问题是 digest 生命周期和业务副作用必须对齐。要记录算法版本、schema 版本、canonicalization 状态、payload size、raw digest 和 canonical digest；非法输入要确定性失败；超时重试不能因为字段顺序或版本变化变成新请求。
```

## Q036. canonical JSON 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？
**回答：**

canonical JSON 的算法瓶颈通常先来自 CPU 和内存，锁竞争是实现层常见次级瓶颈。I/O 和网络不是 canonicalization 本身的成本，除非链路里还包含远程 schema lookup、KMS 签名、对象存储读取或审计写入。

CPU 成本主要有四块。

```text
parse:
  解析 JSON 文本，处理字符串转义、数字 token、UTF-8、对象和数组结构。

sort:
  每个对象都要收集 key 并按固定规则排序；宽对象是 O(k log k)，深对象递归叠加。

serialize:
  按规范重新输出字符串、数字、对象、数组和字面量，不能依赖不稳定默认格式。

hash/sign:
  严格说不属于 canonicalization，但真实链路通常 canonicalize 后立刻 hash 或签名。
```

内存成本来自中间结构。很多实现会同时持有 raw bytes、parsed DOM、排序用 key list、canonical output buffer。大 payload 下内存峰值可能远大于原始输入；高并发下短命对象会增加 GC。优化时可以复用局部 buffer、减少复制、限制 payload，但不能牺牲输出规则。

锁竞争通常来自共享实现细节，比如全局 encoder、buffer pool、hasher、schema registry、LRU cache、metrics 聚合。单次 benchmark 不一定看得出来，高并发下 p99 会抖。需要用 CPU profile、heap profile、mutex/block profile 和压测拆开看。

I/O 成本通常在 canonicalization 前后：读数据库、拉对象存储、写审计日志、查 schema registry。网络成本也类似：远程签名服务、KMS、配置中心、schema registry 都可能盖过本地算法成本，但这不是 canonical JSON 规范本身带来的。

实际判断可以按 payload 特征看：

```text
小 payload、低并发:
  通常不是瓶颈。

宽对象:
  key 排序和字符串比较明显。

深嵌套:
  递归、深度检查和对象分配明显。

大 payload:
  parse、copy、serialize、hash 和内存峰值都明显。

高并发:
  GC、锁竞争、cache stampede 可能压过单次算法耗时。
```

面试里我会强调不要猜：

```text
要分段打点 parse、sort、serialize、hash、IO，并看 p50/p95/p99、allocs、heap、GC 和 mutex profile。canonical JSON 通常不是网络瓶颈；本地 CPU 和内存才是最常见问题。
```

## Q037. canonical JSON 的 correctness test、stress test 和 benchmark 应该分别测什么？
**回答：**

correctness test、stress test、benchmark 的目标不同。correctness test 证明规则对；stress test 证明极端和恶意输入下不会失控；benchmark 证明真实负载下成本可接受。

correctness test 要围绕规范不变量和 golden vectors。我会至少覆盖：

```text
字段顺序:
  输入顺序不同，canonical bytes 相同。

递归排序:
  顶层对象、深层对象、数组里的对象都排序。

数组顺序:
  数组顺序必须保持，不能当集合乱排。

空白:
  缩进、换行、空格变化不影响输出。

字符串:
  引号、反斜杠、控制字符、Unicode 转义输出稳定。

Unicode:
  合法 Unicode 不做额外 normalization，非法 surrogate 拒绝。

数字:
  1、1.0、1e0、-0、指数形式、边界 double、过大整数按规则处理。

非法值:
  NaN、Infinity、重复 key、非法 UTF-8 必须确定性失败。

UTF-8:
  最终输出 bytes 是 UTF-8。

幂等:
  canonicalize(parse(canonicalize(x))) == canonicalize(x)。
```

correctness test 最好跨语言跑。Go、Java、Python、Node.js、Rust 对同一批输入生成同一份 canonical bytes 和同一个 SHA-256。失败时不要只保存 digest，要保存 canonical bytes 或十六进制结果，否则很难定位是排序、数字、Unicode 还是重复 key 策略不同。

stress test 关注资源边界和稳定性。它不要求所有输入都成功，很多 case 正确结果就是快速拒绝。可以测：

```text
极深嵌套；
极宽对象；
超长 key 和 string；
大量相似 key；
重复字段洪泛；
超长 number token；
非法 Unicode；
随机 JSON fuzz；
并发 canonicalize；
输入流中断、取消、超时；
内存上限和最大输出限制。
```

benchmark 要贴近真实 payload 分布，而不是只测 `{"a":1}`。指标至少包括：

```text
p50/p95/p99 latency；
throughput；
CPU time；
allocations/op 和 bytes/op；
heap peak 和 GC；
不同 key 数、深度、payload size 下的曲线；
with cache / without cache；
single-thread / high-concurrency；
canonicalize only / canonicalize + hash / canonicalize + sign。
```

还要有 baseline，比如普通 JSON marshal、只 hash raw bytes、完整 canonical JSON、protobuf deterministic serialization。baseline 的意义不是证明谁永远最好，而是知道为了“稳定语义 fingerprint”付出了多少成本。

面试里可以简洁回答：

```text
correctness test 看规范和跨语言 golden bytes；stress test 看恶意输入、资源上限、并发和取消；benchmark 看真实 payload 下 parse/sort/serialize/hash 的耗时、分配和 p99。只断言 digest 不够，必须保存 canonical bytes，方便定位差异来源。
```

## Q038. 如果要求从零实现一个简化版 canonical JSON，你会先定义哪些不变量？
**回答：**

从零实现 canonical JSON，第一步不是写排序代码，而是定义不变量。canonicalization 的本质是：在一个明确输入集合上，所有实现都必须得到同一个输出。输入集合和输出规则不清楚，代码越短越危险。

我会先定义输入不变量：

```text
类型:
  只接受 object、array、string、number、true、false、null。

对象:
  不允许重复 key；key 必须是合法 Unicode 字符串。

数组:
  保留顺序；元素递归 canonicalize。

字符串:
  必须是合法 Unicode；不做 Unicode normalization；相同字符串只有一种输出。

数字:
  明确支持范围。可以选择只支持 IEEE 754 double 可表达数字，或更保守地只支持安全整数。
  NaN、Infinity、超长 number token 必须拒绝。
  int64、decimal、大整数如果要精确互操作，要求编码成 string。
```

数字规则必须先定，因为跨语言最容易在这里出问题。JavaScript number 是 double；Go 默认可能把 JSON number 解成 float64；Java 和 Python 又可能保留更高精度。如果不限定，fingerprint 会随语言变化。

然后定义输出不变量：

```text
encoding:
  输出 UTF-8 bytes。

whitespace:
  不输出非必要空白。

object order:
  所有对象 key 按明确规则排序。

recursive:
  递归处理所有对象，包括数组里的对象。

array order:
  数组顺序不变。

string output:
  转义规则固定。

number output:
  格式固定，不能受 locale、运行时版本或默认 formatter 影响。

error:
  非法输入确定性失败，不能自动修复。
```

排序规则也必须写死。是按 UTF-16 code unit、Unicode code point，还是 UTF-8 bytes？如果要兼容 JCS，就不能随便用语言默认比较；如果做简化版选择 UTF-8 字节序，也要写进算法版本，不能声称自己就是 RFC 8785。

再定义幂等不变量：

```text
canonicalize(parse(canonical_bytes)) == canonical_bytes
```

如果 canonical 输出再 parse 再 canonicalize 还会变化，说明数字、字符串或 key 排序规则不稳定。

还要定义资源边界：

```text
max input bytes；
max output bytes；
max nesting depth；
max object keys；
max string length；
max number token length；
timeout/cancellation behavior。
```

最后定义版本和测试不变量：

```text
algorithm = "simple-json-c14n-v1"；
hash = "sha-256"；
schema_version 单独记录；
跨语言 golden vectors 固化；
非法输入在所有实现中同样失败。
```

面试里可以说：

```text
我会先定义输入子集、重复 key 策略、数字范围、Unicode 策略、对象 key 排序规则、数组顺序规则、字符串/数字输出规则、UTF-8 输出、错误语义、资源上限和算法版本。canonical JSON 的核心不是 sort keys，而是这些不变量共同保证同一个语义值只有一个字节表示。
```

## Q039. canonical JSON 的常见误用是什么，误用后通常会产生什么线上症状？
**回答：**

canonical JSON 的误用常见在“做了一点，但以为已经完整”。线上症状也不一定直接写着 canonicalization 失败，而是表现为验签失败、缓存 miss、幂等失效、CPU 抖动或跨语言不一致。

常见误用可以按症状记。

```text
hash raw JSON bytes:
  字段顺序、空白、转义变化都会导致 digest 变化。
  症状是幂等 key 命中率低、缓存重复、同一配置出现多个版本。

只用普通 JSON.stringify / marshal:
  语言库默认行为可能受 map 顺序、浮点格式、HTML escape、非 ASCII 转义影响。
  症状是本地验签成功，换语言 SDK 或灰度版本后失败。

只排序顶层 key:
  嵌套对象、数组里的对象仍然不稳定。
  症状是简单请求正常，复杂 payload 间歇性失败。

忽略重复字段名:
  签名方和业务方可能看到不同字段值。
  症状是跨语言解析结果不一致，严重时有安全绕过风险。

用 JSON number 存 int64/decimal:
  超过安全整数范围或需要十进制精度时会丢精度。
  症状是订单金额、雪花 id、纳秒时间戳、资源 id digest 漂移。

把数组当集合自动排序:
  canonicalization 擅自改变业务语义。
  症状是权限列表、规则优先级、执行步骤被错误合并或重排。

把 canonicalization 当 validation:
  字段非法、状态机非法、PII 未脱敏仍然会进入后端。
  症状是“签名通过但业务非法”，或者日志泄漏敏感 payload。

热路径反复 canonicalize:
  每层服务都 parse/sort/serialize/hash。
  症状是 CPU 飙升、GC 增加、p99 抖动。

不记录算法版本:
  规则升级后历史 digest 无法复现。
  症状是老签名验不过、审计链断裂、冷数据迁移后 fingerprint 变化。
```

一个特别典型的问题是把 canonical JSON 当“万能语义相等”。例如两个对象一个省略默认值，一个显式写默认值：

```json
{"retry":3}
{"retry":3,"timeout_ms":1000}
```

如果业务规定 `timeout_ms` 默认就是 `1000`，这两个对象业务上可能等价；但 canonical JSON 不会知道这个默认值规则。要让它们 fingerprint 相同，必须在业务 schema normalization 阶段先补齐或删除默认值，并把这个过程版本化。

面试中可以这样总结：

```text
常见误用是 hash raw JSON、只 sort keys、忽略数字和重复 key、把数组当集合、把 canonicalization 当 validation、在热路径重复做、不记录算法版本。线上症状通常是间歇性验签失败、跨语言 digest 不一致、幂等失效、缓存 miss、历史审计无法复现，以及 CPU/GC/p99 异常。
```

## Q040. canonical JSON 在单机和分布式环境中的语义有什么差异？
**回答：**

在单机里，canonical JSON 更像一个本地确定性函数：给定同一个 JSON value，得到同一个 canonical bytes。只要同一个进程、同一个库、同一个版本，很多问题不明显。到了分布式环境，它就变成跨语言、跨版本、跨时间的协议契约。

单机场景主要关注本地稳定性：

```text
map/dict 遍历顺序不能影响输出；
重启前后输出不能变化；
运行时升级不能悄悄改变数字或字符串格式；
共享 encoder/cache 不能污染不同请求；
并发调用不能有 data race。
```

如果它只用于本地快照测试、配置比较、单服务缓存 key，影响范围相对可控。

分布式环境中，canonical fingerprint 往往参与跨服务验签、API 幂等、消息去重、审计链路、内容寻址、多副本一致性比较和多语言 SDK 输出一致性。此时语义不再是“我的函数怎么输出”，而是“所有参与方共同承诺哪些输入合法、如何排序、如何输出数字、如何处理 Unicode、如何失败、如何版本化”。

最容易出问题的是语言和版本差异。Go、Node.js、Java、Python 可能使用不同 parser。一个库接受重复 key，一个库拒绝；一个库把大整数转成 float，一个库保留 decimal；一个库 escape 非 ASCII，一个库直接输出 UTF-8；一个库对负零输出 `0`，另一个输出 `-0`。单机里这些差异可能永远遇不到，分布式验签时就会变成 digest 不一致。

分布式系统还要求错误语义一致：

```text
duplicate key:
  reject。

NaN/Infinity:
  reject。

unsafe integer:
  reject or encode as string。

invalid Unicode:
  reject。

max size / max depth:
  reject with stable error code。
```

算法版本也必须进入协议。旧消息、旧签名、旧审计记录可能要保存很多年。`canonicalization_version`、`schema_version`、`hash_algorithm` 最好写在 envelope 或持久化记录里，而不是藏在代码版本里。

上线策略也不同。单机可以直接升级；分布式系统要灰度：

```text
先 shadow 计算新 digest；
比较新旧 digest 差异；
跨语言 golden test 先通过；
灰度 producer，再灰度 consumer；
过渡期同时接受旧版本和新版本；
停止旧版本写入后仍保留旧版本读取和验证能力。
```

最后，网络重试和消息重放会把 canonical fingerprint 变成幂等边界。请求可能超时，消息可能重复投递，消费者可能重启，leader 可能切换。如果 fingerprint 不稳定，系统会重复执行副作用；如果 normalization 过度，又可能把不同请求误判成同一个。

面试里可以这样说：

```text
单机里 canonical JSON 是本地确定性序列化函数；分布式里它是跨语言、跨版本、跨时间的协议契约。分布式环境必须显式定义输入子集、错误语义、算法版本、schema 版本、数字和 Unicode 策略，并用跨语言 golden test 和灰度策略保证一致。否则线上症状会是验签失败、幂等失效、消息重复或历史审计无法复现。
```

## Q041. protobuf 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？
**回答：**

protobuf 的核心目标，是用一个明确的 schema 描述结构化数据，然后用跨语言、跨平台、可演进的二进制 wire format 高效传输和存储这些数据。官方概述里对 protobuf 的定位很清楚：它是 language-neutral、platform-neutral、extensible 的结构化数据序列化机制，目标是比 XML 这类文本格式更小、更快、更简单。

如果只在“正确性、性能、安全性、可维护性”里选一个，容易说偏。protobuf 同时服务于性能、正确性和可维护性，但主轴是“高效的结构化数据交换”和“schema evolution”。

性能方面，protobuf 用字段号而不是字段名编码，常见整数用 varint，定长数值用固定 4 或 8 字节，小消息通常比 JSON 更紧凑。解析时也不需要反复比较字符串字段名，生成代码可以直接按字段号写入结构体字段。对 RPC、消息队列、日志事件、内部服务调用来说，这个差距很实际。

正确性方面，protobuf 的 schema 给字段类型、字段号、message 结构、enum、oneof、repeated、map 等定义了明确边界。相比“大家约定 JSON 字段怎么写”，protobuf 更不容易出现 `id` 一会儿是数字、一会儿是字符串、一会儿缺失的情况。wire format 里每个字段带 field number 和 wire type，旧 reader 遇到不认识的字段也可以跳过，这让版本演进有基础。

可维护性方面，`.proto` 文件是契约。它能生成多语言代码，也能作为 API review、兼容性检查、文档和 golden test 的共同来源。字段号一旦分配，就变成长期协议的一部分；这比散落在各语言里的手写 JSON 编解码更容易管理。

安全性不是 protobuf 的核心目标。protobuf 不负责认证、授权、加密、签名、防重放，也不自动防止反序列化资源耗尽。不可信输入仍然要做大小限制、递归深度限制、超时、权限校验和业务校验。protobuf 只是让结构化数据更紧凑、更可演进，不是安全协议。

面试里可以这样答：

```text
protobuf 的核心目标是 schema-first 的高效结构化数据交换。它主要解决性能、跨语言互操作和版本演进问题；正确性来自字段类型和 wire compatibility 规则，可维护性来自 .proto 作为长期契约。它不是安全机制，也不是 canonical fingerprint 格式。签名、加密、权限和不可信输入防护仍然要在协议层做。
```

## Q042. protobuf 的典型适用场景和不适用场景分别是什么？
**回答：**

protobuf 适合的场景，一般有三个特征：数据结构相对稳定，生产者和消费者都愿意共享 schema，系统关心带宽、CPU、延迟或长期兼容性。它最擅长的是内部服务之间的强契约数据交换。

典型适用场景包括：

```text
RPC:
  gRPC 是最常见例子。请求和响应结构明确，生成代码能减少手写解析错误。

内部事件和消息队列:
  Kafka、Pulsar、NATS、日志事件都可以用 protobuf 作为 payload，配合 schema registry 或 descriptor 管理版本。

移动端和边缘端通信:
  带宽和电量敏感，protobuf 的紧凑二进制格式比冗长 JSON 更有优势。

长期存储的结构化记录:
  只要保存 writer schema 或严格管理 schema evolution，旧数据可以被新代码读取。

跨语言 SDK:
  一份 .proto 生成 Go、Java、Python、C++、TypeScript 等代码，减少各语言手写协议漂移。

高频控制面或数据面协议:
  字段重复、结构固定、消息量大时，protobuf 的字段号编码和生成代码比较合适。
```

不适用场景也不少。

第一，不适合没有稳定 schema 的开放 JSON API。如果字段经常临时变化，消费者很多且无法协调升级，JSON 加 OpenAPI 可能更容易对外沟通。protobuf 对字段号、类型和兼容性纪律要求更高。

第二，不适合需要人直接读写的配置文件。二进制 protobuf 不适合手工编辑。虽然有 Text Format 和 ProtoJSON，但它们各自有兼容性边界，不能简单当成“更好的 YAML”。

第三，不适合需要 canonical bytes 的 fingerprint 或签名场景。protobuf 官方文档明确提醒 serialization is not canonical。deterministic serialization 也不等于跨语言、跨版本、跨 schema 的 canonical serialization。要做语义 fingerprint，应该定义额外的规范化规则，或者选专门的 canonical 格式。

第四，不适合任意动态对象。`google.protobuf.Any` 可以带类型 URL，但如果滥用，会把 schema 契约变成运行时类型分发，调试和兼容性都会变差。protobuf 不是“二进制 JSON object”。

第五，不适合强依赖字段名语义的场景。binary protobuf 编码的是字段号，不编码字段名。没有 `.proto` 或 descriptor，payload 基本不可读。对审计、排障和数据湖分析来说，要配套 schema registry、decoder 工具和版本记录。

第六，不适合把不可信大输入直接扔进解析器。protobuf 消息、string、bytes 的理论上限很大，嵌套 message 也可能很深。线上入口仍然要限制消息大小、递归深度、解析时间和解压后大小。

面试里可以这么说：

```text
protobuf 适合内部 RPC、事件消息、移动端通信、跨语言 SDK 和长期演进的结构化数据；不适合随手改字段的开放 API、人手编辑配置、需要 canonical 签名的原始格式、没有 schema 管理的数据湖、以及不受限的不可信输入。它的前提是团队愿意把 .proto 当协议契约维护。
```

## Q043. protobuf 和相近概念最容易混淆的边界在哪里？
**回答：**

protobuf 最容易和 JSON、gRPC、IDL、schema registry、Avro、Thrift、canonical serialization、压缩、加密混在一起。边界讲清楚后，很多设计争论会简单很多。

第一，protobuf 不是 gRPC。protobuf 是序列化格式和 IDL；gRPC 是 RPC 框架，常用 protobuf 定义 service、request、response，但 gRPC 还包含 HTTP/2、状态码、metadata、streaming、deadline、负载均衡等内容。你可以在 Kafka 里用 protobuf，也可以在 gRPC 里理论上用别的编码。

第二，protobuf 不是 JSON 的二进制压缩版。JSON object 依赖字段名，protobuf binary 依赖字段号和 wire type。JSON 可以没有 schema 也被人类大致读懂；protobuf 没有 schema 几乎没法正确解释字段。protobuf 的优势不是“把 JSON 压小”，而是 schema-first、生成代码和 wire compatibility。

第三，protobuf 的 `.proto` 是 schema，但不是数据库 schema。它描述消息编码和字段类型，不描述索引、事务、唯一约束、外键、查询计划。把 protobuf message 直接等同于数据库表，容易把存储模型和传输模型绑死。

第四，protobuf deterministic serialization 不是 canonical serialization。确定性序列化通常只保证同一个实现、同一份数据在某些条件下输出稳定；官方文档强调 protobuf serialization is not canonical。unknown fields、map 顺序、字段重复表示、不同语言实现、不同版本 runtime 都可能影响 bytes。因此不能直接把 protobuf bytes 当长期语义 fingerprint。

第五，protobuf unknown fields 不是业务扩展字段系统。unknown fields 的目的是让旧 reader 在看到新字段时能保留或跳过，支持版本演进。它不是给业务塞任意 JSON、任意 metadata 的后门。业务要扩展，应该明确字段、版本和兼容规则。

第六，`bytes` 和 `string` 不是一回事。官方 encoding 文档里，`string` 是合法 UTF-8 字符串，`bytes` 是任意 8-bit byte 序列。把二进制塞进 string，会造成 UTF-8 校验、日志乱码、JSON 转换和跨语言问题。

第七，protobuf 不是压缩算法。protobuf 比 JSON 紧凑，是因为字段号、varint、二进制数值等编码方式减少冗余；它不做通用压缩。大文本、重复字符串、相似 message 批量传输，仍然可能需要 gzip、zstd 或传输层压缩，但要注意压缩炸弹和长度侧信道。

第八，protobuf 不是加密或签名。二进制不可读不等于安全。中间人仍然可以复制、重放、篡改未认证的 protobuf bytes。安全要靠 TLS、消息签名、AEAD、鉴权和 replay protection。

面试里可以这样答：

```text
protobuf 是 schema-first 的序列化格式和 IDL，不是 gRPC 本身，不是 JSON 压缩器，不是数据库 schema，不是 canonical fingerprint 格式，也不是安全机制。它最重要的边界是字段号和 schema 才定义语义；没有 schema 的 bytes 没有业务含义，deterministic serialization 也不能当长期 canonical bytes 用。
```

## Q044. protobuf 在高并发场景下可能出现哪些隐藏问题？
**回答：**

protobuf 常被认为“快”，但高并发下仍然会出问题。快的是 wire format 和生成代码，不代表使用方式一定快，也不代表没有内存、锁和资源边界。

第一个隐藏问题是反复 marshal/unmarshal。很多服务一进来把 protobuf 解析成对象，中间层又转 JSON 打日志，发下游前再 marshal，写审计时再转一次。单次 protobuf 很快，多层重复转换后，CPU、分配和 GC 都会上来。尤其是大 repeated 字段、map 字段、嵌套 message，成本会被放大。

第二个问题是 message 对象复用不当。为了减少分配，有人会把 protobuf message 放进对象池。对象池如果没有彻底 reset，就可能把上一个请求的字段带到下一个请求，尤其是 repeated、map、oneof、unknown fields。高并发下这类 bug 很隐蔽，表现为偶发脏数据。

第三个问题是并发读写同一个 message。protobuf message 通常应该按普通可变对象看待。一个 goroutine 正在 marshal，另一个 goroutine 修改字段，输出可能不稳定，也可能触发 data race。正确做法是 request scope 内拥有对象，跨 goroutine 传递前冻结、复制或只读访问。

第四个问题是 map 字段。protobuf map 在逻辑上是 map，但 wire format 里会编码成 repeated entry。不同语言和不同实现的 map 遍历顺序可能不同。即使打开 deterministic serialization，也不要把它升级成跨语言 canonical 保证。高并发下如果拿 binary bytes 做 cache key 或 fingerprint，就会出现不稳定。

第五个问题是大消息和深嵌套。protobuf 的单个 string/bytes/message 上限可以很大，实际 runtime 也会有递归深度、内存和 message size 限制。攻击者或异常上游可以发一个超大 repeated 列表、超深嵌套 message 或巨型 bytes 字段，让解析器占用大量内存和 CPU。

第六个问题是 unknown fields 堆积。旧服务转发新字段时可能保留 unknown fields。这个特性对兼容性有用，但在代理、重试、落库、再发布链路里，unknown fields 可能让消息体变大，也可能让下游携带了自己没有理解的数据。是否保留、清理或记录 unknown fields，要按协议设计决定。

第七个问题是 schema registry、descriptor pool、反射和动态 message 的锁竞争。热路径如果频繁通过 descriptor 做动态解析，或者每次请求查远程 schema，protobuf 的优势会被抵消。生成代码路径和动态反射路径的性能差异很大。

第八个问题是日志和可观测性。protobuf binary 不适合直接打印。团队常把消息转 ProtoJSON 进日志，结果把 CPU 成本和 PII 风险都带进高并发路径。更好的做法是结构化记录 type、schema version、size、digest、关键非敏感字段和 decode error。

工程上我会这样控制：

```text
尽量减少重复 marshal/unmarshal；
不要跨请求复用未 reset 的 message；
并发前明确对象所有权，避免边 marshal 边修改；
不要把 protobuf bytes 当 canonical cache key；
限制 message size、嵌套深度、repeated 长度和 bytes 长度；
热路径优先 generated code，少用动态反射；
日志只打必要字段、size、digest 和 schema 信息；
用 CPU/heap/mutex/race 工具压测真实 payload。
```

## Q045. protobuf 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？
**回答：**

protobuf 的边界在正常请求里不一定明显，一到崩溃、重启、超时和重试，就会集中暴露。原因是 protobuf 不只是编码格式，它经常承载幂等请求、事件、长期存储记录和跨版本消息。

第一，重试会暴露幂等语义。客户端发送一个 protobuf 请求，服务端完成了副作用但响应超时，客户端重试。如果服务端的幂等 key 是直接 hash protobuf bytes，就要小心：protobuf bytes 不保证 canonical。不同语言、不同字段顺序、map 编码、unknown fields 都可能让 bytes 变化。幂等 key 应该基于明确业务字段，或者基于额外定义的 canonical 语义对象。

第二，崩溃会暴露“消息是否已经持久化”的状态边界。比如消费者解析 protobuf 后写数据库，写到一半崩溃。重启后 broker 重投消息，如果消费者没有用 message id、业务 id 或 inbox 表去重，就可能重复执行。protobuf 本身不会提供 exactly-once，最多只是把事件结构表达清楚。

第三，重启会暴露 schema 版本漂移。新版本服务可能用新的 `.proto`，旧消息里有 unknown fields，或者旧字段被新代码当成 deprecated。只要字段号保留得好，binary 兼容通常能工作；但如果有人重用字段号、改变 wire-unsafe 类型、把字段移进已有 oneof，就可能解析成错误语义。

第四，超时会暴露大消息解析成本。上游发来超大 protobuf，服务在解析阶段耗时过长，业务层 deadline 已经过了。比较稳的做法是先在传输层限制 body size，再在 protobuf parser 配置 recursion limit/message size limit，业务处理也要尊重 deadline 或 cancellation。

第五，重试和代理会暴露 unknown fields 保留策略。旧服务收到新字段后，如果只是 parse 再 serialize，可能会保留 unknown fields；如果转 ProtoJSON 或映射成业务 DTO，unknown fields 可能丢失。对于“代理必须无损转发”的协议，这会成为兼容性问题；对于“旧服务不应转发自己不理解的新能力”的协议，保留 unknown fields 又可能带来风险。

第六，崩溃恢复会暴露 writer schema 记录问题。长期存储 protobuf binary 时，如果没有保存 message type、schema version、descriptor set 或 schema registry 引用，几年后数据还在，但没人知道怎么解。字段号能解出 wire 类型，却不能恢复字段名和业务语义。

第七，重试会暴露默认值和 presence 语义。proto3 里标量字段默认值和“没设置”在很多情况下不容易区分，optional 字段、wrapper type、oneof 又有不同 presence 语义。请求重放、补偿或审计时，如果业务把“没传”和“传了默认值”当不同含义，就要在 schema 里明确表达。

面试里可以这样说：

```text
protobuf 在 crash/retry 场景里暴露的是幂等、schema 版本、unknown fields、默认值 presence 和持久化边界。不要直接 hash protobuf bytes 当幂等 key；要记录 message type、schema version、业务 id、payload digest 和处理状态。字段号不能重用，wire-unsafe 变更不能灰度混跑，大消息解析也要受 deadline 和 size limit 约束。
```

## Q046. protobuf 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？
**回答：**

protobuf 的单次编解码通常很快，但真实系统里的瓶颈不一定在 wire format 本身。一般要按路径拆：parse、marshal、对象分配、反射、压缩、I/O、网络、日志、schema lookup。

CPU 瓶颈常见在这些地方：

```text
varint 编解码:
  小整数很省空间，但需要逐字节判断 continuation bit。大量整数或 packed repeated 字段会吃 CPU。

大 repeated/map 字段:
  循环解析、扩容、排序或 deterministic 输出都会增加成本。

动态反射:
  DynamicMessage、descriptor lookup、通用转换器比 generated code 慢很多。

ProtoJSON 转换:
  JSON 字段名、base64 bytes、int64 字符串、enum 名称、默认值策略都会增加 CPU。

压缩:
  protobuf 自身不是压缩算法。如果再 gzip/zstd，CPU 可能主要花在压缩层。
```

内存瓶颈常见在大消息和重复转换。解析 protobuf 会分配 message 对象、repeated 列表、map、string、bytes。`bytes` 字段如果被复制多次，内存峰值会很高。高并发下，短命对象会推高 GC。对象池可以减少分配，但 reset 不干净会引入脏数据，比多几次分配更危险。

锁竞争不是 protobuf 规范带来的，但实现里很常见。比如全局 descriptor pool、schema registry client、反射缓存、对象池、日志 encoder、metrics 聚合、压缩器池，都可能在高并发下变成锁热点。单线程 benchmark 看不出来，必须看 mutex profile 或阻塞分析。

I/O 瓶颈通常发生在消息外层：从网络读请求体、从磁盘读写事件、从对象存储加载大 payload、写 WAL、写审计日志。protobuf 把 payload 变小后可能减少网络和磁盘压力，但如果消息本身很大，I/O 仍然可能是主成本。

网络瓶颈取决于场景。跨机 RPC 如果带宽小、延迟高、message 大，protobuf 的紧凑性有帮助；但在数据中心内的小 RPC，网络 RTT、队列排队、TLS、负载均衡、下游处理时间可能比编解码更重要。不要把“使用 protobuf”当成端到端性能优化的充分条件。

比较稳的 benchmark 会拆开看：

```text
marshal only；
unmarshal only；
unmarshal + business mapping；
protobuf binary vs ProtoJSON；
generated code vs reflection；
with compression vs without compression；
small/medium/large message；
single-thread vs high concurrency；
p50/p95/p99、allocs/op、bytes/op、GC、CPU profile、mutex profile。
```

面试里可以这样答：

```text
protobuf 的编码本身通常不是最慢的一段。瓶颈常来自大 repeated/map、动态反射、ProtoJSON 转换、重复 marshal/unmarshal、对象分配和压缩。I/O 和网络可能因为 payload 大小成为主成本，但要通过 profile 拆开看。优化时优先减少转换次数、走 generated code、限制大字段、避免热路径反射和过度日志化。
```

## Q047. protobuf 的 correctness test、stress test 和 benchmark 应该分别测什么？
**回答：**

protobuf 的测试要分层。correctness test 测协议语义和兼容性；stress test 测极端输入和资源边界；benchmark 测真实负载下的延迟、吞吐和分配。只写几个 marshal/unmarshal round-trip 远远不够。

correctness test 应该覆盖：

```text
round-trip:
  构造 message，marshal 后 unmarshal，字段值一致。

golden bytes:
  关键消息保存二进制 golden，防止字段号、wire type、packed 设置被误改。

schema evolution:
  新 writer + 旧 reader，旧 writer + 新 reader 都要测。

unknown fields:
  旧 reader 是否跳过或保留新字段，转发链路是否按协议要求处理。

field presence:
  未设置、设置为默认值、optional、oneof、wrapper type 的语义要测清楚。

map/repeated:
  顺序、重复值、空列表、packed/unpacked 兼容性要测。

enum:
  unknown enum value、默认 enum、重命名 enum name 对 ProtoJSON 的影响要测。

ProtoJSON:
  int64 字符串、bytes base64、enum name、字段名 camelCase/json_name、unknown field 处理。

compatibility rules:
  禁止重用字段号，删除字段后 reserved，wire-unsafe 变更能被测试拦住。
```

跨语言 correctness test 很重要。至少让一门语言生成 bytes，另一门语言解析；再反过来。最好还跑 protobuf 官方 conformance 思路：同一批测试向量，验证每个语言实现对 binary、JSON、非法输入和边界值的行为一致。

stress test 不追求所有输入成功，而是要求失败方式可控。可以测：

```text
超大 message；
超大 string/bytes；
极深嵌套 message；
巨大 repeated/map；
大量 unknown fields；
非法 wire type；
截断 varint；
长度前缀超过实际输入；
递归 group 或旧格式边界；
随机 bytes fuzz；
并发解析和序列化；
deadline/cancellation；
解压后 protobuf 炸弹。
```

正确结果可能是 parse error、resource exhausted、deadline exceeded，而不是服务 OOM、线程卡死或 panic。

benchmark 要用真实业务 payload。小 message、中等 message、大 message要分开；字段分布也要真实，比如 repeated 数量、map 大小、bytes 字段大小、嵌套层数。指标上至少看：

```text
marshal ns/op；
unmarshal ns/op；
allocs/op；
bytes/op；
throughput；
p50/p95/p99；
GC；
CPU profile；
heap profile；
generated code vs reflection；
binary protobuf vs ProtoJSON vs JSON；
with compression vs without compression。
```

面试里可以这样说：

```text
correctness test 看 round-trip、golden bytes、schema evolution、unknown fields、presence、ProtoJSON 和跨语言兼容；stress test 看大消息、深嵌套、非法 wire、fuzz、资源上限和并发；benchmark 看真实 payload 下 marshal/unmarshal、分配、p99、generated code 与 reflection、binary 与 JSON/ProtoJSON 的差距。
```

## Q048. 如果要求从零实现一个简化版 protobuf，你会先定义哪些不变量？
**回答：**

从零实现简化版 protobuf，不能先写代码生成器，应该先定义协议不变量。protobuf 的价值来自“schema 和 wire format 长期稳定”，不是来自某个具体 API。

我会先定义 schema 不变量：

```text
message:
  一个 message 由字段集合组成。

field number:
  每个字段有唯一正整数编号；编号一旦发布不能复用。

field name:
  字段名给代码和文档使用，不进入 binary wire 语义。

field type:
  只支持一小组类型，比如 int32、int64、bool、string、bytes、message、repeated。

reserved:
  删除字段后必须 reserved 字段号和必要的字段名。

required:
  简化版最好不支持 required，避免演进困难。
```

然后定义 wire format 不变量。可以参考 protobuf 的 Tag-Length-Value 思路：

```text
tag:
  tag = field_number << 3 | wire_type。

wire type:
  支持 varint、fixed32、fixed64、length-delimited。

varint:
  使用低 7 位数据 + 高位 continuation bit。

length-delimited:
  先写长度 varint，再写 bytes；string、bytes、嵌套 message、packed repeated 都走这个形式。

unknown field:
  reader 遇到未知 field number，必须能按 wire type 跳过。
```

再定义类型语义：

```text
string:
  必须是合法 UTF-8。

bytes:
  任意字节，不做 UTF-8 校验。

int32/int64:
  明确 varint 编码和符号数策略。如果要支持负数高效编码，需要定义 ZigZag。

repeated:
  保留元素顺序；数值 repeated 可以 packed，也要定义 packed/unpacked 兼容。

message:
  嵌套 message 的长度边界必须明确。
```

还要定义解析不变量：

```text
同一个非 repeated 字段出现多次:
  后出现的值覆盖前值，或直接拒绝。必须明确。

repeated 字段出现多次:
  按出现顺序追加。

unknown fields:
  要么保留，要么丢弃，但行为必须写进版本。

缺失字段:
  返回默认值还是 presence=false，必须明确。

非法 wire:
  截断 varint、长度越界、wire type 不匹配都要确定性失败。
```

资源不变量也不能省：

```text
max message size；
max recursion depth；
max repeated length；
max bytes/string length；
max unknown fields size；
timeout/cancellation；
memory allocation cap。
```

最后定义演进不变量：

```text
新字段只能用新 field number；
不能改变已发布字段的 wire-incompatible 类型；
不能重用删除字段号；
enum 值要保留历史编号；
oneof 迁移要谨慎；
golden bytes 和跨语言测试必须随 schema 一起维护。
```

面试里可以这样答：

```text
我会先定义 field number、wire type、tag 编码、varint、length-delimited、string/bytes 区分、repeated 顺序、unknown field 跳过策略、默认值/presence、非法输入错误语义、资源上限和 schema evolution 规则。简化版 protobuf 的核心不变量是：字段号定义 wire 语义，未知字段可跳过，已发布字段号不能复用。
```

## Q049. protobuf 的常见误用是什么，误用后通常会产生什么线上症状？
**回答：**

protobuf 的误用通常不是“不会写 .proto”，而是把它当成一种普通对象序列化工具，忽略字段号、兼容性、presence、unknown fields 和调试工具。上线后问题往往表现为解析失败、数据丢失、幂等失效、跨语言不一致，或者排障成本暴涨。

常见误用有这些。

```text
重用字段号:
  老数据或老服务会把新字段解释成旧语义。
  症状是字段值看起来合法但业务完全错，最难排查。

删除字段不 reserved:
  后来有人重新使用同一个编号。
  症状是历史数据重放、冷数据迁移或灰度混跑时解析错。

随意改字段类型:
  有些类型 wire-compatible，有些 wire-unsafe。
  症状是新旧版本互相读不懂，或者读懂但值被截断、符号错误。

把 protobuf bytes 当 canonical fingerprint:
  map 顺序、unknown fields、实现版本可能影响输出 bytes。
  症状是幂等 key 漂移、缓存 miss、签名偶发失败。

滥用 string 存二进制:
  string 要求 UTF-8，bytes 才是任意字节。
  症状是解析失败、日志乱码、ProtoJSON 转换异常。

滥用 Any:
  类型分发变成运行时猜测，schema 契约弱化。
  症状是消费者不知道要链接哪些类型，审计和兼容检查失效。

忽略 proto3 presence:
  没区分未设置和设置为默认值。
  症状是 PATCH、配置覆盖、审计重放时默认值语义错。

把 message 当数据库表:
  传输模型和存储模型绑死。
  症状是加字段困难，索引和查询需求反向污染 API。

直接记录完整 payload:
  protobuf 不等于脱敏。
  症状是日志里泄漏 PII、token 或内部策略。

没有 schema registry / descriptor 管理:
  binary payload 留下来却没人能解。
  症状是历史数据无法排障，跨团队消费要靠猜字段号。
```

还有一个很常见的误用是“觉得 protobuf 快，所以消息可以无限大”。protobuf 对大 bytes、大 repeated、大嵌套 message 并没有魔法。大消息会增加内存峰值、网络排队、GC、重试成本和尾延迟。线上症状是单个请求拖慢 worker，队列堆积，甚至触发 OOM。

面试里可以这样总结：

```text
protobuf 的常见误用是重用字段号、删除字段不 reserved、随意改类型、把 bytes 当 canonical、string/bytes 混用、滥用 Any、忽略 presence、没有 schema 管理、热路径转 ProtoJSON 日志。症状通常是新旧版本不兼容、历史数据解析错、幂等和签名漂移、日志泄密、p99 和内存异常。
```

## Q050. protobuf 在单机和分布式环境中的语义有什么差异？
**回答：**

单机里，protobuf 更像一个高效的本地编解码工具：同一份 `.proto`，同一个 runtime，marshal 后再 unmarshal，字段能回来，性能也不错。分布式环境里，protobuf 变成服务之间、语言之间、版本之间的长期契约。这个差异很大。

单机场景关注的是本地对象语义：

```text
生成代码是否正确；
marshal/unmarshal 是否 round-trip；
对象是否被并发修改；
默认值和 presence 在本语言里如何表现；
性能和分配是否可接受。
```

这些问题通常可以通过单元测试和基准测试发现。

分布式环境关注的是协议语义：

```text
字段号能不能长期保留；
旧 reader 能不能读新 writer；
新 reader 能不能读旧 writer；
unknown fields 是保留、丢弃还是禁止转发；
ProtoJSON 和 binary 的规则是否被混用；
不同语言 runtime 对默认值、map、enum、bytes 的处理是否一致；
schema registry、descriptor 和 message type 如何分发；
灰度发布期间新旧 schema 是否能共存。
```

最重要的差异是字段号变成长期资产。单机里你改字段号，测试一起改，也许马上通过；分布式里旧服务、旧数据、旧消息、旧客户端还在。字段号一旦发布，就不能随便改，更不能重用。删除字段也要 reserved。

第二个差异是 schema evolution 要考虑全链路。生产者、消费者、存储、日志、重放工具、离线任务、回放测试，可能不会同时升级。一个新字段在新服务里很自然，到了旧消费者那里就是 unknown field；这个 unknown field 是否保留，会影响后续链路。

第三个差异是 binary 和 ProtoJSON 的兼容规则不同。binary protobuf 依赖字段号，ProtoJSON 更依赖字段名、enum 名、json_name 和默认值输出策略。一个 binary wire-safe 的改动，未必对 ProtoJSON 安全。对外 HTTP API 如果暴露 ProtoJSON，要单独看兼容性。

第四个差异是语义不能靠本语言默认行为。JavaScript 对 int64 的处理、Go 的 presence 表示、Java 的 builder、Python 的动态性、C++ 的 arena，都可能让业务代码写法不同。分布式系统要靠 golden test、conformance test、schema lint、breaking change 检查来兜底。

第五个差异是可观测性要求更高。单机调试可以直接打印对象；分布式排障要知道 message type、schema version、producer version、consumer version、payload size、digest、decode error。没有这些元数据，protobuf binary 在日志里只是一串不可读 bytes。

第六个差异是故障会被重试放大。网络超时、broker 重投、消费者重启、灰度回滚都会让同一份 protobuf 消息经过不同版本代码。协议设计如果只在“当前版本当前语言”成立，分布式环境很快会出问题。

面试里可以这样说：

```text
单机里 protobuf 是高效编解码工具；分布式里 protobuf 是长期协议契约。字段号、wire type、unknown fields、presence、schema version、ProtoJSON 差异、跨语言 runtime 行为都要显式管理。单机 round-trip 通过不代表分布式兼容，真正要看新旧版本互读、跨语言 golden test、breaking change 检查和灰度期间的行为。
```

## Q051. Avro 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？
**回答：**

Avro 的核心目标，是在“记录型数据”和“数据管道”里提供一种带 schema evolution 能力的序列化格式。它和 protobuf 一样是结构化数据序列化系统，但思路不太一样：protobuf 把字段号写进 wire format，Avro 的 binary data 通常不带字段名和字段类型，解释数据时依赖 writer schema。官方规范也把 writer schema 和 reader schema 的解析规则放在很核心的位置。

如果要在正确性、性能、安全性、可维护性里选主轴，我会说 Avro 主要解决的是数据演进正确性和数据管道可维护性，同时兼顾存储与传输性能。它不是安全机制。

Avro 很适合 record data。官网也直接把它定位为 record data 的序列化格式和 streaming data pipelines 的常用选择。它的设计里有几个明显目标：

```text
schema 与数据绑定:
  Object Container File 会把 schema 放进文件元数据；single-object encoding 会带 schema fingerprint。

reader/writer schema resolution:
  读数据时可以用不同于写数据时的 schema，只要二者满足兼容规则。

紧凑 binary encoding:
  record 字段按 writer schema 的声明顺序写出，不重复写字段名。

动态数据处理友好:
  schema 是 JSON 表达，GenericRecord、数据湖、流处理、schema registry 都容易围绕它工作。

跨语言:
  JVM、Python、C/C++、C#、Rust、JavaScript 等生态都有实现。
```

正确性来自 schema resolution。比如旧数据没有新字段，新 reader 可以用字段默认值补上；writer 有字段但 reader 不关心，reader 可以跳过；字段改名可以通过 aliases 做映射。这个模型对长期数据很有用，因为数据文件、Kafka 事件、离线任务和在线服务经常不在同一天升级。

性能来自二进制编码和批量文件格式。Avro binary 不写字段名，整数有紧凑编码，Object Container File 按 block 存储，还可以配 codec 压缩，适合 HDFS、对象存储、批处理和流处理落盘。

可维护性来自 schema 作为契约。相比每个消费者手写 JSON 解析，Avro 让字段、默认值、union、logical type、aliases、文档、兼容性检查都有地方可放。它特别适合多团队数据平台，因为“这条事件长什么样”不再只存在于某个服务代码里。

安全性不是 Avro 的核心目标。Avro 不负责认证、授权、加密、签名、防重放，也不自动限制不可信输入的大小和嵌套深度。binary payload 不容易肉眼阅读，不等于安全。线上仍然要靠 TLS、ACL、schema registry 权限、消息签名、大小限制和字段级脱敏。

面试里可以这样说：

```text
Avro 的核心目标是面向记录型数据的数据交换和长期 schema evolution。它主要解决数据管道里的兼容性、可维护性和存储传输效率问题；正确性来自 writer schema 与 reader schema 的解析规则。它不是安全协议，也不是 canonical payload fingerprint 格式。
```

## Q052. Avro 的典型适用场景和不适用场景分别是什么？
**回答：**

Avro 最适合数据管道，不是最适合所有 RPC。它的强项是“数据写出去以后，几年后还能用新 schema 读回来”，以及“不同消费者可以按自己的 reader schema 读同一批数据”。

典型适用场景有这些：

```text
Kafka / Pulsar / 日志事件:
  事件结构随业务演进，生产者和消费者不一定同步升级。Avro 配 schema registry 很常见。

数据湖和离线批处理:
  Object Container File 自带 schema 和 block，适合落到 HDFS、对象存储，再被 Spark、Flink、Hive 类系统消费。

CDC 和事实表记录:
  记录型数据字段会增加、废弃、改名，Avro 的 defaults、aliases、schema resolution 有用。

跨语言数据平台:
  Java 生产、Python 训练、Rust/Go 服务消费，都可以围绕同一个 schema 读写。

长期存储:
  只要保存 writer schema 或 schema id，旧数据可以被未来 reader 解析。

动态 ETL:
  GenericRecord 和 JSON schema 表达让数据平台可以不提前生成每个业务类型的代码。
```

不适用场景也要讲清楚。

第一，不适合没有 schema 管理的随意 payload。Avro binary 没有 schema 基本不可解释。只保存一串 bytes，不保存 writer schema、schema id 或 OCF header，后面很可能读不回来。

第二，不适合必须人手直接读写的配置文件。Avro schema 是 JSON，但 Avro binary data 不是给人编辑的。把 Avro 当配置语言会让排障变麻烦。

第三，不适合极低延迟的小 RPC 默认选择。Avro 有 RPC 规范，但今天工程上内部 RPC 更常见的是 gRPC/protobuf、HTTP/JSON 或 Thrift。Avro 在数据文件和流式事件里更常见。小 RPC 如果每次都查 schema、做 resolving decoder、走 GenericRecord，反而会拖慢路径。

第四，不适合把原始 bytes 当 canonical fingerprint 或签名输入。Avro 有 schema parsing canonical form 和 schema fingerprint，但那是 schema 层面的规范化，不是业务 record 的 canonical data bytes。record 的 map 顺序、block 切分、codec、OCF header、schema 表达差异都可能影响原始 bytes。

第五，不适合类型极度动态、字段完全不可预期的场景。如果业务本质是任意 JSON 文档，Avro 可以用 map、union 包一层，但 schema 会变得难读，兼容性检查也失去意义。

第六，不适合不受限的不可信输入。大 bytes、大 array、大 map、深层 record、恶意 block size、压缩后膨胀，都可能造成内存和 CPU 问题。Avro 是序列化格式，不是输入防火墙。

面试里可以这样回答：

```text
Avro 适合数据管道、事件流、数据湖、CDC、长期存储和跨语言批处理；不适合没有 schema 管理的裸 bytes、人手编辑配置、极低延迟小 RPC、任意动态 JSON、以及需要 canonical payload 签名的场景。用 Avro 的前提是 writer schema 能被保存、查到并参与 reader schema resolution。
```

## Q053. Avro 和相近概念最容易混淆的边界在哪里？
**回答：**

Avro 最容易和 protobuf、JSON、Parquet、schema registry、canonical schema、OCF、Kafka 消息格式混在一起。边界说清楚后，就不容易把 Avro 用错地方。

第一，Avro 不是 protobuf 的另一种字段号编码。protobuf binary 里每个字段带 field number 和 wire type；Avro record 的 binary 编码按 writer schema 的字段顺序写值，字段名和字段类型不在每条 record 里重复出现。Avro 读数据必须知道 writer schema。这个差异非常关键。

第二，Avro schema 是 JSON，不代表 Avro data 就是 JSON。Avro 支持 JSON encoding，但生产系统里常说的 Avro 多数指 binary encoding。把 Avro schema 文件看成 JSON 文档没问题，但不要把 Avro binary 当成可直接 grep 的 JSON 日志。

第三，Avro 不是 Parquet。Avro 更偏 row-oriented record serialization，适合事件、消息、行式记录和 schema evolution；Parquet 是列式存储，适合分析查询、列裁剪和压缩。数据湖里两者经常共存：流入时 Avro，分析层转 Parquet。

第四，schema registry 不是 Avro 本身。Avro 规范定义 schema、encoding、OCF、schema resolution 等；schema registry 是工程系统，用来登记 schema、分配 id、做兼容性检查、给消费者查 writer schema。没有 registry 也能用 Avro OCF；有 registry 也不代表 schema 设计就自动正确。

第五，Avro 的 parsing canonical form 和 fingerprint 主要是 schema 的 canonical form，不是 payload 的 canonical JSON。它可以帮助识别“这个 schema 是哪个 schema”，不能自动告诉你两个业务 record 是否语义相同。

第六，Object Container File 和 single-object encoding 不是一回事。OCF 是文件格式，header 里有 metadata、schema、codec，后面是 block 和 sync marker，适合文件存储和切分读取。single-object encoding 是单条对象的 wrapper，带 marker 和 schema fingerprint，适合 Kafka 这类一条消息一个对象的场景。

第七，Avro defaults 不是“写数据时可以省略字段”。规范里默认值主要用于 schema evolution：reader 读到 writer schema 里没有的字段时，用 reader schema 的 default 补。字段值等于 default 时，Avro 仍然会编码该字段。这一点特别容易被误解。

第八，logical type 不是新的物理类型。`timestamp-millis` 本质上还是 long，`decimal` 本质上是 bytes 或 fixed 上的注解。不同语言对 logical type 的映射可能不同；忽略 logical type 时，底层物理值仍然能读出来，但业务语义可能错。

面试里可以这样说：

```text
Avro 的关键边界是：binary record 依赖 writer schema，不像 protobuf 那样每个字段带字段号；schema 是 JSON 不等于数据是 JSON；schema fingerprint 不等于 payload fingerprint；OCF 是文件格式，single-object encoding 是单条对象包装；defaults 用于读旧数据，不是写入时自动省字段。
```

## Q054. Avro 在高并发场景下可能出现哪些隐藏问题？
**回答：**

Avro 在数据平台里很常见，但高并发服务里用不好也会出问题。问题通常不在“Avro 能不能编码”，而在 schema 查找、解析缓存、GenericRecord 分配、压缩、日志转换和对象复用。

第一个问题是 schema registry 变成热点。Kafka producer 或 consumer 如果每条消息都查 schema，或者缓存失效时大量并发请求打到 registry，会出现延迟尖峰。正确做法是本地缓存 schema id、fingerprint、writer schema 和 reader/writer resolving plan；缓存还要有版本和容量控制。

第二个问题是 resolving decoder 成本。Avro 的强项是 reader schema 可以不同于 writer schema，但这个解析不是免费的。字段按名字匹配、aliases、默认值、union 分支、类型提升，都需要计算。热路径里应该缓存 writer schema 到 reader schema 的 resolution 结果，而不是每条消息重新构建。

第三个问题是 GenericRecord 分配多。数据平台喜欢 GenericRecord，因为灵活；高并发服务里它可能制造大量 map、list、string、byte buffer 和装箱对象。SpecificRecord 或代码生成路径通常更快，也更少分配。不是所有路径都该用 generic。

第四个问题是 union 处理复杂。Avro union 在 schema 里是数组，默认值还要求匹配 union 的第一个分支。高并发下如果业务经常用大 union 表达动态类型，分支解析、类型判断和错误路径都会增加成本，线上也更难排查。

第五个问题是对象复用和线程安全。很多 Avro encoder、decoder、buffer、datum reader/writer 的具体实现不应该被多个线程随便共享。为了减少分配而复用对象，如果 reset 不彻底，上一条消息的数据可能污染下一条；如果并发使用同一个 encoder，输出会串。

第六个问题是 OCF block 和 codec。Object Container File 的 block 太大，会增加内存峰值和写入延迟；太小，又会降低压缩率并增加 sync marker 和 header 开销。高并发写文件时，还要考虑 flush、fsync、对象存储 multipart、失败重试和小文件问题。

第七个问题是 ProtoJSON 类似的日志转换成本。Avro binary 不好直接读，很多团队会把 Avro 转 JSON 打日志。高并发下这会带来 CPU、内存和 PII 风险。更稳的做法是记录 schema id、event type、payload size、digest、关键非敏感字段和 decode error。

第八个问题是大消息和压缩炸弹。Avro 支持 bytes、array、map、嵌套 record，再配合 deflate/snappy/zstd 这类 codec。解压后数据可能非常大，解析时会占用大量内存。入口要限制原始大小、解压后大小、block size、array/map 元素数量和嵌套深度。

工程上我会这样做：

```text
schema id 和 writer schema 本地缓存；
缓存 writer-reader resolution；
热路径优先 SpecificRecord 或生成代码；
限制 union 复杂度；
encoder/decoder 按线程或请求隔离使用；
限制 message、block、array、map、bytes 大小；
日志只记录元数据和脱敏字段；
压测 generic vs specific、with codec vs without codec。
```

## Q055. Avro 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？
**回答：**

Avro 的很多优势都和长期数据有关，所以 crash、restart、timeout、retry 场景尤其重要。这里最容易暴露的是 schema 可用性、写入原子性、block 完整性、幂等和兼容性。

第一，崩溃会暴露 schema 和数据是否一起持久化。OCF 文件把 schema 放在 header 里，这对文件很友好；但 Kafka 或普通消息里的 Avro binary 通常只带 schema id 或 fingerprint。服务崩溃重启后，如果 schema registry 查不到旧 schema，消息还在，消费者也读不出来。

第二，写文件时会暴露 block 完整性。OCF 是 header 加 block 加 sync marker 的结构。写到一半崩溃，最后一个 block 可能不完整。恢复时要能识别截断文件，丢弃不完整 block，或者通过临时文件加 rename 保证原子发布。对象存储上还要考虑 multipart upload 的提交边界。

第三，超时会暴露 schema registry 依赖。消费消息时先要拿 writer schema，再做 resolution。如果 registry 抖动或网络超时，业务处理还没开始就失败。线上一般要缓存 schema，并把“schema not found”“registry timeout”“decode error”“business error”分成不同错误。

第四，重试会暴露幂等语义。Avro binary bytes 不应该直接当业务幂等 key。OCF block 切分、codec、schema 表达、map 顺序、metadata 都可能影响 bytes。幂等 key 应该来自业务字段，或者来自额外定义的 canonical 语义对象。

第五，重启会暴露 schema evolution 漂移。新代码用新 reader schema 读旧数据，如果新字段没有 default，读旧数据会失败；enum 新增符号但 reader 没有 default，也可能失败；decimal 的 precision/scale 不匹配会解析错误。灰度期间，新旧 producer 和 consumer 混跑，兼容性检查必须提前做。

第六，重试会暴露 defaults 的误解。很多人以为 Avro default 意味着 writer 可以不写字段。实际上 default 主要用于 reader 读不到字段时补值。生产者如果没有按 writer schema 写完整字段，消息本身就是非法的。

第七，崩溃恢复会暴露 logical type 的环境依赖。timestamp 是全局 instant，local timestamp 是本地时间语义；decimal 依赖 precision/scale。重启后时区、语言 runtime 或逻辑类型映射不同，业务展示和比较可能变掉。底层 long 还在，但解释变了。

第八，消息重放会暴露保留策略。数据管道经常要 replay。只要保留 raw Avro payload，还要保留 writer schema 或 schema id 对应关系。否则历史消息能读 bytes，却无法恢复字段名、默认值、aliases 和 logical type。

面试里可以这样说：

```text
Avro 在 crash/retry 场景里最怕 schema 丢失、半写 block、registry 超时、reader schema 不兼容和幂等 key 选错。要保存 writer schema 或 schema id，缓存 schema，区分 decode error 和业务 error，OCF 写入要有原子边界，重试幂等要基于业务字段而不是裸 Avro bytes。
```

## Q056. Avro 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？
**回答：**

Avro 的性能瓶颈取决于用法。如果是 OCF 大文件批处理，I/O、压缩和 block size 很重要；如果是在线高并发消息，CPU、内存分配、schema resolution 和 registry/cache 锁竞争更明显。

CPU 成本主要来自这些地方：

```text
binary encode/decode:
  int/long 的变长编码、字符串 UTF-8、array/map block、record 字段按 schema 遍历。

schema resolution:
  writer schema 和 reader schema 不同时，要匹配字段名、处理 aliases、默认值、union、类型提升。

GenericRecord:
  动态字段访问、装箱拆箱、map/list 构造，比生成代码更重。

JSON encoding:
  Avro JSON encoding 比 binary 更慢，也更容易受字段名、union 表达和转义影响。

compression:
  deflate、snappy、zstd 等 codec 可能成为主 CPU 成本。
```

内存成本来自对象模型和 block。GenericRecord 会分配很多小对象；大 bytes 字段可能复制多次；array/map 会扩容；OCF block 解压后可能一次性占用大内存。高并发下这些短命对象会推高 GC。

锁竞争一般来自工程实现：schema registry client、schema cache、resolver cache、全局 encoder/decoder 池、压缩器池、metrics 聚合、日志转换器。单线程 benchmark 可能很好，高并发下 p99 被锁拖住。要看 mutex profile 或阻塞分析。

I/O 成本在 Avro 文件场景里很常见。OCF block 需要写磁盘、HDFS、对象存储；block size、flush 频率、codec、sync marker、文件数量都会影响吞吐。小文件太多会拖慢元数据系统；block 太大又会拖慢失败恢复和内存。

网络成本主要来自两类：一类是 payload 在网络上传输，Avro binary 可以减少带宽；另一类是 schema registry、远程对象存储、RPC handshake 或下游写入。registry 访问如果没有缓存，会把本地编码问题变成网络延迟问题。

我会这样拆 benchmark：

```text
binary encode only；
binary decode only；
writer schema == reader schema；
writer schema != reader schema；
GenericRecord vs SpecificRecord；
without codec vs with codec；
small event vs large batch block；
registry cache hit vs cache miss；
OCF read/write throughput；
p50/p95/p99、allocs/op、bytes/op、GC、CPU、heap、mutex。
```

面试里可以说：

```text
Avro 的瓶颈常见在 schema resolution、GenericRecord 分配、压缩 codec、大 block I/O 和 schema registry/cache。在线路径看 CPU、内存和锁；离线文件路径看 I/O、压缩和 block size。不能只说 Avro 快，要按 binary/JSON、generic/specific、cache hit/miss、with/without codec 分开测。
```

## Q057. Avro 的 correctness test、stress test 和 benchmark 应该分别测什么？
**回答：**

Avro 的测试重点和 protobuf 不一样。protobuf 更强调字段号和 wire compatibility；Avro 更强调 writer schema、reader schema、schema resolution、defaults、aliases、logical type 和容器格式。

correctness test 首先要测 round-trip：

```text
用 writer schema 写 record；
用同一个 schema 读回来；
字段值、array 顺序、map 内容、bytes、fixed、enum、union、logical type 都一致。
```

然后要测 schema evolution：

```text
旧 writer + 新 reader:
  新增字段有 default 时能读；没有 default 时失败。

新 writer + 旧 reader:
  旧 reader 不认识的新字段会被忽略。

字段改名:
  aliases 配置正确时能解析。

字段重排:
  writer schema 保留时，reader 按字段名解析，不应该因为顺序变化读错。

类型提升:
  int 到 long/float/double 等允许路径要测；不允许路径要失败。

enum:
  writer 的 symbol 不在 reader 中时，有 default 才能兜底。

union:
  null 分支、默认值第一分支、多个 record 名称分支都要测。

logical type:
  decimal precision/scale、timestamp/local-timestamp、date、uuid 要测跨语言一致性。
```

还要测容器和 schema 标识：

```text
OCF header 是否写入正确 schema、codec、sync marker；
block count/size 是否正确；
压缩 codec 是否能被目标 reader 读取；
single-object encoding 的 schema fingerprint 是否能查到 writer schema；
schema parsing canonical form 是否稳定。
```

stress test 看异常输入和资源边界：

```text
超大 bytes/string；
超大 array/map；
极深嵌套 record；
巨大 union；
非法 schema；
schema id 查不到；
截断 varint；
长度前缀超过输入；
OCF 最后一个 block 截断；
压缩后膨胀；
随机 bytes fuzz；
并发 encode/decode；
registry timeout；
reader schema 与 writer schema 不兼容。
```

正确结果可能是明确 decode error、schema resolution error、resource exhausted 或 timeout，而不是 OOM、panic、线程卡死。

benchmark 要贴近业务数据分布。指标至少包括：

```text
encode ns/op；
decode ns/op；
allocs/op；
bytes/op；
throughput；
p50/p95/p99；
GC；
CPU/heap/mutex profile；
GenericRecord vs SpecificRecord；
binary vs JSON encoding；
reader schema same vs evolved；
with codec vs without codec；
OCF block size 曲线；
schema cache hit vs miss。
```

面试里可以这样答：

```text
Avro correctness test 要重点测 writer/reader schema resolution、defaults、aliases、union、logical type、OCF 和 single-object encoding；stress test 看大数组、大 map、深嵌套、截断 block、压缩膨胀、schema registry 失败和不兼容 schema；benchmark 分开测 generic/specific、binary/JSON、cache hit/miss、with/without codec 和 OCF block size。
```

## Q058. 如果要求从零实现一个简化版 Avro，你会先定义哪些不变量？
**回答：**

从零实现简化版 Avro，第一步不是写 encoder，而是定义 schema、writer/reader resolution 和 binary encoding 的不变量。Avro 的核心不在“把对象转 bytes”，而在“数据和 schema 如何长期一起演进”。

我会先定义 schema 不变量：

```text
schema format:
  用 JSON 表达 schema。

primitive:
  支持 null、boolean、int、long、float、double、bytes、string。

record:
  record 有 fullname；字段名在 record 内唯一；字段按声明顺序编码。

array/map:
  array 有 items schema；map 的 key 固定为 string，value 有 schema。

enum/fixed:
  enum symbol 唯一；fixed size 固定。

union:
  union 是 schema 数组，不允许直接嵌套 union；默认值必须匹配第一个分支。

metadata:
  自定义属性允许存在，但不能影响二进制编码。
```

然后定义编码不变量：

```text
binary data 不写字段名；
record 按 writer schema 字段顺序写值；
string 按 UTF-8 bytes 写长度和值；
bytes 写长度和值；
int/long 用变长编码；
array/map 用 block 表达，并能结束；
union 先写分支 index，再写该分支的值；
fixed 写固定长度 bytes。
```

接着定义 schema resolution 不变量：

```text
reader 必须能拿到 writer schema；
record 字段按 name 匹配，不按位置匹配；
writer 有、reader 没有的字段被忽略；
reader 有、writer 没有的字段必须有 default，否则失败；
aliases 可以把旧名字映射到新名字；
允许的类型提升必须列白名单；
enum symbol 缺失时必须有 reader default 才能成功；
logical type 参与兼容性判断时规则要明确。
```

再定义容器不变量：

```text
单条消息:
  要么随消息带完整 writer schema，要么带 schema fingerprint/schema id 并能查到 schema。

文件:
  header 必须包含 writer schema、codec、metadata；
  block 有 count 和 size；
  sync marker 用于恢复和切分。

schema fingerprint:
  基于 schema parsing canonical form，而不是原始 schema JSON 文本。
```

资源边界也要先写：

```text
max schema size；
max message size；
max block size；
max array/map length；
max string/bytes length；
max nesting depth；
max union branches；
max decompressed size；
timeout/cancellation。
```

最后定义测试不变量：

```text
same schema round-trip；
old writer + new reader；
new writer + old reader；
aliases rename；
default injection；
incompatible schema fails deterministically；
OCF truncated block can be detected；
cross-language golden data bytes。
```

面试里可以这样说：

```text
简化版 Avro 要先定义 schema JSON、record 字段顺序编码、writer schema 必须可得、reader schema 按字段名解析、defaults/aliases/type promotion/union 的兼容规则、OCF 或单对象 schema 标识、资源上限和跨语言 golden test。Avro 的核心不变量是数据不能脱离 writer schema 被正确解释。
```

## Q059. Avro 的常见误用是什么，误用后通常会产生什么线上症状？
**回答：**

Avro 的误用经常出现在 schema evolution 上。很多问题不是马上报错，而是在重放历史数据、灰度发布、多消费者读取、离线任务跑旧分区时才爆出来。

常见误用可以按症状记。

```text
只保存 Avro binary，不保存 writer schema 或 schema id:
  症状是历史数据还在，但没人能解码。

新增字段没有 default:
  旧数据被新 reader 读取时失败。

误解 default:
  以为 default 表示写入时可以省略字段。
  症状是 producer 写出非法 record，consumer decode 失败。

随意改字段名，不加 alias:
  症状是新 reader 读旧字段时认为字段缺失，只能用 default 或直接失败。

随意改字段类型:
  症状是 schema resolution 失败，或者数值被提升后精度/语义变化。

union 设计混乱:
  null 不在第一位但 default 写 null，或者 union 分支太多。
  症状是默认值校验失败、跨语言 JSON 表达不一致、错误信息难懂。

enum 删除或重命名 symbol:
  症状是旧数据里的 symbol 新 reader 不认识；没有 enum default 就读失败。

decimal precision/scale 改动:
  症状是金额字段解析失败，或不同语言四舍五入不一致。

把 schema fingerprint 当 payload fingerprint:
  症状是不同数据有同一个 schema fingerprint，被错误去重。

把 OCF 文件切坏:
  症状是最后 block 读失败，或者分布式任务只能读一部分。

把 Avro 当安全边界:
  症状是未授权消费者能读敏感字段，日志泄漏 PII。
```

还有一个很常见的误用是把 Avro schema 当普通 JSON 随手格式化、改 namespace、改 fullname。Avro 的 named type 依赖 fullname，namespace 不是装饰。改错后，schema resolution 会把它当成不同类型。线上表现是同一个字段看起来还叫同一个名字，但 reader 和 writer 认为类型不匹配。

另一个误用是滥用 map 表达所有东西。map key 是 string，value 同一个 schema；如果业务字段本来有明确含义，用 map 会绕过 schema 的字段级演进和文档。短期灵活，长期排障困难。

面试里可以这样总结：

```text
Avro 常见误用是丢 writer schema、新字段没 default、改名没 alias、改类型不看 resolution、union/default 写错、enum symbol 随便删、decimal precision/scale 改动、把 schema fingerprint 当数据 fingerprint、把 OCF 当普通二进制文件乱切。线上症状通常是旧数据读不了、灰度 consumer 报 schema resolution error、历史重放失败、金额时间字段错、数据无法排障。
```

## Q060. Avro 在单机和分布式环境中的语义有什么差异？
**回答：**

单机里，Avro 往往只是一个 schema 加 encoder/decoder：用同一个 schema 写，再用同一个 schema 读，round-trip 通过，看起来很简单。分布式环境里，Avro 的真正语义才出现：生产者、消费者、离线任务、schema registry、历史数据、不同语言 runtime 都可能处在不同版本。

单机场景主要关注这些：

```text
schema 能否 parse；
record 能否 encode/decode；
GenericRecord 或 SpecificRecord 映射是否正确；
logical type 在本语言里如何表示；
性能和分配是否可接受；
encoder/decoder 是否被并发误用。
```

这些问题相对局部。测试用同一个版本的 schema 和 runtime，通常就能发现。

分布式环境关注的是长期协议语义：

```text
writer schema 如何随数据保存；
reader schema 是否和历史 writer schema 兼容；
schema registry 是否高可用；
schema id 或 fingerprint 是否稳定；
不同语言对 logical type、union、default、aliases 的处理是否一致；
灰度期间新旧 producer/consumer 是否能混跑；
历史 OCF 文件和 Kafka 消息是否还能重放。
```

最重要的差异是 writer schema 变成数据的一部分。单机测试里你手里总有 schema；分布式系统里，数据可能在 Kafka、对象存储、HDFS、归档系统里保存多年。只要 writer schema 丢了，Avro binary 的业务含义就丢了。

第二个差异是兼容性方向要明确。数据管道里常说 backward、forward、full compatibility，但真正落地时要问清楚：新 consumer 读旧数据吗？旧 consumer 读新数据吗？历史重放需要读所有版本吗？不同答案会影响是否必须给新字段 default、能否删 enum symbol、能否改 logical type。

第三个差异是 schema registry 成为基础设施。单机里 schema 是本地文件；分布式里 producer 要注册 schema，consumer 要查 writer schema，CI 要做 breaking change 检查，运行时要缓存。registry 抖动会变成业务延迟或消费失败。

第四个差异是数据格式和文件格式要分清。Kafka 单条消息可能用 single-object encoding 或 registry wire format；数据湖可能用 OCF；分析层可能转 Parquet。它们都可能叫 Avro，但元数据位置、切分方式、压缩方式和恢复策略不同。

第五个差异是 logical type 的环境差异。timestamp 是全局时间点，local timestamp 是本地时间语义；decimal 要 precision/scale 一致；uuid 可以是 string 或 fixed。不同语言、时区设置、库版本会让同一底层值显示成不同形式。

第六个差异是故障会被重放放大。单机 decode 失败就是一个异常；分布式里同一个坏 schema 可以让整个 consumer group 卡住，同一个错误 OCF block 可以让批任务反复失败，同一个缺失 default 可以让多年历史数据无法迁移。

面试里可以这样说：

```text
单机里 Avro 是本地 schema 加编解码；分布式里 Avro 是数据生命周期契约。writer schema 必须随数据保存，reader schema 要和历史版本兼容，schema registry、compatibility check、logical type、OCF/single-object encoding、灰度和重放策略都要一起设计。单机 round-trip 通过，不代表分布式数据能长期读回来。
```

## 参考资料

- [RFC 8259: The JavaScript Object Notation Data Interchange Format](https://www.rfc-editor.org/rfc/rfc8259.html)
- [RFC 8785: JSON Canonicalization Scheme](https://www.rfc-editor.org/rfc/rfc8785.html)
- [RFC 7493: The I-JSON Message Format](https://www.rfc-editor.org/rfc/rfc7493.html)
- [ECMAScript JSON.stringify](https://tc39.es/ecma262/multipage/structured-data.html#sec-json.stringify)
- [RFC 4648: The Base16, Base32, and Base64 Data Encodings](https://www.rfc-editor.org/rfc/rfc4648.html)
- [RFC 3339: Date and Time on the Internet: Timestamps](https://www.rfc-editor.org/rfc/rfc3339.html)
- [RFC 7515: JSON Web Signature](https://www.rfc-editor.org/rfc/rfc7515.html)
- [RFC 7516: JSON Web Encryption](https://www.rfc-editor.org/rfc/rfc7516.html)
- [RFC 8725: JSON Web Token Best Current Practices](https://www.rfc-editor.org/rfc/rfc8725.html)
- [RFC 1951: DEFLATE Compressed Data Format Specification](https://www.rfc-editor.org/rfc/rfc1951)
- [Protocol Buffers Overview](https://protobuf.dev/overview/)
- [Protocol Buffers Encoding](https://protobuf.dev/programming-guides/encoding/)
- [Protocol Buffers ProtoJSON Format](https://protobuf.dev/programming-guides/json/)
- [Proto Serialization Is Not Canonical](https://protobuf.dev/programming-guides/serialization-not-canonical/)
- [Protocol Buffers proto3 Language Guide](https://protobuf.dev/programming-guides/proto3/)
- [Protocol Buffers Best Practices](https://protobuf.dev/best-practices/dos-donts/)
- [Protocol Buffers Proto Limits](https://protobuf.dev/programming-guides/proto-limits/)
- [Protocol Buffer MIME Types](https://protobuf.dev/reference/protobuf/mime-types/)
- [Protocol Buffers Conformance Tests](https://github.com/protocolbuffers/protobuf/tree/main/conformance)
- [JSON Schema 2020-12 Validation Specification](https://json-schema.org/draft/2020-12/json-schema-validation)
- [JSON Schema Core Specification](https://json-schema.org/draft/2020-12/json-schema-core)
- [JSON Schema Getting Started](https://json-schema.org/learn/getting-started-step-by-step)
- [OWASP Deserialization Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Deserialization_Cheat_Sheet.html)
- [OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
- [NIST FIPS 180-4: Secure Hash Standard](https://csrc.nist.gov/pubs/fips/180-4/upd1/final)
- [Apache Avro Project](https://avro.apache.org/)
- [Apache Avro 1.12.0 Specification](https://avro.apache.org/docs/1.12.0/specification/)
- [Apache Thrift IDL](https://thrift.apache.org/docs/idl)
- [Apache Thrift Protocol Structure](https://github.com/apache/thrift/blob/master/doc/specs/thrift-protocol-spec.md)
