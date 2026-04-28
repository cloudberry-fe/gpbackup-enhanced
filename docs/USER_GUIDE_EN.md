# gpbackup / gprestore Enhanced Edition User Guide

**Version**: 1.30.5-custom (based on gpbackup 1.30.5 with enhancements)
**Compatibility**: Greenplum 5.x / 6.x / 7.x, Apache Cloudberry, Euler Database

---

## Table of Contents

- [1. Overview](#1-overview)
- [2. Installation](#2-installation)
- [3. gpbackup — Backup](#3-gpbackup--backup)
  - [3.1 Full Backup](#31-full-backup)
  - [3.2 Incremental Backup (Default Mode)](#32-incremental-backup-default-mode)
  - [3.3 Incremental Backup (Heap File Hash Mode)](#33-incremental-backup-heap-file-hash-mode)
  - [3.4 Incremental Backup (AO Partition-Level Mode)](#34-incremental-backup-ao-partition-level-mode)
  - [3.5 Backup Parameter Reference](#35-backup-parameter-reference)
- [4. gprestore — Restore](#4-gprestore--restore)
  - [4.1 Full Restore](#41-full-restore)
  - [4.2 Incremental Restore](#42-incremental-restore)
  - [4.3 Single Table Restore](#43-single-table-restore)
  - [4.4 Restore Parameter Reference](#44-restore-parameter-reference)
- [5. Backup Set Management](#5-backup-set-management)
  - [5.1 List Backups](#51-list-backups)
  - [5.2 Delete Backups](#52-delete-backups)
- [6. Query Backup Data via External Tables](#6-query-backup-data-via-external-tables)
  - [6.1 Create External Tables](#61-create-external-tables)
  - [6.2 Query Backup Data](#62-query-backup-data)
  - [6.3 Cleanup](#63-cleanup)
  - [6.4 Cross-Cluster Backup Querying](#64-cross-cluster-backup-querying)
  - [6.5 Parameter Reference](#65-parameter-reference)
- [7. Incremental Detection Mechanisms](#7-incremental-detection-mechanisms)
- [8. Best Practices](#8-best-practices)
- [9. FAQ](#9-faq)

---

## 1. Overview

This tool extends gpbackup 1.30.5 with the following enhancements:

| Feature | Description |
|---------|-------------|
| **Heap table incremental backup** | Detects heap table changes via `pg_stat_file` (file modification time + size after CHECKPOINT) |
| **AO partition-level detection** | Uses per-table aoseg content hash to solve the GP5 modcount cross-partition propagation issue |
| **Backup set management** | `--list-backups` to list backups, `--delete-backup` to remove backups with cascading delete and physical file cleanup |
| **Query backup data via external tables** | Creates gpfdist-backed external tables to query backup data directly with SQL, without full restore |
| **GP5/6/7 compatibility** | Auto-detects GP version and adapts plpythonu/plpython3u, gp_session_role/gp_role, pg_filespace_entry/datadir, etc. |

---

## 2. Installation

### 2.1 Build

```bash
# Requires Go 1.19+
cd gpbackup/
GIT_VERSION="1.30.5-custom"

go build -tags gpbackup -o gpbackup \
    -ldflags "-X github.com/greenplum-db/gpbackup/backup.version=${GIT_VERSION}" \
    ./gpbackup.go

go build -tags gprestore -o gprestore \
    -ldflags "-X github.com/greenplum-db/gpbackup/restore.version=${GIT_VERSION}" \
    ./gprestore.go

go build -tags gpbackup_helper -o gpbackup_helper \
    -ldflags "-X github.com/greenplum-db/gpbackup/helper.version=${GIT_VERSION}" \
    ./gpbackup_helper.go
```

### 2.2 Deploy

```bash
# Copy to $GPHOME/bin on all nodes
cp gpbackup gprestore gpbackup_helper $GPHOME/bin/
chmod 755 $GPHOME/bin/gpbackup $GPHOME/bin/gprestore $GPHOME/bin/gpbackup_helper

# Deploy external table query script
cp gpbackup_ext_query.sh $GPHOME/bin/
chmod 755 $GPHOME/bin/gpbackup_ext_query.sh

# Verify
gpbackup --version
gprestore --version
```

---

## 3. gpbackup — Backup

### 3.1 Full Backup

```bash
# Basic full backup
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data

# Full backup with AO content hash baseline (for --ao-file-hash incremental)
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --ao-file-hash

# Full backup with both Heap + AO hash baselines
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --heap-file-hash --ao-file-hash

# Full backup with parallelism
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --jobs 4

# Backup specific schemas only
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --include-schema public --include-schema sales

# Backup specific tables only
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --include-table public.orders --include-table public.customers
```

> **Note**: `--leaf-partition-data` is a prerequisite for incremental backup. It is recommended to always include this flag.

### 3.2 Incremental Backup (Default Mode)

Default mode detection mechanisms:
- **AO tables**: Compares `modcount` + `pg_stat_last_operation` DDL timestamp
- **Heap tables**: Compares data file modification time and size via `pg_stat_file` (after CHECKPOINT)

```bash
# Incremental backup (based on most recent full or incremental)
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --incremental

# Incremental backup (based on specific timestamp)
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --from-timestamp 20260422120000
```

**Behavior**:
- AO tables: Only backs up tables whose modcount or DDL timestamp has changed
- Heap tables: Only backs up tables whose data files have changed
- Unchanged tables: Skipped entirely, no data files produced

### 3.3 Incremental Backup (Heap File Hash Mode)

`--heap-file-hash` enables heap table incremental detection. Without this flag, heap tables are always fully backed up during incremental (original gpbackup behavior).

```bash
# Full backup: build heap hash baseline
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --heap-file-hash

# Incremental: unchanged heap tables are skipped
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --heap-file-hash
```

**Use case**: Large heap tables that are infrequently modified.

### 3.4 Incremental Backup (AO Partition-Level Mode)

`--ao-file-hash` is independent of `--heap-file-hash` and solves a GP5-specific partition table problem:

**The GP5 problem**: When inserting data into a single child partition, the parent table and ALL sibling partitions have their `modcount` incremented, causing the incremental backup to treat every partition as "modified" and back them all up.

**The solution**: `--ao-file-hash` uses a per-table content hash derived from `pg_aoseg` metadata (`eof` + `tupcount`, deliberately excluding `modcount`). Only partitions with actual data changes are detected.

```bash
# Step 1: Full backup with --ao-file-hash to establish baseline
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --ao-file-hash

# Step 2: Incremental backup with partition-level precision (--ao-file-hash alone)
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --ao-file-hash

# Or combine both flags for full precision (Heap + AO)
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --heap-file-hash --ao-file-hash
```

**Example**:

```
Full backup: t_part has 3 child partitions (p1, p2, p3), all backed up

-> INSERT 100 rows into p1 only

Incremental (default mode):       Backs up p1 + p2 + p3 (all modcounts changed)
Incremental (--ao-file-hash):     Backs up p1 only      (only p1 eof/tupcount changed)
```

### 3.5 Backup Parameter Reference

#### Standard Parameters

| Parameter | Description |
|-----------|-------------|
| `--dbname` | **Required.** Database to back up |
| `--backup-dir` | Backup file storage directory |
| `--leaf-partition-data` | Create one data file per leaf partition (required for incremental) |
| `--incremental` | Incremental backup mode |
| `--from-timestamp` | Base incremental backup on a specific timestamp |
| `--jobs N` | Number of parallel backup connections |
| `--include-schema` | Back up only specified schema(s) (repeatable) |
| `--include-table` | Back up only specified table(s) (repeatable) |
| `--exclude-schema` | Exclude specified schema(s) |
| `--exclude-table` | Exclude specified table(s) |
| `--data-only` | Back up data only |
| `--metadata-only` | Back up metadata only |
| `--with-stats` | Include query plan statistics |
| `--no-compression` | Disable data file compression |
| `--compression-type` | Compression type: gzip (default) or zstd |

#### New Parameters

| Parameter | Description |
|-----------|-------------|
| `--heap-file-hash` | Use file hash to detect heap table changes. Independently usable: full backup builds baseline, incremental skips unchanged heap tables |
| `--ao-file-hash` | Use aoseg content hash for AO table detection. Independently usable: full backup builds baseline, incremental enables partition-level detection |
| `--gen-ext-metadata` | Generate external table metadata files after backup, enabling cross-cluster backup querying |
| `--list-backups` | List all backups in the backup directory and exit |
| `--delete-backup TS` | Delete the backup with the specified timestamp (full backups cascade-delete dependent incrementals) |

---

## 4. gprestore — Restore

### 4.1 Full Restore

```bash
# Restore to a new database
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --create-db

# Restore to an existing database (truncate target tables first)
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --truncate-table

# Restore and run ANALYZE afterwards
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --create-db --run-analyze
```

### 4.2 Incremental Restore

Specify the incremental backup timestamp. gprestore automatically reads the `restoreplan` from `config.yaml` to determine which tables to restore from which backup sets:

```bash
# Restore to the incremental backup point-in-time
gprestore --timestamp 20260422150000 --backup-dir /data/backup \
    --redirect-db restore_db --create-db
```

> **Note**: You do NOT need to specify `--incremental`. gprestore detects this automatically.

### 4.3 Single Table Restore

```bash
# Restore specific tables only
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db \
    --include-table public.orders --include-table public.customers

# Restore a specific schema only
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --include-schema sales

# Continue on errors
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --on-error-continue
```

### 4.4 Restore Parameter Reference

| Parameter | Description |
|-----------|-------------|
| `--timestamp` | **Required.** Backup timestamp to restore |
| `--backup-dir` | Backup file directory |
| `--redirect-db` | Restore to a different database |
| `--redirect-schema` | Restore to a different schema |
| `--create-db` | Create database before restore |
| `--truncate-table` | Truncate target tables before restore |
| `--include-table` | Restore only specified table(s) (repeatable) |
| `--include-schema` | Restore only specified schema(s) |
| `--exclude-table` | Exclude specified table(s) |
| `--exclude-schema` | Exclude specified schema(s) |
| `--data-only` | Restore data only |
| `--metadata-only` | Restore metadata only |
| `--on-error-continue` | Log errors and continue instead of failing |
| `--jobs N` | Number of parallel restore connections |
| `--with-globals` | Restore global objects (roles, tablespaces, etc.) |
| `--with-stats` | Restore query plan statistics |
| `--run-analyze` | Run ANALYZE after restore |
| `--list-backups` | List all backups and exit |
| `--delete-backup TS` | Delete the specified backup set |

---

## 5. Backup Set Management

### 5.1 List Backups

```bash
# List via gpbackup
gpbackup --list-backups --backup-dir /data/backup

# List via gprestore (identical functionality)
gprestore --list-backups --backup-dir /data/backup
```

**Sample output**:

```
Timestamp         Start Time            Type    Status     Database      Deleted At            Depends On
-------------------------------------------------------------------------------------------------------------------
20260422120000    2026-04-22 12:00:00   Full    Success    mydb
20260422130000    2026-04-22 13:00:00   Incr    Success    mydb                                20260422120000
20260422140000    2026-04-22 14:00:00   Incr    Success    mydb                                20260422120000
20260422150000    2026-04-22 15:00:00   Full    Success    mydb
20260422160000    2026-04-22 16:00:00   Incr    Success    mydb                                20260422150000
```

**Column descriptions**:

| Column | Description |
|--------|-------------|
| Timestamp | Backup timestamp (YYYYMMDDHHMMSS) |
| Start Time | Human-readable start time |
| Type | Full = full backup, Incr = incremental backup |
| Status | Success or Failure |
| Database | Database name |
| Deleted At | Deletion timestamp (empty = active) |
| Depends On | Timestamp of the full backup this incremental depends on |

> **Note**: Results are filtered by `--backup-dir`, showing only backups stored in that directory.

### 5.2 Delete Backups

```bash
# Delete a full backup (cascades to all dependent incrementals + physical files)
gpbackup --delete-backup 20260422120000 --backup-dir /data/backup

# Delete a single incremental backup
gpbackup --delete-backup 20260422130000 --backup-dir /data/backup
```

**Deletion behavior**:

- **Deleting a full backup**: Automatically finds and deletes ALL dependent incremental backups
- **Deleting an incremental backup**: Deletes only that incremental
- **Physical file cleanup**: Automatically removes backup files on the coordinator and all segment hosts via SSH
- **History records**: Hard-deleted from `gpbackup_history.db` (removed entirely, not soft-deleted)

**Sample output**:

```
Deleted 3 backup(s) from history:
  20260422120000  2026-04-22 12:00:00  (target)
  20260422130000  2026-04-22 13:00:00  (incremental)
  20260422140000  2026-04-22 14:00:00  (incremental)

Removing backup files...
  Coordinator: removed 3 backup directories
  Segments: cleaned backup files on 2 host(s): sdw1, sdw2
  File cleanup complete.
```

---

## 6. Query Backup Data via External Tables

`gpbackup_ext_query.sh` allows querying backup data directly without performing a full restore.

**How it works**:
1. Starts gpfdist services on each segment host, serving the backup directory
2. Creates external tables in the target database with the same column definitions as the original tables
3. External tables read backup data files (gzip-compressed CSV) via gpfdist
4. Users query external tables with standard SQL to read backup data

### 6.1 Create External Tables

**Schema naming rules**:
- **With `--ext-schema`**: All external tables are created in the specified schema (e.g., `ext_bak.t_heap`)
- **Without `--ext-schema`** (default): Auto-generates `<source_schema>_<timestamp>` (e.g., `public_20260422120000.t_heap`). Tables from different schemas get their own corresponding schema.

```bash
# Without --ext-schema: auto-generates <source_schema>_<timestamp>
# e.g., public tables → public_20260422120000.t_heap
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb

# With --ext-schema: all tables in one schema
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak

# Create external tables for specific tables only
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak \
    --include-table public.orders \
    --include-table public.customers

# Specify a custom gpfdist port
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak \
    --gpfdist-port 19000
```

**Sample output**:

```
============================================
  gpbackup External Table Query Setup
============================================
  Timestamp:  20260422120000
  Backup Dir: /data/backup
  Database:   mydb
  Ext Schema: ext_bak
============================================

Found 5 table(s) to create external tables for.
Cluster has 4 primary segments.
Using gpfdist port: 18596

Starting gpfdist services...
  Starting gpfdist on sdw1:18596...
  Starting gpfdist on sdw2:18596...
  Starting gpfdist on coordinator mdw:18596...
  gpfdist services started.

Creating external tables...
  OK: ext_bak.orders -> public.orders (oid=12345)
  OK: ext_bak.customers -> public.customers (oid=12346)
  OK: ext_bak.products -> public.products (oid=12347)
  OK: ext_bak.order_items -> public.order_items (oid=12348)
  OK: ext_bak.t_heap -> public.t_heap (oid=12349)

============================================
  Setup Complete
============================================
  External tables created: 5
  Schema: ext_bak
  gpfdist port: 18596
============================================
```

### 6.2 Query Backup Data

Once external tables are created, use standard SQL to query backup data:

```sql
-- Row count
SELECT count(*) FROM ext_bak.orders;

-- Filtered query
SELECT * FROM ext_bak.orders
WHERE order_date >= '2024-01-01' AND total_amount > 1000
ORDER BY order_date DESC
LIMIT 100;

-- Aggregation
SELECT date_trunc('month', order_date) AS month, sum(total_amount)
FROM ext_bak.orders
GROUP BY 1 ORDER BY 1;

-- Compare backup vs current data
SELECT 'backup' AS source, count(*) FROM ext_bak.orders
UNION ALL
SELECT 'current', count(*) FROM public.orders;

-- Find records in backup that are missing from current table
SELECT b.order_id, b.order_date
FROM ext_bak.orders b
LEFT JOIN public.orders c ON b.order_id = c.order_id
WHERE c.order_id IS NULL;
```

### 6.3 Cleanup

```bash
# Remove all external tables + stop gpfdist + drop schema
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak --stop

# Remove only specific external tables (other tables in schema are preserved)
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak \
    --include-table public.orders --stop
```

**Cleanup behavior**:
- Drops external tables individually (`DROP EXTERNAL TABLE`)
- Attempts to drop the schema **without CASCADE** (safe — fails if other objects remain)
- Stops gpfdist processes on all segment hosts and the coordinator via SSH

### 6.4 Cross-Cluster Backup Querying

By default, `gpbackup_ext_query.sh` requires a connection to the source database to retrieve table structures and segment information. With `--gen-metadata` and `--use-metadata`, this information is saved to files, enabling backup querying on **any cluster**.

#### Step 1: Generate metadata files

Two ways to generate metadata files:

**Option A: Automatically during backup (recommended)**

Add `--gen-ext-metadata` to the gpbackup command:

```bash
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --gen-ext-metadata
```

**Option B: Manually after backup**

If `--gen-ext-metadata` was not used during backup, generate metadata afterwards:

```bash
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname mydb --gen-metadata
```

This generates two files in the backup directory:

| File | Description |
|------|-------------|
| `gpbackup_<TS>_ext_metadata.yaml` | Segment info + table column definitions (read-only) |
| `gpbackup_<TS>_ext_config.yaml` | Editable config file (host mapping + directory mapping) |

#### Step 2: Copy backup files to the target server

Copy the entire backup directory (including all `gpseg*` subdirectories and the two metadata files) to the target server.

#### Step 3: Edit the config file

Edit `gpbackup_<TS>_ext_config.yaml` to update host mappings and backup directory path:

```yaml
# Before (source cluster)
backup_dir: "/data/backup"
gpfdist_port: 0
host_map:
  - original: "sdw1"
    target: "sdw1"
  - original: "sdw2"
    target: "sdw2"

# After (target cluster)
backup_dir: "/data1/restore/backup"     # new path
gpfdist_port: 19000                      # optional: specify port
host_map:
  - original: "sdw1"
    target: "new-host-1"               # target hostname
  - original: "sdw2"
    target: "new-host-2"               # target hostname
```

#### Step 4: Create external tables on the target cluster

```bash
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data1/restore/backup \
    --dbname target_db \
    --ext-schema ext_bak \
    --use-metadata
```

`--use-metadata` reads table column definitions and segment info from the metadata file, and host mappings from the config file — **no connection to the source database is needed**.

#### Step 5: Query and cleanup

```bash
# Query
psql -d target_db -c "SELECT count(*) FROM ext_bak.orders;"

# Cleanup
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data1/restore/backup \
    --dbname target_db --ext-schema ext_bak --stop
```

### 6.5 Parameter Reference

| Parameter | Required | Description |
|-----------|----------|-------------|
| `--timestamp <TS>` | Yes | Backup timestamp |
| `--backup-dir <DIR>` | Yes | Backup directory |
| `--dbname <DB>` | Yes | Target database for external tables |
| `--ext-schema <NAME>` | No | External table schema name. If omitted, auto-generates `<source_schema>_<timestamp>` (e.g., `public_20260422120000`) |
| `--include-table <S.T>` | No | Process only specified table(s) (repeatable) |
| `--include-schema <S>` | No | Process only tables in specified schema(s) (repeatable) |
| `--gpfdist-port <PORT>` | No | gpfdist port (default: random 18000-19000) |
| `--gen-metadata` | No | Generate metadata files (segment info + table definitions). Requires source database connection |
| `--use-metadata` | No | Read table structures from metadata file instead of database. Enables cross-cluster querying |
| `--stop` | No | Stop gpfdist and remove external tables |
| `--help` | No | Show help message |

---

## 7. Incremental Detection Mechanisms

### Mode Comparison

| Mode | Flags | Heap Detection | AO Detection | Use Case |
|------|-------|---------------|-------------|----------|
| **Default** | `--incremental` | Always backup (original) | modcount + DDL timestamp | General purpose |
| **Heap hash** | `+ --heap-file-hash` | File hash (mtime + size) | modcount + DDL timestamp | Skip unchanged heap tables |
| **AO partition** | `+ --ao-file-hash` | Always backup (original) | aoseg content hash (eof + tupcount) | GP5 partition tables |
| **Full precision** | `+ --heap-file-hash --ao-file-hash` | File hash (mtime + size) | aoseg content hash (eof + tupcount) | Minimize backup size |

### Detection Details

**Heap tables — File hash detection**:
```
CHECKPOINT is issued before collection -> flushes dirty pages to disk
|
On each segment, pg_stat_file() returns:
  - modification timestamp
  - file size
|
All segment results are aggregated into an MD5 hash
|
Compared with the previous backup's hash -> different = backup, same = skip
```

**AO tables — modcount detection (default)**:
```
Query pg_aoseg.pg_aoseg_<oid> for sum(modcount)
Query pg_stat_last_operation for last DDL timestamp
|
Compare with previous backup -> either different = backup
```

**AO tables — aoseg content hash detection (--ao-file-hash)**:
```
Query pg_aoseg.pg_aoseg_<oid> for each row's (segno, eof, tupcount)
  Note: modcount is deliberately EXCLUDED (propagates across partitions in GP5)
|
Compute MD5 hash
|
Compare with previous backup's hash -> different = backup
```

### TOC File Metadata

Incremental metadata is stored in the `incrementalmetadata` section of `gpbackup_<timestamp>_toc.yaml`:

```yaml
incrementalmetadata:
  ao:
    public.t_ao:
      modcount: 5
      lastddltimestamp: "2026-04-22T10:00:00+08:00"
      fileHashMD5: a1b2c3d4e5f6...           # present when --ao-file-hash is used
    public.t_part_1_prt_p202401:
      modcount: 3
      lastddltimestamp: "2026-04-22T10:00:00+08:00"
      fileHashMD5: f6e5d4c3b2a1...
  heap:
    public.t_heap:
      fileHashMD5: 1a2b3c4d5e6f...           # heap table file hash
```

---

## 8. Best Practices

### 8.1 Recommended Backup Schedule

```bash
# Weekly full backup on Sunday (build heap + AO hash baselines)
0 2 * * 0  gpbackup --dbname prod --backup-dir /data/backup \
    --leaf-partition-data --heap-file-hash --ao-file-hash --jobs 4

# Daily incremental backup Mon-Sat (heap + AO precise detection)
0 2 * * 1-6  gpbackup --dbname prod --backup-dir /data/backup \
    --leaf-partition-data --incremental --heap-file-hash --ao-file-hash --jobs 4
```

### 8.2 Regular Cleanup

```bash
# List all backups
gpbackup --list-backups --backup-dir /data/backup

# Delete a 2-week-old full backup and its incrementals (including physical files)
gpbackup --delete-backup 20260408020000 --backup-dir /data/backup
```

### 8.3 Data Verification (Without Restore)

```bash
# Quickly verify backup data via external tables
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname prod \
    --ext-schema ext_verify --include-table public.orders

psql -d prod -c "
    SELECT
        (SELECT count(*) FROM public.orders) AS current_count,
        (SELECT count(*) FROM ext_verify.orders) AS backup_count;
"

# Cleanup after verification
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname prod \
    --ext-schema ext_verify --stop
```

### 8.4 Emergency Single-Table Recovery

```bash
# Scenario: Some rows were accidentally deleted from the orders table

# Method 1: Query backup via external table and re-insert missing rows
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname prod \
    --ext-schema ext_rescue --include-table public.orders

psql -d prod -c "
    INSERT INTO public.orders
    SELECT * FROM ext_rescue.orders
    WHERE order_id NOT IN (SELECT order_id FROM public.orders);
"

gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname prod \
    --ext-schema ext_rescue --stop

# Method 2: Restore single table to a temporary database via gprestore
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db temp_restore --include-table public.orders --create-db
```

---

## 9. FAQ

### Q1: Incremental backup backs up all tables even though nothing changed

**Cause**: The first time `--heap-file-hash` or `--ao-file-hash` is used for an incremental backup, the previous full backup does not have the corresponding hash baseline.

**Solution**: Ensure the full backup includes the same flags to build the baseline:
- For `--heap-file-hash`: full backup must also use `--heap-file-hash`
- For `--ao-file-hash`: full backup must also use `--ao-file-hash`
- The two flags are independent and can be used separately or together

### Q2: GP5 partition table incremental backs up all child partitions

**Cause**: In GP5, `modcount` propagates across sibling partitions when any one partition is modified.

**Solution**: Use `--ao-file-hash` (independently, no need for `--heap-file-hash`). This uses per-table `eof` + `tupcount` hashing instead of `modcount`.

### Q3: Small INSERT into a heap table is not detected by incremental backup

**Cause**: Data is still in memory and has not been flushed to disk, so `pg_stat_file` sees unchanged file metadata.

**Solution**: The enhanced version automatically issues a `CHECKPOINT` before collecting file hashes to ensure all dirty pages are flushed. If the issue persists, manually run `CHECKPOINT` before the backup.

### Q4: External table query returns "connection refused"

**Cause**: The gpfdist service is not running or the port is occupied.

**Solution**:
```bash
# Check if gpfdist is running
ps aux | grep gpfdist

# Use a different port if there is a conflict
gpbackup_ext_query.sh ... --gpfdist-port 19500
```

### Q5: `--delete-backup` does not remove segment files

**Cause**: The database is not running (segment host information cannot be queried) or passwordless SSH is not configured.

**Solution**: Ensure the database is running and the gpadmin user can SSH to all segment hosts without a password.

### Q6: `--list-backups` shows no records

**Cause**: The `gpbackup_history.db` file cannot be found.

**Solution**: The tool searches the following paths in order:
1. `<backup-dir>/gpbackup_history.db`
2. `<backup-dir>/gpseg-1/gpbackup_history.db`
3. `$MASTER_DATA_DIRECTORY/gpbackup_history.db`
4. `$COORDINATOR_DATA_DIRECTORY/gpbackup_history.db`

Ensure the history database file exists at one of these locations.

### Q7: External table returns fewer rows than expected

**Cause**: The backup was an incremental backup and only contains data for tables that changed. Unchanged tables have no data files in that specific backup timestamp.

**Solution**: Use the full backup timestamp, or use the most recent incremental timestamp (gprestore's `restoreplan` determines which backup to read each table from, but external tables point to a single timestamp).
