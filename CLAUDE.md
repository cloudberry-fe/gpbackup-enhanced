# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Enhanced gpbackup/gprestore for Greenplum Database (5.x/6.x/7.x, Cloudberry, Euler DB). Based on gpbackup 1.30.5 with added heap incremental backup, AO partition-level detection, backup management, and external table querying. Go module: `github.com/greenplum-db/gpbackup`.

## Build Commands

Three separate binaries, each gated by build tags. Version is injected via ldflags:

```bash
# Full build (requires CGO for sqlite3)
GIT_VERSION="1.30.5-custom"
CGO_ENABLED=1 go build -tags gpbackup -o gpbackup -ldflags "-X github.com/greenplum-db/gpbackup/backup.version=${GIT_VERSION}" ./gpbackup.go
CGO_ENABLED=1 go build -tags gprestore -o gprestore -ldflags "-X github.com/greenplum-db/gpbackup/restore.version=${GIT_VERSION}" ./gprestore.go
CGO_ENABLED=1 go build -tags gpbackup_helper -o gpbackup_helper -ldflags "-X github.com/greenplum-db/gpbackup/helper.version=${GIT_VERSION}" ./gpbackup_helper.go

# Or use Makefile (requires GOPATH)
make build

# Cross-compile for Linux
make build_linux
```

**Critical**: Without `-ldflags -X`, `--version` prints empty. Always include version injection.

## Testing

```bash
make unit                    # Unit tests (ginkgo)
make integration             # Integration tests (requires running GPDB)
make end_to_end              # End-to-end tests (requires running GPDB)
make test                    # build + unit + integration
make format                  # goimports formatting
make lint                    # golangci-lint
```

Tests use Ginkgo/Gomega framework. Unit tests are in `*_test.go` files alongside source. Test helpers in `testutils/`.

**Ginkgo gotcha**: never call backup/restore functions that read flags
(`MustGetFlagBool`, `MustGetFlagString`, …) at the *container* level of
a `Describe`/`Context` block — those blocks run during spec-tree
construction, before any `BeforeEach`, so the global `cmdFlags` is
still nil and you get a panic. Always call from inside `BeforeEach`,
`JustBeforeEach`, or the `It` body. (`backup/incremental_test.go` is
the canonical example after the recent fix.)

## Testing against a real cluster

`gpbackup` runs on the coordinator and SSH-fans out to each segment
host to invoke `gpbackup_helper`. When iterating on a patch:

1. Build the three binaries (`gpbackup`, `gprestore`, `gpbackup_helper`)
   on a linux/amd64 host matching the target cluster's OS.
2. **Deploy `gpbackup_helper` to every segment host** at
   `$GPHOME/bin/gpbackup_helper`. Forgetting one host = backup hangs
   or fails on that segment with a confusing error. Back up the
   original first; restore after the test.
3. Run the new `gpbackup` from `dist/` directly (no need to install
   over `$GPHOME/bin/gpbackup`).
4. `--list-backups` / `--delete-backup` need `$MASTER_DATA_DIRECTORY`
   (GP5/6) or `$COORDINATOR_DATA_DIRECTORY` (GP7+/Cloudberry) set in
   the shell — that's where `gpbackup_history.db` is located.
   `source $GPHOME/greenplum_path.sh` then set the env var explicitly.

## Heap-file-hash invariants (don't break)

- `getHeapTableFQNs` returns **leaf partition** FQNs plus standalone
  heap tables. Partition parents are excluded (`NOT IN inhparent`)
  because under `--leaf-partition-data` data flows through leaves
  only, and `FilterTablesForIncremental` looks up hashes by the
  leaf FQN. Reversing this to "exclude leaves" (the original bug)
  causes every leaf to be re-backed up on every incremental.
- `gpbackup_file_info(schema, table)` returns `<mtime>|<size>` from
  `pg_stat_file()` and **requires a `CHECKPOINT` immediately before
  the call** for the mtime/size to reflect the latest writes.
  `backupIncrementalMetadata` issues this CHECKPOINT.
- The hash query aggregates per-segment results
  (`gp_segment_id::text || ',' || info`) and MD5s the whole string,
  so identical mtime+size across two tables can collide. This is
  accepted because the hash is compared per-FQN, not globally — but
  if you ever cross-compare hashes between tables, add the
  relfilenode to the function's return.

## Architecture

### Entry Points (build-tag gated)

- `gpbackup.go` (tag: `gpbackup`) → calls `backup.DoInit()`, `backup.DoBackup()`
- `gprestore.go` (tag: `gprestore`) → calls `restore.DoInit()`, `restore.DoRestore()`
- `gpbackup_helper.go` (tag: `gpbackup_helper`) → segment-level COPY helper process

### Core Packages

| Package | Purpose |
|---------|---------|
| `backup/` | Backup logic: metadata collection, data export (COPY TO), incremental detection |
| `restore/` | Restore logic: metadata replay, data import (COPY FROM) |
| `options/` | CLI flag definitions (`flag.go`) and option parsing |
| `toc/` | TOC (Table of Contents) YAML types — `CoordinatorDataEntry`, `AOEntry`, `HeapEntry`, `IncrementalEntries` |
| `history/` | Backup history SQLite DB — `BackupConfig`, `StoreBackupHistory`, `ListBackups`, `DeleteBackup` |
| `filepath/` | Backup directory path construction per segment |
| `report/` | Backup report file generation |
| `utils/` | Shared utilities (signals, compression, include sets) |
| `helper/` | Segment-level helper for `--single-data-file` mode |

### Backup Flow (backup/backup.go DoBackup)

```
DoBackup()
├─ GetTargetBackupTimestamp()         # incremental: find base backup
├─ RetrieveAndProcessTables()         # get table list
├─ backupIncrementalMetadata()        # collect AO modcount/DDL + optional heap/AO hashes
├─ FilterTablesForIncremental()       # compare with previous backup, filter changed tables
├─ PopulateRestorePlan()              # build restore plan for incremental chain
├─ GenerateExtMetadata()              # optional: --gen-ext-metadata
├─ backupGlobals/backupPredata/backupPostdata  # DDL export
└─ backupData(filteredTables)         # COPY TO for each table
```

### Key Design Patterns

- **Global variables** in `backup/global_variables.go` and `restore/global_variables.go` — `connectionPool`, `globalTOC`, `cmdFlags`, `backupReport`. Accessed via wrapper functions (`MustGetFlagBool`, `MustGetFlagString`).
- **dbconn.DBConn** — database connection pool. Uses `.Select(&results, query)` for queries, `.Exec(query, connNum)` for DDL. No `.Query()` method.
- **Worker model** — Worker 0 holds all ACCESS SHARE locks; Workers 1-N use NOWAIT to avoid deadlocks, deferring failed tables to Worker 0.
- **Version detection** — `connectionPool.Version.Before("6")`, `.Before("7")`, `.AtLeast("6.21.0")` for GP5/6/7 differences.

### Enhanced Features (our additions)

| File | Feature |
|------|---------|
| `backup/incremental.go` | `FilterTablesForIncremental` — unified filter with independent `--heap-file-hash` and `--ao-file-hash` |
| `backup/queries_incremental.go` | Heap: `pg_stat_file` via plpgsql function on segments. AO: `pg_aoseg` content hash (eof+tupcount, excludes modcount) |
| `backup/wrappers.go` | `backupIncrementalMetadata` — conditional hash collection based on flags |
| `backup/manage.go` + `restore/manage.go` | `--list-backups`, `--delete-backup` (hard delete + SSH file cleanup) |
| `backup/ext_metadata.go` | `--gen-ext-metadata` generates YAML with segment info + table column definitions |
| `history/history.go` | `ListBackups`, `DeleteBackup`, `FindDependentBackups` |
| `toc/toc.go` | `HeapEntry{FileHashMD5}`, `AOEntry` gains `FileHashMD5` field |
| `gpbackup_ext_query.sh` | External table creation via gpfdist, `--use-metadata` for cross-cluster |

### GP Version Compatibility (critical for modifications)

When adding segment-level queries:
- **GP5**: `relstorage IN ('ao','co')`, `pg_filespace_entry` for datadir, `plpythonu`, `gp_session_role=utility`
- **GP6+**: `datadir` column in `gp_segment_configuration`, `plpython3u`, `gp_session_role=utility`
- **GP6.21+**: `LOCK TABLE ... MASTER ONLY`
- **GP7+/Cloudberry**: `pg_am.amname IN ('ao_row','ao_column')`, `gp_role=utility`, `COORDINATOR ONLY`

The version-detection layer parses the GP/Cloudberry version string at
startup. **Apache Cloudberry 2.x is NOT recognised** by this fork as
of `82b102f` (it falls through and aborts with "GPDB version … is not
supported. Please upgrade to GPDB 5.1.0 or later."). Only Cloudberry
1.x / GP 5.x–7.x work end-to-end.

### Flag Registration

New flags go in `options/flag.go`: add const + register in `SetBackupFlagDefaults()` (and `SetRestoreFlagDefaults()` if needed). Validation in `backup/validate.go` `validateFlagCombinations()`.

## Documentation

- `docs/USER_GUIDE_CN.md` / `docs/USER_GUIDE_EN.md` — full user manuals
- `docs/LOCK_ANALYSIS_CN.md` / `docs/LOCK_ANALYSIS_EN.md` — lock impact analysis
- `docs/PERFORMANCE_ANALYSIS.md` / `docs/PERFORMANCE_ANALYSIS_EN.md` — hash collection overhead analysis
