# System Design Interview: Senior Engineer Preparation Guide (Part 3/3)

> Topics 17-25: Notifications, Config Center, Service Registry, Rate Limiting, Log Collection, Monitoring, Canary, Feature Flags, Rollback

---

## 17. Notification System Design

### Core Concepts
A notification platform sits between producer services and delivery providers (APNs, FCM, SES, Twilio). Producers publish a logical notification; the system resolves recipients, splits per channel, applies preferences, and ensures at-least-once delivery.

### Architecture
Producers -> Notification API (validate, enqueue, idempotency) -> Kafka (partitioned by user_id) -> Preference resolver -> Channel Workers (push, email, SMS, in-app) -> External providers -> Delivery log

### Key Components
| Component | Responsibility | Technology |
|-----------|---------------|------------|
| Notification API | Single entry point, returns 202 Accepted | Stateless service |
| Message Queue | Durable buffer, decouples intake from delivery | Kafka (partitioned by user_id) |
| Preference Service | Per-user opt-in/out, quiet hours, channel prefs | PostgreSQL + Redis cache |
| Channel Workers | Push (APNs/FCM), Email (SES/SendGrid), SMS (Twilio) | Independent worker pools |
| Delivery Log | Track every send, retry, outcome | Cassandra or DynamoDB |
| Dead Letter Queue | Failed messages after retries exhausted | Kafka DLQ |

### Priority Levels
| Priority | Examples | Target | Strategy |
|----------|----------|--------|----------|
| P0 - Transactional | OTP, password reset, payment confirmation | p99 < 5s | Dedicated workers, bypass batching |
| P1 - User-facing | Chat message, comment reply | p99 < 60s | Standard pipeline, dedup 10s |
| P2 - Bulk/Marketing | Weekly digest, promotional | Hours | Aggressive batching, quiet hours |

### Channel Fallback Strategy
1. Try push (cheapest, lowest friction). If device acknowledges within 60s, stop.
2. Else try in-app inbox + email after delay.
3. Escalate to SMS only for time-critical events.

### Key Design Decisions
- **Idempotency**: Each event has an idempotency key. Dedup window in Redis.
- **Rate limiting**: Per-provider token bucket. SES has MaxSendRate limits.
- **Templating**: Store templates with variable substitution. Render at send time.
- **Webhooks**: Provider callbacks update delivery status (delivered, bounced, complained).

**Sources:** Systems Explained; System Design Handbook; Ajit Singh; AbstractAlgorithms

---

## 18. Configuration Center Design

### Core Concepts
A configuration management service stores application config (DB connection strings, feature toggles, rate limit thresholds) in a centralized, distributed key-value store.

### Core Requirements
- **Key-value storage** with namespacing (e.g., /production/database/url)
- **Strong consistency**: all clients read the same value after write is acknowledged
- **Watch notifications**: clients subscribe to key prefixes, receive push on changes
- **High availability**: survive minority node failures
- **Access control**: per-namespace read/write permissions

### Watch Mechanism (The Killer Feature)
1. Client opens long-lived gRPC stream to config service
2. Client calls Watch(prefix) - server registers watch in watch registry
3. When any key under prefix is written, server fans out to all matching watches
4. Changed key, new value, and version number sent over registered streams
5. On reconnect, client sends last received revision; server replays missed events

### Technology Comparison
| Feature | etcd | ZooKeeper | Consul |
|---------|------|-----------|--------|
| Consensus | Raft | ZAB | Raft |
| API | Simple KV | Hierarchical znodes | KV + DNS + HTTP |
| Performance | 1000s writes/sec | Hundreds/sec | 1000s writes/sec |
| Language | Go | Java | Go |
| Kubernetes | Default data store | Legacy | Service mesh (Connect) |
| Watch support | Yes (gRPC stream) | Yes | Yes |

**Sources:** etcd.io (official docs); TechInterview.org; Medium (Consul vs etcd vs ZooKeeper)

---

## 19. Service Registry Design

### Core Concepts
In microservices, instances are ephemeral (auto-scaling, container restarts, rolling updates). A service registry tracks which instances are live, healthy, and ready to accept traffic.

### Registration Patterns
| Pattern | How It Works | Example |
|---------|-------------|---------|
| Self-registration | Service registers itself on startup, sends heartbeats | Netflix Eureka client |
| Third-party registration | External registrar registers services | Kubernetes, Consul agent |

### Discovery Patterns
| Pattern | How It Works | Example |
|---------|-------------|---------|
| Client-side | Client queries registry, selects instance, calls directly | Eureka + Ribbon |
| Server-side | Client calls load balancer, LB queries registry and forwards | AWS ELB, Kubernetes Services |

### CAP Trade-off: AP vs CP Registries
| Registry | CAP Choice | Behavior During Partition |
|----------|-----------|--------------------------|
| Netflix Eureka | AP (Available) | Serves stale data, self-preservation mode |
| HashiCorp Consul | CP (Consistent) | Returns errors if no quorum |
| etcd | CP (Consistent) | Returns errors if no quorum |
| ZooKeeper | CP (Consistent) | Returns errors if no quorum |

### Health Checking
- **Heartbeat**: Service sends periodic heartbeats (e.g., every 30s)
- **Health endpoint**: Registry probes /health endpoint (HTTP, TCP, gRPC)
- **Deregistration**: If N consecutive checks fail, service is removed from registry

**Sources:** Microservices.io; HashHackers; Uplatz; TechInterview.org

---

## 20. Rate Limiting System Design

### Core Concepts
Rate limiting allows N requests per time window T. The algorithm is the easy part. The hard part is enforcing a single logical limit across a fleet of gateway nodes.

### Algorithm Comparison
| Algorithm | State per key | Burst | Accuracy | Atomicity Cost |
|-----------|-------------|-------|----------|----------------|
| Token bucket | 2 fields (tokens, last_refill) | Configurable | Exact (with Lua) | EVALSHA (Lua) |
| Fixed window | 1 counter | 2x boundary burst | Approximate | INCR + EXPIRE |
| Sliding window log | Sorted set (all timestamps) | None | Exact | ZADD + ZRANGEBYSCORE |
| Sliding window counter | 2 integers | None | ~0.003% error | INCR + GET |
| Leaky bucket | Queue + drain | None | Smooth output | Background drip |
| GCRA | 1 scalar (TAT) | Configurable | Exact | EVALSHA (Lua) |

**Recommendation**: Token bucket for user-facing APIs (burst tolerance), sliding window counter for strict endpoint caps.

### Distributed Enforcement (The Hard Part)
Two gateway pods see two requests for same user simultaneously. Both read bucket, both find 1 token, both allow. User gets 2 instead of 1.

**Solution: Two-tier scheme (Cloudflare, Stripe, Envoy pattern)**
1. Each gateway borrows a chunk of tokens from global Redis bucket (e.g., 10 tokens)
2. Gateway consumes locally until chunk exhausted or expires (5s TTL)
3. When local chunk runs out, borrow another. If global bucket empty, borrow fails.
4. Result: 10x reduction in Redis traffic, ~6% accuracy error

### Atomicity Options
| Option | How | Trade-off |
|--------|-----|-----------|
| Redis Lua | One EVAL script computes refill + decrement + write | ~50us per script, single round-trip |
| Redis WATCH/MULTI | Optimistic locking, retry on conflict | Two round-trips on success |
| DB row-level lock | SELECT FOR UPDATE | ~10x slower than Redis |

### Fail-Open vs Fail-Closed
A rate limiter must **fail open** - never become the outage it was designed to prevent. If Redis is down, gateways should allow requests (with local conservative limits) rather than rejecting all traffic.

**Sources:** HLD Handbook (Rate Limiter); semicolony.dev; CalibreOS; BackendBytes

---

## 21. Distributed Log Collection System

### Core Concepts
A centralized logging system collects, indexes, and makes searchable logs from hundreds or thousands of services.

### Collection Architecture
1. Application writes logs to stdout (container best practice)
2. Log collector agent (Fluent Bit, DaemonSet) tails container log files
3. Agent parses JSON, enriches with Kubernetes metadata, forwards to buffer
4. Kafka buffer absorbs traffic spikes, provides durability (7-day retention)
5. Log indexer (Elasticsearch, Loki) receives, indexes, makes searchable

### Indexing Strategy Comparison
| Strategy | How It Works | Storage Cost | Query Speed | Example |
|----------|-------------|-------------|-------------|---------|
| Full inverted index | Index every field | 3-10x raw size | Fastest | Elasticsearch, Splunk |
| Label-only index | Index only labels, grep-scan content | ~1x raw size | Slower (scan) | Grafana Loki |
| Columnar + skip indexes | Columnar with min/max indexes | ~1.5x raw size | Fast for time-range | ClickHouse |

### Agent Comparison
| Agent | Memory | Language | Best For |
|-------|--------|----------|----------|
| Fluent Bit | 10-50MB per node | C | Collection (lightweight) |
| Fluentd | 100-500MB per node | Ruby | Aggregation/routing |
| Promtail | ~50MB per node | Go | Loki-specific |

### Tiered Retention
- **Hot**: SSD storage, 7 days, full indexing
- **Warm**: HDD/object storage, 30 days, reduced indexing
- **Cold**: Object storage (S3/GCS), 1+ years, no indexing (re-hydrate on demand)

**Sources:** TechInterview.org; HLD Handbook (Logging Platform); Sujeet Jaiswal; Sesame Disk

---

## 22. Real-Time Monitoring and Alerting System Design

### Core Concepts
The standard stack: **Prometheus** (metrics collection + TSDB) + **Grafana** (visualization) + **Alertmanager** (alert routing).

### Architecture
Exporters (Node Exporter, cAdvisor, app /metrics) -> Prometheus (scrape every 15s, TSDB storage) -> Grafana (dashboards) + Alertmanager (routing, dedup, notifications)

### Prometheus Pull Model
Prometheus scrapes targets over HTTP - no need to configure each app to push metrics. Service discovery (Kubernetes, Consul) automatically detects targets.

### The 4 Golden Signals (Google SRE)
1. **Latency** - Time to service a request (p50, p95, p99)
2. **Traffic** - Request rate (QPS/RPS)
3. **Errors** - Rate of failed requests (5xx, 4xx, explicit errors)
4. **Saturation** - How full the service is (CPU, memory, queue depth)

### Alerting Best Practices
- **Alert on symptoms, not causes**: "API latency > 500ms for 5 min" not "CPU > 80%"
- **Multi-window burn rate alerts**: Alert only when errors are sustained across multiple time windows
- **Every alert must have a runbook**: Link to remediation document
- **Alert severity tiers**: Critical (page immediately), Warning (investigate within 1hr), Info (business hours)

### Scaling Prometheus
- **Retention**: 30 days local, long-term via Thanos/Mimir/Cortex to S3
- **HA**: Run Prometheus in pairs scraping same targets
- **Federation**: Meta-Prometheus scrapes primary Prometheus for health monitoring

**Sources:** Florian Courouge; KloudVin; TechSaaS; Karthik Hegde; Google SRE Book (Monitoring)

---

## 23. Canary/Gradual Release System

### Core Concepts
A canary release gradually shifts traffic to a new version, limiting blast radius. If metrics degrade, route traffic back to the old version.

### Canary Process
1. Deploy new version to small subset (1-5% of capacity)
2. Route 1-5% of traffic to canary instances
3. Monitor key metrics: error rate, latency p50/p95/p99, business metrics
4. If healthy after observation window (10-30 min), increase to 25%, 50%, 100%
5. If metrics degrade at any stage, route all traffic back to old version

### Deployment Strategy Comparison
| Strategy | How It Works | Rollback | Cost | Complexity |
|----------|-------------|----------|------|------------|
| Rolling | Update instances gradually | Slow (re-deploy old) | Low | Low |
| Blue-Green | Two identical environments | Instant (switch traffic) | High (2x infra) | Medium |
| Canary | Gradual traffic shift | Fast (redirect traffic) | Medium | Medium-High |
| A/B Testing | Route by user attributes | Instant (stop experiment) | Medium | High |

### Kubernetes Implementation
- **Argo Rollouts**: Automates canary process with traffic percentages, observation windows, metric-based promotion/rollback
- **Istio/Linkerd**: Service mesh traffic splitting for fine-grained canary routing
- **NGINX Ingress**: Weighted traffic distribution across versions

**Sources:** ConfigCat; TechInterview.org (Zero-Downtime Deployment); MadDevs; Harness

---

## 24. Feature Flag System Design

### Core Concepts
Feature flags (toggles) decouple deployment from release. Deploy code with feature hidden behind a flag.

### Flag Types (Martin Fowler)
| Type | Duration | Purpose | Cleanup Required |
|------|----------|---------|-----------------|
| Release flag | Days-weeks | Control feature rollout | Yes |
| Experiment flag | Days | A/B testing | Yes |
| Ops flag | Permanent | Operational control (kill switch) | No |
| Permission flag | Permanent | Control access by user tier | No |

### Architecture
Feature Flag Service (stores flag state) -> SDK (evaluates flags locally) -> Application (checks flag before executing new code path)

### Key Design Decisions
- **Local evaluation**: SDK caches flag rules, evaluates locally (sub-millisecond, no network call per check)
- **Push updates**: SDK receives flag changes via streaming (WebSocket/gRPC) - changes take effect in seconds
- **Kill switch**: If newly released feature causes problems, disable flag immediately - no deployment required
- **Technical debt**: Stale flags must be cleaned up. Track creation date, set cleanup reminder.

### Composed Strategy: Canary + Flags
- Canary controls where code runs and tests infra/runtime under real load
- Flags control who sees the behavior and allow instant mitigation
- Start every deployment as canary with all new flags OFF
- If canary is healthy, deploy to all machines, begin flag rollout

**Sources:** ConfigCat; TechInterview.org; MadDevs; Harness; Martin Fowler (Feature Toggles)

---

## 25. Rollback Mechanism Design

### Core Concepts
A rollback reverts a system to a known-good state after a bad deployment. Speed of rollback is measured by Mean Time to Recovery (MTTR).

### Rollback Strategies

**1. Blue-Green Rollback (Fastest)**
- Two identical environments (blue = old, green = new)
- Deploy to green, test, switch traffic to green
- Rollback: switch traffic back to blue (instant, < 1 second)
- Cost: 2x infrastructure

**2. Canary Rollback (Fast)**
- Gradually shift traffic to new version
- Rollback: redirect all traffic back to old version (seconds)
- No extra infrastructure cost

**3. Rolling Rollback (Slow)**
- Re-deploy previous version instance by instance
- Rollback time = number of instances x deploy time
- Slowest but no extra infrastructure

**4. Feature Flag Rollback (Instant)**
- Disable the feature flag (no deployment needed)
- Takes effect in seconds (SDK polling/push)
- Only works if the bad behavior is behind a flag

### Database Rollback Challenges
| Change Type | Rollback Strategy | Complexity |
|-------------|------------------|------------|
| Schema change (add column) | Safe: new code ignores unknown columns | Low |
| Schema change (remove column) | Dangerous: old code may reference removed column | High |
| Schema change (rename column) | Dangerous: use phased approach (add new, migrate, remove old) | High |
| Data migration | Forward-only: cannot easily reverse | Very High |
| Backward-incompatible change | Must be backward-compatible for one deploy cycle | Medium |

### Phased Migration Pattern (Expand-Collapse)
1. **Expand**: Add new column/table. Write to both old and new.
2. **Migrate**: Backfill data. Run dual-reads to verify correctness.
3. **Collapse**: Remove old column/table. Deploy code that only reads from new.

### Automated Rollback Gates
| Gate | Threshold | Action |
|------|-----------|--------|
| Error rate | > 1% 5xx | Auto-rollback |
| Latency p99 | > 500ms for 5 min | Auto-rollback |
| Business metric | Conversion rate drops > 5% | Auto-rollback |
| Resource usage | Memory grows > 10% per min | Auto-rollback |

**Sources:** TechInterview.org (Zero-Downtime Deployment); MadDevs; Harness; Martin Fowler
