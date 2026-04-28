# Incremental Backup Hash Collection Performance Analysis

This document analyzes the additional overhead introduced by `--heap-file-hash` and `--ao-file-hash` during backup, helping users decide whether to enable these features.

---

## 1. Heap Table Hash Collection (`--heap-file-hash`)

### 1.1 Operation Chain

```
1. CHECKPOINT                          ← Global operation, flushes dirty pages to disk
2. ensureFileStatFunction()            ← Creates plpgsql function (first time only)
3. getFileHashesForTables()            ← Per-table query
   └─ For each heap table:
      SELECT md5(string_agg(...))
      FROM (
        SELECT gp_segment_id,
               gp_toolkit.gpbackup_file_info('schema','table')
        FROM gp_dist_random('gp_id')       ← Executes on each segment in parallel
      ) x
```

### 1.2 gpbackup_file_info Function Internals (executed on each segment)

```sql
-- 1. Query pg_class for relfilenode (catalog lookup, very fast)
SELECT reltablespace, relfilenode FROM pg_class ...

-- 2. Query pg_database for database oid (catalog lookup, very fast)
SELECT oid, dattablespace FROM pg_database ...

-- 3. Construct file path: base/<dboid>/<relfilenode>

-- 4. pg_stat_file(path)
--    Reads filesystem inode metadata only (modification time + size)
--    Does NOT read file contents — no data I/O
--    Returns: modification_timestamp | file_size
```

### 1.3 Overhead Breakdown

| Operation | Overhead | Description |
|-----------|----------|-------------|
| **CHECKPOINT** | **Largest cost** | Forces dirty pages in shared_buffers to disk. Can take seconds to tens of seconds depending on dirty page volume |
| Create plpgsql function | One-time, <1s | Creates `gp_toolkit.gpbackup_file_info` on first backup, skipped subsequently |
| Establish separate connection | <1s | Avoids impacting the main backup transaction |
| `pg_stat_file()` per table | **Very fast, <1ms** | Only reads inode metadata (stat syscall), no file content I/O |
| `gp_dist_random` dispatch | One SQL per table | N segments execute in parallel, ~5-30ms SQL round-trip latency |
| MD5 aggregation | Very fast, <1ms | MD5 of a few short strings |
| **Per-table total** | **~10-50ms** | Dominated by SQL network round-trip latency |

### 1.4 Estimated Duration by Scale

| Heap Table Count | Hash Collection Time | Notes |
|-----------------|---------------------|-------|
| 10 tables | <1 second | Imperceptible |
| 100 tables | 1-5 seconds | Negligible |
| 1,000 tables | 10-50 seconds | Minor impact |
| 10,000 tables | 2-8 minutes | Evaluate trade-off |
| 100,000 tables | 15-60 minutes | Recommend using `--include-schema` to filter |

> **Note**: Above excludes CHECKPOINT duration. CHECKPOINT time depends on dirty page count, not table count.

### 1.5 CHECKPOINT Impact

CHECKPOINT is a prerequisite for heap hash collection — it ensures in-memory data changes are flushed to disk so `pg_stat_file` sees accurate file modification times and sizes.

| Scenario | CHECKPOINT Duration | Notes |
|----------|-------------------|-------|
| System idle, few dirty pages | <1 second | Negligible |
| Normal workload, moderate dirty pages | 1-5 seconds | Acceptable |
| Peak workload, many dirty pages | 5-30 seconds | Schedule backups during off-peak |
| Just completed bulk writes | 10-60 seconds | Wait for natural CHECKPOINT before backup |

**Optimization tip**: Run `CHECKPOINT` manually before backup. The automatic CHECKPOINT during backup will then complete almost instantly.

---

## 2. AO Table Hash Collection (`--ao-file-hash`)

### 2.1 Operation Chain

```
GetAOContentHashes()
├─ getAOSegTableFQNs()         ← Single SQL query for all AO table → aoseg mappings
│   SELECT pg_ao.relid, 'pg_aoseg.' || aoseg_c.relname
│   FROM pg_appendonly pg_ao JOIN pg_class ...
│
└─ Per-table getAOSegContentHash()
    SELECT md5(string_agg(
      segno || ',' || eof || ',' || tupcount, chr(10) ORDER BY segno
    )) FROM pg_aoseg.pg_aoseg_<oid>
    -- GP5: Direct query on master catalog
    -- GP7+: Via gp_dist_random on segments
```

### 2.2 Overhead Breakdown

| Operation | Overhead | Description |
|-----------|----------|-------------|
| Query aoseg table mapping | Single SQL, <100ms | `pg_appendonly` JOIN `pg_class`, results cached |
| Query single table's aoseg content | **Very fast, <5ms** | `pg_aoseg` tables typically have 1-10 rows |
| MD5 computation | Very fast, <1ms | MD5 of a few short text rows |
| Establish separate connection | <1s | Prevents main transaction disruption |
| **No CHECKPOINT** | **Zero extra I/O** | Pure catalog queries, no data file access |
| **Per-table total** | **~2-5ms** | Extremely fast |

### 2.3 Estimated Duration by Scale

| AO Table Count | Hash Collection Time | Notes |
|---------------|---------------------|-------|
| 10 tables | <0.1 seconds | Imperceptible |
| 100 tables | 0.2-0.5 seconds | Negligible |
| 1,000 tables | 2-5 seconds | Very minor |
| 10,000 tables | 20-50 seconds | Acceptable |
| 100,000 tables | 3-8 minutes | Recommend using `--include-schema` to filter |

---

## 3. Comparison Summary

### 3.1 Side-by-Side Comparison

| Dimension | Heap (`--heap-file-hash`) | AO (`--ao-file-hash`) | No flags (original) |
|-----------|--------------------------|----------------------|---------------------|
| **Extra disk I/O** | CHECKPOINT flushes pages | **None** | None |
| **Query method** | `pg_stat_file` (filesystem inode) | `pg_aoseg` catalog table | No extra queries |
| **Execution location** | Each segment in parallel | Master local (GP5) / segments (GP7+) | — |
| **Per-table time** | ~10-50ms | ~2-5ms | 0 |
| **100 tables** | ~1-5s | ~0.2-0.5s | 0 |
| **1,000 tables** | ~10-50s | ~2-5s | 0 |
| **10,000 tables** | ~2-8min | ~20-50s | 0 |
| **Extra connections** | +1 | +1 | 0 |
| **Primary bottleneck** | CHECKPOINT flush | Serial per-table queries | — |
| **Business impact** | CHECKPOINT may briefly increase I/O | **None** | None |

### 3.2 Overall Incremental Backup Efficiency

Although hash collection adds overhead, the **data transfer time saved by incremental backup typically far exceeds the hash collection cost**:

| Scenario | Without hash | With hash | Net effect |
|----------|-------------|-----------|------------|
| 100 heap tables (500GB total), only 5 changed | Back up 500GB | 5s hash + back up 25GB | **~95% time saved** |
| 50 AO partitions, only 2 changed (GP5) | Back up all 50 | 0.3s hash + back up 2 | **~96% time saved** |
| All tables changed | Back up everything | Ns hash + back up everything | Extra N seconds (not beneficial) |

---

## 4. Usage Recommendations

### 4.1 When to Enable

| Scenario | Recommended Flag | Reason |
|----------|-----------------|--------|
| Many large heap tables, infrequently modified | `--heap-file-hash` | Hash takes minutes, saves backing up TB of unchanged data |
| GP5 with many AO partition tables | `--ao-file-hash` | Minimal overhead (seconds), avoids full partition re-backup |
| Limited backup window | Both flags | Maximize reduction in backup data volume |
| Daily incremental backups | Both flags | Daily changes are typically small, hash detection very effective |

### 4.2 When NOT to Enable

| Scenario | Recommendation | Reason |
|----------|---------------|--------|
| All tables modified daily with heavy writes | No flags | Most tables need backup anyway; hash adds overhead |
| More than 100,000 tables | Batch by schema | Serial per-table queries take too long |
| Backup window during peak hours | Only `--ao-file-hash` | Avoid CHECKPOINT I/O impact on business |

### 4.3 Optimization Techniques

| Technique | Effect |
|-----------|--------|
| Run `CHECKPOINT` manually before backup | Automatic CHECKPOINT during backup completes almost instantly |
| Use `--include-schema` to filter | Reduces number of tables needing hash collection |
| Schedule backups during off-peak hours | CHECKPOINT disk flush has minimal business impact |
| Tune `checkpoint_completion_target` | Makes CHECKPOINT more gradual, reducing I/O spikes |

---

## 5. Technical Implementation Details

### 5.1 Heap Tables: Why CHECKPOINT Is Required

PostgreSQL/Greenplum uses shared_buffers to cache data pages. When INSERT/UPDATE/DELETE operations execute, modified data pages (dirty pages) are written to the buffer first and flushed to disk asynchronously.

`pg_stat_file()` reads the **on-disk file's** inode metadata. If dirty pages haven't been flushed, the file's modification time and size on disk won't reflect recent changes, causing the hash to match the previous backup — incorrectly judging the table as "unchanged."

CHECKPOINT forces all dirty pages to be flushed, ensuring `pg_stat_file()` results are accurate.

### 5.2 AO Tables: Why CHECKPOINT Is NOT Required

AO (Append-Optimized) table change tracking is stored in `pg_aoseg` catalog tables, recording each data segment file's `eof` (end-of-file position) and `tupcount` (row count). This metadata is **synchronously updated** to the catalog on transaction commit — no additional disk flush is needed.

Therefore, `--ao-file-hash` queries are completed entirely at the catalog level with zero extra I/O.

### 5.3 Why AO Hash Excludes modcount

In GP5, when data is inserted into a single AO partition table's child partition, `modcount` is **incremented in ALL sibling partitions' aoseg tables**. This is a GP5 kernel behavior that makes modcount-based detection unable to distinguish which partition actually received data changes.

`--ao-file-hash` uses only `eof` + `tupcount` to compute the hash. These two fields **only change in partitions that actually received data modifications**, enabling partition-level precise detection.
