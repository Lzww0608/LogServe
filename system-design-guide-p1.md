# System Design Interview: Senior Engineer Preparation Guide (Part 1/3)

> Topics 1-8: Requirements, Estimation, Scaling, Bottlenecks, Fault Domains

---

## 1. Clarifying Requirements in System Design Interviews

### Core Concepts
The #1 mistake candidates make is diving into a solution before understanding the problem. Alex Xu emphasizes: **"Giving out an answer quickly does not give you bonus points."** The interview is intentionally open-ended.

### The 4-Step Framework (Alex Xu)
| Step | Action | Time |
|------|--------|------|
| 1 | Understand problem and establish design scope | 3-5 min |
| 2 | Propose high-level design and get buy-in | 10-15 min |
| 3 | Design deep-dive | 15-25 min |
| 4 | Wrap-up (bottlenecks, improvements) | 3-5 min |

### Key Clarifying Questions
**Scale & Users:** DAU? Read-to-write ratio? Traffic pattern? 10K or 100M users?
**Functional Scope:** Core vs nice-to-have? Real-time vs batch? Data retention? Multi-tenancy?
**Non-Functional:** Latency p99? Availability SLA? Consistency (strong vs eventual)? Durability?
**Technical:** Existing stack? Budget? Time-to-market?

### Common Pitfalls
| Pitfall | Fix |
|---------|-----|
| Assuming scale | Ask "how many users?" first |
| Skipping edge cases | Walk through concrete use cases |
| Over-engineering | Start simple, add complexity only when justified |
| Silent assumptions | Write assumptions on whiteboard, get buy-in |

**Sources:** Alex Xu Ch.3; Google SRE Workbook (NALSD)

---

## 2. Functional vs Non-Functional Requirements

### Core Concepts
**Functional** = *what* the system does. **Non-Functional (NFRs)** = *how* the system performs.

### NFR Dimensions (ISO 25010)
1. **Performance Efficiency** — latency, throughput, resource utilization
2. **Reliability** — availability, fault tolerance, recoverability
3. **Scalability** — handle growth in users/data/traffic
4. **Security** — auth, authorization, encryption, audit
5. **Maintainability** — modularity, testability, observability
6. **Usability** — API design, error messages, documentation

### NFR Trade-off Matrix
| Requirement | Optimization | Trade-off |
|-------------|-------------|-----------|
| Low latency | Caching, CDN | Stale data, cost |
| High availability | Redundancy, multi-AZ | Cost, complexity |
| Strong consistency | Distributed consensus | Lower throughput, higher latency |
| High durability | Replication (RF=3) | 3x storage cost |

**Sources:** AWS Well-Architected Framework (6 pillars); Google SRE Book (SLOs, Error Budgets)

---

## 3. Back-of-Envelope Calculations

### Core Concepts
BOTE calculations reject infeasible designs and surface dominant costs. **Jeff Dean** advocates evaluating designs using BOTE before building them.

### Essential Latency Numbers (Jeff Dean, 2019)
| Operation | Time | Scaled |
|-----------|------|--------|
| L1 cache reference | 0.5 ns | 0.5s |
| Branch mispredict | 5 ns | 5s |
| L2 cache reference | 7 ns | 7s |
| Mutex lock/unlock | 25 ns | 25s |
| Main memory reference | 100 ns | 100s |
| Compress 1KB with Zippy | 3,000 ns | 50 min |
| Send 2KB over 1 Gbps network | 20,000 ns | 5.6 hrs |
| Read 1MB sequentially from memory | 250,000 ns | 2.9 days |
| Round trip within same datacenter | 500,000 ns | 5.8 days |
| Disk seek | 10,000,000 ns | 4 months |
| Read 1MB sequentially from disk | 30,000,000 ns | 12 months |
| Send packet CA->Netherlands->CA | 150,000,000 ns | 5 years |

### QPS Estimation
```
Average QPS = DAU x requests_per_user_per_day / 86,400
Peak QPS    = Average QPS x Peak Factor (2-10x)

Example (URL Shortener):
  DAU = 100M, Requests/user/day = 10
  Average QPS = 100M x 10 / 86,400 = 11,574 = 12K QPS
  Peak QPS    = 12K x 3 = 36K QPS
```

### Storage Estimation
```
Daily storage   = events_per_day x avg_size_per_event
Annual storage  = daily_storage x 365
5-year storage  = annual_storage x 5 x replication_factor

Example (Twitter):
  100M new tweets/day x 140 bytes = 14 GB/day (raw text)
  With metadata, indexes, replication x 5 = 70 GB/day
  5-year total = 70 GB x 365 x 5 = 128 TB
```

### Bandwidth Estimation
```
Upload bandwidth   = uploads_per_day x avg_size / 86,400
Download bandwidth = upload_bandwidth x read_to_write_ratio

Example (Photo uploads, 1M/day x 200KB):
  Upload   = 1M x 200KB / 86,400 = 2.3 MB/s = 18.4 Mbps
  Download = 2.3 MB/s x 10 = 23 MB/s = 184 Mbps
```

### Reality Adjustment Factors
| Factor | Typical | Source |
|--------|---------|--------|
| Peak factor | 2-10x | Google SRE Workbook |
| Replication factor | ~3x | Dynamo paper, HDFS, Cassandra |
| N+2 redundancy | N+2 | Google SRE Book |
| Metadata/index overhead | 3-5x | Industry practice |

**Sources:** Jeff Dean (Stanford talk); Google SRE Workbook; Alex Xu Ch.2

---

## 4. Deriving Instance Count from QPS

### Core Formula
```
Instance count = Peak QPS / (QPS_per_instance x utilization_target)
utilization_target = 60-80% (leave headroom for spikes)
```

### Typical QPS per Instance
| Service Type | QPS per Core | QPS per 16-core Server |
|-------------|-------------|------------------------|
| Simple API (no DB) | 5,000-10,000 | 40,000-80,000 |
| API + Redis cache | 2,000-5,000 | 16,000-40,000 |
| API + DB query | 500-2,000 | 4,000-16,000 |
| Heavy computation | 50-500 | 400-4,000 |

### Worked Example: URL Shortener
```
Peak QPS = 36,000
QPS per instance = 5,000 (conservative)
Target utilization = 70%

Instance count = 36,000 / (5,000 x 0.7) = 10.3 -> 11 instances
With N+2 redundancy: 13 instances
```

### Connection Pool Sizing
```
DB connections = instances x connections_per_instance (10-50 pooled)
Example: 13 instances x 20 connections = 260 DB connections
PostgreSQL default max_connections = 100 -> Must increase
```

### N+2 Redundancy (Google SRE)
If you need N tasks at peak, run N+2 so a planned upgrade and an unplanned failure don't eat headroom simultaneously.

**Sources:** Google SRE Book (Production Environment); Sujeet Jaiswal (Capacity Planning)

---

## 5. Capacity Planning: Peak vs Average

### Core Concepts
**Average** is for cost estimation. **Peak** is for capacity provisioning. The **peak-to-average ratio** (burst factor) determines how much headroom you need.

### Traffic Pattern Types
| Pattern | Peak-to-Average | Example |
|---------|----------------|---------|
| Steady | 1.5-2x | Enterprise SaaS |
| Diurnal | 2-5x | Social media |
| Event-driven | 10-100x | Flash sale, Super Bowl |
| Viral | 100-1000x | Celebrity tweet |

### The Queueing Knee (Little's Law)
```
As utilization approaches 100%, latency goes to infinity.
The "knee" is around 60-80% utilization.

Queue depth = Utilization / (1 - Utilization)
  At 50%: queue depth = 1
  At 80%: queue depth = 4
  At 90%: queue depth = 9
  At 95%: queue depth = 19
```

### The Cost of Nines
| Availability | Downtime/year | Relative Cost |
|-------------|---------------|---------------|
| 99% | 3.65 days | 1x |
| 99.9% | 8.76 hours | ~2x |
| 99.99% | 52.56 minutes | ~10x |
| 99.999% | 5.26 minutes | ~100x |

Each additional "nine" costs roughly 10x the engineering investment.

**Sources:** Google SRE Book (Capacity Planning); AWS Well-Architected Framework (Reliability)

---

## 6. Horizontal Scaling Design

### Vertical vs Horizontal
| Aspect | Vertical | Horizontal |
|--------|----------|------------|
| What changes | Bigger machine | More machines |
| App changes | None | Stateless design, sharding |
| Cost curve | Exponential | Linear |
| Ceiling | Limited by largest machine | No theoretical limit |
| Downtime | Usually requires restart | Zero-downtime addition |
| Complexity | Low | Higher |

### Foundation: Stateless Services
Any replica can serve any request. State-leaking patterns to avoid:
- In-memory session data (use Redis)
- Local file storage (use S3/object store)
- Sticky sessions (externalize to shared cache)

### Scaling Hierarchy (Senior Engineer's Mental Model)
| Level | Technique | Complexity | Cost |
|-------|-----------|------------|------|
| 1 | Optimize code (fix N+1, add indexes) | Low | $0 |
| 2 | Add caching (absorb 80-95% of reads) | Low | $ |
| 3 | Vertical scaling (bigger server) | None | $$ |
| 4 | Read replicas | Medium | $$ |
| 5 | CDN for static content | Low | $ |
| 6 | Horizontal app servers | Medium | $$ |
| 7 | Database sharding (last resort) | High | $$$$ |

### Amdahl's Law for Systems
```
Speedup = 1 / (1 - P) where P = parallelizable fraction
If 10% is sequential: max speedup = 1/0.10 = 10x
```
Eliminate sequential bottlenecks before adding servers.

### Load Balancing Algorithms
| Algorithm | Best For | Caveat |
|-----------|----------|--------|
| Round robin | Uniform workloads | Doesn't account for load |
| Least connections | Variable durations | Needs connection tracking |
| Consistent hashing | Caches, stateful routing | Complexity |
| Weighted | Heterogeneous servers | Manual tuning |

**Sources:** AWS Well-Architected Framework (Performance Efficiency); Azure Well-Architected Framework; Alex Xu Ch.1

---

## 7. Identifying Single-Point Bottlenecks

### Core Concepts
**Bottleneck** = component whose capacity limits system throughput.
**SPOF** = component whose failure brings down the entire system.

### Bottleneck Identification Framework
Trace a user request through the system. At each step, ask:
1. **CPU-bound?** — Heavy computation, serialization, encryption
2. **Memory-bound?** — Cache misses, large working set, GC pressure
3. **I/O-bound?** — Disk reads/writes, network calls
4. **Connection-bound?** — DB connection pool exhaustion
5. **Contention-bound?** — Lock contention, global mutexes

### Common Bottlenecks (Frequency Order)
| Bottleneck | Symptom | Diagnostic |
|-----------|---------|------------|
| DB connection pool exhaustion | Connection timeouts | Pool metrics |
| Hot key (single shard) | Uneven latency | Per-shard metrics |
| Sync external API (no timeout) | Cascading failures | Distributed tracing |
| Disk I/O on single node | Slow fsync, DB writes | Disk latency |
| Single-threaded code path | CPU at 100% on one core | Flame graphs |

### SPOF Elimination
Run at least 2 app servers behind a load balancer. Use active-passive or active-active load balancers. Database with hot standby. Multiple DNS providers.

**Sources:** System Design Interview Handbook (Scalability); AWS Well-Architected Framework

---

## 8. Fault Domains in High Availability Design

### Core Concepts
A **fault domain** is a set of components that share a single point of failure. When that point fails, all components in the domain are affected.

### Fault Domain Hierarchy
| Domain | Scope | Failure Examples |
|--------|-------|-----------------|
| Process | Single process | Crash, OOM, deadlock |
| Host | Single machine | Hardware failure, kernel panic, power loss |
| Rack | Server rack | Switch failure, power distribution failure |
| Availability Zone | Datacenter cluster | Cooling failure, power grid issue, flood |
| Region | Geographic area | Earthquake, regional network outage |
| Provider | Entire cloud | Control plane failure (rare) |

### Bulkhead Architecture (AWS Well-Architected)
Like ship bulkheads containing a hull breach, fault-isolated boundaries restrict failure effects to a limited set of components.

**Cell-based architecture**: Each "cell" is an independent deployment of the full stack. A failure in one cell doesn't affect others. Used by Amazon, Netflix, and large SaaS providers.

### Shuffle Sharding
Each request is assigned to a subset of instances (e.g., 3 out of 100). A failure in any instance only affects requests that were sharded to it. Dramatically reduces blast radius.

### Static Stability
Design the system so it continues operating without any control plane interaction during a failure. If the orchestrator goes down, running services keep running.

**Sources:** AWS Well-Architected Framework (REL10-BP03 Bulkhead); AWS Fault Isolation Boundaries Whitepaper
