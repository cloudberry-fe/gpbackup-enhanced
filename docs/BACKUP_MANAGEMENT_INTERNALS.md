# `--list-backups` / `--delete-backup` 原理与实现说明

适用版本：gpbackup 增强版 `1.30.5-custom`（含 `backup/manage.go`、`restore/manage.go`、`history/history.go`）。

两条管理命令都不走完整的 gpbackup/gprestore 流水线，而是在 `DoInit` 之后由 `HandleManageCommands()`（`backup/manage.go:18` 与 `restore/manage.go` 两份等价实现）拦截：解析完 flag、找到 history DB 后直接退出，不会再去打开数据库连接或进入备份/恢复主循环。

---

## 1. 基础数据模型：`gpbackup_history.db`

文件位置由 `findHistoryDB()`（`backup/manage.go:287`）按顺序探测：

```
<backup-dir>/gpbackup_history.db
<backup-dir>/gpseg-1/gpbackup_history.db
$MASTER_DATA_DIRECTORY/gpbackup_history.db
$COORDINATOR_DATA_DIRECTORY/gpbackup_history.db
```

任何一个命中即使用。该文件是 **SQLite**，schema 见 `history.InitializeHistoryDatabase()`（`history/history.go:79`）：

| 表 | 关键列 | 角色 |
|---|---|---|
| `backups` | `timestamp` (PK), `backup_dir`, `incremental`, `database_name`, `status`, `date_deleted`, … | 每个备份一条记录 |
| `restore_plans` | `(timestamp, restore_plan_timestamp)` (FK→backups) | 当前备份依赖哪些祖先备份 |
| `restore_plan_tables` | `(timestamp, restore_plan_timestamp, table_fqn)` | 哪些表来自哪个祖先 |
| `include_relations` / `include_schemas` / `exclude_relations` / `exclude_schemas` | timestamp + name | 备份时的过滤器 |

**依赖关系全部走 `restore_plans` 表**：增量备份 `T_n` 写入若干行 `(T_n, T_0), (T_n, T_1), …`，表示它还原时需要哪些祖先；全量 `T_0` 自己也会写一行 `(T_0, T_0)`。

> 重要：`gpbackup_history.db` 是 **全 coordinator 单文件**，跨多个 `--backup-dir` 共享。靠 `backup_dir` 列区分不同备份目录。

---

## 2. `--list-backups`

### 2.1 入口

`HandleManageCommands()`（`backup/manage.go:18`）：

```go
backupDir := MustGetFlagString(options.BACKUP_DIR)
if backupDir == "" {
    fmt.Fprintln(os.Stderr, "ERROR: --backup-dir is required for --list-backups and --delete-backup")
    os.Exit(1)
}
historyDBPath := findHistoryDB(backupDir)
historyDB, _ := history.InitializeHistoryDatabase(historyDBPath)
backups, _   := history.ListBackups(historyDB, backupDir)
printBackupList(backups)
```

`--backup-dir` **强制必填** —— 否则会把所有 backup-dir 下的备份混在一起返回，对操作没有意义。

### 2.2 数据查询：`history.ListBackups`（`history/history.go:440`）

```sql
SELECT timestamp FROM backups
 WHERE backup_dir = '<backupDir>'
 ORDER BY timestamp DESC;
```

对每个 `timestamp`：

1. `GetMainBackupInfo(ts)` 取 `backups` 表的基本字段。
2. `GetBackupConfig(ts)` 把它的 `restore_plans` 也读回来，挂到 `cfg.RestorePlan` 字段。

### 2.3 输出渲染：`printBackupList`（`backup/manage.go:72`）

- 用 `incrBase[ts] = b.RestorePlan[0].Timestamp` 把每个增量直接基底标出来。
- 按 timestamp 升序展示。
- `fmtTS()` 把 `20260515101258` 渲染成 `2026-05-15 10:12:58`。
- `Type`：根据 `Incremental` 字段决定 Full / Incr。
- `Deleted At`：来自 `date_deleted` 列；当前实现 hard delete 不会写它，保留是为了兼容 soft-delete 历史数据。

### 2.4 已知限制

| 限制 | 影响 |
|---|---|
| 只筛 `backup_dir`，**不读磁盘** | 目录已被手工删除但 history 里仍存在的备份依旧会被列出 |
| 不区分 `status` 详情 | 字段直接展示，只看到 `Success` / 其他字符串 |
| 不显示备份大小 | 没有"backup size"列，需要看磁盘 |

---

## 3. `--delete-backup`

分两层：**逻辑删除（history.db 内）** 和 **物理删除（磁盘文件）**。

### 3.1 依赖链收集：`history.DeleteBackup`（`history/history.go:503`）

```go
toDelete := []string{timestamp}

if !cfg.Incremental {                       // 是全量
    deps, _ := FindDependentBackups(historyDB, timestamp)
    allDeps := map[string]bool{}
    for _, d := range deps {
        allDeps[d] = true
    }
    for _, d := range deps {                // 再展开一层
        transitive, _ := FindDependentBackups(historyDB, d)
        for _, t := range transitive {
            allDeps[t] = true
        }
    }
    for dep := range allDeps {
        toDelete = append(toDelete, dep)
    }
}
```

`FindDependentBackups` 是 `restore_plans` 上的子查询：

```sql
SELECT DISTINCT timestamp FROM restore_plans
 WHERE restore_plan_timestamp = '<base>'
   AND timestamp != '<base>'
 ORDER BY timestamp;
```

#### 行为要点

- **删 leaf 增量不会级联** —— `cfg.Incremental == true` 分支直接跳过 deps 收集，`toDelete` 只含自身。这是 Case 5 看到 "Deleted 1 backup(s)" 的来源。
- **删全量会带走它的整条链** —— 所有以它为 base 的增量都被收进 `toDelete`。
- **transitive 只展开 1 层**（共 2 层）。gpbackup 的常见用法是"一个 Full + 多个挂在它身上的增量"，2 层足够；如果有人构造了"增量→增量→增量"的深链，需要继续展开。

### 3.2 逻辑删除：`hardDeleteTimestamp`（`history.go:543`）

对每个 ts，按外键依赖顺序硬删：

```sql
DELETE FROM restore_plan_tables   WHERE timestamp = '<ts>';
DELETE FROM restore_plans         WHERE timestamp = '<ts>';
DELETE FROM exclude_relations     WHERE timestamp = '<ts>';
DELETE FROM exclude_schemas       WHERE timestamp = '<ts>';
DELETE FROM include_relations     WHERE timestamp = '<ts>';
DELETE FROM include_schemas       WHERE timestamp = '<ts>';
DELETE FROM backups               WHERE timestamp = '<ts>';   -- 最后
```

**注意**：不是 `UPDATE backups SET date_deleted = NOW()`（soft delete），而是 `DELETE`（hard delete）。删完后 `--list-backups` 立即看不到。

### 3.3 物理删除：`deleteBackupFiles`（`backup/manage.go:140`）

目录布局：

```
<backup-dir>/gpseg-1/backups/YYYYMMDD/TIMESTAMP/         ← coordinator
<backup-dir>/gpseg<N>/backups/YYYYMMDD/TIMESTAMP/        ← 每个 primary segment
```

#### 步骤

1. **coordinator 本地**：直接 `os.RemoveAll(<backup-dir>/gpseg-1/backups/YYYYMMDD/TIMESTAMP)`；删完再 `os.Remove` 空的日期目录（不递归，目录非空就 silent 失败）。
2. **查 segment 主机**（`getSegmentHosts`，`manage.go:244`）：

   ```sql
   SELECT DISTINCT hostname FROM gp_segment_configuration WHERE content >= 0
   ```

   通过 fork `psql -d <PGDATABASE|postgres|template1>` 跑（不走 gpbackup 自身连接池）。`content >= 0` 表示拿所有 segment（不含 coordinator），`DISTINCT` 把 primary + mirror 共址主机折叠成一份。

3. **每个 segment 主机一次 ssh**：

   ```bash
   ssh -o BatchMode=yes <host> "cd / && rm -rf <backup-dir>/gpseg*/backups/YYYYMMDD/TIMESTAMP …"
   ```

   - 用 shell glob `gpseg*` 一次匹配该主机上承载的所有 segment 数据目录（例如 dbhost03 同时跑 gpseg0+gpseg1）。
   - `cd /` 是防御性的，避免相对路径意外解析。
   - `BatchMode=yes` 杜绝交互——**这意味着 gpadmin 用户必须在所有 segment 主机间预配置好免密 SSH**。

4. **回收空日期目录**：再发一轮 `rmdir <backup-dir>/gpseg*/backups/YYYYMMDD 2>/dev/null; true`。`rmdir` 对非空目录是 no-op，所以只会清掉真正空了的 date 父目录。

#### 输出样例

```
Removing backup files...
  Coordinator: removed 1 backup directories
  Segments: cleaned backup files on 2 host(s): dbhost03, dbhost04
  File cleanup complete.
```

---

## 4. 约定与注意事项

| 项 | 说明 |
|---|---|
| `--backup-dir` 必填 | 命令只看一个 backup-dir 下的备份，不汇总。多 backup-dir 各管各的。 |
| History DB 单文件 | 默认在 `$MASTER_DATA_DIRECTORY/gpbackup_history.db`；跨 backup-dir 共享，按 `backup_dir` 列区分。 |
| 逻辑/物理删除非原子 | history.db 先 `DELETE`（成功），随后才发 ssh。ssh 失败只 `WARNING`，**不会回滚 history**——会留下孤儿目录。 |
| 依赖链展开 2 层 | 适用于"Full + 多个并列 Incr"的常见拓扑；构造"增量→增量"深链时需要继续展开。 |
| 删 Full 带走整条链 | 想保留 Full、只清某个增量，必须传该增量自己的 timestamp。 |
| 无 `--dry-run` | 删除立即生效。预先确认影响面建议先 `--list-backups`，或在 sqlite 里跑：<br>`SELECT timestamp FROM restore_plans WHERE restore_plan_timestamp='<full>' AND timestamp <> '<full>';` |
| `date_deleted` 字段保留 | 当前 hard delete 不写它，但显示在 list 输出里以兼容旧 soft-delete 数据。 |

---

## 5. 整体调用图

```
gpbackup/gprestore --list-backups | --delete-backup
        │
        ▼
DoInit() → HandleManageCommands()      [backup/manage.go:18 / restore/manage.go]
        │
        ├─ findHistoryDB(backup-dir)        [manage.go:287]
        ├─ history.InitializeHistoryDatabase
        │
        ├─ if --list-backups:
        │     history.ListBackups(db, backupDir)         [history.go:440]
        │     printBackupList(...)                       [manage.go:72]
        │
        └─ if --delete-backup:
              history.DeleteBackup(db, ts)               [history.go:503]
                  ├─ FindDependentBackups()              [history.go:474]
                  └─ hardDeleteTimestamp() x N           [history.go:543]
              printDeleteResult(...)                     [manage.go:122]
              deleteBackupFiles(backup-dir, deletedList) [manage.go:140]
                  ├─ os.RemoveAll(<gpseg-1>/...)         (coordinator)
                  ├─ getSegmentHosts() via psql          [manage.go:244]
                  └─ ssh <host> "rm -rf gpseg*/.../<ts>" (segments)
```

## 6. 相关源码索引

| 路径 | 说明 |
|---|---|
| `options/flag.go:59-61` | `LIST_BACKUPS`、`DELETE_BACKUP` flag 常量 |
| `options/flag.go:100-101, 138-139` | 注册到 backup/restore flag set |
| `backup/manage.go` | backup 侧 `HandleManageCommands` + 物理删除 |
| `restore/manage.go` | restore 侧等价实现（与 backup 几乎对称） |
| `history/history.go:440` | `ListBackups` |
| `history/history.go:474` | `FindDependentBackups` |
| `history/history.go:503` | `DeleteBackup`（逻辑删除入口） |
| `history/history.go:543` | `hardDeleteTimestamp`（按外键顺序硬删） |
