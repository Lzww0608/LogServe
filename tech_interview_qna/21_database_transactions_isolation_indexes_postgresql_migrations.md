# 21. 数据库事务、隔离级别、索引、PostgreSQL 与迁移

这一章讨论数据库事务和 PostgreSQL 工程使用中的几个基础问题：ACID、隔离级别、MVCC、锁、索引和迁移。它们经常一起出现在面试里，因为数据库正确性很少只靠一条 SQL 解决。你需要知道一个事务在失败时怎么回滚，提交后怎么保证不丢，多个事务并发时能互相看到什么，什么时候要靠隔离级别，什么时候要显式加锁，什么时候应该退回到应用层重试。

下面的回答主要参考 PostgreSQL 官方文档的 Transactions、Concurrency Control、Transaction Isolation、Explicit Locking、SELECT locking clause 和 WAL 章节。需要注意，SQL 标准给的是隔离级别和异常现象的最低要求；PostgreSQL 的实现有自己的特点，例如 `Read Uncommitted` 实际表现为 `Read Committed`，`Repeatable Read` 基于 snapshot isolation 且在 PostgreSQL 中不会出现 phantom read，而 `Serializable` 使用 Serializable Snapshot Isolation 监测危险的读写依赖，并可能要求事务重试。

## Q001. ACID 分别代表什么？

**回答：**

ACID 是事务系统最常见的四个承诺：Atomicity、Consistency、Isolation、Durability。中文通常翻译为原子性、一致性、隔离性、持久性。

这四个词很短，但面试里最好不要只背展开。真正要说明的是：它们分别约束了事务在失败、并发、约束和崩溃场景下的行为。

**Atomicity，原子性。**

原子性说的是一个事务里的多个操作要么全部生效，要么全部不生效。PostgreSQL 官方事务教程用转账例子解释这个点：Alice 扣款、Bob 加款、分行余额调整，看起来是多条 SQL，但业务上必须作为一个整体提交。中间出错时，不能出现 Alice 扣了钱、Bob 没收到的半成品状态。

典型写法是：

```sql
BEGIN;

UPDATE accounts
SET balance = balance - 100
WHERE name = 'Alice';

UPDATE accounts
SET balance = balance + 100
WHERE name = 'Bob';

COMMIT;
```

如果中间发现余额不足：

```sql
ROLLBACK;
```

事务内已经做过的修改会撤销。原子性解决的是“事务内部多步操作能不能被拆开看”的问题。

**Consistency，一致性。**

一致性这个词容易混。ACID 里的 consistency 不是分布式系统里说的 strong consistency / eventual consistency。这里主要指事务提交前后，数据库要从一个满足约束的状态进入另一个满足约束的状态。

这些约束包括：

- 主键唯一。
- 外键引用存在。
- `CHECK` 约束成立。
- `NOT NULL` 字段不为空。
- 触发器维护的业务约束成立。
- 应用层在事务内维护的业务不变量成立。

数据库能帮你检查一部分，比如唯一约束、外键、检查约束。但很多业务一致性要靠应用自己写对事务。例如“订单总金额等于明细金额之和”，如果没有触发器或约束，数据库不会自动知道这个规则。

**Isolation，隔离性。**

隔离性说的是多个事务并发执行时，彼此能看到什么。没有隔离，事务 A 改了一半的数据可能被事务 B 读到，事务 B 就会基于半成品状态做决策。

隔离性不是只有一个强度。数据库通常提供多个隔离级别，例如：

```text
Read Committed
Repeatable Read
Serializable
```

隔离越强，能避免的并发异常越多，但锁、冲突、重试或监测成本也可能增加。PostgreSQL 默认是 `Read Committed`，不是最强隔离。

**Durability，持久性。**

持久性说的是事务一旦 `COMMIT` 成功并且数据库向客户端确认完成，它的结果不能因为随后数据库崩溃就消失。

PostgreSQL 的持久性主要依赖 WAL。官方 WAL 文档说明，数据页真正写入磁盘前，描述这些修改的 WAL record 必须先刷到持久存储。崩溃后，数据库可以用 WAL 做 redo，重放已经提交但还没写入数据文件的修改。

所以 ACID 可以这样记：

```text
Atomicity   : 事务内部要么全做，要么全不做
Consistency: 提交前后满足约束和业务不变量
Isolation  : 并发事务之间看不到不该看到的中间状态
Durability : 提交成功后，崩溃恢复也不能丢
```

**常见误区。**

第一，把 consistency 理解成“所有副本立刻一致”。这是分布式复制的一致性语义，不是 ACID 里的核心含义。

第二，以为用了事务就自动不会有并发 bug。事务有隔离级别，`Read Committed` 下仍然可能有不可重复读、幻读、写偏斜、丢失更新一类问题。要按业务不变量选择隔离级别、显式锁或重试策略。

第三，以为 `COMMIT` 返回就一定等于跨副本持久。单机 PostgreSQL 默认要求本地 WAL 持久化，但如果涉及同步复制、异步提交、`synchronous_commit` 配置，确认语义会变化。面试里最好说清楚“持久性是对数据库承诺的提交语义而言，具体强度取决于 WAL、fsync、同步提交和复制配置”。

面试里可以这样回答：

```text
ACID 分别是 Atomicity、Consistency、Isolation、Durability。Atomicity 保证事务多步操作要么全部提交要么全部回滚；Consistency 保证事务提交后数据库仍满足约束和业务不变量；Isolation 约束并发事务之间能看到什么，避免读到半成品或产生不可接受的并发异常；Durability 保证事务提交并被数据库确认后，即使随后崩溃也能通过 WAL 等机制恢复提交结果。事务不是只负责回滚，它还定义了并发可见性和崩溃恢复语义。
```

## Q002. 事务的 atomicity 和 durability 有什么区别？

**回答：**

Atomicity 和 durability 都和失败有关，但关注的失败点完全不同。

Atomicity 关注的是：事务还没成功提交时，如果中间失败，已经做过的部分修改怎么办。

Durability 关注的是：事务已经成功提交并告诉客户端成功后，如果数据库崩溃，提交结果会不会丢。

可以用一条时间线区分：

```text
BEGIN
  statement 1
  statement 2
  statement 3
COMMIT returns success
crash
```

在 `COMMIT returns success` 之前，主要考验 atomicity。
在 `COMMIT returns success` 之后，主要考验 durability。

**Atomicity：失败时不能留下半个事务。**

假设转账事务有两步：

```sql
BEGIN;
UPDATE accounts SET balance = balance - 100 WHERE name = 'Alice';
UPDATE accounts SET balance = balance + 100 WHERE name = 'Bob';
COMMIT;
```

如果第一条 `UPDATE` 成功，第二条失败，atomicity 要求 Alice 的扣款也不能留下。数据库要么把整个事务回滚，要么让应用显式 `ROLLBACK`。其他事务也不应该观察到“扣款成功但加款未发生”的半成品状态。

这类问题通常通过 undo 信息、事务状态、MVCC 可见性、回滚机制来处理。对应用来说，你看到的是：事务没提交，效果不应该成为数据库的永久状态。

**Durability：提交后不能因为崩溃丢失。**

如果两条更新都完成，`COMMIT` 返回成功，用户已经拿到“转账成功”的响应。紧接着机器断电。Durability 要求数据库重启后，这笔转账仍然存在。

PostgreSQL 用 WAL 支撑这个承诺。WAL 的基本规则是：数据页可以晚点刷盘，但描述修改的 WAL record 要先刷到持久存储。崩溃后，数据库根据 WAL 把已提交但尚未落到数据文件里的修改 redo 出来。

这里要注意，durability 不是“每次更新都立刻把所有数据页写到磁盘”。PostgreSQL 官方 WAL 文档明确说，只要 WAL 已经持久化，数据页不必每次提交都刷盘，恢复时可以用 WAL 重做。这也是 WAL 比直接刷所有脏页更高效的原因。

**二者的差异可以这样表述。**

```text
Atomicity:
  commit 前失败 -> 整个事务像没发生过

Durability:
  commit 成功后失败 -> 整个事务仍然保留
```

Atomicity 解决“不要部分生效”。Durability 解决“已经确认的成功不要丢”。

**为什么两者容易混？**

因为它们都和崩溃恢复有关。数据库崩溃后，会同时处理两类事务：

```text
committed before crash:
  redo，确保它们存在

not committed before crash:
  undo / ignore，确保它们不留下半成品
```

从恢复角度看，durability 要保护已提交事务，atomicity 要清理未提交事务。

**工程上常见的边界。**

第一，`COMMIT` 成功前客户端超时，结果是 unknown。客户端不知道数据库到底提交了没有，不能简单重试非幂等操作。要用 idempotency key、业务唯一约束或事务查询来确认。

第二，`synchronous_commit=off` 会改变“成功返回”和“WAL 确认落盘”之间的关系。PostgreSQL 文档说明，关闭同步提交时，数据库崩溃可能丢失一些已经报告成功的最近事务，但数据库状态仍像这些事务干净 abort 一样一致。也就是说，atomicity 仍然成立，但 durability 被有意放松。

第三，`fsync=off` 风险更大。PostgreSQL 文档明确警告，关闭 `fsync` 可能在断电或系统崩溃后导致不可恢复的数据损坏，不能把它当成普通性能开关。

面试里可以这样回答：

```text
atomicity 管 commit 前的失败：事务执行到一半出错时，不能留下部分修改，要么全部提交，要么全部回滚。durability 管 commit 后的失败：数据库已经向客户端确认提交后，即使随后崩溃，提交结果也要能恢复。恢复时，已提交事务靠 WAL redo 保住，这是 durability；未提交事务不能留下半成品，这是 atomicity。两者都和故障有关，但一个保护未完成事务的“全或无”，一个保护已提交事务的“不丢”。
```

## Q003. 脏读、不可重复读、幻读分别是什么？

**回答：**

脏读、不可重复读、幻读是 SQL 隔离级别里最经典的三个并发读异常。PostgreSQL 官方隔离级别文档沿用 SQL 标准的定义：这些现象描述的是并发事务互相影响后，低隔离级别可能允许什么。

**脏读：读到别人未提交的数据。**

脏读是指事务 A 读到了事务 B 还没提交的写入。

```text
T1:
  BEGIN;
  UPDATE accounts SET balance = 0 WHERE id = 1;
  -- not commit yet

T2:
  SELECT balance FROM accounts WHERE id = 1;
  -- sees 0

T1:
  ROLLBACK;
```

如果 T2 读到了 0，就读到了一个后来被回滚的数据。这个数据从数据库提交历史看从未真正存在过，所以叫 dirty read。

PostgreSQL 不会发生脏读。即使你请求 `Read Uncommitted`，PostgreSQL 内部也把它当成 `Read Committed`，因为 MVCC 架构下这是合理映射。官方隔离表里也写明，`Read Uncommitted` 在 PostgreSQL 中不允许脏读。

**不可重复读：同一事务里两次读同一行，结果变了。**

不可重复读是指事务 A 在同一个事务中读取同一行两次，中间事务 B 修改并提交了这行，导致 A 第二次读到不同结果。

```text
T1:
  BEGIN;
  SELECT balance FROM accounts WHERE id = 1; -- 100

T2:
  BEGIN;
  UPDATE accounts SET balance = 50 WHERE id = 1;
  COMMIT;

T1:
  SELECT balance FROM accounts WHERE id = 1; -- 50
```

这在 PostgreSQL 的 `Read Committed` 下可能发生，因为每条 SQL 语句开始时拿一个新的 snapshot。第一次 `SELECT` 和第二次 `SELECT` 的 snapshot 不同，所以看到的 committed 数据可以不同。

在 PostgreSQL `Repeatable Read` 下，同一个事务中的普通查询看到的是事务开始时的稳定 snapshot，所以不会出现不可重复读。

**幻读：同一事务里两次执行同一个范围查询，行集合变了。**

幻读不是同一行的值变了，而是满足条件的行集合变了。

```text
T1:
  BEGIN;
  SELECT count(*) FROM orders WHERE status = 'pending'; -- 10

T2:
  BEGIN;
  INSERT INTO orders(status) VALUES ('pending');
  COMMIT;

T1:
  SELECT count(*) FROM orders WHERE status = 'pending'; -- 11
```

第二次查询多出来一行，这行像“幻影”一样出现在范围查询结果里，所以叫 phantom read。

PostgreSQL 的行为有一个重要细节：SQL 标准允许 `Repeatable Read` 出现 phantom read，但 PostgreSQL 的 `Repeatable Read` 实现基于 snapshot isolation，官方文档明确说它不会出现 phantom read。这是 PostgreSQL 比标准最低要求更强的地方。

**三者的关键差别。**

```text
dirty read:
  读到未提交数据

nonrepeatable read:
  同一行，前后两次读值不同，因为别人提交了更新

phantom read:
  同一范围查询，前后两次读到的行集合不同，因为别人提交了插入/删除/更新
```

**还有一个更重要的异常：serialization anomaly。**

面试里只说前三个还不够，因为现代 MVCC 数据库里更常见的问题是 serialization anomaly，也就是一组事务都提交成功，但结果无法等价于任何串行顺序。PostgreSQL 官方文档把它列在隔离级别表里：只有 `Serializable` 禁止 serialization anomaly。

比如写偏斜：

```text
规则：至少一个医生值班

T1 看到 doctor A 和 B 都值班，于是让 A 下班
T2 也看到 A 和 B 都值班，于是让 B 下班
两个事务都提交后，没有医生值班
```

每个事务单独看都没违反自己看到的规则，但合并结果违反业务不变量。`Repeatable Read` 的稳定 snapshot 不一定防住这种问题；`Serializable` 才试图保证提交结果等价于某个串行执行。

面试里可以这样回答：

```text
脏读是读到其他事务尚未提交、后来可能回滚的数据；不可重复读是同一事务中两次读同一行，第二次看到别人已提交的修改；幻读是同一事务中两次执行同一个范围查询，第二次满足条件的行集合变了。PostgreSQL 不允许脏读；Read Committed 下可能有不可重复读和幻读；PostgreSQL 的 Repeatable Read 基于 snapshot isolation，不会出现不可重复读和幻读，但仍可能有 serialization anomaly；真正要防串行化异常要用 Serializable，并准备处理 40001 重试。
```

## Q004. read committed、repeatable read、serializable 的区别是什么？

**回答：**

这三个隔离级别的差异，可以先用一句话概括：

```text
Read Committed:
  每条语句看一个新的已提交快照

Repeatable Read:
  整个事务看一个稳定快照

Serializable:
  在稳定快照基础上监测读写依赖，保证成功提交的事务等价于某个串行顺序
```

PostgreSQL 官方文档里还有两个实现细节要记住：

1. `Read Committed` 是 PostgreSQL 默认隔离级别。
2. PostgreSQL 内部只实现三种不同隔离行为，`Read Uncommitted` 的行为和 `Read Committed` 一样。

**Read Committed。**

`Read Committed` 下，普通 `SELECT` 只能看到查询开始前已经提交的数据，看不到未提交数据，也看不到查询执行过程中其他事务刚提交的数据。关键点是：snapshot 是按语句取的，不是按事务取的。

```sql
BEGIN ISOLATION LEVEL READ COMMITTED;

SELECT balance FROM accounts WHERE id = 1; -- snapshot 1

-- another transaction commits update here

SELECT balance FROM accounts WHERE id = 1; -- snapshot 2

COMMIT;
```

两次 `SELECT` 可能看到不同结果。这就是不可重复读。

`Read Committed` 的好处是简单、吞吐好、冲突少。很多 OLTP 系统默认用它。但它不适合复杂“先读一组数据，再基于这组数据做业务决策”的事务，除非你显式加锁或用条件更新保护不变量。

**Repeatable Read。**

PostgreSQL 的 `Repeatable Read` 在事务第一条非事务控制语句开始时建立 snapshot，后续查询都看这个 snapshot。事务内连续查询同一行或同一范围，看到的视图稳定。

```sql
BEGIN ISOLATION LEVEL REPEATABLE READ;

SELECT count(*) FROM orders WHERE status = 'pending'; -- snapshot fixed

-- other transaction inserts pending order and commits

SELECT count(*) FROM orders WHERE status = 'pending'; -- still same snapshot

COMMIT;
```

这能避免不可重复读。PostgreSQL 的实现还避免 phantom read，强于 SQL 标准对 Repeatable Read 的最低要求。

代价是：如果你试图修改一个在事务开始后被其他事务更新过的行，PostgreSQL 可能报：

```text
ERROR: could not serialize access due to concurrent update
```

应用需要回滚并重试整个事务。

**Serializable。**

`Serializable` 是最强隔离级别。它要求所有成功提交的并发事务，结果等价于某个串行执行顺序。

PostgreSQL 的 `Serializable` 不是简单把所有事务串行执行，也不是用大锁阻塞所有读写。官方文档说明，它和 `Repeatable Read` 类似，但会额外监测可能导致 serialization anomaly 的条件。如果发现一组读写依赖无法对应到任何串行顺序，就让其中一个事务失败：

```text
ERROR: could not serialize access due to read/write dependencies among transactions
SQLSTATE: 40001
```

这意味着 `Serializable` 的正确用法不是“开了就永远成功”，而是：

```text
run transaction
if SQLSTATE 40001:
  rollback
  retry whole transaction
```

**三者对异常的关系。**

按 PostgreSQL 官方表格：

```text
Read Committed:
  dirty read: no
  nonrepeatable read: possible
  phantom read: possible
  serialization anomaly: possible

Repeatable Read:
  dirty read: no
  nonrepeatable read: no
  phantom read: no in PostgreSQL
  serialization anomaly: possible

Serializable:
  dirty read: no
  nonrepeatable read: no
  phantom read: no
  serialization anomaly: no for committed transactions
```

**怎么选。**

`Read Committed` 适合大多数简单事务：按主键更新、单行状态流转、用唯一约束防重复、用条件 `UPDATE ... WHERE version = ?` 做乐观并发。

`Repeatable Read` 适合需要一个稳定读视图的事务，比如生成一致性报表、在一个固定 snapshot 上做多次查询。但它不能自动保证跨行、跨表业务规则在并发下都成立。

`Serializable` 适合业务不变量复杂、显式锁很难写对、能接受事务重试的场景。它可以简化推理：只要单个事务在串行执行时正确，并且所有相关事务都用 `Serializable`，成功提交的结果就等价于某个串行顺序。代价是监测开销和 serialization failure 重试。

面试里可以这样回答：

```text
Read Committed 是 PostgreSQL 默认隔离级别，每条语句看到语句开始时的已提交 snapshot，所以同一事务中两次 SELECT 可能看到不同结果。Repeatable Read 在事务开始后使用稳定 snapshot，PostgreSQL 中不会出现不可重复读和幻读，但仍可能发生 serialization anomaly，更新并发修改过的行时可能要求重试。Serializable 在 Repeatable Read 的基础上监测危险的读写依赖，保证成功提交的事务等价于某个串行顺序；应用必须统一处理 SQLSTATE 40001 并重试整个事务。
```

## Q005. MVCC 的基本思想是什么？

**回答：**

MVCC 是 Multiversion Concurrency Control，多版本并发控制。它的基本思想是：数据库不只保留一份“当前行”，而是用多个行版本和事务可见性规则，让读事务看到一个一致的 snapshot，同时允许写事务继续写。

PostgreSQL 官方并发控制介绍里说，MVCC 意味着每条 SQL 语句看到的是某个时间点的数据库版本，而不是底层数据文件的最新物理状态。它的直接好处是：读锁不和写锁冲突，读不阻塞写，写也不阻塞读。

**传统锁模型的问题。**

如果数据库只有一份数据，读写互斥很容易：

```text
reader locks row
writer waits

writer locks row
reader waits
```

这能保证隔离，但并发性能差。读多写多场景下，查询和更新会互相拖住。

MVCC 换了一个思路：写入时不直接覆盖旧版本，而是创建新版本；读事务根据自己的 snapshot 判断哪个版本对自己可见。

**一个简化例子。**

假设账户行最初是：

```text
accounts(id=1, balance=100)  version created by tx10
```

事务 T20 更新它：

```text
old version: balance=100, visible to old snapshots
new version: balance=50, created by tx20
```

在 T20 提交前，其他事务仍然看旧版本。T20 提交后，新启动的语句或事务可以看新版本。早就开始的 `Repeatable Read` 事务可能继续看旧版本。

所以 MVCC 不是“没有锁”，而是“普通读不用阻塞普通写”。写写冲突、行锁、唯一约束、`SELECT FOR UPDATE` 仍然存在。

**PostgreSQL 里的可见性大致依赖什么。**

简化说，每个行版本会带事务相关的元数据。查询拿到 snapshot 后，会判断：

- 创建这个版本的事务是否已经提交。
- 删除/更新这个版本的事务是否已经提交。
- 这些事务相对于当前 snapshot 是不是可见。

如果版本对 snapshot 可见，就能被这条语句看到；否则跳过。

这也是为什么 PostgreSQL 需要 vacuum。旧行版本不能立即删除，因为可能还有老事务的 snapshot 需要读它们。等确认没有活跃事务再需要旧版本，vacuum 才能清理 dead tuples。

**MVCC 带来的好处。**

第一，读写并发好。普通 `SELECT` 不会因为别人正在更新同一行就阻塞，反过来写入也不会因为普通读而阻塞。

第二，snapshot 语义清楚。`Read Committed` 每条语句一个 snapshot，`Repeatable Read` 整个事务一个 snapshot，行为可以解释。

第三，崩溃恢复和回滚更自然。未提交版本对其他事务不可见，事务失败后这些版本不会成为可见状态。

**MVCC 的成本。**

第一，存储膨胀。更新不是原地覆盖，会产生旧版本。高更新表如果 vacuum 跟不上，会出现 bloat。

第二，长事务危险。一个长时间打开的事务会持有旧 snapshot，阻止旧版本清理，导致表和索引膨胀。

第三，并发异常仍然存在。MVCC 让读写不互相阻塞，但 `Read Committed` 下仍可能不可重复读；snapshot isolation 下仍可能写偏斜。不能把 MVCC 当成自动 serializable。

第四，索引维护更复杂。表里有多个版本，索引项也需要和可见性、vacuum、HOT update 等机制配合。面试里不用展开到存储细节，但要知道 MVCC 不是免费午餐。

**MVCC 和锁的关系。**

PostgreSQL 官方文档说 MVCC 避免了传统锁方法的一些冲突，但表级和行级锁仍然存在。比如：

- `UPDATE` 会锁要更新的行。
- `SELECT FOR UPDATE` 会锁返回的行。
- DDL 可能拿更强表锁。
- Serializable 会使用 predicate locking 的机制监测读写依赖。

所以更准确的说法是：

```text
MVCC 减少读写阻塞，不是消灭所有锁。
```

面试里可以这样回答：

```text
MVCC 的基本思想是为数据保留多个版本，每条语句或每个事务根据自己的 snapshot 选择可见版本。写入创建新版本，旧 snapshot 仍能读旧版本，所以普通读不阻塞写，写也不阻塞普通读。PostgreSQL 的 Read Committed 是每条语句一个 snapshot，Repeatable Read 是事务级稳定 snapshot。MVCC 提升并发，但会带来 dead tuple、vacuum、表膨胀、长事务阻塞清理等成本，也不能自动消除所有并发异常。
```

## Q006. PostgreSQL 中 snapshot isolation 如何工作？

**回答：**

在 PostgreSQL 里，`Repeatable Read` 隔离级别的行为通常可以理解为 snapshot isolation。事务在开始执行第一条真正语句时拿到一个 snapshot，后续普通查询都基于这个 snapshot 判断行版本可见性。这个 snapshot 不会因为其他事务提交而改变。

先看和 `Read Committed` 的区别：

```text
Read Committed:
  每条语句一个新 snapshot

Repeatable Read:
  整个事务一个稳定 snapshot
```

PostgreSQL 官方文档明确说，`Repeatable Read` 事务中的查询看到的是事务第一条非事务控制语句开始时的 snapshot，而不是每条语句开始时的新 snapshot。

**snapshot 里大致包含什么。**

可以把 snapshot 想象成一张“可见性清单”：

```text
snapshot:
  已经提交的事务可以看
  尚未提交的事务不能看
  snapshot 创建后才提交的事务不能看
  本事务自己的修改可以看
```

真实实现比这复杂，会涉及事务 ID、活跃事务集合、提交状态等。但面试回答里，重点是“读的是一个逻辑时间点的数据库版本”。

**例子：稳定读视图。**

```text
T1:
  BEGIN ISOLATION LEVEL REPEATABLE READ;
  SELECT balance FROM accounts WHERE id = 1; -- 100

T2:
  BEGIN;
  UPDATE accounts SET balance = 50 WHERE id = 1;
  COMMIT;

T1:
  SELECT balance FROM accounts WHERE id = 1; -- still 100
  COMMIT;
```

T1 第二次读仍然看到 100，因为它的 snapshot 建立在 T2 提交之前。T2 的新版本对 T1 不可见。

**但本事务自己的修改可见。**

```sql
BEGIN ISOLATION LEVEL REPEATABLE READ;

SELECT balance FROM accounts WHERE id = 1; -- 100

UPDATE accounts
SET balance = 80
WHERE id = 1;

SELECT balance FROM accounts WHERE id = 1; -- 80

COMMIT;
```

即使事务 snapshot 固定，本事务自己的写入仍然可见。PostgreSQL 官方文档也特别指出，每个查询会看到本事务之前更新的效果，即使这些更新还没提交。

**写入冲突怎么处理。**

Snapshot isolation 不是让所有人都随便写。若 T1 试图更新某个行版本，而这行在 T1 的 snapshot 之后已经被其他事务更新并提交，PostgreSQL 在 `Repeatable Read` 下会要求 T1 回滚重试。

典型错误：

```text
ERROR: could not serialize access due to concurrent update
```

原因是：T1 不能在一个旧 snapshot 上修改一个已经被新提交事务改变过的行。正确处理是整个事务重试，而不是只重试最后一条 SQL。

**为什么 PostgreSQL Repeatable Read 没有 phantom read。**

SQL 标准允许 `Repeatable Read` 出现 phantom read，但 PostgreSQL 基于 snapshot 的实现会让范围查询也看同一个 snapshot。

```text
T1:
  SELECT count(*) FROM orders WHERE status='pending'; -- 10

T2:
  INSERT pending order;
  COMMIT;

T1:
  SELECT count(*) FROM orders WHERE status='pending'; -- still 10
```

T2 插入的新行对 T1 的 snapshot 不可见，所以不会出现 phantom read。

**snapshot isolation 仍然不是 serializable。**

最容易漏掉的是写偏斜。两个事务都读同一个稳定 snapshot，各自修改不同的行，结果合起来违反业务规则。

```text
规则：至少一名医生值班

T1 snapshot: A on call, B on call
T2 snapshot: A on call, B on call

T1 turns off A
T2 turns off B

both commit under snapshot isolation
final: no doctor on call
```

两个事务没有更新同一行，所以不会发生 direct write-write conflict。但业务规则被破坏。这类问题需要显式锁、约束重构、把不变量收敛到同一行，或者使用 PostgreSQL `Serializable`。

**工程使用边界。**

Snapshot isolation 很适合需要一致读视图的场景：

- 生成报表。
- 多次查询必须基于同一时间点。
- 读多写少的一致性分析。

但如果事务要“读一批数据，然后决定能不能写另一批数据”，要小心写偏斜。面试里不要把 `Repeatable Read` 说成“完全串行化”。

面试里可以这样回答：

```text
PostgreSQL 的 Repeatable Read 可以理解为 snapshot isolation：事务在第一条非事务控制语句开始时建立 snapshot，后续普通查询都看这个稳定数据库版本，同时能看到本事务自己的修改。其他事务后来提交的更新和插入对它不可见，所以 PostgreSQL 的 Repeatable Read 不会出现不可重复读和幻读。写入时如果目标行在事务开始后被别人更新并提交，可能报 concurrent update 并要求重试。Snapshot isolation 仍然可能有写偏斜，所以它不等于 Serializable。
```

## Q007. serializable isolation 为什么可能产生 serialization failure？

**回答：**

`Serializable` 的目标不是让所有事务都成功，而是让所有成功提交的并发事务结果等价于某个串行执行顺序。如果数据库发现一组事务的读写依赖无法对应到任何串行顺序，就必须让其中至少一个事务失败。这就是 serialization failure。

PostgreSQL 的典型错误是：

```text
ERROR: could not serialize access due to read/write dependencies among transactions
SQLSTATE: 40001
```

应用看到这个错误时，正确做法是回滚并重试整个事务。

**为什么不是等待就行？**

在悲观锁模型里，冲突可以靠阻塞等待解决：

```text
T2 waits for T1
```

但 PostgreSQL 的 Serializable 使用 Serializable Snapshot Isolation，核心不是所有读写互相阻塞，而是在 MVCC snapshot 的基础上监测可能导致串行化异常的读写依赖。官方文档说，这种监测不引入超过 Repeatable Read 的阻塞，但会有额外监测开销；当检测到可能导致 serialization anomaly 的条件时，会触发 serialization failure。

所以它的策略更接近：

```text
让事务并发运行
监测危险依赖
提交时发现不可串行化
回滚其中一个事务
让应用重试
```

**PostgreSQL 官方例子的核心。**

官方文档给了一个聚合读后插入的例子。可以简化成：

```text
初始：
class=1: value 10, 20
class=2: value 100, 200

T1:
  SELECT sum(value) WHERE class = 1; -- 30
  INSERT class=2, value=30

T2:
  SELECT sum(value) WHERE class = 2; -- 300
  INSERT class=1, value=300
```

如果 T1 先串行执行，T2 读 class=2 时应该看到 T1 插入的 30，结果应是 330。
如果 T2 先串行执行，T1 读 class=1 时应该看到 T2 插入的 300，结果应是 330。
但并发 snapshot 下，T1 读到 30，T2 读到 300，两者都基于旧视图插入。这个最终结果无法解释成任意一个串行顺序。

PostgreSQL 因此会让其中一个事务提交失败。

**Predicate locking 的作用。**

Serializable 要防的不只是“同一行被两个人更新”。它还要知道：

```text
某个事务读过一个范围
另一个事务后来写入了会影响这个范围查询结果的数据
```

PostgreSQL 使用 predicate locking 来识别这种依赖。官方文档也说明，这些 SIReadLock 不会像普通锁一样阻塞写入，也不会导致死锁；它们用于记录读写依赖，判断是否可能出现 serialization anomaly。

这点很重要：Serializable 不是简单给所有范围查询加阻塞锁。它可能让事务运行到提交阶段才失败。

**为什么应用必须重试整个事务？**

因为 serialization failure 表示事务基于的读视图不能安全提交。只重试最后一条 SQL 不够，前面的读取结果也可能失效。正确模式是：

```pseudo
for attempt in 1..max:
  begin serializable
  try:
    read
    compute
    write
    commit
    return success
  catch SQLSTATE 40001:
    rollback
    backoff
    retry whole transaction
```

**哪些情况更容易产生 serialization failure？**

- 长事务，读写窗口大。
- 范围查询多。
- 顺序扫描导致 predicate lock 粒度变粗。
- 热点表上读写混合。
- 事务里做太多无关查询。
- 并发连接数过高。
- 缺少合适索引，导致 predicate lock 可能从 tuple/page 提升到 relation 级。

PostgreSQL 官方文档也建议：声明只读事务、控制活跃连接数、事务尽量短、不要长时间 idle in transaction、减少不必要显式锁，并通过索引扫描降低粗粒度 predicate lock 带来的失败率。

**Serializable 的价值。**

`Serializable` 的优点是推理简单。只要每个事务在单独串行执行时能维护业务不变量，所有相关事务都使用 `Serializable`，那么成功提交的结果也能维护这些不变量。代价是系统要接受重试。

面试里可以这样回答：

```text
Serializable 要保证所有成功提交的并发事务等价于某个串行顺序。PostgreSQL 用 Serializable Snapshot Isolation 在 snapshot isolation 基础上监测读写依赖；如果发现一组事务的结果无法对应到任何串行执行顺序，就让其中一个事务失败，返回 SQLSTATE 40001。它不是简单阻塞所有冲突，而是允许并发执行并在发现危险依赖时回滚重试。因此应用必须把 serialization failure 当成正常控制流，回滚并重试整个事务。
```

## Q008. 乐观并发控制和悲观并发控制有什么区别？

**回答：**

乐观并发控制和悲观并发控制的差别，在于系统对冲突发生概率的假设不同。

悲观并发控制认为冲突很可能发生，所以先锁住资源，再做操作。
乐观并发控制认为冲突不常发生，所以先做操作，提交时检查有没有冲突。

**悲观并发控制：先占住，再修改。**

典型例子是 `SELECT ... FOR UPDATE`：

```sql
BEGIN;

SELECT *
FROM tasks
WHERE id = 42
FOR UPDATE;

UPDATE tasks
SET status = 'running'
WHERE id = 42;

COMMIT;
```

第一个事务拿到行锁后，其他想更新或锁定同一行的事务要等待、报错或跳过，取决于是否使用 `NOWAIT` / `SKIP LOCKED`。

悲观控制适合：

- 冲突频繁。
- 冲突代价高。
- 资源必须独占。
- 队列表抢任务。
- 余额扣减、库存预占等关键路径。

代价是：

- 阻塞等待。
- 死锁风险。
- 锁持有时间影响吞吐。
- 长事务会拖垮并发。

**乐观并发控制：提交时检查版本。**

常见写法是版本号或条件更新：

```sql
UPDATE tasks
SET status = 'running',
    version = version + 1
WHERE id = 42
  AND version = 7;
```

如果返回影响行数是 1，说明没有别人抢先修改。
如果影响行数是 0，说明版本变了，应用重新读取、重新判断、重试或返回冲突。

也可以用 PostgreSQL 的唯一约束、`INSERT ... ON CONFLICT`、event store expected revision 等机制实现乐观控制。

乐观控制适合：

- 冲突少。
- 读多写少。
- 请求可以重试。
- 不想长时间持锁。
- Web API 的编辑提交。

代价是：

- 冲突时要重试整个业务逻辑。
- 冲突率高时浪费计算。
- 应用必须处理 retry 和幂等。
- 只检查单行版本时，跨行不变量仍可能漏掉。

**两者不是互斥的。**

一个系统经常同时使用两者：

```text
普通编辑：
  optimistic version check

任务抢占：
  SELECT FOR UPDATE SKIP LOCKED

唯一业务键：
  UNIQUE constraint + ON CONFLICT

复杂不变量：
  Serializable + retry
```

PostgreSQL 的 MVCC 本身偏向让读写并发，减少普通读写锁冲突。但当业务要独占某些行时，你仍然可以显式使用 `SELECT FOR UPDATE` 这类悲观锁。

**和隔离级别的关系。**

隔离级别定义事务能看到什么，并发异常如何处理。乐观/悲观并发控制是应用或数据库处理写冲突的策略。

例如：

- `Read Committed` + `SELECT FOR UPDATE` 是悲观锁常见组合。
- `Read Committed` + `UPDATE ... WHERE version=?` 是乐观控制常见组合。
- `Serializable` 更偏乐观：让事务并发执行，发现不可串行化时失败，让应用重试。

**工程判断。**

如果冲突概率低，用乐观锁通常吞吐更好。
如果冲突概率高，乐观锁会不停失败重试，悲观锁反而更直接。
如果锁持有时间长，悲观锁容易造成排队和死锁。
如果重试代价大，乐观锁也可能不合适。

面试里可以这样回答：

```text
悲观并发控制假设冲突会发生，所以先拿锁再修改，例如 SELECT FOR UPDATE，适合抢任务、库存、余额这类需要独占的资源，但会带来阻塞、死锁和锁等待。乐观并发控制假设冲突较少，先读和计算，提交时用 version、expected revision、唯一约束或条件 UPDATE 检查冲突；失败后重读并重试。冲突少时乐观锁吞吐好，冲突多时悲观锁更稳定。两者可以和不同隔离级别组合使用。
```

## Q009. 行锁、表锁、意向锁分别解决什么问题？

**回答：**

行锁、表锁、意向锁解决的是不同粒度上的并发协调问题。

先说一句容易被忽略的边界：PostgreSQL 有表级锁和行级锁，但“意向锁”这个词更常见于使用层次锁的系统，比如 InnoDB。PostgreSQL 的锁模式不直接以 `intention shared` / `intention exclusive` 命名，不过它在行锁操作时也会拿相应的表级锁，用来协调表结构变更和行级操作。面试里要把通用概念和 PostgreSQL 实现分开讲。

**行锁：保护具体行。**

行锁解决的是“同一行能不能被多个事务同时改”的问题。

例如两个 worker 同时抢同一个任务：

```sql
BEGIN;

SELECT *
FROM tasks
WHERE id = 1
FOR UPDATE;

UPDATE tasks
SET status = 'running'
WHERE id = 1;

COMMIT;
```

第一个事务锁住 `tasks(id=1)` 后，另一个事务如果也想更新或 `FOR UPDATE` 这行，就要等待或失败。行锁粒度小，允许不同事务并发修改不同的行。

适合：

- 按主键更新。
- 抢占单个任务。
- 扣减某个账户。
- 修改某个订单。

风险：

- 热点行会串行化。
- 多行加锁顺序不一致会死锁。
- 长事务持有行锁会拖垮并发。

**表锁：保护整张表级别的操作。**

表锁解决的是“这个操作是否能和整张表上的其他操作并发”的问题。DDL、批量维护、显式 `LOCK TABLE`、某些写操作都会涉及表级锁。

PostgreSQL 有多种表级锁模式，比如：

- `ACCESS SHARE`
- `ROW SHARE`
- `ROW EXCLUSIVE`
- `SHARE`
- `SHARE ROW EXCLUSIVE`
- `EXCLUSIVE`
- `ACCESS EXCLUSIVE`

普通 `SELECT` 会拿 `ACCESS SHARE`。`INSERT`、`UPDATE`、`DELETE` 会拿 `ROW EXCLUSIVE`。`SELECT FOR UPDATE` 会拿 `ROW SHARE` 表锁，同时对返回的行拿行锁。`DROP TABLE`、`TRUNCATE`、很多 `ALTER TABLE` 会拿非常强的 `ACCESS EXCLUSIVE`，会阻塞普通查询。

表锁适合：

- 防止 DDL 和 DML 互相破坏。
- 做全表维护。
- 显式阻止其他事务读写。
- 保护没有行粒度表达的资源。

表锁的代价比行锁大。生产迁移里，最怕不小心执行一个长时间持有 `ACCESS EXCLUSIVE` 的 DDL，把线上读写都堵住。

**意向锁：解决层次锁兼容性判断。**

意向锁的核心思想是：如果一个事务准备在表里的某些行上加锁，它先在表级别声明“我下面要加行锁”。这样另一个事务想拿整张表的强锁时，不需要扫描所有行锁，只要看表级意向锁是否兼容。

通用例子：

```text
T1:
  wants exclusive lock on row r1
  first acquires intention exclusive lock on table
  then acquires exclusive lock on row r1

T2:
  wants exclusive lock on whole table
  sees table has intention exclusive lock
  waits
```

意向锁解决的是层次化锁管理的效率问题：

```text
database -> table -> page -> row
```

如果没有意向锁，一个事务想锁整张表，可能要检查表内每一行有没有行锁。意向锁让冲突判断可以在上层完成。

**PostgreSQL 里的对应关系。**

PostgreSQL 文档不会把表级 `ROW SHARE`、`ROW EXCLUSIVE` 叫作意向锁，但它们有类似协调作用。比如 `SELECT FOR UPDATE` 会拿 `ROW SHARE` 表级锁，这个表锁和某些强表锁冲突，从而防止表在你锁行时被并发执行不兼容的 DDL。

所以回答时可以说：

```text
PostgreSQL 没有像 InnoDB 那样暴露名为 intention lock 的锁模式；
它通过表级锁模式和行级锁组合来协调行操作和表操作。
```

**三者对比。**

```text
行锁:
  锁具体行，粒度小，保护行级更新冲突

表锁:
  锁整张表或表级操作，粒度大，保护 DDL/批量操作/全表一致性

意向锁:
  表级声明下层将要有行锁，帮助层次锁系统快速判断表级锁是否兼容
```

**常见误区。**

第一，以为行锁不会影响表锁。实际上行锁通常伴随某种表级锁，用来防止不兼容 DDL。

第二，以为表锁一定是坏的。短时间表锁很正常，问题是长事务、慢 DDL、批量操作持有强锁。

第三，把 PostgreSQL 和 MySQL/InnoDB 的术语混用。PostgreSQL 的锁模式、MVCC、gap lock 行为和 InnoDB 不一样，不能照搬。

面试里可以这样回答：

```text
行锁保护具体行，解决同一行并发更新、抢任务、扣余额这类冲突；表锁保护表级操作，协调普通读写、DDL、TRUNCATE、显式 LOCK TABLE 等操作；意向锁是层次锁里的概念，用表级标记说明事务准备在下层行上加锁，避免表锁申请者扫描所有行锁。PostgreSQL 有表级锁和行级锁，但不直接暴露 InnoDB 那种 IS/IX 命名；它通过 ROW SHARE、ROW EXCLUSIVE 等表级锁配合行锁，协调行级 DML 和表级 DDL。
```

## Q010. SELECT FOR UPDATE 的作用是什么？

**回答：**

`SELECT FOR UPDATE` 的作用，是在读取行的同时锁住这些行，防止其他事务并发修改或锁定它们。它常用于“先读再改”的业务流程，避免你读到一行后，准备更新时发现别人已经抢先改了。

PostgreSQL 官方 `SELECT` 文档把 `FOR UPDATE`、`FOR NO KEY UPDATE`、`FOR SHARE`、`FOR KEY SHARE` 都称为 locking clauses，它们会影响 `SELECT` 获取行时如何锁定这些行。

**最典型场景：抢任务。**

没有锁的写法：

```sql
BEGIN;

SELECT id
FROM tasks
WHERE status = 'queued'
ORDER BY created_at
LIMIT 1;

UPDATE tasks
SET status = 'running'
WHERE id = 42;

COMMIT;
```

两个 worker 可能同时读到同一个 queued task，然后都尝试更新。即使最后只有一个更新成功，也会产生复杂的竞态。

用 `FOR UPDATE`：

```sql
BEGIN;

SELECT id
FROM tasks
WHERE status = 'queued'
ORDER BY created_at
LIMIT 1
FOR UPDATE;

UPDATE tasks
SET status = 'running'
WHERE id = 42;

COMMIT;
```

第一个事务锁住返回的行。其他事务再想更新、删除或 `FOR UPDATE` 同一行，会等待。

**队列场景常配 `SKIP LOCKED`。**

如果多个 worker 都在抢任务，等待同一行不一定合适。可以跳过已锁行：

```sql
BEGIN;

SELECT id
FROM tasks
WHERE status = 'queued'
ORDER BY created_at
LIMIT 1
FOR UPDATE SKIP LOCKED;

COMMIT;
```

PostgreSQL 文档提醒，`SKIP LOCKED` 会提供不一致视图，不适合普通查询；但它很适合多个消费者访问队列表，避免锁等待。

**不想等待可以用 `NOWAIT`。**

```sql
SELECT *
FROM accounts
WHERE id = 1
FOR UPDATE NOWAIT;
```

如果行已经被锁，语句直接报错，不等待。适合低延迟 API，拿不到锁就返回“资源忙”或重试。

**`FOR UPDATE` 在 Read Committed 下的细节。**

PostgreSQL 官方隔离文档说，在 `Read Committed` 下，`UPDATE`、`DELETE`、`SELECT FOR UPDATE`、`SELECT FOR SHARE` 找目标行时，只找命令开始时已经提交的行。但如果目标行被另一个并发事务更新或锁定，它会等待那个事务提交或回滚。

如果对方提交了更新，PostgreSQL 会基于更新后的行重新检查 `WHERE` 条件。如果仍然匹配，就锁住并返回更新后的行。

这点很重要：`Read Committed` 下 `SELECT FOR UPDATE` 不是简单锁住你最初看到的物理版本，而是会处理并发更新后的可见版本。

**`FOR UPDATE` 在 Repeatable Read/Serializable 下的细节。**

在更高隔离级别里，如果目标行在事务开始后被其他事务更新并提交，再尝试 `FOR UPDATE` 可能失败：

```text
ERROR: could not serialize access due to concurrent update
```

原因是当前事务的 snapshot 不能修改或锁定一个 snapshot 之后被别人改变的行。应用要重试整个事务。

**它锁的不是整张表的所有行。**

`FOR UPDATE` 锁的是查询返回的行。PostgreSQL 文档也说明，如果指定了 `OF table_alias`，只锁对应表的行；如果 join 查询没有指定，则可能锁所有参与返回行的表。聚合结果这类不能明确对应到单个表行的场景，不能使用 locking clause。

例子：

```sql
SELECT t.*
FROM tasks t
JOIN workflows w ON w.id = t.workflow_id
WHERE w.status = 'running'
FOR UPDATE OF t;
```

这里只锁 `tasks` 的行，不锁 `workflows` 的行。

**它不是分布式锁。**

`SELECT FOR UPDATE` 是数据库事务内的行锁。锁的生命周期通常到事务结束：

```text
BEGIN -> SELECT FOR UPDATE -> UPDATE -> COMMIT/ROLLBACK
```

如果应用拿到行锁后去调用远程服务、执行耗时任务、等待用户输入，就会长时间占住数据库锁。这是典型反模式。正确做法是：在事务里短时间标记状态和写入租约，提交后再执行耗时工作。

比如任务系统更合理的模式是：

```sql
BEGIN;

SELECT id
FROM tasks
WHERE status = 'queued'
ORDER BY created_at
LIMIT 1
FOR UPDATE SKIP LOCKED;

UPDATE tasks
SET status = 'running',
    lease_owner = 'worker-1',
    lease_expires_at = now() + interval '30 seconds'
WHERE id = $1;

COMMIT;

-- transaction ended, now run task outside DB lock
```

**常见误区。**

第一，以为 `SELECT FOR UPDATE` 能防所有并发异常。它只锁返回的行，不能自动锁住“没有返回但未来可能插入”的范围。复杂范围不变量要考虑 Serializable、唯一约束、排他约束或显式锁。

第二，在事务外使用。没有显式事务时，单条语句执行完就提交，锁立即释放，通常达不到“先读再改”的目的。

第三，锁太多行。`SELECT ... FOR UPDATE` 没有限制条件或没合适索引，可能锁住大量行，造成严重阻塞。

第四，和 `ORDER BY`、`LIMIT`、`SKIP LOCKED` 混用时没有稳定排序。任务队列要有明确索引和稳定排序，否则公平性和可预测性都会差。

面试里可以这样回答：

```text
SELECT FOR UPDATE 是在 SELECT 返回行时对这些行加更新锁，防止其他事务并发更新、删除或锁定它们，常用于先读再改、抢任务、扣余额、状态流转。它通常要放在显式事务里，锁到 COMMIT 或 ROLLBACK 才释放。队列场景常配 SKIP LOCKED 避免多个 worker 等同一行，低延迟场景可用 NOWAIT。它只锁已返回的行，不是分布式锁，也不能自动保护没有读到的范围不变量；事务要短，不能拿着行锁做远程调用或长时间计算。
```

## Q011. 唯一索引如何帮助实现幂等？

唯一索引能把“同一个业务动作只能成功一次”从应用层约定变成数据库层不变量。幂等最怕的不是普通串行请求，而是重试、超时、客户端断线、消息重复投递、多个 worker 同时消费同一条任务时出现的并发插入。应用层先查再写：

```sql
SELECT id FROM payments WHERE request_id = $1;
-- if not exists:
INSERT INTO payments(...);
```

在并发下有明显竞态。两个事务都可能先查到不存在，然后同时插入。唯一索引的价值在于：即使所有应用实例同时犯同一个判断，数据库仍然只允许一个候选行落库，其他候选行会阻塞、失败，或进入 `ON CONFLICT` 分支。PostgreSQL 官方文档对唯一索引的定义很直接：唯一索引保证表中不存在两个索引键相等的行；唯一约束和主键也会自动创建唯一 B-tree 索引。

常见设计是为每个幂等操作保存一个幂等键：

```sql
CREATE TABLE idempotency_keys (
    tenant_id        bigint NOT NULL,
    operation        text NOT NULL,
    idempotency_key  text NOT NULL,
    payload_hash     bytea NOT NULL,
    status           text NOT NULL,
    response_body    jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, operation, idempotency_key)
);
```

然后用一次原子写入抢占这个键：

```sql
INSERT INTO idempotency_keys (
    tenant_id,
    operation,
    idempotency_key,
    payload_hash,
    status,
    expires_at
) VALUES (
    $1,
    $2,
    $3,
    digest($4, 'sha256'),
    'processing',
    now() + interval '24 hours'
)
ON CONFLICT (tenant_id, operation, idempotency_key)
DO NOTHING
RETURNING *;
```

如果 `RETURNING` 返回一行，说明当前请求拿到了执行权。业务处理完成后再把结果写回：

```sql
UPDATE idempotency_keys
SET status = 'succeeded',
    response_body = $response
WHERE tenant_id = $tenant_id
  AND operation = $operation
  AND idempotency_key = $key;
```

如果 `RETURNING` 没有返回行，说明同一个幂等键已经存在。此时不能简单地把它当成成功，应该继续检查几个字段：

第一，`payload_hash` 是否一致。相同幂等键配不同请求体，通常应该返回 409 或等价错误。否则客户端误复用 key 时，系统会把 A 请求的结果返回给 B 请求。

第二，`status` 是 `processing`、`succeeded` 还是 `failed`。如果前一个请求仍在处理中，可以返回“处理中”、阻塞等待、短轮询，或者让客户端稍后重试。如果已经成功，可以返回缓存响应。如果失败，要看失败类型是可重试、不可重试，还是状态未知。

第三，幂等键作用域是否足够小。幂等键通常要按 `tenant_id`、`operation`、业务资源类型分区。只用一个裸字符串 `idempotency_key` 做全局唯一，很容易让不同租户或不同接口互相冲突。

唯一索引也可以直接建在业务表上。例如订单系统可以对 `(tenant_id, client_order_id)` 建唯一约束：

```sql
CREATE UNIQUE INDEX orders_client_order_uniq
ON orders (tenant_id, client_order_id);
```

这样客户端重复提交同一个 `client_order_id` 时，业务表本身就是幂等边界。这个方案简单，但有一个限制：如果业务处理跨多张表或需要返回完整历史响应，单靠业务表唯一索引不一定够，仍然需要幂等记录表保存请求指纹和最终响应。

几个容易被忽略的边界：

**`NULL` 语义。**

PostgreSQL 普通唯一约束允许多个 `NULL`，因为 `NULL` 默认不被视为彼此相等。如果幂等键字段允许空值，唯一索引可能完全失效。幂等键列应当 `NOT NULL`。如果业务确实需要把空值也视为相同值，可以研究 `NULLS NOT DISTINCT`，但幂等键一般不应该走到这一步。

**唯一键不是分布式锁。**

唯一索引可以决定“谁先写成功”，但它不自动管理长时间任务的租约、超时恢复、重复执行副作用。拿到幂等键后，如果应用崩溃，记录可能停在 `processing`。这时需要 `lease_expires_at`、后台恢复任务，或者把真正的业务状态设计成可重放、可补偿。

**不要先查再插。**

在高并发下，`SELECT` 预检查只能改善提示，不能提供正确性。正确性要依赖唯一约束、事务和 `INSERT ... ON CONFLICT`。

**保留周期要和重试窗口匹配。**

幂等键不能无限增长，但删除太早会让延迟重试重新执行副作用。实践里会把保留时间设为大于客户端、消息队列、支付网关等外部系统的最大重试窗口。

面试里可以这样回答：

```text
唯一索引帮助幂等的核心，是把 idempotency key 或业务唯一键变成数据库强制的不变量。应用不要靠先 SELECT 再 INSERT 判断是否重复，而应让 INSERT 触发唯一约束，用 ON CONFLICT DO NOTHING/DO UPDATE 或捕获 unique violation 来区分首次请求和重复请求。工程上还要保存 payload hash、执行状态、响应结果和过期时间，避免同一个 key 搭配不同请求体、处理中崩溃、幂等记录过早删除等问题。唯一索引保证的是“同一作用域同一键最多一条记录”，不等于完整的副作用恢复协议。
```

## Q012. upsert 的并发语义需要注意什么？

在 PostgreSQL 里，upsert 通常指：

```sql
INSERT INTO table_name (...)
VALUES (...)
ON CONFLICT (unique_key)
DO UPDATE SET ...
RETURNING ...;
```

它的关键语义不是“先插入，失败后应用层再更新”，而是数据库在一个语句里基于唯一索引或唯一约束选择插入路径或冲突处理路径。PostgreSQL 官方 `INSERT` 文档说明，`ON CONFLICT DO UPDATE` 在高并发下会保证原子性的 `INSERT` 或 `UPDATE` 结果。也就是说，对于每个候选行，只要没有发生独立错误，语句会走插入或更新中的一个分支。

这听起来简单，但并发语义有几个很重要的细节。

**第一，冲突判断依赖唯一索引或排他约束。**

`ON CONFLICT` 不是任意条件上的通用 merge。它必须能找到冲突仲裁器，比如：

```sql
CREATE UNIQUE INDEX users_email_uniq ON users (tenant_id, email);
```

然后：

```sql
INSERT INTO users (tenant_id, email, name)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, email)
DO UPDATE SET name = EXCLUDED.name;
```

如果没有真实唯一约束，两个并发请求仍然可能插入重复业务行。不要把 upsert 当成应用层条件判断的替代品，它依赖数据库索引提供冲突边界。

**第二，冲突行可能导致等待。**

PostgreSQL 文档明确提到，向有唯一索引的表插入数据时，如果另一个会话正在锁定或修改匹配的索引值，插入可能阻塞。也就是说，upsert 在冲突热点上不是无锁的。大量请求同时写同一个 key，会排队等待同一行或同一个唯一索引项。

这对延迟很关键。一个热门幂等键、热门用户计数器、热门配置行，都会把 upsert 变成串行瓶颈。吞吐不一定由 CPU 决定，更多时候由行锁等待、索引页竞争、WAL flush 和事务持有时间决定。

**第三，`DO UPDATE` 更新的是冲突后的当前行，不是你语句开始时看到的旧行。**

在 `Read Committed` 下，如果候选行和另一个事务冲突，当前语句可能等待对方提交，然后对提交后的行执行 `DO UPDATE`。这可以保证单语句的原子性，但也意味着“最后写入者覆盖前者”的风险真实存在。

例如：

```sql
INSERT INTO profiles (user_id, display_name, version)
VALUES ($1, $new_name, 1)
ON CONFLICT (user_id)
DO UPDATE SET
    display_name = EXCLUDED.display_name,
    version = profiles.version + 1;
```

两个请求同时改同一个用户昵称，后提交的请求会覆盖先提交的昵称。数据库没有办法知道这是否符合业务语义。如果你需要乐观并发控制，就要把版本条件写进去：

```sql
INSERT INTO profiles (user_id, display_name, version)
VALUES ($1, $new_name, $expected_version + 1)
ON CONFLICT (user_id)
DO UPDATE SET
    display_name = EXCLUDED.display_name,
    version = profiles.version + 1
WHERE profiles.version = $expected_version
RETURNING user_id, version;
```

如果 `RETURNING` 没有结果，说明版本不匹配，需要让上层重读或返回并发冲突。

**第四，`DO UPDATE ... WHERE` 可能锁了行但没有更新。**

PostgreSQL 文档说明，如果 `ON CONFLICT DO UPDATE` 的 `WHERE` 条件不满足，冲突行虽然会被锁住，但不会被更新，也不会出现在 `RETURNING` 中。这常见于条件 upsert：

```sql
INSERT INTO jobs (id, status, updated_at)
VALUES ($1, 'running', now())
ON CONFLICT (id)
DO UPDATE SET
    status = EXCLUDED.status,
    updated_at = EXCLUDED.updated_at
WHERE jobs.status = 'queued'
RETURNING *;
```

如果行已经是 `done`，语句不会更新它。调用方必须检查 `RETURNING`，不能只看 SQL 没报错。

**第五，同一条语句中的候选行不能重复命中同一目标行。**

PostgreSQL 文档把 `INSERT ... ON CONFLICT DO UPDATE` 称为 deterministic statement，意思是一个现有行不能在同一条语句里被影响多次。如果 `VALUES` 或 `INSERT ... SELECT` 里出现两个相同 key 的候选行，可能触发 cardinality violation。批量 upsert 前应先在应用层或临时表里按冲突键去重。

**第六，`DO UPDATE` 会带来更新副作用。**

很多人用：

```sql
ON CONFLICT (key)
DO UPDATE SET key = EXCLUDED.key
```

只是为了拿到 `RETURNING`。这类 no-op update 看似方便，实际可能触发 update trigger、更新时间戳、生成 WAL、产生死元组、增加 bloat，甚至影响逻辑复制。幂等读缓存场景更适合独立的幂等表，或者先 `DO NOTHING RETURNING`，没拿到行时再 `SELECT` 已存在记录。

**第七，`DO NOTHING` 不等于业务成功。**

`DO NOTHING` 只说明冲突发生后不插入。它不会校验重复请求的 payload 是否一致，不会告诉你历史请求是否已经完成，也不会返回旧响应。支付、扣库存、发通知这类接口，必须在幂等记录中保存请求指纹和结果状态。

面试里可以这样回答：

```text
PostgreSQL 的 upsert 通过唯一索引识别冲突，ON CONFLICT DO UPDATE 能在高并发下保证每个候选行原子地插入或更新。但它不是无锁操作，冲突 key 会等待并串行化；DO UPDATE 更新的是等待后可见的当前行，可能出现最后写入覆盖；DO UPDATE WHERE 可能锁行但不更新，必须看 RETURNING；批量 upsert 还要避免同一语句里多个候选行命中同一目标行。工程上要明确冲突键、版本条件、副作用和返回语义，不能把 upsert 简化成“天然幂等”。
```

## Q013. 索引为什么能提高读性能但降低写性能？

索引提高读性能的原因很直观：它给表增加了一套更适合查找的访问结构。没有索引时，数据库可能需要扫描大量数据页，逐行判断 `WHERE` 条件。使用 B-tree 索引后，数据库可以沿着有序树定位到很小的键范围，再读取匹配行。对排序、范围查询、唯一性检查、join、分页、`MIN/MAX` 等场景，索引都可能显著减少需要访问的数据页。

例如：

```sql
CREATE INDEX orders_customer_created_idx
ON orders (customer_id, created_at DESC);

SELECT *
FROM orders
WHERE customer_id = $1
ORDER BY created_at DESC
LIMIT 20;
```

这个索引让数据库直接定位某个客户的最新订单，而不必扫描全表再排序。

写性能下降，是因为索引不是免费的“目录”，它本身也是持久化数据结构。每次表数据变化，相关索引也要变化。

**INSERT 要写表，也要写每个索引。**

如果一张表有 8 个索引，插入一行时不只是追加一条 heap tuple，还要往 8 个索引里插入索引项。索引页可能在不同位置，带来随机 I/O、缓冲池污染、页分裂和更多 WAL。

**UPDATE 可能变成索引删除加插入。**

如果更新了索引列，旧索引项不能继续代表新值，数据库需要写入新的索引项，并让旧版本后续由 VACUUM 清理。即使只更新非索引列，索引过多也可能降低 HOT update 的机会。PostgreSQL 的 HOT update 依赖更新不影响索引列，并且页面上有空间保存新版本；索引越多，能走轻量更新的概率越低。

**DELETE 不能立即让索引干净。**

MVCC 下删除行通常只是产生一个对后续事务不可见的旧版本。索引项也需要等 VACUUM 等机制逐步清理。删除密集表如果 autovacuum 跟不上，索引会膨胀，读写都会变慢。

**唯一索引还要做并发冲突检查。**

普通索引只要插入索引项；唯一索引还要确认同一键是否已有可见或可能可见的冲突行。并发插入同一个唯一键时，事务之间可能等待。

**索引增加 WAL、锁竞争和缓存压力。**

索引页修改需要写 WAL 以保证崩溃恢复。热点索引页会带来 latch 竞争。索引本身占磁盘和内存，可能把更有价值的数据页挤出缓存。索引越多，优化器评估候选路径的成本也越高，统计信息维护也更复杂。

可以把读写成本拆开看：

```text
读路径：
没有索引 -> 扫描大量表页 -> 过滤 -> 排序/聚合
有索引   -> 定位少量索引页 -> 访问少量表页 -> 更少排序

写路径：
没有索引 -> 写表数据 + WAL
有索引   -> 写表数据 + 写 N 个索引 + 更多 WAL + 可能页分裂/唯一性检查/清理成本
```

索引设计的核心不是“越多越好”，而是把读收益大于写成本的索引留下。高频 OLTP 表尤其要克制。一个只服务偶发后台查询的宽索引，可能会拖慢所有线上写请求。

面试里可以这样回答：

```text
索引提高读性能，是因为它用有序结构或哈希结构减少扫描范围，帮助数据库快速定位、排序、做范围查询或 join。它降低写性能，是因为每次 INSERT、UPDATE、DELETE 都要维护相关索引，产生更多随机 I/O、WAL、页分裂、唯一性检查、锁等待和后续 VACUUM 成本。索引还占缓存和磁盘，所以生产里要根据查询频率、选择性、写入压力和维护成本取舍，而不是给所有字段都建索引。
```

## Q014. B+Tree 索引适合什么查询？

PostgreSQL 官方文档使用的术语是 B-tree index。面试里常说 B+Tree，通常是在讲有序树索引的访问特性。不要在术语上纠缠太久，重点是它维护了按 key 排序的结构，因此非常适合“等值、范围、有序”三类访问。

**等值查询。**

```sql
CREATE INDEX users_email_idx ON users (email);

SELECT *
FROM users
WHERE email = 'a@example.com';
```

B-tree 可以通过比较运算快速定位 key。PostgreSQL 文档列出的 B-tree 可用操作符包括 `<`、`<=`、`=`、`>=`、`>`，也包括等价于这些操作组合的 `BETWEEN`、`IN`、`IS NULL`、`IS NOT NULL` 等条件。

**范围查询。**

```sql
CREATE INDEX orders_created_idx ON orders (created_at);

SELECT *
FROM orders
WHERE created_at >= now() - interval '7 days'
  AND created_at < now()
ORDER BY created_at;
```

B-tree 的数据有序，可以定位范围起点，然后顺序扫描到范围终点。这比全表扫描后过滤要高效得多，尤其是范围选择性较好时。

**排序和 Top N。**

```sql
CREATE INDEX orders_customer_created_idx
ON orders (customer_id, created_at DESC);

SELECT id, created_at, amount
FROM orders
WHERE customer_id = $1
ORDER BY created_at DESC
LIMIT 20;
```

如果索引顺序和 `ORDER BY` 匹配，数据库可以避免额外排序，直接按索引顺序读取前 N 条。这对 feed、订单列表、审计日志、任务队列都很常见。

**前缀匹配。**

PostgreSQL 文档提到，在特定条件下，B-tree 也可以支持锚定在字符串开头的模式匹配，比如 `col LIKE 'abc%'`。因为 `'abc%'` 可以转化为一个有序范围。相反，`LIKE '%abc'` 或 `LIKE '%abc%'` 没有固定起点，普通 B-tree 通常帮不上忙，可能需要 trigram、全文检索或其他索引。

**联合索引中的左前缀查询。**

```sql
CREATE INDEX events_tenant_type_created_idx
ON events (tenant_id, event_type, created_at);
```

这个索引适合：

```sql
WHERE tenant_id = $1
WHERE tenant_id = $1 AND event_type = $2
WHERE tenant_id = $1 AND event_type = $2 AND created_at >= $3
```

它不太适合只按 `event_type` 查询，因为 `tenant_id` 是最左列。PostgreSQL 有 skip scan 等优化，但不能把它当成主要设计依据。

**唯一性和外键关联。**

唯一索引在 PostgreSQL 中只能用 B-tree。主键、唯一约束、常见外键关联列都离不开 B-tree。即使查询只是等值，B-tree 也经常是默认选择，因为它既能处理等值，又能处理范围和排序。

B-tree 不适合的场景也要讲清楚：

第一，低选择性字段单独建索引收益有限。比如 `gender`、`is_deleted` 这种只有少量取值的列，如果查询会命中表中很大比例的数据，走索引再回表可能比顺序扫描更慢。更好的办法可能是组合索引、部分索引或直接顺序扫描。

第二，非左锚定模糊匹配不适合普通 B-tree。`LIKE '%keyword%'`、复杂分词搜索应考虑 PostgreSQL 的全文索引或 trigram。

第三，JSON 包含、数组包含、全文检索、地理空间、相似度搜索、极大时间序列表扫描等，通常要考虑 GIN、GiST、SP-GiST、BRIN 等其他索引类型。

第四，返回大比例数据时，索引不一定更快。数据库优化器会估算成本，如果预计要读很多 heap page，顺序扫描可能更便宜。

面试里可以这样回答：

```text
B-tree/B+Tree 索引适合等值查询、范围查询、按索引顺序排序、Top N、前缀匹配和联合索引左前缀访问。它的优势来自 key 有序，可以快速定位起点并顺序扫描范围。它不适合没有固定起点的模糊匹配、低选择性大范围返回、全文搜索、JSON 包含、地理空间和近似相似度这类访问。PostgreSQL 里 B-tree 是默认索引类型，也是唯一索引的基础。
```

## Q015. Hash 索引和 B+Tree 索引的差异是什么？

Hash 索引和 B-tree 索引的根本差异在访问模型。

Hash 索引把 key 通过哈希函数映射到哈希码，再根据哈希码定位桶。它适合回答一个问题：

```text
这个 key 是否等于某个值？
```

PostgreSQL 官方文档也把 Hash index 的能力限定得很窄：它只处理等值比较，也就是 `=` 操作符。

B-tree 索引维护 key 的有序关系。它能回答更多问题：

```text
key 是否等于某个值？
key 是否落在某个范围？
按 key 从小到大或从大到小取前 N 个？
某个联合索引的左前缀是否匹配？
```

对比可以这样看：

| 维度 | Hash 索引 | B-tree/B+Tree 索引 |
| --- | --- | --- |
| 数据组织 | 按哈希码分桶 | 按 key 有序排列 |
| 适合条件 | `=` | `=`、范围、排序、前缀 |
| 是否支持范围查询 | 不支持 | 支持 |
| 是否支持 `ORDER BY` | 不支持 | 支持，取决于索引顺序 |
| 是否适合唯一约束 | PostgreSQL 唯一索引只支持 B-tree | 支持唯一索引 |
| 默认索引类型 | 不是 | PostgreSQL 默认 `CREATE INDEX` 类型 |
| 典型风险 | 只能服务单一等值场景 | 写入维护成本更高，但通用性强 |

例如：

```sql
CREATE INDEX sessions_token_hash_idx
ON sessions USING hash (token);

SELECT *
FROM sessions
WHERE token = $1;
```

这个查询可以使用 Hash 索引。但下面这些查询不适合 Hash 索引：

```sql
-- 范围查询
SELECT *
FROM sessions
WHERE created_at >= now() - interval '1 day';

-- 排序
SELECT *
FROM sessions
ORDER BY created_at DESC
LIMIT 100;

-- 前缀匹配
SELECT *
FROM users
WHERE email LIKE 'alice%';
```

B-tree 则可以支持这些访问模式，只要索引列和顺序设计得当。

工程上，Hash 索引并不等于“等值查询就一定更快”。很多 OLTP 等值查询仍然使用 B-tree，因为 B-tree 足够快、支持唯一约束、支持范围和排序，还能适应查询演化。Hash 索引只有在查询模式非常稳定、只需要等值，并且经过真实 benchmark 证明收益明显时才值得考虑。

还要注意哈希索引的一个语义边界：哈希值相同不等于原始 key 相同。数据库内部会处理冲突和可见性问题，应用不需要自己处理，但这也说明 Hash 索引天然不能表达排序、范围和前缀。

面试里可以这样回答：

```text
Hash 索引按哈希码定位，只适合等值查询，不能支持范围扫描、排序、前缀匹配，也不能作为 PostgreSQL 的唯一索引基础。B-tree/B+Tree 索引维护 key 的有序关系，可以支持等值、范围、ORDER BY、Top N、联合索引左前缀等访问，是 PostgreSQL 的默认索引类型。实际选型中，B-tree 更通用；Hash 索引只有在纯等值、高频、收益经过验证的场景下才考虑。
```

## Q016. 联合索引的最左前缀原则是什么？

联合索引的最左前缀原则，说的是多列 B-tree 索引的有效访问通常从最左列开始。索引：

```sql
CREATE INDEX orders_tenant_status_created_idx
ON orders (tenant_id, status, created_at);
```

不是三个独立索引：

```text
tenant_id
status
created_at
```

它更像按三元组排序：

```text
(tenant_id, status, created_at)
```

先按 `tenant_id` 排，`tenant_id` 相同再按 `status` 排，前两者相同再按 `created_at` 排。因此，查询如果能从左到右约束索引列，数据库就能快速定位连续范围。

适合这个索引的查询包括：

```sql
-- 使用 tenant_id
SELECT *
FROM orders
WHERE tenant_id = 10;

-- 使用 tenant_id + status
SELECT *
FROM orders
WHERE tenant_id = 10
  AND status = 'paid';

-- 使用 tenant_id + status + created_at 范围
SELECT *
FROM orders
WHERE tenant_id = 10
  AND status = 'paid'
  AND created_at >= now() - interval '7 days';
```

不太适合的查询是：

```sql
SELECT *
FROM orders
WHERE status = 'paid';
```

因为索引第一列是 `tenant_id`。在整个索引里，`status='paid'` 的记录分散在不同 `tenant_id` 下面。数据库没有一个连续小范围可以直接跳进去。

PostgreSQL 官方多列索引文档给出的规则更精确：对 B-tree 多列索引来说，最左侧列的等值约束，加上第一个没有等值约束列上的不等式约束，会限制需要扫描的索引范围。更右侧列的条件可以在索引中被检查，但通常不能减少被扫描的索引区间。

用例子说明：

```sql
CREATE INDEX idx ON events (tenant_id, event_type, created_at);
```

查询一：

```sql
WHERE tenant_id = 1
  AND event_type = 'login'
  AND created_at >= '2026-01-01'
```

这是很理想的组合。`tenant_id = 1` 和 `event_type = 'login'` 是左侧等值条件，`created_at >= ...` 是第一个范围条件，索引可以定位到很窄的时间范围。

查询二：

```sql
WHERE tenant_id = 1
  AND created_at >= '2026-01-01'
```

这个查询能用 `tenant_id` 限定大范围，也能在索引里检查 `created_at`，但因为跳过了中间的 `event_type`，扫描范围往往比查询一大。`created_at` 不再像第三列完整连续地限制范围。

查询三：

```sql
WHERE event_type = 'login'
  AND created_at >= '2026-01-01'
```

缺少最左列 `tenant_id`。普通情况下，这个索引效率不高。PostgreSQL 可能在某些数据分布下使用 skip scan，但不要把 skip scan 当成索引设计的主要依赖。真正高频的查询应建匹配的索引，例如：

```sql
CREATE INDEX events_type_created_idx
ON events (event_type, created_at);
```

联合索引设计要从查询模式倒推，而不是机械地把字段堆起来。

常见排序原则：

第一，等值过滤列通常放前面。例如 `tenant_id`、`user_id`、`status`。

第二，范围列通常放在等值列后面。例如 `created_at >= ...`。

第三，`ORDER BY` 和 `LIMIT` 要一起考虑。例如：

```sql
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT 50
```

适合：

```sql
CREATE INDEX events_tenant_created_desc_idx
ON events (tenant_id, created_at DESC);
```

第四，选择性不是唯一标准。很多人会说“高选择性列放最前”，这句话只在某些查询组合里成立。对于多租户系统，`tenant_id` 可能不是全局最高选择性，但几乎所有查询都带它，把它放最前能让索引天然按租户分区，减少扫描范围，也符合权限边界。

第五，不要用一个超宽联合索引试图覆盖所有查询。列越多，索引越大，写入越慢，缓存命中越差。高频查询可以定制索引，低频后台查询可以接受慢一点。

面试里可以这样回答：

```text
最左前缀原则指多列 B-tree 索引按列顺序组成有序键，查询通常要从最左列开始约束才能高效使用索引。对 (a,b,c) 来说，a、a+b、a+b+c 的查询最自然；a+b+范围 c 也很常见。只查 b 或 c 通常无法有效定位连续范围。PostgreSQL 更精确的规则是，左侧等值条件加上第一个非等值列的范围条件决定扫描区间，更右侧条件可能在索引中被过滤，但不一定减少扫描量。索引列顺序要按真实 WHERE、ORDER BY、LIMIT 和数据分布设计。
```

## Q017. 覆盖索引为什么能减少回表？

“回表”是很多数据库里的通俗说法，指先通过二级索引找到候选记录，再回到主表或 heap 读取完整行。PostgreSQL 的存储结构和 InnoDB 不完全一样，但现象类似：普通索引扫描经常要先读索引页，再读 heap page。PostgreSQL 官方 index-only scan 文档也说明，普通索引扫描需要从索引和 heap 两处取数据，而 heap 的随机访问可能很慢。

覆盖索引的目标，是让查询需要的列都能从索引里拿到，从而减少甚至避免访问 heap。

例如：

```sql
CREATE INDEX orders_customer_created_cover_idx
ON orders (customer_id, created_at DESC)
INCLUDE (id, amount, status);
```

查询：

```sql
SELECT id, amount, status, created_at
FROM orders
WHERE customer_id = $1
ORDER BY created_at DESC
LIMIT 20;
```

这个查询需要的列包括：

```text
过滤和排序列：customer_id, created_at
返回列：id, amount, status, created_at
```

这些列都在索引中。理论上，数据库可以只扫索引，不再为每条记录访问 heap。这就是 index-only scan 的基础。

不过 PostgreSQL 的 index-only scan 有一个额外条件：MVCC 可见性。索引项本身通常不保存足够的事务可见性信息，数据库需要知道某个 heap tuple 对当前事务是否可见。为了避免每条都回 heap 检查，PostgreSQL 使用 visibility map。一个 heap page 如果被标记为 all-visible，说明该页上所有 tuple 对所有事务都可见，索引扫描就可以跳过 heap 访问。

所以覆盖索引减少回表需要同时满足几个条件：

**第一，索引类型支持 index-only scan。**

B-tree 总是支持。其他索引类型要看是否能保存或重构原始值。

**第二，查询引用的列都在索引中。**

包括 `SELECT` 列、`WHERE` 列、`ORDER BY` 列，以及可能的 join 条件列。PostgreSQL 可以用 `INCLUDE` 放入不参与排序的 payload 列：

```sql
CREATE INDEX users_email_cover_idx
ON users (tenant_id, email)
INCLUDE (id, display_name);
```

`tenant_id` 和 `email` 是索引 key，`id`、`display_name` 是附带列。附带列不参与 B-tree 排序，只是为了让查询从索引中取值。

**第三，visibility map 足够干净。**

如果表更新频繁，很多页面不是 all-visible，数据库仍然要回 heap 检查可见性。此时虽然索引覆盖了列，实际 heap fetch 仍可能很多。`VACUUM` 会维护 visibility map，所以 autovacuum 是否及时会直接影响 index-only scan 的效果。

**第四，索引大小不能失控。**

覆盖索引不是把所有列都塞进索引。`INCLUDE` 太多大字段会让索引变宽，增加写入成本、缓存压力和页分裂。它适合覆盖高频、返回列少、延迟敏感的查询，不适合覆盖宽 JSON、大文本、低频报表。

可以用 `EXPLAIN (ANALYZE, BUFFERS)` 验证：

```sql
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, amount, status, created_at
FROM orders
WHERE customer_id = 42
ORDER BY created_at DESC
LIMIT 20;
```

如果计划里出现 `Index Only Scan`，并且 `Heap Fetches` 很低，说明覆盖索引和 visibility map 起到了效果。如果 `Heap Fetches` 很高，说明仍然大量回 heap。

面试里可以这样回答：

```text
覆盖索引能减少回表，是因为查询需要的过滤列、排序列和返回列都在索引里，数据库有机会直接从索引回答查询，避免先读索引再随机访问主表。PostgreSQL 中这对应 index-only scan，但还要满足 MVCC 可见性条件：heap page 需要在 visibility map 中标记为 all-visible，否则仍要回 heap 检查可见性。覆盖索引适合高频、窄返回列、读延迟敏感的查询；如果 INCLUDE 太多列，会增大索引并拖慢写入。
```

## Q018. 索引膨胀 bloat 是什么？

索引膨胀 bloat，指索引文件里包含大量已经无用、低效或难以复用的空间，导致索引比真实有效数据需要的体积大得多。它不是单纯“索引大”，而是“索引大到超过合理需要，并且影响性能和维护成本”。

PostgreSQL 使用 MVCC。`UPDATE` 通常不是原地覆盖旧行，而是产生新版本；`DELETE` 也不会立刻把旧版本物理移除。官方 VACUUM 文档说明，旧版本不能在仍可能被其他事务看到时删除，需要之后由 VACUUM 回收。表有死元组，索引也会留下指向旧版本的索引项或产生页内空洞。高更新、高删除、高冲突 upsert、autovacuum 跟不上时，索引 bloat 就会累积。

可以从几个角度理解 bloat：

**表 bloat。**

heap 页面中有大量 dead tuple 或空闲空间。普通 VACUUM 可以把空间标记为可复用，但通常不会把文件收缩回操作系统。

**索引 bloat。**

索引页中有无效索引项、页分裂留下的不均匀空间、删除后难以紧凑的空洞。索引扫描需要读更多 page，缓存命中下降，维护成本上升。

**可复用空间不等于文件变小。**

普通 VACUUM 主要让空间可被后续写入复用。`VACUUM FULL` 会重写表并把空间还给操作系统，但需要更重的锁。索引严重膨胀时，经常还要考虑 `REINDEX` 或并发重建索引。

常见原因：

第一，频繁更新索引列。比如每次状态变化都更新 `(status, updated_at)` 上的两个索引，旧索引项不断失效，新索引项不断插入。

第二，大量 upsert。`ON CONFLICT DO UPDATE` 如果每次都更新行，即使业务上看起来只是“刷新一下”，也会产生新版本和索引维护成本。

第三，长事务阻碍 VACUUM。只要仍有旧 snapshot 可能看到旧版本，VACUUM 就不能安全清理它们。

第四，autovacuum 配置跟不上写入速率。高写入表如果阈值过高、成本限制太保守，死元组会快速堆积。

第五，索引过多。每个更新都要维护更多索引，也给 VACUUM 和缓存带来更重负担。

第六，fillfactor 不合适。页面没有给更新留下空间时，更容易产生 page split 或无法进行 HOT update。

线上症状通常很具体：

```text
表和索引磁盘占用持续增长，但业务数据量没有对应增长
相同查询读的 shared buffers 变多
索引扫描变慢，p95/p99 上升
VACUUM 越来越慢，autovacuum 长时间运行
备份、恢复、逻辑复制和磁盘告警压力变大
```

排查时可以看：

```sql
SELECT relname, n_live_tup, n_dead_tup, last_vacuum, last_autovacuum
FROM pg_stat_all_tables
WHERE schemaname = 'public'
ORDER BY n_dead_tup DESC;
```

也可以通过扩展或维护工具估算表和索引 bloat。面试中不需要背某个工具名，关键是能说清楚：bloat 是 MVCC、写入模式和清理滞后共同造成的物理存储膨胀。

修复策略要分层：

第一，先解决阻碍清理的原因。比如长事务、空闲事务、复制槽滞后、autovacuum 配置过弱。

第二，降低产生 bloat 的速度。减少不必要索引，避免 no-op update，调整 fillfactor，改写热点更新模式。

第三，对已经严重膨胀的对象做重写或重建。表可以考虑 `VACUUM FULL`、`CLUSTER`、逻辑迁移；索引可以考虑 `REINDEX` 或 `REINDEX CONCURRENTLY`。这些操作有锁和资源成本，要按维护窗口或在线方案规划。

面试里可以这样回答：

```text
索引 bloat 是索引文件中积累了大量无效索引项、空洞和难以复用的页空间，使索引体积超过有效数据需要。PostgreSQL 的 MVCC 更新和删除不会立即物理清理旧版本，VACUUM 跟不上、长事务阻塞、频繁更新索引列、upsert 热点和索引过多都会加剧 bloat。症状是磁盘增长、缓存效率下降、索引扫描读更多页、查询和 VACUUM 变慢。普通 VACUUM 主要让空间可复用；严重膨胀可能需要 REINDEX、VACUUM FULL 或重写表。
```

## Q019. VACUUM 在 PostgreSQL 中解决什么问题？

VACUUM 是 PostgreSQL MVCC 模型下的基础维护机制。它解决的不是一个问题，而是一组和旧版本、统计信息、可见性、事务 ID 相关的问题。

PostgreSQL 官方 routine vacuuming 文档列出 VACUUM 的几个主要原因：

```text
回收或复用被更新、删除行占用的空间
更新查询规划器使用的数据统计信息
更新 visibility map，以加速 index-only scan
防止事务 ID 或 multixact ID wraparound
```

可以逐个理解。

**第一，清理 dead tuples，复用空间。**

在 PostgreSQL 里，`UPDATE` 和 `DELETE` 后的旧行版本不会马上从磁盘消失。因为其他并发事务可能还在用旧 snapshot，仍然需要看到旧版本。等这些旧版本不再可能被任何事务看到，VACUUM 就可以把它们标记为可回收空间。

普通 VACUUM 通常不会把磁盘文件缩小给操作系统，而是让表内部空间可以被未来 INSERT/UPDATE 复用。`VACUUM FULL` 会重写表，把空间还给操作系统，但会拿更重的锁，不能随便在线执行。

**第二，清理索引里的无效项。**

表中的旧版本会对应索引项。VACUUM 会帮助清理索引中不再需要的指针，降低索引扫描和索引维护成本。如果索引已经严重膨胀，VACUUM 未必能把结构压回理想状态，可能需要 `REINDEX`。

**第三，更新 planner statistics。**

`VACUUM ANALYZE` 或 autovacuum 的 analyze 阶段会更新统计信息，比如行数估计、列值分布、相关性等。优化器依赖这些统计信息选择索引扫描、顺序扫描、join 顺序和 join 算法。统计信息过旧时，数据库可能明明有好索引却不用，或者错误地选择代价很高的执行计划。

**第四，维护 visibility map，帮助 index-only scan。**

如果一个 heap page 上所有 tuple 都对所有事务可见，PostgreSQL 可以在 visibility map 中标记 all-visible。Index-only scan 看到这个标记后，就能少回 heap 检查 MVCC 可见性。VACUUM 维护 visibility map，所以及时 VACUUM 会影响覆盖索引的实际效果。

**第五，冻结旧事务 ID，防止 wraparound。**

PostgreSQL 用事务 ID 判断版本可见性。事务 ID 空间有限，如果非常旧的 tuple 不被冻结，系统会面临 wraparound 风险。防 wraparound VACUUM 是数据库安全运行的底线，不是性能优化项。忽视它会让数据库进入保护性停写状态。

**第六，配合 autovacuum 保持系统长期稳定。**

生产系统通常依赖 autovacuum 自动触发清理。手工 `VACUUM` 主要用于大批量导入、删除、迁移、临时维护之后，或者针对某些 autovacuum 跟不上的热点表做补救。

几个边界要讲清楚：

第一，VACUUM 不会修复错误索引设计。索引太多、查询不匹配、低选择性字段滥建索引，VACUUM 无法改变这些问题。

第二，普通 VACUUM 不等于释放磁盘给操作系统。它更多是内部复用。看到表文件没有变小，不代表 VACUUM 没有价值。

第三，VACUUM 本身也消耗 I/O、CPU 和 WAL，配置太激进会影响前台请求，太保守会导致 bloat。需要按写入速率调 autovacuum。

第四，长事务、复制槽滞后、prepared transaction 等会限制 VACUUM 能清掉多少旧版本。VACUUM 运行了不等于清理成功。

面试里可以这样回答：

```text
VACUUM 是 PostgreSQL 为 MVCC 付出的后台维护成本。它清理 UPDATE/DELETE 留下的 dead tuples，让空间可复用，清理无效索引项，更新 visibility map 以支持 index-only scan，并在 VACUUM ANALYZE 时更新优化器统计信息，同时还负责冻结旧事务 ID 防止 wraparound。普通 VACUUM 通常不把磁盘还给操作系统；VACUUM FULL 会重写表并释放空间，但锁更重。VACUUM 是否有效还取决于是否有长事务、复制槽滞后和 autovacuum 配置是否跟得上写入压力。
```

## Q020. 长事务为什么会阻碍 VACUUM？

长事务阻碍 VACUUM 的根本原因是 MVCC 可见性。PostgreSQL 不能删除一个旧行版本，除非能确定没有任何当前或未来还在使用旧 snapshot 的事务需要看到它。

假设发生下面的时间线：

```text
T1: BEGIN;
T1: SELECT * FROM orders WHERE id = 1;  -- 看到旧版本 v1

T2: UPDATE orders SET status = 'paid' WHERE id = 1; -- 产生新版本 v2
T2: COMMIT;

autovacuum: 想清理 v1
```

只要 T1 还没有提交或回滚，T1 的 snapshot 可能仍然需要看到 `v1`。VACUUM 就不能把 `v1` 当成可安全删除的 dead tuple。PostgreSQL 官方文档也强调，旧行版本不能在仍可能被其他事务看到时删除。

数据库内部通常会维护一个类似“全局最老仍活跃事务”的边界，也就是 xmin horizon。VACUUM 只能清理早于安全边界、且不再可能被任何活跃 snapshot 看到的旧版本。长事务把这个边界钉在很老的位置，于是后面大量 UPDATE/DELETE 产生的旧版本都不能被真正清走。

阻碍 VACUUM 的不一定是正在大量写入的事务，也可能是一个看似无害的读事务：

```sql
BEGIN;
SELECT count(*) FROM big_table;
-- 应用忘记 COMMIT，连接 idle in transaction 很久
```

这个连接可能什么都不做，却一直保留旧 snapshot。结果是 autovacuum 一直运行，但只能扫描，不能有效回收。

常见来源包括：

**长时间 `idle in transaction`。**

应用开启事务后等待用户输入、远程调用、消息队列结果，或者连接池没有正确结束事务。

**长时间报表查询。**

特别是 `REPEATABLE READ` 或 `SERIALIZABLE` 下的大查询，会保留较老 snapshot。

**批处理事务过大。**

一次迁移或批量更新几千万行，中间不提交，既制造大量 dead tuple，又阻止其他清理前进。

**复制槽或逻辑订阅滞后。**

逻辑复制槽需要保留 WAL 和某些可见性边界。虽然它不是普通 SQL 事务，但同样可能让清理和磁盘回收受限。

**prepared transaction 长时间不提交。**

两阶段提交中的 prepared transaction 如果遗留，也会持有资源和事务边界。

线上影响通常很快扩散：

第一，dead tuple 越积越多，表和索引 bloat 增长。

第二，查询扫描更多无效版本，缓存命中下降，p99 抖动。

第三，index-only scan 变差。页面无法标记为 all-visible，覆盖索引仍然要回 heap。

第四，autovacuum 反复运行但收益有限，I/O 被消耗，前台请求也受影响。

第五，事务 ID 冻结推进受阻，严重时接近 wraparound 风险。

第六，系统维护操作变慢。备份、恢复、迁移、重建索引都会被更大的物理体积拖慢。

治理方式也要从源头下手：

```sql
SELECT pid,
       usename,
       state,
       xact_start,
       now() - xact_start AS xact_age,
       query
FROM pg_stat_activity
WHERE xact_start IS NOT NULL
ORDER BY xact_start;
```

看到长事务后，先判断它是正常任务、卡住的连接，还是应用 bug。对 `idle in transaction`，通常要设置：

```sql
SET idle_in_transaction_session_timeout = '60s';
```

生产上更常见的是在数据库参数、连接池和应用框架里统一配置超时。

应用设计上应遵守几个原则：

第一，事务只包住必须原子提交的数据库操作。不要在事务里做远程 RPC、文件上传、用户交互、大量 CPU 计算。

第二，大批量迁移要分批提交。比如每次处理几千行或几万行，而不是一个事务处理全表。

第三，读报表尽量走只读副本、离线数仓或可接受滞后的物化视图。不要在主库上长时间持有旧 snapshot。

第四，监控 `pg_stat_activity.xact_start`、`state='idle in transaction'`、`age(datfrozenxid)`、`n_dead_tup`、复制槽滞后和 autovacuum 运行情况。

第五，对确实需要长一致性快照的任务，要明确资源预算和时间窗口，不能让它和高写入 OLTP 混在一起。

面试里可以这样回答：

```text
长事务会阻碍 VACUUM，因为 PostgreSQL 的 MVCC 要保证旧 snapshot 仍能看到它开始时可见的行版本。只要一个老事务还没结束，VACUUM 就不能删除这个事务可能看到的旧 tuple，全局清理边界会被它拖住。结果是 dead tuple 和索引 bloat 累积，visibility map 无法推进，index-only scan 变差，autovacuum 做很多扫描但回收有限，严重时还会增加事务 ID wraparound 风险。治理上要缩短事务、禁止 idle in transaction、批量任务分批提交、监控 xact_start 和复制槽滞后。
```

## Q021. WAL 在 PostgreSQL 中有什么作用？

WAL 是 Write-Ahead Log，中文通常叫预写日志。它的核心规则很短：数据页真正写回磁盘之前，描述这次修改的日志记录必须先写入持久化存储。PostgreSQL 官方 WAL 文档也按这个思路解释：表和索引所在的数据文件只能在相关 WAL 记录已经 flush 到持久介质之后再写入。

这条规则解决的是数据库崩溃恢复和事务持久性问题。

如果没有 WAL，事务提交时数据库就要把所有被修改的数据页都刷到磁盘。一个事务可能改了很多表页、索引页，这些页分散在磁盘不同位置，提交路径会非常慢。WAL 把提交路径改成了更可控的形式：先顺序写日志，事务提交只需要保证关键 WAL 记录落盘；脏数据页可以稍后由后台进程、checkpoint 或缓冲区淘汰时写回。

典型流程是：

```text
修改 buffer 中的数据页
生成 WAL record
提交时 flush commit record 到 WAL
事务对外报告成功
稍后再把 dirty data page 写回数据文件
```

如果数据库在数据页写回之前崩溃，重启时从最近 checkpoint 开始读取 WAL，把已经提交但尚未反映到数据文件的修改 redo 一遍。PostgreSQL 文档把这叫 roll-forward recovery，也就是 REDO。

WAL 的作用可以分成几类。

**第一，保证 committed transaction 的 durability。**

事务一旦报告提交成功，数据库要能在崩溃后恢复它的效果。WAL 提供了恢复依据。只要提交相关的 WAL 记录已经安全落盘，即使 heap page 或 index page 还没写完，重启时也能根据 WAL 重做。

这里要补一个边界：如果配置了异步提交，事务返回成功和 WAL 真正落盘之间可能存在窗口。这个窗口换来了延迟，但牺牲了最近事务在崩溃中的持久性保证。面试时不要把“PostgreSQL 有 WAL”直接等同于“所有配置下提交都绝对不丢”。

**第二，降低提交时的随机 I/O。**

WAL 是顺序写。对于大量小事务，顺序写 WAL 的成本远低于每次提交都把所有数据页同步刷盘。PostgreSQL 文档也提到，多个并发小事务还可能通过一次 WAL fsync 分摊提交成本，这就是 group commit 的基础。

**第三，支持 crash recovery。**

checkpoint 之前的数据页保证已经包含之前的修改。checkpoint 之后的修改，如果没有全部写入数据文件，就靠 WAL 重放。恢复时间取决于 checkpoint 之后还要 replay 多少 WAL。

**第四，支持备份、PITR 和复制。**

WAL 不是只为本机崩溃恢复服务。归档 WAL 加基础备份可以做 point-in-time recovery。物理复制也依赖 WAL 流。逻辑复制虽然语义层更高，但底层仍然和 WAL 解码有关。

**第五，支撑部分数据库内部机制。**

索引修改、事务提交、页面第一次修改后的 full-page write、hint bit、可见性相关变化，都会影响 WAL 量。一次看似普通的 `UPDATE`，可能写 heap、写多个索引、写 WAL、触发 full-page write，最后表现成写放大。

WAL 不是审计日志，也不是业务事件日志。它记录的是数据库恢复所需的低层变更信息，格式和数据库版本、存储结构相关。不要指望直接拿 WAL 当订单事件流或用户操作日志。业务审计要单独建表或事件日志；WAL 更接近数据库自己的恢复账本。

常见误区有几个。

第一，以为 WAL 落盘后数据页也已经落盘。不是。WAL 先落盘，数据页可以晚点写。正是这个顺序让系统既能快提交，又能崩溃恢复。

第二，以为 WAL 只影响写入性能。实际上 WAL 写入、WAL flush、checkpoint、复制槽、归档、恢复都会影响读写延迟。WAL 堆积还可能让磁盘打满。

第三，以为关闭 `fsync` 或降低同步要求只是“优化性能”。这类配置会直接改变崩溃后的数据安全边界，不能在生产里轻率使用。

面试里可以这样回答：

```text
WAL 的作用是把数据库修改先记录到持久化日志，再允许数据页稍后写回。事务提交时通常只需要保证提交相关 WAL 记录落盘，崩溃后 PostgreSQL 从最近 checkpoint 开始 replay WAL，把已提交但尚未写入数据文件的修改重做出来。这样既保证 durability，又避免每次提交都随机刷大量 heap/index page。WAL 还支持 PITR、备份和复制。它不是业务审计日志，而是数据库内部的恢复日志；WAL flush、checkpoint 和归档复制都会影响写入延迟和磁盘压力。
```

## Q022. 数据库 checkpoint 会如何影响 I/O 峰值？

checkpoint 是数据库在事务日志序列中划出的恢复边界。PostgreSQL 官方 WAL 配置文档说，checkpoint 时 heap 和 index 数据文件已经包含该点之前写入 WAL 的所有信息；checkpoint 会把脏数据页刷到磁盘，并写入一条 checkpoint record。之后崩溃恢复就可以从这个 checkpoint 对应的 redo 位置开始，而不用从更早的 WAL 开始重放。

问题在于，checkpoint 要处理脏页。脏页越多，需要写的 heap page 和 index page 越多。写脏页、刷 OS page cache、等待 fsync，都可能把 I/O 打到峰值。

可以把 checkpoint 的影响拆成几段。

**第一，checkpoint 会集中写 dirty buffers。**

数据库平时为了吞吐，会把修改先留在内存 buffer 中。checkpoint 到来时，这些脏页需要逐步写回。PostgreSQL 会做节流，不是一次性全部写出，但节流不代表没有压力。如果前台写入很猛，后台 checkpoint 又在刷脏页，二者会竞争同一块磁盘或云盘 I/O 队列。

**第二，checkpoint 结束附近可能出现 fsync stall。**

脏页写到操作系统 page cache 不等于已经落到持久设备。最后还要让内核把数据真正 flush。某些系统如果之前积累了太多脏页，checkpoint 末尾的同步会造成明显卡顿。PostgreSQL 提供 `checkpoint_flush_after` 这类设置，就是为了减少最后集中 flush 造成的停顿。

**第三，checkpoint 太频繁会增加写放大。**

`checkpoint_timeout` 太短或 `max_wal_size` 太小，会让 checkpoint 频繁发生。PostgreSQL 文档明确提醒，checkpoint 很昂贵：它要写当前脏页，而且在开启 `full_page_writes` 时，每个 checkpoint 后某个数据页第一次被修改，会把整个页面写入 WAL。checkpoint 间隔越短，这种 full-page write 越频繁，WAL 量和 I/O 都会上升。

**第四，checkpoint 太少会增加恢复时间和 WAL 保留。**

把 checkpoint 间隔调得很长，可以降低 checkpoint 频率，但崩溃恢复要 replay 的 WAL 更多，`pg_wal` 需要保留的日志也更多。调参不是“越大越好”，而是在前台延迟、恢复时间、磁盘空间之间取平衡。

**第五，`checkpoint_completion_target` 决定 I/O 是否更平滑。**

PostgreSQL 用 `checkpoint_completion_target` 控制 checkpoint 写脏页的铺开程度。默认值接近 0.9，表示尽量把写入摊到大部分 checkpoint 周期里。这个值过低，会让 checkpoint 很快写完，然后出现一段强 I/O、一段空闲的模式，p99 更容易抖。设置得过高也有风险，checkpoint 可能来不及完成，导致下一轮压力叠加。

**第六，`max_wal_size` 太小会触发 requested checkpoint。**

如果 WAL 增长快到接近 `max_wal_size`，系统会提前 checkpoint。批量导入、大量 `COPY`、批量索引构建、大事务更新，都可能让日志里出现 checkpoint warning。偶尔出现不是灾难，经常出现就说明 WAL/checkpoint 参数或写入模式需要调整。

线上能看到的现象通常是：

```text
checkpoint 期间磁盘写 IOPS 和吞吐升高
transaction commit latency 上升
查询 p99 抖动，尤其是共享同一块存储的读请求
WAL 写入和 fsync 时间升高
日志出现 checkpoints are occurring too frequently
云盘 burst credit 被打空后整体延迟恶化
```

排查时可以看 PostgreSQL 版本提供的 checkpoint 统计视图、WAL I/O 统计、日志中的 checkpoint warning、磁盘层 IOPS/吞吐/await，以及 `pg_wal` 增长速度。老版本常看 `pg_stat_bgwriter`，新版本里 checkpoint 统计拆得更细。面试里不需要背所有视图名，但要知道观察对象是：checkpoint 次数、requested checkpoint 比例、写脏页耗时、sync 耗时、WAL 生成速率和存储队列。

工程做法也很具体：

```text
把 max_wal_size 设置到能吸收正常写入峰值
让 checkpoint_timeout 不要过短
保持 checkpoint_completion_target 接近默认的平滑写入思路
避免大批量写入和在线业务高峰叠加
批处理分批提交，控制 WAL 生成速率
监控 pg_wal、checkpoint warning、commit latency 和磁盘队列
```

面试里可以这样回答：

```text
checkpoint 会把 checkpoint 之前的脏 heap/index page 推进到磁盘，并写入 checkpoint record，让崩溃恢复可以从较新的 redo 点开始。它会造成 I/O 峰值，因为脏页写回、OS cache flush 和 fsync 会和前台读写竞争存储资源。checkpoint 太频繁会增加脏页刷新次数和 full-page write 产生的 WAL；太稀疏会增加恢复时间和 WAL 保留。调优重点是用 max_wal_size、checkpoint_timeout、checkpoint_completion_target 把 I/O 摊平，同时监控 checkpoint warning、WAL 生成速率、sync 时间和前台 p99。
```

## Q023. 连接池为什么重要？

连接池重要，是因为数据库连接不是一个轻量的普通对象。对 PostgreSQL 来说，一个客户端连接通常对应后端进程和一组会话状态。连接建立要经过 TCP、TLS、认证、参数协商、权限检查；连接存活期间还占用内存、文件描述符、后台进程槽位和数据库内部资源。PostgreSQL 官方 `max_connections` 文档也提醒，增加 `max_connections` 会直接增加某些资源分配，包括 shared memory。

如果每个请求都新建数据库连接，系统会把大量时间浪费在连接创建和销毁上。更糟的是，高并发瞬间会把数据库连接数冲满，导致新请求排队、报错，甚至让数据库在上下文切换和内存压力下整体变慢。

连接池解决的不是“让并发无限变大”，而是把数据库并发控制在可承受范围内。

一个典型 Web 服务链路是：

```text
HTTP request -> app worker -> acquire db connection -> SQL -> release connection
```

连接池在这里做几件事。

**第一，复用连接，减少建连成本。**

应用不用每次请求都重新认证和初始化会话。常用连接保持在池里，请求来了直接借，用完归还。

**第二，给数据库加背压。**

如果数据库最多稳定处理 80 个并发查询，连接池就不应该放 800 个请求同时冲进去。池子满了，让请求在应用侧排队、超时或快速失败，比让数据库内部被过量连接拖垮更可控。

**第三，平滑流量突刺。**

突发流量到来时，池子可以让应用端出现有限排队，而不是瞬间创建大量连接。对短查询尤其明显。

**第四，隔离不同 workload。**

线上服务、后台任务、报表、迁移脚本最好不要共享一个无限制连接入口。可以用不同连接池、不同角色、不同 `statement_timeout` 和不同资源配额，避免一个报表把线上请求挤死。

**第五，便于观测和限流。**

连接池能直接暴露 active、idle、waiting、acquire latency、max lifetime、timeout count 等指标。很多数据库问题最早不是出现在 SQL 慢日志里，而是出现在“拿连接越来越慢”。

连接池可以放在不同位置。

应用内连接池最常见。Go 的 `database/sql`、Java HikariCP、Node 的 pg pool 都属于这一类。它们适合控制单个应用进程的数据库并发。

PgBouncer 是 PostgreSQL 常用的外部连接池。它有 session pooling、transaction pooling、statement pooling。PgBouncer 官方文档说，session pooling 会把一个 server connection 分给 client 直到 client 断开；transaction pooling 只在一个事务期间分配 server connection；statement pooling 更激进，禁止多语句事务。transaction pooling 能大幅减少后端连接数，但会破坏一些依赖会话状态的 PostgreSQL 特性，比如 `SET`、临时表、会话级 advisory lock、SQL 级 prepared statement 等。用之前要检查应用是否兼容。

连接池设计要注意几个参数：

```text
max open connections: 最多同时打开多少数据库连接
max idle connections: 空闲连接保留多少
connection lifetime: 连接最长复用多久
idle timeout: 空闲多久关闭
acquire timeout: 等连接最多等多久
per-instance pool size: 每个应用实例的池大小
global connection budget: 所有实例加起来不能超过数据库预算
```

一个常见事故是：每个应用实例池大小 50，看起来不大；Kubernetes 扩到 40 个 pod 后，全局上限变成 2000。PostgreSQL 默认 `max_connections` 常见是 100 左右，真实可承受的活跃查询数还要看 CPU、I/O、锁和 SQL 复杂度。池大小一定要按全局算。

面试里可以这样回答：

```text
连接池的重要性在于复用昂贵的数据库连接，并把并发限制在数据库能承受的范围内。PostgreSQL 每个连接会占用后端进程、内存和内部资源，max_connections 增大还会增加资源分配。没有连接池时，请求会频繁建连，突发流量还可能把数据库连接打满。好的连接池既降低建连开销，也提供背压、排队、超时和观测能力。池大小要按所有应用实例的全局连接预算设计，不能只看单个进程。
```

## Q024. 连接池过大为什么可能降低性能？

连接池过大最容易让人误判。表面上看，连接数变多了，系统“并发能力”应该更强。实际经常相反：数据库能稳定执行的活跃查询数是有限的，连接池过大只是允许更多工作同时冲进数据库，最后变成 CPU 抢占、锁等待、内存膨胀、I/O 排队和 p99 飙升。

数据库不是 HTTP 网关。HTTP 层多开一些连接，很多时候只是多一些等待 socket；数据库连接背后是查询执行、锁、buffer、排序、hash、WAL、临时文件、事务状态。每个活跃连接都可能消耗真实资源。

主要问题有几个。

**第一，连接数大于 CPU 可并行能力。**

一台 16 核数据库不可能高效同时跑 1000 个 CPU 密集查询。超过一定点后，更多连接只是增加上下文切换、调度延迟和缓存抖动。单个查询变慢，事务持锁时间变长，又进一步放大锁等待。

**第二，内存不是按连接池愿望无限增长。**

PostgreSQL 的某些内存配置按会话或执行节点使用，比如 `work_mem` 是每个排序、hash 等操作可能使用的上限，不是全库总上限。一个复杂查询可能同时有多个 sort/hash 节点，多个连接同时跑时总内存会远超直觉。内存压力之后，系统开始 swap 或大量 spill 到临时文件，性能会断崖式下降。

**第三，锁竞争会被放大。**

热点行、唯一索引、队列表、库存表、计数器表，本来就有串行化点。连接池过大后，更多事务同时排在同一把锁后面。它们不是并行完成，而是一起占着连接、事务和内存等待。等待队列越长，超时和重试越多，形成二次放大。

**第四，I/O 队列被打爆。**

过多活跃查询会同时读 heap、读索引、写 WAL、刷临时文件、触发 checkpoint。磁盘或云盘到达吞吐/IOPS/队列深度上限后，每个 I/O 的等待时间上升。数据库延迟变差，应用侧又因为请求堆积继续占连接。

**第五，连接池太大削弱了背压。**

连接池本来应该挡住超出数据库能力的并发。池子过大，相当于把排队从应用侧搬到数据库内部。应用侧排队可以设置超时、拒绝、降级；数据库内部排队更难区分优先级，还可能拖慢所有业务。

**第六，多实例部署会把问题乘起来。**

单实例池大小 30，也许没问题。自动扩容到 100 个实例，就是 3000 个潜在连接。很多事故不是某个池子设置离谱，而是实例数、sidecar、worker、定时任务和迁移脚本一起把全局连接预算打穿。

可以用一个简单模型判断：

```text
数据库稳定活跃查询能力 = min(CPU 并行能力, I/O 能力, 锁热点容量, WAL flush 能力)
连接池上限应该接近这个能力，并保留管理连接和后台任务余量
```

这不是说连接池越小越好。太小会让数据库空闲、应用排队过长。正确做法是压测找到拐点：随着连接池增大，吞吐一开始上升，随后趋平，再往后延迟快速恶化。池大小应放在吞吐接近平台期、p95/p99 仍可控的位置。

需要监控的指标包括：

```text
pool active / idle / waiting
connection acquire latency
PostgreSQL active sessions
CPU run queue
lock wait
WAL fsync time
temp file size
IOPS/throughput/await
statement timeout 和 deadlock 数
```

工程上更稳的做法：

第一，按全局连接预算分配。数据库可用 200 个连接，不代表每个服务实例都能开 200。

第二，把在线请求和后台任务分池。后台任务可以慢，但不能挤占在线请求。

第三，设置 acquire timeout 和 statement timeout。拿不到连接或 SQL 超时，要快速释放压力。

第四，在 PgBouncer 或数据库侧保留 emergency slots，避免管理连接也进不去。

第五，扩容应用实例时同步调整每实例 pool size。否则自动扩容会悄悄放大数据库并发。

面试里可以这样回答：

```text
连接池过大会降低性能，因为它让超过数据库承载能力的查询同时进入执行层。更多连接会带来上下文切换、内存放大、锁等待、WAL 和 I/O 队列拥塞，事务变慢后又延长持锁时间，p99 会明显变差。连接池的价值是背压，不是把所有请求都塞进数据库。池大小要按全局连接预算、CPU/I/O 能力、SQL 类型和实例数压测出来，通常看吞吐平台期和延迟拐点，而不是简单设得越大越好。
```

## Q025. 事务中执行远程 RPC 有什么风险？

在数据库事务里执行远程 RPC，是线上系统里很常见也很危险的反模式。它把数据库本地事务的锁、连接、MVCC snapshot 和一个不可控的网络调用绑在一起。远程调用一慢，数据库事务就跟着慢；远程调用结果不确定，数据库状态也跟着难恢复。

典型代码是：

```text
BEGIN;
UPDATE orders SET status = 'paying' WHERE id = ?;
call payment service;
UPDATE orders SET status = 'paid' WHERE id = ?;
COMMIT;
```

看起来顺序清晰，实际有很多边界。

**第一，事务持锁时间被 RPC p99 放大。**

`UPDATE orders` 之后，行锁会一直持有到 `COMMIT` 或 `ROLLBACK`。如果支付服务 p99 是 2 秒，偶发超时是 30 秒，这把行锁就可能持有 30 秒。其他修改同一订单的事务会等待。等待连接堆积后，连接池也会被耗尽。

**第二，远程调用没有数据库事务语义。**

数据库可以回滚 `UPDATE`，但不能回滚已经发出去的 HTTP 请求。支付、发短信、发邮件、扣外部库存、调用第三方 API，一旦对方执行成功，本地事务回滚也不能自动撤销。

**第三，超时结果是未知状态。**

RPC 超时不等于对方没执行。可能是请求没到；可能是对方执行成功但响应丢了；可能是对方执行了一半；可能是对方稍后异步完成。此时本地事务该提交还是回滚？如果没有幂等键和状态查询接口，系统只能猜。

**第四，事务失败会制造重复副作用。**

如果远程调用成功，但本地 `COMMIT` 失败，重试请求可能再次调用远程服务。没有 idempotency key 时，支付可能重复扣款，消息可能重复发送。

**第五，长事务会阻碍 VACUUM。**

事务里等待 RPC，哪怕只是读事务，也可能持有旧 snapshot。写事务还会持有行锁。时间长了，dead tuple 不能清理，bloat 和 autovacuum 压力都会上升。

**第六，容易造成跨系统死锁。**

服务 A 开数据库事务后调用服务 B，服务 B 又回调 A 或访问同一数据库资源。两边都以为自己在等待普通 RPC，实际形成了跨系统等待环。数据库死锁检测只能看到本库锁，不一定能识别整个分布式等待。

更稳的模式是把本地事务和外部副作用拆开。

常见做法一：outbox。

```text
BEGIN;
UPDATE orders SET status = 'pay_pending' WHERE id = ?;
INSERT INTO outbox(event_type, payload, idempotency_key) VALUES (...);
COMMIT;

后台 worker 读取 outbox -> 调用支付服务 -> 写入结果事件 -> 更新订单状态
```

这样本地数据库只提交本地状态和待发送事件。RPC 在事务外执行，失败可以重试，重试靠幂等键约束。

常见做法二：saga。

```text
创建订单 -> 预留库存 -> 发起支付 -> 确认订单
失败时按语义补偿：释放库存、取消订单、退款或冲正
```

saga 不假装所有资源在一个 ACID 事务里，而是把每一步做成本地事务，并为失败路径设计补偿。

常见做法三：先写租约或状态，再事务外处理。

任务系统可以在短事务里把任务从 `queued` 改成 `running`，写入 `lease_expires_at`，提交后再执行远程工作。worker 崩溃后，租约过期即可重试。

如果确实必须在事务中调用远程服务，至少要满足这些条件：

```text
RPC 超时很短且有明确上限
远程接口有幂等键
远程接口支持查询最终状态
事务中持有的锁范围很小
没有用户交互或长 CPU 计算
有 statement_timeout / lock_timeout / idle_in_transaction_session_timeout
```

但这仍然是高风险设计，通常只适合非常短、内部、可控的调用。

面试里可以这样回答：

```text
事务中执行远程 RPC 的风险是把数据库锁和连接暴露给网络 p99。RPC 慢会延长事务持锁时间，阻塞其他请求并耗尽连接池；RPC 超时还会造成未知状态，数据库能回滚本地修改，却不能回滚已经发生的外部副作用。成功调用后本地提交失败、或者本地回滚后远程已执行，都会带来重复扣款、重复消息、状态不一致。更稳的做法是短事务写本地状态和 outbox，事务外调用远程服务，用幂等键、状态查询和 saga 补偿处理失败。
```

## Q026. 分布式事务和本地事务的复杂度差异是什么？

本地事务的复杂度被数据库内核收在一个边界里。一个 PostgreSQL 实例内部有统一的锁管理、MVCC、WAL、buffer manager、checkpoint、崩溃恢复和事务 ID。应用只要写：

```sql
BEGIN;
UPDATE accounts SET balance = balance - 100 WHERE id = 1;
UPDATE accounts SET balance = balance + 100 WHERE id = 2;
COMMIT;
```

数据库可以保证这两个更新要么一起提交，要么一起回滚。崩溃后也由 WAL 恢复。即使实现很复杂，复杂度主要在数据库内部，应用看到的是一个清晰的 ACID 接口。

分布式事务把边界拆开了。

```text
服务 A 的数据库
服务 B 的数据库
消息队列
对象存储
第三方支付
缓存
搜索索引
```

这些资源没有天然共享的锁表、WAL、时钟、故障检测和恢复日志。一个资源提交成功，另一个资源可能失败；协调者可能崩溃；网络可能分区；参与者可能收到 prepare 但收不到 commit；客户端可能超时后重试；某个服务可能已经对外暴露了中间结果。

复杂度差异主要体现在几个方面。

**第一，故障模型不同。**

本地事务里，数据库进程要么完成，要么崩溃恢复。分布式系统里会出现部分失败：A 成功、B 失败、协调者失联、消息发出但 ack 丢失、RPC 超时但对方已执行。应用必须处理“我不知道对方到底做没做”的状态。

**第二，日志不再统一。**

本地事务有一套 WAL。分布式事务需要全局事务日志或事务管理器记录每个参与者的 prepare/commit/abort 状态。没有可靠协调日志，就无法在崩溃后判断应该继续提交还是回滚。

**第三，锁和隔离跨资源很难维持。**

本地数据库可以持有行锁并做死锁检测。跨服务锁很难做全局死锁检测。长时间持有分布式锁或 prepared transaction，会把可用性和吞吐打穿。

**第四，提交协议更复杂。**

本地提交通常是一次 `COMMIT`。分布式提交至少要协调多个参与者，典型是 2PC：先 prepare，再 commit/rollback。多了网络往返、持久化协调日志、参与者不确定状态和恢复流程。

**第五，应用语义更重要。**

本地事务可以依赖回滚。分布式场景经常只能做补偿。比如“已发货”不能简单回滚数据库字段，可能要发起退货流程；“已发短信”无法撤销；“已扣款”需要退款或冲正。补偿不是技术上的 undo，而是业务上的反向动作。

**第六，性能和可用性代价更高。**

分布式事务增加网络往返和持锁时间。只要一个参与者慢，整体就慢。只要协调者或参与者不可用，事务就可能卡住。CAP 不是口号，落实到事务上就是：强一致跨资源提交通常会牺牲可用性和延迟。

工程上有几种选择：

```text
本地事务 + outbox：保证本地状态和待发送消息一致
2PC/XA：保证多个支持 prepare 的资源原子提交
saga：每步本地提交，失败时执行补偿
事件驱动最终一致：接受 read lag，用幂等和重试收敛
重新划分边界：把必须强一致的数据放回同一个数据库或同一个服务
```

判断标准很朴素：如果一个不变量必须强一致、短事务、参与者都在可控数据库里，才考虑 2PC 或单库建模。如果流程很长、包含人工步骤、HTTP 服务、第三方系统，通常用 saga/outbox，而不是硬套分布式 ACID。

面试里可以这样回答：

```text
本地事务的锁、日志、隔离和恢复都在一个数据库内核里，应用看到的是 BEGIN/COMMIT。分布式事务跨多个独立资源，没有共享 WAL 和锁管理，会遇到部分失败、网络分区、协调者崩溃、参与者不确定状态、跨系统死锁和补偿语义。它需要 2PC、事务管理器、outbox、saga 或事件驱动协议来收敛状态。复杂度不只是多几次 RPC，而是从数据库内部 ACID 变成应用必须处理故障恢复、幂等、补偿和可观测性。
```

## Q027. 2PC 的基本流程是什么？

2PC 是 two-phase commit，两阶段提交。它的目标是在多个参与者之间做一个原子决定：要么所有参与者都提交，要么所有参与者都回滚。PostgreSQL 官方 two-phase transaction 文档说，PostgreSQL 支持 2PC 协议，相关命令是 `PREPARE TRANSACTION`、`COMMIT PREPARED` 和 `ROLLBACK PREPARED`，主要供外部事务管理器使用。

2PC 有两个角色：

```text
coordinator: 协调者，负责收集投票并做最终决定
participant: 参与者，通常是数据库或其他事务资源
```

基本流程如下。

**第一阶段：prepare / vote。**

协调者给所有参与者发送 prepare 请求：

```text
Coordinator -> Participant A: prepare?
Coordinator -> Participant B: prepare?
Coordinator -> Participant C: prepare?
```

每个参与者在本地执行自己的事务逻辑，检查约束，写必要日志，确保如果之后收到 commit，自己大概率可以提交。如果准备成功，就进入 prepared state，返回 yes。如果发现约束失败、资源不足、事务冲突无法解决，就返回 no，并在本地回滚。

PostgreSQL 中对应：

```sql
BEGIN;
UPDATE accounts SET balance = balance - 100 WHERE id = 1;
PREPARE TRANSACTION 'global_tx_123_branch_a';
```

官方 `PREPARE TRANSACTION` 文档说明，执行后事务不再属于当前 session，它的状态会完整存到磁盘上；之后可以从任何 session 用 `COMMIT PREPARED` 或 `ROLLBACK PREPARED` 结束。

**第二阶段：commit / abort。**

如果所有参与者都投 yes，协调者把“全局提交”这个决定写入自己的持久化日志，然后通知所有参与者提交：

```text
Coordinator -> all participants: commit
Participant -> Coordinator: ack
```

PostgreSQL 中对应：

```sql
COMMIT PREPARED 'global_tx_123_branch_a';
```

如果任意参与者投 no，或者 prepare 阶段超时，协调者决定全局回滚，并通知已经 prepared 的参与者回滚：

```sql
ROLLBACK PREPARED 'global_tx_123_branch_a';
```

关键点在于“决定”必须持久化。协调者一旦告诉某些参与者 commit，就不能重启后忘记这个决定，又去告诉其他参与者 rollback。事务管理器要有自己的 durable log，用来在崩溃后继续完成第二阶段。

可以用一段时间线表示：

```text
1. coordinator 生成 global transaction id
2. participant A/B/C 执行业务 SQL
3. coordinator 请求 A/B/C prepare
4. A/B/C 持久化 prepared state，返回 yes
5. coordinator 持久化 commit decision
6. coordinator 请求 A/B/C commit prepared
7. A/B/C 提交并释放锁
8. coordinator 收齐 ack，清理事务日志
```

如果第 4 步有任何 no：

```text
coordinator 持久化 abort decision
通知所有已 prepared 的 participant rollback prepared
```

2PC 的正确性依赖几个前提：

第一，每个参与者真的支持 prepare。普通 HTTP 服务没有 `PREPARE TRANSACTION` 语义，不能随便纳入 2PC。

第二，prepared state 必须可恢复。参与者崩溃重启后要知道自己有一个未决事务。

第三，协调者决定必须可恢复。协调者重启后要继续发送 commit/rollback，直到所有参与者结束。

第四，prepared transaction 不能长期悬挂。PostgreSQL 文档特别警告，长时间停留在 prepared 状态会干扰 VACUUM 回收空间，极端情况下还可能导致事务 ID wraparound 风险，并且它会继续持有原来的锁。

面试里可以这样回答：

```text
2PC 分两阶段。第一阶段 coordinator 让所有 participant prepare，参与者在本地执行事务、检查约束、写入可恢复的 prepared state，然后投 yes 或 no。只要有 no，协调者决定全局 abort。全部 yes 时，协调者先把 commit decision 持久化，再进入第二阶段，通知所有参与者 COMMIT PREPARED；如果决定 abort，则通知 ROLLBACK PREPARED。它的关键是参与者 prepared 后不能自己随便决定，协调者的最终决定必须有持久化日志，崩溃后要继续完成提交或回滚。
```

## Q028. 2PC 的阻塞问题是什么？

2PC 的阻塞问题发生在参与者已经投 yes 并进入 prepared state 之后。这个时候，参与者已经承诺：“如果协调者最终说 commit，我就能 commit；如果协调者说 rollback，我就 rollback。”它不能再单方面改变主意。

如果此时协调者崩溃，或者参与者和协调者之间网络断开，参与者就处在不确定状态：

```text
我已经 prepare 成功
我不知道全局决定是 commit 还是 abort
我不能自己 commit
我也不能自己 rollback
```

这就是 blocking。参与者必须等待协调者恢复，或者等待一个能读取协调者持久化决策日志的恢复组件来告诉它最终结果。

为什么不能自己决定？因为可能有其他参与者已经收到了 commit。假设 A 和 B 都 prepared：

```text
Coordinator 持久化 commit decision
Coordinator 通知 A commit，A 成功
Coordinator 崩溃，还没通知 B
```

B 如果因为等太久自己 rollback，就会破坏原子性：A 提交，B 回滚。反过来，如果全局决定其实是 abort，B 自己 commit 也会破坏一致性。prepared 之后，参与者只能等最终决定。

阻塞带来的线上问题很直接。

**第一，锁被长时间持有。**

prepared transaction 仍然持有它在本地事务中拿到的锁。其他事务访问这些行、索引项或表级资源时会等待。

**第二，VACUUM 被影响。**

PostgreSQL `PREPARE TRANSACTION` 文档明确警告，不应该让事务长时间停留在 prepared 状态；它会干扰 VACUUM 回收空间，极端情况下可能触发事务 ID wraparound 保护问题。

**第三，连接和运维复杂度上升。**

prepared transaction 不再绑定原 session，后续可以从其他 session `COMMIT PREPARED` 或 `ROLLBACK PREPARED`。这对恢复是好事，但也意味着必须有清晰的事务管理器和巡检工具，否则遗留 prepared transaction 会悄悄留在库里。

**第四，可用性受协调者影响。**

2PC 为原子性牺牲了可用性。协调者不可用时，已经 prepared 的参与者无法自行完成。系统可能还有读能力，但相关写路径会被锁住。

**第五，人工介入风险很高。**

DBA 看到一个悬挂的 prepared transaction，可以手工 `COMMIT PREPARED` 或 `ROLLBACK PREPARED`，但如果不知道全局决定，手工处理可能制造跨库不一致。真正可靠的处理方式是恢复事务管理器日志，根据全局决定继续完成第二阶段。

可以用一个时间线看阻塞：

```text
T1: A prepare yes, holds locks
T2: B prepare yes, holds locks
T3: coordinator crashes before participants receive decision
T4: A/B remain prepared
T5: other transactions wait on A/B's locks
T6: VACUUM cannot reclaim some versions
T7: coordinator recovers and sends commit/rollback
```

工程上降低风险的做法：

```text
只让 2PC 覆盖短事务
事务管理器必须持久化决策日志
监控 pg_prepared_xacts
设置 prepared transaction 数量上限
prepared 状态停留时间要报警
协调者恢复逻辑要演练
没有事务管理器就保持 max_prepared_transactions = 0
```

PostgreSQL 文档也建议，如果没有外部事务管理器跟踪 prepared transaction 并及时关闭，就应把 `max_prepared_transactions` 设为 0，避免误创建后遗忘。

面试里可以这样回答：

```text
2PC 的阻塞问题是：参与者一旦 prepare 成功并投 yes，就不能再单方面提交或回滚，必须等待协调者的最终 commit/abort 决定。如果协调者崩溃或网络分区，参与者会卡在 prepared state，继续持有锁和事务资源。PostgreSQL 中这种 prepared transaction 还会干扰 VACUUM，严重时影响事务 ID wraparound。2PC 因此保证了原子提交，但牺牲了故障期间的可用性；必须依赖可靠事务管理器、决策日志、恢复流程和 prepared transaction 监控。
```

## Q029. saga 和 2PC 的取舍是什么？

saga 和 2PC 都是在处理跨资源一致性，但它们的思路完全不同。

2PC 追求原子提交。所有参与者先 prepare，全部准备好后再一起 commit。它的目标是让多个资源像一个事务一样提交或回滚。代价是参与者必须支持 prepare，事务期间要持有锁和 prepared state，协调者故障时会阻塞。

saga 把一个长事务拆成多个本地事务。每一步提交后结果就可见；如果后续步骤失败，系统执行补偿事务，把业务状态修正回来。Garcia-Molina 和 Salem 的 saga 论文里对 saga 的定义很清楚：一个 long-lived transaction 如果能拆成一串可与其他事务交错执行的子事务，并且系统能保证所有子事务完成，或者执行补偿事务来修正部分执行结果，就可以看成 saga。

二者对比如下：

| 维度 | 2PC | saga |
| --- | --- | --- |
| 一致性目标 | 跨资源原子提交 | 最终一致，失败后补偿 |
| 隔离性 | prepared 前后通常持锁，隔离更强 | 中间结果可见，隔离弱 |
| 参与者要求 | 必须支持 prepare/commit/rollback | 只需支持本地事务和补偿动作 |
| 故障表现 | 协调者故障可能阻塞 | 编排器可重试，状态会处于中间态 |
| 性能 | 多轮 RPC，持锁时间长 | 每步短事务，吞吐更好 |
| 适用时长 | 短事务 | 长流程、人工步骤、外部 API |
| 失败处理 | 全局回滚或提交 | 语义补偿、重试、人工修复 |
| 典型风险 | 悬挂 prepared transaction | 补偿不完整、用户看到中间状态 |

适合 2PC 的场景：

第一，参与者数量少，并且都是真正支持 2PC/XA 的事务资源。

第二，事务时间短，不包含用户等待、外部 HTTP、人工审批、长计算。

第三，不变量必须强一致，不能接受中间状态对外可见。

第四，系统有可靠事务管理器、决策日志和恢复流程。

例如两个同构数据库之间做短时间原子更新，或者某些传统企业系统里 XA 资源管理器协调数据库和消息系统。即使这样，也要非常谨慎，因为性能和运维成本都高。

适合 saga 的场景：

第一，业务流程天然是多步骤的。比如下单、锁库存、支付、发货、开发票。

第二，步骤之间可以接受短暂中间状态。比如订单先是 `payment_pending`，之后变成 `paid` 或 `cancelled`。

第三，参与者包含 HTTP 服务、第三方 API、消息队列、人工动作，不可能提供真正的 prepare。

第四，每个步骤都有可定义的补偿。锁库存可以释放，支付可以退款，订单可以取消。

saga 的难点在补偿。补偿不是数据库 rollback。已经发送的短信不能“撤回”，只能再发一条更正消息；已经扣款不能删除记录，只能退款或冲正；已经发货可能只能走退货流程。有些动作根本不可补偿，只能通过延迟执行、人工审核或把不可逆动作放到最后一步降低风险。

还有一个常见误区：saga 不提供全局隔离。其他事务可能看到 saga 的部分执行结果。论文里也提到，补偿时不会尝试通知或回滚那些已经看过部分结果的其他事务。因此 saga 系统要把中间状态建模清楚，API 和 UI 不能假装状态已经最终完成。

两者的取舍可以用一句话概括：

```text
2PC 用阻塞和锁换强原子提交；saga 用补偿和状态机换可用性和长流程可执行性。
```

工程上经常还有第三种选择：重新划分服务边界。如果两个数据必须强一致，最简单可靠的方案往往不是引入分布式事务，而是把它们放在同一个数据库事务边界内。只有当组织、规模或独立部署真的要求拆分时，才引入 saga 或 2PC。

面试里可以这样回答：

```text
2PC 适合短、可控、参与者都支持 prepare 的强一致事务；它能提供跨资源原子提交，但会增加网络往返，持有锁和 prepared state，协调者故障时可能阻塞。saga 适合长业务流程和外部服务调用，每一步是本地事务，失败后用补偿事务修正，吞吐和可用性更好，但中间状态可见，补偿也不等同于回滚。选择时要看不变量是否必须强一致、参与者是否支持 2PC、流程是否很长、动作是否可补偿。很多时候，真正的最佳方案是重新划分边界，把必须强一致的数据放回同一个本地事务里。
```

## Q030. schema migration 如何做到向前兼容？

schema migration 的向前兼容，核心是让旧版本应用和新版本应用在一段时间内都能读写数据库。生产部署很少是“停机、替换所有应用、一次性改完 schema”。更常见的是滚动发布：一部分实例已经是新代码，另一部分还在跑旧代码；读副本、后台任务、消费者、脚本也可能滞后。如果迁移只适配新代码，旧代码就会在发布过程中炸掉。

最常用的方法是 expand-contract。

```text
expand: 先添加新结构，保持旧结构可用
migrate: 新旧代码同时兼容，逐步双写、回填、切读
contract: 确认没有旧代码依赖后，再删除旧结构
```

举一个字段改名的例子。不要直接：

```sql
ALTER TABLE users RENAME COLUMN name TO display_name;
```

如果旧代码还在读 `name`，它会立刻报错。更稳的流程是：

**第一步，expand：添加新列。**

```sql
ALTER TABLE users ADD COLUMN display_name text;
```

先不要删除旧列。新旧列并存。

**第二步，部署兼容代码。**

新代码写入时同时写：

```text
name = value
display_name = value
```

读取时可以优先读 `display_name`，为空时 fallback 到 `name`。

**第三步，分批 backfill。**

```sql
UPDATE users
SET display_name = name
WHERE display_name IS NULL
  AND id > $last_id
ORDER BY id
LIMIT 10000;
```

PostgreSQL 不支持直接在 `UPDATE` 里这么写 `ORDER BY LIMIT` 的简单形式，真实实现常用主键分页子查询。重点是分批提交，避免一个大事务长时间持锁、制造大量 WAL 和 bloat。

**第四步，切读。**

确认 backfill 完成、双写稳定、指标正常后，代码改成只读 `display_name`，但仍然可以保留写旧列一段时间。

**第五步，contract：删除旧列。**

等所有旧版本应用、后台任务、报表、消费者都不再依赖 `name`，再删除旧列：

```sql
ALTER TABLE users DROP COLUMN name;
```

这个步骤最好单独发布，方便回滚。

除了字段改名，其他迁移也有类似套路。

**添加列。**

添加 nullable 列通常兼容旧代码。PostgreSQL 文档说明，添加带常量默认值的列不需要在执行 `ALTER TABLE` 时更新每一行，访问旧行时会返回默认值，之后表重写时再应用，因此大表上也可以很快。但如果默认值是 volatile 表达式，比如 `clock_timestamp()`，每行都要计算并更新，可能变成长时间操作。保守做法是：先加 nullable 列或常量默认列，再分批回填，再加约束。

**增加 NOT NULL。**

不要一上来给大表新列加 `NOT NULL`。先允许 null，代码开始写值，backfill，验证没有 null，再加约束。PostgreSQL 支持 `NOT VALID` 和 `VALIDATE CONSTRAINT` 用于某些约束场景，能把“开始约束新数据”和“扫描历史数据验证”拆开。加 `SET NOT NULL` 可能需要扫描全表，要评估锁和耗时。

**增加索引。**

生产大表不要随便 `CREATE INDEX`。PostgreSQL 官方文档说明，普通创建索引会阻塞写入；`CREATE INDEX CONCURRENTLY` 不会拿阻止 insert/update/delete 的锁，但需要更多工作、耗时更长，并且不能放在普通事务块里。失败时还可能留下 invalid index，需要 drop 后重建。

```sql
CREATE INDEX CONCURRENTLY users_email_idx ON users (email);
```

**增加唯一约束。**

先清理重复数据，再 `CREATE UNIQUE INDEX CONCURRENTLY`，最后用现有唯一索引转换为约束。PostgreSQL `ALTER TABLE` 文档也提到，用已有唯一索引添加约束可以减少长时间阻塞更新的风险。

**修改列类型。**

直接 `ALTER COLUMN TYPE` 可能重写表、阻塞写入，还可能让旧代码不兼容。大表上更稳的方式是新增目标类型列，双写，分批回填，切读，然后删旧列。

**删除字段或表。**

删除是最后一步。先让代码不再读，再不再写，观察一段时间。可以先标记 deprecated、停止写入、加日志确认没有访问，再真正 drop。很多生产事故来自“以为没人用”的列被后台脚本或报表依赖。

向前兼容迁移还要处理应用发布顺序：

```text
1. 数据库 expand migration
2. 发布兼容旧 schema 和新 schema 的应用
3. backfill 历史数据
4. 切换读路径或开启 feature flag
5. 停止写旧字段
6. contract migration 删除旧 schema
```

回滚也要设计。只要旧代码可能回滚，就不能删除它需要的列。新代码写出的数据，旧代码至少要能忽略或容忍。API 和数据库都要遵守“新增可选、旧字段保留、语义不突然改变”的原则。

迁移脚本本身最好具备这些性质：

```text
幂等：重复执行不会破坏数据
可观测：有进度、错误、耗时、影响行数
可暂停：批量回填可以停下再继续
小事务：避免长事务阻塞 VACUUM 和锁
低锁：DDL 前评估锁级别，必要时设置 lock_timeout
可回滚：至少有明确的恢复路径
```

面试里可以这样回答：

```text
schema migration 的向前兼容要按 expand-contract 做。先只做兼容性扩展，比如新增 nullable 列、新表、新索引，不删除旧字段；再发布同时兼容新旧 schema 的代码，必要时双写、fallback 读、分批 backfill；确认所有实例和后台任务都切到新结构后，再 contract 删除旧字段或旧表。PostgreSQL 上还要注意 DDL 锁、大表回填、CREATE INDEX CONCURRENTLY、NOT VALID/VALIDATE CONSTRAINT、volatile default 可能重写表等细节。核心原则是让旧代码和新代码在滚动发布窗口内都能正常工作。
```

## Q031. 为什么在线加非空列需要谨慎？

在线加非空列要谨慎，因为它同时碰到三个问题：历史数据怎么满足非空约束，DDL 会拿什么锁，旧代码和新代码在滚动发布期间能不能同时工作。很多事故不是 SQL 语法错了，而是迁移语句在大表上扫描太久、锁住写入，或者某个旧版本应用还在插入不带新列的行。

先看最直接的写法：

```sql
ALTER TABLE users ADD COLUMN region text NOT NULL;
```

如果表里已经有数据，这条语句很难安全。历史行没有 `region` 值，数据库无法凭空证明它们满足 `NOT NULL`。即使你写了默认值，也要看默认值的类型、PostgreSQL 版本和表大小。

PostgreSQL 官方文档对添加列有两个细节值得记住。添加列时，新列会用默认值填充；如果没有默认值，就是 `NULL`。添加带常量默认值的列，不需要在执行 `ALTER TABLE` 时逐行更新，旧行在访问时会返回默认值，因此在大表上可以很快。但如果默认值是 volatile 表达式，比如 `clock_timestamp()`，每一行都要计算并写入，这会变成真正的大表更新。

所以这两条语句的风险完全不同：

```sql
-- 通常较轻，取决于版本和上下文
ALTER TABLE users ADD COLUMN status text DEFAULT 'active';

-- 需要为每行计算，可能触发表重写或长时间更新
ALTER TABLE users ADD COLUMN created_at timestamptz DEFAULT clock_timestamp();
```

非空约束本身也要扫描。PostgreSQL `ALTER TABLE ... SET NOT NULL` 必须确认列里没有 `NULL`。官方文档说明，通常会扫描整张表；如果已经有一个有效的 `CHECK` 约束能证明没有 `NULL`，才可以跳过扫描。大表扫描会消耗 I/O，持有锁，还可能和线上写入互相等待。

另一个问题是锁。PostgreSQL `ALTER TABLE` 文档说，除非某个子命令明确说明使用较弱锁，否则默认会拿 `ACCESS EXCLUSIVE`。这个锁级别很重，会阻塞很多并发访问。即使某条 DDL 实际执行很快，只要它在等前面的长查询释放锁，后面的线上请求也可能排队，形成“锁队列放大”：

```text
long SELECT holds old lock
ALTER TABLE waits for stronger lock
new SELECT/INSERT/UPDATE queue behind ALTER TABLE
application p99 spikes
```

因此在线加非空列通常按多阶段做。

第一步，先加 nullable 列，保持旧代码可用：

```sql
ALTER TABLE users ADD COLUMN region text;
```

旧版本应用插入时不带 `region`，数据库仍然接受。新版本应用可以开始写这个字段。

第二步，发布兼容代码。新代码写入 `region`，读的时候可以处理 `NULL`：

```text
write path: region must be filled for new rows
read path: if region is null, use fallback rule
```

这个阶段不要急着加 `NOT NULL`。滚动发布时，一部分实例可能还是旧代码。只要旧代码还可能写入空值，数据库约束就会把旧代码打爆。

第三步，分批 backfill 历史数据。不要一个事务更新全表：

```sql
UPDATE users
SET region = 'unknown'
WHERE region IS NULL
  AND id >= $start_id
  AND id < $end_id;
```

批量大小要根据 WAL、复制延迟、锁等待、autovacuum 压力和业务低峰调整。每批提交一次，能减少长事务、死元组堆积和复制延迟。

第四步，先用可验证约束降低风险。常见做法是加一个 `CHECK`：

```sql
ALTER TABLE users
ADD CONSTRAINT users_region_not_null
CHECK (region IS NOT NULL) NOT VALID;
```

`NOT VALID` 的价值在于先约束新写入，但跳过对历史数据的长时间扫描。PostgreSQL 文档说明，`NOT VALID` 会让新插入或更新继续受约束影响，但数据库暂时不假设历史行都满足约束。

第五步，单独验证：

```sql
ALTER TABLE users VALIDATE CONSTRAINT users_region_not_null;
```

`VALIDATE CONSTRAINT` 会扫描历史数据。它仍然有成本，但锁级别比很多直接 DDL 轻，更适合在线执行。验证通过后，再根据版本和约束证明情况执行：

```sql
ALTER TABLE users ALTER COLUMN region SET NOT NULL;
```

最后再决定是否保留或删除辅助 `CHECK` 约束。这个动作要看团队规范和数据库版本。

这里还有几个边界。

如果新列参与业务关键路径，比如权限、计费、租户隔离，不能用随便的默认值糊过去。`'unknown'` 可能让权限判断失真。需要先定义真实的业务含义。

如果新列要从其他表计算出来，backfill 要能重复执行。迁移脚本中断后，下一次应该继续处理未完成的行，而不是把已经修正的数据覆盖回旧值。

如果加列后立刻加非空约束，回滚也会麻烦。旧代码一旦回滚，可能又开始写不带新列的记录。向前兼容的窗口要覆盖“发布新代码”和“可能回滚到旧代码”这两个方向。

面试里可以这样回答：

```text
在线加非空列谨慎，是因为它会同时影响历史数据、DDL 锁和滚动发布兼容性。大表上直接 ADD COLUMN NOT NULL 或 SET NOT NULL 可能需要扫描全表，拿较重锁，甚至因为默认值是 volatile 表达式而逐行更新。旧版本应用在发布窗口内也可能继续写不带新列的行。稳妥做法是先加 nullable 列，发布兼容代码，分批 backfill，再用 NOT VALID/CHECK 或验证步骤证明历史数据满足条件，最后设置 NOT NULL。过程中要设置 lock_timeout，观察 WAL、复制延迟、锁等待和错误率。
```

## Q032. 为什么大表建索引需要考虑锁和回填？

大表建索引要考虑锁和回填，是因为“建索引”不是只在元数据里登记一个结构。数据库必须扫描已有表数据，把每一行对应的索引项写入新索引。对大表来说，这就是一次大规模读表、排序或构建树结构、写索引文件、写 WAL、更新系统目录的过程。它会消耗 CPU、I/O、内存和缓存，还会和线上写入互相影响。

PostgreSQL 普通 `CREATE INDEX` 的行为很重。官方文档说，普通索引构建会锁住被索引表的写入；其他事务仍然可以读，但 `INSERT`、`UPDATE`、`DELETE` 会阻塞到索引构建结束。大表索引可能运行数小时，这对生产系统通常不可接受。

```sql
CREATE INDEX orders_created_at_idx
ON orders (created_at);
```

这条语句在小表上可能没感觉，在亿级订单表上就可能把写路径挂住。

PostgreSQL 提供了 `CREATE INDEX CONCURRENTLY`：

```sql
CREATE INDEX CONCURRENTLY orders_created_at_idx
ON orders (created_at);
```

它不会拿阻止并发 insert/update/delete 的锁，更适合在线生产环境。但 `CONCURRENTLY` 不是免费午餐。官方文档列出几个重要代价：

第一，它需要两次扫描表，并等待可能修改或使用该索引的既有事务结束。总工作量比普通建索引更大，耗时更长。

第二，它会给线上系统带来额外 CPU 和 I/O。虽然不阻塞写入，但构建过程仍然读大表、写索引、写 WAL。云盘、共享存储、缓存都可能被打满。

第三，构建过程中索引会先以 invalid 状态进入系统目录。失败时可能留下 invalid index。这个索引不会用于查询，但仍可能带来更新维护开销。官方推荐的恢复方式是 drop 掉这个 invalid index，然后重新 `CREATE INDEX CONCURRENTLY`，或者用并发 reindex。

第四，唯一索引并发构建更微妙。唯一性在第二次扫描开始时就可能开始对其他事务生效。也就是说，在索引还没最终可用前，其他写入就可能因为唯一约束失败。如果第二次扫描失败，invalid index 还可能继续 enforcing uniqueness，处理不当会很难排查。

第五，`CREATE INDEX CONCURRENTLY` 不能放在普通事务块里。迁移工具如果默认把每个 migration 包在一个事务中，这条语句会直接失败。要为它单独配置 non-transactional migration。

第六，同一张表一次只能有一个 concurrent index build。多个迁移脚本同时给同一张大表建索引，会互相等待。

回填也要单独考虑。这里的“回填”有两层含义。

第一层是索引自己的回填。新索引必须覆盖已有数据，不能只覆盖新写入。构建过程扫描整表，把历史行的 key 写进去。这就是为什么大表建索引会慢。

第二层是业务字段回填。如果你先新增了列，再为这个列建索引，就要想清楚顺序：

```text
方案 A：先建索引，再 backfill 字段
每批 UPDATE 都要维护这个新索引，写放大更高。

方案 B：先 backfill 字段，再建索引
backfill 阶段少维护一个索引，但索引完成前查询不能依赖它。
```

哪种更好取决于业务。多数情况下，如果查询还没切到新字段，先 backfill、验证数据，再建索引更省写入成本。如果新代码已经需要靠索引保护延迟，就可能先建部分索引或按阶段切流。

回填还会制造其他压力：

```text
大量 UPDATE 产生 WAL 和 dead tuples
复制延迟上升
autovacuum 压力增大
热点页和索引页竞争
缓存被批处理扫描污染
长事务阻碍清理
```

生产做法一般是：

```sql
SET lock_timeout = '3s';
SET statement_timeout = '30min';

CREATE INDEX CONCURRENTLY orders_created_at_idx
ON orders (created_at);
```

再配合：

```text
低峰执行
观察 pg_stat_progress_create_index
限制 backfill 批大小
监控 WAL 生成速率和 replication lag
失败后检查 invalid index
给索引稳定命名，避免重复创建
上线前用 EXPLAIN 验证查询会使用它
```

面试里可以这样回答：

```text
大表建索引要考虑锁和回填，因为数据库必须扫描历史数据并构建完整索引。普通 CREATE INDEX 会阻塞表上的写入，大表上可能锁很久；CREATE INDEX CONCURRENTLY 可以让正常写入继续，但要做两次扫描、等待旧事务、消耗更多 CPU/I/O，并且失败可能留下 invalid index。业务字段 backfill 还会产生大量 WAL、dead tuple、复制延迟和索引维护成本。正确做法是按低峰、并发建索引、批量回填、进度监控、失败清理和查询计划验证来设计，而不是把建索引当成一个普通元数据操作。
```

## Q033. expand-contract migration 是什么？

expand-contract migration 是一种面向在线发布的数据库迁移方法。它默认系统会滚动发布，旧代码和新代码会在一段时间内同时运行。因此迁移不能只考虑“最终 schema 长什么样”，还要考虑每个中间状态是否能被所有运行中的程序接受。

它分三段：

```text
expand: 先做兼容性扩展
migrate: 让新旧结构并存，完成双写、回填、切读
contract: 确认没有旧依赖后，收缩旧结构
```

举一个改列名的例子。目标是把 `users.name` 改成 `users.display_name`。

危险做法是直接 rename：

```sql
ALTER TABLE users RENAME COLUMN name TO display_name;
```

这在停机发布里可能可行，但在线滚动发布里不安全。旧代码还在读写 `name`，新代码开始读写 `display_name`，中间一定有人报错。

expand-contract 的做法是：

第一步，expand，新增结构但不删除旧结构：

```sql
ALTER TABLE users ADD COLUMN display_name text;
```

第二步，发布兼容代码。写路径双写：

```text
name = value
display_name = value
```

读路径优先读新列，必要时 fallback：

```text
if display_name is not null:
    use display_name
else:
    use name
```

第三步，migrate，分批回填历史数据：

```sql
UPDATE users
SET display_name = name
WHERE display_name IS NULL
  AND id >= $start
  AND id < $end;
```

第四步，切读。确认新列覆盖率、错误率、查询计划、索引、报表和后台任务都正常后，代码只读 `display_name`，但可以继续双写一段时间。

第五步，停止写旧列。再观察一段时间，确认没有旧版本服务、定时任务、手工脚本、BI 报表还依赖 `name`。

第六步，contract，删除旧结构：

```sql
ALTER TABLE users DROP COLUMN name;
```

这个删除动作要晚，而且要单独发布。删除是最难回滚的一步。

再看一个拆表的例子。原来 `orders` 表里有 `shipping_address`，现在要拆到 `order_addresses`。

expand：

```sql
CREATE TABLE order_addresses (
    order_id bigint PRIMARY KEY,
    shipping_address text NOT NULL
);
```

migrate：

```text
新代码创建订单时同时写 orders.shipping_address 和 order_addresses
后台把旧 orders.shipping_address 分批复制到 order_addresses
读路径从 order_addresses 读，缺失时 fallback 到 orders
```

contract：

```text
确认所有读写都切到 order_addresses
停止写 orders.shipping_address
删除 orders.shipping_address
```

expand-contract 的关键不是步骤名字，而是兼容性方向。

**向前兼容。**

旧代码遇到新 schema 不会崩。新增 nullable 列、新表、新索引通常安全；删除列、改列类型、改语义通常危险。

**向后兼容。**

新代码上线后如果回滚到旧代码，旧代码还能处理新数据。比如新代码写入了一个旧代码不认识的枚举值，旧代码可能直接失败。数据库迁移不能只看列是否存在，还要看值域和语义。

**数据迁移和 schema 迁移分开。**

DDL 通常短一些，backfill 可能跑很久。把它们拆开，失败时更容易恢复。

**contract 必须等证据。**

删除旧字段前，要用日志、查询审计、指标、代码搜索、连接用户分析确认没有读写。仅凭“我觉得没有服务用了”不够。

常见错误：

第一，把 expand 和 contract 放在一个 migration 里。刚加新列又删旧列，滚动发布窗口里旧代码必挂。

第二，只有双写，没有校验。双写可能失败一边，或者两个字段语义不一致。需要采样比对、覆盖率指标和修复脚本。

第三，backfill 用一个大事务。大事务会造成锁等待、WAL 暴涨、复制延迟和 VACUUM 受阻。

第四，忽略非应用消费者。报表、导出脚本、数据科学任务、人工 SQL、CDC 消费者都可能依赖旧 schema。

面试里可以这样回答：

```text
expand-contract migration 是在线数据库迁移的三阶段方法。先 expand，只做兼容性扩展，比如新增 nullable 列、新表、新索引，保证旧代码还能跑；再 migrate，让新旧结构并存，通过双写、fallback 读、分批 backfill、校验和切流完成数据迁移；最后 contract，在确认没有旧代码和后台任务依赖后，再删除旧列、旧表或旧索引。它解决的是滚动发布期间新旧代码同时访问数据库的问题。真正的重点是每个中间状态都要兼容，而不是只关心最终 schema。
```

## Q034. 数据库迁移失败如何回滚？

数据库迁移失败时，第一反应不应该是立刻执行 `down.sql`。先判断失败发生在哪个阶段、数据库已经提交了哪些变更、应用代码是否已经发布、是否有数据被新代码写入。很多迁移的安全恢复方式是 roll forward，也就是修一个新的迁移把系统带到一致状态，而不是机械地反向执行。

可以按阶段拆开。

**第一，迁移还在事务内失败。**

PostgreSQL 的很多 DDL 可以放在事务里。如果 migration runner 把整段包在一个事务中，失败后数据库会自动回滚这个事务。此时最重要的是确认没有外部副作用，没有部分提交。

```sql
BEGIN;
ALTER TABLE users ADD COLUMN region text;
ALTER TABLE users ADD CONSTRAINT users_region_check CHECK (region <> '');
-- error
ROLLBACK;
```

这类失败相对容易。修 SQL，重新跑。

**第二，迁移包含不能放在事务里的操作。**

`CREATE INDEX CONCURRENTLY` 和 `DROP INDEX CONCURRENTLY` 不能放在普通事务块里。它们失败后可能留下中间状态。PostgreSQL 官方文档说明，并发建索引失败可能留下 invalid index；这个索引不用于查询，但仍可能消耗更新维护成本。推荐处理方式是 drop 掉 invalid index，再重试并发建索引。

检查：

```sql
SELECT indexrelid::regclass AS index_name,
       indisvalid,
       indisready
FROM pg_index
WHERE indexrelid = 'orders_created_at_idx'::regclass;
```

清理：

```sql
DROP INDEX CONCURRENTLY IF EXISTS orders_created_at_idx;
```

注意，`DROP INDEX CONCURRENTLY` 也不能在事务块里执行，而且不能和 `CASCADE` 一起用。它适合清理普通独立索引，不适合直接删除支撑主键或唯一约束的索引。

**第三，schema 变更已经提交，但应用还没切流。**

如果只是 expand 阶段，比如新增 nullable 列、新表、新索引，通常可以先保留它们，不急着回滚。新增结构一般对旧代码无害。修复 migration 或代码后继续往前走，风险比删除再添加小。

**第四，新代码已经写入新 schema。**

这时回滚应用代码要非常小心。旧代码是否能读懂新数据？新列是否已经成为事实来源？新枚举值是否已经写入？如果旧代码不能处理这些数据，单纯回滚应用会扩大故障。

常见做法是：

```text
暂停相关写入口或降级功能
保留新 schema
修复兼容逻辑
把异常数据修正到旧代码可接受的形态
再决定回滚代码还是继续发布修复版
```

**第五，数据 backfill 失败。**

backfill 要设计成可暂停、可重试。失败时不要回滚已经成功的批次，除非确认数据变换是错误的。更常见的处理是记录 checkpoint，修复脚本后从未完成区间继续。

```sql
CREATE TABLE migration_progress (
    migration_name text PRIMARY KEY,
    last_id bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

每批提交后更新 `last_id`。脚本重启时从 `last_id` 继续。这个设计比一个超大事务可靠。

**第六，破坏性迁移失败。**

如果已经 `DROP COLUMN`、`DROP TABLE`、重写了类型，回滚成本很高。`DROP COLUMN` 删除的不只是字段，还可能影响索引、约束、视图、依赖对象。没有备份、影子列或审计日志时，靠 `down.sql` 很可能恢复不了真实数据。

破坏性迁移要提前准备：

```text
先停写旧字段，再观察
保留旧字段一段时间
删除前备份或创建影子表
确认 PITR 可用
把 contract 放在最后一个单独发布窗口
```

回滚策略通常有四类。

**自动事务回滚。**

适合短 DDL、无外部副作用、能放进事务的迁移。

**补偿迁移。**

新增一个修复 migration，把 schema 或数据带到可用状态。生产里比执行旧的 down 脚本更常见。

**应用回滚加兼容 schema。**

如果只是 expand，保留新列新表，让旧代码继续跑。等修好后再继续。

**数据恢复。**

如果数据被破坏，只能从备份、PITR、审计日志、CDC 或影子表恢复。这个时候要先保全现场，避免修复脚本二次破坏。

迁移工具层面也要有状态表：

```text
version
checksum
started_at
finished_at
state: running / failed / applied
operator or deploy id
```

失败后先查这个状态表，再查数据库对象真实状态。不要只相信 migration runner 的日志。

面试里可以这样回答：

```text
数据库迁移失败要先判断失败点，而不是马上执行 down.sql。如果迁移在事务内失败，PostgreSQL 可以整体回滚；如果用了 CREATE INDEX CONCURRENTLY 这类不能放进事务的操作，失败后可能留下 invalid index，需要检查系统目录、DROP INDEX CONCURRENTLY 后重试。已经提交的 expand 变更通常可以保留，继续 roll forward；已经写入新数据时，应用回滚要确认旧代码能否读懂这些数据。backfill 要靠 checkpoint 重试，破坏性迁移只能依赖提前的备份、影子字段或 PITR。生产恢复更常见的是补偿迁移和向前修复，而不是盲目反向执行。
```

## Q035. 如何设计 migration 的幂等性？

migration 的幂等性，指同一个迁移在失败重试、部署重跑、节点切换或人工恢复时，不会因为重复执行而破坏 schema 或数据。它不等于“所有 SQL 都加 `IF NOT EXISTS`”。真正的幂等要同时验证对象是否存在、对象定义是否符合预期、数据处理是否可重复、迁移状态是否清楚。

最基础的是版本表。每个 migration 有唯一版本号和 checksum：

```sql
CREATE TABLE schema_migrations (
    version text PRIMARY KEY,
    checksum text NOT NULL,
    state text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
```

执行前先抢占版本：

```sql
INSERT INTO schema_migrations(version, checksum, state)
VALUES ($version, $checksum, 'running')
ON CONFLICT (version) DO NOTHING;
```

如果版本已存在，要检查 checksum 和 state。相同版本不同 checksum 是危险信号，不能静默跳过。`failed` 状态也不能直接当作未执行；要检查数据库实际对象状态，再决定清理、继续或人工介入。

DDL 层可以用条件语句，但要带校验。

```sql
ALTER TABLE users ADD COLUMN IF NOT EXISTS region text;
```

这条语句重复执行不会因为列已存在而失败。但它不能证明已有的 `region` 列类型就是 `text`，也不能证明默认值、约束、注释、权限都符合预期。所以迁移脚本要查系统目录或 information_schema：

```sql
SELECT data_type, is_nullable
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = 'users'
  AND column_name = 'region';
```

索引也是一样。PostgreSQL `CREATE INDEX IF NOT EXISTS` 文档说，已有同名关系时不会报错，但不保证已有索引和你本来要创建的索引一样。也就是说：

```sql
CREATE INDEX IF NOT EXISTS users_region_idx ON users (region);
```

只能避免同名错误，不能证明已有 `users_region_idx` 真的是 `(region)` 上的 btree 索引。严谨做法是用稳定命名，加上定义校验：

```sql
SELECT indexdef
FROM pg_indexes
WHERE schemaname = 'public'
  AND tablename = 'users'
  AND indexname = 'users_region_idx';
```

数据 backfill 的幂等更重要。错误写法是无条件覆盖：

```sql
UPDATE users
SET region = infer_region(email);
```

如果第一次执行后用户已经修改了 region，第二次重跑会把用户新值覆盖掉。更稳的是只处理尚未迁移的行：

```sql
UPDATE users
SET region = infer_region(email)
WHERE region IS NULL
  AND id >= $start_id
  AND id < $end_id;
```

如果转换逻辑可能变化，还要保存来源版本：

```sql
ALTER TABLE users ADD COLUMN region_migrated_by text;

UPDATE users
SET region = infer_region(email),
    region_migrated_by = '2026_06_19_region_v1'
WHERE region IS NULL
  AND region_migrated_by IS NULL;
```

这样脚本重跑不会覆盖已经处理过的行。发现 v1 逻辑错了，也能写一个 v2 修复脚本，只处理 `region_migrated_by = 'v1'` 的数据。

幂等迁移还要控制并发。两个部署进程同时跑 migration，可能同时执行 DDL 或 backfill。常见做法是迁移开始前拿数据库锁：

```sql
SELECT pg_advisory_lock(hashtext('schema_migration_lock'));
```

结束后释放：

```sql
SELECT pg_advisory_unlock(hashtext('schema_migration_lock'));
```

如果迁移工具已经提供全局锁，也可以用工具自带能力。核心是同一时间只能有一个 migration runner 修改 schema。

对长任务，幂等性来自进度记录：

```sql
CREATE TABLE migration_progress (
    migration_name text PRIMARY KEY,
    last_processed_id bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

每批处理后更新进度。重启时从上次位置继续。不要把几千万行 backfill 放进一个事务里赌它一次成功。

外部副作用要特别小心。migration 里如果发消息、调 HTTP、写对象存储，就必须有幂等键和结果表。更好的做法是迁移只改数据库状态，把外部副作用交给 outbox 或后台 worker。schema migration 最好不要直接调用外部系统。

还要避免“伪幂等”。

```sql
DROP TABLE IF EXISTS old_orders;
```

这条语句重复执行确实不报错，但如果第一次误删了表，第二次安静跳过，只会掩盖事故。破坏性操作不能只靠 `IF EXISTS`。要先验证依赖、备份、确认无读写，再执行。

一个较好的幂等迁移检查清单：

```text
每个 migration 有唯一 version 和 checksum
执行前拿全局迁移锁
DDL 使用稳定对象名
IF EXISTS / IF NOT EXISTS 后面跟定义校验
数据迁移只处理未完成或特定版本的数据
长 backfill 有 checkpoint，可暂停可恢复
失败状态不会被静默当作成功
破坏性操作有显式确认、备份或观察窗口
迁移完成后验证行数、约束、索引有效性和查询计划
```

面试里可以这样回答：

```text
migration 幂等性不是简单加 IF NOT EXISTS，而是让迁移在失败重试和重复部署时仍然安全。做法包括：用 schema_migrations 表记录 version、checksum 和状态；执行前拿全局迁移锁；DDL 使用稳定命名，并在 IF EXISTS/IF NOT EXISTS 后校验对象定义；数据 backfill 只处理未迁移行，保存进度和迁移版本；长任务分批提交并可恢复；失败状态必须人工或脚本确认后继续。尤其要注意 PostgreSQL 的 CREATE INDEX IF NOT EXISTS 只保证同名对象存在时不报错，不保证已有索引定义正确，所以幂等还必须包含定义校验。
```

## Q036. 如何用 EXPLAIN 分析慢查询？

`EXPLAIN` 的作用不是“看一眼有没有用索引”，而是把一次查询拆成可解释的执行计划：从哪里读数据、怎么过滤、怎么 join、怎么排序、估算行数和真实行数差多少、时间花在哪个节点、有没有读磁盘、有没有临时文件、有没有产生大量 WAL。面试里如果只说“看是不是 Seq Scan”，会显得太浅。很多慢查询用了索引仍然慢；也有一些顺序扫描是正确选择。

分析慢查询我一般按这条顺序来：

```text
先定位慢 SQL -> 再拿到代表性参数 -> 用 EXPLAIN 看计划 -> 用 EXPLAIN ANALYZE 验证真实执行 -> 对比估算和真实 -> 再决定改 SQL、索引、统计信息、内存或数据模型
```

第一步是定位 SQL。线上不要靠感觉猜。常见来源有几类：

```sql
-- 依赖 pg_stat_statements 找累计耗时最高、平均耗时最高、调用次数最多的语句
SELECT query,
       calls,
       total_exec_time,
       mean_exec_time,
       rows,
       shared_blks_hit,
       shared_blks_read,
       temp_blks_read,
       temp_blks_written
FROM pg_stat_statements
ORDER BY total_exec_time DESC
LIMIT 20;
```

也可以用 `log_min_duration_statement` 记录超过阈值的 SQL，用 `auto_explain` 把慢查询计划写进日志。`pg_stat_statements` 更适合做长期排名，日志更适合抓某一次异常，`auto_explain` 更适合发现“偶发慢、参数相关、只有线上才出现”的计划问题。它们不是互斥的。

第二步是确认参数和环境。慢查询常常不是 SQL 文本慢，而是某组参数慢。例如：

```sql
SELECT *
FROM tasks
WHERE tenant_id = $1
  AND status = $2
ORDER BY created_at DESC
LIMIT 50;
```

`tenant_id = 'small-tenant'` 可能很快，`tenant_id = 'large-tenant'` 可能很慢。分析时要尽量用真实慢请求的参数、相同 PostgreSQL 版本、相近数据量、相近统计信息。否则你在测试库看到的计划可能没有意义。

最基础的写法是：

```sql
EXPLAIN
SELECT ...
```

这只显示 planner 选择的计划，不会执行查询。要看真实耗时和真实行数，需要：

```sql
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT ...
```

`ANALYZE` 会真正执行 SQL。对 `SELECT` 来说通常可控，但对 `INSERT`、`UPDATE`、`DELETE`、`MERGE` 这类语句要特别小心。PostgreSQL 官方文档也提醒，`EXPLAIN ANALYZE` 会执行语句，副作用照常发生。想分析写语句又不落库，通常这样做：

```sql
BEGIN;

EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
UPDATE jobs
SET status = 'running'
WHERE id = 42;

ROLLBACK;
```

这样可以看到计划和实际执行情况，又不会提交修改。注意：这仍然会拿锁、读写 buffer、产生临时影响，不能在生产高峰随便跑。

读计划时要从树的底部往上看。底层节点通常负责扫描表或索引，上层节点负责 join、sort、aggregate、limit。一个简单例子：

```text
Limit
  ->  Index Scan using tasks_tenant_status_created_idx on tasks
        Index Cond: ((tenant_id = 't1') AND (status = 'ready'))
```

这表示数据库先通过索引找到候选行，再在上层做 `LIMIT`。如果看到：

```text
Limit
  ->  Sort
        Sort Key: created_at DESC
        ->  Seq Scan on tasks
              Filter: ((tenant_id = 't1') AND (status = 'ready'))
```

就要问几个问题：是不是缺少 `(tenant_id, status, created_at DESC)` 这类组合索引？过滤条件选择性是不是太低，顺序扫描反而更便宜？统计信息是不是过期？`ORDER BY` 和索引顺序能不能匹配？`LIMIT 50` 是否应该通过索引尽早停止，而不是扫描后排序？

`EXPLAIN` 里的 `cost` 是估算成本，不是毫秒。它通常长这样：

```text
cost=0.43..1823.51 rows=500 width=128
```

这几个数分别可以这样理解：

```text
startup cost : 返回第一行前的估算成本
total cost   : 完成该节点的估算总成本
rows         : planner 估算这个节点会输出多少行
width        : 估算每行平均字节数
```

真正要重点看的，是估算行数和真实行数的差距。`EXPLAIN ANALYZE` 会出现：

```text
rows=500 ...
actual time=0.031..250.814 rows=120000 loops=1
```

如果估算 500 行，实际 120000 行，planner 很可能会选错 join 顺序、join 算法或索引。根因可能是统计信息过期、数据倾斜、列之间相关性强、表达式过滤没有统计信息、参数化查询用了不合适的 generic plan。修复方式不一定是加索引，也可能是：

```sql
ANALYZE tasks;

CREATE STATISTICS tasks_tenant_status_stats
ON tenant_id, status
FROM tasks;

ANALYZE tasks;
```

多列相关性很强时，扩展统计信息比盲目加索引更贴近问题。

`BUFFERS` 很关键。它能告诉你慢是“算得多”还是“读得多”。典型输出：

```text
Buffers: shared hit=120 read=43000 dirtied=0 written=0
```

`shared hit` 表示从 shared buffers 命中，不需要真的从数据文件读。`shared read` 表示需要读数据块，可能触发磁盘 I/O 或操作系统 page cache 读取。`temp read/write` 表示排序、hash、materialize 等操作溢出到了临时文件。看到大量 `temp written`，要检查排序或 hash 是否超过 `work_mem`：

```text
Sort Method: external merge  Disk: 204800kB
```

这说明排序落盘了。修复方向可能是加合适索引避免排序，也可能是调大特定会话的 `work_mem`，也可能是减少结果集。不要直接全局调大 `work_mem`，因为它是每个节点、每个并发查询都可能消耗的内存，不是全实例共享上限。

`actual time` 要结合 `loops` 看。比如：

```text
Index Scan ... (actual time=0.020..0.050 rows=1 loops=100000)
```

单次很快，但循环 10 万次就慢了。这常见于 nested loop：外表返回太多行，内表每行查一次。修复方向可能是让外表更早过滤、补索引、改 join 顺序、改写 SQL，或者让 planner 选择 hash join。

join 节点也要会读：

```text
Nested Loop
Hash Join
Merge Join
```

`Nested Loop` 适合外侧行数少、内侧有高效索引的情况。外侧行数被低估时，它可能灾难性变慢。`Hash Join` 适合较大集合等值连接，但 hash 表可能占内存，溢出会写临时文件。`Merge Join` 依赖两侧有序，适合已经按 join key 排好或能通过索引顺序读取的场景。

索引相关的判断也不能只看“有没有 Index Scan”。几个常见问题：

```text
Seq Scan 并不一定错：小表、低选择性条件、要读大部分行时，顺序扫描可能更便宜。
Index Scan 并不一定快：回表太多、随机 I/O 多、过滤条件不在索引里，也会慢。
Bitmap Heap Scan 常见于中等选择性：先从索引拿 TID，再按 heap page 合并读取。
Index Only Scan 需要 visibility map 支持：如果表频繁更新、vacuum 跟不上，仍然可能大量 heap fetch。
```

一个典型反例是函数包住列：

```sql
WHERE lower(email) = lower($1)
```

普通 `email` 索引用不上。要么改成规范化字段，要么建表达式索引：

```sql
CREATE INDEX users_lower_email_idx ON users (lower(email));
```

另一个反例是隐式类型转换：

```sql
WHERE order_id::text = $1
```

如果 `order_id` 是 bigint，这种写法可能让索引条件变差。更好的做法是让参数类型和列类型一致：

```sql
WHERE order_id = $1::bigint
```

分析慢查询还要区分“单次慢”和“累计慢”。有的查询平均 5 ms，但每秒调用几千次，总耗时最高；有的查询平均 3 s，但一天只跑一次。前者可能要做缓存、批量化、减少 N+1；后者可能是报表或后台任务，需要限流、异步化或离线化。

一个比较完整的慢查询检查清单是：

```text
1. SQL 是哪一条？慢的是平均值、p99，还是某一次？
2. 参数是什么？有没有数据倾斜？
3. EXPLAIN 和 EXPLAIN ANALYZE 是否一致？
4. rows 估算和 actual rows 差多少？
5. 时间集中在哪个节点？
6. shared read 多不多？temp read/write 多不多？
7. join 算法是否合理？nested loop 的 loops 是否异常？
8. 排序、聚合、distinct 是否落盘？
9. WHERE、JOIN、ORDER BY 是否能被同一个索引顺序利用？
10. 统计信息、vacuum、表膨胀是否影响计划？
11. 改动后是否用同一组参数复测？
```

面试里可以这样回答：

```text
我会先用 pg_stat_statements、慢查询日志或 auto_explain 定位慢 SQL，然后用真实参数复现。EXPLAIN 先看 planner 估算计划，EXPLAIN ANALYZE 再看真实执行；对写语句会放在 BEGIN/ROLLBACK 里，避免提交副作用。读计划时从底层扫描节点往上看，重点对比 estimated rows 和 actual rows，看时间花在哪个节点，再结合 BUFFERS 判断是缓存命中、磁盘读取、临时文件溢出还是大量 WAL。常见问题包括统计信息不准、索引顺序不匹配、函数或类型转换导致索引用不上、nested loop 被错误选择、sort/hash 落盘、N+1 查询和读太多行。修复时只改一个变量，复测前后计划和延迟，而不是看到 Seq Scan 就机械加索引。
```

## Q037. 如何区分 CPU 慢、I/O 慢、锁等待和网络慢？

区分慢的类型，核心是把一次数据库请求拆成几段：

```text
应用排队/连接池等待
-> 网络发送 SQL
-> 数据库等待锁或资源
-> 数据库真正执行
-> 结果序列化
-> 网络返回结果
-> 应用反序列化和处理
```

用户看到的是总延迟，但数据库慢不一定都发生在数据库执行阶段。很多事故里，数据库 CPU 并不高，真正慢的是连接池打满、行锁等待、磁盘读、WAL fsync、结果集太大导致网络发送慢，或者应用端拿到结果后处理太久。

在 PostgreSQL 里，第一眼看 `pg_stat_activity`：

```sql
SELECT pid,
       state,
       wait_event_type,
       wait_event,
       now() - query_start AS query_age,
       now() - xact_start AS xact_age,
       left(query, 120) AS query
FROM pg_stat_activity
WHERE datname = current_database()
ORDER BY query_age DESC;
```

`wait_event_type` 是很有用的入口。大致可以这样判断：

```text
wait_event_type = Lock   : 大概率在等锁
wait_event_type = IO     : 大概率在等数据文件、WAL 或临时文件 I/O
wait_event_type = Client : 大概率在等客户端读写，可能是网络慢或客户端不消费结果
wait_event_type = IPC/LWLock/BufferPin : 可能是并行执行、buffer I/O、内部锁或恢复冲突
wait_event_type 为空但 state=active 且 CPU 高 : 更像 CPU 计算、表达式执行、排序/hash、JIT 或大量 tuple 处理
```

锁等待最容易确认。可以先看谁在等谁：

```sql
SELECT blocked.pid AS blocked_pid,
       blocked.query AS blocked_query,
       blocker.pid AS blocker_pid,
       blocker.query AS blocker_query,
       now() - blocked.query_start AS blocked_age,
       now() - blocker.xact_start AS blocker_xact_age
FROM pg_stat_activity blocked
JOIN pg_locks blocked_locks
  ON blocked_locks.pid = blocked.pid
 AND NOT blocked_locks.granted
JOIN pg_locks blocker_locks
  ON blocker_locks.locktype = blocked_locks.locktype
 AND blocker_locks.database IS NOT DISTINCT FROM blocked_locks.database
 AND blocker_locks.relation IS NOT DISTINCT FROM blocked_locks.relation
 AND blocker_locks.page IS NOT DISTINCT FROM blocked_locks.page
 AND blocker_locks.tuple IS NOT DISTINCT FROM blocked_locks.tuple
 AND blocker_locks.virtualxid IS NOT DISTINCT FROM blocked_locks.virtualxid
 AND blocker_locks.transactionid IS NOT DISTINCT FROM blocked_locks.transactionid
 AND blocker_locks.classid IS NOT DISTINCT FROM blocked_locks.classid
 AND blocker_locks.objid IS NOT DISTINCT FROM blocked_locks.objid
 AND blocker_locks.objsubid IS NOT DISTINCT FROM blocked_locks.objsubid
 AND blocker_locks.pid <> blocked_locks.pid
 AND blocker_locks.granted
JOIN pg_stat_activity blocker
  ON blocker.pid = blocker_locks.pid
ORDER BY blocked_age DESC;
```

更简单的版本是：

```sql
SELECT pid,
       pg_blocking_pids(pid) AS blocking_pids,
       wait_event_type,
       wait_event,
       query
FROM pg_stat_activity
WHERE cardinality(pg_blocking_pids(pid)) > 0;
```

如果 `wait_event_type = Lock`，`wait_event = transactionid` 或 `tuple`，通常是行级更新冲突。比如两个事务同时更新同一行订单状态，后来的事务要等前一个事务提交或回滚。如果 `wait_event = relation`，可能是 DDL 和 DML 冲突，例如 `ALTER TABLE` 等待长查询，或者长查询挡住 DDL，后续普通请求又排在 DDL 后面，形成锁队列放大。

I/O 慢要结合 `EXPLAIN (ANALYZE, BUFFERS)` 和统计视图看。单条查询可以看：

```sql
EXPLAIN (ANALYZE, BUFFERS)
SELECT ...
```

如果看到大量：

```text
Buffers: shared read=850000
```

说明它读了大量数据块。若开启了 `track_io_timing`，还能看到读写耗时。全局层面可以看 `pg_stat_io`：

```sql
SELECT backend_type,
       object,
       context,
       reads,
       read_bytes,
       read_time,
       writes,
       write_bytes,
       write_time
FROM pg_stat_io
ORDER BY read_time + write_time DESC
LIMIT 20;
```

`DataFileRead`、`DataFileWrite`、`WalWrite`、`WalSync`、`BuffileRead`、`BuffileWrite` 这些 wait event 往往指向不同 I/O 路径。`BuffileRead/Write` 常和临时文件有关，可能是 sort/hash 溢出。`WalSync` 常和提交、WAL 刷盘、同步提交有关。`DataFileRead` 常见于冷数据扫描、索引回表过多、缓存命中率低。

CPU 慢的表现不太一样。数据库后端处于 `active`，但没有明显 wait event；机器 CPU 高；`EXPLAIN ANALYZE` 显示 shared read 不多，却有节点耗时很长。常见原因包括：

```text
扫描了大量已在缓存中的行
复杂表达式、正则、JSON 解析、函数调用很重
排序、聚合、hash join 在内存里消耗 CPU
JIT 编译或执行成本高
并行 worker 都在跑 CPU
行数估算错误导致执行了大量无效过滤
```

CPU 慢的 SQL 例子：

```sql
SELECT *
FROM events
WHERE payload->>'user_id' = 'u123';
```

如果没有合适的表达式索引，这条 SQL 可能扫描大量 JSON 并逐行解析。它可能几乎不读磁盘，因为数据已经在 cache 里，但 CPU 会被打满。修复方向不是提高 IOPS，而是改索引或数据模型：

```sql
CREATE INDEX events_user_id_expr_idx ON events ((payload->>'user_id'));
```

网络慢或客户端慢，经常被误判成数据库慢。`wait_event_type = Client` 时要小心。`ClientRead` 表示数据库在等客户端继续发数据；`ClientWrite` 表示数据库在等客户端接收结果。常见原因：

```text
结果集太大，应用一次拉太多行
应用端处理结果很慢，不及时读取 socket
跨地域访问数据库
连接经过代理、TLS、NAT 或防火墙导致抖动
客户端连接池耗尽，请求在应用侧排队
```

一个典型现象是：数据库端执行很快，但应用日志里 SQL 调用耗时很长。此时要对比三组时间：

```text
应用埋点里的 acquire connection 时间
数据库日志里的 duration
EXPLAIN ANALYZE 的 Execution Time
```

如果连接池等待 800 ms，数据库执行 10 ms，总耗时 810 ms，那不是慢查询。如果数据库日志 duration 20 ms，应用看到 2 s，可能是网络、代理、结果读取或应用端处理。如果 `EXPLAIN ANALYZE` 20 ms，但真实接口 2 s，还要检查结果集大小、客户端分页、ORM hydration、JSON 序列化。

锁等待和慢执行也会混在一起。一条 SQL 可能先等锁 5 秒，然后执行 20 ms。`EXPLAIN ANALYZE` 在没有并发冲突的测试环境里只显示 20 ms，所以你会误以为数据库没问题。线上诊断要看等待事件、阻塞链和事务年龄：

```sql
SELECT pid,
       state,
       wait_event_type,
       wait_event,
       now() - xact_start AS xact_age,
       now() - state_change AS state_age,
       query
FROM pg_stat_activity
WHERE xact_start IS NOT NULL
ORDER BY xact_age DESC;
```

长时间 `idle in transaction` 特别危险。它可能持有锁，也可能保留旧 snapshot，阻碍 vacuum 清理，最终造成表膨胀和更多 I/O。

一个实用的判断表：

```text
现象：CPU 高，active 多，无明显 wait event，BUFFERS read 不多
判断：更像 CPU 慢
处理：看计划节点、行数、表达式、排序聚合、JSON/正则、JIT、并行度

现象：IO wait 高，BUFFERS read/temp read/write 多，wait_event=DataFileRead/BuffileRead/WalSync
判断：更像 I/O 慢
处理：减少扫描行数、补索引、避免 sort/hash 落盘、调批量写入、检查磁盘和 checkpoint

现象：wait_event_type=Lock，pg_blocking_pids 有值
判断：锁等待
处理：找 blocker，缩短事务，避免长事务里做外部调用，拆 DDL，设置 lock_timeout

现象：wait_event_type=Client，数据库 duration 和应用耗时不一致
判断：网络或客户端慢
处理：限制结果集、分页、靠近数据库部署、看连接池等待、检查代理和客户端消费速度
```

在 LogServe 这类系统里也要分清层次。比如 control plane 如果把 metadata view 查询写慢了，可能是 SQL 计划问题；如果 worker 上报或 gRPC 调用慢了，可能根本没到数据库；如果 shared log append 慢了，瓶颈可能是 fsync 策略和本地磁盘，不应该直接归因到 PostgreSQL。

面试里可以这样回答：

```text
我会先把端到端耗时拆开：连接池等待、网络、数据库等待、数据库执行、结果返回。PostgreSQL 里先看 pg_stat_activity 的 state、wait_event_type 和 wait_event。Lock 基本指向锁等待，配合 pg_locks 或 pg_blocking_pids 找 blocker；IO 要结合 DataFileRead、WalSync、BuffileRead/Write、EXPLAIN BUFFERS、pg_stat_io 判断；CPU 慢通常表现为 active、无明显 wait event、CPU 高、buffer read 不多但计划节点耗时长；ClientRead/ClientWrite 则要怀疑客户端或网络。最后用应用埋点、数据库日志和 EXPLAIN ANALYZE 对齐时间线，避免把连接池排队、网络传输或 ORM 处理误判为慢查询。
```

## Q038. 数据库复制延迟如何影响读写分离？

读写分离的基本思路是：写请求走 primary，读请求可以走 standby。问题在于，standby 看到的数据通常落后于 primary。PostgreSQL streaming replication 默认是异步的，primary 提交成功并不等于 standby 已经写入、刷盘、回放并对查询可见。这个时间差就是复制延迟。

复制链路可以拆成几段：

```text
primary 生成 WAL
-> WAL 发送到 standby
-> standby 写入 WAL
-> standby flush 到持久存储
-> standby replay WAL
-> standby 上的只读查询可见
```

读写分离真正关心的是最后一步：这次写入什么时候能被读副本查询看到。`pg_stat_replication` 里有几类 LSN 和 lag：

```sql
SELECT application_name,
       state,
       sync_state,
       sent_lsn,
       write_lsn,
       flush_lsn,
       replay_lsn,
       write_lag,
       flush_lag,
       replay_lag
FROM pg_stat_replication;
```

大致含义是：

```text
sent_lsn   : primary 已经发到该 standby 的 WAL 位置
write_lsn  : standby 已写入本地 WAL 的位置
flush_lsn  : standby 已刷盘的位置
replay_lsn : standby 已回放到数据库可见状态的位置
replay_lag : 最近事务从 primary 到 standby 可见的大致延迟
```

如果应用把刚写完的数据立刻读到 standby，就可能读不到。最典型的例子：

```text
T1: 用户更新头像，事务在 primary 提交成功
T2: 前端立刻刷新个人资料页
T3: 读请求被路由到 lag=2s 的 standby
T4: 页面仍然显示旧头像
```

这不是事务没提交，也不是缓存没刷新，而是读到了旧副本。类似问题还会出现在：

```text
下单后立刻查询订单详情，看到“订单不存在”
支付后立刻查余额，看到旧余额
修改权限后立刻访问接口，权限判断仍按旧数据
创建任务后立刻拉任务队列，队列页看不到新任务
后台迁移写入新字段后，读副本还没回放对应 schema 或数据
```

读写分离会破坏几类用户直觉。

第一类是 read-your-writes。用户写完后，自己下一次读应该看到自己的写入。异步副本不能天然保证这一点。

第二类是 monotonic reads。同一个用户第一次读到了较新的数据，下一次却被路由到更落后的副本，看到了更旧的数据，时间像倒退了一样。

第三类是 causal reads。如果 B 的展示依赖 A 已经发生，但 B 的读请求去了滞后的副本，就可能看到因果关系断裂的状态。

解决方案不是简单“复制延迟一般很小，所以没事”。工程上要把读分级。

强一致读走 primary：

```text
登录鉴权、权限变更后校验
支付、余额、库存、订单状态
刚写完后的详情页
迁移、发布、运维确认步骤
```

可接受陈旧的读可以走 standby：

```text
报表
列表页粗略统计
搜索索引辅助信息
运营后台非实时看板
历史只读查询
```

还有一种做法是 session stickiness。某个用户完成写入后，在一个短窗口内把这个用户的读请求也路由到 primary：

```text
write success -> mark session as primary-read for 3s -> after window expires allow replica
```

这种方案简单，但比较粗。更精确的是用 LSN token。写事务提交后记录一个 WAL 位置，然后读副本只有追到这个位置才允许承接该用户的读：

```sql
-- 写后在 primary 获取当前位置，作为客户端或 session 的一致性水位
SELECT pg_current_wal_lsn();
```

读之前在 standby 检查：

```sql
SELECT pg_last_wal_replay_lsn();
```

如果 standby 的 replay LSN 已经不小于用户携带的 LSN，就说明它至少回放到了那次写入附近，可以读；否则要么短暂等待，要么回退到 primary。这个方案比固定等待 1 秒更可靠，因为它基于复制进度而不是时间猜测。

复制延迟还会影响故障切换。PostgreSQL 官方文档明确说明，异步 streaming replication 下，如果 primary 崩溃，有些已经提交的事务可能还没复制到 standby；故障切换时可能丢失的数据量和当时复制延迟有关。也就是说，读写分离里的“延迟读”问题，在 failover 时会变成 RPO 问题。

同步复制可以降低这类风险，但它不是免费午餐。`synchronous_commit` 和同步 standby 配置会让提交等待某个复制阶段确认。等待到 `remote_write`、`on`、`remote_apply` 的语义不同：

```text
remote_write : standby 已写 WAL，但不一定 flush 或 apply
on           : standby 已 flush WAL
remote_apply : standby 已 apply，查询可见
```

越接近 `remote_apply`，读副本可见性越强，写延迟也越高。跨可用区、跨地域时，这个成本很明显。同步 standby 不健康时，写事务还可能被同步复制等待拖住。高可用设计要在 RPO、读一致性和写延迟之间明确取舍。

standby 读还有一个容易忽略的问题：长查询会和 WAL replay 冲突。Hot standby 上的只读查询如果阻碍了主库已经发生的 WAL 操作，比如 VACUUM cleanup、DDL、Access Exclusive lock 对应的变更，standby 不能无限等，因为它越等越落后。PostgreSQL 会根据配置取消冲突查询或延迟回放。`hot_standby_feedback` 可以减少某些冲突，但会让 primary 保留更多旧行版本，带来 bloat 风险。

读写分离的监控至少要有：

```text
每个 standby 的 replay_lag
primary 和 standby 的 LSN 差距
standby 是否处于 streaming
复制 slot 保留 WAL 是否导致磁盘增长
standby 查询冲突和取消次数
应用读请求被路由到 primary/replica 的比例
写后读 fallback 到 primary 的次数
```

应用层也要设计降级策略：

```text
lag 小于阈值：允许普通读走 standby
lag 超过阈值：只让可陈旧读走 standby，强一致读走 primary
lag 严重或 standby 断流：摘掉该 standby
用户写后读：携带 LSN token 或短期 stickiness
迁移期间：schema 相关读优先走 primary，确认 standby replay 后再放开
```

面试里可以这样回答：

```text
复制延迟会让读写分离失去默认的 read-your-writes 保证。写事务在 primary 提交成功后，standby 可能还没 replay 到对应 WAL，所以用户刚写完就读副本，可能看到旧数据或查不到新行。PostgreSQL 可以通过 pg_stat_replication 观察 sent/write/flush/replay LSN 和 replay_lag；真正对读可见性有意义的是 replay_lsn/replay_lag。工程上要把读分级：强一致读和写后读走 primary，允许陈旧的报表或列表读走 standby；更精确的做法是写后携带 WAL LSN，只有副本 replay 到该 LSN 后才读，否则等待或回退 primary。同步复制能降低数据丢失和可见性延迟，但会增加提交延迟，remote_write、flush、remote_apply 的语义也不同。读写分离必须配合延迟监控、路由降级和故障切换策略，不能只靠“副本一般很快”。
```

## Q039. 主从切换如何影响事务语义？

主从切换更准确地说是 primary/standby failover 或 switchover。它影响的不是 SQL 隔离级别本身，而是“哪个提交进入了新的系统历史”“客户端看到的提交结果是否可靠”“旧 primary 会不会继续接受写入”。这几个问题处理不好，就会出现丢事务、重复执行业务动作、split brain、读到回退状态。

先看异步复制下最典型的时间线：

```text
T1: client 在 old primary 上执行事务
T2: old primary 写 WAL，本地 COMMIT 成功
T3: old primary 向 client 返回成功
T4: WAL 还没传到 standby 或还没 replay
T5: old primary 崩溃
T6: standby 被 promote 成 new primary
```

这时客户端已经看到了提交成功，但新 primary 的历史里可能没有这笔事务。PostgreSQL 官方同步复制文档也说明，默认异步 streaming replication 下，primary 崩溃时，已经提交的事务可能尚未复制到 standby，故障切换时数据丢失量和复制延迟相关。

这就是 RPO 问题。数据库没有魔法把没有复制过去的 WAL 变出来。应用层必须接受：异步复制 + failover 的语义不是“所有 acknowledged commit 都一定存在于新 primary”。如果业务不能接受，就要使用同步复制、降低 RPO，或者把关键写入放在更强确认路径上。

同步复制能改善这个问题，但要说清楚等待到哪一步。常见提交确认强度可以这样理解：

```text
local        : 本地提交确认，不等待 standby
remote_write : 等 standby 写入 WAL，但不一定 flush/apply
on           : 等 standby flush WAL
remote_apply : 等 standby apply 后才确认，对同步 standby 查询可见
```

如果要求 failover 后尽量不丢已经确认的事务，至少要让提交等待同步 standby 的 WAL 持久化。但这会增加写延迟，还会引入同步 standby 不可用时的提交等待问题。高可用不是“打开同步复制就结束”，还要设计 quorum、超时、降级和告警。

切换还会制造 unknown commit。客户端执行 `COMMIT` 时连接断了：

```text
client -> COMMIT
network broken
client timeout
primary might have committed, might not
```

客户端不能简单说“没收到成功，所以重试整个业务动作”。如果重试的是扣款、创建订单、发送消息，就可能重复。正确做法是用幂等键、业务唯一约束或事务状态表确认结果：

```sql
CREATE TABLE request_dedup (
    request_id text PRIMARY KEY,
    status text NOT NULL,
    result_ref text,
    created_at timestamptz NOT NULL DEFAULT now()
);
```

业务事务里先写入 `request_id`，后续重试按同一个 `request_id` 查询结果。这样客户端遇到 failover、超时、断线时，不需要猜提交是否成功。

主从切换还会让所有连接上的事务中断。原来连接 old primary 的会话、事务、临时表、prepared statement、advisory lock、session 变量都不能假设还存在。应用要做的是：

```text
捕获连接断开和 read-only 错误
丢弃旧连接，重新从连接池建立连接
重新设置 session 参数
重试可重试事务
对不可重试业务动作按幂等键查询结果
```

不要试图让一个数据库事务跨 failover 延续。事务要么已经进入新 primary 的历史，要么没有；中间连接状态不属于可恢复语义。

另一个严重问题是 split brain。old primary 崩溃后，如果 standby 被提升为 new primary，而 old primary 又恢复并继续接受写入，就会有两个 primary。PostgreSQL 官方 failover 文档明确强调，old primary 重启时必须有机制让它知道自己不再是 primary，否则两个系统都认为自己能写，会导致混乱和数据丢失。工程上常见做法包括：

```text
fencing / STONITH，确保旧 primary 不能继续写
VIP、DNS、代理或服务发现只指向新 primary
旧 primary 恢复后先隔离，再用 pg_rewind 或重建为 standby
failover manager 要有多数派或 witness，避免网络分区时误提升
```

切换后还会产生 timeline 变化。standby promote 后会进入新的 WAL timeline；其他 standby 要跟随新的 primary。原 primary 如果没有按新 timeline 修复，不能直接重新加入。对应用来说，这通常被 failover 工具屏蔽，但数据库运维要知道：failover 不是普通重启，它改变了复制拓扑和历史分支。

读语义也会受影响。假设用户在 old primary 上写入 A，随后系统 failover 到没有 A 的 standby。用户刷新页面发现 A 不见了。这不是隔离级别问题，而是异步复制下 acknowledged commit 没有进入新 primary。对于用户体验，系统可能需要：

```text
在关键写入上使用同步提交
在故障恢复后做业务补偿或对账
把外部副作用和数据库提交通过 outbox/idempotency 绑定
对可能重复的回调、消息、任务执行做去重
```

迁移和 DDL 在 failover 中也要谨慎。比如 migration 在 old primary 上完成了一半，standby promote 时只 replay 了一部分 WAL，新的 primary 可能处于某个中间 schema 状态。迁移系统必须能根据 schema_migrations 表、对象真实状态和 checksum 继续向前修复，而不是盲目执行反向脚本。这和前面说的 migration 幂等性是一套问题。

对 LogServe 这类项目，边界要说清楚：当前项目是单机/多进程机制验证，核心状态来自 shared log 和可 replay 的 materialized view，不应该把 PostgreSQL failover 写成已经完成的生产级能力。如果面试被问到生产化，可以说数据库层要补 primary/standby、连接路由、幂等请求表、迁移状态表和 failover 演练；LogServe 自身的 log-first 语义只能解决项目内部状态重放，不能替代数据库集群的高可用协议。

面试里可以这样回答：

```text
主从切换会影响“哪些事务进入新主库历史”。异步复制下，旧主库已经返回 COMMIT 成功的事务，如果 WAL 还没被 standby replay，故障切换后可能在新主库上消失，所以 RPO 取决于复制延迟。同步复制可以降低这个风险，但 remote_write、flush、remote_apply 的确认语义和写延迟不同。客户端在 COMMIT 时断线还会遇到 unknown commit，不能盲目重试非幂等业务，必须用 idempotency key、唯一约束或状态表确认。切换会断开连接，原会话里的事务、临时表、prepared statement 和 advisory lock 都不能保留。最危险的是 split brain，所以旧主恢复时必须 fencing、rewind 或重建，不能继续接受写入。应用层要把 failover 当成正常故障路径：重连、重试可重试事务、确认未知提交、用幂等和 outbox 处理外部副作用。
```

## Q040. 如何为数据库层设计高可用？

数据库高可用先要问目标，不要一上来堆组件。至少要明确：

```text
RTO：故障后多久恢复服务？
RPO：最多允许丢多少已提交数据？
读一致性：读副本能不能陈旧？
写可用性：同步副本不可用时，是阻塞写入还是降级？
故障范围：进程崩溃、机器故障、磁盘损坏、可用区故障，还是区域级故障？
运维能力：团队能否值守、演练、恢复备份、处理 split brain？
```

不同答案会导向不同设计。金融扣款和后台报表不是一套要求。一个只要求读扩展的系统，hot standby 足够；一个要求主库故障后不丢已经确认的关键事务，就要同步复制或更严格的提交路径；一个要求跨地域容灾，就要接受更高延迟或更复杂的异步补偿。

一个常见 PostgreSQL 高可用架构是：

```text
           client / service
                  |
          connection router
        /                     \
 primary PostgreSQL      read-only standbys
        |
 WAL archive / backups / PITR
        |
 failover manager + fencing + monitoring
```

核心是单写主库。PostgreSQL 原生的物理复制适合这种模型：一个 primary 接受写入，一个或多个 standby 接收 WAL；hot standby 可以承担读查询；primary 故障时 promote 某个 standby。这个模型简单、成熟，但不是自动完整 HA。PostgreSQL 文档也说明，它不提供检测 primary 故障并通知 standby 的系统软件；这些要靠外部 failover 工具、操作系统能力和运维流程。

第一层是复制。至少要有一个 standby，最好跨机器、跨机架或跨可用区。异步复制延迟低、对写入影响小，但 failover 时可能丢失尚未复制的事务。同步复制能降低 RPO，但每次提交要等 standby 确认，写延迟至少增加网络往返和远端 WAL 处理时间。工程上可以按业务拆：

```text
普通低风险写入：异步或 local commit
关键资金/权限/订单状态：同步提交或业务级确认
报表读扩展：异步 standby
强一致写后读：primary 或 remote_apply 语义的同步副本
```

第二层是故障检测和提升。一个可靠 failover manager 至少要做几件事：

```text
判断 primary 是否真的不可用，而不是短暂网络抖动
选择最合适的 standby promote
确保 old primary 被 fencing，不能继续写
更新连接入口，例如 VIP、DNS、代理、服务发现或连接字符串
让其他 standby 跟随新 primary
记录切换事件，支持审计和回滚分析
```

这里最怕 split brain。两台机器同时认为自己是 primary，比短暂停机更危险。为了避免误切换，常见设计会引入 quorum、witness、租约或外部一致性存储。只有两台数据库节点时尤其要谨慎：网络分区时，standby 看不到 primary，不代表 primary 已经死了。

第三层是连接路由。应用不应该把 primary 地址写死在配置里。可以用：

```text
VIP
DNS + 短 TTL
HAProxy / PgBouncer / Envoy
云厂商数据库 endpoint
服务发现
```

路由层要区分读写。写请求只能进 primary；读请求可以按一致性要求分流到 standby。failover 后，连接池里旧连接要丢弃，应用要重新建连并重试可重试事务。只改 DNS 但应用连接池长期持有旧连接，切换仍然会失败。

第四层是数据保护。HA 不是备份。主备复制会把误删、错误 migration、坏数据很快复制到 standby。必须有：

```text
定期 base backup
连续 WAL archive
PITR 恢复能力
备份加密和权限控制
恢复演练
备份可用性监控
```

如果没有做过恢复演练，备份只能算“看起来存在”。真正的高可用要能回答：某天 10:32 误删一张表，能不能恢复到 10:31？恢复到哪里？恢复要多久？谁执行？应用如何切过去？

第五层是复制和存储监控。至少要告警：

```text
standby 是否 streaming
replay_lag / flush_lag / LSN bytes lag
replication slot 保留 WAL 是否导致磁盘逼近上限
WAL archive 是否失败
checkpoint、WAL sync、DataFileRead/Write 是否异常
连接数、锁等待、长事务、idle in transaction
主备角色和 timeline 是否符合预期
```

复制 slot 是常见坑。它能防止 standby 掉线时 primary 过早删除 WAL，但 standby 长时间不可用时，slot 会让 primary 保留越来越多 WAL，直到磁盘打满。高可用系统不能只监控“副本还在不在”，还要监控“因为副本不在，主库会不会被拖死”。

第六层是数据库变更策略。很多数据库事故不是机器坏了，而是 DDL 或 migration 把线上锁死。高可用设计必须包含在线迁移规范：

```text
expand-contract
CREATE INDEX CONCURRENTLY
分批 backfill
lock_timeout / statement_timeout
NOT VALID / VALIDATE CONSTRAINT
迁移幂等和 checkpoint
发布回滚兼容
```

DDL 也会通过 WAL 复制到 standby。大表索引、批量回填、VACUUM、长事务都会影响复制延迟。迁移期间读写分离策略也要更保守，避免新代码读到尚未 replay 的 schema 或数据。

第七层是应用事务语义。数据库 failover 一定会让部分请求失败、超时或结果未知。应用必须有：

```text
事务重试框架，处理 serialization failure、deadlock、连接断开
幂等键，处理 unknown commit 和重复请求
业务唯一约束，防止重复创建
outbox，保证数据库提交和外部消息发送能恢复
合理的 statement_timeout、lock_timeout、idle_in_transaction_session_timeout
```

没有这些，数据库层切得再快，业务层也会出现重复扣款、重复发消息、订单状态悬挂。

第八层是读副本策略。读副本不是越多越好。每个副本都会消耗 WAL 发送、保留、监控和故障处理成本。读路由要根据一致性分级：

```text
强一致读：primary
写后读：primary 或 LSN 等待
陈旧可接受读：standby
长报表：专门报表副本，避免拖慢普通读副本
延迟过高副本：自动摘除
```

Hot standby 上的长查询可能和 WAL replay 冲突，被取消或导致副本延迟增长。报表副本最好和低延迟在线读副本分开，不要让一个长查询影响所有读流量。

第九层是演练。纸面 HA 没有太大意义。需要定期做：

```text
primary 进程 kill
primary 机器断网
standby promote
old primary rewind / rebuild
应用重连和事务重试验证
备份恢复和 PITR
迁移失败恢复
复制延迟压测
```

演练要记录真实 RTO/RPO，而不是写“预计 30 秒”。很多系统第一次 failover 才发现连接池不会刷新 DNS、应用没有重试、旧 primary 没有 fencing、复制 slot 把磁盘撑满、备份恢复脚本没人会跑。

一个相对稳妥的设计清单：

```text
单写 primary，至少一个跨故障域 standby
根据 RPO 选择异步、同步或 quorum 同步复制
独立 failover manager，带 fencing 和防 split brain 机制
连接入口可切换，应用能丢弃旧连接并重试
读写分离带一致性分级和 lag 阈值
WAL archive + base backup + PITR，定期恢复演练
监控 pg_stat_replication、pg_stat_activity、pg_locks、pg_stat_io、磁盘和 WAL
迁移按在线变更规范执行，避免长锁和不可回滚破坏性操作
应用层有幂等、outbox、事务重试和超时控制
定期做 failover、rewind、restore、migration recovery 演练
```

面试里可以这样回答：

```text
数据库高可用要先定义 RTO、RPO 和读一致性要求。PostgreSQL 常见做法是单 primary 加一个或多个 standby，通过 streaming replication 复制 WAL；异步复制写延迟低但 failover 可能丢最近事务，同步复制能降低 RPO 但增加提交延迟。还需要外部 failover manager 做健康检查、promote、连接入口切换和 fencing，因为 PostgreSQL 本身不负责完整自动故障管理。设计时必须防 split brain，监控 replay lag、replication slot、WAL archive、锁等待、长事务和磁盘。读写分离要按一致性分级，写后读走 primary 或等待副本追到指定 LSN。HA 也不能替代备份，必须有 base backup、WAL archive、PITR 和恢复演练。最后应用层要能处理连接断开、unknown commit、事务重试和幂等，否则数据库切换成功，业务仍然可能重复执行或状态不一致。
```

## Q041. MVCC 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

MVCC 的核心目标可以压成一句话：在并发事务里给每个读操作一个规则明确的可见视图，同时尽量减少读写之间的阻塞。它首先是并发控制和事务隔离机制，主线是正确性；性能是它选择“多版本”这条路带来的直接收益。安全性和可维护性不是它的主要目标。

PostgreSQL 官方并发控制介绍里有两个点很重要。第一，数据一致性通过 multiversion model 维护，每条 SQL 语句看到的是某个时间点的数据库版本，而不是底层文件此刻最新的物理状态。第二，MVCC 避免传统读锁和写锁之间的大量冲突，让普通读不阻塞写，普通写也不阻塞读。这个说法很准确：MVCC 不是为了“读得更快”而随便复制数据，它是为了让读事务在并发写入发生时仍能看到一个可解释、可重复推理的版本。

所以如果面试问“MVCC 解决的是正确性、性能、安全性还是可维护性”，我会这样排序：

```text
第一层：正确性
  给读操作定义清楚的可见性规则，避免读到未提交数据和半成品状态。

第二层：性能
  普通读写互不阻塞，提高多用户环境下的吞吐和尾延迟。

第三层：工程折中
  用版本、事务 ID、snapshot、VACUUM 等机制，把锁竞争换成版本维护成本。

不是主目标：安全性
  MVCC 不做鉴权、审计、加密，也不防 SQL 注入。

不是直接目标：可维护性
  它让应用少写一部分手工锁逻辑，但数据库内部和运维复杂度反而增加。
```

正确性方面，MVCC 主要回答的是“这个事务现在能看见哪个版本”。例如 T1 正在更新一行余额，但还没提交；T2 做普通 `SELECT` 时不能读到 T1 的半成品版本。若 T1 最后回滚，T2 曾经读到这个版本就会破坏事务语义。MVCC 用版本可见性把这个问题挡住了。

性能方面，MVCC 避免了很多读写互斥。没有 MVCC 的简单锁模型可能是：

```text
读一行 -> 拿读锁
写一行 -> 等所有读锁释放
长查询 -> 阻塞更新
更新事务 -> 阻塞读查询
```

MVCC 的思路是：

```text
写事务创建新版本
旧 snapshot 继续读旧版本
新 snapshot 按规则读新版本
普通 SELECT 不需要等待正在更新的事务完成
```

这就是为什么它在 OLTP 系统里很有价值。读请求很多、写请求也不少，如果每次读写都互相排队，数据库很快会被锁等待拖死。

但要小心一句话：MVCC 不等于“没有锁”。PostgreSQL 文档明确说，表级锁和行级锁仍然存在。`UPDATE` 和 `DELETE` 仍然会拿行锁；两个事务更新同一行仍然要等待或冲突；DDL 仍然可能拿很重的表锁；`SELECT FOR UPDATE` 仍然是显式锁。MVCC 主要减少的是普通读和写之间的冲突，不是取消所有同步。

它也不自动提供最强隔离。PostgreSQL 的 `Read Committed` 每条语句拿一个 snapshot，所以同一事务里两次 `SELECT` 可以看到不同结果。`Repeatable Read` 使用事务级稳定 snapshot，但仍可能出现 serialization anomaly，例如写偏斜。要防这类问题，需要 `Serializable`、显式锁、唯一约束、排他约束，或者把业务不变量改造成数据库能直接保护的形式。

从实现角度看，MVCC 把“阻塞成本”换成了“版本成本”。数据库需要保存旧版本，需要判断每个版本对当前 snapshot 是否可见，需要清理 dead tuples，需要维护 visibility map，需要处理事务 ID wraparound，需要让索引、VACUUM、WAL、崩溃恢复都和版本规则对齐。这些成本不小，只是它们通常比大量读写互斥更适合高并发数据库。

一个容易答错的点是把 MVCC 说成“性能优化”。这只说了一半。MVCC 带来的性能收益来自正确的可见性模型：读者可以读旧版本，写者可以创建新版本。没有可见性规则，单纯保存多个版本只是缓存，不是并发控制。

面试里可以这样回答：

```text
MVCC 的核心目标是给并发事务定义清楚的版本可见性，让每条语句或每个事务在并发写入下仍看到一个一致的 snapshot。它主要解决事务隔离和一致性问题，同时用多版本减少普通读写之间的锁冲突，所以性能收益很明显。它不是安全机制，也不是单纯的可维护性机制。代价是数据库要维护旧版本、事务 ID、可见性判断、VACUUM、索引清理和 wraparound 防护。工程上要记住：MVCC 让读不阻塞写、写不阻塞普通读，但写写冲突、显式锁、DDL 锁、序列化失败和业务不变量仍然要单独处理。
```

## Q042. MVCC 的典型适用场景和不适用场景分别是什么？

MVCC 最适合“读很多、写也有、读写经常交错，但业务能接受用事务隔离规则表达可见性”的场景。它不适合被当成万能并发锁，也不适合用来掩盖热点写入、无限长事务和跨系统一致性问题。

典型适用场景有几类。

第一类是普通 OLTP 系统。比如订单、用户、任务、账单、权限、库存流水这类业务，大量请求是短事务：

```sql
BEGIN;
SELECT status, amount FROM orders WHERE id = $1;
UPDATE orders SET status = 'paid' WHERE id = $1 AND status = 'pending';
COMMIT;
```

这种场景里，读请求不能看到未提交更新，写请求也不能因为普通读太多就全部排队。MVCC 很合适。

第二类是读写混合的后台管理和 API 查询。用户列表、任务列表、最近运行记录、workflow 状态页这类查询，通常希望正在更新的数据不会让普通查询阻塞。PostgreSQL 的 `Read Committed` 对很多这种查询已经够用：每条语句看到语句开始时的已提交数据。

第三类是短时间的一致性读。比如一个事务里读多张表，要求看到同一个时间点的视图。`Repeatable Read` 能让事务内普通查询看到稳定 snapshot。它适合生成一份短报表、做一次一致性校验、读取多个相关对象。这里的“短”很重要。长事务会拖住 VACUUM，后面会变成运维问题。

第四类是可通过约束收敛的并发写。比如幂等请求、唯一用户名、同一业务 ID 只能创建一次。MVCC 配合唯一索引、`INSERT ... ON CONFLICT`、事务重试，效果很好。真正起保护作用的不只是 MVCC，还有约束。

第五类是需要高并发普通读的系统。MVCC 最大的实用价值就是普通读不阻塞写。一个系统里如果 95% 是读请求，5% 是更新请求，用传统粗粒度锁会很难受；MVCC 可以让读请求继续走旧版本。

不适用或要特别小心的场景也很明确。

第一类是单行热点计数器。比如所有请求都更新同一行：

```sql
UPDATE counters SET value = value + 1 WHERE name = 'global';
```

MVCC 不能让同一行的并发写入同时成功。它仍然要串行化这行更新，产生行锁等待、dead tuples、WAL 和索引维护成本。高并发计数通常要分片计数、批量聚合、异步写入，或者换专门的数据结构。

第二类是强业务不变量但没有约束表达的场景。例如“至少保留一名值班医生”“账户组总余额不能低于 0”“同一时间段最多 N 个资源被占用”。在 snapshot isolation 或 PostgreSQL `Repeatable Read` 下，两个事务可能各自看到规则成立，然后一起提交后破坏规则。这就是写偏斜。MVCC 不会自动发现所有业务不变量。要用 `Serializable`、显式锁、排他约束，或者把不变量建模到同一行/同一个唯一约束上。

第三类是长时间事务。比如一个事务打开后慢慢分页扫全表、导出大报表、等待用户操作、调用外部 HTTP。长事务会保留旧 snapshot，VACUUM 不能清理它可能看到的旧版本。线上症状通常是表膨胀、索引膨胀、磁盘增长、index-only scan 退化、autovacuum 一直跑但回收有限。

第四类是高频更新大字段或大量索引列的表。PostgreSQL 更新通常创建新 tuple；如果更新列影响索引，索引也要维护。写入压力高、autovacuum 跟不上时，MVCC 的旧版本会变成 bloat。此时要考虑减少索引、拆冷热字段、降低更新频率、批量写入或设计 append-only 表。

第五类是跨系统事务。MVCC 只在一个数据库内核的事务管理范围内工作。它不能保证“PostgreSQL 更新成功、Kafka 消息一定发送成功、对象存储文件一定存在”。这类问题要用 outbox、saga、幂等、补偿或分布式事务协议，不能说“数据库用了 MVCC 所以一致”。

第六类是对“最新值”非常敏感的读。MVCC 读到的是某个 snapshot，不一定是物理最新版本。普通查询读旧版本是正确行为，不是 bug。如果业务要求读必须阻塞直到某个写完成，就要显式锁、同步协议或读 primary 的指定 LSN，而不是指望 MVCC 自动等。

第七类是低延迟内存状态机。某些系统只需要单线程事件循环、CAS 或内存队列，不需要 SQL 事务和历史版本。硬套 MVCC 可能只是增加版本清理、内存占用和实现复杂度。

面试里可以这样回答：

```text
MVCC 适合短事务、读写混合、普通读很多、需要一致 snapshot 的 OLTP 场景。它能让普通 SELECT 不阻塞 UPDATE，UPDATE 也不阻塞普通 SELECT。它也适合配合唯一约束、ON CONFLICT 和事务重试处理幂等创建。它不适合解决单行热点写、高频全表更新、长事务报表、跨系统一致性，也不能自动保护所有业务不变量。遇到写偏斜、强约束和资源分配问题，要用 Serializable、显式锁、唯一/排他约束或重新建模。遇到长事务和写入 churn，要重点治理 VACUUM、bloat、索引数量和事务边界。
```

## Q043. MVCC 和相近概念最容易混淆的边界在哪里？

MVCC 最容易被混成四类东西：隔离级别、snapshot isolation、乐观锁、WAL。它们有关联，但不是一回事。

第一，MVCC 不是隔离级别。MVCC 是实现并发可见性的一类机制；`Read Committed`、`Repeatable Read`、`Serializable` 是数据库对事务隔离暴露出来的语义。PostgreSQL 同样基于 MVCC，但不同隔离级别拿 snapshot 的时机不同：

```text
Read Committed:
  每条语句开始时拿一个新 snapshot。

Repeatable Read:
  事务第一条非事务控制语句开始时拿 snapshot，后续普通查询复用它。

Serializable:
  类似 Repeatable Read 的 snapshot，再额外监测危险读写依赖。
```

所以不能说“用了 MVCC 就是 repeatable read”，也不能说“MVCC 天然 serializable”。PostgreSQL 文档的隔离表很清楚：`Repeatable Read` 在 PostgreSQL 中不会出现 phantom read，但仍可能有 serialization anomaly；只有 `Serializable` 禁止 serialization anomaly。

第二，MVCC 不等于 snapshot isolation。Snapshot isolation 通常指事务级稳定快照加写写冲突检测。PostgreSQL 的 `Repeatable Read` 可以理解为 snapshot isolation，但 PostgreSQL 的 `Read Committed` 也是 MVCC，只是每条语句一个 snapshot。也就是说：

```text
MVCC 是机制家族
Snapshot isolation 是一种隔离语义
PostgreSQL Repeatable Read 接近 snapshot isolation
PostgreSQL Read Committed 仍然使用 MVCC
```

这个边界非常常考。回答时最好直接说“MVCC 可以实现多种隔离级别，snapshot isolation 只是其中一种语义”。

第三，MVCC 不等于乐观锁。乐观锁通常是应用层或数据库层的冲突检测策略，例如：

```sql
UPDATE documents
SET body = $new_body,
    version = version + 1
WHERE id = $id
  AND version = $old_version;
```

如果更新行数是 0，说明版本变了，应用重试或提示冲突。MVCC 内部也可能采用“先并发执行，提交或更新时检测冲突”的味道，但它不是应用层 version 字段。应用乐观锁保护的是业务对象；MVCC 保护的是事务可见性。

第四，MVCC 不等于无锁。PostgreSQL 里普通 `SELECT` 不会和行级写锁冲突，但 `UPDATE`、`DELETE`、`SELECT FOR UPDATE` 会拿行锁；DDL 会拿表锁；唯一索引检查也会等待并发插入结果；Serializable 还会记录 predicate lock 形式的 `SIReadLock`。如果有人说“MVCC 数据库不需要锁”，基本就是概念没分清。

第五，MVCC 不等于 WAL。WAL 解决崩溃恢复和持久性：提交后如何 redo，未完成事务如何不留下永久效果。MVCC 解决并发可见性：哪个事务能看哪个版本。它们会配合，但边界不同。一个版本能不能被看见，取决于事务状态、snapshot 和可见性规则；崩溃后能不能恢复，取决于 WAL、checkpoint、redo 等机制。

第六，MVCC 不等于 event sourcing 或业务版本表。有些业务表会保存：

```text
document_id, version, content, created_at
```

这是业务层历史版本。数据库 MVCC 的旧 tuple 是内部并发控制数据，普通 SQL 不会把它当成业务历史长期保留；VACUUM 会在安全后清理旧版本。不能把 PostgreSQL 的 MVCC 当成审计日志，也不能指望它替代历史表。

第七，MVCC 不等于读副本一致性。读副本落后是复制问题，不是单机 MVCC 可见性问题。一个 standby 可能有自己的 snapshot 规则，但它看到的 WAL replay 位置落后于 primary。用户写后读不到，根因是复制延迟，不是 MVCC 失效。

第八，PostgreSQL MVCC 和 InnoDB MVCC 不能术语混用。InnoDB 有 undo log、read view、next-key lock、gap lock 等实现细节；PostgreSQL 有 tuple version、xmin/xmax、VACUUM、visibility map、HOT update 等概念。面试里可以讲通用思想，但落到具体数据库时要说清楚是哪一个实现。

一个容易判断边界的方法是问“它回答的问题是什么”：

```text
MVCC:
  当前事务能看见哪个版本？

隔离级别:
  事务之间允许哪些现象，不允许哪些异常？

Snapshot isolation:
  一个事务是否读固定 snapshot，并如何处理写写冲突？

锁:
  哪些并发操作必须等待？

WAL:
  崩溃后如何恢复提交结果？

业务版本表:
  业务上是否需要保留可查询的历史？

复制:
  另一个节点什么时候看到这次提交？
```

面试里可以这样回答：

```text
MVCC 是多版本可见性机制，不是隔离级别本身。PostgreSQL 的 Read Committed、Repeatable Read 和 Serializable 都建立在 MVCC 上，只是 snapshot 时机和冲突检测不同。Snapshot isolation 是一种事务级快照隔离语义，不能和 MVCC 完全画等号；乐观锁是应用或数据库的冲突检测方式，也不是 MVCC 本身。MVCC 也不等于无锁，写写冲突、SELECT FOR UPDATE、DDL、唯一约束和 Serializable 依然会用锁或依赖检测。WAL 管崩溃恢复，业务版本表管审计历史，读副本延迟管跨节点可见性，它们都和 MVCC 相邻，但回答的问题不同。
```

## Q044. MVCC 在高并发场景下可能出现哪些隐藏问题？

MVCC 在高并发下最危险的地方，是它把一部分等待隐藏掉了。普通读写互不阻塞，看起来很顺，但旧版本、锁等待、VACUUM、索引膨胀、重试风暴可能在后台累积。等到线上出问题时，表现通常不是“MVCC 报错”，而是磁盘涨、查询变慢、autovacuum 忙、锁等待多、p99 抖动、事务重试率升高。

第一类隐藏问题是 dead tuples 和 bloat。PostgreSQL 的 `UPDATE` 通常创建新行版本，`DELETE` 也只是让旧版本对后续事务不可见。旧版本不能立刻删除，因为可能还有老 snapshot 要读。高并发更新下，如果 VACUUM 跟不上，表和索引都会膨胀：

```text
磁盘占用持续增长
同样的索引扫描要读更多页
shared buffers 被无效页面挤占
VACUUM 越来越慢
checkpoint 和 WAL 压力变大
```

第二类是长事务拖住清理。一个 `idle in transaction` 的连接，或者一个跑了几十分钟的报表事务，可能让全局清理边界停住。后面的 UPDATE/DELETE 产生的旧版本都不能回收。很多线上 bloat 事故，根因不是写入量突然变大，而是某个连接开着事务没关。

可以用类似查询排查：

```sql
SELECT pid,
       state,
       now() - xact_start AS xact_age,
       wait_event_type,
       wait_event,
       left(query, 120) AS query
FROM pg_stat_activity
WHERE xact_start IS NOT NULL
ORDER BY xact_age DESC;
```

第三类是 index-only scan 退化。PostgreSQL 的 index-only scan 依赖 visibility map。如果页面没有被标记为 all-visible，数据库即使从索引拿到了列，也要回 heap 检查 MVCC 可见性。高频更新、VACUUM 滞后、长事务都会让 visibility map 推不动。结果是明明建了覆盖索引，线上还是大量 heap fetch。

第四类是写写冲突并没有消失。普通读不阻塞写，但两个事务更新同一行还是要竞争行锁。热点订单、热点账户、全局计数器、单行调度游标都会变成锁队列：

```sql
UPDATE scheduler_state
SET last_seq = last_seq + 1
WHERE name = 'global';
```

这类 SQL 在低并发下很好，高并发下会把所有 worker 串在一行上。MVCC 不会让同一行出现多个同时成功的最终版本。要分片、批量、用 sequence、改成 append-only，再异步汇总。

第五类是写偏斜。两个事务读同一组数据，各自更新不同的行，写写冲突检测发现不了，但业务不变量被破坏。例如医生值班例子：

```text
T1 看到 A、B 都在值班，于是让 A 下班
T2 看到 A、B 都在值班，于是让 B 下班
两者更新不同的行，都能提交
最后没人值班
```

在 PostgreSQL `Repeatable Read` 下，这类 snapshot isolation 异常需要靠 `Serializable`、显式锁或约束重构处理。

第六类是 serialization failure 或 deadlock 重试风暴。提高到 `Serializable` 后，数据库可以阻止 serialization anomaly，但方式是让某些事务失败。高并发冲突下，如果应用没有退避，所有请求立即重试，会把冲突放大：

```text
40001 serialization_failure 上升
40P01 deadlock_detected 上升
请求平均延迟不高但 p99 很差
CPU 消耗在反复执行失败事务上
```

第七类是 autovacuum 被资源限制卡住。autovacuum 不是无限强。表很大、更新很猛、成本延迟配置保守、I/O 已经打满、长事务阻塞清理时，autovacuum 可能一直追不上。此时手工 `VACUUM` 也不一定立刻解决，必须先处理长事务和写入模式。

第八类是事务 ID wraparound 风险。PostgreSQL 用事务 ID 参与可见性判断，旧事务 ID 需要冻结。VACUUM 长期无法推进时，数据库会越来越接近 wraparound 保护阈值，严重时会为了保护数据而拒绝部分写入。这不是理论问题，老系统里很常见。

第九类是唯一约束和 upsert 的隐性等待。`INSERT ... ON CONFLICT` 在 `Read Committed` 下可能等待另一个还不可见的并发事务结果。应用看到的是 insert 慢、upsert 慢，不一定看到普通锁等待。热点唯一键、幂等键冲突、重复请求风暴都会触发这个问题。

第十类是连接数太多造成快照和锁管理压力。每个活跃事务都可能影响全局 xmin、snapshot 获取、ProcArray 扫描、锁表内存和调度。PostgreSQL 不是连接越多吞吐越高。高并发应用通常要用连接池和限流，而不是让几千个业务线程直接连数据库。

线上监控要盯这些指标：

```text
pg_stat_activity 里的长事务和 idle in transaction
pg_stat_all_tables 的 n_dead_tup、vacuum_count、autovacuum_count
表和索引体积增长
pg_stat_user_indexes 的 idx_scan 和 heap fetch 情况
pg_locks 和 pg_blocking_pids
serialization_failure、deadlock_detected、lock_timeout 数量
autovacuum 日志
replication slot 滞后和 xmin 保留
```

面试里可以这样回答：

```text
MVCC 高并发下的隐藏问题主要来自旧版本和冲突被延后处理。高频 UPDATE/DELETE 会产生 dead tuples，VACUUM 跟不上就出现表和索引 bloat；长事务和 idle in transaction 会保留旧 snapshot，阻止清理推进；visibility map 滞后会让 index-only scan 退化；热点行更新仍然会行锁排队；Repeatable Read 下可能有写偏斜；Serializable 下要处理 40001 重试；upsert 和唯一约束也可能等待不可见的并发事务。更严重时还会有事务 ID wraparound 风险。治理重点是短事务、控制连接数、监控 dead tuple 和长事务、让 autovacuum 跟上写入、减少热点行和不必要索引，并在业务层准备重试和退避。
```

## Q045. MVCC 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

MVCC 在故障场景下的第一条边界是：它不负责持久性。MVCC 定义版本可见性，WAL 和事务提交日志负责崩溃恢复。数据库重启后，已提交事务要能 redo，未提交事务的版本不能变成可见数据。二者配合起来，应用看到的才是“提交的留下，没提交的不留下”。

崩溃时，一个事务可能处在几种状态：

```text
还没提交：产生过的新版本不能对外成为已提交结果
提交记录已经持久化：重启后要能恢复提交结果
客户端发出 COMMIT 后断线：客户端不知道它属于哪一种
```

最后一种就是 unknown commit。客户端超时没有收到 `COMMIT` 成功，不代表事务一定失败。网络断了、连接被切、primary 重启，都可能发生在提交前后。对非幂等业务，不能简单重试整个操作。要用 request id、唯一约束或业务状态表确认：

```sql
INSERT INTO request_dedup(request_id, status)
VALUES ($request_id, 'running')
ON CONFLICT (request_id) DO NOTHING;
```

后续重试先查这个 `request_id` 的最终状态，而不是重新扣款、重新创建订单。

超时也有边界。`statement_timeout` 取消的是语句，语句失败后当前事务通常会进入 aborted 状态，后续命令会报错，直到 `ROLLBACK`。如果用 savepoint，可以把错误限制在子事务里：

```sql
BEGIN;
SAVEPOINT s1;

-- 某条语句超时或冲突

ROLLBACK TO SAVEPOINT s1;
-- 继续事务内其他逻辑
COMMIT;
```

但 savepoint 不是万能恢复按钮。被取消的语句可能已经消耗了大量 I/O、拿过锁、产生过临时文件。它的事务效果会回滚，可系统资源压力已经发生过。

重试场景下，MVCC 暴露的典型错误包括：

```text
40001 serialization_failure
40P01 deadlock_detected
could not serialize access due to concurrent update
lock_timeout
连接断开导致事务结果未知
```

重试粒度必须是整个事务，而不是只重试失败的最后一条 SQL。原因很简单：失败事务之前读到的 snapshot、做过的判断、拿过的锁都已经不再可靠。PostgreSQL 文档在 `Repeatable Read` 和 `Serializable` 章节都强调，应用收到 serialization failure 后应该 abort 当前事务，从头重试。

崩溃重启后还有一些性能边界。比如 hint bits、visibility map、缓存状态、prepared plans、连接池状态都可能变化。数据库恢复正确性没有问题，但刚重启后查询可能变慢，因为 shared buffers 冷、visibility 信息需要重新确认、连接池集中重连。不要把这种冷启动慢误判成 MVCC 语义错误。

长事务和 prepared transaction 是另一个边界。普通事务断开会回滚，锁释放；但 two-phase commit 里的 prepared transaction 可以在崩溃后保留，等待 `COMMIT PREPARED` 或 `ROLLBACK PREPARED`。如果 prepared transaction 悬挂，它可能继续持有锁，阻碍 VACUUM，并影响事务 ID 回收。PostgreSQL 文档也明确警告，不应该让 prepared transaction 长时间停留。

会话级 advisory lock 也容易踩坑。事务回滚不会自动释放会话级 advisory lock，连接池复用连接时尤其危险。它不是 MVCC 的一部分，但经常和“数据库事务回滚了为什么还卡住”一起出现。事务级 advisory lock 更符合短事务使用习惯。

复制和 failover 会把 MVCC 的边界拉到节点外。单机上，snapshot 只讨论一个 PostgreSQL 实例里的可见性；failover 后，如果异步复制没追上，新 primary 可能没有旧 primary 已确认的一些事务。这个问题不是 MVCC 能解决的，而是复制确认语义和 RPO 问题。写后读副本读不到，也不是 MVCC 失效，而是 standby 还没 replay 对应 WAL。

一个故障场景下的处理清单：

```text
收到连接断开：丢弃该连接，不复用事务状态
收到 40001/40P01：回滚整个事务，带退避重试
收到 statement timeout：回滚事务或回滚到 savepoint，确认状态
收到 unknown commit：用幂等键或业务状态查询确认
服务重启后：预期缓存变冷，观察慢查询和连接风暴
发现 prepared transaction：尽快确认并 commit/rollback
发现长事务：先处理长事务，再期待 VACUUM 回收
```

面试里可以这样回答：

```text
MVCC 本身管版本可见性，不管持久性；崩溃恢复要靠 WAL 和事务状态。重启后，未提交版本不能变成可见数据，已提交事务要能 redo。故障边界主要在客户端视角：COMMIT 超时可能是 unknown commit，不能盲目重试非幂等业务；serialization failure、deadlock 和 concurrent update 要回滚整个事务并重试；statement_timeout 后事务可能进入 aborted 状态，需要 ROLLBACK 或 savepoint 处理。长事务、prepared transaction、会话级 advisory lock 会在故障和重启后暴露资源保留问题。复制切换时，异步副本可能没有旧主已确认事务，这是 RPO 问题，不是 MVCC 单机可见性问题。
```

## Q046. MVCC 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

MVCC 的性能瓶颈最常来自 CPU、I/O、锁竞争和后台清理之间的组合。网络通常不是单机 MVCC 本身的瓶颈，但在读写分离、分布式事务、远程存储和跨区域复制里会变成主要因素。

CPU 开销主要来自可见性判断和执行计划实际处理的行数。数据库扫描到一个 tuple 后，要判断它对当前 snapshot 是否可见。这个判断涉及事务 ID、提交状态、当前活跃事务集合、行版本头部信息等。单次判断不贵，但扫描几千万行时就很贵。高更新表还会让扫描遇到大量 dead tuples，CPU 花在“看了但不能用”的数据上。

CPU 还会浪费在错误计划上。统计信息不准时，planner 可能低估行数，选 nested loop，最后反复做索引查找和可见性判断。MVCC 旧版本多、表膨胀严重时，统计信息和实际物理代价也更容易偏离。

内存瓶颈主要有几类：

```text
shared buffers 被膨胀表和索引挤占
大量连接和活跃事务增加 snapshot 管理成本
Serializable 的 predicate lock / SIReadLock 需要内存
排序、hash join、聚合使用 work_mem，溢出后转成 I/O 问题
autovacuum、后台任务和前台查询争用缓存
```

PostgreSQL 不是连接越多越快。很多业务把连接数开到几千，结果每个连接都可能带事务状态、snapshot、锁等待和内存开销。通常需要 PgBouncer 或应用连接池，把数据库并发控制在能稳定工作的范围内。

锁竞争来自写写冲突和显式锁，不会因为 MVCC 消失。典型瓶颈包括：

```text
多个事务更新同一行
SELECT FOR UPDATE 抢任务
唯一索引热点键冲突
外键检查导致相关行锁等待
DDL 等待长查询，后续请求排队
Serializable 失败后重试过密
```

热点行尤其常见。比如任务系统用一行记录全局游标，或者所有 worker 抢同一个队列表头。即使普通读不阻塞写，写者之间仍然排队。解决方向通常是分片、批量、租约、跳过锁定行、减少单点状态，而不是调 MVCC 参数。

I/O 开销是 MVCC 最容易被低估的成本。`UPDATE` 创建新版本，`DELETE` 留下 dead tuple，VACUUM 以后再清理。这意味着：

```text
写入会产生更多 WAL
旧版本占用 heap 页面
索引可能包含无效项或空洞
VACUUM 要扫描表和索引
checkpoint 和后台写会变重
查询读到更多无效页面
```

严重 bloat 后，查询慢不一定是 SQL 写错，而是同样的逻辑数据被摊在更多物理页上。缓存命中率下降，磁盘读增加，VACUUM 也更慢，形成循环。

网络在单机 MVCC 里不是核心瓶颈。一次普通查询的版本判断都在数据库进程内部完成，不需要网络协调。但下面几种情况网络会进入主路径：

```text
同步复制等待 standby 确认
读写分离里等待副本追到某个 LSN
分布式事务或跨分片读写
应用在事务里调用远程服务
数据库和应用跨地域部署
```

这时问题已经不是“MVCC 判断慢”，而是事务边界跨出了单机内核。

排查性能瓶颈时，不能只看一个指标。一个实用判断方式：

```text
CPU 高、I/O 不高、active 查询多：
  看可见性检查、表达式、JSON、排序聚合、执行计划。

I/O 高、shared read/temp read/write 多：
  看 bloat、VACUUM、索引、顺序扫描、sort/hash 落盘。

锁等待多：
  看 pg_locks、pg_blocking_pids、热点行、DDL、SELECT FOR UPDATE。

内存压力大：
  看连接数、work_mem、hash/sort、shared buffers 命中率、predicate locks。

网络等待多：
  看同步复制、客户端读取结果、连接池、跨地域访问。
```

常见优化也要对症：

```text
CPU 型：减少扫描行数，补合适索引，更新统计信息，避免函数逐行计算。
I/O 型：治理 bloat，调 VACUUM，减少无用索引，拆冷热数据。
锁型：缩短事务，固定锁顺序，拆热点行，使用 SKIP LOCKED 或租约。
内存型：控制连接数，合理 work_mem，避免大事务，监控 Serializable 锁内存。
网络型：缩短事务内远程调用，调整复制确认级别，应用靠近数据库。
```

面试里可以这样回答：

```text
MVCC 的性能瓶颈通常不是单一维度。CPU 花在可见性判断、表达式执行和处理大量无效 tuple 上；I/O 来自旧版本、索引 bloat、VACUUM、WAL 和 checkpoint；锁竞争来自写写冲突、热点行、唯一索引、SELECT FOR UPDATE 和 DDL；内存压力来自连接数、snapshot 管理、work_mem、Serializable predicate lock 和缓存被膨胀数据挤占。网络不是单机 MVCC 的核心成本，但同步复制、读写分离、跨分片和事务内远程调用会把网络放进事务路径。优化时要先用 wait event、EXPLAIN BUFFERS、pg_stat_activity、pg_locks、表/索引体积和 VACUUM 指标定位瓶颈，再决定是改 SQL、改索引、缩短事务、拆热点还是调 autovacuum。
```

## Q047. MVCC 的 correctness test、stress test 和 benchmark 应该分别测什么？

这三类测试不要混在一起。Correctness test 测语义对不对；stress test 测高并发和故障下会不会暴露竞态；benchmark 测在明确负载下性能和资源消耗。用 benchmark 代替正确性测试，会漏掉并发异常。用 correctness test 代替 benchmark，也不知道线上能不能扛住。

Correctness test 首先要测可见性规则。一个最小 MVCC 测试至少包括：

```text
未提交写入不能被其他事务读到
事务自己的写入自己可见
Read Committed 下两条 SELECT 可以看到不同已提交版本
Repeatable Read 下同一事务内普通 SELECT 看到稳定 snapshot
删除版本对旧 snapshot 仍可见，对新 snapshot 不可见
回滚事务产生的版本不可见
提交顺序决定后续 snapshot 可见性
```

可以写成事务交错测试，而不是只跑单线程 SQL。例如：

```text
T1: BEGIN
T1: UPDATE accounts SET balance = 50 WHERE id = 1
T2: BEGIN
T2: SELECT balance FROM accounts WHERE id = 1
期望：T2 读到旧值，而不是 50
T1: COMMIT
T2: 在 Read Committed 下再次 SELECT
期望：读到 50
```

还要测并发异常边界。比如写偏斜：

```text
初始：doctor A on_call=true, doctor B on_call=true
T1: 看到有两人值班，把 A 改成 false
T2: 看到有两人值班，把 B 改成 false
```

在 snapshot isolation 下，两者可能都提交；在 Serializable 下，应该至少有一个失败。这个测试能验证你没有把 `Repeatable Read` 误当成真正串行化。

约束和冲突也要测：

```text
两个事务插入相同唯一键，只能一个成功
两个事务更新同一行，后者等待、失败或基于新版本重检条件
SELECT FOR UPDATE 能阻塞同一行更新
deadlock 能被检测并中止其中一个事务
serialization failure 后整个事务重试能成功
```

Stress test 测的是“压力下是否还能维持这些性质”。它不只是把并发开大。好的 stress test 会随机生成事务交错，加入超时、取消、回滚、连接断开、重试、长事务和后台 VACUUM。重点看：

```text
是否出现违反业务不变量的最终状态
是否出现死锁风暴或重试风暴
长事务是否导致 dead tuple 持续增长
高冲突下 p99 是否失控
autovacuum 是否追不上
连接池是否耗尽
```

可以用“模型校验”的思路。先定义一个小模型，例如账户总额不变、库存不能为负、每个任务只能被一个 worker 完成。压力测试跑完后，不只看 SQL 是否报错，还要查最终状态：

```sql
SELECT SUM(balance) FROM accounts;

SELECT task_id, count(*)
FROM task_results
GROUP BY task_id
HAVING count(*) > 1;
```

Benchmark 测的是性能。它要明确负载模型：

```text
读写比例：95/5、80/20、50/50
事务大小：单行点查、多行范围查、批量更新
冲突率：低冲突、热点 1%、热点 10%
隔离级别：Read Committed、Repeatable Read、Serializable
数据规模：是否大于内存
索引数量：写入维护成本
事务时长：短事务、混入长读事务
```

指标也要分层：

```text
吞吐：tx/s、queries/s
延迟：p50、p95、p99、max
失败：serialization_failure、deadlock、lock_timeout、retry 次数
资源：CPU、IOPS、WAL bytes、temp files、buffer hit/read
存储：表大小、索引大小、n_dead_tup、bloat 估计
维护：VACUUM 次数、耗时、回收效果、wraparound 年龄
```

对 MVCC 来说，benchmark 不能只跑 5 分钟。很多问题要写一段时间才出现，尤其是 bloat、VACUUM 和 visibility map。一个查询刚建表时很快，跑一天更新后变慢，这才是 MVCC 成本。

如果是从零实现简化 MVCC，correctness test 还应该包含内部不变量：

```text
每个版本有创建事务和删除事务
未提交事务版本不可见
已回滚事务版本不可见
snapshot 的活跃事务集合判断正确
GC 不会删除仍可能被活跃 snapshot 看到的版本
提交顺序和可见性顺序一致
```

面试里可以这样回答：

```text
Correctness test 测 MVCC 语义：未提交不可见、自己写自己可见、Read Committed 每语句 snapshot、Repeatable Read 稳定 snapshot、回滚版本不可见、同一行写冲突和唯一约束正确、Serializable 能阻止写偏斜。Stress test 测高并发交错、超时、取消、重试、长事务、VACUUM 滞后和连接池压力下，不变量是否仍成立，是否出现死锁和重试风暴。Benchmark 测性能，要固定读写比例、冲突率、隔离级别、数据规模和事务时长，记录 tx/s、p99、CPU、I/O、WAL、dead tuples、bloat、VACUUM 效果和重试率。三者不能互相替代，尤其不能用短时间吞吐 benchmark 证明 MVCC 语义正确。
```

## Q048. 如果要求从零实现一个简化版 MVCC，你会先定义哪些不变量？

从零实现 MVCC，先不要急着写索引和优化。先定义不变量。不变量不清楚，后面任何“性能优化”都可能把可见性搞坏。

最小模型可以从三个对象开始：

```text
Transaction:
  txn_id
  status: active | committed | aborted
  commit_ts 或 commit_seq

Snapshot:
  read_ts 或 visible_commit_seq
  active_txn_set

Version:
  key
  value
  created_by
  deleted_by
  next_version
```

第一个不变量：未提交版本不能被其他事务看到。

```text
如果 version.created_by 是 active 或 aborted，
除非读取者就是 created_by，
否则该 version 不可见。
```

这条保护脏读。没有它，事务回滚后，其他事务可能已经基于脏数据做了决策。

第二个不变量：事务自己的写入自己可见。

```text
如果 version.created_by == current_txn，
并且没有被 current_txn 自己删除，
则 current_txn 可以看见它。
```

否则一个事务插入一行后自己查不到，会破坏基本 SQL 直觉。

第三个不变量：snapshot 决定可见边界。

简化实现里可以用 commit sequence：

```text
version.created_by 已提交
并且 creator.commit_seq <= snapshot.visible_commit_seq
并且 version.deleted_by 不存在，或者删除事务对 snapshot 不可见
```

这条定义了“读的是哪个时间点”。`Read Committed` 可以每条语句创建新 snapshot；`Repeatable Read` 可以事务开始时创建一次 snapshot 并复用。

第四个不变量：回滚版本永远不可见。

```text
如果 created_by aborted，则版本不可见，并且可以在安全时回收。
如果 deleted_by aborted，则删除无效，旧版本仍按原规则可见。
```

这条要和崩溃恢复配合。重启后事务状态不能丢，否则可见性没法判断。

第五个不变量：同一 key 的版本链顺序明确。

```text
同一个 key 的版本按照创建顺序或 commit 顺序链接。
读取时最多返回一个可见版本。
```

如果一个 snapshot 能读到两个版本，说明版本链或删除标记错了。实际数据库实现比这复杂，但“一个 key 对一个 snapshot 至多一个当前值”这个不变量很基础。

第六个不变量：写写冲突必须有规则。

最简单的规则：

```text
同一 key 同时只能有一个未提交写者。
如果当前事务要更新的最新已提交版本，在本事务 snapshot 之后被别人改过，
则当前事务必须等待、失败或重试。
```

如果不处理写写冲突，就会丢失更新。两个事务都基于旧值 10 写成 11，最后看起来只加了一次。

第七个不变量：提交必须原子发布。

一个事务可能写多个版本。提交时不能让其他事务看到“一半版本已提交，一半版本未提交”。简化实现里可以先写事务私有版本，再用事务状态从 active 切到 committed 作为发布点。读取时通过事务状态判断版本是否可见。

第八个不变量：GC 不能删除仍可能被活跃 snapshot 看到的版本。

```text
oldest_active_snapshot = 所有活跃事务 snapshot 中最老的可见边界
只有早于该边界且不可能再被任何活跃事务读取的旧版本，才能回收。
```

这就是 VACUUM 的简化思想。删早了会让老事务读不到它应该看到的数据；删晚了会 bloat。

第九个不变量：索引必须能处理多个版本。

简化实现可以先让索引指向 key，再回表找可见版本。不要让索引直接假设一个 key 只有一个物理版本。否则更新后旧 snapshot 可能找不到旧版本。真实 PostgreSQL 里索引、HOT、visibility map 更复杂，但最小实现要先保证正确性。

第十个不变量：重试是协议的一部分。

如果实现 `Serializable` 或更强冲突检测，就要承认有些事务会失败。系统要返回明确错误，让上层重试整个事务。不要在底层偷偷重放最后一条写入，那样可能基于过期 snapshot 做错业务判断。

可以用伪代码描述可见性：

```text
visible(version, snapshot, current_txn):
  if version.created_by == current_txn:
      return version.deleted_by != current_txn

  creator = txn_table[version.created_by]
  if creator.status != committed:
      return false
  if creator.commit_seq > snapshot.visible_commit_seq:
      return false

  if version.deleted_by is null:
      return true

  if version.deleted_by == current_txn:
      return false

  deleter = txn_table[version.deleted_by]
  if deleter.status != committed:
      return true
  if deleter.commit_seq > snapshot.visible_commit_seq:
      return true

  return false
```

这段伪代码不够工业级，但面试时能说明你知道核心不是“复制一份数据”，而是围绕事务状态、snapshot 和版本链定义可见性。

面试里可以这样回答：

```text
从零实现简化 MVCC，我会先定义事务状态、snapshot 和版本链，然后写不变量。未提交和回滚版本对其他事务不可见；事务自己的写入自己可见；每个 snapshot 对同一 key 最多读到一个版本；Read Committed 每条语句拿新 snapshot，Repeatable Read 复用事务 snapshot；写写冲突必须等待、失败或重试，不能丢失更新；提交要原子发布事务的所有版本；GC 只能删除不可能被任何活跃 snapshot 看到的旧版本；索引不能假设一个 key 只有一个物理版本；Serializable 或冲突检测失败要让上层重试整个事务。先把这些不变量测透，再谈 HOT、visibility map、索引优化和 vacuum 策略。
```

## Q049. MVCC 的常见误用是什么，误用后通常会产生什么线上症状？

MVCC 的误用通常不是语法错误，而是把它的保证想得太强。线上症状也不一定直接指向 MVCC，更多表现为延迟抖动、磁盘增长、偶发一致性 bug、锁等待和重试率升高。

第一种误用：认为“用了事务就不会有并发 bug”。事务有隔离级别。PostgreSQL 默认是 `Read Committed`，同一事务里两次普通 `SELECT` 可以看到不同的已提交数据。复杂读改写逻辑如果依赖“我刚才查到的集合不会变”，就可能出错。

症状：

```text
偶发重复创建
状态机跳过中间状态
列表统计和详情不一致
余额、库存、名额这类业务偶发不满足预期
```

第二种误用：把 `Repeatable Read` 当成真正 Serializable。PostgreSQL 的 `Repeatable Read` 很强，不会出现不可重复读和 phantom read，但仍可能有 serialization anomaly。写偏斜就是典型例子。

症状：

```text
每个事务单独看都合理
日志里没有明显写同一行冲突
最终状态违反跨行不变量
问题很难复现，通常只在高并发下出现
```

第三种误用：长事务里做慢操作。比如事务开始后调用远程服务、等待用户输入、跑大报表、批量处理几千万行。MVCC 会为了它保留旧版本。

症状：

```text
pg_stat_activity 里有很老的 xact_start
n_dead_tup 持续升高
autovacuum 运行频繁但回收有限
表和索引体积增长
index-only scan heap fetch 增多
磁盘告警
```

第四种误用：热点行抢锁。用一行保存全局状态、全局计数器、全局队列游标，然后让所有并发请求更新它。

症状：

```text
wait_event_type=Lock
pg_blocking_pids 能看到阻塞链
吞吐随着并发增加不升反降
p99 高，CPU 和 I/O 不一定满
deadlock 或 lock_timeout 偶发
```

第五种误用：用应用先查再插入实现唯一性。

```sql
SELECT id FROM users WHERE email = $1;
-- 没有就 INSERT
```

两个事务可以同时查不到，然后一起插。正确做法是唯一约束加 `INSERT ... ON CONFLICT`。MVCC 不会替你把“先查再插”的业务逻辑变成原子操作。

症状：

```text
重复用户、重复订单、重复任务
补偿脚本越来越多
线上偶发 unique violation
重试后又产生另一个重复副作用
```

第六种误用：把 MVCC 当审计历史。数据库内部旧版本会被 VACUUM 清理，不能当业务历史记录。需要审计就建审计表、事件表或 CDC。

症状：

```text
想追历史值追不到
误以为能恢复某行旧版本
排障只能看应用日志或 WAL 归档，成本很高
```

第七种误用：忽视 VACUUM。高更新表不调 autovacuum，不监控 dead tuple，不处理长事务。

症状：

```text
同样 SQL 越跑越慢
表大小远超有效数据量
VACUUM 后不释放磁盘但性能略恢复
REINDEX 后索引明显变小
接近事务 ID wraparound 告警
```

第八种误用：在事务里做外部副作用。比如事务里发消息、调 HTTP、写对象存储。事务回滚不了这些外部动作，超时后还可能不知道数据库是否提交。

症状：

```text
消息发了但数据库回滚
数据库提交了但消息没发
重试造成重复通知、重复扣款、重复任务
排障时数据库状态和外部系统状态对不上
```

第九种误用：不处理 40001 和 40P01。很多应用把 serialization failure 和 deadlock 当成不可恢复错误，直接返回 500。高并发时这类错误是正常控制流的一部分。

症状：

```text
高峰期 500 增多
日志里有 serialization_failure 或 deadlock_detected
重试没有退避，失败率被放大
用户重复提交，业务侧又出现幂等问题
```

第十种误用：读副本上做强一致读。用户写 primary 后立刻读 standby，结果读到旧数据。这是复制延迟问题，但经常被误以为是 MVCC 隔离问题。

症状：

```text
刚创建的数据详情页 404
刚修改的权限不生效
刷新几秒后又好了
主库查询正常，副本查询落后
```

治理思路很直接：

```text
用约束表达唯一性和不变量
强一致逻辑用 Serializable、显式锁或重新建模
事务保持短小，不在事务里做远程调用
热点写拆分或批量化
处理 serialization/deadlock 重试
监控 VACUUM、bloat、长事务和复制延迟
外部副作用走 outbox 和幂等
```

面试里可以这样回答：

```text
MVCC 常见误用是把它的保证想得太强：以为事务自动防所有并发 bug，把 Repeatable Read 当 Serializable，用先查再插替代唯一约束，在长事务里做 RPC 或报表，把内部旧版本当审计历史，不处理 VACUUM 和 bloat，不处理 40001/40P01 重试，以及在读副本上做写后强一致读。线上症状通常是偶发重复数据、跨行不变量被破坏、p99 抖动、锁等待、表和索引膨胀、autovacuum 追不上、磁盘增长、重试风暴、写后读旧数据。修复不是关掉 MVCC，而是把业务不变量交给约束/锁/Serializable，缩短事务，治理热点和 bloat，并把幂等、outbox、重试退避补齐。
```

## Q050. MVCC 在单机和分布式环境中的语义有什么差异？

单机 MVCC 的边界在一个数据库实例内。所有事务状态、版本可见性、锁管理、WAL、checkpoint、VACUUM 都由同一个内核协调。分布式环境里，数据分布在多个节点，snapshot、提交顺序、故障恢复和复制延迟都跨节点。这个差异很大，不能把单机 MVCC 的直觉直接搬过去。

单机 PostgreSQL 里，一个事务读哪个版本，主要由本实例的 snapshot 和事务状态决定：

```text
当前有哪些事务 active
哪些事务已经 committed 或 aborted
当前语句或事务的 snapshot 是什么
目标 tuple 的 xmin/xmax 对这个 snapshot 是否可见
```

这些判断都在本机共享状态里完成。事务提交时，本地 WAL 和事务状态提供恢复基础。VACUUM 也可以根据本机活跃事务和复制相关保留边界判断哪些旧版本能清理。

分布式环境里，至少多出几类问题。

第一，snapshot 怎么定义。一个跨分片事务如果读 shard A 和 shard B，需要一个全局一致的读时间点。否则可能在 A 上读到新版本，在 B 上读到旧版本，组合起来不是任何真实时刻的系统状态。解决方案可能是全局时间戳、混合逻辑时钟、中心化 timestamp oracle、2PC 协调，或者只提供较弱的一致性。

第二，提交顺序怎么定义。单机可以用本地事务提交顺序；分布式系统要决定跨节点事务的全局序。两个事务分别在不同分片提交，如果它们有因果关系，系统要不要保证读者看到这个因果顺序？这不是本地 MVCC 自动能解决的。

第三，故障恢复不再只是本地 WAL。跨两个分片的事务可能在 A prepared、B 未 prepared 时协调者崩溃；也可能 A commit 成功、B 网络超时。要保证原子提交，需要 2PC、事务协调器、决策日志和恢复协议。要避免阻塞，又可能要接受 saga、补偿或更弱语义。

第四，复制延迟会影响可见性。单机上“提交后新 snapshot 能看到”比较直观；读副本上不一定。异步副本可能还没 replay WAL，多地域副本可能落后几百毫秒甚至几秒。写后读、单调读、因果读都要应用或数据库协议额外保证。

第五，时钟和顺序变成问题。单机不需要相信物理时钟来判断事务顺序；分布式系统如果用时间戳做 MVCC，就要处理时钟漂移、等待不确定性窗口、节点重启后时间回退、跨地域延迟。某些系统会用 TrueTime 一类机制，某些系统用逻辑时钟或集中分配时间戳。无论哪种，都不是“多保存几个版本”这么简单。

第六，GC 更难。单机 VACUUM 只要知道本机最老 snapshot 和相关复制保留边界。分布式 MVCC 要知道所有可能读取旧版本的节点、事务、备份、CDC、复制流是否还需要这些版本。一个慢节点或离线副本可能拖住全局 GC，导致版本堆积。

第七，隔离级别名称可能相同，语义不一定相同。很多分布式数据库会提供 snapshot read、read committed、serializable、strict serializable 等选项，但实现差异很大。有的只保证单分片事务强一致；有的跨分片事务要显式开启；有的读默认走 follower，可能陈旧；有的 serializable 不是 strict serializable。面试里最好问清楚具体系统。

单机和分布式可以这样对比：

```text
单机 MVCC:
  一个事务管理器
  一个锁管理器
  一个 WAL 顺序
  本地 snapshot
  本地 VACUUM/GC
  故障恢复主要靠本地 WAL

分布式 MVCC:
  多个事务参与者
  可能有全局时间戳或协调器
  跨分片提交协议
  副本延迟和读路由
  分布式 deadlock 或重试
  GC 受慢节点、长事务、复制和备份影响
  网络分区下要在一致性和可用性之间取舍
```

对应用来说，最实际的差异是事务边界。单机里可以把强一致数据放在一个数据库事务里；分布式里，如果一个业务不变量跨分片、跨服务、跨数据库，就要明确它靠什么保证：

```text
跨分片 serializable transaction
2PC
唯一键路由到同一分片
全局约束服务
outbox + saga
幂等补偿
最终一致
```

不能简单说“底层都是 MVCC，所以事务一样”。MVCC 只是版本可见性机制，跨节点原子性、线性化、因果一致、读你所写、全局约束都需要额外协议。

在 LogServe 这类单机机制验证项目里，也要说清楚边界。LogServe 的 shared log、replay、materialized view 可以帮助项目内部状态恢复和可解释，但它不是一个分布式 MVCC 数据库。若未来把 metadata store 做成 PostgreSQL 主备或分片系统，就要分别处理数据库事务、复制延迟、failover、幂等、outbox 和应用级恢复。

面试里可以这样回答：

```text
单机 MVCC 的语义边界在一个数据库实例内，事务状态、snapshot、锁、WAL 和 VACUUM 都由同一个内核协调。分布式环境要额外解决全局 snapshot、跨分片提交顺序、原子提交、复制延迟、时钟、网络分区和全局 GC。单机里提交后本地新 snapshot 可以按规则看到新版本；分布式读副本或跨分片读可能看到不同进度的数据。跨节点事务要靠 2PC、全局时间戳、事务协调器或更弱的 saga/outbox 语义。隔离级别名字相同也不代表行为相同，必须看具体系统是否支持跨分片 serializable、是否 strict serializable、读是否可能陈旧。MVCC 是基础机制，不自动给分布式系统提供全局一致性。
```

## Q051. serializable 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

`Serializable` 的核心目标是正确性。更具体地说，它要求所有成功提交的并发事务，结果等价于这些事务按某个顺序一个一个执行。这个定义里有两个词很关键：成功提交、等价。它不承诺每个事务都能提交，也不承诺事务真的按单线程执行；它承诺的是最终提交历史可以解释成某个串行顺序。

PostgreSQL 官方文档对 `Serializable` 的描述很直接：它是最严格的事务隔离级别，模拟所有已提交事务串行执行的效果。PostgreSQL 的实现不是给所有读写加大锁，而是在 `Repeatable Read` 的 snapshot 基础上监测会导致不可串行化的读写依赖。发现危险依赖时，它会让某个事务失败，返回 `40001 serialization_failure`。所以 `Serializable` 的正确用法从来不是“打开后所有并发问题自动消失”，而是“打开后成功提交的事务可按串行顺序推理，失败事务由应用整体重试”。

如果按目标排序，我会这样分：

```text
第一目标：正确性
  防止 serialization anomaly，让提交结果能对应到某个串行执行顺序。

第二目标：降低应用推理复杂度
  业务只需要证明单个事务串行执行时正确，再统一处理重试。

不是主要目标：性能
  Serializable 会增加依赖监测、predicate lock、回滚和重试成本。

不是安全性目标：
  它不做权限控制、加密、审计，也不防注入。

不是持久性目标：
  崩溃后提交结果能不能恢复，是 WAL 和提交语义的问题。
```

为什么说它主要解决正确性？看一个经典写偏斜场景：

```text
规则：至少有一名医生值班。

T1 读取 A、B 都在值班，于是让 A 下班。
T2 读取 A、B 都在值班，于是让 B 下班。
两个事务更新不同的行。
```

在普通 snapshot isolation 下，两个事务可能都能提交，因为它们没有写同一行。最后 A、B 都不值班，业务规则破坏了。`Serializable` 要阻止的是这种“每个事务单独看都合理，但组合起来不可能对应任何串行顺序”的结果。

这也是它和显式锁的差别。显式锁要求你提前知道要锁哪一行、哪一段范围、哪个表；`Serializable` 让数据库记录读写依赖并在发现不可串行化组合时中止事务。它通常能让业务代码少写一部分锁协议，但代价是必须接受失败和重试。

性能方面，`Serializable` 不一定总比显式锁慢。PostgreSQL 文档也指出，在某些环境里，和显式大锁或大量 `SELECT FOR UPDATE` 相比，`Serializable` 反而可能是更好的选择。原因很简单：它允许事务先并发运行，只在确认危险依赖时回滚其中一个，而不是一开始就阻塞所有潜在冲突。但这不是免费午餐。监测依赖有 CPU 和内存成本，失败事务重试也会消耗吞吐。

一个容易答错的点是把 `Serializable` 当成“串行执行”。真正串行执行是同时只跑一个事务，吞吐会很差。`Serializable` 是隔离语义：并发执行可以存在，只要提交结果等价于某个串行顺序。PostgreSQL 的 Serializable Snapshot Isolation 就是这个思路。

另一个容易答错的点是把 `Serializable` 当成“最新读”或“实时顺序”。SQL 里的 serializable 通常只要求存在某个串行顺序，不一定要求这个顺序严格符合真实时间。分布式系统里常说的 `strict serializable` 或 linearizability 更强，后面会单独讲。

面试里可以这样回答：

```text
Serializable 的核心目标是正确性：让所有成功提交的并发事务结果等价于某个串行执行顺序。它主要防 serialization anomaly，比如写偏斜这类在 snapshot isolation 下可能出现的跨行不变量破坏。PostgreSQL 的 Serializable 不是简单把事务串行跑，也不是给所有读写加阻塞锁，而是在 Repeatable Read 的 snapshot 基础上监测危险读写依赖，必要时让事务失败并返回 40001。它能简化业务推理，但应用必须整体重试失败事务。性能、安全性和可维护性都不是它的主目标；性能上还要付出依赖监测和重试成本。
```

## Q052. serializable 的典型适用场景和不适用场景分别是什么？

`Serializable` 适合“业务不变量很重要、并发交错很难手工锁对、事务可以重试”的场景。它不适合“不能重试、事务很长、冲突极高、包含外部副作用、或者只是普通低风险读写”的场景。

典型适用场景有几类。

第一类是跨多行、多表的不变量。比如：

```text
至少保留一名值班医生
一个账户组总余额不能为负
同一资源在时间区间内不能超卖
审批额度不能超过预算池剩余量
任务图状态不能出现已完成父节点但缺失必要子结果
```

这些规则不一定能靠单行锁保护。你可以显式锁整张表或手工锁范围，但代码容易写错，吞吐也可能很差。`Serializable` 的价值在于：只要每个事务在单独执行时维护规则，所有相关事务都用 `Serializable`，成功提交的结果就能按串行顺序解释。

第二类是开发阶段需要先保证语义的系统。复杂业务刚上线时，与其用一堆不完整的锁协议赌并发正确性，不如先用 `Serializable + retry` 把正确性边界立住。等热点和冲突模式清楚后，再针对性能瓶颈做局部优化。

第三类是冲突率不高但一旦出错代价很高的写路径。比如余额冻结、库存扣减、权限变更、唯一业务动作确认。只要事务短、重试代价可控，`Serializable` 很适合。

第四类是只读一致性校验。PostgreSQL 支持 `SERIALIZABLE READ ONLY DEFERRABLE`。这类事务可以等待一个安全 snapshot，读到后就不容易因为可串行化冲突失败。适合做一致性检查、审计报表、发布前校验。当然，它可能在开始时等待，不适合对延迟非常敏感的在线请求。

不适用场景也很明确。

第一类是不可重试事务。比如事务中已经发了短信、扣了外部支付、调用了不可幂等 HTTP、写了对象存储并通知用户。`Serializable` 可能在提交时失败；如果外部副作用已经发生，数据库回滚不能撤销它。正确做法是把外部副作用移出事务，用 outbox、幂等键和后台 worker 处理。

第二类是高冲突热点写。比如所有请求更新同一个余额行、全局计数器、队列头、租约表单行。`Serializable` 不能让热点消失。高冲突下它可能变成大量 `40001`、死锁或行锁等待。此时要拆热点、分片、批处理、按 key 路由，或者把强一致范围缩小。

第三类是长事务。长事务会持有 snapshot 和依赖信息更久，增加和其他事务重叠的窗口，也增加重试代价。如果一个事务跑 30 秒，最后 `40001`，应用不只浪费 30 秒，还可能把系统推向重试风暴。

第四类是低价值、可陈旧的读。比如首页计数、普通列表、运营报表、搜索辅助信息。它们通常不需要 `Serializable`。用 `Read Committed`、缓存、异步物化视图更实际。

第五类是团队没有统一重试框架。`Serializable` 只有在应用能正确处理 `40001` 时才可靠。只在某几个 DAO 层零散开启，然后上层不重试，用户会看到更多 500。

第六类是只有部分事务使用 `Serializable`。如果维护同一个业务不变量的事务里，有一部分仍用较低隔离级别直接写，整体推理就断了。Serializable 不是护身符，它要求相关事务都遵守同一套协议。

面试里可以这样回答：

```text
Serializable 适合强业务不变量、跨行跨表约束、显式锁很难写对、冲突率可控且事务能整体重试的场景，比如库存、排班、预算、余额冻结和一致性校验。PostgreSQL 中只读校验还可以考虑 SERIALIZABLE READ ONLY DEFERRABLE，等待安全 snapshot 后再读。它不适合不可重试事务、事务里有外部副作用、高热点写、长事务、低价值陈旧读，也不适合没有统一 40001 重试框架的系统。关键判断是：这条路径是否真的需要串行化语义，以及失败后能不能安全从事务开头重跑。
```

## Q053. serializable 和相近概念最容易混淆的边界在哪里？

`Serializable` 的边界很容易混。它和“串行执行”“线性一致性”“Repeatable Read”“Snapshot Isolation”“2PC”“锁”都相邻，但回答的问题不同。

第一，Serializable 不等于真的串行执行。真的串行执行是同一时间只跑一个事务；Serializable 是结果等价。数据库可以让事务并发运行，只要最后成功提交的历史能找到一个串行顺序。PostgreSQL 的 Serializable Snapshot Isolation 就是并发执行、依赖监测、必要时回滚。

第二，Serializable 不等于 `strict serializable`。Serializable 只要求存在一个串行顺序；strict serializable 还要求这个顺序尊重真实时间。举例：

```text
T1 已经提交并返回给客户端。
T2 在 T1 返回之后才开始。
strict serializable 要求串行顺序中 T1 在 T2 前。
普通 serializable 不一定表达这个实时约束。
```

在单机数据库里，事务开始和提交顺序通常比较直观，但到了分布式系统，这个差别非常重要。很多系统标注 `serializable`，不代表它提供线性化读写。

第三，Serializable 不等于 `Repeatable Read`。PostgreSQL 的 `Repeatable Read` 已经比 SQL 标准最低要求更强，不会出现 phantom read，但仍可能出现 serialization anomaly。`Serializable` 在这个基础上监测危险读写依赖，防止成功提交历史不可串行化。

第四，Serializable 不等于 Snapshot Isolation。Snapshot Isolation 通常允许事务读固定快照，并用写写冲突检测处理同一行冲突；它可能允许写偏斜。PostgreSQL 的 `Serializable` 是 SSI：在 snapshot isolation 之上额外跟踪读写依赖。可以说它建立在 snapshot 思路上，但不能把两者画等号。

第五，Serializable 不等于 `SELECT FOR UPDATE`。`SELECT FOR UPDATE` 锁住返回的行，适合你明确知道要保护哪些行的场景。它保护不了“当前不存在但未来可能插入”的范围，也不自动理解跨行聚合不变量。Serializable 通过 predicate lock / `SIReadLock` 记录读影响范围，用来检测某个写是否会影响先前读的结果。不过在 PostgreSQL 中这些 `SIReadLock` 不阻塞写，只用于依赖检测。

第六，Serializable 不等于 2PC。Serializable 是隔离级别，回答并发事务结果是否等价于串行执行。2PC 是原子提交协议，回答多个参与者要么都提交、要么都回滚。一个分布式事务可以用 2PC 但隔离级别不强；也可以单机 `Serializable` 但完全不涉及 2PC。

第七，Serializable 不等于持久性。事务成功提交后能不能在崩溃后恢复，是 WAL、fsync、同步复制、提交确认语义的问题。Serializable 管的是并发隔离，不是提交落盘。

第八，Serializable 不等于业务幂等。Serializable 可以让某次事务提交历史可串行化，但客户端超时后重复发起同一业务请求，仍然可能重复创建外部动作。幂等键、唯一约束、outbox 仍然要做。

第九，Serializable 不等于所有错误都消失。PostgreSQL 文档特别提到，Serializable 下仍可能看到 unique violation 或 exclusion violation；有些其实是并发导致的，但数据库不一定把它们都归类成 `40001`。应用对 `23505` 要谨慎判断：有些是持久业务错误，有些是可以按并发重试处理的 transient 错误。

可以用这张边界表记：

```text
Serializable:
  成功提交结果能否等价于某个串行顺序？

Strict serializable / linearizable:
  是否还尊重真实时间顺序？

Snapshot Isolation:
  是否读固定 snapshot，并只处理写写冲突？

SELECT FOR UPDATE:
  是否显式阻塞其他事务修改这些行？

2PC:
  多参与者能否原子提交？

Durability:
  提交后崩溃能否恢复？

Idempotency:
  客户端重试同一业务动作是否只生效一次？
```

面试里可以这样回答：

```text
Serializable 是隔离语义：成功提交的事务结果必须等价于某个串行顺序，但事务不一定真的单线程执行。它和 strict serializable 不同，后者还要求尊重真实时间；和 Repeatable Read/Snapshot Isolation 不同，后者仍可能有写偏斜；和 SELECT FOR UPDATE 不同，显式锁是阻塞具体行或范围，PostgreSQL Serializable 的 SIReadLock 主要用于依赖检测；和 2PC 不同，2PC 管多参与者原子提交；和 durability、idempotency 也不是一回事。面试里最容易混的是把 PostgreSQL Repeatable Read 当 Serializable，或者把 Serializable 当成线性一致。
```

## Q054. serializable 在高并发场景下可能出现哪些隐藏问题？

Serializable 在高并发下最常见的隐藏问题不是数据错，而是“正确性通过失败体现出来”。也就是说，系统不再悄悄提交错误结果，而是让一部分事务失败、等待、重试。应用如果没准备好，就会把正确性保护变成线上抖动。

第一类问题是 `40001` 比例上升。PostgreSQL 的 Serializable 会监测读写依赖；高并发下事务重叠越多，依赖图越容易出现危险结构。数据库为了保证可串行化，只能回滚其中一个事务。应用看到的是：

```text
ERROR: could not serialize access due to read/write dependencies among transactions
SQLSTATE 40001
```

这不是数据库坏了，而是隔离级别的正常控制流。

第二类问题是重试风暴。很多应用收到 `40001` 立刻无退避重试，几十个请求又在同一批热点数据上撞一次。结果是失败率更高，CPU 被重复执行事务吃掉，p99 拉长。正确做法是整体重试、有限次数、指数退避、带 jitter，并且对用户请求做幂等。

第三类问题是 predicate lock 内存压力。PostgreSQL 的 SSI 会用 `SIReadLock` 记录读依赖。它们不阻塞写，也不造成普通死锁，但需要内存。内存不足时，细粒度 predicate lock 可能合并成页级或表级；粒度变粗后，误判冲突增加，serialization failure 也可能增加。

第四类问题是顺序扫描扩大冲突范围。PostgreSQL 文档明确提醒，顺序扫描会需要关系级 predicate lock，更容易增加 serialization failure。一个缺索引的查询，在 `Read Committed` 下可能只是慢；在 `Serializable` 下，它还可能把冲突范围扩大到整张表。

第五类问题是长事务提高冲突窗口。事务越长，和其他事务重叠的概率越高，保留的 `SIReadLock` 越久，失败重试代价也越大。长读事务如果不是 `READ ONLY DEFERRABLE`，也可能参与依赖链。

第六类问题是热点写仍然会锁等待。Serializable 不是无锁协议。两个事务更新同一行，仍会有行锁等待、deadlock 或 concurrent update 失败。高并发扣同一个账户余额，Serializable 不能让它并行变快。

第七类问题是 unique violation 可能混进来。PostgreSQL 文档提到，在 Serializable 下，即使程序先检查 key 不存在，再插入，也可能因为并发插入看到唯一约束错误；某些场景本质上是可重试的并发冲突，但错误码可能是 `23505`。应用不能盲目把所有唯一约束错误都重试，也不能完全不重试。要按业务 key 判断。

第八类问题是只读事务也不总是免费。普通只读 Serializable 事务可能需要参与依赖监测；`READ ONLY DEFERRABLE` 可以等待安全 snapshot，减少后续失败，但它会在开始时等待。这是延迟和失败率之间的交换。

第九类问题是连接数放大冲突。并发连接太多时，重叠事务数量增大，predicate lock 和事务状态管理压力增大，重试也更容易成批发生。PostgreSQL 官方性能建议里专门提到，使用 Serializable 的忙系统更要控制活跃连接数。

第十类问题是混用隔离级别破坏推理。部分事务用 Serializable，部分事务用 Read Committed 直接改同一组数据，系统整体并不等价于“所有业务都可串行化”。这类问题很隐蔽，因为单个关键事务看起来已经很严格。

监控上要看这些东西：

```text
40001 serialization_failure 数量和比例
40P01 deadlock_detected 数量
23505/23P01 中哪些可能是并发生成 key
pg_locks 中 SIReadLock 数量和粒度
长事务和 idle in transaction
顺序扫描和缺索引慢查询
重试次数、重试成功率、重试后端到端延迟
连接池等待和数据库活跃连接数
```

面试里可以这样回答：

```text
Serializable 高并发下的隐藏问题主要是失败和重试成本。PostgreSQL 用 SSI 监测读写依赖，冲突多时会返回 40001；如果应用立即重试，会形成重试风暴。SIReadLock 需要内存，粒度合并成页级或表级后会增加误报冲突；顺序扫描会扩大 predicate lock 范围；长事务增加重叠窗口；热点写仍然会行锁等待或死锁；唯一约束错误有时也可能是并发冲突表现。治理上要短事务、补索引、控制连接数、声明 READ ONLY/DEFERRABLE、统一重试退避、监控 40001 和 SIReadLock，并避免混用隔离级别破坏整体推理。
```

## Q055. serializable 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

Serializable 在故障场景下最重要的边界是：事务结果只有在成功提交后才可以被业务当成有效事实。PostgreSQL 文档也强调，依赖 Serializable 防异常时，事务里读到的数据在事务成功提交前不能被当作最终有效结果。因为这个事务最后可能因为依赖冲突失败。

第一类边界是提交时失败。Serializable 事务可能前面所有 SQL 都成功，最后 `COMMIT` 才返回 `40001`。这对应用很关键：不能在事务中间就对外发送“已成功”的消息，也不能在提交前把读到的判断结果用于外部副作用。

错误模式可能是：

```text
BEGIN ISOLATION LEVEL SERIALIZABLE;
SELECT ...
UPDATE ...
COMMIT;  -- 这里返回 40001
```

正确处理是回滚事务，从最开始重新执行业务逻辑。只重试 `COMMIT` 或最后一条 `UPDATE` 不够，因为前面的读结果可能已经不再能安全提交。

第二类边界是超时。`statement_timeout`、`lock_timeout`、客户端超时、连接池超时都可能发生。超时不等于事务一定没提交，尤其是客户端在 `COMMIT` 附近断线时。Serializable 不能解决 unknown commit。非幂等业务仍然要用幂等键或状态表确认结果。

第三类边界是死锁和 serialization failure 的处理不同但都可能要重试。`40001` 是 Serializable 的正常失败路径；`40P01` 是死锁检测。PostgreSQL 文档建议 serialization failure 通常应无条件重试完整事务，deadlock failure 也常常适合重试。唯一约束和排他约束错误则要更谨慎，因为它们可能是持久业务错误，也可能是并发生成 key 的瞬时冲突。

第四类边界是崩溃恢复。Serializable 管隔离，不管持久化。数据库崩溃后，已提交事务靠 WAL 恢复；未提交事务不能留下可见效果。若事务在崩溃前已经返回成功，它是否能在新 primary 或重启实例上看到，取决于 WAL、fsync、同步复制和故障切换语义，不取决于 Serializable。

第五类边界是 prepared transaction。两阶段提交里，事务可能 `PREPARE TRANSACTION` 后悬挂。它可能保留锁和资源，也可能阻碍后续事务推进。Serializable 的重试也可能因为冲突的 prepared transaction 一直不结束而无法成功。PostgreSQL 文档在 serialization failure handling 里也提到，遇到冲突的 prepared transaction 时，可能要等它 commit 或 rollback 才能继续。

第六类边界是重启后应用连接状态丢失。事务隔离级别是事务属性，不是业务请求的永久状态。连接断开后，应用必须重新开始事务，重新设置 isolation level，重新执行逻辑。不能假设旧连接里的 snapshot、临时表、advisory lock 或 prepared statement 还存在。

第七类边界是只读 deferrable 事务的等待。`SERIALIZABLE READ ONLY DEFERRABLE` 可能在开始时阻塞，直到拿到安全 snapshot。它后续更稳定，但如果系统持续写入很重，开始等待可能超时。应用要把这个等待当成设计的一部分，而不是误判成死锁。

第八类边界是外部副作用必须放在提交之后。比如：

```text
事务中读库存并更新订单
事务里直接调用支付系统或发送 MQ
COMMIT 返回 40001
数据库回滚了，但外部副作用已经发生
```

这类 bug 和 Serializable 没有冲突；它只是暴露出事务边界设计错误。正确做法是数据库事务内写 outbox，提交后由 worker 投递外部消息，投递侧幂等。

面试里可以这样回答：

```text
Serializable 的故障边界是：事务内读到的结果只有在事务成功提交后才可信。PostgreSQL 可能在 COMMIT 时返回 40001，所以应用必须回滚并从头重试整个事务，不能只重试最后一条 SQL。超时和连接断开还会产生 unknown commit，Serializable 不解决幂等问题，仍要用 request id 或状态表确认。崩溃恢复靠 WAL，不靠隔离级别；failover 下已确认事务是否保留取决于复制确认语义。prepared transaction 可能悬挂并阻塞重试。事务里不要做不可回滚外部副作用，应该用 outbox 和幂等投递。
```

## Q056. serializable 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

Serializable 的瓶颈通常不是某一个维度，而是依赖监测、重试、查询计划和事务形态叠加出来的。单机 PostgreSQL 里，最典型的是 CPU、内存、锁竞争和 I/O；网络一般不是单机 Serializable 的核心成本，但在同步复制、分布式事务和跨区域场景里会变成瓶颈。

CPU 开销来自两部分。第一部分是正常查询执行：扫描、join、排序、聚合、可见性判断。第二部分是 Serializable 额外的读写依赖监测。事务越多、读写集合越大、重叠越严重，监测成本越高。失败后的重试还会把同一段业务逻辑再跑一遍，这也是 CPU。

内存开销主要来自 predicate lock / `SIReadLock`。PostgreSQL 需要记录事务读过哪些 tuple、page 或 relation，以便判断后续写是否会影响之前读的结果。锁粒度越细，内存越多；内存不足时合并成更粗粒度，失败率可能上升。相关参数包括：

```text
max_pred_locks_per_transaction
max_pred_locks_per_relation
max_pred_locks_per_page
```

锁竞争仍然存在。Serializable 的 `SIReadLock` 本身不阻塞写，但行级写锁、唯一索引冲突、外键检查、DDL 锁仍然会阻塞。热点更新在 Serializable 下不会变快。并发更新同一行，还是要等待、死锁检测或失败重试。

I/O 开销常常来自查询计划。顺序扫描不仅读更多页面，还会在 Serializable 下扩大 predicate lock 范围。缺索引查询可能导致关系级 `SIReadLock`，增加 false positive 冲突。大量重试也会重复读写页面、重复产生 WAL。长事务还会拖住 VACUUM，间接造成 bloat 和更多 I/O。

网络不是单机隔离级别本身的主要开销，但这些场景会让网络进入关键路径：

```text
同步复制下 COMMIT 等待 standby
分布式 Serializable 需要全局时间戳或协调器
跨分片事务使用 2PC
事务里错误地调用远程服务
应用和数据库跨地域部署
```

因此，问“Serializable 慢在哪里”时，要先分辨是哪一种慢：

```text
40001 多：
  主要是冲突和重试成本，查依赖范围、热点、事务时长、索引。

CPU 高：
  查执行计划、扫描行数、重试次数、表达式和聚合。

SIReadLock 多或粒度粗：
  查 predicate lock 内存、顺序扫描、max_pred_locks 配置。

锁等待高：
  查热点行、SELECT FOR UPDATE、唯一索引、外键、DDL。

I/O 高：
  查顺序扫描、bloat、VACUUM、重试产生的重复读写和 WAL。

网络慢：
  查同步复制、跨分片协调、客户端连接路径。
```

优化时有几条很实用的原则：

```text
事务越短越好，只放维护不变量所需的读写。
能声明 READ ONLY 就声明 READ ONLY。
只读校验能接受等待时，用 READ ONLY DEFERRABLE。
给谓词查询补合适索引，避免顺序扫描扩大冲突范围。
控制活跃连接数，不让冲突集合无限变大。
移除不再需要的 SELECT FOR UPDATE，避免双重保守。
对 40001 做退避重试，而不是立即打满数据库。
```

面试里可以这样回答：

```text
Serializable 的性能瓶颈通常来自依赖监测和重试，而不是单纯某个硬件资源。CPU 花在查询执行、可见性判断、SSI 依赖检测和失败事务重跑上；内存花在 SIReadLock/predicate lock 上，粒度合并后还会增加误报冲突；锁竞争来自热点行、唯一索引、外键和 DDL，Serializable 不能消除写写冲突；I/O 来自顺序扫描、bloat、VACUUM 滞后和重试产生的重复读写。网络在单机里不是核心，但同步复制、跨分片和分布式事务会让它进入提交路径。优化要短事务、补索引、控制连接数、声明 READ ONLY/DEFERRABLE、监控 40001 和 SIReadLock，并做退避重试。
```

## Q057. serializable 的 correctness test、stress test 和 benchmark 应该分别测什么？

Serializable 的测试要分三层。Correctness test 证明不会提交不可串行化结果；stress test 证明高并发、超时、重试下系统仍然维持不变量；benchmark 量化代价，包括吞吐、延迟、失败率和资源消耗。

Correctness test 的核心是构造在较低隔离级别下会错、在 Serializable 下必须失败或串行化的案例。

第一组是写偏斜：

```text
初始：A、B 两名医生都 on_call=true。
T1: 读到 A、B 都在，更新 A=false。
T2: 读到 A、B 都在，更新 B=false。
期望：Serializable 下不能两个都提交。
```

第二组是聚合不变量：

```text
规则：某预算池 sum(approved_amount) <= limit。
T1/T2 同时读取当前总额，都认为还能批 100。
两者分别插入不同审批记录。
期望：不能两个都提交后超过 limit。
```

第三组是范围谓词：

```text
规则：同一房间同一时间段不能有重叠预约。
T1 查某时间段无预约，插入预约。
T2 查同一时间段无预约，插入另一个重叠预约。
期望：Serializable 或排他约束必须阻止最终重叠。
```

第四组是只读事务有效性。PostgreSQL 文档提醒，Serializable 事务读到的数据在事务成功提交前不应被视为有效。测试要覆盖只读事务和并发写事务重叠时，读事务是否可能失败；如果使用 `READ ONLY DEFERRABLE`，要验证它等待安全 snapshot 后不会参与普通失败路径。

第五组是重试语义。收到 `40001` 后，必须从业务逻辑开头重试。测试应该故意让第一次失败，确认第二次使用新的读结果，而不是沿用旧判断。

Stress test 要把这些事务随机化、并发化、故障化。可以设计：

```text
多线程随机执行转账、预约、预算审批、状态流转。
随机插入 sleep，扩大事务交错窗口。
随机触发 statement_timeout、lock_timeout、连接断开。
随机把一部分事务做成长事务，观察失败率和延迟。
对 40001 做退避重试，验证最终不变量。
```

Stress test 不只看错误数，还要查最终状态：

```sql
-- 预算是否超限
SELECT budget_id, sum(amount), max(limit_amount)
FROM approvals
JOIN budgets USING (budget_id)
GROUP BY budget_id
HAVING sum(amount) > max(limit_amount);

-- 任务是否被多个 worker 完成
SELECT task_id, count(*)
FROM task_results
GROUP BY task_id
HAVING count(*) > 1;
```

Benchmark 则要固定负载模型。至少要比较：

```text
Read Committed + 显式锁
Repeatable Read
Serializable
Serializable READ ONLY
Serializable READ ONLY DEFERRABLE
```

指标要包括：

```text
tx/s
p50/p95/p99 latency
40001 比例
40P01 比例
平均重试次数
重试后成功率
CPU
shared_blks_hit/read
WAL bytes
temp file
SIReadLock 数量和粒度
连接池等待
```

Benchmark 还要区分冲突率：

```text
低冲突：key 分散，Serializable 应该接近 Repeatable Read。
中冲突：部分热点，观察 40001 和 p99。
高冲突：集中热点，观察重试风暴和吞吐塌陷点。
```

一个常见误区是只测“事务都成功时的吞吐”。Serializable 的真实性能要算重试后的端到端成本。如果一次用户请求平均执行 1.4 次数据库事务才成功，吞吐和延迟都要按用户请求口径统计。

面试里可以这样回答：

```text
Correctness test 要构造写偏斜、聚合不变量、范围谓词、只读事务有效性和 40001 整事务重试，证明 Serializable 不会让不可串行化结果成功提交。Stress test 要随机并发这些事务，加入 sleep、超时、连接断开、长事务和重试退避，最后检查业务不变量，而不是只看 SQL 是否报错。Benchmark 要比较不同隔离级别和显式锁方案，在不同冲突率下记录 tx/s、p99、40001/40P01、平均重试次数、CPU、I/O、WAL、SIReadLock 和连接池等待。Serializable 的性能必须按用户请求最终成功口径统计，不能只算单次数据库事务。
```

## Q058. 如果要求从零实现一个简化版 serializable，你会先定义哪些不变量？

从零实现简化版 Serializable，先要定义“什么叫可串行化”。最直接的不变量是：所有成功提交事务形成的依赖图必须无环。只要有环，就说明不存在一个串行顺序能解释这些事务的结果。

可以先把事务之间的依赖分成几类：

```text
write-read:
  T2 读到了 T1 写入的版本，说明 T1 必须排在 T2 前。

read-write:
  T1 读了某个旧版本或范围，T2 后来写入会影响这个读结果，说明可能有危险依赖。

write-write:
  T1 和 T2 写同一个对象，必须有确定顺序，不能丢失更新。
```

第一个不变量：提交事务图无环。

```text
如果 T1 -> T2 -> T3 -> T1，
则三者不能都提交。
```

简化实现可以在提交时检查当前事务是否会关闭一个依赖环；如果会，就 abort 当前事务。

第二个不变量：事务读取的谓词要被记录。只记录“读了哪一行”不够。比如事务查“某时间段没有预约”，结果是空集；另一个事务插入了这个时间段的预约。没有读到行也要产生保护信息，否则 phantom/write skew 防不住。

简化实现可以这样做：

```text
点查：记录 key read set
范围查：记录 range read set
全表扫描：记录 table read set
写入：检查是否命中其他活跃事务的 read set
```

第三个不变量：读写集合要和查询计划无关地表达语义。真实系统很难做到完全语义化，PostgreSQL 的 predicate lock 会受访问路径影响，顺序扫描可能变成 relation-level。简化系统至少要承认：读范围粒度越粗，误杀越多；越细，内存越大。

第四个不变量：写写冲突有全序。两个事务写同一个 key，不能都基于旧版本提交出丢失更新。可以用行锁、版本校验或提交时间戳顺序处理。

第五个不变量：只提交能找到串行位置的事务。一个事务提交时，如果它的读依赖和写依赖已经说明“它既必须在某事务前，又必须在同一事务后”，就不能提交。

第六个不变量：失败事务的读写不能进入提交历史。abort 后要释放或标记依赖信息，不能让后续事务因为一个已失败事务被永久误杀。

第七个不变量：重试必须看到新 snapshot。事务失败后，重试不能沿用旧 snapshot 或旧业务判断。否则还是在尝试提交同一个不可串行化历史。

第八个不变量：只读事务也要有规则。普通只读事务可能参与依赖环；如果要实现 deferrable read-only，就要在事务开始前等待一个安全 snapshot，并证明它不会成为未来危险结构的一部分。

第九个不变量：GC 不能过早删除依赖信息。Serializable 的依赖有时要保留到重叠事务结束以后。PostgreSQL 文档也提到，`SIReadLock` 有时需要在事务提交后继续保留，直到重叠的读写事务结束。简化实现也要有类似边界。

第十个不变量：错误必须显式暴露给上层。不要在数据库内部偷偷自动重试整个事务，因为数据库不知道应用在事务语句之间做了哪些决策。正确接口是返回 `serialization_failure`，由业务层重新执行完整逻辑。

一个极简提交检查可以这样描述：

```text
begin(tx):
  tx.snapshot = current_committed_state
  tx.read_set = empty
  tx.write_set = empty

read(tx, predicate):
  record predicate in tx.read_set
  return visible rows from tx.snapshot

write(tx, key):
  record key in tx.write_set
  detect write-write conflict

commit(tx):
  add dependency edges from observed reads/writes
  if dependency graph would contain cycle:
      abort tx
      return serialization_failure
  publish writes
  mark tx committed
```

真实 SSI 不会这么粗糙地每次全图检测；它会用危险结构、rw-conflict 等更高效的规则。但面试里先把不变量讲清楚，比一上来背实现细节更重要。

面试里可以这样回答：

```text
从零实现简化 Serializable，我会先定义成功提交事务的依赖图必须无环。要记录 write-read、read-write、write-write 依赖；点查、范围查、空结果范围查都要进入 read set，否则防不住 phantom 和写偏斜；写同一 key 必须有确定顺序；提交时如果当前事务会形成依赖环，就返回 serialization_failure；失败事务不能进入提交历史；重试必须拿新 snapshot；只读事务也要参与规则，除非实现 deferrable safe snapshot；依赖信息不能在重叠事务结束前过早 GC。最后，数据库不要偷偷自动重试，因为业务决策也要重跑。
```

## Q059. serializable 的常见误用是什么，误用后通常会产生什么线上症状？

Serializable 的误用通常来自两个极端：一种是以为它能替代所有工程设计；另一种是开启后却没有按它的协议处理失败。线上症状一般是 500 增多、重试风暴、延迟抖动、唯一约束异常、吞吐下降，或者更糟，部分事务没用 Serializable 导致不变量仍被破坏。

第一种误用：不处理 `40001`。这是最常见的。应用把 serialization failure 当普通数据库异常返回给用户。高峰期一来，错误率上升。

症状：

```text
日志大量 SQLSTATE 40001
接口偶发 500
重放请求后又成功
数据库看起来没有死锁或宕机
```

第二种误用：只重试最后一条 SQL。Serializable 失败意味着事务之前的读判断也不能信。只重试 `UPDATE` 或 `COMMIT` 会继续基于旧 snapshot 和旧业务决策，语义不对。

症状：

```text
重试后仍频繁失败
偶发业务判断和最终数据对不上
代码里有局部 retry，但没有完整事务 retry
```

第三种误用：事务里做外部副作用。比如在 Serializable 事务里发 MQ、调支付、发邮件，然后提交失败。

症状：

```text
用户收到通知但数据库没有对应状态
支付或任务被重复触发
补偿流程复杂，排障需要跨系统对账
```

第四种误用：只让一部分事务使用 Serializable。比如扣库存事务用了 Serializable，但后台修正库存、导入脚本、管理端调整仍用 Read Committed 直接写。同一个不变量的所有写路径没有统一协议，Serializable 的推理就破了。

症状：

```text
核心路径看起来严格，但后台操作后出现负库存或超额审批
问题只在运营脚本、迁移、管理端批处理后出现
```

第五种误用：把 Serializable 当作分布式全局一致性。单个 PostgreSQL 实例的 Serializable 不会保护另一个数据库、Kafka、Redis、对象存储里的状态。跨系统仍然需要 outbox、saga、幂等或分布式事务。

症状：

```text
数据库内状态可串行化，但消息系统重复或丢失
缓存和数据库短时间或长期不一致
跨服务流程卡在中间状态
```

第六种误用：长事务加 Serializable。报表、批处理、人工审核都放在一个大事务里，最后失败再重来。

症状：

```text
事务耗时很长，失败代价高
SIReadLock 数量大
40001 在提交阶段集中出现
p99 很差，用户感觉“快结束时失败”
```

第七种误用：缺索引导致冲突范围扩大。范围查询没有索引，数据库顺序扫描；在 Serializable 下可能拿关系级 predicate lock，导致更多不相关写事务失败。

症状：

```text
某个查询上线后 40001 全局上升
EXPLAIN 看到 Seq Scan
pg_locks 里 SIReadLock 粒度较粗
加索引后失败率下降
```

第八种误用：把所有 `23505` 都当业务重复，或者都当可重试。Serializable 下唯一约束错误有时和并发生成 key 有关，但也可能是真正重复请求。需要结合业务幂等键判断。

症状：

```text
有些请求本可重试却直接失败
有些真实重复被无限重试
日志里 23505 和 40001 同时上升
```

第九种误用：无限重试。没有最大次数、没有退避、没有熔断。冲突高时，所有请求反复抢同一组数据。

症状：

```text
数据库 CPU 高但有效吞吐低
同一请求执行多次事务
连接池占满
尾延迟雪崩
```

第十种误用：在不需要的地方全局开启。很多普通读写路径不需要 Serializable。全局开启可能让简单列表、低价值统计、后台扫描都参与依赖监测，增加失败率和成本。

症状：

```text
整体吞吐下降
低价值查询拖累关键写入
业务没有更强一致收益，但数据库错误和延迟增加
```

面试里可以这样回答：

```text
Serializable 常见误用包括不处理 40001、只重试最后一条 SQL、事务里做外部副作用、只有部分相关写路径使用 Serializable、把单库 Serializable 当分布式一致性、长事务大报表使用 Serializable、缺索引导致 predicate lock 过粗、误判 23505、无限重试，以及全局滥用。线上症状是 40001/40P01/23505 上升、接口 500、p99 抖动、重试风暴、CPU 高但有效吞吐低、外部消息和数据库状态不一致、后台脚本绕过隔离后破坏不变量。修复重点是完整事务重试、退避和幂等、短事务、补索引、统一所有相关写路径的隔离协议，并把外部副作用放到提交后的 outbox。
```

## Q060. serializable 在单机和分布式环境中的语义有什么差异？

单机 Serializable 的边界在一个数据库实例里。事务状态、snapshot、锁、依赖监测、提交顺序和 WAL 都由同一个内核管理。分布式 Serializable 要跨节点定义“一个串行顺序”，还要处理网络分区、时钟、复制延迟、跨分片提交和故障恢复。名字一样，难度不是一个量级。

单机 PostgreSQL 的 Serializable 可以这样理解：

```text
每个事务在本机 snapshot 上运行
数据库记录读写依赖
发现不可串行化结构时让某个事务失败
成功提交的事务可以解释成某个串行顺序
```

这套机制依赖一个实例里的共享事务状态。即便有 standby，standby 上的只读事务也有特殊边界：PostgreSQL caveats 里提到，hot standby 上目前最严格支持到 Repeatable Read，不提供 primary 上完整 Serializable 的同等能力；standby 可能看到 primary 上一组可串行化事务的中间回放状态。

分布式环境里，首先要问事务是否跨分片。如果所有事务都按 key 路由到单个分片，那么每个分片内部可以用单机 Serializable；但跨分片不变量仍然没有自动保护。比如：

```text
用户 A 在 shard 1
用户 B 在 shard 2
规则：A+B 总额度不能超过 100
```

如果两个事务分别在不同 shard 上读写，没有全局协调，就不能只靠单分片 Serializable 保证总额度。

跨分片 Serializable 至少要解决三个问题。

第一，全局读时间点。事务读多个 shard 时，要保证读到的组合状态来自同一个逻辑时间点，或者至少能被某个全局串行顺序解释。实现可能用 timestamp oracle、混合逻辑时钟、TrueTime 一类时间 API，或事务协调器分配时间戳。

第二，原子提交。跨 shard 写入要么都提交，要么都回滚。常见方案是 2PC。没有原子提交，即使每个 shard 内部都 Serializable，整体仍可能出现 A shard 提交、B shard 失败的半成品状态。

第三，依赖检测或锁要跨节点。单机可以在本地内存里记录依赖；分布式要么集中协调，要么在各节点交换读写集合和时间戳，要么用更保守的锁。网络延迟和节点故障会直接影响提交路径。

再看 strict serializable。很多分布式系统不仅要 Serializable，还要真实时间顺序。例如 T1 已经向用户返回提交成功，T2 后开始并读数据，T2 应该看到 T1。普通 Serializable 只要求存在某个串行顺序；strict serializable 要求这个顺序还尊重真实时间。跨地域系统为了做到这一点，通常要付出时钟等待或协调成本。

复制也会改变语义。一个系统可能 primary 上事务 Serializable，但 follower read 默认是陈旧的。用户写完后读 follower，如果 follower 还没追上，就读不到自己的写入。这里不是 Serializable 失效，而是读请求没有在同一个一致性边界内执行。

网络分区下还要面对 CAP 取舍。单机数据库进程内没有这个问题；分布式系统如果分区后仍允许多边写入，就很难保持全局 Serializable。多数系统会在分区时牺牲部分可用性，或者只给单分区/单 key 强语义，跨分区走补偿。

分布式 Serializable 的测试也更难。单机测试可以看事务交错；分布式还要测：

```text
协调者崩溃
参与者 prepare 后断网
部分 shard 提交、部分 shard 超时
时钟漂移
follower read 陈旧
跨区域延迟抖动
旧 primary 恢复后的 fencing
```

对应用来说，最好不要只看产品文档里的“Serializable”四个字。要问清楚：

```text
是否支持跨分片事务？
默认读是否可能走 follower？
是否 strict serializable？
是否要求所有读写都在事务里？
事务失败错误码是什么？
是否有自动重试，自动重试是否包括业务逻辑？
故障切换后是否可能丢已确认事务？
```

面试里可以这样回答：

```text
单机 Serializable 由一个数据库内核管理事务状态、snapshot、依赖检测和提交顺序，PostgreSQL 用 SSI 让成功提交事务等价于某个串行顺序。分布式 Serializable 要跨节点定义全局串行顺序，还要解决全局时间戳、跨分片读一致性、2PC 原子提交、跨节点依赖检测、复制延迟、网络分区和故障恢复。单分片 Serializable 不等于跨分片 Serializable；Serializable 也不一定是 strict serializable，后者还要求尊重真实时间。读 follower、异步复制和 failover 都可能让应用看到和单机不同的行为。工程上必须看具体数据库的事务范围、读路由、失败重试和故障语义，不能只凭隔离级别名字判断。
```

## Q061. unique index 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

Unique index 的核心目标是把“某个键在一个定义好的作用域内只能出现一次”变成数据库内核强制维护的不变量。它首先解决的是正确性问题，其次才顺带提供查询性能。可维护性也会受益，因为约束写在 schema 里，比散落在多个服务里的 `SELECT` 预检查可靠得多。安全性不是它的主要目标，除非业务把唯一性作为权限或身份边界的一部分，那也只是间接相关。

在 PostgreSQL 里，唯一索引和普通索引最大的差别不是“查得更快”，而是插入或更新时会检查重复键。官方文档说，唯一约束和主键会自动创建唯一 B-tree 索引；目前只有 B-tree 索引能声明为 unique。也就是说，唯一索引既是一个访问路径，也是约束执行机制。

最典型的问题是这个竞态：

```text
事务 A: SELECT id FROM users WHERE email = 'a@example.com'; -- 没有
事务 B: SELECT id FROM users WHERE email = 'a@example.com'; -- 没有
事务 A: INSERT email='a@example.com';
事务 B: INSERT email='a@example.com';
```

如果只靠应用层先查再插，两个事务都可能认为自己可以创建。唯一索引把最后的判断放在数据库写入路径上，保证两个候选行不会同时成为可见的有效行。另一个事务要么等待前一个事务结束，要么收到 `23505 unique_violation`，要么进入 `ON CONFLICT` 分支。

这里要注意 MVCC 语义。PostgreSQL 的索引内部允许物理上存在重复索引项，因为它们可能指向同一逻辑行的不同版本，也可能指向还没提交或已经删除的元组。真正要保证的是：没有一个 MVCC snapshot 能同时看见两个相同 key 的有效行。官方的 Index Uniqueness Checks 文档把这个点讲得很清楚：唯一性检查必须结合 heap tuple 的可见性，而不是只看索引页里有没有相同 key。

所以 unique index 的目标可以拆成三层：

```text
逻辑目标：同一作用域内，同一 key 最多一条有效记录
并发目标：并发插入、更新、删除时仍然保持这个不变量
工程目标：让应用用错误码或 ON CONFLICT 明确处理重复请求
```

它解决正确性的方式很“硬”：数据库不相信应用已经检查过了，而是在写路径里重新检查。这个机制尤其适合用户名、邮箱、订单号、幂等键、外部事件 ID、租户内业务编号这类“不能重复”的数据。

性能方面，唯一索引当然也能加速等值查询：

```sql
SELECT *
FROM users
WHERE tenant_id = 1 AND email = 'a@example.com';
```

如果有：

```sql
CREATE UNIQUE INDEX users_tenant_email_uq
ON users (tenant_id, email);
```

数据库既能快速定位记录，也能阻止同一租户下重复邮箱。但面试里不要把它说成“主要为性能服务”。普通 B-tree index 也能加速查询，unique index 的独特价值在于约束。

维护性上，unique index 的好处是把业务不变量变成数据库对象。新服务、后台脚本、导入任务、管理后台都绕不过它。否则只在 API 层做校验，后续补一个批处理脚本就可能写出重复数据。

不过 unique index 也有边界。默认情况下，PostgreSQL 的唯一约束把 `NULL` 当作不相等，所以一个唯一列里可以有多个 `NULL`。如果业务语义是“空值也只能有一个”，需要用 `NULLS NOT DISTINCT`，或者在旧版本/特定场景下用 partial unique index 表达。

```sql
CREATE UNIQUE INDEX users_email_uq
ON users (email) NULLS NOT DISTINCT;
```

组合唯一索引也常被误解。`UNIQUE (a, b)` 约束的是 `(a, b)` 这对组合，不表示 `a` 单独唯一，也不表示 `b` 单独唯一。

```text
(tenant=1, email=a@example.com) 只能有一条
(tenant=2, email=a@example.com) 可以同时存在
```

面试里可以这样回答：

```text
unique index 的核心目标是维护唯一性不变量，主要解决正确性问题。它让数据库在插入和更新路径上检查重复 key，而不是依赖应用层先 SELECT 再 INSERT。PostgreSQL 里唯一约束和主键会自动创建唯一 B-tree 索引；因为 MVCC，索引里物理上可能有重复版本，真正保证的是没有一个可见快照能同时看到两个相同 key 的有效行。它也能提升等值查询性能，但这不是它区别于普通索引的核心。安全性不是主要目标，可维护性是副收益，因为约束写进 schema 后，所有写入口都必须遵守。
```

## Q062. unique index 的典型适用场景和不适用场景分别是什么？

Unique index 适合表达“同一业务作用域内最多一条”的规则。只要这个规则可以由一组列、表达式和可选谓词定义出来，它就比应用层校验可靠。

第一类场景是业务自然键。比如同一租户内邮箱唯一：

```sql
CREATE UNIQUE INDEX users_tenant_email_uq
ON users (tenant_id, email);
```

如果是全局邮箱唯一，就不需要 `tenant_id`；如果是租户内唯一，就必须把 `tenant_id` 放进唯一键。很多线上重复数据问题都来自作用域漏写。开发者以为 email 唯一，业务实际要求是 tenant 内唯一，或者反过来。

第二类场景是幂等键。比如支付回调、任务提交、订单创建：

```sql
CREATE UNIQUE INDEX idempotency_keys_uq
ON idempotency_records (tenant_id, operation, idempotency_key);
```

第一次请求插入记录，后续相同请求命中唯一键。应用可以读取已保存的状态和响应结果。这里 unique index 只保证“同一个幂等键只有一条记录”，不负责自动恢复外部副作用；幂等协议还要保存 payload hash、执行状态、最终结果和过期策略。

第三类场景是去重导入或事件消费。外部事件通常有 source + event_id：

```sql
CREATE UNIQUE INDEX inbox_events_source_event_uq
ON inbox_events (source, event_id);
```

消费者重复收到消息时，数据库能稳定识别已经处理过的事件。比起“先查是否处理过”，直接插入处理记录并捕获冲突更可靠。

第四类场景是软删除或状态条件下的唯一性。比如一个用户只能有一个 active subscription，但历史 canceled subscription 可以保留：

```sql
CREATE UNIQUE INDEX subscriptions_one_active_uq
ON subscriptions (user_id)
WHERE status = 'active';
```

这是 partial unique index。PostgreSQL 文档也给了类似例子：只对满足谓词的行强制唯一。注意它不是普通 `UNIQUE` constraint，因为 SQL 层唯一约束不能直接表达“只约束部分行”。

第五类场景是表达式唯一。典型例子是大小写不敏感邮箱：

```sql
CREATE UNIQUE INDEX users_lower_email_uq
ON users (tenant_id, lower(email));
```

PostgreSQL 的 expression index 文档说明，唯一表达式索引可以阻止只在大小写上不同的值同时出现。这里要确认表达式和业务语义一致。比如邮箱还可能涉及 Unicode normalization、空格裁剪、域名大小写、国际化域名。数据库只按索引表达式执行，不会猜业务规则。

第六类场景是候选键。外键可以引用主键，也可以引用满足条件的唯一键。比如商品有内部 `id`，也有对外稳定的 `sku`，如果订单明细按 `sku` 引用，就需要 `sku` 有唯一约束或合适的唯一索引。

不适用场景也很多。

第一，不适合低基数字段。`status`、`gender`、`type` 这类字段本来就会重复。给它们建 unique index 不是优化，是把正常数据写入变成错误。

第二，不适合表达“最多 N 条”。比如“一个用户最多 3 个有效设备”。Unique index 只能表达最多 1 条。最多 N 条通常要改模型，比如预分配 slot：

```text
UNIQUE (user_id, slot_no)
slot_no in (1, 2, 3)
```

或者用事务锁、计数表、Serializable、触发器等方式维护。

第三，不适合时间区间重叠约束。比如同一个会议室同一时间不能有重叠预订。唯一索引只能比较相等，不能表达区间 overlap。PostgreSQL 里更适合用 exclusion constraint 和 GiST：

```sql
EXCLUDE USING gist (
  room_id WITH =,
  during WITH &&
)
```

第四，不适合频繁变动、且不是业务身份的字段。比如把昵称设成唯一，用户改名会造成大量冲突和锁等待；把商品标题设成唯一，导入时容易误杀正常数据。唯一键最好稳定、明确、可解释。

第五，不适合“全局唯一但数据实际分片”的场景，除非路由规则能保证相同 key 总在同一个分片，或者系统提供全局唯一索引。单个 PostgreSQL 分区表也有类似限制：分区表上的唯一约束必须包含所有分区键，这样潜在冲突行才会落到可检查的同一分区范围内。

第六，不适合拿来替代权限检查。唯一索引可以保证用户名不重复，但不能保证调用者有权创建这个用户名。授权、审计、租户隔离仍然要在独立机制里做。

第七，不适合当成完整业务幂等。比如：

```sql
INSERT INTO payments(idempotency_key, amount)
VALUES (...)
ON CONFLICT DO NOTHING;
```

这只能避免重复插入 payment 记录，不能保证第三方支付没有被调用两次。外部副作用要配合 outbox、状态机、重试和对账。

面试里可以这样回答：

```text
unique index 适合业务自然键、租户内唯一键、幂等键、外部事件去重、候选键、软删除下的部分唯一性、大小写归一后的表达式唯一性。它不适合低基数字段、最多 N 条、时间区间不重叠、频繁变化的展示字段、跨分片全局唯一、权限控制和完整副作用幂等。判断标准是：业务规则能不能被稳定地表示成 key、表达式和可选谓词，并且所有写入口是否都应该被数据库强制约束。
```

## Q063. unique index 和相近概念最容易混淆的边界在哪里？

最容易混淆的第一组边界是 unique index 和 unique constraint。Unique constraint 是 schema 层的约束语义，unique index 是 PostgreSQL 用来执行这个约束的物理机制之一。定义：

```sql
CREATE TABLE users (
  email text UNIQUE
);
```

PostgreSQL 会自动创建唯一 B-tree 索引。反过来，直接写：

```sql
CREATE UNIQUE INDEX users_email_uq
ON users (email);
```

也能强制唯一，但它在 schema 表达上更偏索引对象。两者很多时候效果相近，但不是完全等价。比如 partial unique index 可以表达“只对 active 行唯一”，普通 unique constraint 不能直接表达；unique constraint 可以被命名、可以参与更标准的约束管理，并且在一些外键和迁移语义上更清晰。面试里可以说：业务模型里的主键、候选键优先用 constraint；表达式唯一、部分唯一通常用 unique index。

第二组边界是 unique index 和 primary key。Primary key 可以理解为“唯一 + 非空 + 表的主要行身份”。PostgreSQL 文档说，主键会自动创建唯一 B-tree 索引，并强制列 `NOT NULL`。一张表只能有一个 primary key，但可以有多个 unique constraints。

```text
PRIMARY KEY (id)      : 行身份，默认外键目标，非空
UNIQUE (email)        : 候选键，可以有多个，默认 NULL 可重复
UNIQUE (tenant, name) : 组合唯一，不代表单列唯一
```

如果一个字段是内部行标识，用 primary key；如果它是业务候选键，用 unique constraint 或 unique index。

第三组边界是 unique index 和普通 index。普通索引只提供访问路径，不阻止重复值。下面这个索引不会防止两个用户有相同 email：

```sql
CREATE INDEX users_email_idx
ON users (email);
```

它只能帮助查询。要保证唯一，必须显式 `UNIQUE` 或定义唯一约束。

第四组边界是 unique index 和 `SELECT ... FOR UPDATE`。锁可以让你在某些流程中串行化操作，但锁的范围取决于你锁到了什么。对于不存在的行，`SELECT FOR UPDATE` 没有行可锁。创建新 key 时，唯一索引才是更直接的保护。

```text
先查再插：查不到时没有锁住“空位”
唯一索引：插入路径会检查 key 空位是否已经被别人占用
```

第五组边界是 unique index 和 Serializable。Serializable 能防止一类读写依赖异常，但它不是替代唯一约束的理由。唯一约束是更窄、更便宜、更清晰的业务不变量表达。比如“用户名不能重复”就应该用唯一约束，不应该只靠 Serializable 事务里的查询判断。

第六组边界是 unique index 和 exclusion constraint。Unique index 检查的是“相等 key 不能重复”。Exclusion constraint 检查的是“任意定义的操作符组合不能同时成立”。时间段 overlap、空间范围相交、IP range 重叠这类规则通常不是 unique index 的工作。

第七组边界是 unique index 和 idempotency。唯一索引可以作为幂等协议的一部分，但幂等还要回答：

```text
同一个 key 搭配不同请求体怎么办？
第一次执行中途崩溃怎么办？
外部副作用已经发生但数据库事务失败怎么办？
重复请求应该返回旧响应、当前状态，还是 409？
幂等记录什么时候过期？
```

如果只写 `ON CONFLICT DO NOTHING`，很多边界没有被定义。

第八组边界是 NULL 语义。很多人以为 `UNIQUE(email)` 会阻止多个空 email。PostgreSQL 默认不是这样，多个 `NULL` 不冲突。业务上要限制多个空值，需要 `NULLS NOT DISTINCT`，或者使用 partial unique index：

```sql
CREATE UNIQUE INDEX users_one_null_email_uq
ON users ((1))
WHERE email IS NULL;
```

第九组边界是表达式、排序规则和归一化。`UNIQUE(email)` 和 `UNIQUE(lower(email))` 是不同规则。`lower` 受 collation、数据类型和 Unicode 处理影响。数据库只执行索引定义，不会自动理解“邮箱等价”的全部业务含义。

第十组边界是 INCLUDE 列。PostgreSQL 的 `INCLUDE` 列只是 payload，不参与唯一性判断：

```sql
CREATE UNIQUE INDEX users_email_cover_uq
ON users (email)
INCLUDE (name, created_at);
```

这个索引约束的是 `email`，不是 `(email, name, created_at)`。把 include 列误当成唯一键的一部分，会造成设计错误。

面试里可以这样回答：

```text
unique index 最容易和 unique constraint、primary key、普通 index、锁、Serializable、exclusion constraint、幂等机制混淆。unique constraint 是逻辑约束，PostgreSQL 通常用唯一 B-tree 索引实现；primary key 是唯一且非空的主行身份；普通 index 不阻止重复；锁不等于锁住不存在的 key；Serializable 不替代明确的唯一约束；exclusion constraint 才适合区间重叠；幂等还要处理请求体、状态和外部副作用。另外还要讲清楚 NULL、表达式、partial predicate、collation 和 INCLUDE 列的边界。
```

## Q064. unique index 在高并发场景下可能出现哪些隐藏问题？

高并发下，unique index 的第一个隐藏问题是等待，而不是马上报错。事务 A 插入一个 key 但还没提交，事务 B 插入同一个 key。B 不能立刻判断成功或失败，因为 A 可能提交，也可能回滚。PostgreSQL 的唯一性检查会等待 A 结束，然后重新检查可见性。应用看到的现象可能只是 insert 慢、连接占用、p99 抖动，而日志里未必马上出现 `23505`。

```text
T1: INSERT key='K'; -- 未提交
T2: INSERT key='K'; -- 等待 T1
T1: COMMIT;
T2: 23505 或 ON CONFLICT 分支
```

第二个问题是热点 key。幂等接口、登录注册、秒杀下单、消息重复投递时，大量请求可能打到同一个 unique key。唯一索引把正确性守住了，但所有冲突请求会在同一个 key 上排队。吞吐不会随着并发数线性上升，反而可能因为等待、重试和连接池占满而下降。

第三个问题是重试风暴。客户端超时后重试，网关也重试，消息队列又重投，同一个 idempotency key 被上百个请求同时写入。唯一索引会让其中一个成功，其余冲突或进入 `ON CONFLICT`，但数据库仍然要做索引查找、等待、锁管理、WAL、事务清理。正确性没有坏，容量先被打满。

第四个问题是 `ON CONFLICT DO UPDATE` 造成热点行更新。很多人以为 upsert 冲突时“只是读一下旧行”，实际 `DO UPDATE` 会锁定并更新目标行。哪怕只是刷新 `updated_at`，也会产生新行版本、WAL、索引维护和 autovacuum 压力。

```sql
INSERT INTO counters(key, value)
VALUES ('global', 1)
ON CONFLICT (key)
DO UPDATE SET value = counters.value + 1;
```

这个写法语义正确，但所有请求都更新同一行。瓶颈会落在行锁、WAL、buffer page 和 vacuum 上。

第五个问题是死锁。单个唯一键冲突通常是等待，不一定死锁。但如果一个事务同时插入或更新多个唯一键，另一个事务按相反顺序操作，就可能互等。

```text
T1: 写 unique key A，随后写 B
T2: 写 unique key B，随后写 A
```

这种问题在批量导入、批量 upsert、多唯一约束表里更常见。修复方式通常是稳定排序、缩短事务、减少同一事务内跨 key 操作，必要时用显式锁统一顺序。

第六个问题是 deferred unique constraint 把失败推迟到提交时。如果唯一约束是 deferrable，语句中间可能暂时允许重复，最后 `COMMIT` 才失败。业务如果在事务中间已经做了大量工作，失败代价会很高。更糟的是，开发者看到前面 SQL 都成功，以为已经安全。

第七个问题是 partial unique index 的谓词只约束一部分行。并发更新如果把行从不受约束状态改成受约束状态，冲突可能出现在状态切换时：

```sql
CREATE UNIQUE INDEX subscriptions_active_uq
ON subscriptions (user_id)
WHERE status = 'active';
```

两个事务分别把两条历史记录改成 `active`，最终只有一个能成功。应用要把这个错误当作业务冲突处理，而不是数据库异常。

第八个问题是 `CREATE UNIQUE INDEX CONCURRENTLY` 的中间状态。PostgreSQL 文档说明，并发创建唯一索引时，第二次扫描开始后唯一性约束已经可能对其他事务生效，即使索引还没有变成可用于查询的 valid index；如果构建失败，还可能留下 invalid index，并继续带来更新维护成本，甚至在某些情况下继续执行唯一性检查。生产迁移里这点很容易踩坑。

第九个问题是索引页和 heap 可见性检查成本。唯一性检查不能只看索引项，还要确认冲突 tuple 是否对 MVCC 来说仍然有效。热点写入、长事务、未清理 dead tuple、索引膨胀都会让冲突检查更贵。

第十个问题是错误处理路径不统一。有的代码捕获 `23505`，有的代码用 `ON CONFLICT DO NOTHING`，有的代码把冲突转成 500。高并发下相同业务冲突会表现成不同用户体验。正确做法是统一约束名、错误码和业务响应。

线上症状通常是这些：

```text
INSERT / UPSERT p99 上升
pg_stat_activity 里等待 transactionid、tuple、lock 或 IO
23505、40P01、40001 偶发或集中出现
连接池被占满，但 CPU 不一定满
autovacuum 变忙，表和索引 bloat 增长
同一个 key 的请求大量重试
CREATE UNIQUE INDEX CONCURRENTLY 失败后留下 INVALID index
```

面试里可以这样回答：

```text
unique index 在高并发下隐藏问题主要是冲突等待、热点 key 排队、重试风暴、upsert 热点行更新、批量多 key 操作死锁、deferred constraint 提交时失败、partial unique index 在状态切换时冲突、并发建唯一索引的 invalid index 和提前约束生效，以及 MVCC 可见性检查带来的额外成本。它能保证正确性，但不能消除容量瓶颈。工程上要监控 lock wait、23505/40P01/40001、索引膨胀、WAL、autovacuum 和 p99，并把业务冲突转成可预期响应。
```

## Q065. unique index 在崩溃、重启、超时或重试场景下会暴露哪些边界条件？

崩溃恢复下，unique index 的边界来自两个事实：索引是持久化结构，事务又可能在任意点失败。数据库必须保证崩溃后索引和 heap 的关系仍然可恢复，已经提交的唯一性不变量仍然成立，未提交事务的痕迹不能变成可见重复数据。

在 PostgreSQL 里，普通持久表和索引依赖 WAL 恢复。事务提交前崩溃，恢复后它不应该成为有效行；事务提交后崩溃，恢复后它应该能通过 WAL redo 保留下来。唯一索引内部可能留下指向旧版本或已删除版本的索引项，但 MVCC 可见性和 vacuum 会处理这些历史痕迹。唯一性语义看的是 live tuple，不是“索引文件里字节级绝对没有重复 key”。

第一个边界条件是客户端超时不等于数据库回滚。应用发出：

```sql
INSERT INTO users(email) VALUES ('a@example.com');
```

客户端等超时了，但数据库可能随后提交成功。应用重试时收到 `23505`，这不一定表示“别人创建了”，也可能是“自己上一轮其实成功了”。如果业务需要给用户返回同一个结果，最好用幂等键或业务唯一键配合 `RETURNING`、`ON CONFLICT` 查询旧记录。

第二个边界条件是 `23505` 不能一律当成可重试，也不能一律当成不可重试。对“创建用户邮箱”来说，`23505` 可能是用户输入重复，不该自动重试；对“幂等请求记录”来说，`23505` 可能是同一个请求的重复投递，应该读取旧状态。判断依据是唯一键的业务含义。

第三个边界条件是事务中途失败。一个事务插入了唯一键，又做了其他操作，最后回滚。并发事务可能等待它结束。它回滚后，等待者应该能继续插入同一个 key。应用层如果在事务回滚前已经发了外部消息，就会出现“外部世界看到创建成功，数据库里没有记录”的问题。唯一索引救不了这个副作用。

第四个边界条件是 retry 使用了新的随机 key。比如第一次创建订单时生成一个随机 order_id，客户端超时后重试又生成新 order_id。唯一索引只能保证两个 order_id 各自唯一，不能识别它们其实是同一个业务请求。幂等键必须由请求方稳定提供，或者由服务端在请求入口持久化。

第五个边界条件是序列号空洞。PostgreSQL 文档提醒过 sequence 的变化不会随事务回滚而回退。唯一索引经常和 `bigserial` 主键一起出现，事务失败后 id 有空洞是正常现象。不要用“id 连续”表达业务正确性。

第六个边界条件是并发建唯一索引失败。生产迁移常见流程是先清理重复数据，再：

```sql
CREATE UNIQUE INDEX CONCURRENTLY users_email_uq
ON users (email);
```

如果构建期间发现重复值、死锁、表达式计算错误，命令可能失败并留下 `INVALID` index。这个 index 不会用于查询，但可能仍消耗更新维护成本。官方推荐是 drop 掉后重建。对于 unique concurrent build，还要注意约束可能在索引 valid 前已经对其他事务报错。

第七个边界条件是重启后的连接状态丢失。数据库重启会中断连接，应用可能不知道最后一个事务是否提交。正确做法不是盲目重放所有 SQL，而是按业务幂等键检查最终状态：

```text
如果 idempotency_key 已存在并且状态 complete：返回旧结果
如果存在但状态 processing 且超时：进入恢复或抢占流程
如果不存在：重新创建
```

第八个边界条件是 deferrable unique constraint。冲突可能到 `COMMIT` 才暴露。事务前半段做过的读取和计算都要重跑，不能只重试提交动作。

第九个边界条件是主从切换。单机 PostgreSQL 内的唯一索引只保护这个实例确认提交的状态。如果使用异步复制，primary 接受了一个唯一键写入但还没复制到 standby 就故障，standby 被提升后可能看不到这条记录。客户端重试可能再次创建同一个业务对象。解决这个问题要靠同步复制、故障切换策略、fencing、幂等键和对账，不是唯一索引本身能单独解决。

面试里可以这样回答：

```text
unique index 在失败场景下的核心边界是：客户端超时不代表数据库没提交；重试时的 23505 可能是上一轮成功，也可能是真正业务重复；事务回滚会释放唯一键，但外部副作用不会自动回滚；随机生成的新 key 不能提供幂等；sequence 空洞正常；CREATE UNIQUE INDEX CONCURRENTLY 失败可能留下 invalid index；deferrable unique 可能到提交才失败；异步复制 failover 下，唯一性只覆盖已复制并成为新主的数据。可靠设计要用稳定幂等键、状态查询、事务级重试、outbox、迁移清理和故障切换语义一起处理。
```

## Q066. unique index 的性能瓶颈通常来自 CPU、内存、锁竞争、I/O 还是网络？

Unique index 的瓶颈通常不是单一来源。低并发、索引都在缓存里时，成本主要是 CPU 和内存访问：计算索引 key、比较 key、走 B-tree、维护缓冲区。高并发或大数据量时，常见瓶颈会转向锁竞争、I/O、WAL 和 vacuum。网络一般不是数据库内部 unique index 的主要瓶颈，但应用端等待数据库返回时会把它表现成接口延迟。

先看一次插入需要做什么：

```text
计算索引 key
下降 B-tree 找到叶子页
检查是否有重复 key
必要时访问 heap 判断冲突 tuple 是否 live
插入索引项
写 WAL
维护 buffer、page split、统计信息
```

如果 key 是简单整数，CPU 成本很低。如果 key 是长字符串、复杂 collation、表达式索引、JSON 表达式、函数计算，CPU 成本就会上来。比如：

```sql
CREATE UNIQUE INDEX users_email_norm_uq
ON users (tenant_id, lower(trim(email)));
```

每次插入和相关更新都要计算表达式。PostgreSQL 文档也提醒，expression index 的维护成本比普通列索引更高。

内存瓶颈主要是 buffer cache 和维护内存。唯一索引太大，热点页不在缓存里，每次冲突检查都要随机读。索引包含宽列或 `INCLUDE` 过多，会增大叶子页，降低缓存命中率。`CREATE INDEX` 或 `CREATE INDEX CONCURRENTLY` 构建阶段还会受 `maintenance_work_mem`、并行 worker 和排序/构建过程影响。

锁竞争是高并发下最明显的瓶颈。这里的“锁”不只是一把表锁，还包括：

```text
等待未提交冲突事务结束
目标行 tuple lock
B-tree 页面 latch
事务 ID 等待
并发 upsert 的行级锁
死锁检测开销
```

同一个 unique key 被大量写入时，等待会非常集中。数据库可能 CPU 不高，但连接池满了，因为连接都在等同一个事务或同一行。

I/O 瓶颈来自几块。第一是索引随机读写。第二是 WAL。唯一索引插入、更新、page split、`ON CONFLICT DO UPDATE` 产生的新版本都要写 WAL。第三是 vacuum 和 bloat。频繁 upsert 热点行会制造 dead tuple，索引也会膨胀。第四是并发建索引时的全表扫描和两次扫描成本。

网络通常不是唯一索引内部瓶颈，但有两个间接问题。第一，应用把每次冲突都做成多轮 SQL：

```text
SELECT 是否存在
INSERT
失败后 SELECT
```

这会增加网络往返。第二，数据库等待期间连接被占用，服务端线程或协程也被挂住，最终表现成 API 超时。

不同场景的主瓶颈可以这样判断：

```text
CPU 高，lock wait 低：表达式、collation、索引比较、SQL 执行本身
IO wait 高，buffer hit 低：索引太大、随机读、建索引、checkpoint/WAL 压力
lock wait 高，CPU 不高：热点 key、upsert 行锁、未提交事务等待
WAL 写入高：大量 insert/update/upsert、page split、索引太多
autovacuum 忙：upsert churn、长事务、dead tuple 清理不及时
网络往返多：应用层先查再写或冲突后多次补查
```

优化也要对症。热点 key 冲突不能靠加 CPU 解决；表达式索引慢不能靠调 lock timeout 解决；索引太宽导致 I/O 慢，盲目加连接只会更糟。

常见优化方向包括：

```text
把唯一键设计得稳定且短
避免把宽列放进唯一索引 key
谨慎使用 INCLUDE
避免无意义 DO UPDATE，比如只为刷新 updated_at
批量 upsert 前先按 key 去重
给冲突 key 分散写入路径，必要时分片或按业务拆热点
缩短事务，避免持有未提交唯一键太久
清理重复数据后再建唯一索引
生产大表用 CREATE UNIQUE INDEX CONCURRENTLY，并监控 invalid index
监控 WAL、bloat、autovacuum 和 lock wait
```

面试里可以这样回答：

```text
unique index 的性能瓶颈通常来自锁竞争、I/O 和 WAL，其次是 CPU 与内存。简单 key、缓存命中高时，成本是 B-tree 查找和 key 比较；表达式唯一索引、复杂 collation 会增加 CPU；索引大或 page split 多时会变成随机 I/O 和 WAL；高并发同 key 写入时，主要瓶颈是等待未提交事务、tuple lock、事务 ID 和 B-tree 页竞争。网络不是索引内部瓶颈，但应用层多轮 SELECT/INSERT/补查会放大延迟。优化要先看 lock wait、buffer hit、WAL、bloat 和 p99，而不是只说加索引或加机器。
```

## Q067. unique index 的 correctness test、stress test 和 benchmark 应该分别测什么？

Correctness test 要测的是唯一性语义有没有被破坏。重点不是“能不能建索引”，而是各种边界下是否仍然只有合法状态能提交。

基础 correctness 用例包括：

```text
插入两个相同 key，第二个必须失败
插入两个不同 key，都应成功
更新 key 到已有 key，必须失败
更新非 key 列，不应误报唯一冲突
删除旧行后插入同 key，应成功
同一事务内插入后回滚，其他事务应能插入同 key
```

组合键要单独测：

```text
UNIQUE (tenant_id, email)
(1, a@example.com) 与 (1, a@example.com) 冲突
(1, a@example.com) 与 (2, a@example.com) 不冲突
(1, a@example.com) 与 (1, b@example.com) 不冲突
```

NULL 语义必须测。PostgreSQL 默认多个 `NULL` 不冲突；`NULLS NOT DISTINCT` 下多个 `NULL` 应冲突。多列唯一里，只要有列为 `NULL`，默认行为也容易和直觉不一致。

表达式唯一要测归一化：

```text
Alice@example.com
alice@example.com
```

如果索引是 `lower(email)`，它们应冲突；如果是原始 `email`，它们不一定冲突。还要测试空格、Unicode、collation 和不可变函数约束。

Partial unique index 要测谓词边界：

```sql
CREATE UNIQUE INDEX one_active_uq
ON subscriptions (user_id)
WHERE status = 'active';
```

需要覆盖：

```text
两个 active 同 user 冲突
一个 active 一个 canceled 不冲突
两个 canceled 不冲突
canceled 更新成 active 时触发冲突
active 更新成 canceled 后释放唯一名额
```

MVCC correctness 要测并发可见性：

```text
T1 插入 key K 未提交，T2 插入 K 应等待
T1 提交后，T2 应失败或进入 ON CONFLICT
T1 回滚后，T2 应成功
T1 删除 K 未提交，T2 插入 K 应等待
T1 删除提交后，T2 应成功
T1 删除回滚后，T2 应失败
```

Deferrable unique constraint 要测提交时失败：

```text
事务中间暂时重复
约束延迟到 COMMIT 检查
COMMIT 时如果仍重复，应失败
如果事务内最终消除重复，应成功
```

Crash/recovery correctness 如果是自研存储或数据库内核实现，要测：

```text
插入唯一键后崩溃，已提交记录恢复后仍存在
未提交插入崩溃后不可见
索引项和 heap 不一致时能恢复或重建
page split 中途崩溃后 B-tree 结构仍可遍历
```

Stress test 要测的是高并发交错下有没有死锁、等待爆炸、错误分类混乱或罕见竞态。典型压测模型：

```text
1000 个并发事务插入同一个 key
1000 个并发事务插入不同 key
高比例重复 key + 低比例新 key
批量 INSERT 中同一批次有重复 key
多唯一索引表，事务按不同顺序更新多个 key
partial unique index 状态来回切换
CREATE UNIQUE INDEX CONCURRENTLY 期间持续写入
客户端超时后重试
```

Stress test 不只看最终数据，还要看：

```text
23505 数量是否符合预期
40P01 deadlock 是否可解释
40001 是否被事务级重试处理
是否有连接池耗尽
是否有长时间 transactionid wait
是否出现 invalid index
是否有重试风暴
```

Benchmark 要测性能曲线，不要只给一个 QPS。至少要分几组：

```text
无唯一索引 insert 基线
普通唯一索引 insert
组合唯一索引 insert
表达式唯一索引 insert
partial unique index insert/update
ON CONFLICT DO NOTHING
ON CONFLICT DO UPDATE
热点 key 冲突
随机 key 写入
批量写入
```

指标要包括：

```text
吞吐：rows/s、transactions/s
延迟：p50、p95、p99、最大值
错误：23505、40P01、40001、cardinality violation
等待：lock wait、transactionid wait、tuple wait
资源：CPU、buffer hit、read/write IOPS、WAL bytes
存储：table/index size、bloat、dead tuples
维护：autovacuum 次数和耗时
迁移：CREATE UNIQUE INDEX CONCURRENTLY 总时长和失败恢复时间
```

如果要做面试里的测试设计，可以把它讲成三层：

```text
correctness test 证明不会提交重复的 live key
stress test 证明并发等待、死锁、重试和迁移边界可控
benchmark 量化不同 key 分布、冲突率和索引形态下的吞吐、延迟和资源成本
```

面试里可以这样回答：

```text
unique index 的 correctness test 要覆盖重复插入、更新冲突、删除后复用、组合键、NULLS DISTINCT/NOT DISTINCT、表达式唯一、partial unique、deferrable constraint、MVCC 下未提交插入/删除的等待和提交/回滚分支，以及崩溃恢复后的索引和 heap 一致性。stress test 要用大量并发同 key、不同 key、批量重复 key、多唯一索引反向操作、状态切换、并发建索引和超时重试去找等待、死锁、invalid index 和错误处理问题。benchmark 要量化吞吐、p95/p99、23505/40P01/40001、lock wait、WAL、I/O、CPU、索引大小、bloat 和 autovacuum，而不是只测一条插入语句的平均耗时。
```

## Q068. 如果要求从零实现一个简化版 unique index，你会先定义哪些不变量？

从零实现 unique index，先不要急着写 B-tree。应该先定义唯一性语义和崩溃语义。数据结构可以换，关键不变量不能含糊。

第一个不变量：同一唯一作用域内，任意时刻不能提交两个 live row 拥有相同 logical key。

```text
live(row1) && live(row2) && key(row1) == key(row2) => row1 == row2
```

如果系统支持 MVCC，这里的 live 必须相对提交历史和 snapshot 定义。物理上可以有多个版本，逻辑上不能让同一个 snapshot 同时看到两个相同 key。

第二个不变量：key 提取必须确定。唯一索引不能依赖会变的函数、外部表、当前时间、随机数。PostgreSQL 要求 index expression 和 partial index predicate 里的函数是 immutable，本质上就是为了让索引里的 key 和之后的检查保持同一个语义。

```text
key(row, schema_version, collation) 必须稳定
```

如果 collation 或归一化规则变了，需要重建索引或明确迁移。

第三个不变量：NULL 语义要固定。实现前必须定义：

```text
NULL 是否等于 NULL？
多列 key 中部分 NULL 如何比较？
排序 NULLS FIRST/LAST 是否影响唯一性？
```

在 PostgreSQL 默认唯一索引里，`NULL` 不等于 `NULL`；`NULLS NOT DISTINCT` 会改变这个语义。排序位置和唯一性比较也不能混在一起。

第四个不变量：插入和唯一性检查必须是一个原子临界区。不能先查索引，再释放锁，再插入。两个并发写者会同时看到空位。

简化实现可以这样想：

```text
lock bucket/page for key
check committed/live owner
register pending insert
write heap row and index entry
commit publishes owner
unlock
```

真实数据库会更复杂，但“不允许检查和插入之间被别人插队”这个原则不能破。

第五个不变量：未提交冲突必须有等待和重检规则。看到相同 key 的 pending row 时，不能直接报重复，也不能直接放行。要等对方 commit/abort：

```text
对方 commit  : 当前事务失败或走 conflict action
对方 abort   : 当前事务重新检查后可继续
对方删除同 key: 还要看删除是否提交
```

这对应 PostgreSQL 文档里 unique check 对未提交插入和未提交删除的处理。

第六个不变量：更新 key 等价于删除旧 key、插入新 key。更新非 key 列不应该制造唯一冲突；更新 key 时必须检查新 key 是否可用。MVCC 系统里，同一逻辑行更新可能留下多个物理版本，不能把自己的旧版本误判成冲突。

第七个不变量：索引和主表必须可恢复一致。崩溃可能发生在这些位置：

```text
heap row 已写，index entry 未写
index entry 已写，commit record 未写
page split 写了一半
事务提交了，部分脏页没落盘
```

简化实现至少要用 WAL 或 copy-on-write 让恢复后满足：

```text
已提交 row 能被索引找到
未提交 row 不作为 live unique owner
索引结构可遍历
重复 live key 不会被恢复出来
```

第八个不变量：删除和 GC 不能过早破坏正在进行的读写。一个事务可能正在通过索引项去 heap 检查可见性，vacuum 或 compaction 不能把对应版本提前回收。PostgreSQL 的索引锁文档也提到 index scan、heap tuple、VACUUM 之间需要额外规则避免读到错误 tuple。

第九个不变量：并发控制要有固定锁顺序。多 key 插入、批量 upsert、多唯一索引更新都可能产生死锁。简化系统至少要：

```text
按 key 排序加锁
同一事务内锁顺序稳定
支持死锁检测或超时回滚
失败后释放 pending owner
```

第十个不变量：错误必须可解释。唯一冲突要能告诉上层是哪个 constraint/index 失败，最好有稳定错误码。否则应用无法区分“业务重复”“幂等重放”“系统暂时冲突”。

第十一个不变量：如果支持 partial unique index，谓词和 key 必须一起纳入语义。

```text
predicate(row) == false 的行不参与唯一性
row 从 false 更新成 true 时要检查冲突
row 从 true 更新成 false 时释放唯一名额
predicate 也必须确定
```

第十二个不变量：如果支持 deferred unique constraint，提交前必须重新检查。语句中间允许的重复不能泄漏到提交历史。

简化伪代码可以写成：

```text
insert(tx, row):
  k = compute_key(row)
  if predicate(row) == false:
      insert_heap_only(row)
      return

  lock_key(k)
  owner = unique_map[k]
  if owner == none:
      unique_map[k] = pending(tx, row)
      insert_heap_and_index(row)
      return

  if owner.tx is committed live:
      error unique_violation

  if owner.tx is in_progress:
      wait owner.tx
      retry insert(tx, row)

  if owner.tx aborted or owner row is dead:
      retry insert(tx, row)
```

提交时：

```text
commit(tx):
  flush WAL
  publish all pending unique owners as committed
  release locks
```

回滚时：

```text
abort(tx):
  mark heap rows aborted/dead
  remove or ignore pending unique owners
  release locks
```

面试里可以这样回答：

```text
从零实现 unique index，我会先定义这些不变量：同一作用域内不能提交两个相同 logical key 的 live row；key 和 partial predicate 必须确定；NULL 语义固定；唯一性检查和插入必须原子化；遇到未提交冲突要等待并重检；更新 key 等价于删除旧 key 再插入新 key；崩溃恢复后 heap 与 index 一致，未提交记录不能变成 live owner；GC 不能提前删除仍可能被检查的版本；多 key 操作要有稳定锁顺序；唯一冲突要有稳定错误码；partial unique 和 deferrable unique 要有额外的谓词切换和提交重检规则。数据结构可以先用 hash map 或 B-tree，但这些语义不变量要先定死。
```

## Q069. unique index 的常见误用是什么，误用后通常会产生什么线上症状？

第一种误用是用 `SELECT` 预检查替代唯一索引。

```sql
SELECT 1 FROM users WHERE email = $1;
-- 没有再 INSERT
```

低并发测试没问题，高并发就会重复。症状是偶发两个相同业务对象、后续流程报“期望一条结果却查到多条”、人工清理重复数据。

第二种误用是唯一作用域漏字段。多租户系统里只写：

```sql
CREATE UNIQUE INDEX users_email_uq ON users (email);
```

如果业务要求租户内唯一，这会误伤不同租户；如果业务要求全局唯一，却写成 `(tenant_id, email)`，会允许跨租户重复。症状是注册失败、导入失败、或者跨租户账号冲突。

第三种误用是忽略 NULL 语义。以为 `UNIQUE(email)` 能阻止多个空 email，结果数据库允许多条 `email IS NULL`。症状是“唯一约束明明在，为什么还有多条未绑定邮箱记录”。修复要看业务：`NOT NULL`、`NULLS NOT DISTINCT`、partial unique index，三者含义不同。

第四种误用是软删除没加 partial predicate。

```sql
CREATE UNIQUE INDEX users_email_uq ON users (email);
```

用户删除账号后想重新注册同一邮箱，但旧行还在表里，只是 `deleted_at` 不为空。症状是用户无法复用邮箱。常见修复：

```sql
CREATE UNIQUE INDEX users_active_email_uq
ON users (email)
WHERE deleted_at IS NULL;
```

第五种误用是大小写或归一化规则没写进索引。业务上 `Alice@example.com` 和 `alice@example.com` 应该相同，数据库却建了原始值唯一索引。症状是登录、找回密码、第三方回调时查到多个“同一邮箱”的账号。

第六种误用是把 mutable 展示字段设成唯一。昵称、标题、备注、人名这类字段容易变化，也可能天然重复。症状是用户修改失败、后台导入大量 `23505`、客服不得不手工改字段。

第七种误用是用 unique index 表达它表达不了的规则。比如“同一会议室时间段不能重叠”，只建 `(room_id, start_time)` 唯一。它只能阻止相同开始时间，阻止不了区间交叉。症状是预订系统出现重叠订单。应该考虑 exclusion constraint 或专门的事务锁设计。

第八种误用是手工重复建索引。定义了 unique constraint 后，又创建一个同列普通 index：

```sql
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
CREATE INDEX users_email_idx ON users (email);
```

PostgreSQL 文档明确提醒，不需要给唯一列再手工创建重复索引。症状是写入变慢、WAL 增多、磁盘占用增大、vacuum 成本增加，但查询计划未必更好。

第九种误用是 migration 前不清理历史重复数据。大表上线 unique index 时直接执行，构建到一半失败，甚至留下 invalid index。症状是发布失败、后续写入变慢、`\d` 看到 `INVALID` index、迁移工具状态卡住。

第十种误用是把 `CREATE INDEX IF NOT EXISTS` 当作定义校验。PostgreSQL 文档说明，`IF NOT EXISTS` 只是不因同名对象报错，不保证已有索引定义符合你想要的定义。症状是迁移显示成功，实际索引列、谓词、唯一性、排序规则都可能不对。

第十一种误用是在分区表或分库分表里误以为局部唯一等于全局唯一。PostgreSQL 分区表要求唯一约束包含所有分区键。分库分表系统如果每个 shard 独立建 `UNIQUE(email)`，只能保证 shard 内唯一。症状是跨分片重复账号，或者上线全局查询后发现同一 key 多条。

第十二种误用是盲目 `ON CONFLICT DO NOTHING`。冲突发生时什么都不做，看起来接口成功，实际业务对象没创建。症状是用户点击后无结果、上游以为提交成功、数据库没有新记录，排查时只看到冲突被吞掉。

第十三种误用是 `ON CONFLICT DO UPDATE` 无条件覆盖。重复请求带着旧 payload，又把新状态覆盖回旧状态。症状是状态回退、字段莫名被旧值覆盖、审计日志显示同一行被频繁 update。

第十四种误用是没有统一处理 `23505`。有的路径返回 409，有的返回 500，有的无限重试。症状是相同重复请求在不同接口表现不一致，错误率随流量上升。

第十五种误用是把唯一键做得太宽。把长 JSON、长文本、多列 payload 都放进唯一索引。症状是索引膨胀、插入慢、缓存命中差，甚至单条索引项超过 B-tree 限制导致写入失败。

面试里可以这样回答：

```text
unique index 常见误用包括用 SELECT 预检查替代约束、漏掉 tenant 等作用域字段、误解 NULL、软删除没有 partial unique、大小写归一化没进表达式索引、把昵称标题这类可变字段设成唯一、用唯一索引表达区间不重叠、重复建普通索引、迁移前不清理历史重复、误用 IF NOT EXISTS、分片内唯一冒充全局唯一、DO NOTHING 吞业务冲突、DO UPDATE 无条件覆盖、23505 处理不统一，以及唯一键太宽。线上症状通常是重复数据、误报冲突、注册或导入失败、invalid index、写入变慢、WAL 和 bloat 增长、状态被旧请求覆盖、接口 500 或 409 混乱。
```

## Q070. unique index 在单机和分布式环境中的语义有什么差异？

单机 PostgreSQL 里，unique index 的语义边界很清楚：一个数据库实例、一个表或一个分区约束定义范围内，数据库内核能在写入路径上检查冲突。事务状态、索引页、heap 可见性、WAL、锁等待都在同一个系统里协调。

例如：

```sql
CREATE UNIQUE INDEX users_email_uq
ON users (email);
```

只要所有写入都进入这张表，同一个 PostgreSQL 实例就能保证 `email` 不会出现两个 live row。应用有多少个实例无所谓，后台脚本也无所谓，只要它们写同一个约束边界，就绕不过数据库。

分布式环境的第一层差异是分区表。PostgreSQL 分区表上的唯一约束不是随便定义的。官方 `CREATE TABLE` 文档说明，对多层分区层次建立唯一约束时，目标分区表及其子分区表的所有分区键都必须包含在约束定义里。原因很直接：如果唯一键不包含分区键，两个相同 key 可能落到不同分区，单个分区索引无法判断全局冲突。

```text
PARTITION BY RANGE (created_at)
UNIQUE (email)                 -- 无法只靠本地分区索引保证全局 email 唯一
UNIQUE (created_at, email)     -- 包含分区键，冲突能落在可检查范围内
```

这会影响建模。按时间分区的用户表想保证 email 全局唯一，本身就和分区策略冲突。解决方法可能是换分区键、单独建全局用户表、使用查重服务，或者接受 email 不是这个分区表里的全局唯一约束。

第二层差异是分库分表。每个 shard 上的唯一索引只保证本 shard 内唯一：

```text
shard 1: UNIQUE(email)
shard 2: UNIQUE(email)
```

如果路由规则是按 `user_id`，同一个 email 可能被路由到不同 shard。要保证 email 全局唯一，常见方案有几种：

```text
按 email 路由，让相同 email 总到同一 shard
单独维护全局唯一表或注册中心
使用全局二级索引，并用事务/共识维护
用外部分配器生成不可冲突 ID
接受最终一致，再做异步冲突检测和补偿
```

这些方案的语义和成本差别很大。按 email 路由简单，但不一定符合查询主路径；全局唯一表会成为热点；全局二级索引需要跨节点事务；异步补偿不能给用户同步唯一保证。

第三层差异是复制。异步主从复制下，primary 上唯一约束已经成功，standby 可能还没回放到这条 WAL。读 follower 时看不到刚写入的唯一键，不代表唯一约束失效，而是读请求不在同一个新鲜度边界内。

```text
写 primary: INSERT email='a@example.com' 成功
立刻读 follower: 可能查不到
再次发创建请求到 primary: 会被唯一约束拦住
如果 failover 到没追上的 standby: 语义取决于复制和切换策略
```

第四层差异是 failover。异步复制里，旧 primary 已确认给客户端的提交如果还没复制到新 primary，就可能丢失。此时客户端重试同一个业务创建，新 primary 可能接受。对用户来说，这像是唯一键“失忆”了。根因不是 B-tree unique index 坏了，而是故障切换承诺没有覆盖已确认提交。要用同步复制、提交确认策略、fencing、旧主隔离和对账降低这个风险。

第五层差异是多主写入。两个 region 都能写同一个逻辑表，如果没有共识或全局冲突检测，就可能同时接受同一个 key。后续复制冲突时再解决，已经不是强唯一，而是最终冲突处理。冲突策略可能是 last-write-wins、保留两条、人工合并或业务补偿。面试里要明确：最终一致复制下的“唯一”往往不是同步唯一约束。

第六层差异是分布式 SQL 数据库。有些系统提供全局唯一索引，但它背后通常要用 range lease、Raft/Paxos、timestamp oracle、2PC 或事务协调器。语义更强，延迟也更高。一次唯一检查可能要访问索引分片、数据分片和事务协调器，不再是单机内存里的 B-tree 检查。

第七层差异是外部系统。单机数据库唯一索引只能约束数据库表。分布式业务里还常有缓存、搜索索引、消息队列、对象存储、第三方支付。数据库里唯一，不代表外部副作用唯一。仍然需要 outbox、inbox、幂等 key、去重表和对账。

判断一个系统的 unique index 是否满足需求，要问这些问题：

```text
唯一性范围是单表、单分区、单 shard、单 region，还是全局？
路由键是否包含唯一键？
读写是否都走 primary？
failover 是否可能丢失已确认提交？
是否多主写入？
全局唯一索引是否由共识或事务维护？
冲突是同步拒绝，还是异步发现后补偿？
```

面试里可以这样回答：

```text
单机 unique index 的语义由一个数据库实例统一维护，事务状态、heap 可见性、索引检查和 WAL 都在同一边界内。分布式环境要先问唯一性范围。PostgreSQL 分区表上的唯一约束必须包含分区键；分库分表里每个 shard 的唯一索引只保证本 shard 内唯一，除非按唯一键路由或引入全局唯一表/全局二级索引；异步复制和 follower read 会带来读陈旧，failover 可能丢失未复制的已确认写；多主写入如果没有共识，只能做异步冲突处理。分布式全局唯一可以做，但要付出跨节点协调、2PC/共识和更高延迟，不能把单机唯一索引的语义直接套过去。
```

## Q071. upsert 的核心目标是什么，它主要解决正确性、性能、安全性还是可维护性问题？

Upsert 的核心目标是把“如果不存在就插入，如果已经存在就按规则处理”做成一个数据库原子语句。它主要解决正确性问题，也提升代码可维护性；性能有时会更好，因为少了一些应用层往返，但在高冲突场景下不一定更快。安全性不是它的主要目标。

PostgreSQL 的写法是：

```sql
INSERT INTO users (tenant_id, email, name)
VALUES (1, 'a@example.com', 'Alice')
ON CONFLICT (tenant_id, email)
DO UPDATE SET name = EXCLUDED.name
RETURNING id, tenant_id, email, name;
```

这里的 `ON CONFLICT` 不是语法糖那么简单。官方 `INSERT` 文档说明，对于每一行候选插入，要么插入成功，要么在违反仲裁约束或索引时执行替代动作；`ON CONFLICT DO UPDATE` 在高并发下保证原子的 `INSERT` 或 `UPDATE` 结果。也就是说，它解决的是“先查再插/改”的竞态。

没有 upsert 时，很多代码会写成：

```text
SELECT id FROM users WHERE tenant_id = 1 AND email = 'a@example.com';
如果没有：INSERT
如果有：UPDATE
```

并发下两个事务都可能查不到，然后同时插入。最终要么重复数据，要么其中一个插入失败后应用再补救。Upsert 把这个分支交给数据库的唯一索引仲裁。

Upsert 的正确性依赖冲突目标。PostgreSQL 不能凭空知道“存在”的含义。它需要唯一索引、唯一约束或排他约束作为 arbiter：

```sql
ON CONFLICT (tenant_id, email)
```

或者：

```sql
ON CONFLICT ON CONSTRAINT users_tenant_email_key
```

如果没有合适的唯一约束，upsert 就没有可靠仲裁对象。把任意 `WHERE` 条件理解成 upsert 条件是误解。

`DO NOTHING` 和 `DO UPDATE` 的目标也不同。

```sql
ON CONFLICT (request_id) DO NOTHING
```

适合“重复请求不需要改变已有记录”的场景。但它会让冲突行不返回，除非额外查询。对于需要返回已存在对象的 API，要设计好返回路径。

```sql
ON CONFLICT (request_id)
DO UPDATE SET last_seen_at = now()
```

适合冲突时确实需要修改已有行。注意这会锁行并产生更新版本，不是零成本。

Upsert 对可维护性的帮助在于减少分支代码。应用不再散落着“先查、判断、插入、失败后再查、再更新”的流程，冲突处理集中在一条 SQL 或少数 DAO 方法里。约束名也可以成为业务错误映射的一部分：

```text
users_tenant_email_key -> 邮箱已被使用
orders_external_id_key -> 外部订单已导入
idempotency_key_key    -> 重复请求，读取旧结果
```

它不自动保证幂等。下面这条语句每次重复请求都会更新 `updated_at`：

```sql
INSERT INTO jobs(idempotency_key, payload, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (idempotency_key)
DO UPDATE SET payload = EXCLUDED.payload, updated_at = now();
```

如果重复请求的 payload 不同，这条 SQL 甚至会覆盖旧请求。真正幂等还要校验 payload hash、状态机和返回结果。

面试里可以这样回答：

```text
upsert 的核心目标是让“插入或冲突后处理”在数据库里成为原子操作，主要解决正确性问题，避免应用层先 SELECT 再 INSERT/UPDATE 的竞态。PostgreSQL 通过 INSERT ... ON CONFLICT 基于唯一索引、唯一约束或排他约束选择冲突分支；DO UPDATE 在高并发下保证每个候选行要么插入要么更新。它也能减少应用层分支，提高可维护性，但高冲突时会有锁等待和更新成本。它不是安全机制，也不是完整幂等协议，冲突键、更新条件、返回语义和副作用仍要业务层定义清楚。
```

## Q072. upsert 的典型适用场景和不适用场景分别是什么？

Upsert 适合“同一个业务 key 的创建请求可能重复到达，但数据库里最终只需要一条记录”的场景。它的优势在于把重复创建、重复导入、重复投递这类竞态压缩到唯一约束和一个写语句里。

第一类场景是幂等创建。比如创建订单时客户端带 `idempotency_key`：

```sql
INSERT INTO idempotency_records (
  tenant_id, operation, idempotency_key, payload_hash, status
)
VALUES ($1, 'create_order', $2, $3, 'processing')
ON CONFLICT (tenant_id, operation, idempotency_key)
DO NOTHING
RETURNING id;
```

如果返回一行，说明当前请求抢到了执行权；如果没有返回，说明已有请求在处理或已完成，应用再读取旧记录。这里 `DO NOTHING` 比 `DO UPDATE` 更安全，因为重复请求不应该随便覆盖首次请求状态。

第二类场景是外部事件去重。消息队列、Webhook、CDC 事件都可能重复投递：

```sql
INSERT INTO inbox_events(source, event_id, received_at)
VALUES ($1, $2, now())
ON CONFLICT (source, event_id) DO NOTHING;
```

插入成功才处理事件；冲突则跳过或读取已有处理状态。

第三类场景是缓存型或投影型表。比如用户资料投影、统计快照、物化视图增量：

```sql
INSERT INTO user_profiles(user_id, display_name, avatar_url)
VALUES ($1, $2, $3)
ON CONFLICT (user_id)
DO UPDATE SET
  display_name = EXCLUDED.display_name,
  avatar_url = EXCLUDED.avatar_url;
```

这种表的语义通常是“以最新事件为准”，upsert 很自然。但如果事件可能乱序，就要加版本条件。

第四类场景是计数或累加，但要小心热点：

```sql
INSERT INTO daily_counts(day, key, count)
VALUES ($1, $2, 1)
ON CONFLICT (day, key)
DO UPDATE SET count = daily_counts.count + 1;
```

这能保证并发累加不丢，但同一个 `(day, key)` 会变成热点行。高吞吐计数通常要分桶、异步聚合或使用专门的计数系统。

第五类场景是“首次写入，后续只刷新元数据”。比如设备 last_seen：

```sql
INSERT INTO devices(device_id, first_seen_at, last_seen_at)
VALUES ($1, now(), now())
ON CONFLICT (device_id)
DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at;
```

这里重复上报本来就要更新 `last_seen_at`，upsert 语义清晰。

不适用场景也要明确。

第一，不适合没有唯一仲裁键的业务。比如“找一个状态为 pending 的任务，如果没有就创建”。这个条件不是一个唯一 key。你需要任务队列模型、锁、`SKIP LOCKED`、Serializable 或约束重构，而不是硬套 upsert。

第二，不适合复杂跨行不变量。比如“一个部门所有员工工资总和不能超过预算”。Upsert 只能围绕某个冲突行做插入或更新，不能自动保护聚合约束。

第三，不适合冲突时有不可重复外部副作用的流程。比如冲突分支里已经调用支付、发券、发邮件。数据库语句可以回滚，外部副作用不会跟着回滚。应改成事务内写 outbox，事务后异步发送。

第四，不适合无条件覆盖重要字段。重复请求、旧事件、乱序消息都可能带旧值：

```sql
DO UPDATE SET status = EXCLUDED.status
```

如果没有版本条件，旧事件可能把 `shipped` 覆盖回 `paid`。应加：

```sql
WHERE orders.version < EXCLUDED.version
```

并处理 `RETURNING` 为空的情况。

第五，不适合高冲突热点计数直接打单行。语义正确，容量不一定够。症状是 tuple lock 等待、WAL 高、autovacuum 忙。

第六，不适合同一条 `INSERT` 语句里候选行自己重复的批量导入。PostgreSQL 文档说明 `ON CONFLICT DO UPDATE` 是 deterministic statement，不允许同一现有行在同一语句里被影响多次。如果 `VALUES` 或 `INSERT ... SELECT` 里有重复 key，可能直接报 cardinality violation。批量 upsert 前要先去重。

第七，不适合把冲突当成功但又不读结果。`DO NOTHING` 后如果没有 `RETURNING`，应用可能不知道自己是插入成功还是被跳过。接口语义要求明确时，要读取已有行或返回 409。

第八，不适合替代审计和业务校验。Upsert 可以写入或更新，但不会自动检查调用者是否允许覆盖这条记录。

面试里可以这样回答：

```text
upsert 适合幂等创建、外部事件去重、投影表刷新、设备 last_seen、缓存表、低到中等冲突的累加和“有明确唯一键的创建或更新”。不适合没有唯一仲裁键的条件创建、跨行聚合约束、带外部副作用的流程、乱序事件无条件覆盖、高热点单行计数、批量输入自身重复、DO NOTHING 后又不处理返回结果，以及权限和审计校验。判断标准是：能否用唯一约束定义冲突，冲突后更新是否真的是业务想要的状态转移。
```

## Q073. upsert 和相近概念最容易混淆的边界在哪里？

第一组边界是 upsert 和“先查再写”。这两段逻辑看起来等价：

```text
SELECT
不存在则 INSERT
存在则 UPDATE
```

和：

```sql
INSERT ...
ON CONFLICT (...)
DO UPDATE ...
```

但并发语义不同。先查再写中间有空窗，另一个事务可以插入同一个 key。Upsert 依赖唯一索引在写路径里仲裁，冲突检查和动作选择在一个语句内完成。

第二组边界是 upsert 和 `MERGE`。PostgreSQL 文档明确说，带 `INSERT` 和 `UPDATE` 的 `MERGE` 看起来类似 `INSERT ... ON CONFLICT DO UPDATE`，但不保证一定发生插入或更新；如果 `MERGE` 尝试插入时遇到并发唯一冲突，会报唯一性错误，不会像 `ON CONFLICT` 那样重新走冲突分支。`MERGE` 更通用，upsert 更专注于唯一冲突上的原子插入/更新。

第三组边界是 upsert 和 MySQL 的 `REPLACE`。`REPLACE` 在一些数据库里是删除旧行再插入新行，可能触发删除级联、改变主键、重置默认值。PostgreSQL 的 `ON CONFLICT DO UPDATE` 是更新已有行，不是 delete + insert。把两者当成一样，会在外键、触发器、审计字段上出问题。

第四组边界是 `DO NOTHING` 和“成功”。`DO NOTHING` 只是冲突时不插入，不表示业务操作成功。比如创建订单接口：

```sql
INSERT INTO orders(request_id, amount)
VALUES ($1, $2)
ON CONFLICT (request_id) DO NOTHING;
```

如果没有检查 `RETURNING` 或行数，应用可能把重复请求当作新订单创建成功。正确语义可能是返回旧订单、返回处理中、返回参数冲突，或者返回 409。

第五组边界是 `DO UPDATE` 和幂等。很多 upsert 不是幂等的：

```sql
DO UPDATE SET retry_count = jobs.retry_count + 1
```

重复执行会改变结果。即使 key 相同，也不等于幂等。幂等通常要求相同请求重复执行得到同一业务结果，而不是每次都更新一遍。

第六组边界是 upsert 和乐观锁。乐观锁通常用 version 或 revision 防止覆盖：

```sql
UPDATE docs
SET body = $new_body, version = version + 1
WHERE id = $id AND version = $expected_version;
```

Upsert 的冲突目标是唯一键，不是“版本匹配”。当然可以在 `DO UPDATE WHERE` 里加版本条件：

```sql
ON CONFLICT (id)
DO UPDATE SET
  body = EXCLUDED.body,
  version = docs.version + 1
WHERE docs.version = EXCLUDED.expected_version;
```

但如果 `WHERE` 不满足，行会被锁住但不更新，`RETURNING` 也可能没有结果。应用必须处理这个分支。

第七组边界是 conflict target 和任意条件。`ON CONFLICT` 只能处理唯一约束、唯一索引或排他约束冲突。它不能因为 `CHECK` 失败、外键失败、业务 `WHERE` 不匹配就自动转入更新分支。

第八组边界是 partial unique index 推断。PostgreSQL 支持按 conflict target 推断唯一索引，也支持 `index_predicate`。但如果 partial predicate 没有匹配，推断可能失败，或者选中非 partial unique index。复杂场景下显式写 `ON CONSTRAINT constraint_name` 更稳，前提是它确实是可作为仲裁器的约束。

第九组边界是 `EXCLUDED`。`EXCLUDED` 不是目标表里的旧行，它表示本来要插入的候选行。`DO UPDATE` 里同时能访问旧行和 `EXCLUDED`：

```sql
DO UPDATE SET
  count = counters.count + EXCLUDED.count
```

如果误把 `EXCLUDED` 当成当前数据库状态，就会写出覆盖 bug。

第十组边界是 trigger。PostgreSQL 文档说明，per-row `BEFORE INSERT` trigger 的影响会反映在 `EXCLUDED` 值里，因为这些效果可能参与了冲突判断。也就是说，upsert 不是简单拿客户端传入值做冲突分支，触发器可能已经改过候选行。

第十一组边界是批量 upsert 的 deterministic 限制。同一条语句不能让同一个现有行被影响多次。输入数据要先按冲突键去重，不要把这个工作完全丢给数据库。

面试里可以这样回答：

```text
upsert 最容易和先查再写、MERGE、REPLACE、DO NOTHING 成功语义、幂等、乐观锁和任意条件更新混淆。PostgreSQL 的 INSERT ... ON CONFLICT 是基于唯一/排他仲裁器的一条原子语句；MERGE 更通用但没有相同的并发唯一冲突保证；DO NOTHING 只是跳过插入，不等于业务成功；DO UPDATE 会锁行并更新，不天然幂等；乐观锁要靠 version 条件单独表达；EXCLUDED 是候选插入行，不是旧行；批量 upsert 还要避免同一语句内重复命中同一目标行。
```

## Q074. upsert 在高并发场景下可能出现哪些隐藏问题？

Upsert 在高并发下最常见的隐藏问题是热点行串行化。`ON CONFLICT DO UPDATE` 碰到同一个 key 时，会围绕同一行排队。语义正确，但并发度会被压成接近单行更新。

```sql
INSERT INTO daily_counts(day, name, count)
VALUES (current_date, 'login', 1)
ON CONFLICT (day, name)
DO UPDATE SET count = daily_counts.count + 1;
```

如果所有登录都更新同一行，数据库必须按顺序修改这行。CPU 可能不高，p99 却很差，因为请求都在等 tuple lock。

第二个问题是 Read Committed 下的可见性让人意外。PostgreSQL 文档说明，在 Read Committed 中，`INSERT ... ON CONFLICT DO UPDATE` 的每个候选行要么插入要么更新；如果冲突来自另一个尚不可见的事务，`UPDATE` 分支仍然可能作用到那一行。应用不要假设“我的 snapshot 看不见它，所以不会更新它”。这是 upsert 为了保证原子结果做出的并发语义。

第三个问题是最后写入覆盖。多个请求用同一个 key upsert 同一行：

```sql
ON CONFLICT (id)
DO UPDATE SET status = EXCLUDED.status;
```

如果请求乱序，旧状态可能覆盖新状态。修复一般要加版本、时间戳或状态机条件：

```sql
DO UPDATE SET status = EXCLUDED.status, version = EXCLUDED.version
WHERE target.version < EXCLUDED.version;
```

然后必须检查 `RETURNING`，因为条件不满足时不会更新。

第四个问题是 `DO UPDATE WHERE` 锁行但不更新。PostgreSQL 文档提到，如果冲突行被锁住但 `WHERE` 条件不满足，它不会被更新，也不会出现在 `RETURNING` 里。应用如果只看 SQL 没报错，可能误以为更新成功。

```sql
INSERT INTO docs(id, body, version)
VALUES ($1, $2, $expected_version + 1)
ON CONFLICT (id)
DO UPDATE SET body = EXCLUDED.body, version = EXCLUDED.version
WHERE docs.version = $expected_version
RETURNING *;
```

返回 0 行意味着乐观锁失败或冲突条件不满足，不是数据库异常。

第五个问题是批量输入自身重复。比如：

```sql
INSERT INTO users(email, name)
VALUES
  ('a@example.com', 'Alice'),
  ('a@example.com', 'Alicia')
ON CONFLICT (email)
DO UPDATE SET name = EXCLUDED.name;
```

这不是“最后一条赢”这么简单。PostgreSQL 要求 `ON CONFLICT DO UPDATE` 是 deterministic，同一现有行不能在同一语句里被影响多次，可能直接报 cardinality violation。批量 upsert 前要在临时表或应用层按 key 去重并明确胜出规则。

第六个问题是死锁。两个事务各自 upsert 多个 key，但顺序不同：

```text
T1: upsert A -> upsert B
T2: upsert B -> upsert A
```

如果冲突分支要更新已有行，就可能互相等待。批量任务应按冲突键排序，或者拆小事务。

第七个问题是 `DO NOTHING` 掩盖冲突。高并发下大量请求被跳过，但应用没有统计 affected rows，也没有 `RETURNING`，业务层以为都成功。症状是上游成功数和数据库新增数对不上。

第八个问题是无意义更新制造 bloat。很多代码为了拿到旧行，写：

```sql
ON CONFLICT (email)
DO UPDATE SET email = EXCLUDED.email
RETURNING id;
```

这会把冲突行也更新一遍，产生新版本和 WAL。对于高频重复请求，这种“空更新”成本很高。可以改成冲突后单独查询、使用 CTE，或者只在确实需要修改时更新。具体取舍看一致性和延迟要求。

第九个问题是触发器和副作用。`DO UPDATE` 会触发 UPDATE 相关触发器。高并发重复 upsert 可能导致审计日志膨胀、缓存失效消息重复发送、更新时间不断变化。触发器里如果调用外部系统，问题更严重。

第十个问题是索引选择和 conflict target 不清晰。表上有多个唯一索引时，`ON CONFLICT (col)` 可能通过索引推断选择仲裁器；如果 partial index、表达式 index、collation 混在一起，最好明确约束名或完整 conflict target。否则迁移新增索引后，SQL 行为和错误位置可能变得难以理解。

第十一个问题是事务太长。Upsert 冲突行后，事务还继续做很多事，目标行锁会被持有更久。其他请求都堵在同一个 key 上。把外部调用、复杂计算、大批量处理放在 upsert 事务里，会放大等待。

第十二个问题是自动重试扩大冲突。`40001`、`40P01` 或客户端超时后，所有请求立即重试，又打到同一个 key。没有退避和幂等状态读取时，数据库会被重复冲突拖垮。

第十三个问题是分区或分片路由。Upsert 的冲突仲裁只在目标表/分区/分片能看到的唯一约束范围内发生。分布式系统里如果相同 key 可能路由到不同 shard，本地 upsert 不会提供全局唯一。

第十四个问题是返回语义。高并发接口最好明确区分：

```text
inserted：当前请求创建了新行
updated：当前请求更新了已有行
skipped：冲突但条件不满足或 DO NOTHING
stale：版本太旧，没有更新
duplicate_payload_mismatch：同一幂等键但请求体不同
```

只返回“成功”会让排查很难。

面试里可以这样回答：

```text
upsert 高并发下的隐藏问题包括热点 key 或热点行串行化、Read Committed 下更新到当前语句 snapshot 看不见的冲突行、最后写入覆盖、DO UPDATE WHERE 锁行但不更新、批量输入自身重复导致 cardinality violation、多 key 顺序不同造成死锁、DO NOTHING 掩盖业务冲突、空更新制造 WAL 和 bloat、触发器副作用重复、多个唯一索引下仲裁目标不清晰、长事务持锁、自动重试风暴、分片内 upsert 冒充全局 upsert，以及返回语义不区分 inserted/updated/skipped/stale。upsert 解决的是原子冲突分支，不是容量、顺序、幂等和副作用的全部问题。
```

## 参考资料

- PostgreSQL Documentation, [Transactions](https://www.postgresql.org/docs/current/tutorial-transactions.html)
- PostgreSQL Documentation, [Concurrency Control: Introduction](https://www.postgresql.org/docs/current/mvcc-intro.html)
- PostgreSQL Documentation, [Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html)
- PostgreSQL Documentation, [Explicit Locking](https://www.postgresql.org/docs/current/explicit-locking.html)
- PostgreSQL Documentation, [Data Consistency Checks at the Application Level](https://www.postgresql.org/docs/current/applevel-consistency.html)
- PostgreSQL Documentation, [Serialization Failure Handling](https://www.postgresql.org/docs/current/mvcc-serialization-failure-handling.html)
- PostgreSQL Documentation, [Concurrency Control Caveats](https://www.postgresql.org/docs/current/mvcc-caveats.html)
- PostgreSQL Documentation, [Locking and Indexes](https://www.postgresql.org/docs/current/locking-indexes.html)
- PostgreSQL Documentation, [SELECT](https://www.postgresql.org/docs/current/sql-select.html)
- PostgreSQL Documentation, [EXPLAIN](https://www.postgresql.org/docs/current/sql-explain.html)
- PostgreSQL Documentation, [Using EXPLAIN](https://www.postgresql.org/docs/current/using-explain.html)
- PostgreSQL Documentation, [pg_stat_statements](https://www.postgresql.org/docs/current/pgstatstatements.html)
- PostgreSQL Documentation, [auto_explain](https://www.postgresql.org/docs/current/auto-explain.html)
- PostgreSQL Documentation, [Monitoring Database Activity](https://www.postgresql.org/docs/current/monitoring.html)
- PostgreSQL Documentation, [The Cumulative Statistics System](https://www.postgresql.org/docs/current/monitoring-stats.html)
- PostgreSQL Documentation, [Viewing Locks](https://www.postgresql.org/docs/current/monitoring-locks.html)
- PostgreSQL Documentation, [Progress Reporting](https://www.postgresql.org/docs/current/progress-reporting.html)
- PostgreSQL Documentation, [Write-Ahead Logging](https://www.postgresql.org/docs/current/wal-intro.html)
- PostgreSQL Documentation, [Write Ahead Log Settings](https://www.postgresql.org/docs/current/runtime-config-wal.html)
- PostgreSQL Documentation, [High Availability, Load Balancing, and Replication](https://www.postgresql.org/docs/current/high-availability.html)
- PostgreSQL Documentation, [Log-Shipping Standby Servers](https://www.postgresql.org/docs/current/warm-standby.html)
- PostgreSQL Documentation, [Failover](https://www.postgresql.org/docs/current/warm-standby-failover.html)
- PostgreSQL Documentation, [Hot Standby](https://www.postgresql.org/docs/current/hot-standby.html)
- PostgreSQL Documentation, [Replication Configuration](https://www.postgresql.org/docs/current/runtime-config-replication.html)
- PostgreSQL Documentation, [System Administration Functions](https://www.postgresql.org/docs/current/functions-admin.html)
- PostgreSQL Documentation, [Unique Indexes](https://www.postgresql.org/docs/current/indexes-unique.html)
- PostgreSQL Documentation, [Constraints](https://www.postgresql.org/docs/current/ddl-constraints.html)
- PostgreSQL Documentation, [Index Uniqueness Checks](https://www.postgresql.org/docs/current/index-unique-checks.html)
- PostgreSQL Documentation, [Index Locking Considerations](https://www.postgresql.org/docs/current/index-locking.html)
- PostgreSQL Documentation, [Partial Indexes](https://www.postgresql.org/docs/current/indexes-partial.html)
- PostgreSQL Documentation, [Indexes on Expressions](https://www.postgresql.org/docs/current/indexes-expressional.html)
- PostgreSQL Documentation, [INSERT](https://www.postgresql.org/docs/current/sql-insert.html)
- PostgreSQL Documentation, [Indexes](https://www.postgresql.org/docs/current/indexes.html)
- PostgreSQL Documentation, [Index Types](https://www.postgresql.org/docs/current/indexes-types.html)
- PostgreSQL Documentation, [Multicolumn Indexes](https://www.postgresql.org/docs/current/indexes-multicolumn.html)
- PostgreSQL Documentation, [Index-Only Scans and Covering Indexes](https://www.postgresql.org/docs/current/indexes-index-only-scans.html)
- PostgreSQL Documentation, [Visibility Map](https://www.postgresql.org/docs/current/storage-vm.html)
- PostgreSQL Documentation, [Routine Vacuuming](https://www.postgresql.org/docs/current/routine-vacuuming.html)
- PostgreSQL Documentation, [VACUUM](https://www.postgresql.org/docs/current/sql-vacuum.html)
- PostgreSQL Documentation, [WAL Configuration](https://www.postgresql.org/docs/current/wal-configuration.html)
- PostgreSQL Documentation, [Connections and Authentication](https://www.postgresql.org/docs/current/runtime-config-connection.html)
- PostgreSQL Documentation, [Resource Consumption](https://www.postgresql.org/docs/current/runtime-config-resource.html)
- PgBouncer Documentation, [Features](https://www.pgbouncer.org/features.html)
- PgBouncer Documentation, [Configuration](https://www.pgbouncer.org/config.html)
- PostgreSQL Documentation, [PREPARE TRANSACTION](https://www.postgresql.org/docs/current/sql-prepare-transaction.html)
- PostgreSQL Documentation, [Two-Phase Transactions](https://www.postgresql.org/docs/current/two-phase.html)
- PostgreSQL Documentation, [COMMIT PREPARED](https://www.postgresql.org/docs/current/sql-commit-prepared.html)
- PostgreSQL Documentation, [ROLLBACK PREPARED](https://www.postgresql.org/docs/current/sql-rollback-prepared.html)
- PostgreSQL Documentation, [ALTER TABLE](https://www.postgresql.org/docs/current/sql-altertable.html)
- PostgreSQL Documentation, [Modifying Tables](https://www.postgresql.org/docs/current/ddl-alter.html)
- PostgreSQL Documentation, [CREATE INDEX](https://www.postgresql.org/docs/current/sql-createindex.html)
- PostgreSQL Documentation, [CREATE TABLE](https://www.postgresql.org/docs/current/sql-createtable.html)
- PostgreSQL Documentation, [Table Partitioning](https://www.postgresql.org/docs/current/ddl-partitioning.html)
- PostgreSQL Documentation, [DROP INDEX](https://www.postgresql.org/docs/current/sql-dropindex.html)
- Hal Berenson, Phil Bernstein, Jim Gray, Jim Melton, Elizabeth O'Neil, Patrick O'Neil, [A Critique of ANSI SQL Isolation Levels](https://arxiv.org/abs/cs/0701157), SIGMOD 1995.
- Dan R. K. Ports and Kevin Grittner, [Serializable Snapshot Isolation in PostgreSQL](https://arxiv.org/abs/1208.4179), VLDB 2012.
- Hector Garcia-Molina and Kenneth Salem, [Sagas](https://www.cs.cornell.edu/andru/cs711/2002fa/reading/sagas.pdf), ACM SIGMOD 1987.
