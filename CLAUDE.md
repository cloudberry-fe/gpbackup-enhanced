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

### Flag Registration

New flags go in `options/flag.go`: add const + register in `SetBackupFlagDefaults()` (and `SetRestoreFlagDefaults()` if needed). Validation in `backup/validate.go` `validateFlagCombinations()`.

## Documentation

- `docs/USER_GUIDE_CN.md` / `docs/USER_GUIDE_EN.md` — full user manuals
- `docs/LOCK_ANALYSIS_CN.md` / `docs/LOCK_ANALYSIS_EN.md` — lock impact analysis
- `docs/PERFORMANCE_ANALYSIS.md` / `docs/PERFORMANCE_ANALYSIS_EN.md` — hash collection overhead analysis
