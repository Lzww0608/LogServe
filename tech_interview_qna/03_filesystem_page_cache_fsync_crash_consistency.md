# 三、文件系统、page cache、fsync 与 crash consistency 常见技术面试题

这份题库整理文件写入、page cache、`fsync`、目录持久化、`rename` 原子性、journaling 和磁盘 write cache 相关问题。回答默认以 Linux/POSIX 语义为主，面试时要主动说明：不同文件系统、挂载参数、块设备、网络文件系统和虚拟化环境都会影响最终保证。

## Q001. write 返回成功是否代表数据已经落盘？

**回答：**

不代表。`write()` 返回成功，通常只说明内核已经接受了这次写入，把用户态 buffer 里的数据复制到了内核管理的缓存页，或者至少完成了这次系统调用语义上的写入。它不等于数据已经写到磁盘介质，也不等于断电后一定还能读回来。

普通 buffered I/O 的路径大致是：

1. 应用调用 `write(fd, buf, n)`。
2. 内核把数据复制到 page cache 对应的页里。
3. 这些页被标记为 dirty。
4. 后台 writeback 线程在之后某个时间把 dirty pages 写到块设备。
5. 块设备、磁盘控制器或 SSD 内部还可能有自己的 cache。

所以 `write()` 成功更接近“数据进入了内核的写入路径”，不是“数据已经持久化”。

还有几个容易忽略的点：

1. **write 可能部分成功**

   返回值小于请求写入的字节数时，调用方必须继续写剩余部分。不能只判断返回值不是 -1。

2. **错误可能延后报告**

   磁盘满、配额耗尽、网络文件系统写回失败、底层 I/O 错误，不一定在当前 `write()` 立刻返回。它们可能在后续 `write()`、`fsync()`，甚至 `close()` 时才暴露。Linux man-pages 对这一点写得很明确。

3. **后续 read 能读到，不代表落盘**

   一个进程写完后，另一个进程马上读，通常能从 page cache 读到新数据。这是可见性，不是持久性。机器断电后还能不能读到，是另一个问题。

4. **O_SYNC/O_DSYNC 改变语义，但成本更高**

   如果文件以 `O_SYNC` 或 `O_DSYNC` 打开，写入路径会更接近同步持久化语义。但它们仍然依赖文件系统和设备正确实现 flush、barrier、FUA 等机制。

5. **O_DIRECT 也不等于万能持久化**

   `O_DIRECT` 主要是绕过 page cache，减少缓存污染和内存复制。它不自动等价于 `fsync()`。很多场景仍然需要配合同步标志或显式 `fsync()` 才能得到持久化边界。

面试里可以这样回答：`write()` 成功代表内核接受了数据，最多说明数据对后续读操作可见；要把“写入成功”升级成“崩溃后可恢复”，通常需要 `fsync()` 或同等级别的同步机制，并且要检查它的返回值。

## Q002. page cache 在文件写入路径中扮演什么角色？

**回答：**

page cache 是内核用内存缓存文件内容的机制。它既服务读，也服务写。对普通 buffered I/O 来说，page cache 是应用和块设备之间的主要缓冲层。

写入时，page cache 做了几件事：

1. **吸收写入**

   应用调用 `write()` 时，内核通常先把数据写入 page cache，而不是马上同步写到磁盘。这样系统调用可以较快返回。

2. **合并和重排 I/O**

   多次小写入可以在内存里合并成更大的磁盘 I/O。内核也可以根据调度策略把写回顺序整理得更适合设备。

3. **标记 dirty pages**

   被修改但还没写回存储设备的页叫 dirty pages。后台 writeback 会在内存压力、时间阈值、dirty ratio、显式同步等条件下把它们写出去。

4. **提供读后写一致性**

   写入后立刻读取同一文件区域，通常直接命中 page cache。应用看到的是新内容，即使磁盘上还没更新。

5. **连接 mmap 和文件 I/O**

   file-backed `mmap` 通常也通过 page cache。进程通过内存映射改了文件页，这些页同样会变 dirty，后续通过 `msync()`、`fsync()` 或后台 writeback 写回。

它带来的好处是明显的：减少磁盘访问，合并小写，提升吞吐，让读写共享缓存。但它也带来持久性上的误区：数据在 page cache 里，不等于数据在磁盘上。崩溃时，dirty pages 可能丢失。

它还会影响线上行为：

- 写入延迟看起来很低，但 `fsync()` 时突然变慢，因为脏页集中写回。
- 大量写入可能触发 dirty throttling，应用写入速度被内核限制。
- 内存压力下，干净页可以直接回收，脏页必须先写回。
- 使用 `O_DIRECT` 可以绕过 page cache，但会增加对对齐、I/O 大小和应用缓存管理的要求。

所以 page cache 是性能优化层，也是 crash consistency 中必须认真处理的一层。很多“write 已成功但重启后数据没了”的问题，本质上就是把 page cache 可见性误当成了持久性。

## Q003. fsync、fdatasync、sync、msync 的区别是什么？

**回答：**

这几个接口都和“把内存里的修改同步到存储”有关，但同步范围和使用场景不同。

**fsync(fd)**

`fsync()` 同步某个文件描述符对应文件的脏数据和相关元数据。它要保证系统崩溃或重启后，已经同步的信息仍然能被取回。文件内容、文件大小、必要 inode 元数据都会被考虑进去。Linux man-pages 还强调，如果有磁盘 cache，`fsync()` 需要写穿或刷新它。

但 `fsync(file_fd)` 不一定保证目录项也持久化。也就是说，新建文件、`rename()`、`unlink()` 这类目录结构变化，通常还需要对父目录执行 `fsync()`。

**fdatasync(fd)**

`fdatasync()` 和 `fsync()` 类似，但它尽量只同步后续读取文件数据所必需的信息。比如文件大小变化必须同步，因为不更新 size 就读不到新扩展出来的数据；但单纯的 atime、mtime 这类不影响读取内容的元数据，可以不强制同步。

它的目标是减少不必要的元数据 I/O。对数据库、日志系统来说，如果只关心数据内容和大小，`fdatasync()` 可能比 `fsync()` 更合适。

**sync()**

`sync()` 是系统级或全局性质的同步。它要求把所有挂起的文件系统元数据和缓存文件数据写到底层文件系统。Linux 上还有 `syncfs(fd)`，只同步某个 fd 所在的文件系统。

它不适合作为普通应用的精确提交边界。应用通常需要知道“我的这个文件写持久了吗”，而不是“系统里所有脏数据都写一遍”。`sync()` 范围太大，干扰也大。

**msync(addr, len, flags)**

`msync()` 用于 `mmap()` 出来的内存映射区域。应用直接改映射内存时，并没有调用 `write()`，所以需要 `msync()` 把映射区域对应的修改刷回文件系统。

`MS_SYNC` 表示等待写回完成，`MS_ASYNC` 表示调度写回后返回。Linux 上 `MS_ASYNC` 在较新内核里实际意义很弱，因为内核本来就会跟踪 dirty pages 并按需写回。为了可移植，还是应该明确传 `MS_SYNC` 或 `MS_ASYNC`。

可以简单记：

| 接口 | 作用对象 | 典型用途 |
| --- | --- | --- |
| `fsync` | 单个文件 fd | 持久化文件数据和相关元数据 |
| `fdatasync` | 单个文件 fd | 持久化数据和读取所需元数据 |
| `sync` | 全系统文件系统缓存 | 管理或关机前的粗粒度同步 |
| `syncfs` | 某个文件系统 | 同步一个挂载文件系统 |
| `msync` | mmap 地址范围 | 持久化内存映射写入 |

面试时最好补一句：这些接口都要检查返回值。`fsync()` 失败不是小事，它可能意味着之前的写入没有真正持久化。

## Q004. 为什么 fsync 很慢？

**回答：**

`fsync()` 慢，是因为它把平时被系统异步化、合并、延迟的工作，强行推进到一个必须等待的持久化边界上。

慢的来源主要有这些：

1. **要等待脏数据写回**

   普通 `write()` 可以把数据留在 page cache 里，稍后由后台写回。`fsync()` 要等目标文件的脏页写到存储设备。如果前面写了很多数据，`fsync()` 就会替之前的异步写入付账。

2. **要同步必要元数据**

   文件大小、block 分配、inode、间接块或 extent、目录项相关信息都可能需要更新。写文件不是只写 payload；文件系统还要维护“这些 bytes 放在哪些块上”。

3. **要提交 journal**

   在 journaling 文件系统上，`fsync()` 可能需要等待当前事务写入 journal 并提交。事务里可能有别的文件的元数据更新，导致调用方被同一批 journal commit 拖住。

4. **要保证写入顺序**

   crash consistency 依赖顺序。例如数据块要先于某些元数据提交，journal commit 要按顺序落盘。为了保证顺序，文件系统和块层可能要插入 barrier、flush 或 FUA。

5. **要刷新设备 write cache**

   现代磁盘和 SSD 常有自己的 volatile write cache。设备可能已经向操作系统报告“写入完成”，但数据还在设备缓存里。为了断电安全，`fsync()` 路径通常需要发 cache flush 或等价命令。这个操作延迟可能很高。

6. **小同步写破坏批量优化**

   存储设备喜欢大块、顺序、可合并的 I/O。频繁 `fsync()` 会把写入切成很多小事务，减少合并机会，增加 journal commit 和 cache flush 次数。

7. **并发场景会互相影响**

   多个线程或进程同时写同一文件系统，`fsync()` 可能等待共享资源：journal 锁、inode 锁、块层队列、设备 flush。一个看似只同步小文件的调用，可能被系统整体 I/O 状态影响。

所以 `fsync()` 慢不是因为系统调用本身重，而是因为它要求给出“到这里为止，崩溃后也要成立”的保证。这个保证需要穿过 page cache、文件系统、journal、块层、设备 cache，任何一层都可能让它等待。

工程上常见优化不是“去掉 fsync”，而是减少 fsync 频率、做 group commit、批量提交、把日志顺序写、使用 `fdatasync()`、选择合适的文件系统和挂载参数，或者把持久化协议设计成能容忍批量提交。

## Q005. fsync 文件和 fsync 目录分别保证什么？

**回答：**

`fsync(file_fd)` 和 `fsync(dir_fd)` 解决的是两类不同的持久化问题。文件 fd 关注文件内容和文件自身元数据，目录 fd 关注目录项，也就是“名字到 inode 的映射”。

**fsync 文件保证什么**

对普通文件调用 `fsync()`，主要保证：

- 文件脏数据写回。
- 读取这些数据所需的元数据写回。
- 文件大小变化等关键 inode 状态写回。
- 必要时刷新底层设备 cache。

举例说，应用往 `data.tmp` 写入 4KB，然后 `fsync(data.tmp)` 成功。它可以更有把握地认为这个文件 inode 对应的数据内容已经持久化。

但这还不够保证“这个文件名在目录里一定存在”。如果 `data.tmp` 是刚创建的文件，文件内容 fsync 了，父目录的目录项可能还没有持久化。崩溃后，文件内容可能在，文件名不一定在。

**fsync 目录保证什么**

对目录 fd 调用 `fsync()`，主要保证目录结构修改持久化，比如：

- 新建文件产生的目录项。
- `rename()` 改变的名字映射。
- `unlink()` 删除的目录项。
- 创建或删除子目录的目录项变化。

典型的安全写文件流程是：

```text
open temp file in same directory
write temp file
fsync(temp file)
rename(temp, final)
fsync(parent directory)
```

第一段 `fsync(temp file)` 保证文件内容和文件自身元数据。`rename()` 提供运行时可见的原子替换。最后的 `fsync(parent directory)` 保证这个名字替换在崩溃后仍然存在。

如果跨目录 rename，源目录和目标目录都发生目录项变化，稳妥做法是同步相关目录。很多应用为了简化，会把临时文件放在目标文件同一目录下，避免跨目录持久化问题。

面试里最重要的一句是：`fsync` 文件不等于 `fsync` 文件名。文件名属于目录，目录项要同步目录。

## Q006. rename 在 POSIX 文件系统中通常提供什么原子性？

**回答：**

`rename(old, new)` 通常提供的是命名空间可见性上的原子性。也就是说，进程观察 `new` 这个路径时，不会看到一个“中间状态”。

常见保证包括：

1. **替换是原子的**

   如果 `new` 已经存在，成功的 `rename(old, new)` 会原子替换它。其他进程不会看到 `new` 突然消失再出现。Linux man-pages 对这一点也有说明：不会存在另一个进程访问 `newpath` 时发现它缺失的瞬间。

2. **失败时 new 通常仍在**

   如果 `new` 已存在而 rename 失败，`rename()` 要保证 `new` 仍然留在原处。不能失败到一半，把旧文件删了。

3. **打开的文件描述符不受路径替换影响**

   如果某个进程已经打开了旧的 `new` 文件，另一个进程用 rename 替换路径名，已打开的 fd 仍然指向原来的 inode。路径名变了，不代表已打开 fd 自动切换。

4. **同一文件系统内才有这种原子 rename**

   跨文件系统 rename 通常失败并返回 `EXDEV`。用户态工具 `mv` 会退化成 copy + unlink，那就不是同一个原子操作了。

但要注意：`rename()` 的原子性不等于崩溃持久性。

运行时看，它是原子的；断电后看，如果没有正确 `fsync()` 文件和目录，最终恢复出来的状态可能不是你以为的状态。比如 temp 文件内容还没落盘，或者目录项替换还没持久化。

所以写配置文件、manifest、索引文件时，常见模式不是只做 rename，而是：

```text
write temp
fsync temp
rename temp -> final
fsync parent directory
```

可以这样总结：`rename()` 保证路径切换的可见性原子，不单独保证数据内容和目录项在 crash 后都持久。

## Q007. 为什么写临时文件再 rename 仍然可能需要 fsync directory？

**回答：**

因为 `rename()` 修改的是目录项，而目录项属于目录文件本身。你把临时文件 `a.tmp` 改名成 `a`，本质上是修改父目录里“文件名 -> inode”的映射。`fsync(a.tmp)` 只能保证临时文件自己的内容和 inode 状态，不保证父目录里的名字映射已经落盘。

安全写入通常分三层：

1. **文件内容持久**

   写 temp 文件后，对 temp 文件 fd 调 `fsync()`。这保证 temp 文件内容不是只停留在 page cache 里。

2. **命名空间原子切换**

   调 `rename(temp, final)`。这保证运行时其他进程看到的路径要么是旧文件，要么是新文件，不会看到半个文件。

3. **目录项持久**

   对父目录 fd 调 `fsync()`。这保证 rename 造成的目录项变化在崩溃后还能恢复出来。

如果少了最后一步，可能出现这些情况：

- 进程返回成功，应用以为 `final` 已经更新。
- 机器断电。
- 重启后目录项还停留在旧状态，或者新文件名不存在，具体取决于文件系统、journal、挂载参数和崩溃时机。

如果 temp 文件和 final 在同一个目录，通常 fsync 这个父目录。如果是跨目录 rename，源目录删除 temp 名字、目标目录创建 final 名字，两个目录都可能需要同步。实际工程里为了少踩坑，临时文件一般放在目标文件同目录。

这个问题的面试重点是区分两个概念：文件内容属于文件，文件名属于目录。`fsync(file)` 解决内容持久化，`fsync(directory)` 解决目录项持久化。

## Q008. 文件系统 journaling 的 metadata journaling 和 data journaling 有什么区别？

**回答：**

journaling 的目标是让文件系统崩溃后能恢复到结构一致的状态。区别在于 journal 里记录哪些内容。

**metadata journaling**

metadata journaling 只把文件系统元数据写入 journal，比如 inode、目录项、block bitmap、extent 信息、文件大小等。真正的文件数据块不写入 journal，而是直接写到它们最终的位置。

它的好处是开销较低。大多数文件系统操作的结构一致性可以靠元数据 journal 恢复。崩溃后，文件系统不至于出现严重结构损坏，比如 block 被重复分配、目录项指向乱掉的 inode。

它的限制是：文件内容不一定受 journal 保护。如果应用写入文件数据后崩溃，metadata journal 可能能恢复文件系统结构，但不能保证文件内容就是应用想要的新内容。

**data journaling**

data journaling 会把文件数据和元数据都写入 journal，再提交到最终位置。这样 crash 后可以通过 journal 恢复更完整的数据和元数据状态。

它的保证更强，但成本也更高。因为文件数据可能要写两遍：先写 journal，再写最终位置。对写密集场景，带宽和延迟开销很明显。

可以这样比较：

| 模式 | journal 内容 | 优点 | 代价 |
| --- | --- | --- | --- |
| metadata journaling | 元数据 | 性能较好，保护文件系统结构 | 文件内容一致性较弱 |
| data journaling | 数据 + 元数据 | crash 后内容保证更强 | 写放大更高，延迟更大 |

要注意，metadata journaling 不等于应用层 crash consistency。即使文件系统结构是好的，应用文件也可能处于“旧内容”“新内容”“部分新内容”之间的某个状态。数据库、日志系统、KV 存储仍然要设计自己的 WAL、checksum、commit marker 和恢复协议。

## Q009. writeback、ordered、journal 模式的 crash consistency 有什么差异？

**回答：**

这里通常说的是 ext3/ext4 这类 journaling 文件系统的数据模式：`data=writeback`、`data=ordered`、`data=journal`。它们的差异在于文件数据和元数据之间的写入顺序，以及文件数据是否进入 journal。

**writeback 模式**

writeback 模式只 journal 元数据，不保证文件数据在相关元数据提交前写到磁盘。这样性能较好，但 crash 后风险更大。

典型问题是：元数据已经说明某个文件扩展到了新 block，但对应数据 block 还没写好。崩溃恢复后，文件系统结构可能是自洽的，但文件内容可能是旧数据、未初始化数据或不符合应用预期的数据。现代文件系统会做一些防护，但语义上它仍然是三种模式里最弱的一类。

**ordered 模式**

ordered 模式也主要 journal 元数据，但要求相关文件数据先写到最终位置，再提交指向这些数据的元数据 journal。它不把普通文件数据写进 journal，但会维持“数据先于元数据”的顺序。

这个模式通常是折中选择。它避免了很多“元数据指向垃圾数据”的问题，性能又比 data journaling 好。ext3/ext4 常见默认就是 ordered 语义。

但它仍然不保证应用的一次多块写入具有事务性。崩溃后，你可能看到旧版本，也可能看到部分新版本。要做应用级原子更新，仍然要靠 temp + fsync + rename + fsync dir，或者 WAL/事务协议。

**journal 模式**

journal 模式把文件数据和元数据都写进 journal，再落到最终位置。它的 crash consistency 最强，能避免更多数据和元数据不同步的问题。

代价也最大：写放大明显，journal 压力更高，`fsync()` 延迟可能更大。它适合更重视一致性的场景，但不是所有业务都能接受性能成本。

可以这样记：

| 模式 | 数据是否进 journal | 数据和元数据顺序 | crash 后风险 |
| --- | --- | --- | --- |
| writeback | 否 | 不强制数据先于元数据 | 可能看到旧/脏/不符合预期内容 |
| ordered | 否 | 数据先写，再提交相关元数据 | 结构较安全，但应用事务仍需自己保证 |
| journal | 是 | 数据和元数据都 journal | 保证更强，写放大更高 |

面试时最好补一句：这三种模式解决的是文件系统层面的崩溃恢复，不等于数据库事务。应用如果需要“这条记录要么完整存在，要么不存在”，还要设计自己的提交协议。

## Q010. 磁盘 write cache 会如何影响 fsync 语义？

**回答：**

磁盘 write cache 会让 `fsync()` 的语义多一层依赖。操作系统把数据交给块设备，不代表数据已经进入真正的非易失介质。很多硬盘、SSD、RAID 控制器、虚拟化存储都会先把写入放进设备缓存，再稍后刷到介质。

如果设备 write cache 是易失的，断电时缓存里的数据可能丢。为了让 `fsync()` 真正有持久化意义，I/O 栈需要让设备执行 cache flush、FUA 或等价机制，保证前面的写入到达稳定存储。

它会带来几个影响：

1. **fsync 可能变慢**

   cache flush 往往是高延迟操作。设备需要把内部缓存中相关写入推到稳定介质，还要维持顺序。频繁 `fsync()` 会频繁触发 flush，吞吐会下降。

2. **写入顺序依赖 barrier**

   journaling 文件系统需要保证 journal commit 和数据/元数据写入顺序。如果设备或 I/O 栈重排写入，而没有 barrier/flush 约束，crash 后 journal 语义可能被破坏。

3. **设备说谎会破坏 fsync**

   如果磁盘、RAID 卡、虚拟块设备报告 flush 成功，但实际没有把数据写到非易失存储，操作系统无法凭空修复。应用以为 `fsync()` 成功，断电后仍可能丢数据。

4. **电池保护缓存语义不同**

   如果 RAID 控制器或存储设备有电池/超级电容保护，write cache 可以在断电后继续保存并恢复写入。这种场景下启用缓存风险小很多，性能也好。但前提是保护机制真的可靠，并且监控电池状态。

5. **虚拟化和云盘更复杂**

   虚拟机看到的“磁盘”可能背后是宿主机 page cache、网络存储、分布式副本和 SSD。`fsync()` 的真实语义取决于整个栈是否正确传递 flush/fua。某一层吞掉 flush，语义就会变弱。

6. **禁用 barrier 有风险**

   有些文件系统或设备允许关闭 barrier 来提高性能。这样做只有在底层有可靠非易失缓存或其他顺序保证时才安全。否则就是用 crash consistency 换吞吐。

所以 `fsync()` 不是一个只在应用和内核之间完成的动作。它要穿过文件系统、块层、驱动、控制器和设备缓存。只要底层 write cache 不可靠，或者 flush 语义没有正确实现，`fsync()` 的持久化承诺就会被削弱。

## Q011. barrier、flush、FUA 的作用是什么？

**回答：**

这三个词都和“把写入真正按顺序推到非易失存储”有关，但层次不一样。

**barrier** 关注顺序。它的意思是：barrier 前面的写入必须先于 barrier 后面的写入到达稳定存储，不能被设备、控制器或 I/O 调度随意重排。文件系统做 journaling 时很依赖这个顺序。例如 journal commit block 不能先于它前面的 journal data 乱序落盘，否则崩溃恢复时可能把不完整事务当成完整事务。

**flush** 关注清空设备的易失写缓存。很多磁盘、SSD、RAID 控制器会在内部缓存写入，提前向操作系统报告完成。flush 命令要求设备把缓存里的相关脏数据推到非易失介质。Linux block 层里，对应的语义常见于 `REQ_PREFLUSH`：在某个 I/O 开始之前，先保证之前已经完成的写请求到达非易失存储。

**FUA** 是 Force Unit Access。它关注某一次写请求本身：这个写请求只有在数据进入非易失存储后，才能向上报告完成。Linux block 层里对应 `REQ_FUA`。它比“先写入缓存，之后全局 flush”更细粒度，因为只要求这次写直接具有持久完成语义。

三者可以这样理解：

| 概念 | 解决什么问题 | 粗略理解 |
| --- | --- | --- |
| barrier | 写入顺序 | 前面的别跑到后面去 |
| flush | 清设备缓存 | 把已经完成的缓存写推到稳定介质 |
| FUA | 单次写直达稳定介质 | 这次写完成时必须已经持久 |

它们和 `fsync()` 的关系也很直接。应用调用 `fsync()`，文件系统需要保证文件数据、必要元数据和 journal 状态能在崩溃后恢复。为了做到这一点，文件系统会在块层发出带顺序和持久化要求的 I/O。底层如果有 volatile write cache，就需要 flush 或 FUA 这类机制。

常见误区是把它们当成同一个东西。barrier 不一定等于 flush；flush 不一定只针对某个写；FUA 不等于把整个设备 cache 都清空。实际实现还取决于设备是否支持 FUA、驱动是否正确传递、虚拟化层是否吞掉 flush、RAID 控制器是否有电池保护缓存。

面试里可以这样说：barrier 保顺序，flush 清缓存，FUA 让某次写完成时已经在稳定存储上。`fsync()` 的持久化语义往往要靠这些机制穿过块层和设备层。

## Q012. SSD、HDD、NVMe 在 fsync 延迟上有什么差异？

**回答：**

`fsync()` 的延迟不是只由接口类型决定，还和文件系统、journal、队列深度、设备缓存、是否有掉电保护、电源管理、写放大、当前负载有关。但从硬件形态看，HDD、SATA SSD、NVMe SSD 的表现确实不同。

**HDD**

HDD 是机械设备。它有寻道和旋转延迟，随机写和 flush 都比较慢。`fsync()` 如果触发 journal commit、数据写回和 cache flush，延迟很容易被机械动作放大。小文件频繁 `fsync()` 对 HDD 很不友好，因为每次提交都可能打断顺序写，变成高成本的同步随机 I/O。

**SATA/SAS SSD**

SSD 没有机械寻道，普通随机写延迟比 HDD 低很多。但 `fsync()` 仍然可能慢。原因是 SSD 内部有 FTL 映射、擦除块、垃圾回收、磨损均衡和内部缓存。flush 可能要求 SSD 把 DRAM cache、映射表更新和数据页都推进到非易失状态。如果设备没有掉电保护，flush 成本通常更明显。

**NVMe SSD**

NVMe 的协议和队列模型更适合并行 I/O，提交/完成路径比传统 SATA 更低延迟，队列深度和多核扩展性也更好。所以在高并发随机 I/O 下，NVMe 通常比 SATA SSD 表现好。

但 NVMe 不代表 `fsync()` 免费。`fsync()` 的关键成本往往是“强制持久化边界”，而不是单个 I/O 提交命令。flush 仍然可能让设备等待内部数据和元数据稳定下来。没有 PLP，也就是 power loss protection 的消费级 NVMe，遇到频繁 fsync 时仍可能有明显尾延迟。企业级 NVMe 如果有电容保护和更强的固件策略，`fsync()` 延迟通常更稳定。

可以按这个角度比较：

| 设备 | 普通随机 I/O | fsync 主要成本 | 常见表现 |
| --- | --- | --- | --- |
| HDD | 慢，受机械延迟影响 | 寻道、旋转、cache flush | 平均和尾延迟都高 |
| SATA/SAS SSD | 比 HDD 快 | FTL、GC、flush、无 PLP 时 cache 落盘 | 平均低，但尾延迟可能抖 |
| NVMe SSD | 更高并发、更低协议开销 | flush、FTL、PLP、队列竞争 | 通常更快，但 fsync 仍是同步边界 |

面试里不要说“SSD fsync 一定很快”。更准确的说法是：SSD/NVMe 去掉了机械延迟，但 `fsync()` 仍要等待文件系统提交和设备持久化；是否有掉电保护、flush 实现质量和当前写入压力，会强烈影响延迟。

## Q013. 为什么 group commit 可以提高吞吐？

**回答：**

group commit 的核心思路是：多条事务共享一次昂贵的持久化操作。它常见于数据库、日志系统、消息队列和文件系统 journal。

`fsync()`、journal commit、设备 cache flush 都有固定成本。假设每条请求都单独：

```text
write record A -> fsync
write record B -> fsync
write record C -> fsync
```

那么每条记录都要独立等待一次 journal 提交和设备 flush。吞吐会被同步边界限制住。

group commit 会把一小段时间内到达的多条记录合并：

```text
write A
write B
write C
fsync once
ack A/B/C
```

这样一次 `fsync()` 可以提交多条记录。固定成本被摊薄，设备也更容易看到大块、顺序、可合并的 I/O。吞吐通常会明显提高。

它提高吞吐的原因主要有这些：

1. **摊薄 flush 成本**

   cache flush 或 FUA 是高成本操作。一次 flush 提交 100 条记录，比 100 次 flush 每次提交 1 条记录要划算得多。

2. **减少 journal commit 次数**

   journaling 文件系统和数据库 WAL 都有 commit 记录。把多个事务放进同一个 commit，可以减少 journal 压力。

3. **提高 I/O 合并机会**

   多个小写入可以合并成更大的顺序写，减少设备随机写开销。

4. **降低锁和调度成本**

   每次 `fsync()` 都会穿过 VFS、文件系统、块层和设备队列。批量提交能减少这些路径上的重复工作。

5. **改善并发等待**

   多个请求可以一起等待同一个提交完成。对调用方来说，单条请求延迟可能略增，但系统总吞吐更高。

代价也很清楚：group commit 会增加等待窗口。请求可能要等一小段时间，凑够批次或等下一个提交时钟。系统需要在吞吐和延迟之间选一个点。

还要区分 ack 策略。如果系统在 group fsync 成功后才 ack，那么已经 ack 的数据有较强持久性；崩溃时最多丢未 ack 的当前 batch。如果系统先 ack、后 fsync，那么吞吐更高，但已 ack 数据也可能在崩溃时丢失。

## Q014. interval fsync 和 always fsync 的可靠性差异是什么？

**回答：**

`always fsync` 是每次关键写入后都同步。`interval fsync` 是隔一段时间同步一次，比如每 100ms、1s 或每 N 条记录同步一次。可靠性差异取决于一个关键点：系统什么时候向上层返回成功。

**always fsync**

每条记录或每次事务写入后执行 `fsync()`，并且只有 `fsync()` 成功后才 ack。这样崩溃后，已经 ack 的记录通常应该能恢复。代价是每条记录都要付同步持久化成本，吞吐低，尾延迟高。

这种模式适合强持久性需求，比如数据库事务提交、金融账务、元数据变更、不能接受 ack 后丢失的业务。

**interval fsync**

系统先把记录写入 page cache 或应用 buffer，然后每隔一段时间批量 `fsync()`。如果它在 `fsync()` 前就 ack，那么崩溃时可能丢掉最近一个同步周期内已经返回成功的数据。

例如每 1 秒 fsync 一次，进程在第 900ms 时 ack 了一批日志，随后机器断电。这些日志可能只在 page cache 里，重启后丢失。对用户来说，就是“服务说写成功了，但重启后没了”。

但 interval fsync 也可以设计得更保守：写入可以先进入 batch，只有 batch fsync 成功后才 ack。这样它接近 group commit，而不是简单的异步刷盘。此时可靠性比“先 ack 后 fsync”强，代价是请求要等到下一个提交批次。

可以这样比较：

| 策略 | ack 时机 | 已 ack 数据崩溃后是否可能丢 | 性能 |
| --- | --- | --- | --- |
| always fsync | 每次 fsync 成功后 | 通常不应丢，前提是存储栈可靠 | 慢 |
| interval fsync，先 ack | 写入内存或 page cache 后 | 可能丢最近一个 interval | 快 |
| interval/group fsync，后 ack | batch fsync 成功后 | 通常不应丢已 ack batch | 折中 |

面试里要说清楚：`interval fsync` 不是天然不可靠，真正的语义由 ack 时机决定。先 ack 后刷盘是吞吐优先；刷盘后 ack 是持久性优先。

## Q015. batch fsync 在系统崩溃时最多可能丢失哪些数据？

**回答：**

batch fsync 崩溃时最多丢什么，还是看 ack 策略和写入协议。

如果系统是“写入 batch 后先 ack，后台定期 fsync”，那么最多可能丢失从上一次成功 `fsync()` 之后到崩溃前的所有已 ack 记录。这个窗口可以按时间定义，也可以按条数或字节数定义。

例如：

```text
t0: fsync 成功，持久点到 offset 1000
t1: 写入 1001-1500，已 ack
t2: 写入 1501-1800，已 ack
t3: 崩溃，还没 fsync
```

恢复后最多只能保证到 offset 1000。1001-1800 这些记录即使之前返回成功，也可能丢失。部分记录也可能残留在磁盘上，但不能盲信，必须通过 checksum、length、commit marker、sequence number 判断最后一条完整记录。

如果系统是“batch fsync 成功后再 ack”，那么崩溃时通常只会丢未 ack 的当前 batch。已经 ack 的 batch 应该能恢复，前提是 `fsync()` 成功返回并且存储栈没有说谎。

还要考虑几类边界：

1. **最后一条记录可能 torn write**

   崩溃发生在写 record 中间，重启后可能看到半条记录。日志格式要有 length、checksum、magic 或 commit marker，恢复时截断到 last good offset。

2. **目录项可能没持久化**

   如果 batch 写入涉及新 segment 文件、rename manifest、切换 active log，光 fsync 数据文件不够。目录项没 fsync 时，崩溃后文件名或 rename 结果可能丢。

3. **索引和数据可能不一致**

   日志数据 fsync 了，内存索引或单独索引文件没 fsync。恢复时应该以 WAL/log 为准重建索引，而不是相信未同步索引。

4. **跨文件 batch 更复杂**

   如果一个 batch 写了多个文件，单个文件 fsync 不能形成跨文件事务。需要 manifest、事务标记或恢复协议处理“部分文件已持久，部分文件未持久”的状态。

5. **设备 cache 影响最终边界**

   如果底层 flush/FUA 不可靠，`fsync()` 成功也可能不代表真正断电持久。这属于更底层的存储语义问题。

所以一个严谨回答是：先 ack 后 batch fsync，最多丢上次成功 fsync 之后所有已 ack 数据；fsync 后再 ack，最多丢未 ack batch；无论哪种，都要用日志校验和恢复协议处理最后一条 partial record。

## Q016. direct I/O 绕过 page cache 的利弊是什么？

**回答：**

direct I/O 通常指用 `O_DIRECT` 打开文件，让读写尽量绕过 page cache，直接在用户态 buffer 和块设备之间传输。它不是“更快 I/O”的万能开关，而是把缓存管理责任从内核转移给应用。

好处主要有这些：

1. **避免双重缓存**

   数据库、存储引擎、缓存系统通常自己有 buffer pool。如果再让 page cache 缓一份，就会浪费内存。direct I/O 可以减少这种重复缓存。

2. **减少 page cache 污染**

   大量顺序扫描、备份、压缩、导入导出任务可能把热点文件页挤出 page cache。direct I/O 可以降低这种影响。

3. **让应用控制缓存策略**

   数据库更知道哪些页热、哪些页冷、哪些页可以淘汰。direct I/O 让应用自己的缓存策略更可控。

4. **减少部分内存复制**

   在某些路径上，direct I/O 可以减少用户态和 page cache 之间的复制。不过具体收益取决于文件系统、设备、I/O 大小和对齐。

代价也很明显：

1. **对齐要求麻烦**

   Linux 上 `O_DIRECT` 可能要求用户 buffer 地址、I/O 长度、文件 offset 按文件系统或块设备要求对齐。不同文件系统和内核版本还不完全一样。对齐不满足时，可能返回 `EINVAL`，也可能退回 buffered I/O。

2. **小 I/O 性能可能更差**

   page cache 能做合并、预读、延迟写。direct I/O 绕过这些优化，小块随机 I/O 可能更慢。

3. **不等于同步持久化**

   `O_DIRECT` 自己不提供 `O_SYNC` 的持久化保证。它绕过 page cache，不代表数据已经通过设备 cache 到达稳定介质。需要同步语义时仍然要 `O_SYNC`、`O_DSYNC` 或 `fsync()`。

4. **不能随意和 buffered I/O 混用**

   同一文件、同一区域同时使用 direct I/O、普通 buffered I/O、`mmap`，会带来一致性和性能问题。即使文件系统处理了 coherency，吞吐也可能变差。

5. **应用复杂度上升**

   应用要自己处理缓存、预读、淘汰、写回、对齐、I/O 调度和错误恢复。做不好会比 page cache 更差。

6. **网络文件系统语义不同**

   例如 NFS 中，`O_DIRECT` 可能只绕过客户端 page cache，服务端仍然可能缓存。稳定存储语义还取决于服务端实现。

所以 direct I/O 适合数据库、日志存储、对象存储节点这类自己掌控缓存和 I/O 调度的系统。不适合普通应用随手打开。Linux man-pages 也建议把 `O_DIRECT` 当成需要谨慎开启的性能选项，而不是默认选项。

## Q017. mmap 写文件和 write 系统调用在一致性上有什么差异？

**回答：**

`write()` 和 `mmap()` 最后都可能通过 page cache 修改文件，但它们给应用暴露的接口和一致性风险不同。

**write 系统调用**

应用调用 `write(fd, buf, len)`，内核把用户 buffer 中的数据复制到 page cache 或提交到对应 I/O 路径。返回值告诉你这次系统调用写了多少字节。它可能部分成功，调用方要检查返回值并继续写。

`write()` 的好处是边界清楚：每次系统调用有返回值，有 errno，有写入长度。错误路径比较显式。

**mmap 写文件**

应用用 `mmap()` 把文件映射到内存，然后像写普通内存一样写这个区域。如果是 `MAP_SHARED`，修改会对其他映射同一文件区域的进程可见，并且会被写回底层文件；要精确控制写回时间，需要 `msync()`。

它的问题是写入没有每次系统调用边界。你写一个内存地址时不会立刻得到“写文件成功”或“磁盘满”的返回值。错误可能在 page fault、`msync()`、`fsync()`、`munmap()` 或后续访问时暴露。文件被截断后继续访问映射区域，还可能触发 `SIGBUS`。

一致性差异主要有这些：

1. **错误报告不同**

   `write()` 有返回值和 partial write。`mmap` 写内存没有每次写入返回值，很多错误被推迟。

2. **同步边界不同**

   `write()` 后可以调用 `fsync(fd)`。`mmap` 修改后通常要调用 `msync(MS_SYNC)`，必要时还要 `fsync(fd)` 处理文件元数据或目录项。

3. **可见性不同**

   `MAP_SHARED` 的修改对其他映射进程可见；`MAP_PRIVATE` 是 copy-on-write，修改不会写回文件。这个选项选错，可能以为改了文件，实际只改了进程私有页。

4. **文件大小变化更敏感**

   `mmap` 的映射长度和文件大小关系很微妙。文件尾部不足一页的区域会零填充，超出文件对象末尾的修改不会写回文件。文件被其他线程 truncate 时，访问映射可能出错。

5. **并发控制更靠应用**

   多进程 mmap 同一区域写入时，普通内存写不是自动事务。需要锁、原子操作、版本号、checksum 或 WAL 来保证结构一致。

面试里可以这样总结：`write()` 是显式 I/O 调用，错误和边界更清楚；`mmap` 把文件变成内存访问，减少 syscall，但错误延迟、同步边界和并发一致性更难管理。

## Q018. mmap 场景下 msync 是否等价于 fsync？

**回答：**

不完全等价。`msync()` 和 `fsync()` 都能参与持久化，但作用对象不同。

`msync(addr, len, MS_SYNC)` 针对的是一段内存映射区域。它要求把这段映射中被修改的页同步回文件系统，并等待完成。它适合回答：“我通过 mmap 改了这些页，怎样把这些页刷回去？”

`fsync(fd)` 针对的是文件描述符对应的文件。它同步文件脏数据和相关元数据，例如文件大小、block 分配、inode 状态等。它适合回答：“这个文件到目前为止的修改，怎样变成崩溃后可恢复？”

两者的差异有几个：

1. **范围不同**

   `msync()` 是地址范围。`fsync()` 是文件。你 mmap 了文件的一部分，`msync()` 只覆盖那段映射范围；文件其他 dirty pages 不一定被它同步。

2. **元数据语义不同**

   `msync()` 主要同步映射页内容。文件大小变化、目录项、rename 结果、某些元数据持久化，通常不能只靠 `msync()`。这些仍然需要 `fsync(file)` 或 `fsync(directory)`。

3. **文件扩展场景不同**

   如果你先 `ftruncate()` 扩展文件，再 mmap 写新区域，崩溃一致性要考虑 size 元数据和新数据页。只 `msync()` 数据页不一定足够表达“新文件大小也持久”。

4. **目录项更不是 msync 的职责**

   新建文件、rename 文件、替换 manifest，这些目录项变化属于父目录。`msync()` 对映射内容有效，不会让目录项持久化。

5. **MAP_PRIVATE 不写回文件**

   如果映射是 `MAP_PRIVATE`，修改是私有 copy-on-write，`msync()` 也不能把这些私有修改变成文件内容。

实践上，如果只是在已存在文件的固定范围内用 `MAP_SHARED` 改内容，`msync(MS_SYNC)` 可以同步这部分内容。但如果涉及文件大小、分配、目录项、rename、事务边界，仍然要按文件协议使用 `fsync()` 和目录 `fsync()`。

一句话：`msync()` 管 mmap 页，`fsync()` 管文件持久化边界。二者有交集，但不能互相替代。

## Q019. 稀疏文件、预分配和 fallocate 分别解决什么问题？

**回答：**

这三个概念都和文件空间有关，但目标不同。

**稀疏文件**

稀疏文件是逻辑大小大，但中间某些区域没有实际分配磁盘块的文件。未分配的区域叫 hole。读取 hole 时，文件系统返回零字节；只有真正写入数据的区域才占用物理空间。

例如虚拟机镜像、数据库快照、日志预留文件、科学计算输出，都可能逻辑上很大，但实际写入区域很少。稀疏文件节省空间，也让创建大文件更快。

它的风险是：文件看起来很大，不代表空间已经保留。后续写 hole 时仍可能因为磁盘满失败。备份和复制工具如果不识别 hole，还可能把稀疏文件变成真正占满空间的大文件。

**预分配**

预分配是提前为文件保留磁盘空间，降低后续写入失败和碎片化风险。日志系统、数据库、消息队列 segment 经常会预分配一段空间，因为它们希望写入热路径不要突然遇到 `ENOSPC`，也希望文件块尽量连续。

预分配的重点不是节省空间，而是“提前占住空间”。

**fallocate**

`fallocate()` 是 Linux 上直接操作文件已分配空间的系统调用。默认模式会给指定范围分配磁盘空间；如果范围超过文件大小，还会扩展文件大小。成功后，后续写入这段范围不应因为缺少磁盘空间失败。

它还有一些模式：

- `FALLOC_FL_KEEP_SIZE`：预分配空间但不改变文件逻辑大小，适合优化 append workload。
- `FALLOC_FL_PUNCH_HOLE`：打洞，释放一段空间，让读取该范围返回零。
- `FALLOC_FL_ZERO_RANGE`：把一段范围置零，很多文件系统会用 unwritten extents 优化，不一定真的写满零。
- `FALLOC_FL_COLLAPSE_RANGE`、`FALLOC_FL_INSERT_RANGE`：删除或插入文件中间范围，要求文件系统支持。

可以这样区分：

| 概念 | 解决的问题 |
| --- | --- |
| 稀疏文件 | 逻辑大文件不必真的占满磁盘 |
| 预分配 | 提前保留空间，减少写入时 ENOSPC 和碎片 |
| fallocate | Linux 上执行分配、保留、打洞、置零等空间操作的接口 |

注意，`fallocate()` 改的是空间分配和文件元数据。它成功不等于应用数据已经写入，也不等于目录项已经持久。需要 crash consistency 时，仍然要结合 `fsync()`、目录 `fsync()` 和应用日志协议。

## Q020. 为什么日志系统常用 append-only 写入？

**回答：**

日志系统常用 append-only，是因为追加写最符合磁盘、文件系统和崩溃恢复的工程现实。它牺牲了一部分空间整理复杂度，换来写入路径简单、吞吐高、恢复清楚。

主要原因有这些：

1. **顺序写性能好**

   HDD 喜欢顺序写，SSD/NVMe 也更容易处理大块连续写。append-only 可以把随机更新变成顺序追加，减少 seek、写放大和元数据抖动。

2. **崩溃恢复简单**

   追加日志可以从头扫描到尾，遇到最后一条不完整 record 就截断到 last good offset。只要每条 record 有 length、checksum、magic、sequence number，恢复逻辑就很直接。

3. **提交语义清楚**

   WAL、binlog、commit log 都可以把“记录追加到某个 durable offset”作为提交点。比起原地更新复杂结构，append-only 更容易定义 durable boundary。

4. **并发写更容易组织**

   多个线程可以把记录排队到一个 append writer，由它顺序写入。再配合 group commit，一次 `fsync()` 提交多个记录，吞吐比每个线程随机写再 fsync 更好。

5. **避免原地更新 torn state**

   原地更新 B-tree、索引页或对象文件时，崩溃可能停在中间状态。append-only 先写新记录，再通过 checkpoint、manifest、索引切换来发布新状态，恢复时可以选择最后一个完整提交点。

6. **便于复制和追赶**

   分布式系统里，append-only log 天然有 offset。follower 可以从某个 offset 继续拉取，消费者可以记录消费位点，恢复和重放都方便。

7. **便于审计和回放**

   append-only 保存了历史事件。出现 bug 时可以从日志重放，重建状态或定位哪条操作造成问题。

它也有代价：

- 文件会持续增长，需要 segment rolling 和 compaction。
- 删除或更新只是追加 tombstone 或新版本，空间回收滞后。
- 需要处理重复记录、乱序恢复、部分写、checksum mismatch。
- 需要 checkpoint 或索引，否则从头重放太慢。

所以 append-only 不是因为它简单到没有问题，而是因为它把最难的原地一致性问题，转化成了更可控的追加、校验、截断、重放和压缩问题。对日志系统来说，这个交换通常很划算。

## Q021. O_APPEND 是否能保证多进程并发追加的原子性？

**回答：**

在本地 POSIX 风格文件系统上，`O_APPEND` 通常能保证“每次 `write()` 前把文件 offset 移到文件末尾，并且 offset 调整和写入作为一个原子步骤完成”。这意味着多个进程同时以 `O_APPEND` 写同一个普通文件时，不会出现两个进程都先看到同一个 EOF，然后把数据写到同一位置互相覆盖的典型竞态。

但这个保证很容易被误解。

第一，`O_APPEND` 保证的是单次 `write()` 的追加位置原子，不保证应用层“记录”原子。如果一条日志记录被拆成多次 `write()`：

```text
write(header)
write(payload)
write(checksum)
```

另一个进程可能插进来写自己的记录。最终文件里可能变成：

```text
A.header
B.header
B.payload
A.payload
A.checksum
B.checksum
```

从文件系统角度看，每次 `write()` 都追加成功；从日志格式角度看，记录已经交错损坏。所以多进程日志如果依赖 `O_APPEND`，应尽量把一条完整记录放进一次 `write()` 或 `writev()`，或者由单 writer 线程/进程串行化。

第二，`O_APPEND` 不保证持久性。写入返回成功不代表数据已经落盘。要保证 crash 后恢复，需要 `fsync()`、group commit、checksum、length、commit marker 等协议。

第三，网络文件系统可能不满足同样语义。Linux man-pages 特别提到，NFS 上 `O_APPEND` 可能导致文件损坏，因为 NFS 协议本身不支持真正的 append，客户端内核只能模拟，多个客户端并发追加时会有竞态。

第四，`O_APPEND` 不保证记录顺序符合业务因果。多个进程并发写入时，文件顺序取决于内核实际执行每次写的顺序，而不是业务事件产生的顺序。需要严格顺序时，要引入序列号、单 writer、队列或锁。

一句话回答：本地文件系统上，`O_APPEND` 能保证单次 `write()` 的追加 offset 原子；它不能保证多次写组成的记录不交错，不能保证跨机器文件系统语义，不能保证持久化，也不能保证业务顺序。

## Q022. 多线程写同一个文件描述符需要注意哪些问题？

**回答：**

多个线程写同一个文件描述符，最容易出问题的是共享 file offset、记录边界、错误处理和关闭时机。

首先要分清 file descriptor 和 open file description。多个线程共享同一个 fd 时，它们共享同一个 open file description，其中包括当前文件 offset 和文件状态标志。线程 A 写完以后，offset 会推进；线程 B 的下一次写会从推进后的位置开始。现代 Linux 对常规文件的 offset 更新已经按 POSIX 要求修复了历史竞态，但这不代表应用层记录顺序自动正确。

需要注意这些点：

1. **写入顺序不确定**

   两个线程同时 `write(fd, ...)`，谁先进入内核，谁先写入，并不是由业务代码里的事件时间决定的。日志、WAL、binlog 如果需要全局顺序，最好用单 writer 队列或显式锁。

2. **记录可能交错**

   如果一条记录拆成多次 `write()`，其他线程可以插进来。要么把一条记录编码成一个连续 buffer 后一次写，要么用 `writev()`，要么在应用层加锁。

3. **partial write 仍然要处理**

   文件写入不应该默认“要么全写，要么失败”。磁盘满、配额、信号、非阻塞 fd、管道/socket 都可能产生 partial write。多线程下，如果没有把剩余部分写完，就会造成记录截断。

4. **不要边写边 close**

   一个线程关闭 fd，另一个线程还在写，会引入 fd 复用风险。fd 是进程内的小整数，关闭后可能被别的 open/socket 复用。后续写可能写到完全不同的对象上。应通过生命周期管理保证所有 I/O 完成后再 close。

5. **fsync 和 ack 要配合**

   一个线程写，另一个线程 fsync。必须定义清楚 fsync 覆盖哪些记录，哪些记录可以返回成功。常见做法是维护 durable offset：fsync 成功后，只 ack offset 不超过 durable offset 的记录。

6. **共享用户态 buffer 有风险**

   调用 `write()` 时，内核会从用户 buffer 复制数据。调用返回前，不要让其他线程修改这段 buffer。异步 I/O 或 direct I/O 场景更要小心 buffer 生命周期。

7. **O_APPEND 只解决追加位置，不解决协议**

   `O_APPEND` 能避免共享 offset 造成覆盖，但不能解决多次写交错、记录顺序、持久化边界和恢复校验。

工程上更稳的模式是：多个线程把完整 record 放入队列，一个 writer 线程批量 `writev()`，再 group fsync，并把持久化进度发布给等待 ack 的线程。这样顺序、错误处理和恢复协议都更容易写对。

## Q023. 如何处理 partial write？

**回答：**

partial write 指 `write(fd, buf, len)` 返回了一个大于 0 但小于 `len` 的值。它不是异常情况，也不应该当成完整写入。正确处理方式是：移动 buffer 指针和剩余长度，继续写剩下的部分。

伪代码大致是：

```c
size_t off = 0;
while (off < len) {
    ssize_t n = write(fd, buf + off, len - off);
    if (n > 0) {
        off += n;
        continue;
    }
    if (n == -1 && errno == EINTR) {
        continue;
    }
    if (n == -1 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
        wait_writable(fd);
        continue;
    }
    return error;
}
```

几个细节要注意：

1. **不要重写整个 buffer**

   如果第一次写入了前 100 字节，第二次应该从第 101 字节开始写。重试整个 buffer 会造成重复数据。

2. **正数返回优先于 errno**

   只要 `write()` 返回正数，就说明这些字节已经被接受。即使信号发生，也不能把这次调用当失败处理。

3. **EINTR 只在没有写入任何字节时返回错误**

   Linux man-pages 说明，如果写入前被信号中断，会返回 `EINTR`；如果已经写入至少一个字节，会返回实际写入数量。

4. **非阻塞 fd 要等待可写**

   `EAGAIN` 或 `EWOULDBLOCK` 表示现在不能继续写。应等待 epoll/poll/select 通知，而不是忙等。

5. **普通文件也可能 short write**

   很多人只在 socket 上处理 partial write，普通文件也可能发生，比如磁盘空间不足、资源限制、信号中断、文件大小限制。

6. **direct I/O 的 partial error 更危险**

   对 `O_DIRECT` 写入，如果返回错误，不一定代表没有任何数据写入。man-pages 提到，direct I/O 出错时，目标 offset 上的数据应视为不一致。存储系统通常要通过 checksum、版本号或重写整块处理。

7. **日志 record 要有恢复校验**

   即使写循环处理正确，崩溃也可能发生在 record 中间。append-only log 应给每条 record 加 length、checksum、magic 或 commit marker。恢复时截断到最后一条完整 record。

总结：partial write 是写入 API 的正常语义，不是边缘异常。所有写固定长度数据的代码都应该有 `write_all` 这种循环，并且把“写完”和“fsync 成功”分成两个阶段。

## Q024. 如何处理 EINTR、EAGAIN、short read 和 short write？

**回答：**

这几个情况都说明一个事实：I/O API 很少保证“一次调用完成你想要的全部工作”。处理它们要按返回值和 errno 分开。

**EINTR**

`EINTR` 表示系统调用在完成前被信号中断。对 `read()` 来说，如果没有读到任何数据就被信号中断，可以返回 `-1/EINTR`。对 `write()` 也是类似：如果写入前被中断，返回 `EINTR`；如果已经写了一部分，通常返回已写字节数。

处理原则是：如果返回 `-1` 且 errno 是 `EINTR`，通常重试；如果返回正数，先消费这些字节，不要因为信号存在就丢掉进度。

**EAGAIN / EWOULDBLOCK**

这通常出现在 nonblocking fd 上，表示现在读不到或写不进去。处理方式不是立即失败，也不是忙循环，而是注册到 epoll/poll/select，等待可读或可写后继续。

socket 上要同时处理 `EAGAIN` 和 `EWOULDBLOCK`，因为 POSIX 允许二者值不同。

**short read**

`read(fd, buf, n)` 返回小于 `n` 的正数不是错误。原因可能是当前可用数据不足、pipe/socket 边界、终端输入、接近 EOF、被信号打断等。

如果协议需要读满 N 字节，比如 frame header 或 record payload，就要循环读，直到：

- 累计读满 N 字节；
- 返回 0，表示 EOF；
- 返回不可恢复错误；
- 超时或取消。

如果 EOF 出现在读 record 中间，应报告 truncated record，而不是把半条数据交给上层解析。

**short write**

`write()` 返回小于请求长度的正数，也不是错误。调用方要继续写剩余部分。不能把 short write 当完整成功，也不能从头重写。

可以把处理规则压缩成一张表：

| 情况 | 正确处理 |
| --- | --- |
| `n > 0` | 消费进度，继续处理剩余部分 |
| `n == 0` on read | EOF；如果正在读固定长度对象，就是截断 |
| `-1/EINTR` | 通常重试 |
| `-1/EAGAIN` | 等待 fd 可读/可写后重试 |
| `-1/其他错误` | 停止当前操作，按协议清理状态 |

面试时可以补一句：不要写“调用一次 read/write 就完成”的代码。网络协议、日志恢复、文件复制、WAL 写入，都应该有明确的 `read_exact` 和 `write_all` 逻辑。

## Q025. 文件损坏通常分为哪些类型？

**回答：**

文件损坏可以按层次分。这样分类比只说“文件坏了”更有用，因为不同损坏需要不同恢复策略。

1. **物理或块级损坏**

   底层设备某些 sector/page 读不出来，返回 I/O error；或者读出来但内容发生 bit flip。可能来自磁盘坏块、SSD 介质问题、控制器 bug、内存错误、DMA 问题。

2. **torn write**

   写入只完成一部分。比如应用以为写了 16KB record，但崩溃时只有前 4KB 或前 12KB 到达存储。底层设备的原子写粒度通常比应用 record 小，不能假设大 record 原子落盘。

3. **乱序持久化**

   应用按 A 再 B 的顺序写，崩溃后 B 持久了，A 没持久。文件系统、块层、设备 cache 都可能重排。没有 barrier/flush/fsync 协议时，顺序不可靠。

4. **元数据和数据不一致**

   文件大小更新了，但新数据块没写完；目录项存在，但文件内容没落盘；索引指向新 segment，但 segment 没完整写入。这类问题常见于没有正确 fsync 文件和目录。

5. **应用级格式损坏**

   文件系统结构没坏，但应用格式坏了。比如 record length 错、checksum mismatch、magic 不对、schema version 不支持、header 写了 payload 没写完。

6. **截断**

   文件比预期短。可能是写入中断、复制未完成、truncate 调错、对象上传未完成。日志系统通常把尾部截断到最后一条完整 record。

7. **尾部垃圾**

   文件后面有多余 bytes。可能来自预分配空间没有正确标记有效长度，或者旧数据残留。恢复时不能把文件物理大小当成有效数据长度。

8. **空洞或零填充异常**

   sparse file 的 hole 被读成零，这可能是预期行为，也可能是数据块没有真正写入。恢复逻辑如果不能区分“合法零”和“缺失数据”，会误判。

9. **跨文件不一致**

   数据文件更新了，索引文件没更新；manifest 指向新版本，实际文件缺失；多个 shard 中只有一部分提交。这是应用协议问题，不是单个文件 checksum 能完全解决的。

10. **语义损坏**

    bytes 格式正确，checksum 也通过，但业务语义不对。比如重复提交、顺序倒置、旧版本覆盖新版本、事务只应用了一半。需要 sequence number、term、epoch、事务 ID 或幂等协议来发现。

工程上常见防护是分层的：底层用 checksum/ECC，文件格式用 magic、length、version、CRC，日志协议用 commit marker 和 fsync，分布式系统再用副本、quorum、term 和重放。

## Q026. 如何设计 crash test 验证写入协议的正确性？

**回答：**

crash test 的目标不是证明“正常写入能跑通”，而是证明任意崩溃点之后，恢复出来的状态仍然满足协议不变量。写入协议如果只靠普通单元测试，很容易漏掉 fsync、rename、目录项、写乱序这些问题。

设计步骤可以这样做：

1. **先定义不变量**

   例如：

   - 已 ack 的 record 必须可恢复。
   - 未 ack 的 record 可以丢，但不能变成半条合法 record。
   - 恢复后 offset 单调递增。
   - checksum 不通过的 record 必须被丢弃。
   - manifest 指向的 segment 必须存在且完整。
   - 不能出现重复提交或乱序提交。

2. **把写入协议拆成步骤**

   例如：

   ```text
   write record header
   write payload
   write checksum
   fsync data file
   rename manifest.tmp -> manifest
   fsync directory
   ack client
   ```

   crash test 要在这些步骤之间插入崩溃点。

3. **生成小而覆盖关键路径的 workload**

   不要一开始就跑复杂业务。先覆盖创建文件、写 record、append 多条 record、rename、truncate、rotate segment、更新 manifest、删除旧文件。很多 crash consistency bug 可以用很短的操作序列复现。

4. **模拟崩溃并重新挂载/恢复**

   真正好的测试不是 kill 进程后继续读 page cache，而是让文件系统经历类似断电后的恢复。可以用虚拟块设备、快照、dm-flakey、QEMU、CrashMonkey/Ace 这类工具，或者在测试块层拦截并选择已持久写集合。

5. **检查恢复结果**

   每个崩溃点恢复后，运行 recovery，再检查不变量。不要只看进程能启动。要检查文件内容、record 序列、manifest、索引、已 ack 集合、未 ack 集合。

6. **覆盖文件系统和挂载参数**

   ext4 ordered、writeback、XFS、btrfs、tmpfs、NFS、不同 barrier 设置，行为都可能不同。至少要在目标生产环境的文件系统和挂载参数上测试。

7. **注入底层错误**

   除了断电，还要测 ENOSPC、EIO、fsync 失败、rename 失败、partial write、short read、checksum mismatch、尾部垃圾、文件空洞。

8. **记录可复现证据**

   每个失败样例都要保存 workload、崩溃点、预期 durable state、实际恢复 state、文件 hex dump。否则 crash test 很难调。

OSDI 2018 的 B3/CrashMonkey 思路很值得借鉴：把文件系统操作序列限制在一个有界范围内，穷举这些小 workload，并在执行中模拟 power-loss crash，再检查恢复状态。对应用写入协议，也可以用同样思路：小 workload，密集 crash point，严格 oracle。

## Q027. 为什么单元测试很难覆盖真实断电场景？

**回答：**

单元测试通常运行在一个“太干净”的环境里。真实断电不是简单的进程退出，它会同时影响应用、内核 page cache、文件系统 journal、块层队列、设备 write cache 和挂载恢复流程。

难点主要有这些：

1. **单元测试看不到 page cache 丢失**

   测试写完文件后马上读，往往读的是 page cache。即使数据没落盘，测试也能通过。断电后 page cache 会消失，这是普通进程内测试模拟不了的。

2. **kill 进程不等于断电**

   `kill -9` 只杀应用进程。内核还活着，后台 writeback 可能继续把 dirty pages 写到磁盘。真正断电时，这些脏页可能全部丢失。

3. **文件系统会做复杂重排**

   journaling、delayed allocation、writeback、ordered mode、barrier、device cache 都会影响崩溃后状态。单元测试通常只观察系统调用返回值，不观察落盘顺序。

4. **设备 cache 很难模拟**

   磁盘或 SSD 可能已经向 OS 报告写入完成，但数据仍在 volatile cache。单元测试无法简单判断设备内部哪些写真正持久。

5. **崩溃点太多**

   一个写入协议可能有几十个步骤，每个步骤前后都可能断电。单元测试通常只测少数正常路径和错误路径，不会穷举崩溃点。

6. **跨文件原子性很难测**

   数据文件、索引文件、manifest、目录项可能处于不同持久化状态。普通测试很少构造“数据文件已落盘，manifest 未落盘”这种状态。

7. **错误延迟报告**

   写入错误可能延迟到 `fsync()` 或 `close()` 才出现。单元测试如果 mock `write()` 成功，就可能漏掉 writeback error、ENOSPC、EDQUOT、EIO。

8. **硬件和文件系统差异大**

   tmpfs、ext4、XFS、btrfs、NFS、云盘、NVMe 的行为不一样。在开发机通过的测试，不能自动推导到生产环境。

所以单元测试适合验证编码、解码、状态机、恢复函数的局部逻辑；真实 crash consistency 还需要故障注入、崩溃点枚举、块层模拟、文件系统矩阵和恢复后 oracle。

## Q028. 如何模拟进程 kill、机器断电、磁盘写乱序？

**回答：**

这三类故障不在同一层，模拟方法也不一样。

**模拟进程 kill**

最简单的是在写入协议的关键步骤之间插入故障点，然后 `SIGKILL` 进程：

```text
after write header
after write payload
after fsync data
after rename
before ack
```

重启进程后运行 recovery，检查状态。这个测试能发现应用没有处理 half record、临时文件、未完成 rename、重复提交等问题。但它不能模拟 page cache 丢失，因为内核还会继续写回脏页。

**模拟机器断电**

更接近真实断电的方式是用虚拟机、虚拟块设备或快照。常见做法：

- 在 QEMU/虚拟机里运行 workload，然后强制断电虚拟机。
- 用块设备快照记录某个时刻的持久状态，恢复后重新挂载文件系统。
- 使用测试框架在文件系统操作中间模拟 power loss，然后检查重挂载后的状态。

关键是不能让内核优雅 sync，也不能让进程正常 close。否则测到的是正常关机，不是断电。

**模拟磁盘写乱序**

写乱序更低层。可以在块层记录应用发出的写请求，然后人为选择一个符合或不符合设备顺序约束的子集作为“崩溃后持久状态”。常见手段包括：

- 使用 dm-flakey、device mapper、故障注入块设备。
- 用 CrashMonkey/Ace 这类 crash-consistency 测试工具。
- 在用户态模拟一个块设备或文件系统，把写请求记录下来后重排、丢弃、截断。
- 在存储引擎测试中，用 mock disk 明确建模 reorder、partial write、lost flush、failed fsync。

这三类测试的覆盖范围不同：

| 故障 | 能发现什么 | 局限 |
| --- | --- | --- |
| kill 进程 | 应用恢复逻辑、未完成 record、重复提交 | 不能模拟 page cache 丢失 |
| 机器断电 | 脏页丢失、journal recovery、目录项持久化 | 成本高，复现慢 |
| 写乱序 | 缺少 flush/barrier/fsync 的协议 bug | 需要专门工具或模型 |

一个成熟系统通常三种都做：单元测试验证恢复函数，kill 测试验证进程级故障，power-loss/写乱序测试验证真正的持久化协议。

## Q029. 为什么不能只依赖操作系统缓存来保证持久性？

**回答：**

因为操作系统缓存主要是性能机制，不是持久化协议。page cache 可以让写入更快返回、让后续读取更快命中，但它没有承诺“应用返回成功后，断电也不会丢”。

普通 buffered write 之后，数据可能只在内存 dirty page 里。后台 writeback 会在稍后某个时间写出，触发条件可能是时间、内存压力、dirty ratio、显式 sync、文件系统策略。机器在写回前断电，这些 dirty pages 就没了。

只依赖 OS cache 会带来几类问题：

1. **持久化时机不可控**

   应用不知道哪条 record 已经落盘，哪条还在内存。崩溃后无法给用户明确承诺。

2. **写入顺序不可控**

   OS 和设备可能合并、重排写入。没有 `fsync()`、barrier、flush、journal 协议，应用认为的顺序不一定是落盘顺序。

3. **错误可能延迟**

   `write()` 成功不代表空间已保留或数据已写入。ENOSPC、EIO、EDQUOT 可能到后续 `write()`、`fsync()`、`close()` 才出现。

4. **目录项不一定持久**

   新建文件、rename、unlink 都是目录元数据变化。只写文件内容，不 fsync 目录，崩溃后文件名映射可能不是预期状态。

5. **设备 cache 还在下面**

   即使 OS 把脏页交给设备，设备也可能放在 volatile write cache。没有 flush/FUA，断电仍可能丢。

6. **应用恢复没有边界**

   如果没有 durable offset、commit marker、checksum、sequence number，恢复时不知道哪些数据可以信，哪些要丢弃。

正确做法是把 OS cache 当作性能层，把持久性作为协议层来设计。应用要明确什么时候 `fsync()`，什么时候 ack，如何处理 fsync 失败，如何恢复 partial record，如何同步目录，如何校验最后一个提交点。

一句话：page cache 能提高写入性能，不能替你定义 durable commit。

## Q030. 文件截断和文件空洞会对恢复流程带来什么风险？

**回答：**

文件截断和文件空洞都会让恢复流程误判“文件里到底有哪些有效数据”。如果恢复逻辑只看文件大小或只看读出来的零，很容易出错。

**文件截断的风险**

截断可能来自崩溃、手动 `truncate`、复制中断、对象下载未完成、日志 segment 没写完。恢复时常见风险是：

1. **半条 record**

   header 写了，payload 没写完；length 声明 4KB，实际只剩 1KB。恢复逻辑必须识别 truncated record，并截断到上一条完整记录。

2. **索引越界**

   索引文件或 manifest 记录了某个 offset，但数据文件被截短。恢复时不能盲信索引，要校验 offset 是否小于文件大小、record 是否完整。

3. **checksum 缺失**

   record 尾部 checksum 没写完。不能把缺 checksum 的 record 当成功提交。

4. **截断和合法删除混淆**

   有些系统会主动 truncate segment 或 compact 文件。恢复时要能区分“协议内的合法 truncate”和“异常导致的短文件”。

**文件空洞的风险**

空洞是 sparse file 中未分配的区域，读取会返回零。风险在于，零不一定表示应用写过零，也可能表示这段根本没有分配。

常见问题：

1. **把 hole 当合法零数据**

   如果 record payload 恰好允许全零，恢复逻辑可能把未写区域当成合法数据。需要 checksum、length、magic、sequence number 辅助判断。

2. **预分配空间被误读**

   日志系统可能 `fallocate()` 预分配 1GB segment，但实际只写到 100MB。如果恢复按文件 size 扫描，会读到后面大量零或 unwritten extents。必须有有效长度、commit offset 或最后完整 record 判断。

3. **SEEK_HOLE/SEEK_DATA 不完全可靠**

   Linux 支持用 `lseek(SEEK_HOLE/SEEK_DATA)` 查找 hole 和数据区，但文件系统不一定精确报告。man-pages 也说明，文件系统可以用简单实现把 hole 报得很粗。恢复协议不能完全依赖它。

4. **打洞后旧索引失效**

   如果 compaction 或清理使用 hole punching，旧索引仍指向被打洞区域，读取会得到零而不是原数据。恢复时要检查索引版本和 segment 生命周期。

5. **跨文件复制破坏稀疏性**

   备份或迁移工具如果不保留 hole，可能把稀疏文件变成真实占用空间的大文件；反过来，如果错误制造 hole，恢复会读到零数据。

稳妥的恢复流程不应只依赖文件大小。它应该从已知起点扫描 record，检查 magic、length、checksum、sequence，遇到截断、空洞、checksum mismatch 或非法 length 就停止，并截断到 last good offset。对预分配文件，还要单独维护 durable offset 或 commit marker。

## Q031. truncate 到最后一个有效 record 的策略有什么优缺点？

**回答：**

truncate 到最后一个有效 record，是 append-only log、WAL、segment 文件里很常见的恢复策略。基本做法是：启动恢复时从某个可信起点开始顺序扫描 record，逐条校验 magic number、version、length、checksum、sequence number、LSN 或 commit marker；一旦遇到半条 record、非法 length、checksum mismatch、sequence 断裂、文件尾部不完整，就停止扫描，把文件截断到上一条完整可信 record 的结束 offset。

它解决的是一个很具体的问题：崩溃可能发生在最后一条或最后几条 record 写入过程中，磁盘上留下了“看起来像文件内容、但协议上没有提交完成”的尾巴。与其让后续读取每次都遇到坏尾巴，不如恢复时一次性清理。

优点主要有几个：

1. **恢复逻辑简单**

   只要 record framing 设计得好，恢复流程可以非常机械：顺序扫描，验证，记录 `last_good_offset`，遇到第一个坏 record 就 truncate。它不需要猜测，也不需要复杂修复。

2. **能清理崩溃尾巴**

   header 已写但 payload 没写完、payload 已写但 checksum 没写完、length 已写但实际字节不足，这些都可以归类为“未完成 record”，直接丢弃。

3. **后续读取更干净**

   恢复完成后，文件末尾重新落在 record 边界上。正常读取路径不需要反复处理同一个坏尾巴。

4. **适合 append-only 设计**

   append-only log 通常只允许尾部增长，不允许中间随机修改。崩溃损坏也通常集中在尾部，因此 truncate tail 是自然选择。

5. **可以和 index 重建配合**

   先把 log 修到最后一个有效 record，再根据 log 重建 index，可以避免 index 指向无效 tail。

但它也有明显缺点：

1. **可能丢掉尾部已写入但未确认的数据**

   如果某条 record 实际已经写到磁盘，但 checksum 或 commit marker 没来得及写，恢复时会把它当作未提交数据丢掉。这个丢失是合理的，因为系统没有足够证据证明它已经完成提交。

2. **遇到中间损坏会比较棘手**

   truncate tail 默认“第一个坏 record 之后都不可信”。如果文件中间某一条损坏，而后面还有很多有效 record，这个策略会把后面的 record 一起丢掉。它更适合尾部损坏，不适合任意位置损坏。

3. **依赖 record 校验设计**

   如果 record 没有 checksum、sequence、magic、length 上限，恢复逻辑就很难判断“坏”还是“合法但内容特殊”。没有强 framing，truncate 策略容易误判。

4. **对预分配和稀疏文件要小心**

   如果 segment 预分配了 1GB，但只写入了前 128MB，文件 size 不能代表有效数据边界。恢复时必须依赖 durable offset、commit marker 或最后有效 record，而不是依赖 `stat()` 看到的 size。

5. **truncate 本身也要纳入持久化协议**

   恢复进程调用 `ftruncate()` 后，如果希望“截断结果”在再次崩溃后仍然可见，也需要考虑对文件和目录元数据的同步。否则可能出现“恢复时截断过，但又崩了，重启后旧尾巴又可见”的情况，具体取决于文件系统和写回时机。

所以面试里可以这样概括：truncate 到最后一个有效 record 是一种非常实用的 tail repair 策略，前提是文件格式有明确 record 边界和校验字段。它不是数据修复算法，而是把未完成提交的尾部数据从协议状态里移除。

## Q032. 如果 index 文件已经写入但 log 文件没有写入，恢复时应该相信谁？

**回答：**

通常应该相信 log，而不是相信 index。更准确地说，恢复时应该把 log、WAL 或主数据文件视为权威数据，把 index 视为派生数据。index 只有在能被 log 里的 record 验证时才可信。

原因在于 index 的作用通常是加速查找，例如 `key -> offset`、`LSN -> file offset`、`timestamp -> segment`。它本身不应该是提交事实的来源。如果 index 文件已经写入并 fsync，但 log 文件对应 record 没有写入或没有 fsync，那么 index 可能指向一个根本不存在的 record，或者指向一条只写了一半的 record。

恢复时常见处理方式是：

1. **先确定 log 的 durable boundary**

   顺序扫描 log，验证每条 record 的 magic、length、checksum、LSN、commit marker。扫描结束得到 `last_good_offset` 或最后一个 durable LSN。

2. **校验 index 条目是否落在 durable boundary 内**

   如果 index 里有 offset 超过 `last_good_offset`，这类条目必须丢弃。它们最多说明“曾经准备写入”，不能说明“已经提交成功”。

3. **校验 index 指向的 record 内容**

   即使 offset 没越界，也要确认 offset 处确实是预期 key、预期 record type、预期 LSN。不能只因为 index 里写了这个 offset 就信。

4. **必要时直接重建 index**

   如果 index 和 log 出现分歧，很多系统会选择丢弃 index，从 log 扫描重建。这样恢复慢一些，但语义清楚。

这里的关键是写入顺序。正确的持久化协议通常应该是：

1. 写 log record。
2. `fsync()` 或 `fdatasync()` log，确保 record 达到 durable。
3. 再更新 index、manifest 或内存状态。
4. 如需 index 也持久化，再同步 index。

如果顺序反过来，index 就可能“跑在 log 前面”。崩溃恢复时绝不能因为 index 先落盘，就把 log 中不存在的数据当作已提交。

一句话：index 可以帮助定位数据，但不能替代数据本身。log 没有证明提交完成时，index 条目只能被视为可疑缓存。

## Q033. 如果 log 文件写入但 index 文件丢失，如何恢复？

**回答：**

如果 log 文件完整而 index 文件丢失，最常见的恢复方式是扫描 log 并重建 index。这个设计非常常见，因为 log 通常包含系统状态变化的完整历史，而 index 只是为了提高查询速度。

恢复流程一般是：

1. **找到扫描起点**

   如果有 checkpoint 或 snapshot，可以从最近一次 checkpoint 后面的 log 开始扫。没有 checkpoint 时，就从 log 起始位置扫。

2. **顺序读取 durable record**

   对每条 record 检查 magic、version、length、checksum、LSN、record type。遇到坏尾巴时停止，并 truncate 到最后一个有效 record。

3. **按 record 语义重放**

   如果 record 是 `put(key, value_offset)`，就更新 index 中 key 的位置；如果是 delete/tombstone，就删除或标记 key；如果是 compaction 结果，就更新 segment 引用。

4. **处理重复和幂等**

   崩溃可能发生在“record 已写入，但 index 未更新”之间。重放时要用 LSN、sequence number、term、epoch 或 transaction id 保证重复应用不会破坏状态。

5. **写出新的 index**

   重建完成后，可以把 index 写到临时文件，fsync 文件，rename 到正式路径，再 fsync 目录。这样下一次启动就不必全量扫描。

这个策略的代价是启动恢复可能变慢。log 很大时，全量扫描会消耗 I/O 和 CPU。因此实际系统通常会配合 checkpoint、snapshot、segment manifest、稀疏索引或定期 index snapshot，把重建范围限制在最近一段日志。

但语义上，这是一种很干净的设计：只要 log 是完整、可校验、可重放的，index 丢失不是数据丢失，只是性能状态丢失。恢复后系统可以重新获得查询能力。

## Q034. 为什么很多系统把 index 当作可重建数据？

**回答：**

因为 index 的核心职责通常是“让查找更快”，而不是“定义事实”。在很多存储系统里，真正的事实存在于 append-only log、WAL、SSTable、数据页、manifest 或 checkpoint 里；index 是从这些事实推导出来的加速结构。

把 index 设计成可重建数据，有几个工程收益：

1. **降低 crash consistency 难度**

   如果 index 也是权威数据，那么每次写入都要保证“数据文件、index 文件、manifest 文件”三者同时一致。这个协议很难写对。把 index 变成派生数据后，只需要优先保证 log 或主数据文件可恢复，index 崩了可以重建。

2. **减少 fsync 压力**

   权威数据通常需要频繁 fsync。index 如果每次更新都 fsync，会显著增加写放大和延迟。可重建 index 可以批量落盘，甚至只在 checkpoint 时写出。

3. **更容易处理损坏**

   启动时发现 index checksum 错、版本不匹配、offset 越界，可以直接丢弃 index 并重建，而不是把整个数据库判为不可用。

4. **适合 compaction 和重组**

   LSM compaction、日志清理、segment merge 都会改变数据物理位置。index 如果是派生结构，就可以跟随新的数据布局重建，不必把旧位置当作不可变真相。

5. **方便版本升级**

   index 格式可以变化。只要主数据格式仍可读，升级后可以用新格式生成新 index。

当然，这个前提是 index 真的可由权威数据完整推导。如果某个 index 里保存了唯一存在的信息，例如唯一的二级属性、唯一的排序元数据、唯一的外部引用，那它就不是缓存，而是数据本身。此时不能随便删除重建。

所以判断标准很简单：丢掉 index 后，系统能不能从 log、SSTable、数据页或 checkpoint 里重新得到同样的逻辑状态？能，就是可重建 index；不能，它就是权威数据的一部分。

## Q035. LSM、WAL、database checkpoint 都如何使用 fsync？

**回答：**

LSM、WAL 和 database checkpoint 都会用到 fsync，但它们使用 fsync 的位置和目的不同。面试时要先区分：fsync 不是“性能优化开关”，它是持久化协议里的提交边界。

**WAL 如何使用 fsync**

WAL 的核心规则是 write-ahead：先把变更记录写入日志并持久化，再允许对应的数据页、索引或内存状态被认为可恢复。

典型流程是：

1. 事务生成 WAL record。
2. WAL append 到日志文件。
3. 在需要 durability 的提交点调用 `fsync()` 或 `fdatasync()`。
4. fsync 成功后，才向客户端返回“提交成功”。
5. 崩溃恢复时，从最后一个 checkpoint 后的 WAL 开始 redo 或 undo。

为了降低延迟，数据库通常不会每个事务单独 fsync，而是 group commit：多个事务共享一次 WAL fsync。这样单个事务仍然以 WAL fsync 成功作为 durable commit 边界，但底层 I/O 次数减少了。

**LSM 如何使用 fsync**

LSM 写入路径通常是 WAL + memtable：

1. 写 WAL。
2. 根据配置同步 WAL。
3. 更新 memtable。
4. memtable 满后变成 immutable memtable。
5. flush 成 SSTable。
6. fsync SSTable 文件。
7. 更新 manifest 或 version metadata。
8. fsync manifest 和必要的目录项。

这里有两个重点。第一，WAL 保证 memtable 还没 flush 成 SSTable 时也能恢复。第二，SSTable 写完后，只有当 SSTable 文件和 manifest 更新都持久化，系统才能认为这个新的文件集合是可恢复状态。

如果 manifest 先持久化但 SSTable 没持久化，恢复时会引用不存在或不完整的 SSTable。如果 SSTable 已持久化但 manifest 没更新，恢复时最多看不到新 SSTable，不能让系统状态损坏。

**database checkpoint 如何使用 fsync**

checkpoint 的目标是把某一时刻的数据库状态落成一个较短的恢复起点。它通常不直接替代 WAL，而是缩短需要重放的 WAL 范围。

常见流程是：

1. 记录 checkpoint 开始时的 LSN。
2. 把脏页、快照文件或状态文件写出。
3. fsync checkpoint 相关文件。
4. 原子发布 checkpoint metadata，例如 rename manifest。
5. fsync 包含 manifest 的目录。
6. 确认 checkpoint durable 后，才能删除或截断更早的 WAL。

数据库还有一条重要约束：不能让某个数据页的持久化状态超过 WAL 能恢复的范围。也就是说，数据页落盘之前，对应 WAL 必须已经 durable。否则崩溃后数据页可能包含一个没有日志记录的变更，恢复逻辑无法解释。

所以三者的共同点是：fsync 用来建立“崩溃后还能从这里恢复”的边界；不同点是 WAL fsync 保护提交，LSM fsync 保护新文件和 manifest，checkpoint fsync 保护新的恢复基线。

## Q036. 云盘、本地盘、网络文件系统对 fsync 语义可能有什么差异？

**回答：**

`fsync()` 在应用看到的接口上是同一个系统调用，但它下面经过的路径可能完全不同。本地盘、云盘、网络文件系统的差异，会体现在延迟、错误返回、缓存层级、复制策略和故障模型上。

**本地盘**

本地盘路径通常是：应用 -> page cache -> 文件系统 -> block layer -> 设备队列 -> SSD/HDD/NVMe。`fsync()` 需要把文件脏数据、必要元数据以及设备 volatile cache 中的数据推到足够持久的位置。

本地盘的关键变量包括：

- 文件系统是否正确使用 barrier、flush、FUA。
- 设备是否有 volatile write cache。
- SSD 是否有断电保护电容，也就是 PLP。
- RAID 控制器是否有电池保护缓存。
- 挂载参数是否削弱了同步语义。

在可靠本地盘上，fsync 语义相对直接；但如果设备或控制器谎报 flush 完成，应用层也很难挽救。

**云盘**

云盘通常不是一块直接插在机器上的物理盘，而是虚拟块设备。应用调用 `fsync()` 后，请求可能经过宿主机、虚拟化层、网络、存储服务、复制层、后端 SSD/HDD。

它的差异主要是：

- 延迟尾部更明显，可能受网络和多租户影响。
- durability 取决于云厂商对“写入确认”的定义。
- 有些云盘会在多个副本或多个 fault domain 上复制后才确认。
- 有些场景下缓存、快照、复制、限流会改变 fsync 延迟。
- 磁盘性能可能受 IOPS、吞吐、队列深度、突发额度影响。

所以云盘上不能只看平均 fsync 延迟，还要看 p99、p999，以及 provider 对崩溃、宿主机故障、可用区故障的持久性承诺。

**网络文件系统**

NFS、SMB、CephFS、GlusterFS 等网络文件系统又多了一层文件语义转换。`fsync()` 可能意味着客户端把 dirty data 发给服务器，也可能还涉及服务器端缓存、服务器磁盘 flush、集群复制、元数据服务确认。

它的风险包括：

- 客户端缓存和服务器状态可能短时间不一致。
- 多客户端并发写需要额外一致性协议或锁。
- 服务端 `sync`/`async` 导出配置会影响持久性。
- 网络分区、重试、server failover 会让错误延迟出现。
- 某些 POSIX 语义可能只是近似实现。

网络文件系统尤其不能简单假设“本地 ext4 上通过的 WAL 协议，放到 NFS 上也一样正确”。要看具体协议版本、挂载选项、服务端实现、底层存储和故障测试结果。

一句话：fsync 是应用和内核之间的接口，不是对所有底层介质的统一魔法。真正的持久性要看从文件系统到设备或远端服务的整条链路。

## Q037. NFS 上的 fsync 语义为什么需要格外谨慎？

**回答：**

NFS 上的 fsync 需要格外谨慎，因为 NFS 不是本地文件系统。它有客户端缓存、服务端缓存、网络重试、多客户端一致性、挂载选项、协议版本和服务端实现等多层因素。应用看到的 `fsync()` 成功，未必和本地磁盘上的直觉完全等价。

需要注意的点有这些：

1. **客户端缓存会影响可见性**

   NFS 为了性能会缓存文件数据、目录项和属性。Linux NFS 文档也强调，NFS 采用的是较弱的 cache coherence，例如 close-to-open consistency，而不是严格的集群文件系统一致性。多客户端同时读写同一个文件时，不能只靠普通读写调用来获得强一致。

2. **close-to-open 不是事务协议**

   close-to-open 语义适合“一个客户端写完关闭，另一个客户端再打开读取”的顺序共享模式。但 WAL、数据库、锁文件、leader election 文件这类场景，通常需要更强的顺序和可见性保证。

3. **`O_APPEND` 在 NFS 上有历史风险**

   Linux `open(2)` 明确提示过，NFS 上 append 可能由客户端模拟，多个进程同时 append 可能发生竞态。因此把 NFS 文件当成本地 append-only log 使用，需要非常谨慎。

4. **错误可能延迟或在 close/fsync 时暴露**

   NFS 写入可能先进入客户端缓存或服务端缓存，实际错误可能到后续 `fsync()`、`close()` 或重试失败时才返回。应用必须检查这些返回值，不能只检查 `write()`。

5. **soft mount 可能损坏数据语义**

   NFS 的 `soft` 或 `softerr` 挂载在请求超时后可能向应用返回错误。文档也提醒，soft timeout 在某些情况下可能造成 silent data corruption。对数据库和 WAL，一般更偏向谨慎使用 hard mount，并配合超时监控和故障处理。

6. **服务端导出和底层存储也会影响 fsync**

   如果服务端使用异步导出、后端设备 cache、虚拟化存储或复制系统，`fsync()` 的实际持久性取决于服务端如何处理稳定写入和 flush。

7. **锁和租约不是所有故障下都稳固**

   NFS 锁、delegation、session、failover 都可能在网络分区或服务端重启时暴露边界条件。应用要能处理 `ESTALE`、`EIO`、锁丢失、重试后重复写等问题。

因此，严肃的数据库、队列、WAL 系统如果要跑在 NFS 上，不能只说“我们调用了 fsync”。更合理的做法是明确支持矩阵：NFS 版本、挂载选项、服务端配置、锁策略、故障恢复测试、数据损坏检测都要写清楚。无法确认时，应把 NFS 视为风险较高的部署环境。

## Q038. 容器环境下持久化 volume 的 fsync 行为可能受哪些因素影响？

**回答：**

容器里的 `fsync()` 最终还是系统调用，通常会进入宿主机内核。但它能保证什么，取决于容器文件路径背后到底是什么存储层。容器本身不是持久化边界，volume 才是关键。

常见影响因素包括：

1. **写在容器可写层还是 volume 里**

   如果数据写在容器镜像的 writable layer，例如 overlayfs 上层目录，容器删除、重建或迁移时可能丢失。数据库数据通常不应放在容器可写层，而应放在明确声明的 volume 或 persistent volume 中。

2. **storage driver**

   Docker、containerd、CRI-O 可能使用 overlay2、btrfs、zfs、devicemapper 等不同 storage driver。不同 driver 对 copy-up、rename、fsync directory、白名单文件、元数据持久化的处理细节可能不同。

3. **volume 类型**

   bind mount、本地 named volume、Kubernetes local PV、云盘 CSI volume、NFS volume、Ceph/RBD、EBS、Azure Disk、GCE Persistent Disk 等，底层语义差异很大。应用调用同一个 `fsync()`，背后可能是本地 ext4，也可能是网络存储。

4. **宿主机文件系统和挂载参数**

   容器内看到的是路径，真正决定 crash consistency 的往往是宿主机上的 ext4、XFS、btrfs、ZFS，以及它们的挂载参数、barrier、journal 模式和磁盘 cache 设置。

5. **编排系统生命周期**

   容器重启、Pod 重调度、节点重启、节点断电不是同一类故障。容器重启不会清空 PV，但节点断电会考验底层存储的 fsync 语义。跨节点迁移还会涉及 attach/detach 和远端存储一致性。

6. **I/O 限流和 cgroup**

   cgroup I/O 限速、blkio 权重、Kubernetes QoS、云盘 IOPS 限额都会影响 fsync 延迟。它们通常不改变语义，但会让超时、leader lease、心跳和写入批处理暴露问题。

7. **tmpfs 和 emptyDir**

   Kubernetes `emptyDir` 如果使用内存介质，或者容器里写入 `/dev/shm`、tmpfs，`fsync()` 不等于持久到物理存储。它可能只保证内存文件系统状态一致，一旦节点重启数据就没了。

8. **sidecar、备份和快照**

   在线备份、volume snapshot、sidecar 拷贝文件时，如果没有和数据库 checkpoint/fsync 协调，可能拿到逻辑不一致的文件集合。

所以容器环境下讨论 fsync，不能只问“代码有没有调用 fsync”，还要问“这个路径挂载到哪里、底层是什么文件系统、volume driver 怎么实现、节点故障时 provider 承诺什么”。对数据库类应用，最好在目标容器运行时和目标 volume 类型上做 crash test，而不是只在裸机目录里测试。

## Q039. Linux dirty_ratio、dirty_background_ratio 对写入延迟有什么影响？

**回答：**

`dirty_background_ratio` 和 `dirty_ratio` 是 Linux 控制脏页写回的重要参数。它们影响的不是单个 `write()` 是否成功，而是 buffered write 什么时候开始被后台写回，以及写入进程什么时候会被迫参与写回。

可以这样理解：

- `dirty_background_ratio`：当脏页达到一定比例后，后台 flusher 线程开始把 dirty data 写回磁盘。
- `dirty_ratio`：当脏页达到更高比例后，正在产生脏页的进程会被 throttle，甚至自己参与写回，写入延迟会明显上升。

Linux 还提供对应的字节数参数：

- `dirty_background_bytes` 对应 `dirty_background_ratio`。
- `dirty_bytes` 对应 `dirty_ratio`。

同一组里一般只能使用 ratio 或 bytes 的一种。对于内存很大的机器，使用 bytes 有时比 ratio 更可控，因为 10% 内存在大机器上可能是非常大的 dirty data。

对写入延迟的影响主要体现在：

1. **参数较高时，吞吐可能更好，但延迟尖峰更大**

   系统允许积累更多 dirty pages，前面的 `write()` 会很快返回，看起来吞吐很好。但一旦到达阈值，后台写回压力很大，后续写入可能突然卡住。此时 `fsync()` 也可能要等待大量历史脏页，尾延迟变高。

2. **参数较低时，写回更平滑，但峰值吞吐可能下降**

   dirty pages 较早被写回，内存里积压少，fsync 尾延迟可能更可控。但后台 I/O 更频繁，写入合并空间减少，吞吐可能不如高阈值设置。

3. **会影响崩溃丢失窗口**

   dirty pages 越多，普通 buffered write 尚未持久化的数据越多。即使应用没有显式 durability 承诺，崩溃后可能丢失的最近写入范围也会变大。

4. **会影响多进程公平性**

   一个进程大量写入导致 dirty pages 达到阈值时，其他进程的写入或 fsync 也可能受影响。面试中可以把它理解成“page cache 是共享资源，dirty limit 也是全局或按 bdi/cgroup 约束的资源”。

5. **会和存储设备能力强相关**

   快 NVMe 能较快消化 dirty pages，慢 HDD 或网络盘可能让 dirty pages 长时间积压。相同 dirty_ratio 在不同设备上延迟表现完全不同。

调参时不要只看吞吐。对日志系统、数据库、消息队列，更重要的是看 fsync latency、p99/p999 写延迟、dirty pages 长期水平、writeback 是否堆积、I/O queue 是否持续拉长。

## Q040. 如何观测 page cache 命中、脏页回写和 I/O wait？

**回答：**

观测这类问题要分层看：page cache 是内存层，dirty writeback 是内核写回层，I/O wait 是 CPU 等待块设备 I/O 的表现。单看一个指标很容易误判。

**观测 page cache**

常用入口是 `/proc/meminfo`：

- `Cached`：大致表示文件页缓存。
- `Buffers`：块设备相关缓冲。
- `Dirty`：等待写回磁盘的内存。
- `Writeback`：正在写回磁盘的内存。
- `Mapped`：被 mmap 映射的文件页。
- `SReclaimable`：可回收的 slab，例如 inode、dentry cache。

命令上可以用：

```bash
cat /proc/meminfo
free -h
vmstat 1
```

如果要看 page cache 命中率，传统 Linux 基础命令并不会直接给一个“全局命中率”。更常见的做法是结合：

- `cachestat`、`cachetop` 这类 bcc/eBPF 工具。
- `perf stat` 或 tracepoint 观察 page fault、readahead、文件系统读路径。
- 应用层读延迟、磁盘读 IOPS 变化。

需要注意，page cache 命中率不是越高越好。数据库如果使用自己的 buffer pool，OS page cache 命中率可能不是主要指标；Direct I/O 场景下 page cache 本来就会被绕过。

**观测脏页和回写**

可以看 `/proc/meminfo` 的：

- `Dirty`
- `Writeback`
- `WritebackTmp`

也可以看 `/proc/vmstat`：

- `nr_dirty`
- `nr_writeback`
- `nr_writeback_temp`
- `nr_dirtied`
- `nr_written`
- `nr_dirty_threshold`
- `nr_dirty_background_threshold`

配合命令：

```bash
vmstat 1
cat /proc/vmstat
cat /proc/sys/vm/dirty_ratio
cat /proc/sys/vm/dirty_background_ratio
cat /proc/sys/vm/dirty_bytes
cat /proc/sys/vm/dirty_background_bytes
```

如果 `Dirty` 长期很高，说明应用写入产生脏页的速度超过了设备写回速度，或者 dirty limit 设置过高。如果 `Writeback` 长期很高，说明内核正在持续写回，底层 I/O 可能已经成为瓶颈。

**观测 I/O wait 和块设备延迟**

常用命令：

```bash
iostat -x 1
vmstat 1
pidstat -d 1
iotop
top
mpstat 1
```

`iostat -x` 里常看的指标包括：

- `r/s`、`w/s`：读写 IOPS。
- `rkB/s`、`wkB/s`：吞吐。
- `await`、`r_await`、`w_await`：请求平均等待时间。
- `aqu-sz`：平均队列长度。
- `%util`：设备繁忙程度。

CPU 里的 `%iowait` 也常被看，但要小心解释。iowait 表示 CPU 空闲且有未完成 I/O 时的时间比例，它不是“应用因为 I/O 卡住的精确比例”。如果 CPU 很忙，iowait 可能不高，但应用仍然被 I/O 延迟影响；如果系统很闲，一个慢 I/O 也可能让 iowait 看起来很突出。

**观测 fsync 延迟**

对 crash consistency 来说，最重要的往往不是平均写吞吐，而是 fsync 延迟分布。可以从几层看：

- 应用层记录每次 fsync 的耗时直方图。
- `strace -T -e fsync,fdatasync,msync` 看系统调用耗时。
- eBPF/bpftrace 观察 `fsync`、writeback、block I/O tracepoint。
- `blktrace`、`biosnoop`、`biolatency` 看块设备 I/O 延迟。

最终排查路径一般是：先看应用 fsync 是否变慢，再看 dirty pages 是否堆积，再看 block device await 和队列长度，再看是否有 cgroup 限流、云盘额度耗尽、网络存储抖动或文件系统 journal commit 抖动。

## Q041. 为什么 tail latency 常常被 fsync 放大？

**回答：**

因为 `fsync()` 不是普通的内存写入路径，它把很多平时被隐藏起来的成本一次性暴露出来。普通 `write()` 可以很快返回，是因为数据先进入 page cache；`fsync()` 则要等待数据、必要元数据、journal commit、设备 flush，甚至远端存储确认。前面的写入越“轻松”，后面的 fsync 就越可能集中还债。

tail latency 被 fsync 放大的原因通常有这些：

1. **fsync 会等待历史脏页**

   调用 `fsync(fd)` 时，内核要把该文件相关的 dirty pages 写出去。如果应用之前连续写了很多数据但没有同步，单次 fsync 就会承担大量积压 I/O。平均写延迟看起来很低，p99 fsync 可能很高。

2. **fsync 可能等待文件系统 journal commit**

   在 journaling 文件系统里，fsync 不只是写数据，还可能等待元数据事务提交。多个文件的元数据操作可能被放进同一个 journal transaction。某个请求的 fsync 可能正好撞上 journal commit 周期，于是延迟被放大。

3. **设备 flush 往往比普通写更慢**

   普通写可以被设备缓存、合并、重排；flush 或 FUA 要求设备给出更强的持久化确认。HDD 上可能要等盘片旋转和寻道，SSD 上可能触发 FTL 元数据刷写，云盘或网络盘上还可能等复制确认。

4. **队列里排在前面的 I/O 会拖慢它**

   fsync 的关键 I/O 到达块层时，设备队列里可能已经有大量读写请求。即使 fsync 自己的数据不多，也要等前面的请求完成或被调度。写入流量、读放大、compaction、备份扫描都会影响它。

5. **writeback 会制造抖动**

   后台 flusher 线程如果正在大量回写脏页，fsync 可能和普通 writeback 抢设备带宽。dirty ratio 配得太高时，这种抖动会更明显：平时写得很快，突然一段时间所有人一起等 I/O。

6. **group commit 既能降成本，也会带来排队**

   group commit 把多个提交合并到一次 fsync，吞吐会变好。但如果批次等待时间、队列长度或 leader 调度不合理，尾部请求可能要等上一批 fsync 完，再等自己这一批 fsync。

7. **存储设备本身有长尾**

   SSD 可能有 garbage collection、wear leveling、SLC cache 回收；云盘可能有多租户干扰、突发额度耗尽、网络抖动；RAID 或分布式存储可能有某个副本慢。fsync 刚好碰到这些事件，就会把长尾直接传给应用。

8. **锁和全局资源会形成 convoy**

   文件系统锁、inode 锁、journal 锁、block queue、设备队列、数据库提交锁，都可能让一批请求排队。一个慢 fsync 不只影响自己，还可能把后面的请求一起拖住。

这也是为什么数据库和日志系统通常不只看平均延迟。平均 `write()` 延迟很漂亮，不代表持久化路径健康。真正要看的指标是 fsync p99/p999、每次 fsync 同步了多少字节、dirty pages 是否积压、journal commit 延迟、设备 await、队列深度和云盘限流状态。

## Q042. 如果 fsync 延迟突然升高，你会从哪些层排查？

**回答：**

排查 fsync 延迟要按写入链路往下走，不要一开始就猜磁盘坏了。`fsync()` 变慢可能是应用批次变大，也可能是 dirty pages 堆积、文件系统 journal 抖动、块设备队列拥塞、云盘限额耗尽，甚至是另一个进程在同一块盘上做大写入。

我一般会按这些层看：

1. **应用层**

   先确认 fsync 调用模式有没有变：

   - 是否从 batch fsync 变成了每条 record fsync。
   - 单次 fsync 前写入了多少字节。
   - 是否有更多线程同时提交。
   - group commit 批大小和等待时间是否异常。
   - 是否有 compaction、checkpoint、snapshot、日志切分、索引重建同时发生。
   - 是否把目录 fsync、manifest fsync、数据文件 fsync 串在了同一个请求路径上。

   应用应该记录 fsync 耗时直方图，而不是只记录总写入耗时。最好同时记录 `bytes_since_last_fsync`、`records_since_last_fsync`、queue depth、batch size。

2. **运行时和进程层**

   看进程是否被 CPU、锁或调度拖住：

   - Go/Java/Rust 运行时是否有 GC、stop-the-world 或线程池饥饿。
   - 提交锁、WAL 锁、文件句柄锁是否竞争变严重。
   - fsync goroutine/thread 是否被调度延迟。
   - 是否有 cgroup CPU throttle。

   有时日志里看到“fsync 慢”，其实是调用 fsync 前排队慢，或者 fsync 返回后持有锁太久。

3. **page cache 和 dirty writeback 层**

   看 `/proc/meminfo` 和 `/proc/vmstat`：

   - `Dirty` 是否持续升高。
   - `Writeback` 是否长期不降。
   - `nr_dirty`、`nr_writeback`、`nr_dirtied`、`nr_written` 是否异常。
   - dirty limit 是否触发写入进程 throttle。
   - `dirty_ratio`、`dirty_background_ratio` 或 bytes 参数是否过高。

   如果 dirty pages 积压很大，fsync 变慢通常不是单个请求的问题，而是系统写回能力落后于写入速度。

4. **文件系统层**

   看文件系统类型和挂载参数：

   - ext4、XFS、btrfs、ZFS 的 fsync 行为不同。
   - ext4 的 journal commit、barrier、data mode 会影响延迟。
   - 是否频繁创建、rename、unlink 文件，导致目录和元数据同步变多。
   - 是否有大量小文件 fsync。
   - 是否有 fallocate、truncate、hole punching、文件扩展导致元数据更新。

   如果最近改过文件格式，例如从固定 segment 改成频繁滚动小文件，fsync 延迟可能来自元数据路径。

5. **block layer 和设备队列**

   用 `iostat -x 1`、`pidstat -d`、`iotop`、eBPF 工具看：

   - `await`、`w_await` 是否上升。
   - `aqu-sz` 队列是否拉长。
   - `%util` 是否接近饱和。
   - 写吞吐或 IOPS 是否接近设备上限。
   - 是否有读流量把写 flush 挤到后面。

   这里要把“设备忙”和“fsync 语义慢”分开。设备完全空闲但 fsync 慢，可能是 flush、journal 或远端确认；设备队列很长，则可能是普通 I/O 拥塞。

6. **存储介质层**

   本地 SSD/HDD/NVMe 要看：

   - SMART/NVMe health。
   - SSD 是否进入 GC 或写入放大严重。
   - HDD 是否有坏道重试。
   - RAID 控制器 cache 策略是否变化。
   - 断电保护缓存是否失效。

   云盘要看：

   - IOPS、吞吐、突发额度、队列深度。
   - provider 的 volume metrics。
   - 是否发生迁移、快照、后台复制、AZ 内网络抖动。

7. **网络文件系统和容器层**

   如果数据在 NFS、CephFS、SMB、CSI volume 上，要看：

   - 网络 RTT、丢包、重传。
   - 服务端负载。
   - mount options。
   - volume driver。
   - Kubernetes 节点和 PV 事件。

   容器里还要看 cgroup I/O 限流、宿主机是否有其他容器抢同一块盘。

排查时最好先画一条链路：应用提交队列 -> WAL 写入 -> fsync syscall -> page cache/writeback -> 文件系统 journal -> block layer -> 设备或远端存储。每一层都有自己的队列和指标。尾延迟常常不是某一层单独造成的，而是多个队列叠在一起。

## Q043. 如何在持久性和吞吐之间设计可配置策略？

**回答：**

持久性和吞吐的矛盾，本质是“每次提交都等稳定存储确认”还是“允许一小段时间内的数据停留在易失层”。配置策略要把这个选择明确暴露出来，不能让用户误以为高吞吐模式仍然有强持久性。

常见设计可以分成几档：

1. **always fsync**

   每次提交都写 WAL 并 fsync，fsync 成功后才 ack。

   语义最清楚：只要底层存储守约，ack 过的数据在进程崩溃和机器断电后都应该可恢复。代价也明显：吞吐受 fsync 次数、设备 flush 延迟和 journal commit 限制。

   适合金融交易、元数据服务、强一致队列、配置中心、分布式共识日志等场景。

2. **group commit**

   多个提交共享一次 fsync。请求可以在短暂等待窗口内排队，leader 线程统一刷盘，然后一起 ack。

   它通常是更实用的默认值，因为能大幅降低 fsync 次数，同时仍然可以保证“ack 发生在 fsync 成功之后”。风险主要是排队策略写不好会增加尾延迟。

   关键配置包括：

   - 最大等待时间，例如 1ms、5ms、10ms。
   - 最大批大小。
   - 最大待刷字节数。
   - fsync 并发度，通常单 WAL 文件不宜盲目并发 fsync。

3. **interval fsync**

   后台每隔一段时间 fsync 一次，例如每 100ms 或 1s。写请求可以在数据进入 page cache 后就返回。

   吞吐会更高，但机器崩溃时可能丢失最近一个 interval 内已经 ack 的数据。这里必须把语义写清楚：ack 表示“内核已接受写入”，不表示“已持久化”。

   适合日志采集、指标、缓存型数据、可重放消息、允许少量丢失的场景。

4. **size-based fsync**

   累积到一定字节数后 fsync，例如每 4MB 或 64MB 同步一次。它适合吞吐型写入，但崩溃丢失窗口不按时间固定，而是按最近一批未同步字节计算。

5. **manual 或 explicit flush**

   系统默认异步写，调用方在业务边界显式 flush。例如“每个 checkpoint 后 flush”“每个批处理任务结束 flush”。

   这种模式灵活，但容易误用。API 名称要清楚区分 `write()`、`flush_to_os()`、`sync_to_disk()`，不能把用户态 buffer flush 伪装成磁盘持久化。

一个好的配置设计要满足几个要求：

1. **把 ack 语义写清楚**

   每个模式都要说明：客户端收到成功时，数据到底在哪里？用户态 buffer、page cache、文件系统、设备 cache，还是稳定存储？

2. **给出可估算的损失窗口**

   例如：

   - always fsync：理论上不丢 ack 后数据。
   - group commit：不丢 ack 后数据，但请求可能多等一个 batch。
   - interval fsync 100ms：机器崩溃可能丢最近约 100ms 的 ack 数据。
   - size-based 64MB：机器崩溃可能丢最近未同步的最多 64MB 左右数据。

3. **失败处理要明确**

   fsync 返回 EIO、ENOSPC、EDQUOT 时，系统不能继续假装写入成功。要进入只读、停止 ack、标记副本落后、触发重建，或者把错误传播给上层。

4. **默认值要保守**

   面向数据库、队列、元数据服务时，默认应该偏安全。高吞吐弱持久性模式可以提供，但应该需要用户显式开启，并在配置名里体现风险，例如 `durability=async`。

5. **指标必须跟配置配套**

   应暴露 fsync latency、pending bytes、last durable LSN、last acknowledged LSN、dirty bytes、dropped records、sync error count。没有这些指标，用户不知道自己实际承担了多大风险。

6. **配置应按数据类型区分**

   同一个系统里，元数据、WAL、普通日志、缓存文件的持久性要求不同。不要只提供一个全局开关把所有路径都改成异步。

面试回答可以落到一句话：吞吐策略可以配置，但提交语义不能含糊。允许用户选择风险，不允许系统隐藏风险。

## Q044. page cache 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

page cache 的核心目标是性能。它用内存缓存文件数据，把慢速存储访问变成更快的内存访问，并把小写入合并成更适合设备的批量写回。

它主要解决几个性能问题：

1. **加速重复读取**

   文件第一次读入后，数据页会留在内存里。后续读取同一段文件时，如果 page cache 命中，就不必再次访问磁盘。

2. **支持预读**

   顺序读文件时，内核可以预测接下来要读的页，提前把它们读入内存。这样应用下一次 read 时更可能命中。

3. **吸收写入突发**

   buffered write 可以先把数据复制到 page cache 并标记为 dirty，再由后台 writeback 稍后写出。应用不必每次都等磁盘。

4. **合并和重排 I/O**

   多次小写入可以在内存里合并成较大的写回请求，减少设备随机 I/O 压力。

5. **让 mmap 和普通文件 I/O 共享缓存**

   `read()`、`write()` 和 `mmap()` 访问同一个文件时，很多情况下底层共享同一批 page cache 页，避免重复缓存。

但 page cache 不是持久性机制。它不会保证 `write()` 返回后数据已经落盘，也不会保证断电后 dirty page 还在。它也不是安全机制，不能防篡改，不能做访问控制，不能代替加密或认证。

它对正确性有一些间接作用，例如让同一台机器上对同一文件的读写有一致的内核缓存视图，配合文件系统维护页状态。但从系统设计角度看，page cache 的第一目标仍然是性能。正确性要靠文件系统语义、锁、fsync、checksum、WAL 和恢复协议来保证。

所以可以把它归类为：主要解决性能问题，顺带影响正确性边界；不解决安全性问题，也不是为了可维护性而设计的抽象。

## Q045. page cache 的典型适用场景和不适用场景分别是什么？

**回答：**

page cache 适合“数据会被重复访问，或者写入可以先缓存再后台刷出”的场景。不适合“应用自己已经有完整缓存策略，或者每次 I/O 都要求精确控制持久化和缓存占用”的场景。

典型适用场景：

1. **普通文件服务**

   Web 静态文件、配置文件、模板文件、图片、小型资源文件，经常被重复读取。page cache 可以显著减少磁盘读。

2. **读多写少的数据文件**

   例如本地索引文件、词典文件、模型元数据、只读 segment。文件加载后会长期被读，page cache 很合适。

3. **顺序读写**

   顺序读可以利用 readahead，顺序写可以利用 writeback 合并。日志、批处理输入文件、导入导出任务都能受益。

4. **工具型和脚本型程序**

   很多程序没有必要自己实现复杂缓存。直接用 buffered I/O，让内核管理缓存，通常更简单也足够快。

5. **mmap 读取**

   mmap 访问文件时，page cache 是核心路径。对只读大文件、内存映射索引、词典、二进制资源，mmap + page cache 经常很自然。

不适用或需要谨慎的场景：

1. **数据库已有自己的 buffer pool**

   许多数据库会自己管理页缓存、淘汰策略、预读和刷脏。如果再让 OS page cache 缓存一份，可能出现双重缓存，浪费内存，还会让延迟更难预测。

2. **大规模一次性扫描**

   备份、全表扫描、日志归档可能把热数据挤出 page cache。此时需要考虑 `posix_fadvise()`、direct I/O、限速或单独隔离。

3. **强持久性写路径**

   WAL、raft log、事务提交路径如果依赖 page cache 提高吞吐，也必须配合 fsync。不能因为 write 很快就认为数据已经安全。

4. **需要稳定低尾延迟的系统**

   page cache 会引入 writeback 抖动、dirty throttle、内存回收、cache miss 长尾。低延迟系统有时会选择 direct I/O、预分配、mlock、专用 I/O 线程来减少不可控因素。

5. **内存非常紧张的环境**

   page cache 和应用 heap 共享物理内存。容器内存限制、混部场景、批处理大扫描都可能造成 cache thrash 或 OOM。

6. **远端或特殊文件系统**

   NFS、FUSE、对象存储挂载、某些 CSI volume 的缓存语义和本地文件系统不同。page cache 命中不代表远端状态新鲜，写回成功也不一定等于远端持久化达到业务预期。

判断是否适合 page cache，可以问三个问题：数据会不会复用？应用是否需要自己控制缓存？写入成功语义是否必须等稳定存储？如果数据复用明显、语义要求普通，page cache 很合适；如果应用要自己掌控缓存和持久化路径，就要谨慎。

## Q046. page cache 和相近概念最容易混淆的边界在哪里？

**回答：**

page cache 容易和很多“缓存”混在一起。面试里要把它们分清楚，因为每一层缓存的失效条件、持久化语义和观测方式都不同。

常见混淆点如下：

1. **page cache vs 用户态 buffer**

   用户态 buffer 是应用自己内存里的数据，例如 Go 的 `bufio.Writer`、C 的 stdio buffer、Java 的 BufferedOutputStream。调用 `flush()` 通常只是把用户态 buffer 推给内核，不等于落盘。

   page cache 是内核里的文件页缓存。数据从用户态写入内核后，可能在 page cache 中变成 dirty page。要让它进入持久化路径，还需要 `fsync()`、`fdatasync()` 或类似机制。

2. **page cache vs 磁盘 write cache**

   page cache 在操作系统内存里；磁盘 write cache 在 SSD/HDD/NVMe 或 RAID 控制器里。内核把 dirty page 写给设备后，设备仍可能先放在 volatile cache。flush/FUA 是控制设备缓存的重要手段。

3. **page cache vs 数据库 buffer pool**

   数据库 buffer pool 是数据库自己管理的缓存，通常有自己的页格式、脏页列表、checkpoint、淘汰策略和 WAL 协议。page cache 是内核按文件页管理的缓存。两者叠加可能造成双重缓存。

4. **page cache vs buffer cache**

   现代 Linux 里，文件内容主要通过 page cache 管理。历史上的 buffer cache 更多指块设备 buffer。很多资料会混用这两个词，但讨论普通文件 I/O 时，重点通常是 page cache。

5. **page cache vs mmap**

   mmap 是一种访问文件的接口，不是另一套独立缓存。MAP_SHARED 映射的文件页通常背后就是 page cache。mmap 写入后也可能只是脏页，仍需要 `msync()` 或 `fsync()` 处理持久化边界。

6. **page cache vs readahead**

   page cache 是缓存本身；readahead 是内核预测顺序访问并提前填充 page cache 的策略。readahead 命中能让读更快，但随机读或错误预测会浪费 I/O 和内存。

7. **page cache vs tmpfs**

   tmpfs 本身是内存文件系统，数据主要在内存里，可以被 swap。page cache 是普通文件系统访问磁盘文件时的缓存。对 tmpfs 调用 fsync，不能得到“写入物理磁盘”的语义。

8. **page cache vs CDN/应用缓存**

   CDN、Redis、本地 LRU cache 都是在更高层缓存对象或业务数据。page cache 缓存的是文件页。业务缓存命中不代表文件页命中，文件页命中也不代表业务对象仍然有效。

最容易出错的地方是把“flush”混为一谈。用户态 flush、内核 writeback、文件 fsync、目录 fsync、设备 cache flush 是不同层级的动作。回答这类问题时，把数据从应用内存到稳定存储的路径讲清楚，边界就不会乱。

## Q047. page cache 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，page cache 的问题往往不是“有没有缓存”这么简单，而是缓存变成共享资源后，谁把内存、I/O 队列和写回能力用掉了。

常见隐藏问题有这些：

1. **cache thrashing**

   多个任务同时扫描大文件，可能把真正的热数据挤出 page cache。表面上每个任务只是顺序读，合起来却让系统整体 cache 命中率下降，线上读延迟突然变差。

2. **dirty pages 堆积**

   多个写线程同时写文件，dirty pages 增长速度超过设备写回速度。前期写入很快，后面触发 dirty throttle，所有写线程一起变慢。fsync 尾延迟也会被拉高。

3. **写回干扰读请求**

   大量后台 writeback 会占用设备队列和带宽。读请求本来很小，却排在大量写请求后面，导致读延迟升高。对延迟敏感服务，这种读写互相干扰很明显。

4. **inode、address_space 和文件系统锁竞争**

   多线程写同一个文件、频繁扩展文件、频繁更新元数据，可能在内核锁上竞争。应用看到的是 write 或 fsync 变慢，根因可能是同一个 inode 的并发修改太重。

5. **全局或 cgroup dirty limit 互相影响**

   一个租户或一个容器大量写入，会消耗全局 dirty budget 或 cgroup I/O 能力。另一个服务即使写得不多，也可能被 throttle 或被设备队列拖慢。

6. **NUMA 和内存回收开销**

   大机器上 page cache 分布、内存回收、跨 NUMA 节点访问都可能影响性能。内存压力下，回收线程会扫描页、回收 inode/dentry/slab，应用延迟会变得不稳定。

7. **mmap 并发修改带来的可见性和 SIGBUS 问题**

   一个进程 mmap 文件，另一个进程 truncate 或替换文件，可能让映射访问出现 SIGBUS 或读到非预期内容。共享 mmap 写入还要考虑内存可见性、msync 和文件锁。

8. **备份、压缩、日志归档把缓存污染掉**

   后台任务通常吞吐大、持续时间长，如果不做限速或 fadvise，可能把业务热文件挤掉。问题不一定出在业务进程里，而是旁路任务改变了 page cache 状态。

9. **小文件风暴**

   高频创建、删除、fsync 小文件会让 page cache、dentry cache、inode cache 和 journal 都承压。此时瓶颈不一定是数据页读写，而是元数据和目录项。

高并发下设计 page cache 策略，要把它当作共享容量和共享写回通道来管理。常见做法包括限制后台扫描速度、使用 `posix_fadvise()` 降低缓存污染、把 WAL 和大扫描放在不同设备、配置 cgroup I/O、记录 dirty pages 和 fsync 延迟分布。

## Q048. page cache 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

page cache 在正常运行时很像“文件已经写好了”，但一遇到崩溃和重启，边界就会变清楚：内存里的 dirty page 不是稳定存储。很多 crash consistency bug 都来自把 page cache 的成功路径当成了持久化成功路径。

常见边界条件如下：

1. **进程崩溃**

   进程崩溃不一定丢 page cache。因为 page cache 属于内核，不属于某个进程。进程写入后还没 fsync，如果只是进程自己被 kill，数据可能仍在 page cache，随后被后台写回。

   这会造成一个测试误区：kill 进程后恢复成功，不代表断电后也成功。机器断电会丢掉内存里的 dirty pages。

2. **机器断电或内核崩溃**

   dirty pages 会丢。已经进入设备 volatile cache 但没 flush 的数据也可能丢。恢复协议必须假设最后一段写入可能不存在、部分存在或顺序不同。

3. **重启后 cache 变冷**

   重启会清空 page cache。系统启动后第一次读可能变慢，checkpoint 加载、索引重建、segment 扫描都会产生冷启动 I/O 峰值。平时压测如果一直跑 warm cache，很容易低估重启恢复时间。

4. **fsync 错误延迟暴露**

   `write()` 成功后，错误可能到后续 fsync 或 close 才返回。超时和重试逻辑如果只看 write 返回，就可能把失败写入当成功提交。

5. **超时不等于取消写入**

   应用层等待 `write()` 或 `fsync()` 超时，不代表内核 I/O 被取消，也不代表数据不会稍后写入。上层如果直接重试同一条 record，可能出现重复记录。需要 sequence number、idempotency key 或 transaction id。

6. **重试可能和旧 dirty page 交错**

   第一次写入超时后，第二次重试写同一 offset 或同一文件尾部。如果没有单写线程、offset 管理或 record 边界，恢复时可能看到交错 record、重复 record 或 index 指向旧位置。

7. **rename 和目录项持久化边界**

   写临时文件、fsync 文件、rename 成正式文件，这只是路径可见性的原子替换。崩溃后 rename 是否持久，还要看目录是否 fsync。page cache 不会替你保存目录项提交点。

8. **mmap 写入的同步边界更容易被误解**

   mmap 写了内存，不等于文件已经持久化。需要考虑 `msync()`、`fsync()`、映射范围、文件截断、SIGBUS 和进程退出时的写回时机。

9. **网络文件系统重试语义更复杂**

   NFS、FUSE、云盘挂载可能把超时、重试、服务端确认、客户端缓存混在一起。应用以为本地 page cache 已经接收，远端可能尚未稳定持久化。

所以恢复协议不要依赖“刚才 write 成功过”。它应该依赖可验证的 durable boundary，例如 fsync 成功后的 LSN、record checksum、commit marker、manifest 版本和目录 fsync。page cache 可以让正常路径快，但崩溃语义必须按持久化边界设计。

## Q049. page cache 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

page cache 的瓶颈没有固定答案，要看工作负载。它可能来自内存容量，也可能来自 I/O，甚至来自锁竞争。可以按读路径和写路径分开看。

**读路径**

如果 page cache 命中，瓶颈通常不是磁盘，而是：

- CPU 拷贝数据到用户态的成本。
- page fault 处理成本，尤其是 mmap 随机访问。
- 内存带宽。
- NUMA 远程访问。
- 文件系统和 VFS 路径上的锁。
- 应用自己的解析、解压、校验成本。

如果 page cache 不命中，瓶颈通常会转到：

- 磁盘或云盘读延迟。
- readahead 是否有效。
- 随机读 IOPS。
- 网络文件系统 RTT。
- 块设备队列。

**写路径**

buffered write 前半段通常是内存路径，瓶颈可能是：

- 从用户态复制到 page cache 的 CPU 和内存带宽。
- 分配 page cache 页的成本。
- 文件扩展和元数据更新。
- 同一文件并发写导致的锁竞争。

真正持久化时，瓶颈通常转到：

- 后台 writeback 能力。
- 设备写吞吐和 flush 延迟。
- journal commit。
- dirty throttle。
- fsync 等待。

**内存瓶颈**

内存不足时，page cache 会被回收。回收本身要消耗 CPU，还可能造成热页被淘汰，随后读请求变成磁盘 I/O。容器环境里，如果内存限制较紧，page cache 和应用 heap 会互相挤压。

**锁竞争瓶颈**

同一个 inode 上大量并发写、频繁 append、truncate、fallocate、fsync，可能让锁竞争显著。这个瓶颈在指标上不一定表现为高磁盘 util，而可能表现为系统调用耗时变长、CPU system time 增加、线程在内核态等待。

**网络瓶颈**

本地文件系统的 page cache 主要不受网络影响。但如果底层是 NFS、SMB、CephFS、FUSE remote storage 或云盘，cache miss、writeback、fsync 都可能受网络 RTT、丢包、服务端拥塞影响。

简化判断可以这样做：

- cache 命中但慢：看 CPU、内存带宽、锁、page fault。
- cache 不命中慢：看磁盘、readahead、I/O 队列。
- write 快但 fsync 慢：看 dirty pages、writeback、journal、flush、云盘。
- 并发越高越慢：看锁竞争、dirty throttle、cgroup、共享设备。

page cache 是内存机制，但它把 CPU、内存和 I/O 连在了一起。只盯一个指标，通常查不到根因。

## Q050. page cache 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

这三类测试目标不同。correctness test 看语义是否正确，stress test 看极端并发和资源压力下会不会出错，benchmark 看性能曲线。把它们混在一起，结果很容易误读。

**correctness test 应该测什么**

正确性测试要验证“读到的内容、持久化边界、错误处理”是否符合协议：

1. **读写一致性**

   写入文件后立即读取，确认同进程、不同 fd、不同进程能看到预期内容。覆盖覆盖写、append 写、跨 page 边界写、小写入和大写入。

2. **mmap 与 read/write 一致性**

   一个路径用 `write()` 写，另一个路径用 mmap 读；或者 mmap 写后用 read 读。确认可见性符合预期，并明确什么时候需要 `msync()` 或 `fsync()`。

3. **fsync 边界**

   写 record、fsync、崩溃恢复，确认 fsync 前后的记录边界正确。没有 fsync 的写入，测试不应假设机器断电后还能恢复。

4. **错误传播**

   模拟 ENOSPC、EDQUOT、EIO、EINTR、EAGAIN、short write，确认系统不会把失败写入当成功提交。

5. **truncate、rename、目录 fsync**

   测试临时文件 rename、manifest 更新、目录项持久化。崩溃恢复后要么看到旧版本，要么看到新版本，不能看到半发布状态。

6. **恢复校验**

   用 checksum、magic、length、sequence 验证最后一条 record。遇到坏尾巴时截断到 last good offset。

**stress test 应该测什么**

压力测试要制造正常单元测试里看不到的竞争和资源紧张：

1. **高并发读写**

   多线程、多进程、多个 fd 同时读写同一文件和不同文件。观察是否有交错写、offset 错乱、锁竞争、吞吐塌陷。

2. **dirty page 压力**

   持续写入超过设备写回能力，观察 dirty throttle、fsync p99、writeback 是否堆积。

3. **内存压力**

   限制内存或在容器里运行，观察 page cache 被回收后是否出现长尾、OOM 或恢复时间过长。

4. **后台干扰**

   同时运行备份、全量扫描、compaction、checkpoint、日志归档，看业务热数据是否被挤出 page cache。

5. **故障注入**

   kill 进程、模拟断电、让 fsync 返回错误、让 write 部分成功、让磁盘变慢或满盘。重点看恢复协议是否稳。

6. **网络和云盘抖动**

   如果部署在云盘或网络文件系统上，要加入延迟、丢包、限流、服务端重启、volume detach/attach 这类场景。

**benchmark 应该测什么**

benchmark 要把变量拆开，否则数字没有解释力：

1. **冷缓存 vs 热缓存**

   冷缓存测试磁盘读和预读，热缓存测试内存路径和系统调用开销。两者不能混在一起比较。

2. **顺序 vs 随机**

   顺序读写能利用 readahead 和合并写；随机访问更容易暴露 IOPS、page fault 和 cache miss 成本。

3. **buffered I/O vs direct I/O**

   对数据库或日志系统，要比较 page cache 路径和 direct I/O 路径，观察吞吐、延迟、CPU、内存占用和 fsync 表现。

4. **不同 fsync 策略**

   比较 always fsync、group commit、interval fsync、size-based fsync。指标要包括吞吐、平均延迟、p99/p999、丢失窗口和恢复时间。

5. **工作集大小**

   工作集小于内存、接近内存、大于内存，结果会完全不同。page cache benchmark 必须明确工作集大小和机器内存。

6. **资源指标**

   不只看 QPS。还要记录 CPU user/system、内存、Dirty/Writeback、I/O await、队列长度、设备 util、上下文切换、major/minor page fault。

7. **长时间稳定性**

   短测容易只测到缓存和突发额度。长测才能看到 SSD GC、云盘突发额度耗尽、writeback 堆积、checkpoint 周期、compaction 抖动。

一个靠谱的 page cache 测试报告，至少要写清楚：是否 warm cache、是否 drop cache、文件系统和挂载参数、I/O 模式、fsync 策略、工作集大小、并发度、存储设备、容器限制和观测指标。否则 benchmark 数字很难复现，也很难指导线上调优。

## Q051. 如果要求从零实现一个简化版 page cache，你会先定义哪些不变量？

**回答：**

从零实现简化版 page cache，不要一开始就写 LRU、readahead 或复杂 writeback。先定义不变量。page cache 的 bug 往往不是“少了一个优化”，而是同一页有两个副本、脏页被错误丢弃、truncate 后还能读到旧数据、fsync 返回时数据其实还没写完。

我会先定义这些不变量：

1. **页身份唯一**

   每个缓存页必须由稳定 key 唯一标识，例如 `(file_id, page_index)`。同一个文件的同一个 page index，在 cache 中最多只能有一个 resident page。

   如果允许出现两个副本，后果很糟：一个写成 dirty，另一个还是 clean；读线程命中旧页，写回线程刷了新页；最后磁盘状态取决于谁后写。

2. **页大小和 offset 映射固定**

   设定固定 page size，例如 4KB。文件 offset 到 page 的映射必须确定：

   - `page_index = offset / page_size`
   - `page_offset = offset % page_size`

   跨页读写要拆成多段。不能让一个 write 同时绕过 page cache 修改磁盘，又留下旧 page 在 cache 里。

3. **页状态机清楚**

   一个简化状态机至少要有：

   - `Clean`：page 内容和后端存储一致。
   - `Dirty`：page 被修改，尚未持久化。
   - `Writeback`：page 正在写回。
   - `Invalid`：page 已失效，不能再被新读命中。

   状态迁移要受锁保护。比如 `Dirty -> Writeback -> Clean`，写回失败时不能标成 clean，而要保留 dirty 或记录错误。

4. **read-after-write 可见**

   同一台机器、同一份 page cache 内，如果 `write()` 已经更新某个页，后续 `read()` 应该读到新内容，除非调用方用了明确绕过缓存的路径并承担一致性责任。

   这条不变量不等于持久性。它只说明内核缓存视图里读写一致，不说明断电后还能恢复。

5. **dirty page 不能无声丢弃**

   脏页不能因为内存压力直接淘汰。淘汰前要么成功写回，要么把错误返回给调用方并保留错误状态。除非是明确的 discard 或 truncate 语义，否则丢 dirty page 就是数据损坏。

6. **正在被使用的页不能回收**

   page 需要 refcount 或 pin 机制。读写、mmap、writeback、I/O 提交期间，页不能被释放或复用。否则会出现 use-after-free、读到别的文件内容、写回错误地址。

7. **truncate 必须让缓存和文件大小一致**

   文件被截短后：

   - EOF 之后的缓存页必须 invalid。
   - EOF 所在页的尾部要按语义处理，通常读出来应该是 EOF 或零填充，而不是旧内容。
   - 旧 index、旧 mmap、旧 writeback 不能继续把被截掉的数据写回。

   truncate 是 page cache 最容易写错的路径之一，因为它改变了“哪些页还属于这个文件”。

8. **fsync 必须等待目标范围的 dirty/writeback 完成**

   如果实现 `fsync(file)`，它至少要保证：这个文件当前需要同步的 dirty pages 已经提交给后端并收到成功结果，相关 writeback 错误已经被返回或记录。

   `fsync()` 不能只把写回任务丢到队列就返回成功。否则上层会把还在内存里的数据当作 durable。

9. **错误要 sticky**

   后端写回失败，例如 EIO、ENOSPC，不能只在后台日志里打印一下。错误要挂到文件、页或 address space 上，后续 `fsync()`、`close()` 或相关写入路径必须能看到。

   这点很实际。很多线上数据丢失不是因为没有检测到错误，而是错误被后台线程吞掉了。

10. **并发插入和查找要线性化**

   多线程同时读同一页时，只能有一个线程负责创建并填充 page，其他线程等待或复用同一个 page。不能同时从磁盘读两份，再把后完成的那份覆盖先写入的 dirty page。

11. **writeback 不能覆盖更新后的数据**

   假设线程 A 开始把 dirty page 写回，线程 B 又修改了同一个 page。实现必须区分“正在写回的版本”和“后来又变脏的版本”。写回完成后不能直接把 page 标 clean，除非确认没有新的修改发生。

   常见做法是用 dirty generation、writeback bit 或页锁保护状态。

12. **缓存淘汰只影响性能，不改变文件语义**

   clean page 被淘汰后，再读应该能从后端存储拿到同样内容。淘汰策略可以是 LRU、clock、2Q，但它不能改变读写语义。

如果是面试中描述简化实现，我会先画出结构：

```text
PageCache
  map[(file_id, page_index)] -> Page

Page
  data[page_size]
  state: clean | dirty | writeback | invalid
  refcount
  lock
  dirty_generation
  last_error
```

然后再说读写流程。读路径先查 cache，miss 时分配 page 并从后端读入；写路径定位 page，修改 data，标 dirty；后台线程按策略 writeback；fsync 等待该文件 dirty/writeback 全部完成并返回错误。这样即使没有复杂优化，语义也站得住。

## Q052. page cache 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

page cache 的误用通常来自一句错误直觉：“write 返回了，文件就写好了。”这句话在正常运行时经常看起来成立，到了断电、重启、满盘、延迟抖动时就会出问题。

常见误用和线上症状如下：

1. **把 `write()` 成功当成持久化成功**

   误用：写完业务数据后不调用 fsync，就向客户端返回成功。

   症状：进程 kill 测试没问题，机器断电或内核崩溃后丢最近一段已确认数据。日志里看不到明显错误，因为数据从来没真正落到稳定存储。

2. **把用户态 flush 当成 fsync**

   误用：调用 `bufio.Flush()`、`fflush()`、`BufferedOutputStream.flush()` 后认为数据已经落盘。

   症状：短时间内读文件能读到数据，但重启后文件变短、最后几条 record 消失，或者 manifest 指向的数据文件不存在。

3. **忽略 fsync 或 close 返回的错误**

   误用：只检查 `write()` 返回值，不检查后续 `fsync()`、`fdatasync()`、`close()`。

   症状：磁盘满、配额满、网络存储错误时，业务层仍然记录“写入成功”。恢复时出现 checksum mismatch、index 指向空洞、WAL 缺尾。

4. **数据库和 page cache 双重缓存**

   误用：数据库已经有 buffer pool，又让 OS page cache 缓存同一批数据，且没有控制内存比例。

   症状：内存占用看起来很高，业务 heap 被挤压，容器 OOM，缓存命中率不稳定。压测时性能好，线上混部后一遇到扫描就抖。

5. **用 warm cache benchmark 代表真实磁盘性能**

   误用：反复读同一个小文件，不清楚数据已经全在 page cache 里，然后宣称磁盘吞吐很高。

   症状：上线后冷启动慢、重启恢复慢、数据集变大后延迟断崖式上升。测试结果复现不了，因为测试其实测的是内存。

6. **大扫描污染 page cache**

   误用：备份、导出、全量校验、日志归档直接顺序读大量冷数据，不做限速，也不使用 fadvise。

   症状：扫描任务本身没报错，但在线查询延迟升高。热索引或热点 segment 被挤出 cache，磁盘读 IOPS 突然上升。

7. **忽略 dirty page 积压**

   误用：大量 buffered write，不关注 dirty ratio、writeback、fsync 延迟。

   症状：写入前期很快，过一段时间突然卡住；p99/p999 写延迟升高；`Dirty` 长期很高；`iostat` 里 await 和队列长度上升。

8. **混用 mmap、write、truncate 但没有定义同步规则**

   误用：一个线程 mmap 读写，另一个线程用 write 覆盖或 truncate 文件。

   症状：读到旧数据、进程收到 SIGBUS、文件尾部出现非预期零、恢复时 record 边界乱掉。

9. **在网络文件系统上套用本地 page cache 直觉**

   误用：把 NFS、FUSE、远端 CSI volume 当作本地 ext4 使用，默认多客户端读写马上可见，fsync 语义完全等价。

   症状：多节点读到旧内容，锁文件不可靠，append log 出现交错或丢记录，故障切换后发现某个节点的视图落后。

10. **用 drop caches 当成线上调优手段**

   误用：看到内存被 page cache 占用就定期清 cache。

   症状：短时间内 free memory 变多，但随后读延迟上升、磁盘压力增加，系统整体更慢。page cache 可回收，不等于内存泄漏。

这些症状有一个共同点：正常路径看着没问题，边界条件下才暴露。定位时不要只看应用日志，要同时看 fsync 错误、Dirty/Writeback、I/O await、重启恢复日志、checksum mismatch 和业务 ack 时间点。

## Q053. page cache 在单机和分布式环境中的语义有什么差异？

**回答：**

单机环境里，page cache 属于同一个内核。多个进程读写同一个本地文件时，通常共享同一份内核缓存视图。分布式环境就不是这样了：每台机器有自己的 page cache，远端存储或分布式文件系统要额外定义缓存一致性协议。

**单机语义**

在本地文件系统上：

- 同一台机器上的进程通常通过同一个 page cache 访问文件内容。
- 一个进程 `write()` 修改文件后，另一个进程随后 `read()` 同一位置，通常会看到 page cache 中的新内容。
- mmap 和 read/write 很多情况下共享底层页缓存。
- page cache 命中只说明本机内存里有数据，不说明数据已经持久化。
- 机器断电会丢 dirty pages。

所以单机 page cache 更像“本机文件内容的内存视图”。它能提供本机范围内的缓存一致性，但持久性仍要靠 fsync，进程间并发语义仍要靠文件锁、原子写规则或应用协议。

**分布式语义**

分布式环境里情况会复杂很多：

- 每个客户端节点都有自己的 page cache。
- 客户端 A 的写入不一定立刻让客户端 B 的 page cache 失效。
- NFS 常见的是 close-to-open consistency，不是严格的全局线性一致。
- 分布式文件系统可能用 lease、delegation、cache invalidation、元数据服务来协调。
- 对象存储挂载通常更不像本地 POSIX 文件系统，rename、append、fsync 语义都可能不同。

因此，分布式环境不能把“本机读到新数据”直接推广成“所有节点都读到新数据”。如果业务需要跨节点强一致，必须在更高层设计：

- 版本号或 epoch。
- lease 或分布式锁。
- quorum 写入和读取。
- manifest 原子发布协议。
- checksum 和 generation 校验。
- 客户端 cache invalidation。
- 明确的 leader 写入模型。

**对持久性的差异**

单机上，`fsync()` 至少是应用向本机内核和本地文件系统请求持久化。分布式文件系统上，`fsync()` 还可能涉及客户端把数据发给服务端、服务端刷盘、远端副本确认、元数据服务提交。不同系统对这些步骤的承诺不同。

如果是数据库或共识日志，ack 语义通常不能建立在“写进本机 page cache”上，而要建立在“足够副本已经按协议持久化”上。否则节点故障或 failover 后，另一个节点可能根本没有那条数据。

**对性能的差异**

单机 page cache 命中可以避免本地磁盘 I/O；分布式场景下，cache miss 可能变成网络请求。重启、迁移、扩容后，新节点 cache 是冷的，读放大和恢复时间会更明显。云盘或网络文件系统还可能有服务端缓存，形成多层 cache，排查延迟时要分清哪一层命中。

一句话：单机 page cache 是本机内核缓存；分布式环境里的 page cache 是每个客户端自己的局部缓存。跨节点正确性不能靠它自动保证。

## Q054. fsync 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

`fsync()` 的核心目标是持久化正确性。它让应用有办法把“已经写到内核缓存里的文件数据和必要元数据”推进到稳定存储，并等待这个过程完成或返回错误。

它主要解决的是正确性问题，更准确地说是 crash consistency 和 durability 问题：

- 提交成功的数据，崩溃后应该还能恢复。
- 文件内容和必要元数据不能停留在易失缓存里。
- 写回错误不能永远藏在后台线程里。
- 上层系统可以用 fsync 成功作为 durable boundary。

它不是性能优化。调用 fsync 往往会降低吞吐、增加尾延迟，因为它强制等待写回、journal commit、设备 flush 或远端存储确认。系统会用 group commit、batch、interval sync 来降低 fsync 成本，但这些是在持久性语义和吞吐之间做取舍。

它也不是安全机制。fsync 不能防止恶意篡改，不能做身份认证，不能替代权限控制、加密、签名或 HMAC。攻击者如果能修改文件，fsync 只会帮他把修改持久化。

它对可维护性有间接帮助：有清楚的 fsync 边界，恢复协议更容易推理。但 fsync 本身不是为了代码组织而存在，它是为了让应用能控制“哪些写入在崩溃后必须存在”。

一个简洁判断是：`write()` 让数据进入内核；`fsync()` 尝试让数据越过崩溃边界。它解决的不是快，而是崩溃后说得清。

## Q055. fsync 的典型适用场景和不适用场景分别是什么？

**回答：**

fsync 适合那些“返回成功后，崩溃也不能随便丢”的写入路径。不适合每一条无关紧要的数据都强行同步，也不适合被用来弥补错误的协议设计。

典型适用场景：

1. **WAL 和事务提交**

   数据库、KV 存储、消息队列在提交事务前，需要把 WAL 或 commit record fsync。fsync 成功后才能向客户端确认 durable commit。

2. **共识日志**

   Raft、Paxos 类系统的日志项如果已经对外承诺持久化，通常要写入本地日志并同步。否则节点重启后可能忘掉已参与多数派决策的日志。

3. **元数据和 manifest 发布**

   LSM 的 manifest、segment 列表、checkpoint 指针、版本文件，都需要在发布时同步。否则崩溃后可能引用不存在的文件，或者丢失新版本入口。

4. **写临时文件再 rename 的发布流程**

   典型流程是写临时文件，fsync 临时文件，rename 到正式路径，再 fsync 目录。这样能减少崩溃后看到半文件或丢目录项的概率。

5. **配置、凭证、账务、任务状态**

   这些数据量可能不大，但业务语义重。写完就丢，会比慢一点更糟。

6. **checkpoint 和 snapshot**

   checkpoint 文件写完后，需要 fsync 文件和元数据，再更新指针。确认 checkpoint durable 后，才能安全删除旧 WAL。

不适用或要谨慎的场景：

1. **每条普通日志都 fsync**

   访问日志、调试日志、指标流水通常允许少量丢失。每条都 fsync 会显著拉低吞吐，甚至拖垮业务路径。

2. **临时文件和缓存文件**

   如果文件本来就可以重算、重下或重建，强 fsync 可能没有价值。更好的做法是把缓存损坏检测和重建逻辑做好。

3. **高频小写入没有 batch**

   每写几十字节就 fsync 一次，设备 flush 成本会压倒业务。应该考虑 group commit、批量写、segment append。

4. **错误的目录同步假设**

   只 fsync 文件，不 fsync 目录，不能保证新文件名或 rename 结果一定在崩溃后存在。这里不是“不该 fsync”，而是 fsync 对象选错了。

5. **不理解底层语义的网络文件系统**

   在 NFS、FUSE、某些云盘挂载上，fsync 的成本和语义依赖具体实现。数据库类工作负载要先验证支持矩阵。

6. **把 fsync 当作并发控制**

   fsync 不解决多线程写同一 offset、append record 交错、index 与 log 不一致的问题。并发正确性要靠锁、单写线程、LSN、record framing。

好的使用方式不是“到处加 fsync”，而是先定义哪些状态需要跨崩溃保存，再在这些状态的提交点使用 fsync。

## Q056. fsync 和相近概念最容易混淆的边界在哪里？

**回答：**

fsync 最容易和各种“flush”混淆。面试中只要把每一层数据在哪里讲清楚，边界就很明白。

1. **`write()` vs `fsync()`**

   `write()` 把数据交给内核，通常进入 page cache。它成功不代表数据已经落盘。`fsync()` 才是请求内核把该文件的脏数据和必要元数据同步到稳定存储。

2. **用户态 flush vs `fsync()`**

   `fflush()`、`bufio.Flush()`、`BufferedOutputStream.flush()` 通常只是把应用 buffer 里的数据推到内核。它们不保证磁盘持久化。很多误用就卡在这里。

3. **`fsync()` vs `fdatasync()`**

   `fsync()` 同步文件数据和必要元数据。`fdatasync()` 更偏数据同步，可能跳过不影响后续读取的元数据，例如某些时间戳更新。但如果文件大小变化，size 这类元数据仍然需要同步。

4. **`fsync()` vs `sync()`**

   `fsync(fd)` 针对一个文件描述符相关文件。`sync()` 是全局同步请求，会把系统范围的脏数据安排写回。数据库通常不应该用 `sync()` 代替精确的文件 fsync。

5. **`fsync()` vs `msync()`**

   `msync()` 处理 mmap 映射范围的同步。mmap 写文件时，`msync(MS_SYNC)` 可以把映射修改写回，但它和文件元数据、目录项持久化不是同一个问题。很多场景还要考虑文件 `fsync()`。

6. **文件 fsync vs 目录 fsync**

   文件 fsync 主要保证文件内容和必要元数据。目录 fsync 保证目录项变化，例如新文件名、rename、unlink。写临时文件再 rename 时，只 fsync 文件不一定够。

7. **fsync vs 设备 flush/FUA**

   fsync 是应用调用的系统调用。设备 flush/FUA 是文件系统和块层向存储设备发出的更底层命令。fsync 可能触发 flush/FUA，但应用一般不直接操作它们。

8. **fsync vs O_SYNC/O_DSYNC**

   `O_SYNC` 和 `O_DSYNC` 会让每次写入带同步语义，减少显式 fsync 调用。但它们会改变每次 write 的成本，不等于更快，也不一定替代目录 fsync。

9. **fsync vs checksum**

   fsync 解决“有没有持久化”，checksum 解决“读出来是否损坏”。fsync 成功的数据仍可能因为介质损坏、程序 bug、误写而损坏；checksum 发现损坏也不能证明数据曾经 fsync 成功。

10. **fsync vs 分布式提交**

   本地 fsync 只说明本地副本的持久化尝试完成。分布式系统里的 commit 还可能要求多数派复制、远端副本落盘、epoch 校验。不能用单机 fsync 代替 quorum commit。

一句话：fsync 是文件级持久化边界，不是用户态 flush，不是目录同步的全部，也不是分布式一致性的替代品。

## Q057. fsync 在高并发场景下可能出现哪些隐藏问题？

**回答：**

高并发下，fsync 的成本不只是“一个系统调用慢”。它会把应用提交队列、文件系统 journal、设备 flush、锁竞争和批处理策略串起来。某个点写得不好，尾延迟就会很难看。

常见隐藏问题有这些：

1. **fsync storm**

   多个线程或进程同时对同一块盘、同一目录、同一 WAL 文件 fsync。每个调用都想建立持久化边界，底层却只能按设备和文件系统能力处理。结果是 flush 频繁、队列变长、吞吐下降。

2. **group commit 实现不当**

   group commit 本来是为了减少 fsync 次数，但实现不好会引入新问题：

   - batch 等待太久，p99 上升。
   - batch 太小，吞吐上不去。
   - leader 线程慢，所有提交都排队。
   - fsync 错误没有广播给同批请求。
   - 请求被错误地提前 ack。

3. **提交锁扩大临界区**

   如果持有全局 mutex 调用 fsync，其他写入线程会全部阻塞。fsync 慢一次，后面请求一起排队。正确做法通常是缩小锁范围，把“分配 LSN、写入 buffer、等待 durable”分成清楚的阶段。

4. **多个文件之间互相影响**

   一个后台 checkpoint 正在 fsync 大文件，前台 WAL fsync 也用同一块设备。它们在应用层是两个模块，在 block layer 是同一队列。前台延迟可能被后台任务拖住。

5. **目录 fsync 被忽略或集中爆发**

   高频创建、rename、删除文件时，如果每次都目录 fsync，元数据延迟可能很高；如果完全不做，崩溃后目录项又不可靠。需要批量发布、segment 策略或 manifest 设计。

6. **fsync 错误传播给错对象**

   Linux 上 writeback error 可能在后续 fsync 中报告。高并发下，如果多个 fd、多个线程操作同一文件，错误应该如何映射给请求，需要应用层设计。不能让错误只被某个后台线程看到。

7. **同一文件 append 竞争**

   多线程同时 append record，再分别 fsync，很容易把 record 边界、LSN 顺序、ack 顺序搞乱。常见做法是单 WAL writer 或严格的提交队列。

8. **设备 flush 合并失败**

   如果每个线程都独立 fsync，底层很难有效合并。一个统一提交器可以把多次提交合并成一次 flush。没有这个层，吞吐会被 flush 次数压死。

9. **cgroup 和多租户干扰**

   容器环境里，一个服务的 fsync 可能被另一个容器的写回流量影响。I/O 限流、云盘额度、同节点 noisy neighbor 都会让 fsync 尾延迟抖动。

高并发 fsync 的设计重点不是“让每个线程自己同步”，而是建立清楚的提交队列：谁负责写 WAL，谁负责 fsync，哪些请求属于同一批，fsync 失败如何通知，ack 什么时候发出。

## Q058. fsync 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

fsync 的边界条件很容易被低估，因为正常路径只有两种结果：返回 0 或返回错误。到了崩溃、超时和重试时，真正难的是判断“上一次写入到底有没有跨过 durable boundary”。

常见边界条件如下：

1. **fsync 成功后仍要考虑目录项**

   文件内容 fsync 成功，不代表新文件名、rename 或 unlink 已经持久化。写临时文件再 rename 的场景，如果崩溃后发现正式文件名不存在，常见原因就是缺少目录 fsync。

2. **fsync 失败后的状态不一定好推理**

   fsync 返回 EIO、ENOSPC、EDQUOT 后，不能简单认为“所有数据都没写入”。可能有一部分已经写入，一部分失败。系统要进入保守状态，例如停止 ack、重建副本、只读保护或重新扫描校验。

3. **fsync 超时不等于没有生效**

   应用层超时只是等待者放弃了。内核中的写回或设备请求可能还在进行，稍后可能成功，也可能失败。如果上层马上重试同一条 record，恢复时可能看到重复写入。

4. **进程崩溃和机器断电不同**

   进程崩溃后，内核还在，之前的 dirty pages 可能继续写回。机器断电则会丢内存 dirty pages 和设备 volatile cache。只做 kill 测试不能证明 fsync 协议正确。

5. **重启后要从 durable marker 恢复**

   系统不能根据“崩溃前准备写到哪里”恢复，而要根据磁盘上可验证的 record、checksum、LSN、commit marker、manifest 来恢复。内存里的 last offset 不可信。

6. **ack 顺序和 fsync 顺序必须一致**

   如果请求 B 比请求 A 先收到成功，但 A 的 LSN 更小，崩溃恢复可能出现奇怪缺口。提交系统通常要保证 durable LSN 单调推进，ack 不能越过未同步的前序记录。

7. **重试要幂等**

   客户端没收到响应，可能重试同一个写入。服务端如果第一次已经 fsync 成功但响应丢失，第二次再写会产生重复。需要 request id、transaction id、sequence number 或去重表。

8. **checkpoint 与 WAL 删除的边界**

   checkpoint fsync 成功前，不能删除旧 WAL。否则崩溃后 checkpoint 不完整，WAL 又没了，恢复路径断掉。

9. **存储层可能延迟报告错误**

   某次写回错误可能到后续 fsync 才报告。重启恢复时发现坏数据，不能只检查最后一次写入日志；错误可能来自更早的 writeback。

10. **分布式 failover 要区分本地 fsync 和集群提交**

   leader 本地 fsync 成功，但尚未复制到多数派时崩溃，新的 leader 未必有这条记录。反过来，多数派写入成功但某个 follower 本地还没刷盘，也会影响它重启后的追赶逻辑。

处理这些边界，核心是两点：本地用可验证的 durable offset 恢复，分布式用协议定义的 commit index 或 quorum boundary 恢复。不要用“上一次函数调用到哪里了”当事实来源。

## Q059. fsync 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

fsync 的主要瓶颈通常是 I/O，但不能只看 I/O。它经常同时受锁竞争、dirty page 数量、文件系统 journal、设备 flush、云盘或网络存储影响。

可以分层看：

**CPU**

CPU 通常不是 fsync 的第一瓶颈，但在这些情况下会明显：

- checksum、压缩、加密和序列化放在 fsync 前的提交路径。
- 大量小文件导致 VFS 和文件系统元数据路径消耗 CPU。
- eBPF、审计、加密文件系统、FUSE 引入额外处理。
- 高并发系统调用导致 kernel CPU 上升。

如果 `iostat` 不忙，但 CPU system time 很高，要怀疑内核路径、锁竞争或文件系统元数据开销。

**内存**

内存影响 fsync 的方式主要是 dirty pages：

- 单次 fsync 前积累的 dirty data 越多，等待越久。
- 内存压力会触发回收，干扰写入路径。
- page cache 和应用 heap 竞争内存，可能造成 GC、OOM 或 cache thrash。

fsync 慢时，如果 `Dirty` 很高，说明它可能在替之前的 buffered writes 还债。

**锁竞争**

高并发提交路径里，锁竞争非常常见：

- WAL mutex。
- group commit leader 锁。
- inode lock。
- journal transaction lock。
- 文件系统全局或局部锁。
- 应用层队列锁。

锁竞争的特点是设备不一定很忙，但线程都在等。表现为 syscall 耗时、调度延迟、mutex wait、p99 放大。

**I/O**

这是 fsync 最常见的瓶颈来源：

- 设备写吞吐到顶。
- 随机写 IOPS 不够。
- flush/FUA 慢。
- journal commit 慢。
- 队列深度高。
- checkpoint、compaction、备份和前台 WAL 抢同一块盘。

HDD 上 seek 和旋转延迟明显；SSD 上可能有 GC 和写放大；NVMe 通常快，但也会被 flush、队列和固件行为影响。

**网络**

本地盘没有网络瓶颈，但云盘和网络文件系统有：

- 云盘 fsync 可能等待远端复制或存储服务确认。
- NFS/SMB/CephFS 会受 RTT、丢包、服务端负载影响。
- Kubernetes CSI volume 可能经过额外虚拟化和网络路径。

如果 fsync p99 和网络延迟、云盘 burst credit、服务端负载同时波动，瓶颈很可能不在本机代码。

一个实用判断：

- `Dirty` 高，`Writeback` 高：写回跟不上。
- `await` 高，队列长：块设备忙。
- 设备不忙但线程等锁：看应用锁和文件系统锁。
- 本地指标正常但 fsync 长尾：看云盘、NFS、虚拟化层、cgroup。
- CPU system 高：看小文件、元数据、FUSE、加密、内核路径。

所以 fsync 性能排查不能只问“磁盘快不快”。要看从应用提交队列到稳定存储确认的整条路径。

## Q060. fsync 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

fsync 的测试要分三类。correctness test 证明持久化语义对不对；stress test 证明并发和故障压力下不会乱；benchmark 量化吞吐、延迟和丢失窗口。只跑吞吐 benchmark，不能证明 fsync 用对了。

**correctness test 应该测什么**

1. **ack 之后崩溃可恢复**

   写入 record，fsync 成功，向客户端 ack。然后模拟崩溃并恢复，确认 ack 过的 record 一定存在，且 checksum、length、sequence 正确。

2. **未 fsync 的数据允许丢**

   写入但不 fsync，模拟机器断电。测试不应该假设这些数据一定存在。恢复逻辑应该接受“存在一部分”或“完全不存在”，并通过 record 边界判断。

3. **文件 fsync 和目录 fsync**

   测试临时文件写入、fsync 文件、rename、fsync 目录。崩溃后只能看到旧版本或新版本，不能看到半写文件或 manifest 指向不存在文件。

4. **fsync 错误处理**

   注入 ENOSPC、EDQUOT、EIO。确认系统不会在 fsync 失败后继续 ack；错误能传到调用方；恢复时不会盲信失败批次。

5. **partial write 和坏尾巴**

   构造 header 完整但 payload 不完整、checksum 缺失、length 越界的日志尾部。恢复后必须截断到 last good offset。

6. **ack 顺序**

   并发写入多个 LSN，确认 durable LSN 单调推进。不能让后面的请求先 ack，前面的请求还没同步。

7. **重试幂等**

   模拟 fsync 成功但响应丢失，客户端重试同一个请求。系统应该识别重复，而不是写出两条业务记录。

**stress test 应该测什么**

1. **高并发提交**

   多线程同时写 WAL、排队 group commit、等待 fsync。观察是否有 record 交错、LSN 缺口、重复 ack、死锁或提交队列堆积。

2. **fsync storm**

   多个文件、多个目录、多个进程同时 fsync，观察文件系统和设备是否出现长尾，应用是否能限流或降级。

3. **后台任务干扰**

   同时跑 checkpoint、compaction、snapshot、备份、日志归档，看前台 fsync p99 是否被拖垮。

4. **资源压力**

   满盘、配额满、内存压力、dirty pages 高、cgroup I/O 限流、CPU throttle。重点看错误传播和恢复语义。

5. **真实故障注入**

   kill 进程、重启机器、断电测试、块设备延迟注入、写乱序模拟、云盘 detach/attach、NFS 服务端重启。kill 进程只是其中一种，不够。

6. **长时间运行**

   fsync 问题经常在 checkpoint 周期、SSD GC、云盘额度耗尽、日志 segment 轮转时出现。短测容易漏掉。

**benchmark 应该测什么**

1. **不同同步策略**

   比较 always fsync、group commit、interval fsync、size-based fsync、O_SYNC/O_DSYNC。不要只给一个吞吐数字，要同时给语义说明。

2. **延迟分布**

   必须记录 average、p50、p95、p99、p999、max。fsync 的问题通常在尾部，不在平均值。

3. **吞吐和 batch 关系**

   改变 batch size、等待窗口、单次写入字节数、并发度，观察吞吐和延迟如何变化。

4. **工作负载形态**

   小 record、大 record、顺序 append、多文件写、频繁 rename、目录 fsync、小文件风暴。不同形态的 fsync 成本差异很大。

5. **存储和文件系统矩阵**

   ext4、XFS、btrfs、本地 NVMe、云盘、NFS、容器 volume。至少要写清文件系统、挂载参数、设备型号或云盘规格。

6. **系统指标**

   同时采集 Dirty/Writeback、iostat await、队列长度、设备 util、CPU system、上下文切换、应用提交队列长度、fsync 错误数。

7. **恢复时间**

   benchmark 不只测正常写入，还要测崩溃后的恢复扫描时间、truncate tail 时间、index 重建时间。持久化策略会直接影响恢复成本。

最后要注意测试结论的表达。`fsync` benchmark 不能只说“每秒多少次写入”，还要说“这个数字对应什么持久性语义”。否则一个 async 模式和一个 always fsync 模式放在一起比较，数字大的一方只是承诺少，不一定实现更好。

## Q061. 如果要求从零实现一个简化版 fsync，你会先定义哪些不变量？

**回答：**

从零实现简化版 `fsync()`，第一步不是写刷盘线程，而是定义“调用返回成功到底承诺了什么”。如果这个承诺不清楚，上层数据库、日志系统和恢复逻辑都会建立在沙子上。

我会先定义这些不变量：

1. **同步对象必须明确**

   `fsync(fd)` 作用于 fd 对应的文件对象，不是整个进程，也不是整个文件系统。简化实现里要先定义 fd 如何映射到 inode/file object，以及这个对象包含哪些 dirty data 和 metadata。

   如果 fd 指向普通文件，同步的是这个文件的内容和必要元数据。如果要同步目录项变化，例如 rename 后的新文件名，则必须对目录对象另行 fsync。文件 fsync 不能偷偷替代目录 fsync。

2. **fsync 有一个清楚的同步 epoch**

   调用 `fsync()` 时，要确定这次调用需要覆盖哪些修改。简化实现可以定义一个 `sync_generation`：

   - 每次写入让文件 dirty generation 增加。
   - fsync 开始时捕获当前 generation。
   - fsync 必须等待小于等于这个 generation 的 dirty pages 和必要 metadata 完成写回。
   - fsync 开始后发生的新写入，可以留给下一次 fsync。

   没有 generation，就很难处理“fsync 过程中又有人写同一个文件”的并发场景。

3. **写入完成和持久化完成是两种状态**

   `write()` 完成只说明数据进入内核缓存或文件系统事务。`fsync()` 完成才说明这批修改已经越过定义好的持久化边界。实现里要把 dirty、writeback、durable 分开，不能把“提交给写回线程”当成“已经 durable”。

4. **数据和必要元数据必须一起考虑**

   如果写入扩展了文件大小，或者分配了新的 block，fsync 不能只写 data block。它还要同步让这些数据可读所需的 metadata，例如 file size、extent/block mapping、inode 状态、journal transaction。

   反过来，普通 mtime/atime 这类不影响数据读取的 metadata，可以根据接口语义决定是否同步。`fsync()` 通常比 `fdatasync()` 更完整。

5. **不能让 metadata 指向未持久化的数据**

   如果崩溃后 metadata 已经显示文件变大、extent 已经分配，但对应数据块没有正确写入，恢复后就可能读到垃圾、旧数据或零。简化实现里也要遵守顺序：先保证数据块安全，再发布引用这些数据的 metadata，或者用 journal/事务把两者绑定起来。

6. **fsync 返回成功前，相关 writeback 必须完成**

   把 dirty page 放进 I/O 队列不够。fsync 要等待 I/O 完成，并确认后端返回成功。对有 volatile device cache 的设备，还要定义是否需要 flush/FUA。否则 fsync 成功只是“排队成功”，不是“持久化成功”。

7. **错误必须返回，不能被后台线程吞掉**

   写回失败可能发生在后台。实现要把 EIO、ENOSPC、EDQUOT 等错误记录到文件对象或 address space 上，让后续 fsync 能返回。错误还要有消费规则：返回给谁、是否 sticky、清除条件是什么。

8. **fsync 失败后的状态不能假装干净**

   如果某些页写回失败，不能把它们标成 clean。可以保留 dirty 状态、标记 error、禁止继续 ack、要求上层恢复或重建副本。最危险的是 fsync 返回错误后内部状态却像成功一样推进 durable offset。

9. **并发 fsync 要合并或正确排队**

   多个线程同时 fsync 同一个文件，简化实现可以让它们共享同一次 writeback，也可以串行执行。但不能出现后一个 fsync 返回成功，前一个覆盖的修改还没持久化的情况。durable generation 必须单调前进。

10. **fsync 不负责修复上层 record 语义**

   fsync 只同步字节和必要元数据。它不知道一条 WAL record 是否完整，也不知道业务事务是否提交。record 边界、checksum、commit marker、LSN 顺序仍然要由上层协议定义。

11. **目录同步和文件同步边界分开**

   新建文件、rename、unlink 改的是目录项。简化实现里也要明确：文件 fsync 成功后，文件内容 durable；目录 fsync 成功后，名字到 inode 的映射 durable。两个对象不能混为一谈。

12. **fsync 应该是幂等的**

   对同一个未再修改的文件重复 fsync，第二次可以很快返回，但语义不能变。第一次成功后 durable generation 已推进，后续 fsync 不应该重新引入错误状态，也不应该破坏文件内容。

一个简化结构可以是：

```text
FileObject
  dirty_pages
  dirty_metadata
  current_generation
  durable_generation
  writeback_error
  lock

fsync(file):
  target = file.current_generation
  submit dirty pages <= target
  submit required metadata <= target
  wait for writeback completion
  flush backend cache if required
  if error: record and return error
  durable_generation = max(durable_generation, target)
  return success
```

真正的文件系统会复杂得多，有 journal、transaction、block allocator、delayed allocation、writeback error accounting、barrier 和设备 flush。但简化实现也不能绕开这些不变量：范围清楚、顺序清楚、错误清楚、返回语义清楚。

## Q062. fsync 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

fsync 的误用大多不是“没听过 fsync”，而是调用了错误对象、错误时机，或者把它当成比实际更强的保证。

常见误用和症状如下：

1. **写完文件但不 fsync 目录**

   误用：写临时文件，fsync 临时文件，rename 到正式路径，然后结束。

   症状：崩溃后文件内容本身可能是完整的，但正式文件名不存在，或者目录里仍然是旧文件。配置发布、manifest 发布、SSTable 发布都容易踩这个坑。

2. **fsync 错了 fd**

   误用：写的是临时文件 A，fsync 的却是旧文件 B；rename 后继续 fsync 原路径重新打开的文件；或者忘了 fsync 新目录。

   症状：测试里看起来文件存在，断电恢复后版本回退。日志会让人困惑，因为代码“确实调用过 fsync”，但同步对象不是提交路径上的对象。

3. **把 close 当成 fsync**

   误用：写完文件后直接 close，认为 close 会保证落盘。

   症状：进程正常退出时大多没问题，机器断电后丢尾部数据。更糟的是，close 也可能返回 delayed write error，如果调用方不检查返回值，错误会被静默吞掉。

4. **忽略 fsync 返回值**

   误用：调用 fsync 只是为了“形式上同步”，不处理 EIO、ENOSPC、EDQUOT。

   症状：磁盘满或存储故障时，系统仍然向客户端返回成功。恢复后出现 WAL 缺口、index 指向不存在 offset、文件 checksum mismatch。

5. **先 ack 再 fsync**

   误用：为了降低请求延迟，写入 page cache 后先返回成功，再由后台线程 fsync。

   症状：吞吐很好，但断电后丢已经 ack 的数据。用户看到的是“服务说写入成功，重启后没了”。如果这是强持久化 API，就是语义 bug。

6. **每条小记录都 fsync**

   误用：为了安全，每写几十字节就 fsync 一次。

   症状：p99 延迟很高，吞吐被 flush 次数压死，云盘费用和 IOPS 压力上升。并发稍高就出现 fsync storm。

7. **在全局锁下 fsync**

   误用：持有 WAL mutex、数据库全局锁或请求队列锁时调用 fsync。

   症状：一次慢 fsync 卡住所有写请求。CPU 和磁盘未必满，但请求排队时间很长，尾延迟被放大。

8. **把 fsync 当成事务提交的全部**

   误用：认为只要数据文件 fsync，就不需要 WAL 顺序、commit marker、checksum、LSN。

   症状：崩溃后文件里有一部分新数据，但不知道哪些事务提交过，哪些只是写了一半。恢复只能猜，最终可能丢数据或重复应用。

9. **在网络文件系统上套用本地语义**

   误用：把 NFS、FUSE、对象存储挂载当成本地 ext4/XFS，默认 fsync 成本和语义完全一样。

   症状：多客户端可见性异常，failover 后数据缺失，fsync 尾延迟随网络抖动，偶发 ESTALE/EIO，append log 出现竞争。

10. **用 fsync 掩盖错误的写入顺序**

   误用：先写 index，再写 log，最后 fsync 两者，以为这样就安全。

   症状：崩溃后 index 指向 log 中不存在的 record。fsync 只能同步已有写入，不能把错误的依赖顺序变正确。

线上排查时，如果看到“偶发丢最后几条记录”“manifest 指向不存在文件”“重启后版本回退”“fsync p99 很高但吞吐不高”“满盘后状态不一致”，都应该回头检查 fsync 对象、顺序、错误处理和 ack 时机。

## Q063. fsync 在单机和分布式环境中的语义有什么差异？

**回答：**

单机里的 fsync 是本地持久化边界；分布式系统里的提交通常还需要复制、仲裁和故障转移语义。两者不能混为一谈。

**单机环境**

在本地文件系统上，`fsync(fd)` 的语义大致是：把这个文件的脏数据和必要元数据推到稳定存储，并等待完成。它解决的是本机崩溃后的恢复问题。

单机上你关心的是：

- `write()` 之后是否调用了 fsync。
- 文件 fsync 和目录 fsync 是否都做了。
- fsync 失败怎么处理。
- 崩溃后如何根据 durable record 恢复。
- 底层设备是否正确支持 flush/FUA。

如果只有一个节点，一个本地磁盘，一个本地文件系统，那么 fsync 成功可以作为本地 durable commit 的核心依据。当然，它仍然不等于 checksum 校验，也不等于目录项一定同步。

**分布式环境**

分布式系统里，本地 fsync 只是其中一步。客户端看到的“提交成功”可能要求：

- leader 本地 WAL 写入并 fsync。
- follower 接收日志。
- 多数派或指定副本数确认。
- 某些系统还要求 follower 也落盘。
- commit index 推进。
- 新 leader 选举后仍能保留已提交日志。

如果 leader 本地 fsync 成功，但日志还没复制到多数派，leader 立即宕机，新的 leader 可能没有这条日志。对集群来说，它不一定是 committed。反过来，如果多数派已经确认，但某个 follower 本地没 fsync，那个 follower 重启后可能需要从 leader 重新补日志。

**网络存储环境**

如果所谓“单机文件”实际位于 NFS、CephFS、云盘或 CSI volume 上，fsync 的路径会经过网络和远端服务。此时 fsync 可能涉及服务端缓存、复制、元数据服务、后端磁盘 flush。应用要看具体系统的承诺，而不能只按本地 ext4 的直觉推理。

**语义差异的核心**

单机 fsync 回答的是：“这个节点本地能不能在崩溃后找回这批字节？”

分布式 commit 回答的是：“在允许的节点故障模型下，集群是否还能保留这次提交？”

这两个问题相关，但不是一个问题。可靠系统通常两层都要做：每个副本用 fsync 保证本地恢复，复制协议用 quorum 或 commit index 保证集群恢复。

## Q064. fdatasync 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

**回答：**

`fdatasync()` 的核心目标是用较小的元数据同步范围，获得足够的数据持久化语义。它主要解决正确性问题，同时带有性能取舍。

和 `fsync()` 相比，`fdatasync()` 也要把文件数据同步到存储设备；不同点在于，它可以跳过某些不影响后续读取文件数据的元数据。例如仅仅修改访问时间或修改时间，这类 metadata 不一定要为了 fdatasync 被同步。

但如果某个 metadata 对读取数据是必要的，就不能跳过。典型例子是文件大小。如果写入把文件从 4KB 扩展到 8KB，那么 size 更新必须持久化；否则崩溃后即使数据块在磁盘上，文件大小还是 4KB，后 4KB 对应用来说不可见。extent/block mapping 也类似，没有映射就读不到数据。

所以 fdatasync 不是“只刷 data block”。更准确的说法是：它刷数据，以及让这些数据在崩溃后能被正常读取所必需的元数据。

它不是安全机制，不能防篡改，也不能认证数据来源。它对可维护性只有间接影响：如果系统把数据持久化边界定义清楚，恢复逻辑会更好维护。

面试里可以这样讲：`fsync()` 更完整，`fdatasync()` 更聚焦数据。fdatasync 试图省掉不必要的元数据同步，但不能省掉会影响文件内容可达性的元数据。

## Q065. fdatasync 的典型适用场景和不适用场景分别是什么？

**回答：**

fdatasync 适合“关心文件内容持久化，但不关心所有元数据立即持久化”的场景。它常见于 WAL、append-only log、数据文件更新等路径。

典型适用场景：

1. **WAL append**

   WAL 主要关心日志字节能否恢复。每次 append 后，如果不需要同步无关时间戳，fdatasync 往往比 fsync 更贴近需求。只要文件大小和 block mapping 等必要元数据被同步，恢复就能读取到日志。

2. **预分配日志文件**

   如果日志 segment 已经 `fallocate()` 预分配，后续写入不频繁改变文件大小和 block mapping，fdatasync 的元数据负担可能更小。很多系统会配合预分配来减少提交路径上的 metadata 工作。

3. **数据文件原地更新**

   如果写入覆盖已有范围，不扩展文件，不改变目录项，fdatasync 可以把修改的数据页同步出去，而不强迫所有 inode metadata 都同步。

4. **批量数据落盘**

   例如批处理写一个大文件，内容比 mtime 更重要。fdatasync 可以作为比 fsync 更窄的同步点。

5. **数据库提交路径**

   数据库通常有自己的事务元数据、LSN 和 checkpoint 机制。对 WAL 文件，fdatasync 常常足够表达“日志内容 durable”。

不适用或要谨慎的场景：

1. **新建文件发布**

   新文件名出现在目录里是目录项变化。fdatasync 文件不能保证目录项持久。临时文件 rename 成正式文件后，仍要考虑目录 fsync。

2. **rename、unlink、link 这类目录操作**

   这些操作的关键状态在目录，不在普通文件数据。fdatasync 普通文件解决不了。

3. **依赖完整 metadata 的场景**

   如果业务关心权限、owner、mode、xattr、mtime 等 metadata 崩溃后也必须准确，fdatasync 可能不够，应考虑 fsync 或专门同步相关对象。

4. **不清楚文件系统实现差异时**

   不同文件系统对 fdatasync 的优化程度不同。某些场景下它和 fsync 成本接近。不能只凭接口名字假设一定更快。

5. **把 fdatasync 当成分布式提交**

   fdatasync 只处理本地文件数据同步。副本复制、quorum、远端持久化不在它的语义里。

实践里，如果写入路径只关心“这些字节崩溃后能读回来”，fdatasync 很值得考虑；如果发布的是一个名字、一个版本、一个 manifest 指针，目录 fsync 和元数据协议不能省。

## Q066. fdatasync 和相近概念最容易混淆的边界在哪里？

**回答：**

fdatasync 最容易被误解成“只写数据，不写任何元数据”。这个说法太粗，会误导设计。

几个边界要分清：

1. **fdatasync vs fsync**

   `fsync()` 同步文件数据和更完整的文件元数据。`fdatasync()` 同步文件数据，以及后续读取这些数据所必需的元数据。mtime、atime 这类不影响读数据的 metadata，fdatasync 可以不强制同步。

2. **fdatasync vs write**

   `write()` 成功只说明数据进入内核路径。`fdatasync()` 要等待数据写回完成。fdatasync 不是更大号的 write，它是持久化边界。

3. **fdatasync vs 用户态 flush**

   用户态 flush 只是把应用 buffer 推到内核。fdatasync 是让内核把文件数据推向存储设备，并等待完成。

4. **fdatasync vs 目录 fsync**

   fdatasync 普通文件不能保证目录项持久。新建文件、rename、unlink 的 durability 要看目录 fsync。很多人以为 fdatasync 文件后 rename 就安全，这是不完整的。

5. **fdatasync vs O_DSYNC**

   `O_DSYNC` 是打开文件时指定的写入同步模式，让每次 write 带有类似 data-sync 的语义。`fdatasync()` 是显式系统调用。一个改变每次写的行为，一个在调用点同步。

6. **fdatasync vs msync**

   mmap 写入后，`msync()` 处理映射范围的写回；fdatasync 处理文件描述符对应文件的数据同步。mmap 场景下要小心二者覆盖范围和 metadata 问题。

7. **fdatasync vs checksum**

   fdatasync 保证写入尽量持久，checksum 检查读出来是否完整。二者解决的问题不同。fdatasync 成功不说明数据没被程序写错，checksum 通过也不说明数据按预期 fsync 过。

8. **fdatasync vs WAL commit**

   fdatasync 可以作为 WAL commit 的一个底层动作，但 WAL commit 还需要 record framing、LSN、checksum、commit marker、ack 顺序。不能把系统调用直接等同于业务提交。

更稳的理解是：fdatasync 是“文件数据可恢复”的同步接口，不是“只刷裸数据块”，也不是“发布文件名”的接口。

## Q067. fdatasync 在高并发场景下可能出现哪些隐藏问题？

**回答：**

fdatasync 比 fsync 同步的 metadata 范围可能更窄，但高并发下它仍然是同步 I/O 边界。该排队的仍会排队，该 flush 的仍可能 flush。

常见隐藏问题：

1. **多线程同时 fdatasync 同一个 WAL**

   每个线程写一条 record 后自己 fdatasync，看似简单，实际会制造大量同步请求。底层设备 flush 频繁，吞吐下降，p99 延迟变高。更好的方式通常是单 WAL writer 加 group commit。

2. **文件扩展导致 metadata 仍然很重**

   如果 append 不断扩展文件，fdatasync 仍要同步 file size 和 block mapping。此时它不一定比 fsync 便宜多少。预分配 segment 可以缓解这个问题。

3. **delayed allocation 带来的提交抖动**

   文件系统可能延迟分配块。fdatasync 时才需要真正分配、写数据、提交必要元数据。平时 write 很快，fdatasync 突然变慢。

4. **和后台 writeback 抢设备**

   fdatasync 也要等相关数据写回。高并发 buffered write 造成 dirty pages 堆积时，fdatasync 会被 writeback 压力拖慢。

5. **错误传播复杂**

   多个线程共用一个文件，后台 writeback 错误可能在某个 fdatasync 调用里暴露。应用要决定错误影响哪些请求，不能让某个线程吞掉全局写回错误。

6. **ack 顺序错乱**

   多个请求并发写 WAL 并 fdatasync，如果没有统一 LSN 和 durable offset，可能出现后写请求先 ack，前写请求失败的情况。恢复时会看到缺口。

7. **锁竞争仍然存在**

   fdatasync 可能走 inode、address_space、journal、block layer 的锁。同步范围窄不代表没有锁竞争。并发越高，提交队列和文件系统内部锁越容易放大尾延迟。

8. **多文件提交没有原子性**

   如果一次业务提交涉及多个文件，对每个文件分别 fdatasync，并不自动形成跨文件事务。崩溃后可能一个文件更新了，另一个文件没更新。需要 WAL、manifest 或两阶段发布协议。

高并发下使用 fdatasync 的重点和 fsync 类似：减少同步次数，合并提交，定义 durable LSN，处理错误广播，把文件扩展和目录变更从热路径里拿掉。

## Q068. fdatasync 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

**回答：**

fdatasync 的边界集中在一句话上：它保证数据同步，但不保证所有 metadata 和目录项都同步。崩溃恢复时，很多问题就卡在“数据写了，但名字、大小、映射、版本指针是否可见”。

常见边界条件：

1. **文件大小是必要元数据**

   append 写扩展文件后，fdatasync 必须让新 size 持久化，否则重启后数据块可能存在，但文件大小没变，应用读不到尾部数据。测试时要覆盖 append 和覆盖写两种路径。

2. **目录项不属于普通文件数据**

   新建文件后 fdatasync 文件，不能保证目录项一定持久。rename 后也一样。崩溃后可能内容写好了，但文件名不见了，或者 manifest 指针没发布。

3. **fdatasync 超时不等于未写入**

   上层等待超时后，内核 I/O 可能仍在进行。重试同一条 record 可能产生重复。需要 request id、LSN 或幂等逻辑。

4. **fdatasync 失败后可能部分写入**

   返回错误不代表所有数据都没落盘。恢复逻辑必须按 checksum、length、sequence 扫描，而不是简单回滚到内存里的 last offset。

5. **进程崩溃和机器断电不同**

   进程崩溃后，dirty pages 可能继续由内核写回。机器断电会丢内存脏页。fdatasync 的测试不能只 kill 进程。

6. **checkpoint 指针不能只靠 fdatasync 数据文件**

   checkpoint 文件内容 fdatasync 成功后，如果发布 checkpoint 的 manifest 或目录项没同步，重启后系统可能找不到新 checkpoint。

7. **重试要区分同一业务请求和新请求**

   客户端超时后重试，如果第一次 fdatasync 实际成功但响应丢了，第二次不能生成两条业务记录。日志系统要有去重或幂等提交。

8. **跨文件状态仍然可能撕裂**

   文件 A fdatasync 成功，文件 B 还没同步，机器断电。恢复时只能看到部分更新。多文件一致性要靠 WAL 或 manifest 原子发布，不靠 fdatasync 自动解决。

恢复时应该依赖磁盘上的可验证状态：record checksum、LSN、commit marker、durable manifest，而不是依赖崩溃前内存里“fdatasync 已经发起”的状态。

## Q069. fdatasync 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

**回答：**

fdatasync 的瓶颈通常仍然来自 I/O 和 flush 等待，但 workload 不同，瓶颈会在几层之间切换。

**I/O**

最常见。fdatasync 要等文件数据写回完成。如果底层设备写吞吐不足、flush 慢、队列长，fdatasync 直接变慢。云盘和网络文件系统还可能等待远端确认。

**内存和 dirty pages**

如果调用 fdatasync 前积累了大量 dirty pages，这次调用就要替之前的 buffered write 还债。`Dirty` 很高、`Writeback` 长期不降时，fdatasync p99 往往不好看。

**metadata 路径**

fdatasync 比 fsync 少同步一部分 metadata，但扩展文件、分配 extent、更新 size 时仍然要走 metadata 路径。频繁 append 小记录但不预分配，会让 fdatasync 承担块分配和元数据提交成本。

**锁竞争**

同一个 WAL 文件上高并发写入和 fdatasync，会竞争应用提交锁、inode 锁、address_space 锁、journal transaction 锁。设备不忙时 fdatasync 仍慢，常常要看锁和调度。

**CPU**

CPU 通常不是第一瓶颈，但小文件风暴、FUSE、加密文件系统、校验压缩、频繁系统调用会让 CPU system time 上升。fdatasync 调用次数太多，本身也会带来内核开销。

**网络**

本地盘没有网络因素；NFS、SMB、CephFS、云盘、容器 CSI volume 有。网络 RTT、重传、服务端负载、远端复制都会反映到 fdatasync 延迟上。

判断时可以看：

- fdatasync 前写入字节数。
- Dirty/Writeback 是否堆积。
- 文件是否频繁扩展。
- iostat 的 await、aqu-sz、util。
- 应用提交锁等待。
- 云盘 IOPS/吞吐/突发额度。
- NFS 或分布式存储服务端指标。

不要因为用了 fdatasync 就默认瓶颈消失。它只是缩小同步范围，不是绕过持久化成本。

## Q070. fdatasync 的 correctness test、stress test 和 benchmark 应该分别测什么？

**回答：**

fdatasync 的测试要特别覆盖“数据可恢复”和“非必要元数据可不恢复”的边界。否则很容易把它测成 fsync，或者漏掉文件大小、目录项这类关键问题。

**correctness test 应该测什么**

1. **覆盖写恢复**

   在已有文件范围内覆盖写，fdatasync 成功后模拟崩溃，确认新数据可读、checksum 正确。

2. **append 写恢复**

   append 扩展文件后 fdatasync，重启后确认文件 size 已更新，尾部 record 可读。这个测试验证“必要元数据不能省”。

3. **非必要 metadata 不作为强要求**

   如果测试期望 mtime/atime 必然更新，就把 fdatasync 测成 fsync 了。correctness test 应关注数据和必要元数据。

4. **目录项边界**

   新建文件、fdatasync 文件、rename，但不 fsync 目录。crash test 应允许目录项丢失。再加目录 fsync 后，验证发布语义变强。

5. **错误注入**

   模拟 ENOSPC、EDQUOT、EIO，确认 fdatasync 错误会阻止 ack，恢复逻辑不会信任失败批次。

6. **坏尾巴处理**

   构造 fdatasync 前后崩溃留下的半条 record，确认恢复能截断到 last good offset。

7. **并发 ack 顺序**

   多线程写入同一 WAL，确认 fdatasync 成功推进的 durable LSN 单调，不会让后序 record 越过前序 record。

**stress test 应该测什么**

1. **高并发 append + fdatasync**

   观察提交队列、LSN 顺序、重复 record、锁竞争和 p99 延迟。

2. **文件扩展压力**

   不预分配和预分配两组对比，观察 fdatasync 是否被 extent 分配和 size 更新拖慢。

3. **dirty page 积压**

   持续写入超过设备能力，观察 dirty throttle、Writeback、fdatasync 长尾。

4. **满盘和配额**

   fdatasync 经常暴露延迟错误。满盘测试要确认应用不继续 ack。

5. **后台干扰**

   compaction、checkpoint、备份扫描和前台 fdatasync 同盘运行，观察互相影响。

6. **网络存储故障**

   如果部署目标包含 NFS 或云盘，要测网络抖动、服务端重启、volume detach/attach。

**benchmark 应该测什么**

1. **fdatasync vs fsync**

   在覆盖写、append、预分配 append、新建文件发布等场景分别比较。不要只测一种 workload。

2. **预分配效果**

   比较 fallocate 前后的 fdatasync 延迟，尤其是 p99 和 p999。

3. **batch 策略**

   比较每条 fdatasync、group commit、interval fdatasync、size-based fdatasync。

4. **延迟分布**

   记录 average、p95、p99、p999、max。fdatasync 的价值常常体现在尾延迟，而不是平均值。

5. **系统指标**

   同时采集 Dirty/Writeback、iostat await、队列长度、CPU system、锁等待、fdatasync 错误数。

6. **恢复成本**

   benchmark 后做 crash recovery，测扫描 tail、截断、index 重建时间。同步策略会改变恢复范围。

最终报告要写清楚：fdatasync 的测试是否涉及文件扩展，是否 fsync 目录，是否预分配，底层文件系统是什么。缺这些信息，数字很难解释。

## Q071. 如果要求从零实现一个简化版 fdatasync，你会先定义哪些不变量？

**回答：**

简化版 fdatasync 可以复用很多 fsync 的框架，但同步范围要更精确。它的核心不变量是：数据必须可恢复，读取这些数据所需的元数据也必须可恢复，不必要的元数据可以不在本次同步范围内。

我会定义这些不变量：

1. **数据页同步不变量**

   fdatasync 返回成功后，本次同步 epoch 覆盖的数据页必须已经写回成功。不能只是提交到后台队列。

2. **必要元数据同步不变量**

   任何影响数据可达性的 metadata 都必须同步，包括文件大小、block/extent mapping、必要的 inode 状态、分配信息。没有这些，数据块写到磁盘也读不到。

3. **非必要元数据可延后**

   atime、mtime、ctime 这类不影响读取数据内容的 metadata，可以不被 fdatasync 强制同步。实现要明确哪些 metadata 是 data-critical，哪些不是。

4. **append 和 overwrite 区分**

   覆盖已有 block 通常不需要更新 size；append 可能需要更新 size 和 extent。实现要把这两种写入路径分开标记，否则会漏同步必要元数据。

5. **delayed allocation 必须收敛**

   如果写入时还没分配物理 block，fdatasync 必须在返回前完成分配、写数据并同步必要映射。不能让文件逻辑上写了，崩溃后却没有映射。

6. **writeback 错误传播**

   数据写回或必要元数据写回失败，fdatasync 必须返回错误。错误不能因为“只是 fdatasync，不是 fsync”而被忽略。

7. **并发 generation**

   和 fsync 一样，fdatasync 要定义同步 epoch。调用开始前已完成的写入要纳入本次同步；调用后发生的新写入可以留给后续同步。

8. **目录项不在普通文件 fdatasync 范围内**

   新文件名、rename 结果、unlink 结果属于目录对象。简化实现要明确普通文件 fdatasync 不负责这些状态。

9. **durable data generation 单调前进**

   文件可以维护 `durable_data_generation`。fdatasync 成功后推进它；失败时不能推进。上层 WAL 可以用它对应 durable LSN。

10. **幂等性**

   没有新写入时重复 fdatasync，应快速返回并保持语义一致。它不能因为跳过某些 metadata 而破坏已经 durable 的数据。

可以用这样的简化结构：

```text
FileObject
  dirty_data_pages
  data_required_metadata
  optional_metadata
  current_data_generation
  durable_data_generation
  writeback_error

fdatasync(file):
  target = current_data_generation
  write dirty data pages <= target
  write metadata required to read those pages
  wait for completion
  flush backend cache if required
  if error: return error
  durable_data_generation = target
```

这个模型刻意不承诺目录项，也不承诺所有 inode metadata。它的价值就在这里：比 fsync 范围窄，但又不能窄到让数据不可读。

## Q072. fdatasync 的常见误用是什么，误用后通常会产生什么线上症状？

**回答：**

fdatasync 的误用通常来自两个方向：一类人把它当成 fsync 的完全替代品，另一类人把它误解成只刷 data block、完全不用管 metadata。两种都危险。

常见误用和症状：

1. **用 fdatasync 发布新文件**

   误用：写新文件，fdatasync 文件，rename，然后不 fsync 目录。

   症状：崩溃后新文件名消失，manifest 回退，或者系统找不到刚生成的 SSTable/checkpoint。

2. **忽略 append 的 file size**

   误用：认为 fdatasync 不刷 metadata，所以 append 后只要数据块写了就行。

   症状：重启后文件变短，尾部 record 读不到。根因是 size 这类必要元数据没有正确进入持久化边界，或者测试没有覆盖这条路径。

3. **把 fdatasync 当成总是比 fsync 快**

   误用：把所有 fsync 替换成 fdatasync，期待性能一定提升。

   症状：延迟几乎没变。因为 workload 一直在扩展文件、分配 block、提交 journal，fdatasync 仍要同步必要 metadata。

4. **不处理 fdatasync 错误**

   误用：认为 fdatasync 只是优化接口，失败也无所谓。

   症状：满盘或 EIO 后继续 ack，恢复时 WAL 缺失或尾部损坏。

5. **跨文件事务只 fdatasync 各文件**

   误用：更新 data 文件和 index 文件，各自 fdatasync，认为二者原子一致。

   症状：崩溃后 data 新、index 旧，或者 index 新、data 旧。需要 WAL 或 manifest，不是多次 fdatasync。

6. **mmap 写入后只调用 fdatasync 但没处理映射同步**

   误用：mmap 修改文件后，不理解 msync、fdatasync、映射脏页之间的关系。

   症状：不同平台或文件系统上表现不一致。恢复后部分修改缺失，或者测试在一种环境通过、另一种环境失败。

7. **在网络文件系统上默认语义一致**

   误用：把 fdatasync 用在 NFS/FUSE/云挂载路径上，不验证服务端语义。

   症状：偶发长尾、错误延迟返回、多客户端可见性异常，服务端故障后数据状态和客户端预期不一致。

8. **用 fdatasync 掩盖缺少 checksum 和 record framing**

   误用：认为 fdatasync 成功后，日志尾部一定是完整 record。

   症状：崩溃后最后一条 record 半写，恢复不知道该截断到哪里。fdatasync 是同步动作，不是 record 完整性证明。

这些误用的共同表现是：正常压测看起来没问题，crash test 或满盘测试才暴露。fdatasync 用得好，必须和文件格式、目录 fsync、错误传播、恢复扫描一起设计。

## Q073. fdatasync 在单机和分布式环境中的语义有什么差异？

**回答：**

fdatasync 在单机上是文件数据层面的本地持久化边界；在分布式环境里，它只是某个节点、某个客户端、某个文件副本上的局部动作。分布式提交还需要复制协议来定义。

**单机环境**

本地文件系统上，fdatasync 成功后，可以把它理解为：这个文件中本次覆盖的数据，以及读取这些数据所需的元数据，已经按文件系统和设备语义同步完成。

它适合作为 WAL durable LSN 的底层依据。比如数据库写入 WAL record，fdatasync 成功后推进本地 durable LSN。机器重启后，从这个 durable LSN 之前的 record 应该能恢复，前提是文件系统和设备守约，且 record 自己有 checksum、length、LSN。

它不保证：

- 新文件名已经持久。
- rename 已经持久。
- 所有 inode metadata 都持久。
- 其他机器已经看到这次写入。
- 其他副本已经保存这次写入。

**分布式环境**

分布式系统中，fdatasync 只能说明某个副本本地做过数据同步。集群是否提交，要看复制协议：

- 单副本 fdatasync 成功，不代表多数派成功。
- leader fdatasync 成功，不代表 follower 已持久化。
- follower fdatasync 成功，也不代表日志已经 committed。
- 客户端收到成功，应该对应协议定义的 commit boundary，而不是某个节点随手同步过。

如果系统要求“多数派落盘后才 ack”，那么每个参与副本的 fdatasync 都只是 quorum commit 的组成部分。如果系统只要求“多数派收到内存后 ack”，那吞吐可能更高，但节点同时故障时风险也更大。这个语义必须在文档和配置里写清楚。

**网络文件系统和云盘**

如果单机应用把文件放在网络文件系统或云盘上，fdatasync 的路径可能经过远端服务。它是否意味着远端稳定存储、多少副本、什么故障域，取决于实现和服务承诺。不能把“本地系统调用返回成功”自动解释为“跨机房持久化成功”。

**恢复差异**

单机恢复依赖本地文件：扫描 WAL、校验 record、截断坏尾巴、重建 index。

分布式恢复还要处理：

- leader 切换。
- 日志截断和补齐。
- term/epoch。
- committed index。
- follower 本地 durable LSN 落后。
- 重复请求去重。

所以 fdatasync 的单机语义可以回答“这个副本是否尽力把数据落到本地稳定存储”；分布式语义还要回答“足够多副本是否按协议保存，并且新的 leader 会不会保留它”。后者不是 fdatasync 单独能给出的答案。

## 参考和校验点

- [Linux man-pages: write(2)](https://man7.org/linux/man-pages/man2/write.2.html) 说明 `write()` 成功不保证数据已提交到磁盘，错误可能延迟到后续 `write()`、`fsync()` 或 `close()`。
- [Linux man-pages: fsync(2)](https://man7.org/linux/man-pages/man2/fsync.2.html) 说明 `fsync()`、`fdatasync()` 的同步范围，以及文件 `fsync()` 不一定同步父目录项。
- [Linux man-pages: sync(2)](https://man7.org/linux/man-pages/man2/sync.2.html) 说明 `sync()` 和 `syncfs()` 的范围，以及 Linux 与 POSIX 对 `sync()` 等待行为的差异。
- [Linux man-pages: msync(2)](https://man7.org/linux/man-pages/man2/msync.2.html) 说明 `msync()` 用于同步 `mmap()` 修改，`MS_SYNC` 等待完成，`MS_ASYNC` 调度写回。
- [Linux man-pages: rename(2)](https://man7.org/linux/man-pages/man2/rename.2.html) 说明 `rename()` 对已有 `newpath` 的原子替换语义，以及打开 fd 不受路径 rename 影响。
- [Linux kernel ext4 documentation](https://docs.kernel.org/admin-guide/ext4.html) 说明 ext4 journaling、commit、barrier 等挂载语义，特别是 write barriers 用来保证 journal commit 的磁盘顺序。
- [Linux kernel block writeback cache control](https://docs.kernel.org/block/writeback_cache_control.html) 说明 volatile write-back cache、`REQ_PREFLUSH` 和 `REQ_FUA`，以及文件系统如何通过 flush/FUA 控制设备缓存持久化。
- [Linux man-pages: open(2)](https://man7.org/linux/man-pages/man2/open.2.html) 说明 `O_DIRECT` 尽量减少 cache 影响，但不提供 `O_SYNC` 的同步持久化保证，并列出对齐和混用 buffered I/O 的风险。
- [Linux man-pages: mmap(2)](https://man7.org/linux/man-pages/man2/mmap.2.html) 说明 `MAP_SHARED`、`MAP_PRIVATE` 的可见性和写回差异，以及精确控制映射写回需要 `msync()`。
- [Linux man-pages: fallocate(2)](https://man7.org/linux/man-pages/man2/fallocate.2.html) 说明 `fallocate()` 的空间分配、`FALLOC_FL_KEEP_SIZE`、打洞和置零等操作。
- [Linux man-pages: lseek(2)](https://man7.org/linux/man-pages/man2/lseek.2.html) 说明稀疏文件 hole 的读零语义，以及 `SEEK_DATA`、`SEEK_HOLE` 用于发现数据区和空洞。
- [Linux man-pages: open(2)](https://man7.org/linux/man-pages/man2/open.2.html) 说明 `O_APPEND` 会把 offset 移到文件末尾并把 offset 调整与写入作为原子步骤，但在 NFS 上可能因客户端模拟 append 而出现竞态。
- [Linux man-pages: read(2)](https://man7.org/linux/man-pages/man2/read.2.html) 说明 short read 不是错误，`EINTR`、`EAGAIN`、`EWOULDBLOCK` 都需要调用方按语义处理。
- [Linux man-pages: nfs(5)](https://man7.org/linux/man-pages/man5/nfs.5.html) 说明 NFS 的 close-to-open cache consistency、attribute cache、soft/hard mount 行为，以及 NFS 不提供严格集群文件系统一致性的边界。
- [Linux kernel sysctl vm documentation](https://docs.kernel.org/admin-guide/sysctl/vm.html) 说明 `dirty_background_ratio`、`dirty_ratio`、`dirty_bytes`、`dirty_background_bytes` 和 dirty writeback 相关参数。
- [Linux man-pages: proc_meminfo(5)](https://man7.org/linux/man-pages/man5/proc_meminfo.5.html) 说明 `/proc/meminfo` 中 `Dirty`、`Writeback` 等字段的含义。
- [Linux man-pages: proc_vmstat(5)](https://man7.org/linux/man-pages/man5/proc_vmstat.5.html) 说明 `/proc/vmstat` 中 `nr_dirty`、`nr_writeback`、`nr_dirtied`、`nr_written` 等统计项。
- [USENIX OSDI 2018: Finding Crash-Consistency Bugs with Bounded Black-Box Crash Testing](https://www.usenix.org/conference/osdi18/presentation/mohan) 介绍 B3、CrashMonkey 和 Ace，通过有界 workload 和模拟 power-loss crash 检查文件系统恢复状态。
