# gpbackup / gprestore 增强版使用手册

**版本**: 1.30.5-custom (基于 gpbackup 1.30.5 增强)
**兼容**: Greenplum 5.x / 6.x / 7.x、Apache Cloudberry、Euler Database

---

## 目录

- [一、概述](#一概述)
- [二、安装部署](#二安装部署)
- [三、gpbackup 备份](#三gpbackup-备份)
  - [3.1 全量备份](#31-全量备份)
  - [3.2 增量备份（默认模式）](#32-增量备份默认模式)
  - [3.3 增量备份（Heap 表增量模式）](#33-增量备份heap-表增量模式)
  - [3.4 增量备份（AO 分区精确模式）](#34-增量备份ao-分区精确模式)
  - [3.5 备份参数一览](#35-备份参数一览)
- [四、gprestore 恢复](#四gprestore-恢复)
  - [4.1 全量恢复](#41-全量恢复)
  - [4.2 增量恢复](#42-增量恢复)
  - [4.3 指定表恢复](#43-指定表恢复)
  - [4.4 恢复参数一览](#44-恢复参数一览)
- [五、备份集管理](#五备份集管理)
  - [5.1 查看备份列表](#51-查看备份列表)
  - [5.2 删除备份集](#52-删除备份集)
- [六、通过外部表查询备份数据](#六通过外部表查询备份数据)
  - [6.1 创建外部表](#61-创建外部表)
  - [6.2 查询备份数据](#62-查询备份数据)
  - [6.3 清理外部表](#63-清理外部表)
  - [6.4 跨集群查询备份数据](#64-跨集群查询备份数据)
  - [6.5 参数一览](#65-参数一览)
- [七、增量备份检测机制说明](#七增量备份检测机制说明)
- [八、最佳实践](#八最佳实践)
- [九、常见问题](#九常见问题)

---

## 一、概述

本工具在 gpbackup 1.30.5 基础上增加了以下增强功能：

| 增强功能 | 说明 |
|---------|------|
| **Heap 表增量备份** | 通过 `pg_stat_file` 检测 Heap 表数据文件变化，实现 Heap 表增量备份 |
| **AO 表分区级精确检测** | 通过 aoseg 元数据内容哈希，解决 GP5 中 modcount 跨分区传播的问题 |
| **备份集管理** | `--list-backups` 查看备份列表，`--delete-backup` 删除备份集及物理文件 |
| **外部表查询备份数据** | 通过 gpfdist + 外部表，无需恢复即可直接 SQL 查询备份集中的数据 |
| **GP5/6/7 兼容** | 自动检测 GP 版本，适配 plpythonu/plpython3u、gp_session_role/gp_role 等差异 |

---

## 二、安装部署

### 2.1 编译

```bash
# 需要 Go 1.19+
export GOPROXY=https://goproxy.cn,direct

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

### 2.2 部署

```bash
# 复制到 GPHOME/bin（所有节点）
cp gpbackup gprestore gpbackup_helper $GPHOME/bin/
chmod 755 $GPHOME/bin/gpbackup $GPHOME/bin/gprestore $GPHOME/bin/gpbackup_helper

# 部署外部表查询脚本
cp gpbackup_ext_query.sh $GPHOME/bin/
chmod 755 $GPHOME/bin/gpbackup_ext_query.sh

# 验证
gpbackup --version
gprestore --version
```

---

## 三、gpbackup 备份

### 3.1 全量备份

```bash
# 基础全量备份
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data

# 全量备份 + 采集 AO 内容哈希（为后续 --ao-file-hash 增量建立基线）
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --ao-file-hash

# 全量备份 + 同时采集 Heap 和 AO 哈希基线
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --heap-file-hash --ao-file-hash

# 全量备份 + 多并发
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --jobs 4

# 仅备份指定 schema
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --include-schema public --include-schema sales

# 仅备份指定表
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --include-table public.orders --include-table public.customers
```

> **说明**: `--leaf-partition-data` 是增量备份的前提，建议所有备份都加上此参数。

### 3.2 增量备份（默认模式）

默认模式的检测机制：
- **AO 表**: 比较 `modcount` + `pg_stat_last_operation` DDL 时间戳
- **Heap 表**: 通过 `pg_stat_file` 比较数据文件的修改时间和大小（CHECKPOINT 后）

```bash
# 增量备份（基于最近一次全量或增量）
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --incremental

# 增量备份（基于指定时间戳）
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --from-timestamp 20260422120000
```

**行为**:
- AO 表: 仅备份 modcount 或 DDL 时间戳变化的表
- Heap 表: 仅备份数据文件发生变化的表
- 未变化的表: 跳过，不产生数据文件

### 3.3 增量备份（Heap 表增量模式）

`--heap-file-hash` 使 Heap 表也支持增量检测（不加此参数时 Heap 表每次增量都全量备份，与原版行为一致）：

```bash
# 全量备份时加 --heap-file-hash 建立 Heap 基线
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --heap-file-hash

# 增量备份时加 --heap-file-hash，未修改的 Heap 表将被跳过
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --heap-file-hash
```

**适用场景**: Heap 表数据量大但修改不频繁，希望减少增量备份数据量。

### 3.4 增量备份（AO 分区精确模式）

`--ao-file-hash` 独立使用，解决 GP5 中的分区表问题（不依赖 `--heap-file-hash`）：

**GP5 问题**: 向一个子分区 INSERT 数据时，父表和所有兄弟分区的 `modcount` 都会同步递增，导致增量备份时所有分区都被判定为"已修改"，全部重新备份。

**解决方案**: `--ao-file-hash` 使用每张表独立的 `pg_aoseg` 元数据内容哈希（基于 `eof` + `tupcount`，排除 `modcount`），只有实际发生数据变化的分区才会被检测到。

```bash
# 第一步: 全量备份时加 --ao-file-hash 建立基线
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --ao-file-hash

# 第二步: 增量备份，精确到子分区级别（--ao-file-hash 独立使用）
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --ao-file-hash

# 或同时启用两个参数，最大化减少备份量
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data \
    --incremental --heap-file-hash --ao-file-hash
```

**示例**:

```
全量备份: t_part 有 3 个子分区 (p1, p2, p3)，全部备份

↓ 仅向 p1 INSERT 100 行

增量备份 (默认模式):        备份 p1 + p2 + p3 (modcount 全部变了)
增量备份 (--ao-file-hash):  仅备份 p1           (只有 p1 的 eof/tupcount 变了)
```

### 3.5 备份参数一览

#### 原有参数

| 参数 | 说明 |
|------|------|
| `--dbname` | **必填** 要备份的数据库名 |
| `--backup-dir` | 备份文件存储目录 |
| `--leaf-partition-data` | 按叶子分区生成独立数据文件（增量备份必须） |
| `--incremental` | 增量备份模式 |
| `--from-timestamp` | 指定增量基准时间戳 |
| `--jobs N` | 并行备份连接数 |
| `--include-schema` | 仅备份指定 schema（可多次指定） |
| `--include-table` | 仅备份指定表（可多次指定） |
| `--exclude-schema` | 排除指定 schema |
| `--exclude-table` | 排除指定表 |
| `--data-only` | 仅备份数据 |
| `--metadata-only` | 仅备份元数据 |
| `--with-stats` | 同时备份统计信息 |
| `--no-compression` | 不压缩数据文件 |
| `--compression-type` | 压缩类型: gzip（默认）或 zstd |

#### 新增参数

| 参数 | 说明 |
|------|------|
| `--heap-file-hash` | 使用文件哈希检测 Heap 表变化（全量时建立基线，增量时跳过未修改的 Heap 表），可独立使用 |
| `--ao-file-hash` | 使用 aoseg 内容哈希检测 AO 表变化（全量时建立基线，增量时按分区精确检测），可独立使用 |
| `--gen-ext-metadata` | 备份完成后自动生成外部表元数据文件，用于跨集群查询备份数据 |
| `--list-backups` | 列出备份目录中的所有备份记录并退出 |
| `--delete-backup TS` | 删除指定时间戳的备份（全量备份会连带删除依赖的增量） |

---

## 四、gprestore 恢复

### 4.1 全量恢复

```bash
# 恢复到新数据库
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --create-db

# 恢复到已有数据库（先清空目标表）
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --truncate-table

# 恢复并在恢复后执行 ANALYZE
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --create-db --run-analyze
```

### 4.2 增量恢复

增量恢复时指定增量备份的时间戳，gprestore 会自动从 `config.yaml` 的 `restoreplan` 中确定需要从哪些备份集恢复哪些表：

```bash
# 恢复到增量备份时间点
gprestore --timestamp 20260422150000 --backup-dir /data/backup \
    --redirect-db restore_db --create-db
```

> **注意**: 不需要指定 `--incremental` 参数，gprestore 会自动识别。

### 4.3 指定表恢复

```bash
# 仅恢复指定表
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db \
    --include-table public.orders --include-table public.customers

# 仅恢复指定 schema
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --include-schema sales

# 恢复时遇错继续
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db restore_db --on-error-continue
```

### 4.4 恢复参数一览

| 参数 | 说明 |
|------|------|
| `--timestamp` | **必填** 要恢复的备份时间戳 |
| `--backup-dir` | 备份文件所在目录 |
| `--redirect-db` | 恢复到指定数据库（而非原库） |
| `--redirect-schema` | 恢复到指定 schema |
| `--create-db` | 恢复前自动创建数据库 |
| `--truncate-table` | 恢复前清空目标表 |
| `--include-table` | 仅恢复指定表（可多次指定） |
| `--include-schema` | 仅恢复指定 schema |
| `--exclude-table` | 排除指定表 |
| `--exclude-schema` | 排除指定 schema |
| `--data-only` | 仅恢复数据 |
| `--metadata-only` | 仅恢复元数据 |
| `--on-error-continue` | 遇错继续恢复 |
| `--jobs N` | 并行恢复连接数 |
| `--with-globals` | 同时恢复全局对象（角色、表空间等） |
| `--with-stats` | 同时恢复统计信息 |
| `--run-analyze` | 恢复后执行 ANALYZE |
| `--list-backups` | 列出备份列表并退出 |
| `--delete-backup TS` | 删除指定备份集 |

---

## 五、备份集管理

### 5.1 查看备份列表

```bash
# 通过 gpbackup 查看
gpbackup --list-backups --backup-dir /data/backup

# 通过 gprestore 查看（功能相同）
gprestore --list-backups --backup-dir /data/backup
```

**输出示例**:

```
Timestamp         Start Time            Type    Status     Database      Deleted At            Depends On
-------------------------------------------------------------------------------------------------------------------
20260422120000    2026-04-22 12:00:00   Full    Success    mydb
20260422130000    2026-04-22 13:00:00   Incr    Success    mydb                                20260422120000
20260422140000    2026-04-22 14:00:00   Incr    Success    mydb                                20260422120000
20260422150000    2026-04-22 15:00:00   Full    Success    mydb
20260422160000    2026-04-22 16:00:00   Incr    Success    mydb                                20260422150000
```

**字段说明**:

| 字段 | 说明 |
|------|------|
| Timestamp | 备份时间戳（YYYYMMDDHHMMSS） |
| Start Time | 备份开始时间 |
| Type | Full=全量, Incr=增量 |
| Status | Success=成功, Failure=失败 |
| Database | 数据库名 |
| Deleted At | 删除时间（空=未删除） |
| Depends On | 依赖的全量备份时间戳（增量备份） |

> **说明**: 列表按 `--backup-dir` 过滤，只显示该目录下的备份记录。

### 5.2 删除备份集

```bash
# 删除指定全量备份（同时删除所有依赖的增量备份 + 物理文件）
gpbackup --delete-backup 20260422120000 --backup-dir /data/backup

# 删除指定增量备份
gpbackup --delete-backup 20260422130000 --backup-dir /data/backup
```

**删除行为**:

- **删除全量备份**: 自动查找并删除所有依赖该全量的增量备份
- **删除增量备份**: 仅删除该条增量
- **物理文件清理**: 自动通过 SSH 删除 coordinator 和所有 segment 主机上的备份文件
- **历史记录**: 从 `gpbackup_history.db` 中彻底移除（硬删除）

**输出示例**:

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

## 六、通过外部表查询备份数据

`gpbackup_ext_query.sh` 可以在不执行完整恢复的情况下，通过外部表直接查询备份集中的数据。

**工作原理**:
1. 在每台 segment 主机上启动 gpfdist 服务，指向备份目录
2. 在目标数据库中创建与原表结构相同的外部表（EXTERNAL TABLE）
3. 外部表通过 gpfdist 读取各 segment 上的备份数据文件（gzip 压缩的 CSV）
4. 用户通过标准 SQL 查询外部表，即可读取备份数据

### 6.1 创建外部表

**Schema 命名规则**：
- **指定 `--ext-schema`**：所有外部表创建在指定的 schema 下（如 `ext_bak.t_heap`）
- **不指定 `--ext-schema`**（默认）：按 `<原schema>_<时间戳>` 自动命名（如 `public_20260422120000.t_heap`），不同 schema 的表分别放在对应的 schema 下

```bash
# 不指定 --ext-schema: 自动使用 <原schema>_<timestamp> 命名
# 例如 public 下的表会创建在 public_20260422120000 schema 中
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb

# 指定 --ext-schema: 所有表放在同一个 schema 下
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak

# 仅为指定表创建外部表
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak \
    --include-table public.orders \
    --include-table public.customers

# 指定 gpfdist 端口
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak \
    --gpfdist-port 19000
```

**输出示例**:

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

  查询示例:
    psql -d mydb -c "SELECT count(*) FROM ext_bak.<table_name>;"

  清理 (停止 gpfdist + 删除外部表):
    gpbackup_ext_query.sh --timestamp 20260422120000 --backup-dir /data/backup --dbname mydb --ext-schema ext_bak --stop
============================================
```

### 6.2 查询备份数据

外部表创建后，可以使用标准 SQL 查询备份数据：

```sql
-- 查看行数
SELECT count(*) FROM ext_bak.orders;

-- 条件查询
SELECT * FROM ext_bak.orders
WHERE order_date >= '2024-01-01' AND total_amount > 1000
ORDER BY order_date DESC
LIMIT 100;

-- 聚合查询
SELECT date_trunc('month', order_date) AS month, sum(total_amount)
FROM ext_bak.orders
GROUP BY 1 ORDER BY 1;

-- 与当前表对比
SELECT 'backup' AS source, count(*) FROM ext_bak.orders
UNION ALL
SELECT 'current', count(*) FROM public.orders;

-- 查找备份中存在但当前表中缺失的记录
SELECT b.order_id, b.order_date
FROM ext_bak.orders b
LEFT JOIN public.orders c ON b.order_id = c.order_id
WHERE c.order_id IS NULL;
```

### 6.3 清理外部表

```bash
# 清理所有外部表 + 停止 gpfdist + 删除 schema
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak --stop

# 仅清理指定的外部表（schema 下其他表保留）
gpbackup_ext_query.sh \
    --timestamp 20260422120000 \
    --backup-dir /data/backup \
    --dbname mydb \
    --ext-schema ext_bak \
    --include-table public.orders --stop
```

**清理行为**:
- 逐个删除外部表（`DROP EXTERNAL TABLE`）
- 尝试删除 schema（不使用 CASCADE，如果 schema 下还有其他对象则保留）
- 通过 SSH 停止所有节点上的 gpfdist 进程

### 6.4 跨集群查询备份数据

默认模式下，`gpbackup_ext_query.sh` 需要连接源数据库获取表结构和 segment 信息。通过 `--gen-metadata` 和 `--use-metadata` 参数，可以将这些信息保存到文件中，实现在**任意集群**上查询备份数据。

#### 步骤 1：生成元数据文件

有两种方式生成元数据文件：

**方式 A：备份时自动生成（推荐）**

在 gpbackup 命令中加 `--gen-ext-metadata`，备份完成后自动生成：

```bash
gpbackup --dbname mydb --backup-dir /data/backup --leaf-partition-data --gen-ext-metadata
```

**方式 B：备份后手动生成**

如果备份时未加 `--gen-ext-metadata`，也可以事后用脚本生成：

```bash
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname mydb --gen-metadata
```

会在备份目录中生成两个文件：

| 文件 | 说明 |
|------|------|
| `gpbackup_<TS>_ext_metadata.yaml` | segment 信息 + 表列定义（只读） |
| `gpbackup_<TS>_ext_config.yaml` | 可编辑配置文件（主机映射 + 目录映射） |

#### 步骤 2：拷贝备份文件到目标服务器

将整个备份目录（包括所有 `gpseg*` 子目录和上述两个文件）拷贝到目标服务器。

#### 步骤 3：编辑配置文件

编辑 `gpbackup_<TS>_ext_config.yaml`，修改主机映射和备份目录路径：

```yaml
# 修改前（源集群）
backup_dir: "/data/backup"
gpfdist_port: 0
host_map:
  - original: "sdw1"
    target: "sdw1"          # ← 改为目标服务器主机名
  - original: "sdw2"
    target: "sdw2"          # ← 改为目标服务器主机名

# 修改后（目标集群）
backup_dir: "/data1/restore/backup"     # ← 新路径
gpfdist_port: 19000                      # ← 可指定端口
host_map:
  - original: "sdw1"
    target: "new-host-1"               # ← 目标主机名
  - original: "sdw2"
    target: "new-host-2"               # ← 目标主机名
```

#### 步骤 4：在目标集群上创建外部表

```bash
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data1/restore/backup \
    --dbname target_db \
    --ext-schema ext_bak \
    --use-metadata
```

`--use-metadata` 模式从元数据文件读取表列定义和 segment 信息，从配置文件读取主机映射，**不依赖源数据库**。

#### 步骤 5：查询和清理

```bash
# 查询
psql -d target_db -c "SELECT count(*) FROM ext_bak.orders;"

# 清理
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data1/restore/backup \
    --dbname target_db --ext-schema ext_bak --stop
```

### 6.5 参数一览

| 参数 | 必填 | 说明 |
|------|------|------|
| `--timestamp <TS>` | 是 | 备份时间戳 |
| `--backup-dir <DIR>` | 是 | 备份目录 |
| `--dbname <DB>` | 是 | 创建外部表的目标数据库 |
| `--ext-schema <NAME>` | 否 | 外部表的 schema 名称。不指定时自动使用 `<原schema>_<时间戳>`（如 `public_20260422120000`） |
| `--include-table <S.T>` | 否 | 仅处理指定表（可多次指定） |
| `--include-schema <S>` | 否 | 仅处理指定 schema 中的表（可多次指定） |
| `--gpfdist-port <PORT>` | 否 | gpfdist 端口（默认: 随机 18000-19000） |
| `--gen-metadata` | 否 | 生成元数据文件（segment 信息 + 表列定义），需连接源数据库 |
| `--use-metadata` | 否 | 从元数据文件读取表结构，不依赖源数据库，可在任意集群上使用 |
| `--stop` | 否 | 停止 gpfdist 并删除外部表 |
| `--help` | 否 | 显示帮助信息 |

---

## 七、增量备份检测机制说明

### 三种模式对比

| 模式 | 参数 | Heap 表检测 | AO 表检测 | 适用场景 |
|------|------|------------|----------|---------|
| **默认模式** | `--incremental` | 始终备份（原版行为） | modcount + DDL 时间戳 | 通用场景 |
| **Heap 增量模式** | `--incremental --heap-file-hash` | 文件哈希 (mtime+size) | modcount + DDL 时间戳 | 跳过未修改的 Heap 表 |
| **AO 精确模式** | `--incremental --ao-file-hash` | 始终备份（原版行为） | aoseg 内容哈希 (eof+tupcount) | GP5 分区表场景 |
| **完整精确模式** | `--incremental --heap-file-hash --ao-file-hash` | 文件哈希 (mtime+size) | aoseg 内容哈希 (eof+tupcount) | 最大化减少备份量 |

### 检测原理

**Heap 表 — 文件哈希检测**:
```
备份前执行 CHECKPOINT → 刷脏页到磁盘
↓
在每个 segment 上通过 pg_stat_file() 获取数据文件的:
  - modification (修改时间)
  - size (文件大小)
↓
将所有 segment 的结果聚合为 MD5 哈希
↓
与上次备份的哈希比较 → 不同则备份，相同则跳过
```

**AO 表 — modcount 检测（默认）**:
```
查询 pg_aoseg.pg_aoseg_<oid> 获取 sum(modcount)
查询 pg_stat_last_operation 获取最后 DDL 时间
↓
与上次备份的值比较 → 任一不同则备份
```

**AO 表 — aoseg 内容哈希检测（--ao-file-hash）**:
```
查询 pg_aoseg.pg_aoseg_<oid> 获取每行的 (segno, eof, tupcount)
  注意: 故意排除 modcount（GP5 中会跨分区传播）
↓
计算 MD5 哈希
↓
与上次备份的哈希比较 → 不同则备份
```

### TOC 文件中的元数据

增量元数据存储在 `gpbackup_<timestamp>_toc.yaml` 的 `incrementalmetadata` 部分：

```yaml
incrementalmetadata:
  ao:
    public.t_ao:
      modcount: 5
      lastddltimestamp: "2026-04-22T10:00:00+08:00"
      fileHashMD5: a1b2c3d4e5f6...           # --ao-file-hash 时才有
    public.t_part_1_prt_p202401:
      modcount: 3
      lastddltimestamp: "2026-04-22T10:00:00+08:00"
      fileHashMD5: f6e5d4c3b2a1...
  heap:
    public.t_heap:
      fileHashMD5: 1a2b3c4d5e6f...           # Heap 表文件哈希
```

---

## 八、最佳实践

### 8.1 备份策略建议

```bash
# 每周日: 全量备份 (建立 Heap 和 AO 哈希基线)
0 2 * * 0  gpbackup --dbname prod --backup-dir /data/backup \
    --leaf-partition-data --heap-file-hash --ao-file-hash --jobs 4

# 每天凌晨: 增量备份 (Heap + AO 都精确检测)
0 2 * * 1-6  gpbackup --dbname prod --backup-dir /data/backup \
    --leaf-partition-data --incremental --heap-file-hash --ao-file-hash --jobs 4
```

### 8.2 定期清理

```bash
# 查看备份列表
gpbackup --list-backups --backup-dir /data/backup

# 删除 2 周前的全量备份及其增量（物理文件也一并清理）
gpbackup --delete-backup 20260408020000 --backup-dir /data/backup
```

### 8.3 数据验证（无需恢复）

```bash
# 通过外部表快速验证备份数据
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname prod \
    --ext-schema ext_verify --include-table public.orders

psql -d prod -c "
    SELECT
        (SELECT count(*) FROM public.orders) AS current_count,
        (SELECT count(*) FROM ext_verify.orders) AS backup_count;
"

# 验证完成后清理
gpbackup_ext_query.sh --timestamp 20260422120000 \
    --backup-dir /data/backup --dbname prod \
    --ext-schema ext_verify --stop
```

### 8.4 紧急数据恢复（单表）

```bash
# 场景: 误删了 orders 表的部分数据，需要从备份中恢复

# 方法 1: 通过外部表直接查询并插入
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

# 方法 2: 通过 gprestore 恢复单表到临时库
gprestore --timestamp 20260422120000 --backup-dir /data/backup \
    --redirect-db temp_restore --include-table public.orders --create-db
```

---

## 九、常见问题

### Q1: 增量备份时所有表都被备份了

**原因**: 首次使用 `--heap-file-hash` 或 `--ao-file-hash` 增量时，上一次全量备份没有对应的哈希基线。

**解决**: 确保全量备份时使用相同的参数建立基线：
- 使用 `--heap-file-hash` 增量时，全量也要加 `--heap-file-hash`
- 使用 `--ao-file-hash` 增量时，全量也要加 `--ao-file-hash`
- 两个参数独立使用，可单独或同时指定

### Q2: GP5 中分区表增量备份了所有子分区

**原因**: GP5 的 modcount 会在子分区间传播。

**解决**: 使用 `--ao-file-hash` 参数（独立使用即可，不需要 `--heap-file-hash`）。

### Q3: Heap 表小量 INSERT 后增量未检测到变化

**原因**: 数据在内存中尚未刷到磁盘，`pg_stat_file` 看到的文件未变。

**解决**: 增强版在采集文件哈希前会自动执行 `CHECKPOINT`，确保数据刷盘。如仍有问题，手动执行 `CHECKPOINT` 后再备份。

### Q4: 外部表查询报错 `connection refused`

**原因**: gpfdist 服务未启动或端口被占用。

**解决**:
```bash
# 检查 gpfdist 是否运行
ps aux | grep gpfdist

# 如果端口冲突，指定其他端口
gpbackup_ext_query.sh ... --gpfdist-port 19500
```

### Q5: `--delete-backup` 时 segment 文件未删除

**原因**: 数据库未运行（无法查询 segment 主机信息）或 SSH 免密未配置。

**解决**: 确保数据库正常运行，gpadmin 用户能免密 SSH 到所有 segment 主机。

### Q6: `--list-backups` 没有显示任何记录

**原因**: `gpbackup_history.db` 路径不正确。

**解决**: 脚本依次搜索以下路径:
1. `<backup-dir>/gpbackup_history.db`
2. `<backup-dir>/gpseg-1/gpbackup_history.db`
3. `$MASTER_DATA_DIRECTORY/gpbackup_history.db`
4. `$COORDINATOR_DATA_DIRECTORY/gpbackup_history.db`

确保上述路径之一存在 history 数据库文件。
