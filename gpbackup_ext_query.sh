#!/bin/bash
# gpbackup_ext_query.sh — 通过外部表直接查询 gpbackup 备份集数据
set -uo pipefail

# ── Defaults ──
TIMESTAMP=""
BACKUP_DIR=""
DBNAME=""
EXT_SCHEMA=""
GPFDIST_PORT=""
INCLUDE_TABLES=()
INCLUDE_SCHEMAS=()
DO_STOP=false
USE_METADATA=false
GEN_METADATA=false

usage() {
    cat <<'EOF'
gpbackup_ext_query.sh — 通过外部表直接查询 gpbackup 备份集数据

用法:
  gpbackup_ext_query.sh --timestamp <TS> --backup-dir <DIR> --dbname <DB> [OPTIONS]

必填参数:
  --timestamp <TS>         备份时间戳
  --backup-dir <DIR>       备份目录
  --dbname <DB>            创建外部表的目标数据库

可选参数:
  --ext-schema <NAME>      外部表 schema 名称 (默认: ext_backup)
  --include-table <S.T>    只创建指定表的外部表 (可多次指定)
  --include-schema <S>     只创建指定 schema 中的表 (可多次指定)
  --gpfdist-port <PORT>    gpfdist 端口 (默认: 随机 18000-19000)
  --stop                   停止 gpfdist 并删除外部表
  --gen-metadata           生成元数据文件 (segment 信息 + 表列定义), 需连接源数据库
  --use-metadata           使用元数据文件创建外部表, 不依赖源数据库的表结构
  --help                   显示帮助

工作模式:
  默认模式:       从 --dbname 数据库读取表结构和 segment 信息
  --gen-metadata: 在备份完成后执行, 将表结构和 segment 信息保存到元数据文件
  --use-metadata: 从元数据文件读取, 可在任意集群上创建外部表查询备份数据
                  同时读取 ext_config.yaml 中的主机映射 (备份文件拷贝到其他服务器时使用)

示例:
  # 1. 备份后生成元数据文件 (在源集群上执行一次)
  gpbackup_ext_query.sh --timestamp 20260422194405 \
      --backup-dir /data/backup --dbname mydb --gen-metadata

  # 2. 在源集群上创建外部表查询 (默认模式)
  gpbackup_ext_query.sh --timestamp 20260422194405 \
      --backup-dir /data/backup --dbname mydb --ext-schema ext_bak

  # 3. 将备份文件拷贝到其他服务器后, 编辑 ext_config.yaml 修改主机映射,
  #    然后在目标集群上通过元数据文件创建外部表
  gpbackup_ext_query.sh --timestamp 20260422194405 \
      --backup-dir /new_path/backup --dbname target_db \
      --ext-schema ext_bak --use-metadata

  # 4. 清理
  gpbackup_ext_query.sh --timestamp 20260422194405 \
      --backup-dir /data/backup --dbname mydb --ext-schema ext_bak --stop
EOF
    exit 0
}

# ── Parse args ──
while [[ $# -gt 0 ]]; do
    case "$1" in
        --timestamp)      TIMESTAMP="$2"; shift 2;;
        --backup-dir)     BACKUP_DIR="$2"; shift 2;;
        --dbname)         DBNAME="$2"; shift 2;;
        --ext-schema)     EXT_SCHEMA="$2"; shift 2;;
        --gpfdist-port)   GPFDIST_PORT="$2"; shift 2;;
        --include-table)  INCLUDE_TABLES+=("$2"); shift 2;;
        --include-schema) INCLUDE_SCHEMAS+=("$2"); shift 2;;
        --stop)           DO_STOP=true; shift;;
        --use-metadata)   USE_METADATA=true; shift;;
        --gen-metadata)   GEN_METADATA=true; shift;;
        --help|-h)        usage;;
        *) echo "Unknown option: $1"; exit 1;;
    esac
done

[[ -z "$TIMESTAMP" ]] && echo "ERROR: --timestamp is required" && exit 1
[[ -z "$BACKUP_DIR" ]] && echo "ERROR: --backup-dir is required" && exit 1
[[ -z "$DBNAME" ]]     && echo "ERROR: --dbname is required" && exit 1

DATE_DIR=${TIMESTAMP:0:8}
TAG="gpfdist_backup_${TIMESTAMP}"
META_DIR="$BACKUP_DIR/gpseg-1/backups/$DATE_DIR/$TIMESTAMP"
META_FILE="$META_DIR/gpbackup_${TIMESTAMP}_ext_metadata.yaml"
EXT_CONFIG_FILE="$META_DIR/gpbackup_${TIMESTAMP}_ext_config.yaml"
TOC_FILE="$META_DIR/gpbackup_${TIMESTAMP}_toc.yaml"

# ══════════════════════════════════════════════════════════════
# Get segment info: from database or metadata file
# ══════════════════════════════════════════════════════════════
get_segment_info_from_db() {
    local HAS_DATADIR=$(psql -d "$DBNAME" -tA -c "
        SELECT count(*) FROM pg_attribute a JOIN pg_class c ON a.attrelid = c.oid
        WHERE c.relname = 'gp_segment_configuration' AND a.attname = 'datadir';" 2>/dev/null)
    if [[ "$HAS_DATADIR" -gt 0 ]]; then
        psql -d "$DBNAME" -tA -F'|' -c "
            SELECT content, hostname, port, datadir
            FROM gp_segment_configuration WHERE role = 'p' AND content >= 0 ORDER BY content;"
    else
        psql -d "$DBNAME" -tA -F'|' -c "
            SELECT c.content, c.hostname, c.port, f.fselocation
            FROM gp_segment_configuration c JOIN pg_filespace_entry f ON c.dbid = f.fsedbid
            WHERE c.role = 'p' AND c.content >= 0
            AND f.fsefsoid = (SELECT oid FROM pg_filespace WHERE fsname = 'pg_system')
            ORDER BY c.content;"
    fi
}

get_segment_info_from_metadata() {
    # Read from ext_config.yaml for host mapping, ext_metadata.yaml for segment list
    python2 -c "
import yaml
with open('$META_FILE') as f:
    meta = yaml.safe_load(f)

host_map = {}
try:
    with open('$EXT_CONFIG_FILE') as f:
        cfg = yaml.safe_load(f)
    for m in cfg.get('host_map', []):
        host_map[m['original']] = m['target']
except:
    pass

for seg in meta.get('segments', []):
    host = seg['hostname']
    host = host_map.get(host, host)
    print('%s|%s|%s|%s' % (seg['content'], host, seg['port'], seg['datadir']))
" 2>/dev/null
}

# ══════════════════════════════════════════════════════════════
# Get column definitions: from database or metadata file
# ══════════════════════════════════════════════════════════════
get_col_def_from_db() {
    local schema=$1 name=$2
    psql -d "$DBNAME" -tA -c "
        SELECT string_agg(quote_ident(attname) || ' ' || format_type(atttypid, atttypmod), ', ' ORDER BY attnum)
        FROM pg_attribute a JOIN pg_class c ON a.attrelid = c.oid
        JOIN pg_namespace n ON c.relnamespace = n.oid
        WHERE n.nspname = '$schema' AND c.relname = '$name'
        AND a.attnum > 0 AND NOT a.attisdropped;" 2>/dev/null
}

get_col_def_from_metadata() {
    local schema=$1 name=$2
    python2 -c "
import yaml
with open('$META_FILE') as f:
    meta = yaml.safe_load(f)
for t in meta.get('tables', []):
    if t['schema'] == '$schema' and t['name'] == '$name':
        cols = []
        for c in t.get('columns', []):
            cols.append('\"' + c['name'] + '\" ' + c['type'])
        print(', '.join(cols))
        break
" 2>/dev/null
}

# ══════════════════════════════════════════════════════════════
# --gen-metadata mode: generate metadata files
# ══════════════════════════════════════════════════════════════
if $GEN_METADATA; then
    [[ ! -f "$TOC_FILE" ]] && echo "ERROR: TOC file not found: $TOC_FILE" && exit 1

    echo "Generating external table metadata for timestamp $TIMESTAMP..."

    # 1. Segment info
    echo "  Collecting segment information..."
    SEG_INFO=$(get_segment_info_from_db)

    # 2. Table list from TOC
    TABLE_LIST=$(python2 -c "
import yaml
with open('$TOC_FILE') as f:
    toc = yaml.safe_load(f)
for e in toc.get('dataentries', []):
    print('%s|%s|%s' % (e.get('schema',''), e.get('name',''), e.get('oid',0)))
" 2>/dev/null)

    # 3. Write metadata YAML
    echo "  Writing metadata file..."
    cat > "$META_FILE" <<YAML_HDR
# gpbackup external table metadata
# Generated: $(date '+%Y-%m-%d %H:%M:%S')
# Timestamp: $TIMESTAMP
# Database: $DBNAME

segments:
YAML_HDR

    while IFS='|' read -r content hostname port datadir; do
        [[ -z "$content" ]] && continue
        echo "  - content: $content" >> "$META_FILE"
        echo "    hostname: \"$hostname\"" >> "$META_FILE"
        echo "    port: $port" >> "$META_FILE"
        echo "    datadir: \"$datadir\"" >> "$META_FILE"
    done <<< "$SEG_INFO"

    echo "" >> "$META_FILE"
    echo "tables:" >> "$META_FILE"

    TABLE_COUNT=0
    while IFS='|' read -r schema name oid; do
        [[ -z "$schema" || -z "$name" ]] && continue
        COL_INFO=$(psql -d "$DBNAME" -tA -F'|' -c "
            SELECT attname, format_type(atttypid, atttypmod)
            FROM pg_attribute a JOIN pg_class c ON a.attrelid = c.oid
            JOIN pg_namespace n ON c.relnamespace = n.oid
            WHERE n.nspname = '$schema' AND c.relname = '$name'
            AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum;" 2>/dev/null)
        [[ -z "$COL_INFO" ]] && continue

        echo "  - schema: \"$schema\"" >> "$META_FILE"
        echo "    name: \"$name\"" >> "$META_FILE"
        echo "    oid: $oid" >> "$META_FILE"
        echo "    columns:" >> "$META_FILE"
        while IFS='|' read -r cname ctype; do
            [[ -z "$cname" ]] && continue
            echo "      - name: \"$cname\"" >> "$META_FILE"
            echo "        type: \"$ctype\"" >> "$META_FILE"
        done <<< "$COL_INFO"
        TABLE_COUNT=$((TABLE_COUNT+1))
    done <<< "$TABLE_LIST"

    # 4. Write editable config YAML
    echo "  Writing config file..."
    UNIQUE_HOSTS=$(echo "$SEG_INFO" | cut -d'|' -f2 | sort -u)

    cat > "$EXT_CONFIG_FILE" <<CFG
# gpbackup external table config — 可编辑
# 将备份文件拷贝到其他服务器后, 修改此文件中的 host_map 和 backup_dir
#
# backup_dir: 备份目录路径 (如果备份文件移动了, 修改此项)
# gpfdist_port: gpfdist 端口 (0 = 随机)
# host_map: 主机映射, 将原始主机名映射到备份文件实际所在的主机名
#   original: 备份时的原始主机名
#   target:   备份文件当前所在的主机名 (默认与 original 相同)

timestamp: "$TIMESTAMP"
database: "$DBNAME"
backup_dir: "$BACKUP_DIR"
gpfdist_port: 0

host_map:
CFG
    for host in $UNIQUE_HOSTS; do
        echo "  - original: \"$host\"" >> "$EXT_CONFIG_FILE"
        echo "    target: \"$host\"" >> "$EXT_CONFIG_FILE"
    done

    echo ""
    echo "Generated:"
    echo "  Metadata:  $META_FILE ($TABLE_COUNT tables)"
    echo "  Config:    $EXT_CONFIG_FILE"
    echo ""
    echo "跨集群使用步骤:"
    echo "  1. 将备份文件和这两个文件拷贝到目标服务器"
    echo "  2. 编辑 ext_config.yaml, 修改 host_map 中的 target 和 backup_dir"
    echo "  3. 执行: gpbackup_ext_query.sh --timestamp $TIMESTAMP --backup-dir <新路径> --dbname <目标库> --ext-schema ext_bak --use-metadata"
    exit 0
fi

# ══════════════════════════════════════════════════════════════
# --stop mode
# ══════════════════════════════════════════════════════════════
if $DO_STOP; then
    echo "Stopping gpfdist and cleaning up external tables..."
    echo "  Tag: $TAG"

    # Determine which schemas to clean
    STOP_SCHEMAS=()
    if [[ -n "$EXT_SCHEMA" ]]; then
        STOP_SCHEMAS=("$EXT_SCHEMA")
    else
        # Auto mode: find all schemas matching *_<TIMESTAMP>
        STOP_SCHEMAS=($(psql -d "$DBNAME" -tA -c "
            SELECT nspname FROM pg_namespace WHERE nspname LIKE '%_${TIMESTAMP}';" 2>/dev/null))
    fi

    DROP_COUNT=0
    for stop_schema in "${STOP_SCHEMAS[@]:-}"; do
        [[ -z "$stop_schema" ]] && continue

        # Collect tables to drop in this schema
        EXT_TABLES=""
        if [[ ${#INCLUDE_TABLES[@]} -gt 0 ]]; then
            for tbl in "${INCLUDE_TABLES[@]:-}"; do
                [[ -z "$tbl" ]] && continue
                tname="${tbl##*.}"
                EXT_TABLES="$EXT_TABLES ${stop_schema}.${tname}"
            done
        else
            EXT_TABLES=$(psql -d "$DBNAME" -tA -c "
                SELECT '${stop_schema}.' || c.relname
                FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid
                WHERE n.nspname = '${stop_schema}';" 2>/dev/null)
        fi

        for ext_tbl in $EXT_TABLES; do
            ext_tbl=$(echo "$ext_tbl" | tr -d '[:space:]')
            [[ -z "$ext_tbl" ]] && continue
            psql -d "$DBNAME" -c "DROP EXTERNAL TABLE IF EXISTS ${ext_tbl};" 2>/dev/null && {
                echo "  Dropped: $ext_tbl"
                DROP_COUNT=$((DROP_COUNT+1))
            }
        done

        echo "  Trying to drop schema $stop_schema (without CASCADE)..."
        DROP_RESULT=$(psql -d "$DBNAME" -c "DROP SCHEMA ${stop_schema};" 2>&1)
        if echo "$DROP_RESULT" | grep -q "DROP SCHEMA"; then
            echo "  Schema $stop_schema dropped."
        else
            echo "  Schema $stop_schema not dropped (other objects remain, this is expected)."
        fi
    done
    echo "  Dropped $DROP_COUNT external table(s)."

    # Get hosts to kill gpfdist: from config file or database
    HOSTS=""
    if [[ -f "$EXT_CONFIG_FILE" ]]; then
        HOSTS=$(python2 -c "
import yaml
with open('$EXT_CONFIG_FILE') as f:
    cfg = yaml.safe_load(f)
for m in cfg.get('host_map', []):
    print(m.get('target', m.get('original', '')))
" 2>/dev/null | sort -u)
    fi
    if [[ -z "$HOSTS" ]]; then
        HOSTS=$(psql -d "$DBNAME" -tA -c "SELECT DISTINCT hostname FROM gp_segment_configuration WHERE content >= 0;" 2>/dev/null)
    fi

    for host in $HOSTS; do
        echo "  Stopping gpfdist on $host..."
        ssh -o StrictHostKeyChecking=no -o BatchMode=yes -f "$host" \
            "cd / && pkill -f \"$TAG\" 2>/dev/null; true" </dev/null 2>/dev/null || true
    done
    echo "  Stopping gpfdist on coordinator..."
    pkill -f "$TAG" 2>/dev/null || true
    echo "Cleanup complete."
    exit 0
fi

# ══════════════════════════════════════════════════════════════
# Create external tables mode
# ══════════════════════════════════════════════════════════════
[[ ! -f "$TOC_FILE" ]] && echo "ERROR: TOC file not found: $TOC_FILE" && exit 1

if $USE_METADATA; then
    [[ ! -f "$META_FILE" ]] && echo "ERROR: Metadata file not found: $META_FILE" && echo "Run with --gen-metadata first on the source cluster." && exit 1
    MODE_DESC="metadata file"
else
    MODE_DESC="database ($DBNAME)"
fi

# Read backup_dir override from config if --use-metadata
EFFECTIVE_BACKUP_DIR="$BACKUP_DIR"
if $USE_METADATA && [[ -f "$EXT_CONFIG_FILE" ]]; then
    CFG_PORT=$(python2 -c "
import yaml
with open('$EXT_CONFIG_FILE') as f:
    cfg = yaml.safe_load(f)
print(cfg.get('gpfdist_port', 0))
" 2>/dev/null)
    if [[ -n "$CFG_PORT" && "$CFG_PORT" != "0" && -z "$GPFDIST_PORT" ]]; then
        GPFDIST_PORT="$CFG_PORT"
    fi
fi

echo "============================================"
echo "  gpbackup External Table Query Setup"
echo "============================================"
echo "  Timestamp:  $TIMESTAMP"
echo "  Backup Dir: $BACKUP_DIR"
echo "  Database:   $DBNAME"
if [[ -n "$EXT_SCHEMA" ]]; then
    echo "  Ext Schema: $EXT_SCHEMA"
else
    echo "  Ext Schema: <source_schema>_${TIMESTAMP} (auto)"
fi
echo "  Source:     $MODE_DESC"
echo "============================================"
echo ""

# ── Parse TOC for table list ──
echo "Parsing backup TOC..."
TABLES_INFO=$(python2 -c "
import yaml
with open('$TOC_FILE') as f:
    toc = yaml.safe_load(f)
entries = toc.get('dataentries', [])
include_tables = set('${INCLUDE_TABLES[*]:-}'.split()) if '${INCLUDE_TABLES[*]:-}' else set()
include_schemas = set('${INCLUDE_SCHEMAS[*]:-}'.split()) if '${INCLUDE_SCHEMAS[*]:-}' else set()
for e in entries:
    schema = e.get('schema', '')
    name = e.get('name', '')
    oid = e.get('oid', 0)
    fqn = schema + '.' + name
    if include_tables and fqn not in include_tables:
        continue
    if include_schemas and schema not in include_schemas:
        continue
    print('%s|%s|%s' % (schema, name, oid))
" 2>/dev/null)

[[ -z "$TABLES_INFO" ]] && echo "ERROR: No tables found in TOC" && exit 1
TABLE_COUNT=$(echo "$TABLES_INFO" | wc -l | tr -d ' ')
echo "Found $TABLE_COUNT table(s)."

# ── Get segment info ──
echo "Getting segment information..."
if $USE_METADATA; then
    SEG_INFO=$(get_segment_info_from_metadata)
else
    SEG_INFO=$(get_segment_info_from_db)
fi
SEG_COUNT=$(echo "$SEG_INFO" | wc -l | tr -d ' ')
echo "Cluster has $SEG_COUNT primary segments."
UNIQUE_HOSTS=$(echo "$SEG_INFO" | cut -d'|' -f2 | sort -u)

# ── gpfdist port ──
[[ -z "$GPFDIST_PORT" ]] && GPFDIST_PORT=$((18000 + RANDOM % 1000))
echo "Using gpfdist port: $GPFDIST_PORT"

# ── Start gpfdist ──
echo ""
echo "Starting gpfdist services..."
for host in $UNIQUE_HOSTS; do
    echo "  Starting gpfdist on $host:$GPFDIST_PORT..."
    ssh -o StrictHostKeyChecking=no -o BatchMode=yes -f "$host" \
        "cd / ; gpfdist -d $BACKUP_DIR -p $GPFDIST_PORT -m 1048576 >>/tmp/$TAG.log 2>&1 &" </dev/null
done
COORD_HOST=$(hostname)
echo "  Starting gpfdist on coordinator $COORD_HOST:$GPFDIST_PORT..."
gpfdist -d "$BACKUP_DIR" -p "$GPFDIST_PORT" -m 1048576 >>/tmp/$TAG.log 2>&1 &
sleep 2
echo "  gpfdist services started."

# ── Determine schema naming mode ──
# If --ext-schema is specified: all tables go into that single schema
# If not specified: each table goes into <original_schema>_<timestamp> schema
AUTO_SCHEMA=false
if [[ -z "$EXT_SCHEMA" ]]; then
    AUTO_SCHEMA=true
    echo "Schema mode: auto (<original_schema>_${TIMESTAMP})"
else
    echo "Schema mode: fixed ($EXT_SCHEMA)"
fi

# ── Pre-create schemas ──
echo ""
CREATED_SCHEMAS=()
if $AUTO_SCHEMA; then
    # Collect unique source schemas from table list
    UNIQUE_SCHEMAS=$(echo "$TABLES_INFO" | cut -d'|' -f1 | sort -u)
    for src_schema in $UNIQUE_SCHEMAS; do
        auto_name="${src_schema}_${TIMESTAMP}"
        echo "Creating schema $auto_name..."
        psql -d "$DBNAME" -c "DROP SCHEMA IF EXISTS $auto_name CASCADE;" 2>/dev/null
        psql -d "$DBNAME" -c "CREATE SCHEMA $auto_name;" 2>/dev/null
        CREATED_SCHEMAS+=("$auto_name")
    done
else
    echo "Creating schema $EXT_SCHEMA..."
    psql -d "$DBNAME" -c "DROP SCHEMA IF EXISTS $EXT_SCHEMA CASCADE;" 2>/dev/null
    psql -d "$DBNAME" -c "CREATE SCHEMA $EXT_SCHEMA;" 2>&1
    if ! psql -d "$DBNAME" -tA -c "SELECT 1 FROM pg_namespace WHERE nspname='$EXT_SCHEMA';" 2>/dev/null | grep -q 1; then
        echo "ERROR: Failed to create schema $EXT_SCHEMA"
        exit 1
    fi
    CREATED_SCHEMAS+=("$EXT_SCHEMA")
fi

# ── Create external tables ──
echo "Creating external tables..."
CREATED=0; FAILED=0

while IFS='|' read -r schema name oid; do
    [[ -z "$schema" || -z "$name" || -z "$oid" ]] && continue

    # Get column definitions
    if $USE_METADATA; then
        COL_DEF=$(get_col_def_from_metadata "$schema" "$name")
    else
        COL_DEF=$(get_col_def_from_db "$schema" "$name")
    fi

    if [[ -z "$COL_DEF" ]]; then
        echo "  SKIP: $schema.$name (column definitions not available)"
        FAILED=$((FAILED+1))
        continue
    fi

    # Build LOCATION
    LOCATIONS=""
    while IFS='|' read -r seg_content seg_host seg_port seg_datadir; do
        [[ -z "$seg_content" ]] && continue
        FILE_PATH="gpseg${seg_content}/backups/$DATE_DIR/$TIMESTAMP/gpbackup_${seg_content}_${TIMESTAMP}_${oid}.gz"
        [[ -n "$LOCATIONS" ]] && LOCATIONS="$LOCATIONS,"
        LOCATIONS="${LOCATIONS}
    'gpfdist://${seg_host}:${GPFDIST_PORT}/${FILE_PATH}'"
    done <<< "$SEG_INFO"

    [[ -z "$LOCATIONS" ]] && echo "  SKIP: $schema.$name (no segment info)" && FAILED=$((FAILED+1)) && continue

    if $AUTO_SCHEMA; then
        TARGET_SCHEMA="${schema}_${TIMESTAMP}"
    else
        TARGET_SCHEMA="$EXT_SCHEMA"
    fi
    EXT_TABLE_NAME="${TARGET_SCHEMA}.${name}"
    CREATE_SQL="
DROP EXTERNAL TABLE IF EXISTS ${EXT_TABLE_NAME};
CREATE EXTERNAL TABLE ${EXT_TABLE_NAME} (
    ${COL_DEF}
) LOCATION (${LOCATIONS}
) FORMAT 'CSV' (DELIMITER ',')
ENCODING 'UTF8'
LOG ERRORS SEGMENT REJECT LIMIT 10;"

    RESULT=$(psql -d "$DBNAME" -c "$CREATE_SQL" 2>&1)
    if [[ $? -eq 0 ]]; then
        echo "  OK: ${EXT_TABLE_NAME} -> $schema.$name (oid=$oid)"
        CREATED=$((CREATED+1))
    else
        echo "  FAIL: ${EXT_TABLE_NAME}: $RESULT"
        FAILED=$((FAILED+1))
    fi
done <<< "$TABLES_INFO"

# ── Summary ──
echo ""
echo "============================================"
echo "  Setup Complete"
echo "============================================"
echo "  External tables created: $CREATED"
echo "  Failed/skipped: $FAILED"
if $AUTO_SCHEMA; then
    echo "  Schemas: ${CREATED_SCHEMAS[*]}"
else
    echo "  Schema: $EXT_SCHEMA"
fi
echo "  gpfdist port: $GPFDIST_PORT"
echo ""
echo "  查询示例:"
if $AUTO_SCHEMA; then
    FIRST_SCHEMA="${CREATED_SCHEMAS[0]:-}"
    echo "    psql -d $DBNAME -c \"SELECT count(*) FROM ${FIRST_SCHEMA}.<table_name>;\""
else
    echo "    psql -d $DBNAME -c \"SELECT count(*) FROM ${EXT_SCHEMA}.<table_name>;\""
fi
echo ""
echo "  清理 (停止 gpfdist + 删除外部表):"
echo "    $0 --timestamp $TIMESTAMP --backup-dir $BACKUP_DIR --dbname $DBNAME --ext-schema $EXT_SCHEMA --stop"
echo ""
echo "  gpfdist 日志: /tmp/$TAG.log"
echo "============================================"
