# Greenplum Backup (Enhanced Edition)

`gpbackup` and `gprestore` are Go utilities for performing Greenplum Database backups. This enhanced edition adds **heap table incremental backup**, **AO partition-level change detection**, **backup set management**, and **external table backup querying**.

## Key Enhancements

| Feature | Description |
|---------|-------------|
| **Heap table incremental** | Detects heap table changes via `pg_stat_file` after CHECKPOINT |
| **AO partition-level detection** | Uses `pg_aoseg` content hash (`eof` + `tupcount`) to solve GP5 modcount propagation |
| **Backup management** | `--list-backups` / `--delete-backup` with cascade delete and physical file cleanup |
| **External table query** | `gpbackup_ext_query.sh` — query backup data via gpfdist + external tables without restore |
| **GP5/6/7 compatible** | Auto-adapts plpythonu/plpython3u, gp_session_role/gp_role, pg_filespace_entry/datadir |

## Documentation

- **[User Guide (English)](docs/USER_GUIDE_EN.md)** — full usage manual with examples and best practices
- **[User Guide (中文)](docs/USER_GUIDE_CN.md)** — 完整中文使用手册

## Quick Start

### Prerequisites

- Go 1.19+ (build only)
- Greenplum Database 5.x / 6.x / 7.x, Apache Cloudberry, or Euler Database
- Passwordless SSH between coordinator and segment hosts (for `--delete-backup` and `gpbackup_ext_query.sh`)

### Build

```bash
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

### Install

```bash
cp gpbackup gprestore gpbackup_helper $GPHOME/bin/
cp gpbackup_ext_query.sh $GPHOME/bin/
chmod 755 $GPHOME/bin/gpbackup $GPHOME/bin/gprestore $GPHOME/bin/gpbackup_helper $GPHOME/bin/gpbackup_ext_query.sh
```

### Usage

```bash
# Full backup (with AO content hash baseline)
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --ao-file-hash

# Incremental backup (partition-level precision)
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --heap-file-hash --ao-file-hash

# Restore (automatically handles full + incremental)
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --create-db

# List all backups
gpbackup --list-backups --backup-dir /data/backup

# Delete backup (cascades to dependent incrementals + physical files)
gpbackup --delete-backup 20260422120000 --backup-dir /data/backup

# Query backup data via external tables (no restore needed)
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname mydb --ext-schema ext_bak

psql -d mydb -c "SELECT count(*) FROM ext_bak.orders;"

# Cleanup external tables and gpfdist
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname mydb --ext-schema ext_bak --stop
```

## New Parameters

### gpbackup

| Parameter | Description |
|-----------|-------------|
| `--heap-file-hash` | Use file hash to detect heap table changes. Independently usable with full backup (baseline) or incremental |
| `--ao-file-hash` | Use aoseg content hash for AO tables. Independently usable with full backup (baseline) or incremental (partition-level detection) |
| `--gen-ext-metadata` | Generate external table metadata files after backup for cross-cluster querying |
| `--list-backups` | List all backups in `--backup-dir` and exit |
| `--delete-backup <TS>` | Delete backup and dependent incrementals, including physical files on all hosts |

### gprestore

| Parameter | Description |
|-----------|-------------|
| `--list-backups` | List all backups in `--backup-dir` and exit |
| `--delete-backup <TS>` | Delete backup and dependent incrementals |

### gpbackup_ext_query.sh

| Parameter | Description |
|-----------|-------------|
| `--timestamp <TS>` | Backup timestamp |
| `--backup-dir <DIR>` | Backup directory |
| `--dbname <DB>` | Target database for external tables |
| `--ext-schema <NAME>` | External table schema. Default: `<source_schema>_<timestamp>` |
| `--include-table <S.T>` | Filter to specific table(s) |
| `--include-schema <S>` | Filter to specific schema(s) |
| `--gpfdist-port <PORT>` | gpfdist port (default: random) |
| `--gen-metadata` | Generate metadata files for cross-cluster querying |
| `--use-metadata` | Read table structures from metadata file (no source DB needed) |
| `--stop` | Stop gpfdist and drop external tables |

## Incremental Detection Modes

| Mode | Heap Tables | AO Tables | Best For |
|------|-------------|-----------|----------|
| `--incremental` | Always backup (original) | modcount + DDL timestamp | General use |
| `+ --heap-file-hash` | File hash (mtime+size) | modcount + DDL timestamp | Skip unchanged heap tables |
| `+ --ao-file-hash` | Always backup (original) | aoseg content hash (eof+tupcount) | GP5 partition tables |
| `+ both` | File hash (mtime+size) | aoseg content hash (eof+tupcount) | Full precision |

## Project Structure

```
gpbackup/
├── gpbackup.go              # gpbackup entry point
├── gprestore.go             # gprestore entry point
├── gpbackup_helper.go       # segment helper entry point
├── gpbackup_ext_query.sh    # external table query tool
├── backup/
│   ├── backup.go            # main backup logic
│   ├── incremental.go       # incremental change detection (enhanced)
│   ├── queries_incremental.go  # SQL queries for incremental metadata (enhanced)
│   ├── manage.go            # --list-backups / --delete-backup implementation
│   └── wrappers.go          # metadata collection orchestration (enhanced)
├── restore/
│   ├── restore.go           # main restore logic
│   └── manage.go            # --list-backups / --delete-backup implementation
├── history/
│   └── history.go           # backup history DB operations (enhanced)
├── toc/
│   └── toc.go               # TOC types with HeapEntry + FileHashMD5 (enhanced)
├── options/
│   └── flag.go              # CLI flag definitions (enhanced)
├── docs/
│   ├── USER_GUIDE_EN.md     # English user guide
│   └── USER_GUIDE_CN.md     # Chinese user guide
└── ...
```

## Original Documentation

The original gpbackup project documentation is available at the [gpbackup wiki](https://github.com/greenplum-db/gpbackup/wiki).

## Development

### Running Tests

```bash
make unit           # unit tests
make integration    # integration tests (requires running GPDB)
make end_to_end     # end-to-end tests (requires running GPDB)
```

### Code Formatting

```bash
make format         # auto-format with goimports + gofmt
make lint           # run linter
```

## License

See [LICENSE](LICENSE) file.
