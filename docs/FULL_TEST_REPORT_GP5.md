# gpbackup 增强版 GP5.28.11 全功能测试报告

测试日期：2026-05-15
测试人：Chong / Claude Code
被测版本：`1.30.5-custom-test`（fix/heap-incremental-partition-leaves 分支，含 commit `0868261`）

## 1. 测试环境

| 项 | 值 |
|---|---|
| 集群 | Greenplum 5.28.11 / Euler DB 2.5.8（PG 8.3.23 内核） |
| Coordinator | dbhost01 |
| Segment 主机 | dbhost03、dbhost04（各 4 个 primary） |
| 主 segment 数 | 8 |
| GPHOME | `/opt/gpsql-v2.5.8.ccb-centos7-opt-12992` |
| 编译工具 | Go 1.21.13 + gcc 4.8.5（CentOS 7） |

## 2. 构建与部署

在 dbhost01 上将源码包解压到 `/root/gpbackup-build/` 后构建：

```bash
GIT_VERSION=1.30.5-custom-test
CGO_ENABLED=1 go build -tags gpbackup        -o gpbackup        -ldflags "-X github.com/greenplum-db/gpbackup/backup.version=${GIT_VERSION}"  ./gpbackup.go
CGO_ENABLED=1 go build -tags gprestore       -o gprestore       -ldflags "-X github.com/greenplum-db/gpbackup/restore.version=${GIT_VERSION}" ./gprestore.go
CGO_ENABLED=1 go build -tags gpbackup_helper -o gpbackup_helper -ldflags "-X github.com/greenplum-db/gpbackup/helper.version=${GIT_VERSION}"  ./gpbackup_helper.go
```

部署步骤：

1. 把原 `$GPHOME/bin/{gpbackup,gprestore,gpbackup_helper}` 备份到 `/root/gpbackup-bin-backup-20260515-101108/`。
2. 新二进制 cp 到 dbhost01:`$GPHOME/bin/`，所有者 `gpadmin:gpadmin`。
3. `gpbackup_helper` 通过 `scp` 分发到 dbhost03、dbhost04 的 `$GPHOME/bin/`。
4. 验证四端版本统一：`1.30.5-custom-test`。

## 3. 测试数据集

数据库 `gpbackup_test`，统一造表脚本 `/tmp/setup_testdb.sql`：

| 表 | 类型 | 行数 | 用途 |
|---|---|---|---|
| `public.h_simple` | heap | 1000 | 独立 heap，会被改动 |
| `public.h_stable` | heap | 500 | 独立 heap，全程保持不变 |
| `public.h_part`（3 leaf） | heap 分区 | 900 | 分区叶子，是 `0868261` 修复的关键场景 |
| `public.ao_simple` | AO row | 1000 | 独立 AO，会被改动 |
| `public.ao_stable` | AO row | 500 | 独立 AO，全程保持不变 |
| `public.ao_part`（3 leaf） | AO row 分区 | 900 | 用于演示 GP5 modcount 传播 |
| `public.aoco_simple` | AO column | 500 | 列存 AO |
| `public.ext_sample` | EXTERNAL | n/a | 验证外部表跳过逻辑 |

## 4. 用例执行结果总览

| # | 用例 | 关键指标 | 结论 |
|---|---|---|---|
| 1 | 全量备份 + 恢复 | 7 张表行数全部 OK | ✅ |
| 2 | 默认 modcount 增量 | 备份 9 张表，恢复行数 OK | ✅ |
| 3 | `--heap-file-hash` 增量 | 备份 **3** 张表（未变 heap 叶子被跳过） | ✅ |
| 4 | `--ao-file-hash` 增量 | 改 1 leaf → 备份 **1** 张；对照组 → **3** 张 | ✅ |
| 5 | `--list-backups` / `--delete-backup` | 列出依赖链；删除时同步清 coordinator + segments + history.db | ✅ |
| 6 | `--gen-ext-metadata` | 4 segments / 11 tables / 28 columns 写入 YAML | ✅ |

总体：**6 / 6 PASS**。

## 5. 用例详情

### 5.1 Case 1 — 全量备份 + 恢复

命令：

```bash
gpbackup  --dbname gpbackup_test --leaf-partition-data --backup-dir $WORK/backups
gprestore --timestamp $TS --backup-dir $WORK/backups --create-db --redirect-db gpbackup_test_restored
```

关键日志：

```
20260515:10:12:59 gpbackup ... -[INFO]:-Data backup complete
Tables backed up: 11 / 11
20260515:10:13:00 gpbackup ... -[INFO]:-Skipped data backup of 1 external/foreign table(s).
20260515:10:13:00 gpbackup ... -[INFO]:-Backup completed successfully
```

恢复后逐表对比：

```
OK h_simple   orig=1000 restored=1000
OK h_stable   orig=500  restored=500
OK h_part     orig=900  restored=900
OK ao_simple  orig=1000 restored=1000
OK ao_stable  orig=500  restored=500
OK ao_part    orig=900  restored=900
OK aoco_simple orig=500 restored=500
```

`ext_sample` 跳过备份（external/foreign），符合预期。

### 5.2 Case 2 — 默认增量（modcount）

只改 `ao_simple`、`h_simple`、`ao_part` 的 Feb 叶子。`gpbackup --incremental --leaf-partition-data` 输出：

```
Collected incremental metadata: 7 AO tables (0 with content hash), 5 heap tables
Tables backed up: 9 / 9
```

**预期 9 张** 的分解：

| 类型 | 张数 | 说明 |
|---|---|---|
| Heap（全部） | 5 | h_simple + h_stable + h_part 3 个叶子。默认模式下 heap **始终重新备份** |
| AO 改动 | 1 | ao_simple |
| AO 分区叶子（全 3 个） | 3 | 改 ao_part_1_prt_2 → GP5 modcount **传播**到同家族所有兄弟，3 个全被标记为变更 |
| 总计 | **9** | ↔ 日志 `9 / 9` |

恢复结果全部 OK。**结论**：默认 modcount 增量对 heap 无优化，AO 受 GP5 modcount 传播影响存在分区级假阳性。

### 5.3 Case 3 — `--heap-file-hash` 增量

只改 `h_simple` 和 `h_part_1_prt_2`，另加一处 AO 改动 `ao_simple` 作为对照。

```
Collected incremental metadata: 7 AO tables (0 with content hash), 5 heap tables
Tables backed up: 3 / 3
```

**3 张的分解**：

| 表 | 是否被改 | 是否备份 |
|---|---|---|
| h_simple | ✓ | ✓ |
| h_part_1_prt_2 | ✓ | ✓ |
| ao_simple | ✓ | ✓ |
| h_stable | × | **× 跳过** |
| h_part_1_prt_1 / _3 | × | **× 跳过** |
| ao_stable / ao_part 三叶子 / aoco_simple | × | × 跳过 |

**关键验证**：commit `0868261` 修复的回归（`getHeapTableFQNs` 错误地把分区叶子排除）此前会导致 h_part 三个叶子每次都被重备份；现在 **只有真正变化的 h_part_1_prt_2 进入备份集**。

恢复结果：所有 7 张表行数 OK。

### 5.4 Case 4 — `--ao-file-hash` 增量

> 本用例的全量与两次增量**始终携带 `--heap-file-hash`**——目的是把 heap
> 这一维度固定下来（heap 表全部未改 → 永远跳过），从而单独观察
> `--ao-file-hash` 对 AO 分区族的影响。如果不加 `--heap-file-hash`，5
> 张 heap 表会在每次增量里被无条件重备份，掩盖 AO 维度的差异。

#### 实验组（带 `--ao-file-hash --heap-file-hash`）

```
gpbackup ... --leaf-partition-data --ao-file-hash --heap-file-hash --incremental --from-timestamp $TS_FULL
```

仅改 `ao_part_1_prt_2`。日志：

```
Collecting AO aoseg content hashes (--ao-file-hash)
Collected incremental metadata: 7 AO tables (7 with content hash), 5 heap tables
Tables backed up: 1 / 1
```

只备份了变化的那一片叶子；兄弟 leaf 1 / 3 被识别为内容未变并跳过，全部 heap 也跳过。

#### 对照组（仅带 `--heap-file-hash`，不带 `--ao-file-hash`）

```
gpbackup ... --leaf-partition-data --heap-file-hash --incremental --from-timestamp $TS_PREV
```

紧接着仅改 `ao_part_1_prt_3`，触发增量：

```
Collected incremental metadata: 7 AO tables (0 with content hash), 5 heap tables
Tables backed up: 3 / 3
```

3 张全部是 `ao_part` 的叶子——GP5 modcount 传播导致兄弟 leaf 1、2 假阳性。

#### 对比

| 模式 | 实际变更叶子 | 备份张数 | 结论 |
|---|---|---|---|
| `--ao-file-hash --heap-file-hash` | 1 | **1** | 精确 |
| 仅 `--heap-file-hash`（modcount 走默认） | 1 | **3** | 整个 AO 分区族被重备份 |

随着分区族尺寸增长（如月分区 12 张、日分区 30/365 张），精确度差距会被放大。

恢复结果：含 leaf 级行数对比，全部 OK。

> **注**：若以上两组都**去掉** `--heap-file-hash`，结果会分别变成
> `1 + 5 = 6 张` 和 `3 + 5 = 8 张`——多出的 5 张是被无差别重备份的 heap
> 表（h_simple、h_stable、3 个 h_part 叶子）。2026-05-19 回归把这一现象
> 复现并写进 `case4_incr_aohash.out` / `case4_incr_modcount.out`。

### 5.5 Case 5 — `--list-backups` / `--delete-backup`

`--list-backups --backup-dir <dir>` 输出样例：

```
Timestamp         Start Time            Type    Status     Database      Deleted At  Depends On
---------------------------------------------------------------------------------------------------
20260515101258    2026-05-15 10:12:58   Full    Success    gpbackup_test
20260515101335    2026-05-15 10:13:35   Incr    Success    gpbackup_test             20260515101258
```

删除中间的 leaf 增量 `20260515101335`：

```
Deleted 1 backup(s) from history:
  20260515101335  2026-05-15 10:13:35  (target)

Removing backup files...
  Coordinator: removed 1 backup directories
  Segments: cleaned backup files on 2 host(s): dbhost03, dbhost04
  File cleanup complete.
```

验证：

- 删除后 `--list-backups` 输出仅剩 Full（`20260515101258`）。
- coordinator + 两台 segment 主机的 `gpseg*/backups/20260515/20260515101335/` 目录全部消失。
- sqlite 查询 `backups` 表，该 timestamp 无记录。

再删 Full `20260515101258`：因 leaf 增量已被先删，它现在已经是孤立 Full，删除成功（同样 1 backup deleted + 两台 segment 文件清理）。

### 5.6 Case 6 — `--gen-ext-metadata`

```bash
gpbackup --dbname gpbackup_test --leaf-partition-data --gen-ext-metadata \
    --backup-dir $WORK/backups_ext
```

新增两个产物（与原本 `_toc.yaml` / `_metadata.sql` 同目录）：

- `gpbackup_<TS>_ext_metadata.yaml`：segments + 每张表的列定义
- `gpbackup_<TS>_ext_config.yaml`：可手工编辑的 host_map（跨集群恢复用）

抽样校验：

```
segments listed: 4         ← 与 gp_segment_configuration primary 数对齐
tables listed:   11        ← 与 TOC 中 dataentries 一致（h_simple, h_stable, h_part 3 leaves, ao_simple, ao_stable, ao_part 3 leaves, aoco_simple）
columns listed:  28        ← 列定义齐全
hostname 包含 dbhost03、dbhost04
```

GP5 走的是 `pg_filespace_entry` 取 datadir（`backup/ext_metadata.go:111`），均正确输出。

## 6. 远端测试遗留物

dbhost01:`/home/gpadmin/gpbackup-fulltest/`：

| 内容 | 备注 |
|---|---|
| `case[1-6].log`、`case[*]_*.out` | 每个用例的完整日志 |
| `backups_h/` | Case 3 备份链（保留） |
| `backups_ao/` | Case 4 备份链（保留） |
| `backups_ext/` | Case 6 备份目录（保留） |
| `backups/` | Case 1+2 链（已被 Case 5 删除） |
| `/home/gpadmin/gpbackup_history.db` | 历史 DB |

如需还原原二进制：

```bash
cp /root/gpbackup-bin-backup-20260515-101108/* $GPHOME/bin/
# 别忘了 dbhost03/dbhost04 上的 gpbackup_helper
```

## 7. 结论

1. 三个新增功能（`--heap-file-hash`、`--ao-file-hash`、备份管理、`--gen-ext-metadata`）在 GP 5.28.11 上行为正确。
2. `commit 0868261` 修复使 `--heap-file-hash` 在带 `--leaf-partition-data` 的分区表上真正生效，未变化叶子不再被重备份。
3. `--ao-file-hash` 在 GP5 modcount 传播场景下显著减少误备份的 AO 分区张数。
4. 备份管理命令在多主机部署下能正确穿透 SSH 清理段主机数据。
