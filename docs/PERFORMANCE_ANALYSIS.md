# 增量备份哈希采集性能影响分析

本文档分析 `--heap-file-hash` 和 `--ao-file-hash` 两个参数在备份过程中的额外开销，帮助用户决策是否启用。

---

## 一、Heap 表哈希采集（`--heap-file-hash`）

### 1.1 操作链

```
1. CHECKPOINT                          ← 全局操作，强制刷脏页到磁盘
2. ensureFileStatFunction()            ← 创建 plpgsql 函数（仅首次）
3. getFileHashesForTables()            ← 逐表查询
   └─ 对每张 Heap 表执行：
      SELECT md5(string_agg(...))
      FROM (
        SELECT gp_segment_id,
               gp_toolkit.gpbackup_file_info('schema','table')
        FROM gp_dist_random('gp_id')       ← 每个 segment 并行执行
      ) x
```

### 1.2 gpbackup_file_info 函数内部（每个 segment 上执行）

```sql
-- 1. 查 pg_class 获取 relfilenode（catalog 查询，极快）
SELECT reltablespace, relfilenode FROM pg_class ...

-- 2. 查 pg_database 获取数据库 oid（catalog 查询，极快）
SELECT oid, dattablespace FROM pg_database ...

-- 3. 拼接文件路径：base/<dboid>/<relfilenode>

-- 4. pg_stat_file(path)
--    只读取文件系统 inode 元数据（modification time + size）
--    不读取文件内容，不产生磁盘 I/O
--    返回: modification_timestamp | file_size
```

### 1.3 性能开销明细

| 操作 | 开销 | 说明 |
|------|------|------|
| **CHECKPOINT** | **最大开销** | 强制将共享缓冲区中的脏页刷写到磁盘。数据量大、脏页多时可能耗时数秒到数十秒 |
| 创建 plpgsql 函数 | 一次性，<1s | 首次备份时创建 `gp_toolkit.gpbackup_file_info`，后续备份跳过 |
| 建立独立数据库连接 | <1s | 避免影响主备份事务 |
| `pg_stat_file()` 单表 | **极快，<1ms** | 只读 inode 元数据（stat 系统调用），不读文件内容 |
| `gp_dist_random` 分发 | 每张表一次 SQL | N 个 segment 并行执行，SQL 往返延迟约 5-30ms |
| MD5 聚合 | 极快，<1ms | 对几个短字符串做 MD5 |
| **单表总耗时** | **约 10-50ms** | 主要是 SQL 网络往返延迟 |

### 1.4 不同规模下的预估耗时

| Heap 表数量 | 哈希采集耗时 | 说明 |
|------------|-----------|------|
| 10 张 | <1 秒 | 几乎无感知 |
| 100 张 | 1-5 秒 | 可忽略 |
| 1,000 张 | 10-50 秒 | 影响较小 |
| 10,000 张 | 2-8 分钟 | 需评估是否值得 |
| 100,000 张 | 15-60 分钟 | 建议配合 `--include-schema` 过滤 |

> **注意**：以上不含 CHECKPOINT 耗时。CHECKPOINT 耗时取决于脏页数量，与表数量无关。

### 1.5 CHECKPOINT 影响

CHECKPOINT 是 Heap 哈希采集的前提——确保内存中的数据变更已刷写到磁盘，`pg_stat_file` 才能看到准确的文件修改时间和大小。

| 场景 | CHECKPOINT 耗时 | 说明 |
|------|---------------|------|
| 系统空闲，脏页少 | <1 秒 | 几乎无影响 |
| 正常业务，适量脏页 | 1-5 秒 | 可接受 |
| 业务高峰，大量脏页 | 5-30 秒 | 建议在低峰期执行备份 |
| 刚做完大批量写入 | 10-60 秒 | 建议等待自然 CHECKPOINT 后再备份 |

**优化建议**：如果备份窗口紧张，可提前手动执行 `CHECKPOINT`，备份时的自动 CHECKPOINT 就会很快完成。

---

## 二、AO 表哈希采集（`--ao-file-hash`）

### 2.1 操作链

```
GetAOContentHashes()
├─ getAOSegTableFQNs()         ← 一次 SQL 查询所有 AO 表对应的 aoseg 表名
│   SELECT pg_ao.relid, 'pg_aoseg.' || aoseg_c.relname
│   FROM pg_appendonly pg_ao JOIN pg_class ...
│
└─ 逐表执行 getAOSegContentHash()
    SELECT md5(string_agg(
      segno || ',' || eof || ',' || tupcount, chr(10) ORDER BY segno
    )) FROM pg_aoseg.pg_aoseg_<oid>
    -- GP5: 直接查 master 上的 catalog 表
    -- GP7+: 通过 gp_dist_random 查 segment
```

### 2.2 性能开销明细

| 操作 | 开销 | 说明 |
|------|------|------|
| 查 aoseg 表映射关系 | 一次 SQL，<100ms | `pg_appendonly` JOIN `pg_class`，结果缓存 |
| 查单张表的 aoseg 内容 | **极快，<5ms** | `pg_aoseg_<oid>` 表通常只有 1-10 行 |
| MD5 计算 | 极快，<1ms | 几行短文本的 MD5 |
| 建立独立数据库连接 | <1s | 避免主事务中断 |
| **无 CHECKPOINT** | **零额外 I/O** | 纯 catalog 查询，不涉及数据文件 |
| **单表总耗时** | **约 2-5ms** | 极快 |

### 2.3 不同规模下的预估耗时

| AO 表数量 | 哈希采集耗时 | 说明 |
|----------|-----------|------|
| 10 张 | <0.1 秒 | 无感知 |
| 100 张 | 0.2-0.5 秒 | 可忽略 |
| 1,000 张 | 2-5 秒 | 影响极小 |
| 10,000 张 | 20-50 秒 | 可接受 |
| 100,000 张 | 3-8 分钟 | 建议配合 `--include-schema` 过滤 |

---

## 三、对比总结

### 3.1 两种方式对比

| 维度 | Heap (`--heap-file-hash`) | AO (`--ao-file-hash`) | 不加参数（原版） |
|------|--------------------------|----------------------|------------|
| **额外磁盘 I/O** | CHECKPOINT 刷盘 | **无** | 无 |
| **查询方式** | `pg_stat_file`（文件系统 inode） | `pg_aoseg` catalog 表 | 无额外查询 |
| **执行位置** | 每个 segment 并行 | master 本地（GP5）/ segment（GP7+）| — |
| **每表耗时** | ~10-50ms | ~2-5ms | 0 |
| **100 表** | ~1-5s | ~0.2-0.5s | 0 |
| **1000 表** | ~10-50s | ~2-5s | 0 |
| **10000 表** | ~2-8min | ~20-50s | 0 |
| **额外连接数** | +1 | +1 | 0 |
| **最大开销来源** | CHECKPOINT 刷盘 | 串行逐表查询 | — |
| **对业务影响** | CHECKPOINT 可能短暂增加 I/O | **无** | 无 |

### 3.2 增量备份总体效率分析

虽然哈希采集有额外开销，但在增量备份中**节省的数据传输时间通常远超哈希采集开销**：

| 场景 | 不用哈希 | 用哈希 | 净效果 |
|------|--------|--------|--------|
| 100 张 Heap 表（共 500GB），仅 5 张有变更 | 备份 500GB | 哈希 5s + 备份 25GB | **节省 ~95% 时间** |
| 50 个 AO 分区，仅 2 个有变更（GP5） | 备份 50 个分区 | 哈希 0.3s + 备份 2 个分区 | **节省 ~96% 时间** |
| 所有表都有变更 | 备份全部 | 哈希 Ns + 备份全部 | 多花 N 秒（不划算） |

---

## 四、使用建议

### 4.1 推荐启用的场景

| 场景 | 推荐参数 | 理由 |
|------|---------|------|
| Heap 表多、数据量大、修改不频繁 | `--heap-file-hash` | 哈希采集几分钟，避免备份 TB 级不变数据 |
| GP5 AO 分区表多 | `--ao-file-hash` | 开销极小（秒级），避免全分区重复备份 |
| 备份窗口有限 | 两者都用 | 最大化减少备份数据量 |
| 每日增量备份 | 两者都用 | 每日变更通常不多，哈希检测效果显著 |

### 4.2 不推荐启用的场景

| 场景 | 建议 | 理由 |
|------|------|------|
| 所有表每天都有大量修改 | 不加参数 | 大部分表都要备份，哈希采集反而增加开销 |
| 表数量超过 10 万 | 按 schema 分批备份 | 串行逐表查询耗时过长 |
| 备份窗口在业务高峰期 | 仅用 `--ao-file-hash` | 避免 CHECKPOINT 对业务 I/O 的影响 |

### 4.3 优化措施

| 措施 | 效果 |
|------|------|
| 提前手动执行 `CHECKPOINT` | 备份时的自动 CHECKPOINT 几乎瞬间完成 |
| 使用 `--include-schema` 过滤 | 减少需要采集哈希的表数量 |
| 在低峰期执行备份 | CHECKPOINT 刷盘对业务影响最小 |
| 适当设置 `checkpoint_completion_target` | 让 CHECKPOINT 更平滑，减少 I/O 峰值 |

---

## 五、技术实现细节

### 5.1 Heap 表：为什么需要 CHECKPOINT？

PostgreSQL/Greenplum 使用共享缓冲区（shared_buffers）缓存数据页。当执行 INSERT/UPDATE/DELETE 时，修改的数据页（脏页）先写入缓冲区，异步刷写到磁盘。

`pg_stat_file()` 读取的是**磁盘上文件**的 inode 元数据。如果脏页还未刷写，磁盘上的文件修改时间和大小不会反映最新的变更，导致哈希值与上次备份相同——误判为"未修改"。

CHECKPOINT 强制刷写所有脏页，确保 `pg_stat_file()` 的结果准确。

### 5.2 AO 表：为什么不需要 CHECKPOINT？

AO（Append-Optimized）表的变更追踪存储在 `pg_aoseg` catalog 表中，记录每个数据段文件的 `eof`（文件结束位置）和 `tupcount`（行数）。这些元数据在事务提交时**同步更新**到 catalog，不需要额外的刷盘操作。

因此 `--ao-file-hash` 的查询完全在 catalog 层面完成，零额外 I/O。

### 5.3 为什么 AO 哈希排除 modcount？

在 GP5 中，当向一个 AO 分区表的某个子分区插入数据时，`modcount` 会在**所有兄弟分区的 aoseg 表中同步递增**。这是 GP5 内核的行为，导致基于 modcount 的检测无法区分哪个分区真正发生了数据变更。

`--ao-file-hash` 只使用 `eof` + `tupcount` 计算哈希，这两个字段**只在实际发生数据变更的分区中才会改变**，实现了分区级精确检测。
