# Lock Impact Analysis for Backup and Restore

This document analyzes database lock usage during gpbackup and gprestore operations and their impact on online workloads.

---

## 1. gpbackup — Lock Analysis

### 1.1 Lock Type

gpbackup uses **only ACCESS SHARE locks** — the weakest lock level in PostgreSQL/Greenplum.

```sql
-- Source: backup/queries_relations.go:500
LOCK TABLE schema.table IN ACCESS SHARE MODE
```

### 1.2 Lock Acquisition Flow

```
gpbackup starts
│
├─ Phase 1: Worker 0 batch locking (main connection)
│   Executes LOCK TABLE ... IN ACCESS SHARE MODE for ALL tables
│   Batches of 100 tables at a time
│   Locks held until the backup transaction COMMITs
│   ⚠ Blocks if any table has an ACCESS EXCLUSIVE lock (e.g., ALTER TABLE in progress)
│
├─ Phase 2: Workers 1~N per-table locking (parallel connections)
│   Executes LOCK TABLE ... IN ACCESS SHARE MODE NOWAIT for each table
│   NOWAIT mode: returns error immediately if lock cannot be acquired (no waiting)
│   Failed tables are deferred to Worker 0 (which already holds all locks)
│
├─ Phase 3: COPY data export
│   Workers execute COPY table TO PROGRAM ... in parallel
│   COPY implicitly acquires ACCESS SHARE lock (compatible with existing lock)
│
└─ Phase 4: Transaction COMMIT
   All ACCESS SHARE locks released
```

### 1.3 GP Version Differences

| GP Version | Lock Mode | Description |
|-----------|-----------|-------------|
| GP5 / GP6 (<6.21) | `IN ACCESS SHARE MODE NOWAIT` | Lock propagated to all segments |
| GP6 (≥6.21) | `IN ACCESS SHARE MODE NOWAIT MASTER ONLY` | Lock on master only, not propagated |
| GP7+ / Cloudberry | `IN ACCESS SHARE MODE NOWAIT COORDINATOR ONLY` | Same as above, keyword changed |

**GP6.21+ `MASTER ONLY` optimization**: Significantly reduces lock overhead on segments.

### 1.4 Impact on Online Workloads

#### ACCESS SHARE Lock Compatibility Matrix

| Business Operation | Required Lock | Compatible with ACCESS SHARE? | Blocked by gpbackup? |
|-------------------|--------------|------------------------------|---------------------|
| `SELECT` | ACCESS SHARE | ✅ Compatible | **Not blocked** |
| `INSERT` | ROW EXCLUSIVE | ✅ Compatible | **Not blocked** |
| `UPDATE` | ROW EXCLUSIVE | ✅ Compatible | **Not blocked** |
| `DELETE` | ROW EXCLUSIVE | ✅ Compatible | **Not blocked** |
| `CREATE INDEX` | SHARE | ✅ Compatible | **Not blocked** |
| `VACUUM` | SHARE UPDATE EXCLUSIVE | ✅ Compatible | **Not blocked** |
| `ANALYZE` | SHARE UPDATE EXCLUSIVE | ✅ Compatible | **Not blocked** |
| `ALTER TABLE` | ACCESS EXCLUSIVE | ❌ Conflicts | **Blocked** |
| `DROP TABLE` | ACCESS EXCLUSIVE | ❌ Conflicts | **Blocked** |
| `TRUNCATE` | ACCESS EXCLUSIVE | ❌ Conflicts | **Blocked** |
| `VACUUM FULL` | ACCESS EXCLUSIVE | ❌ Conflicts | **Blocked** |
| `REINDEX` | SHARE | ✅ Compatible | **Not blocked** |

#### Summary

> **Normal DML operations (SELECT/INSERT/UPDATE/DELETE) are completely unaffected.**
> **Only DDL operations (ALTER/DROP/TRUNCATE/VACUUM FULL) are blocked until the backup completes.**

### 1.5 Deadlock Prevention

gpbackup has built-in deadlock prevention:

**Problem scenario**:
```
Timeline:
T1: gpbackup Worker 0 acquires ACCESS SHARE on table A (succeeds)
T2: User executes ALTER TABLE A — requests ACCESS EXCLUSIVE (queued, waiting)
T3: gpbackup Worker 1 tries ACCESS SHARE NOWAIT on table A (fails — blocked by ALTER's queue)
```

**Solution**: Workers 1~N use `NOWAIT` mode. On failure, they don't block — the table is deferred to Worker 0, which already holds all locks and won't deadlock.

```go
// Source: backup/data.go:372
func LockTableNoWait(dataTable Table, connNum int) error {
    lockMode = `IN ACCESS SHARE MODE NOWAIT`
    query := fmt.Sprintf("LOCK TABLE %s %s;", dataTable.FQN(), lockMode)
    _, err := connectionPool.Exec(query, connNum)
    // On failure, caller defers table to Worker 0
}
```

### 1.6 Lock Duration

| Phase | Lock Status | Duration |
|-------|------------|----------|
| Metadata backup | Worker 0 holds ACCESS SHARE on all tables | Seconds |
| Data backup | Locks continue to be held | Minutes to hours (depends on data size) |
| Transaction commit | All locks released | Instant |
| **Total lock duration** | **Equals the entire backup duration** | Minutes to hours |

---

## 2. gprestore — Lock Analysis

### 2.1 Restore Phases and Locks

gprestore typically restores to a **new or empty database**, where lock conflicts don't exist. The following analysis applies to restoring into a **database with existing data** (e.g., `--truncate-table` or `--incremental`).

#### Phase 1: Metadata Restore (DDL)

```sql
-- Executes CREATE TABLE, CREATE INDEX, etc.
-- New objects: no conflicts
-- With --truncate-table: TRUNCATE is executed first
```

| Operation | Lock | Impact |
|-----------|------|--------|
| `CREATE TABLE` | ACCESS EXCLUSIVE (new table) | No conflict on new tables |
| `CREATE INDEX` | SHARE (target table) | Blocks writes to target table |

#### Phase 2: Data Restore (COPY FROM)

```sql
-- Source: restore/data.go:51
COPY tablename FROM PROGRAM '...' WITH CSV DELIMITER ',' ON SEGMENT;
```

`COPY FROM` implicitly acquires **ROW EXCLUSIVE** lock:

| Operation | Compatible with ROW EXCLUSIVE? | Description |
|-----------|-------------------------------|-------------|
| `SELECT` | ✅ Compatible | Can query the table being restored |
| `INSERT` | ✅ Compatible | Can write simultaneously |
| `UPDATE/DELETE` | ✅ Compatible | Can modify simultaneously |
| `ALTER TABLE` | ❌ Conflicts | Blocked |
| `TRUNCATE` | ❌ Conflicts | Blocked |

#### Phase 2.5: TRUNCATE (only with `--truncate-table` or `--incremental`)

```go
// Source: restore/data.go:286-288
if MustGetFlagBool(options.INCREMENTAL) || MustGetFlagBool(options.TRUNCATE_TABLE) {
    connectionPool.Exec(`TRUNCATE ` + tableName, whichConn)
}
```

`TRUNCATE` acquires **ACCESS EXCLUSIVE** lock:

| Operation | Impact |
|-----------|--------|
| All operations on the table | **Completely blocked** until TRUNCATE completes |

> ⚠ With `--truncate-table` or `--incremental`, each table is briefly **completely inaccessible** during its TRUNCATE operation.

#### Phase 3: Post-data DDL

```sql
-- CREATE INDEX, ADD CONSTRAINT, CREATE TRIGGER, etc.
```

`CREATE INDEX` acquires **SHARE** lock, blocking writes:

| Operation | Compatible with SHARE? | Description |
|-----------|----------------------|-------------|
| `SELECT` | ✅ Compatible | Can query |
| `INSERT/UPDATE/DELETE` | ❌ Conflicts | Blocked until index creation completes |

### 2.2 Restore Scenario Lock Impact Summary

| Restore Scenario | Lock Impact on Target Database |
|-----------------|-------------------------------|
| Restore to new database (`--create-db`) | **No impact** — no existing connections |
| Restore to empty database (`--redirect-db`) | **No impact** — empty tables, no conflicts |
| `--truncate-table` restore to existing database | **Brief blocking** — table inaccessible during TRUNCATE |
| `--incremental` restore | **Brief blocking** — TRUNCATE + COPY per table |
| `--include-table` selective restore | **Only affects specified tables** |
| `--data-only` data restore | **Affects restored tables** (COPY FROM ROW EXCLUSIVE) |
| `--metadata-only` metadata restore | **Affects index creation** (SHARE lock blocks writes) |

---

## 3. Enhanced Incremental Features — Lock Analysis

### 3.1 `--heap-file-hash` Additional Operations

| Operation | Lock Impact |
|-----------|------------|
| CHECKPOINT | **No database locks**, but briefly increases disk I/O |
| `pg_stat_file()` | **No locks** — reads filesystem inode metadata only |
| Separate database connection | **No impact on main backup transaction** |

### 3.2 `--ao-file-hash` Additional Operations

| Operation | Lock Impact |
|-----------|------------|
| Query `pg_aoseg` catalog tables | **ACCESS SHARE** (on catalog tables, not business tables) |
| Separate database connection | **No impact on main backup transaction** |

### 3.3 `--list-backups` / `--delete-backup`

These management commands **do not connect to the database** (only read/write the SQLite history file). **Zero impact on business operations.**

---

## 4. Best Practices

### 4.1 During Backup

| Recommendation | Description |
|---------------|-------------|
| ✅ Continue normal DML operations | SELECT/INSERT/UPDATE/DELETE are unaffected |
| ✅ VACUUM is safe during backup | Regular VACUUM is compatible with ACCESS SHARE |
| ✅ ANALYZE is safe during backup | Compatible with ACCESS SHARE |
| ⚠ Avoid DDL during backup | ALTER TABLE/DROP/TRUNCATE will be blocked |
| ⚠ Avoid VACUUM FULL during backup | Requires ACCESS EXCLUSIVE, will be blocked |
| ❌ Do not perform bulk DDL changes during backup | May cause prolonged lock waits |

### 4.2 During Restore

| Recommendation | Description |
|---------------|-------------|
| ✅ Prefer restoring to a new database | Avoids all lock conflicts |
| ✅ Use `--include-table` to minimize impact | Only locks the tables being restored |
| ⚠ Notify users before `--truncate-table` restore | TRUNCATE briefly interrupts table access |
| ⚠ Avoid writes during index creation phase | CREATE INDEX SHARE lock blocks writes |

### 4.3 Monitoring Lock Waits

```sql
-- Check current lock waits
SELECT l.pid, l.mode, l.granted, a.current_query, a.waiting
FROM pg_locks l
JOIN pg_stat_activity a ON l.pid = a.procpid
WHERE NOT l.granted
ORDER BY a.query_start;

-- Check locks held by gpbackup
SELECT l.relation::regclass, l.mode, l.granted
FROM pg_locks l
JOIN pg_stat_activity a ON l.pid = a.procpid
WHERE a.application_name LIKE 'gpbackup%';
```
