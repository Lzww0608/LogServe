# System Design Interview: Senior Engineer Preparation Guide (Part 2/3)

> Topics 9-16: Multi-AZ/Region, Sync/Async, Read/Write, Hotspots, Flash Sales, URL Shortener

---

## 9. Multi-AZ Deployment Benefits and Trade-offs

### Core Concepts
An **Availability Zone (AZ)** is one or more data centers with independent power, cooling, and networking. AZs are physically separated (miles apart) but close enough for single-digit millisecond latency.

### Multi-AZ Architecture
Deploy application instances across >= 2 AZs. A load balancer distributes traffic across AZs. If one AZ fails, traffic routes to remaining AZs.

### Benefits
| Benefit | Explanation |
|---------|-------------|
| Fault isolation | AZ failures (power, cooling, flood) don't cascade |
| High availability | Survive single-AZ outage with no data loss |
| Rolling updates | Update one AZ at a time, zero downtime |
| Lower latency than multi-Region | AZs within same Region: <2ms RTT |

### Trade-offs
| Trade-off | Impact |
|-----------|--------|
| Cross-AZ data transfer costs | AWS charges for cross-AZ traffic |
| Higher resource footprint | 2x-3x instances for redundancy |
| Application must be AZ-aware | Distribute instances, handle failover |
| Stateful services need replication | DB replication across AZs adds latency |

### AWS Guidance (Well-Architected)
"Deploy and operate all production workloads in at least two Availability Zones (AZs) in a Region." Most customers can achieve resilience goals in a single Region using Multi-AZ.

### Multi-AZ Database Patterns
- **RDS Multi-AZ**: Synchronous standby replica in another AZ. Automatic failover. ~1-2 min downtime on failure.
- **Aurora**: 6 copies across 3 AZs. Up to 15 read replicas across AZs.
- **Cassandra/Scylla**: NetworkTopologyStrategy, RF=3 per DC, spread across AZs.

**Sources:** AWS Well-Architected Framework (REL10-BP01, REL10-BP02); AWS Fault Isolation Boundaries Whitepaper

---

## 10. Multi-Region Deployment Benefits and Trade-offs

### Core Concepts
An **AWS Region** is a physically and logically independent geographic area with >= 3 AZs. Regions are isolated from each other by hundreds of miles.

### When Multi-Region is Needed
- >99.99% availability requirements
- Disaster recovery from Region-wide events (earthquake, regional network outage)
- Data residency/compliance (data must stay in specific geographic boundaries)
- Lower latency for global users (serve from nearest Region)

### Multi-Region Patterns
| Pattern | Description | Complexity |
|---------|-------------|------------|
| Active-Passive | One Region serves traffic, other is standby | Medium |
| Active-Active | All Regions serve traffic simultaneously | High |
| Pilot Light | Minimal standby, scale up on failover | Low |
| Warm Standby | Scaled-down copy running in second Region | Medium |

### Active-Active Challenges
| Challenge | Solution |
|-----------|----------|
| Data replication across Regions | Global Tables (DynamoDB), Aurora Global Database, Cassandra multi-DC |
| Conflict resolution | Last-writer-wins (LWW), CRDTs, application-level reconciliation |
| Global traffic routing | Route53 latency-based routing, Anycast DNS |
| Cross-Region latency | 50-200ms RTT between Regions |
| Idempotency | Every operation must be safe to replay |

### AWS Guidance
"Multi-Region is most common for: regulatory requirements, disaster recovery with bounded RTO, and latency optimization for global users." Most workloads do NOT need multi-Region.

### The Multi-AZ vs Multi-Region Decision
| Factor | Multi-AZ | Multi-Region |
|--------|----------|--------------|
| Max availability | 99.99% | 99.999%+ |
| RTO | Minutes | Minutes to hours |
| RPO | Zero (sync replication) | Seconds to minutes (async) |
| Cost | 2x-3x | 3x-5x+ |
| Complexity | Low-Medium | High |
| Latency to users | Regional | Global |

**Sources:** AWS Well-Architected Framework (REL10-BP01, REL10-BP02); AWS Multi-Region Fundamentals; AWS Architecture Blog

---

## 11. Synchronous vs Asynchronous Communication

### Core Concepts
**Synchronous**: Sender waits for response before continuing. Blocking control flow.
**Asynchronous**: Sender sends message and continues without waiting. Non-blocking.

### Comparison
| Aspect | Synchronous | Asynchronous |
|--------|-------------|--------------|
| Blocking | Yes (waits for response) | No (returns immediately) |
| Failure propagation | Cascades (A fails if B fails) | Isolated (B's failure queued, retried) |
| Latency | Accumulates (100ms x 5 = 500ms) | Minimal to caller |
| Buffering | None | Queue buffers requests |
| Scaling | Harder (scale all together) | Easier (independent scaling) |
| Consistency | Strong | Eventual |

### When to Use Sync
- Critical path operations needing immediate decision
- Auth validation (token valid, user exists)
- Payment processing (need success/failure response)
- Inventory checks (stock availability)
- Keep to minimum: typically 1-2 calls per operation

### When to Use Async
- Email, SMS notifications (can be delayed)
- Analytics, logging, metrics (non-blocking)
- Reporting and data warehouse updates (eventual)
- Image processing, file conversions (background jobs)
- Fan-out patterns (one event triggers multiple handlers)

### The Decision Heuristic
Ask: "Must the response from this call be known and accurate for me to give a meaningful response to the current request?"
- **Yes** -> lean sync (with circuit breakers and timeouts)
- **No** -> candidate for async

### Production Rules
1. Default to async. Use sync only when you need an immediate response.
2. Never chain more than 2 sync calls. If you need A->B->C, redesign.
3. Every sync call needs a circuit breaker and timeout.
4. Every async consumer must be idempotent (safe to process twice).
5. Use dead letter queues for failed messages.
6. If two services always deploy together, they shouldn't be separate services.

**Sources:** AWS Well-Architected Framework (REL04-BP01); ArchMan; Paul Serban; ScaledByDesign

---

## 12. Isolating Critical Paths from Non-Critical Paths

### Core Concepts
The **critical path** is the sequence of operations that must complete synchronously for a user request to succeed. Everything else is non-critical and can be deferred.

### Critical Path Identification
1. Map the user journey. For each step, ask: "Does the user need this to complete before they get a response?"
2. The synchronous core should be as small as possible while preserving UX.
3. Non-critical operations: analytics, logging, notifications, reporting, secondary data enrichment.

### Isolation Techniques
| Technique | How It Works | Example |
|-----------|-------------|---------|
| Message queue | Defer non-critical work to async consumers | Log click event after redirect |
| Background workers | Process in separate thread pool | Send email after order confirmation |
| CQRS | Separate read and write paths | Analytics reads from replica |
| Circuit breaker | Protect critical path from downstream failures | Fail fast if rec service is down |
| Bulkhead | Dedicated thread pool per path type | Separate executors for checkout vs recs |

### Example: URL Shortener
Critical path (sync): Redis lookup -> 302 redirect (sub-5ms)
Non-critical (async): Click event -> Kafka -> Flink -> ClickHouse analytics

### Example: E-commerce Checkout
Critical path (sync): Validate cart -> Reserve inventory -> Process payment -> Create order
Non-critical (async): Send confirmation email -> Update recommendations -> Update analytics

### Anti-Pattern: Critical Path Pollution
Adding non-critical work to the critical path. Symptoms: high p99 latency, cascading failures from non-essential services.

**Sources:** AWS Well-Architected Framework; ScaledByDesign

---

## 13. Read-Heavy vs Write-Heavy System Design Differences

### Core Concepts
| Aspect | Read-Heavy | Write-Heavy |
|--------|------------|-------------|
| Read:Write ratio | 100:1 to 1000:1 | 1:1 to 1:10 |
| Primary bottleneck | Data retrieval latency | Data ingestion throughput |
| Examples | Social feed, product catalog, news | Logging, IoT telemetry, analytics |
| Key optimization | Caching + replication | Partitioning + append-only |

### Read-Heavy Optimization Strategies
| Strategy | How It Works | Impact |
|----------|-------------|--------|
| Caching (Redis, CDN) | Store frequently accessed data in memory | 80-95% read reduction |
| Read replicas | Route reads to replicas, writes to primary | Scale reads horizontally |
| Indexing | Add indexes on frequently queried fields | Faster lookups (but slower writes) |
| Denormalization | Pre-join data for read-optimized views | Faster reads, redundant storage |
| Materialized views | Pre-compute complex queries | Instant query results |

### Write-Heavy Optimization Strategies
| Strategy | How It Works | Impact |
|----------|-------------|--------|
| Append-only storage | Write sequentially, never update in place | 10x write throughput |
| Batching | Group writes into batches | Reduce per-write overhead |
| Asynchronous writes | Queue writes, process in background | Non-blocking UX |
| Sharding | Distribute writes across partitions | Linear write scaling |
| LSM Trees | Log-structured merge trees (Cassandra, RocksDB) | High write throughput |
| Write-ahead log (WAL) | Sequential writes, async flush to SSTables | Durable, fast |

### CQRS (Command Query Responsibility Segregation)
| Concern | Write Side | Read Side |
|---------|-----------|-----------|
| Schema | Normalized (3NF) | Denormalized (read-optimized) |
| DB type | Relational (ACID) | Document store or search index |
| Scale | Shard by entity ID | Replicate widely |
| Consistency | Strong | Eventually consistent |

### The Fundamental Tension
- **Reads want**: caches, replicas, eventual consistency
- **Writes want**: coordination, serialization, confirmation

**Sources:** System Design Handbook; Vivek Molkar; DesignGurus.io; tutorialQ

---

## 14. Hotspot Protection Design

### Core Concepts
A **hot partition** (hot key) occurs when one partition absorbs significantly more traffic than its peers.

### Root Causes of Hotspots
| Root Cause | Example | Mitigation |
|-----------|---------|------------|
| Celebrity/viral key | Taylor Swift's profile | Caching + hybrid fan-out |
| Time-based keys | All writes to today's partition | Compound key with shard suffix |
| Low-cardinality keys | Gender (M/F), status enum | Redesign partition key |
| Sudden traffic spike | Breaking news, flash sale | Adaptive capacity + caching |
| Write-heavy hot key | Single product in flash sale | Write sharding |

### Mitigation Strategies

**1. Caching (for read hotspots)**
Place a cache in front of the hot partition. 95% cache hit rate reduces DB traffic by 95%.

**2. Write Sharding (for write hotspots)**
Append a random suffix (e.g., user_id + random(0-99)) to the partition key. Writes distribute across 100 shards. Reads must scatter-gather.

**3. Hybrid Fan-Out (Twitter pattern)**
- Regular users: fan-out on write (pre-compute feeds)
- Celebrity users (>1M followers): fan-out on read (fetch at query time)

**4. Consistent Hashing with Virtual Nodes**
Distributes load more evenly. Minimal redistribution on node changes.

**5. Adaptive Capacity**
Auto-scale hot partitions. Move data from hot to cold partitions dynamically.

### Detection
Monitor p50 vs p99 latency divergence. If p99 is 10x p50, a hot partition is likely.

**Sources:** SpaceComplexity (Hot Partitions); Twitter engineering blog

---

## 15. Flash Sale / Seckill System Design

### Core Concepts
A flash sale must serve millions of buyers competing for fixed inventory in seconds, with **zero tolerance for overselling**.

### The Four-Layer Architecture
Each layer drops traffic by an order of magnitude:
- Layer 1: CDN Waiting Room (10M -> 1M)
- Layer 2: Token Gate (1M -> 100K)
- Layer 3: Atomic Inventory (100K -> 1K)
- Layer 4: Async Order Queue (1K -> steady DB writes)

### Layer Details

**Layer 1: CDN Waiting Room**
- Static HTML/JS/CSS on CDN (countdown timer runs client-side)
- At T-0, JS initiates queue join request
- Absorbs 10M+ users without hitting origin

**Layer 2: Token Gate (Virtual Queue)**
- Redis sorted set with random score (prevents bot advantage)
- ZADD waitroom:{sale_id} RANDOM_SCORE user_id
- Admit users in batches: ZPOPMIN waitroom:{sale_id} COUNT N
- Admitted users get time-limited session token (30 min)

**Layer 3: Atomic Inventory (Redis + Lua)**
Redis Lua script for atomic decrement:
- GET stock, if <= 0 return -1 (sold out)
- DECR stock
- SADD user to prevent duplicate purchase
- Runs atomically on Redis single-threaded event loop

**Layer 4: Async Order Queue (Kafka)**
- Users who pass inventory gate receive order reference
- Purchase request placed in Kafka queue
- Workers consume at sustainable rate (e.g., 1,000 orders/sec)
- Back-pressure: if queue grows, admit fewer users from waiting room

### Overselling Prevention (4 Patterns)
| Pattern | How | Trade-off |
|---------|-----|-----------|
| DB optimistic lock | UPDATE SET qty=qty-1 WHERE qty>0 | DB bottleneck at scale |
| Redis atomic DECR | Lua script, single-threaded | Hot key on single product |
| Queue-based serialization | All requests through single queue | Throughput limited |
| Pre-allocate per region | Split stock across regions | Regional imbalance |

### Bot Detection
- WAF + custom rules at CDN layer
- Rate limiting per IP
- CAPTCHA for suspicious traffic
- Device fingerprinting

**Sources:** Sujeet Jaiswal; Ajit Singh; CrackingWalnuts; TechInterview.org

---

## 16. URL Shortener System Design

### Core Concepts
A URL shortener is fundamentally a **read-heavy caching problem disguised as a storage problem**. The read-to-write ratio is ~100:1.

### Key Metrics
| Metric | Value |
|--------|-------|
| Read:Write ratio | 100:1 |
| Short code length | 7 characters (Base62) |
| Total possible codes | 62^7 = 3.5 trillion |
| Redirect latency target | p99 < 10ms server-side |

### Short Code Generation (3 Options)

**Option 1: Hash + Collision Detection**
- Hash long URL (MD5, SHA256), take first 7 chars
- Check DB for collision, retry if needed
- Pro: No coordination needed
- Con: DB read per write, birthday paradox collisions at scale

**Option 2: Range-Based ID (Flickr Ticket Server)**
- Pre-allocate ID ranges per server
- Server claims range: UPDATE id_ranges SET next_range = next_range + 1000
- Local counter for next 1000 IDs, encode to Base62
- Pro: Zero per-write coordination, collision-free
- Con: Need ZooKeeper/DB for range management

**Option 3: Snowflake-style ID**
- Timestamp + worker ID + sequence number
- Encode to Base62
- Pro: Globally unique, no coordination per write
- Con: Clock skew issues

### Architecture
Write Path: Client -> Create Service -> Range-based ID generator -> DB -> Cache (Redis)
Read Path: Client -> CDN -> Redirect Service -> Local LRU -> Redis -> DB (on miss) -> 302
Analytics Path (async): Redirect Service -> Kafka -> Flink -> ClickHouse

### 301 vs 302 Redirect
| Status | Behavior | Analytics | Revocation |
|--------|----------|-----------|------------|
| 301 | Browser caches permanently | Lost after first hit | Takes weeks |
| 302 | Every click hits server | Exact | Instant |
| 301 + max-age=90 | Cached for 90s | ~90s lag | ~90s delay |

Bitly uses 301 with Cache-Control: private, max-age=90.

### Cache Strategy
- L1: In-process LRU (singleflight to prevent thundering herd)
- L2: Redis cluster (~150M entries per region)
- L3: DB (Scylla/DynamoDB, RF=3)

### Multi-Region
- Regional counters for ID generation (no cross-region coordination)
- Scylla NetworkTopologyStrategy for cross-DC replication
- Anycast DNS routes users to nearest Region

**Sources:** HLD Handbook (URL Shortener); CrackingWalnuts; Ajit Singh; AbstractAlgorithms
