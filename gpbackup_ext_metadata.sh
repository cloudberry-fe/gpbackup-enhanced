#!/bin/bash
#
# gpbackup_ext_metadata.sh — 在 gpbackup 完成后生成外部表元数据文件
#
# 生成两个文件:
#   1. gpbackup_<TS>_ext_metadata.yaml  — segment 信息 + 表列定义（只读，备份时生成）
#   2. gpbackup_<TS>_ext_config.yaml    — 可编辑的配置文件（主机映射 + 目录映射）
#
# 使用方式:
#   gpbackup 完成后执行:
#     gpbackup_ext_metadata.sh --timestamp <TS> --backup-dir <DIR> --dbname <DB>
#
#   也可以集成到 gpbackup 的 post-backup hook 中自动执行
#

set -uo pipefail

TIMESTAMP=""
BACKUP_DIR=""
DBNAME=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --timestamp)  TIMESTAMP="$2"; shift 2;;
        --backup-dir) BACKUP_DIR="$2"; shift 2;;
        --dbname)     DBNAME="$2"; shift 2;;
        --help|-h)
            echo "Usage: gpbackup_ext_metadata.sh --timestamp <TS> --backup-dir <DIR> --dbname <DB>"
            echo "Generates external table metadata files for cross-cluster backup querying."
            exit 0;;
        *) echo "Unknown: $1"; exit 1;;
    esac
done

[[ -z "$TIMESTAMP" ]] && echo "ERROR: --timestamp required" && exit 1
[[ -z "$BACKUP_DIR" ]] && echo "ERROR: --backup-dir required" && exit 1
[[ -z "$DBNAME" ]]     && echo "ERROR: --dbname required" && exit 1

DATE_DIR=${TIMESTAMP:0:8}
META_DIR="$BACKUP_DIR/gpseg-1/backups/$DATE_DIR/$TIMESTAMP"
TOC_FILE="$META_DIR/gpbackup_${TIMESTAMP}_toc.yaml"
META_FILE="$META_DIR/gpbackup_${TIMESTAMP}_ext_metadata.yaml"
CONFIG_FILE="$META_DIR/gpbackup_${TIMESTAMP}_ext_config.yaml"

[[ ! -d "$META_DIR" ]] && echo "ERROR: Backup directory not found: $META_DIR" && exit 1
[[ ! -f "$TOC_FILE" ]] && echo "ERROR: TOC file not found: $TOC_FILE" && exit 1

echo "Generating external table metadata for timestamp $TIMESTAMP..."

# ── 1. Collect segment info ──
echo "  Collecting segment information..."

HAS_DATADIR=$(psql -d "$DBNAME" -tA -c "
    SELECT count(*) FROM pg_attribute a
    JOIN pg_class c ON a.attrelid = c.oid
    WHERE c.relname = 'gp_segment_configuration' AND a.attname = 'datadir';" 2>/dev/null)

if [[ "$HAS_DATADIR" -gt 0 ]]; then
    SEG_QUERY="SELECT content, hostname, port, datadir
        FROM gp_segment_configuration WHERE role = 'p' AND content >= 0 ORDER BY content"
else
    SEG_QUERY="SELECT c.content, c.hostname, c.port, f.fselocation
        FROM gp_segment_configuration c
        JOIN pg_filespace_entry f ON c.dbid = f.fsedbid
        WHERE c.role = 'p' AND c.content >= 0
        AND f.fsefsoid = (SELECT oid FROM pg_filespace WHERE fsname = 'pg_system')
        ORDER BY c.content"
fi

SEG_INFO=$(psql -d "$DBNAME" -tA -F'|' -c "$SEG_QUERY" 2>/dev/null)

# ── 2. Collect table column definitions ──
echo "  Collecting table column definitions..."

# Parse TOC for table list
TABLE_LIST=$(python2 -c "
import yaml
with open('$TOC_FILE') as f:
    toc = yaml.safe_load(f)
for e in toc.get('dataentries', []):
    print('%s|%s|%s' % (e.get('schema',''), e.get('name',''), e.get('oid',0)))
" 2>/dev/null)

# ── 3. Write metadata YAML ──
echo "  Writing metadata file..."

cat > "$META_FILE" <<YAML_HEADER
# gpbackup external table metadata
# Generated: $(date '+%Y-%m-%d %H:%M:%S')
# Timestamp: $TIMESTAMP
# Database: $DBNAME
# This file is read-only. Do not modify.

segments:
YAML_HEADER

while IFS='|' read -r content hostname port datadir; do
    [[ -z "$content" ]] && continue
    cat >> "$META_FILE" <<YAML_SEG
  - content: $content
    hostname: "$hostname"
    port: $port
    datadir: "$datadir"
YAML_SEG
done <<< "$SEG_INFO"

echo "" >> "$META_FILE"
echo "tables:" >> "$META_FILE"

while IFS='|' read -r schema name oid; do
    [[ -z "$schema" || -z "$name" ]] && continue

    COL_DEF=$(psql -d "$DBNAME" -tA -F'|' -c "
        SELECT attname, format_type(atttypid, atttypmod)
        FROM pg_attribute a
        JOIN pg_class c ON a.attrelid = c.oid
        JOIN pg_namespace n ON c.relnamespace = n.oid
        WHERE n.nspname = '$schema' AND c.relname = '$name'
        AND a.attnum > 0 AND NOT a.attisdropped
        ORDER BY a.attnum;" 2>/dev/null)

    [[ -z "$COL_DEF" ]] && continue

    cat >> "$META_FILE" <<YAML_TBL
  - schema: "$schema"
    name: "$name"
    oid: $oid
    columns:
YAML_TBL

    while IFS='|' read -r col_name col_type; do
        [[ -z "$col_name" ]] && continue
        echo "      - name: \"$col_name\"" >> "$META_FILE"
        echo "        type: \"$col_type\"" >> "$META_FILE"
    done <<< "$COL_DEF"

done <<< "$TABLE_LIST"

# ── 4. Write editable config YAML ──
echo "  Writing config file..."

cat > "$CONFIG_FILE" <<CONFIG
# gpbackup external table config
# Edit this file to remap hosts/paths when querying backups on a different cluster.
#
# timestamp: The backup timestamp this config belongs to
# backup_dir: The backup directory path (change if backup files are moved)
# gpfdist_port: Port for gpfdist services (0 = auto random)
# host_map: Map original hostnames to new hostnames
#   - If backup files are on the same hosts, leave as-is
#   - If copied to different hosts, update the "target" fields
# path_map: Map original backup paths to new paths (if backup-dir changed)
#   - Only needed if the backup-dir path differs on the target hosts

timestamp: "$TIMESTAMP"
database: "$DBNAME"
backup_dir: "$BACKUP_DIR"
gpfdist_port: 0

# Host mapping: original -> target
# Change "target" to the actual hostname where backup files are located
host_map:
CONFIG

# Collect unique hosts
UNIQUE_HOSTS=$(echo "$SEG_INFO" | cut -d'|' -f2 | sort -u)
for host in $UNIQUE_HOSTS; do
    echo "  - original: \"$host\"" >> "$CONFIG_FILE"
    echo "    target: \"$host\"" >> "$CONFIG_FILE"
done

cat >> "$CONFIG_FILE" <<CONFIG2

# Path mapping (optional): if backup_dir is different on target hosts
# path_map:
#   - original: "/data0/hashdata/backup"
#     target: "/data1/restore/backup"
CONFIG2

echo ""
echo "Generated:"
echo "  Metadata: $META_FILE ($(wc -c < "$META_FILE" | tr -d ' ') bytes)"
echo "  Config:   $CONFIG_FILE ($(wc -c < "$CONFIG_FILE" | tr -d ' ') bytes)"
echo ""
echo "To query backups on another cluster:"
echo "  1. Copy backup files + these two files to the target"
echo "  2. Edit $CONFIG_FILE to update host_map and backup_dir"
echo "  3. Run: gpbackup_ext_query.sh --timestamp $TIMESTAMP --backup-dir <new_dir> --dbname <db> --ext-schema ext_bak --use-metadata"
