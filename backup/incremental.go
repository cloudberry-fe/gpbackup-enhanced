package backup

import (
	"fmt"
	"path"

	"github.com/greenplum-db/gp-common-go-libs/gplog"
	"github.com/greenplum-db/gp-common-go-libs/operating"
	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
	"github.com/pkg/errors"
)

// FilterTablesForIncremental filters tables based on change detection.
//
// --ao-file-hash and --heap-file-hash are independent:
//   - AO tables:  default=modcount+DDL; with --ao-file-hash=aoseg content hash
//   - Heap tables: default=always backup (original gpbackup behavior);
//                   with --heap-file-hash=file hash (skip unchanged)
func FilterTablesForIncremental(lastBackupTOC, currentTOC *toc.TOC, tables []Table) []Table {
	useAOHash := MustGetFlagBool(options.AO_FILE_HASH)
	useHeapHash := MustGetFlagBool(options.HEAP_FILE_HASH)

	var filteredTables []Table
	for _, table := range tables {
		fqn := table.FQN()

		// --- AO table ---
		if currentAO, isAO := currentTOC.IncrementalMetadata.AO[fqn]; isAO {
			if useAOHash && currentAO.FileHashMD5 != "" {
				// --ao-file-hash: per-table aoseg content hash
				prevAO, hasPrev := lastBackupTOC.IncrementalMetadata.AO[fqn]
				if !hasPrev || prevAO.FileHashMD5 == "" {
					gplog.Debug("Filter: %s (AO/content) prev hash missing, including", fqn)
					filteredTables = append(filteredTables, table)
					continue
				}
				changed := prevAO.FileHashMD5 != currentAO.FileHashMD5
				gplog.Debug("Filter: %s (AO/content) prev=%s curr=%s changed=%v",
					fqn, prevAO.FileHashMD5, currentAO.FileHashMD5, changed)
				if changed {
					filteredTables = append(filteredTables, table)
				}
			} else {
				// Default: modcount + DDL timestamp
				prevAO := lastBackupTOC.IncrementalMetadata.AO[fqn]
				changed := prevAO.Modcount != currentAO.Modcount ||
					prevAO.LastDDLTimestamp != currentAO.LastDDLTimestamp
				gplog.Debug("Filter: %s (AO/modcount) prev=%d curr=%d changed=%v",
					fqn, prevAO.Modcount, currentAO.Modcount, changed)
				if changed {
					filteredTables = append(filteredTables, table)
				}
			}
			continue
		}

		// --- Heap table ---
		if useHeapHash {
			// --heap-file-hash: use file hash to detect changes
			if currentTOC.IncrementalMetadata.Heap == nil {
				filteredTables = append(filteredTables, table)
				continue
			}
			currentHeap, isHeap := currentTOC.IncrementalMetadata.Heap[fqn]
			if !isHeap || currentHeap.FileHashMD5 == "" {
				gplog.Debug("Filter: %s (Heap) no current hash, including", fqn)
				filteredTables = append(filteredTables, table)
				continue
			}
			if lastBackupTOC.IncrementalMetadata.Heap == nil {
				filteredTables = append(filteredTables, table)
				continue
			}
			prevHeap, hasPrev := lastBackupTOC.IncrementalMetadata.Heap[fqn]
			if !hasPrev || prevHeap.FileHashMD5 == "" {
				gplog.Debug("Filter: %s (Heap) prev hash missing, including", fqn)
				filteredTables = append(filteredTables, table)
				continue
			}
			changed := prevHeap.FileHashMD5 != currentHeap.FileHashMD5
			gplog.Debug("Filter: %s (Heap) prev=%s curr=%s changed=%v",
				fqn, prevHeap.FileHashMD5, currentHeap.FileHashMD5, changed)
			if changed {
				filteredTables = append(filteredTables, table)
			}
		} else {
			// Default (original gpbackup behavior): Heap tables always included
			filteredTables = append(filteredTables, table)
		}
	}

	return filteredTables
}

func GetTargetBackupTimestamp() string {
	targetTimestamp := ""
	if fromTimestamp := MustGetFlagString(options.FROM_TIMESTAMP); fromTimestamp != "" {
		validateFromTimestamp(fromTimestamp)
		targetTimestamp = fromTimestamp
	} else {
		targetTimestamp = GetLatestMatchingBackupTimestamp()
	}
	return targetTimestamp
}

func GetLatestMatchingBackupTimestamp() string {
	latestTimestamp := ""
	var latestMatchingBackupHistoryEntry *history.BackupConfig

	historyDBPath := globalFPInfo.GetBackupHistoryDatabasePath()
	_, err := operating.System.Stat(historyDBPath)
	if err == nil {
		latestMatchingBackupHistoryEntry = GetLatestMatchingBackupConfig(historyDBPath, &backupReport.BackupConfig)
	}

	if latestMatchingBackupHistoryEntry == nil {
		gplog.FatalOnError(errors.Errorf("There was no matching previous backup found with the flags provided. " +
			"Please take a full backup."))
	} else {
		latestTimestamp = latestMatchingBackupHistoryEntry.Timestamp
	}

	return latestTimestamp
}

func GetLatestMatchingBackupConfig(historyDBPath string, currentBackupConfig *history.BackupConfig) *history.BackupConfig {
	// get list of timestamps for backups that match filterable flags, most recent first, then
	// iterate through them querying and checking one at a time. this is necessary due to the
	// impracticality of checking the include and exclude sets directly in a query

	historyDB, _ := history.InitializeHistoryDatabase(historyDBPath)

	whereClause := fmt.Sprintf(`backup_dir = '%s' AND database_name = '%s' AND leaf_partition_data = %v
		AND plugin = '%s' AND single_data_file = %v AND compressed = %v AND date_deleted = '' AND status = '%s'`,
		MustGetFlagString(options.BACKUP_DIR),
		currentBackupConfig.DatabaseName,
		MustGetFlagBool(options.LEAF_PARTITION_DATA),
		currentBackupConfig.Plugin,
		MustGetFlagBool(options.SINGLE_DATA_FILE),
		currentBackupConfig.Compressed,
        history.BackupStatusSucceed)

	getBackupTimetampsQuery := fmt.Sprintf(`
		SELECT timestamp
		FROM backups
		WHERE %s
		ORDER BY timestamp DESC`, whereClause)
	timestampRows, err := historyDB.Query(getBackupTimetampsQuery)
	if err != nil {
		gplog.Error(err.Error())
		return nil
	}
	defer timestampRows.Close()

	timestamps := make([]string, 0)
	for timestampRows.Next() {
		var timestamp string
		err = timestampRows.Scan(&timestamp)
		if err != nil {
			gplog.Error(err.Error())
			return nil
		}
		timestamps = append(timestamps, timestamp)
	}

	for _, ts := range timestamps {
		backupConfig, err := history.GetBackupConfig(ts, historyDB)
		if err != nil {
			gplog.Error(err.Error())
			return nil
		}
		if !backupConfig.Failed() && matchesIncrementalFlags(backupConfig, currentBackupConfig) {
			return backupConfig
		}
	}

	return nil
}

func matchesIncrementalFlags(backupConfig *history.BackupConfig, currentBackupConfig *history.BackupConfig) bool {
	_, pluginBinaryName := path.Split(backupConfig.Plugin)
	return backupConfig.BackupDir == MustGetFlagString(options.BACKUP_DIR) &&
		backupConfig.DatabaseName == currentBackupConfig.DatabaseName &&
		backupConfig.LeafPartitionData == MustGetFlagBool(options.LEAF_PARTITION_DATA) &&
		pluginBinaryName == currentBackupConfig.Plugin &&
		backupConfig.SingleDataFile == MustGetFlagBool(options.SINGLE_DATA_FILE) &&
		backupConfig.Compressed == currentBackupConfig.Compressed &&
		// Expanding of the include list happens before this now so we must compare again current backup config
		utils.NewIncludeSet(backupConfig.IncludeRelations).Equals(utils.NewIncludeSet(currentBackupConfig.IncludeRelations)) &&
		utils.NewIncludeSet(backupConfig.IncludeSchemas).Equals(utils.NewIncludeSet(MustGetFlagStringArray(options.INCLUDE_SCHEMA))) &&
		utils.NewIncludeSet(backupConfig.ExcludeRelations).Equals(utils.NewIncludeSet(MustGetFlagStringArray(options.EXCLUDE_RELATION))) &&
		utils.NewIncludeSet(backupConfig.ExcludeSchemas).Equals(utils.NewIncludeSet(MustGetFlagStringArray(options.EXCLUDE_SCHEMA)))
}

func PopulateRestorePlan(changedTables []Table,
	restorePlan []history.RestorePlanEntry, allTables []Table) []history.RestorePlanEntry {
	currBackupRestorePlanEntry := history.RestorePlanEntry{
		Timestamp: globalFPInfo.Timestamp,
		TableFQNs: make([]string, 0, len(changedTables)),
	}

	for _, changedTable := range changedTables {
		changedTableFQN := changedTable.FQN()
		currBackupRestorePlanEntry.TableFQNs = append(currBackupRestorePlanEntry.TableFQNs, changedTableFQN)
	}

	changedTableFQNs := make(map[string]bool)
	for _, changedTable := range changedTables {
		changedTableFQN := changedTable.FQN()
		changedTableFQNs[changedTableFQN] = true
	}

	allTableFQNs := make(map[string]bool)
	for _, table := range allTables {
		tableFQN := table.FQN()
		allTableFQNs[tableFQN] = true
	}

	// Removing filtered table FQNs for the current backup from entries with previous timestamps
	for i, restorePlanEntry := range restorePlan {
		tableFQNs := make([]string, 0)
		for _, tableFQN := range restorePlanEntry.TableFQNs {
			if !changedTableFQNs[tableFQN] && allTableFQNs[tableFQN] {
				tableFQNs = append(tableFQNs, tableFQN)
			}
		}
		restorePlan[i].TableFQNs = tableFQNs
	}
	restorePlan = append(restorePlan, currBackupRestorePlanEntry)

	return restorePlan
}
