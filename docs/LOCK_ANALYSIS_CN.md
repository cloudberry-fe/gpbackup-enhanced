# 备份恢复过程中的锁影响分析

本文档分析 gpbackup 备份和 gprestore 恢复过程中对数据库锁的使用，以及对在线业务的影响。

---

## 一、gpbackup 备份过程锁分析

### 1.1 锁类型

gpbackup 备份过程中**仅使用 ACCESS SHARE 锁**——PostgreSQL/Greenplum 中最弱的锁级别。

```sql
-- 源码: backup/queries_relations.go:500
LOCK TABLE schema.table IN ACCESS SHARE MODE
```

### 1.2 加锁流程

```
gpbackup 启动
│
├─ 阶段 1：Worker 0 批量加锁（主连接）
│   对所有备份表执行 LOCK TABLE ... IN ACCESS SHARE MODE
│   每批 100 张表，逐批加锁
│   这些锁持续到整个备份事务 COMMIT 后才释放
│   ⚠ 如果表上有 ACCESS EXCLUSIVE 锁（如 ALTER TABLE），此处会阻塞等待
│
├─ 阶段 2：Worker 1~N 逐表加锁（并行连接）
│   对每张表执行 LOCK TABLE ... IN ACCESS SHARE MODE NOWAIT
│   NOWAIT 模式：获取不到锁立即返回错误，不排队等待
│   失败的表推迟给 Worker 0 处理（Worker 0 已持有所有锁，不会死锁）
│
├─ 阶段 3：COPY 数据导出
│   各 Worker 并行执行 COPY table TO PROGRAM ...
│   COPY 隐式获取 ACCESS SHARE 锁（与已有锁兼容）
│
└─ 阶段 4：事务 COMMIT
   释放所有 ACCESS SHARE 锁
```

### 1.3 GP 版本差异

| GP 版本 | 锁模式 | 说明 |
|--------|--------|------|
| GP5 / GP6 (<6.21) | `IN ACCESS SHARE MODE NOWAIT` | 锁传播到所有 segment |
| GP6 (≥6.21) | `IN ACCESS SHARE MODE NOWAIT MASTER ONLY` | 锁仅在 master 获取，不传播到 segment |
| GP7+ / Cloudberry | `IN ACCESS SHARE MODE NOWAIT COORDINATOR ONLY` | 同上，关键字改为 COORDINATOR |

**GP6.21+ 的 `MASTER ONLY` 优化**：大幅减少了 segment 上的锁开销，是性能改进的重要优化。

### 1.4 对在线业务的影响

#### ACCESS SHARE 锁兼容矩阵

| 业务操作 | 所需锁 | 与 ACCESS SHARE 兼容？ | 是否被 gpbackup 阻塞？ |
|---------|-------|---------------------|---------------------|
| `SELECT` | ACCESS SHARE | ✅ 兼容 | **不阻塞** |
| `INSERT` | ROW EXCLUSIVE | ✅ 兼容 | **不阻塞** |
| `UPDATE` | ROW EXCLUSIVE | ✅ 兼容 | **不阻塞** |
| `DELETE` | ROW EXCLUSIVE | ✅ 兼容 | **不阻塞** |
| `CREATE INDEX` | SHARE | ✅ 兼容 | **不阻塞** |
| `VACUUM` | SHARE UPDATE EXCLUSIVE | ✅ 兼容 | **不阻塞** |
| `ANALYZE` | SHARE UPDATE EXCLUSIVE | ✅ 兼容 | **不阻塞** |
| `ALTER TABLE` | ACCESS EXCLUSIVE | ❌ 冲突 | **会阻塞** |
| `DROP TABLE` | ACCESS EXCLUSIVE | ❌ 冲突 | **会阻塞** |
| `TRUNCATE` | ACCESS EXCLUSIVE | ❌ 冲突 | **会阻塞** |
| `VACUUM FULL` | ACCESS EXCLUSIVE | ❌ 冲突 | **会阻塞** |
| `REINDEX` | SHARE | ✅ 兼容 | **不阻塞** |

#### 结论

> **正常的 DML 操作（SELECT/INSERT/UPDATE/DELETE）完全不受影响。**
> **只有 DDL 操作（ALTER/DROP/TRUNCATE/VACUUM FULL）会被阻塞，直到备份完成。**

### 1.5 防死锁机制

gpbackup 内置了防死锁设计：

**问题场景**：
```
时间线:
T1: gpbackup Worker 0 对表 A 加 ACCESS SHARE 锁（成功）
T2: 用户执行 ALTER TABLE A ... 请求 ACCESS EXCLUSIVE 锁（排队等待 Worker 0 释放）
T3: gpbackup Worker 1 对表 A 加 ACCESS SHARE NOWAIT（失败——被 ALTER 的排队请求阻塞）
```

**解决**：Worker 1~N 使用 `NOWAIT` 模式，获取失败不阻塞等待，而是将该表推迟给 Worker 0。Worker 0 已持有所有表的锁，不会遇到死锁。

```go
// 源码: backup/data.go:372
func LockTableNoWait(dataTable Table, connNum int) error {
    lockMode = `IN ACCESS SHARE MODE NOWAIT`
    query := fmt.Sprintf("LOCK TABLE %s %s;", dataTable.FQN(), lockMode)
    _, err := connectionPool.Exec(query, connNum)
    // 失败时返回错误，调用方将表推迟给 Worker 0
}
```

### 1.6 锁持有时间

| 阶段 | 锁持有情况 | 持续时间 |
|------|---------|--------|
| 元数据备份 | Worker 0 持有所有表的 ACCESS SHARE 锁 | 秒级 |
| 数据备份 | 持续持有 | 取决于数据量（分钟到小时） |
| 事务提交 | 释放所有锁 | 瞬间 |
| **总锁持有时间** | **等于整个备份过程时间** | 分钟到小时 |

---

## 二、gprestore 恢复过程锁分析

### 2.1 恢复阶段与锁

gprestore 通常恢复到**新数据库或空库**，此时不存在锁冲突。以下分析适用于恢复到**已有数据的库**（如 `--truncate-table` 或 `--incremental`）。

#### 阶段 1：元数据恢复（DDL）

```sql
-- 执行 CREATE TABLE, CREATE INDEX 等 DDL
-- 如果目标表已存在，不会冲突（CREATE IF NOT EXISTS 或跳过）
-- 如果使用 --truncate-table，会先 TRUNCATE
```

| 操作 | 锁 | 影响 |
|------|---|------|
| `CREATE TABLE` | ACCESS EXCLUSIVE（新表） | 新表无冲突 |
| `CREATE INDEX` | SHARE（目标表） | 阻塞目标表的写入 |

#### 阶段 2：数据恢复（COPY FROM）

```sql
-- 源码: restore/data.go:51
COPY tablename FROM PROGRAM '...' WITH CSV DELIMITER ',' ON SEGMENT;
```

`COPY FROM` 隐式获取 **ROW EXCLUSIVE** 锁：

| 操作 | 与 COPY FROM 的 ROW EXCLUSIVE 兼容？ | 说明 |
|------|-----------------------------------|------|
| `SELECT` | ✅ 兼容 | 可以查询正在恢复的表 |
| `INSERT` | ✅ 兼容 | 可以同时写入 |
| `UPDATE/DELETE` | ✅ 兼容 | 可以同时修改 |
| `ALTER TABLE` | ❌ 冲突 | 被阻塞 |
| `TRUNCATE` | ❌ 冲突 | 被阻塞 |

#### 阶段 2.5：TRUNCATE（仅在 `--truncate-table` 或 `--incremental` 模式）

```go
// 源码: restore/data.go:286-288
if MustGetFlagBool(options.INCREMENTAL) || MustGetFlagBool(options.TRUNCATE_TABLE) {
    connectionPool.Exec(`TRUNCATE ` + tableName, whichConn)
}
```

`TRUNCATE` 获取 **ACCESS EXCLUSIVE** 锁：

| 操作 | 影响 |
|------|------|
| 所有对该表的操作 | **全部阻塞**，直到 TRUNCATE 完成 |

> ⚠ `--truncate-table` 和 `--incremental` 恢复时，每张表在数据加载前会被 TRUNCATE，此时该表**短暂完全不可访问**。

#### 阶段 3：后置元数据（Post-data DDL）

```sql
-- CREATE INDEX, ADD CONSTRAINT, CREATE TRIGGER 等
```

`CREATE INDEX` 获取 **SHARE** 锁，阻塞目标表的写入：

| 操作 | 与 SHARE 锁兼容？ | 说明 |
|------|----------------|------|
| `SELECT` | ✅ 兼容 | 可以查询 |
| `INSERT/UPDATE/DELETE` | ❌ 冲突 | 被阻塞，直到索引创建完成 |

### 2.2 恢复场景锁影响总结

| 恢复场景 | 对目标库的锁影响 |
|--------|-------------|
| 恢复到新数据库（`--create-db`） | **无影响**——新库无业务连接 |
| 恢复到空库（`--redirect-db`） | **无影响**——空表无冲突 |
| `--truncate-table` 恢复到已有库 | **短暂阻塞**——TRUNCATE 时表不可访问 |
| `--incremental` 增量恢复 | **短暂阻塞**——每张表 TRUNCATE + COPY |
| `--include-table` 指定表恢复 | **仅影响指定的表** |
| `--data-only` 数据恢复 | **影响被恢复的表**（COPY FROM 的 ROW EXCLUSIVE） |
| `--metadata-only` 元数据恢复 | **影响索引创建**（SHARE 锁阻塞写入） |

---

## 三、增量备份增强功能的锁分析

### 3.1 `--heap-file-hash` 的额外操作

| 操作 | 锁影响 |
|------|-------|
| CHECKPOINT | **不加数据库锁**，但短暂增加磁盘 I/O |
| `pg_stat_file()` | **不加锁**，只读文件系统 inode |
| 独立连接查询 | **不影响主备份事务** |

### 3.2 `--ao-file-hash` 的额外操作

| 操作 | 锁影响 |
|------|-------|
| 查询 `pg_aoseg` catalog 表 | **ACCESS SHARE**（对 catalog 表，不影响业务表） |
| 独立连接 | **不影响主备份事务** |

### 3.3 `--list-backups` / `--delete-backup`

这两个管理命令**不连接数据库**（只读写 SQLite history 文件），**对业务完全无影响**。

---

## 四、最佳实践

### 4.1 备份期间的操作建议

| 建议 | 说明 |
|------|------|
| ✅ 备份期间正常执行 DML | SELECT/INSERT/UPDATE/DELETE 不受影响 |
| ✅ 备份期间可执行 VACUUM | 普通 VACUUM 与 ACCESS SHARE 兼容 |
| ✅ 备份期间可执行 ANALYZE | 与 ACCESS SHARE 兼容 |
| ⚠ 避免备份期间执行 DDL | ALTER TABLE/DROP/TRUNCATE 会被阻塞 |
| ⚠ 避免备份期间执行 VACUUM FULL | 需要 ACCESS EXCLUSIVE 锁，会被阻塞 |
| ❌ 不要在备份期间做大规模 DDL 变更 | 可能导致长时间锁等待 |

### 4.2 恢复期间的操作建议

| 建议 | 说明 |
|------|------|
| ✅ 优先恢复到新数据库 | 避免所有锁冲突 |
| ✅ 使用 `--include-table` 最小化影响 | 只锁定需要恢复的表 |
| ⚠ `--truncate-table` 恢复时通知业务方 | TRUNCATE 会短暂中断该表访问 |
| ⚠ 索引创建期间避免写入被恢复的表 | CREATE INDEX 的 SHARE 锁阻塞写入 |

### 4.3 监控锁等待

```sql
-- 查看当前锁等待情况
SELECT l.pid, l.mode, l.granted, a.current_query, a.waiting
FROM pg_locks l
JOIN pg_stat_activity a ON l.pid = a.procpid
WHERE NOT l.granted
ORDER BY a.query_start;

-- 查看 gpbackup 持有的锁
SELECT l.relation::regclass, l.mode, l.granted
FROM pg_locks l
JOIN pg_stat_activity a ON l.pid = a.procpid
WHERE a.application_name LIKE 'gpbackup%';
```
