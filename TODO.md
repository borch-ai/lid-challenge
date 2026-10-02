# Future Enhancement Roadmap: Option A (Dynamic Feature Toggles & Zero-Downtime Migration)

## 1. Overview & Context

In the current release, **Option B (Deployment-Level & Environment-Variable Toggling)** has been implemented:
- Operators configure persistence modes (`sql_only`, `dual_write`, `dual_write_nosql_primary`, `nosql_only`) via the `PERSISTENCE_MODE` environment variable.
- The `RoutingDAO` coordinates primary and secondary writes, and handles graceful degradation when secondary replication fails.
- The `NoSQLDAO` provides unified document storage semantics, secondary index management, and conforms to the `dao.UserDAO` and `dao.MigratableDAO` interfaces.

**Option A** represents the long-term target architecture: transitioning from static environment-based toggles to **dynamic, runtime-configurable feature flags and an automated zero-downtime migration pipeline**.

---

## 2. Option A Architecture & Improvement Backlog

### Task 1: Dynamic Feature Flag Provider Integration (OpenFeature)
- **Objective**: Decouple configuration changes from process restarts.
- **Implementation**:
  - Integrate an industry-standard dynamic flag client (e.g., [OpenFeature Go SDK](https://github.com/open-feature/go-sdk), LaunchDarkly, Unleash, or cloud-native providers like GCP Cloud Runtime Configuration / AWS AppConfig).
  - Subscribe to real-time configuration events and update `RoutingDAO.SetMode()` on active instances without process disruption or downtime.
  - Add thread-safe atomic pointer swaps (`sync/atomic.Pointer[PersistenceMode]`) to ensure lock-free hot swapping under high read concurrency.

### Task 2: Live Shadow Reads & Automated Parity Verification
- **Objective**: Validate read fidelity and performance between SQL and NoSQL engines before shifting production read traffic.
- **Implementation**:
  - Implement a `ShadowReadDAO` decorator that:
    1. Serves the read request directly from the primary datastore (SQL).
    2. Spawns an asynchronous background worker to execute the identical query against the secondary datastore (NoSQL).
    3. Performs field-level semantic comparison (ignoring benign metadata variations such as clock precision).
    4. Emits Prometheus / OpenTelemetry telemetry metrics:
       - `persistence_shadow_match_count`
       - `persistence_shadow_mismatch_count` (labeled by table/collection and field)
       - `persistence_shadow_latency_seconds` (histogram comparing primary vs secondary response times)
    5. Injects sample mismatches into a telemetry queue for debugging.

### Task 3: Transactional Outbox & Eventual Consistency Pipeline (CDC)
- **Objective**: Prevent split-brain states or lost updates caused by transient network partitions during dual-write.
- **Implementation**:
  - Replace best-effort in-process replication with an **Outbox Pattern** or **Change Data Capture (CDC)**:
    - Primary write executes within an atomic SQL transaction that also writes an outbox event.
    - A dedicated CDC worker (e.g., Debezium, Kafka Connect, or a polling relay) forwards outbox events to a durable message broker (Kafka or Google Cloud Pub/Sub).
    - An idempotent consumer ingests records into the NoSQL store with retry semantics and Dead-Letter Queue (DLQ) support for unprocessable payloads.

### Task 4: Historical Data Backfill & Reconciliation Job
- **Objective**: Migrate historical SQL records created prior to dual-write enablement.
- **Implementation**:
  - Build a CLI subcommand `lid-server backfill --batch-size=1000 --parallelism=4`:
    - Reads chunks of legacy `users`, `user_credentials`, and `user_profiles` records.
    - Transforms relational rows into unified JSON/BSON document representations (`userDocument`).
    - Performs idempotent upserts into the NoSQL collection.
  - Build a reconciliation validator `lid-server reconcile`:
    - Computes cryptographic checksums (SHA-256) of canonicalized records across both stores.
    - Reports discrepancies and missing IDs in a structured audit report.

### Task 5: Granular Canary & Hash-Based Traffic Rollout
- **Objective**: Minimize blast radius during the cutover of read traffic to NoSQL.
- **Implementation**:
  - Add percentage-based or tenant-based routing rules in `RoutingDAO`:
    - Route a percentage `P` (e.g., 1% -> 5% -> 25% -> 50% -> 100%) of reads to NoSQL based on a deterministic hash of `userID` (`crc32(userID) % 100 < P`).
    - Allow header-based overrides (e.g., `X-Persistence-Backend: nosql`) for internal testing and synthetic probe validation.

### Task 6: Automated Circuit Breakers & Rollback Triggers
- **Objective**: Safeguard system availability if the NoSQL cluster encounters latency spikes or resource saturation.
- **Implementation**:
  - Wrap NoSQL operations with a circuit breaker (e.g., `sony/gobreaker`).
  - Automatically flip read routing back to SQL if the secondary error rate exceeds 1% or if p99 latency breaches 250ms over a 1-minute rolling window.
  - Alert on-call engineers via pager duty / alerting webhooks upon circuit breaker trips.

### Task 7: Schema Deprecation & Cleanup
- **Objective**: Decommission relational infrastructure after 100% stable cutover.
- **Implementation**:
  - Switch flag to `nosql_only`.
  - Discontinue dual writes and shut down CDC pipelines.
  - Archive relational databases and execute final cleanup of legacy relational migration files.
