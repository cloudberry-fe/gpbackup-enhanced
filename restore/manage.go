package restore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/greenplum-db/gpbackup/history"
	"github.com/greenplum-db/gpbackup/options"
)

// HandleManageCommands checks for --list-backups and --delete-backup flags.
// Returns true if a management command was handled (caller should exit).
func HandleManageCommands() bool {
	doList := MustGetFlagBool(options.LIST_BACKUPS)
	deleteTS := MustGetFlagString(options.DELETE_BACKUP)

	if !doList && deleteTS == "" {
		return false
	}

	backupDir := MustGetFlagString(options.BACKUP_DIR)
	if backupDir == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --backup-dir is required for --list-backups and --delete-backup")
		os.Exit(1)
	}

	historyDBPath := findHistoryDB(backupDir)
	if historyDBPath == "" {
		fmt.Fprintf(os.Stderr, "ERROR: Backup history database not found. Searched:\n")
		fmt.Fprintf(os.Stderr, "  %s/gpbackup_history.db\n", backupDir)
		fmt.Fprintf(os.Stderr, "  $MASTER_DATA_DIRECTORY/gpbackup_history.db\n")
		fmt.Fprintf(os.Stderr, "  $COORDINATOR_DATA_DIRECTORY/gpbackup_history.db\n")
		os.Exit(1)
	}

	historyDB, err := history.InitializeHistoryDatabase(historyDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Could not open history database: %v\n", err)
		os.Exit(1)
	}
	defer historyDB.Close()

	if doList {
		backups, err := history.ListBackups(historyDB, backupDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		printBackupList(backups)
	}

	if deleteTS != "" {
		deleted, err := history.DeleteBackup(historyDB, deleteTS)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		printDeleteResult(deleteTS, deleted)

		// Delete physical backup files on all nodes
		deleteBackupFiles(backupDir, deleted)
	}

	return true
}

func printBackupList(backups []history.BackupConfig) {
	if len(backups) == 0 {
		fmt.Println("No backups found.")
		return
	}

	incrBase := make(map[string]string)
	for _, b := range backups {
		if b.Incremental && len(b.RestorePlan) > 0 {
			incrBase[b.Timestamp] = b.RestorePlan[0].Timestamp
		}
	}

	// Sort ascending: oldest first, newest last
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Timestamp < backups[j].Timestamp
	})

	fmt.Println()
	fmt.Printf("%-16s  %-20s  %-6s  %-9s  %-12s  %-20s  %s\n",
		"Timestamp", "Start Time", "Type", "Status", "Database", "Deleted At", "Depends On")
	fmt.Println(strings.Repeat("-", 115))

	for _, b := range backups {
		typeStr := "Full"
		if b.Incremental {
			typeStr = "Incr"
		}

		status := b.Status
		if status == "" {
			status = "Unknown"
		}

		deleted := ""
		if b.DateDeleted != "" {
			deleted = fmtTS(b.DateDeleted)
		}

		dep := ""
		if base, ok := incrBase[b.Timestamp]; ok {
			dep = base
		}

		fmt.Printf("%-16s  %-20s  %-6s  %-9s  %-12s  %-20s  %s\n",
			b.Timestamp, fmtTS(b.Timestamp), typeStr, status, b.DatabaseName, deleted, dep)
	}
	fmt.Println()
}

func printDeleteResult(target string, deleted []string) {
	sort.Strings(deleted)
	fmt.Printf("\nDeleted %d backup(s) from history:\n", len(deleted))
	for _, ts := range deleted {
		label := "(incremental)"
		if ts == target {
			label = "(target)"
		}
		fmt.Printf("  %s  %s  %s\n", ts, fmtTS(ts), label)
	}
	fmt.Println()
}

// deleteBackupFiles removes physical backup files for the given timestamps on all nodes.
// Strategy:
//   1. Find all segment hosts from gp_segment_configuration via psql
//   2. Delete files on coordinator locally
//   3. Delete files on each segment host via ssh
func deleteBackupFiles(backupDir string, timestamps []string) {
	if len(timestamps) == 0 {
		return
	}

	fmt.Println("Removing backup files...")

	// Build list of directories to delete
	// Each timestamp maps to: <backup-dir>/gpseg*/backups/YYYYMMDD/TIMESTAMP/
	var dirsToDelete []string
	for _, ts := range timestamps {
		if len(ts) < 8 {
			continue
		}
		dateDir := ts[:8]
		// Pattern: <backup-dir>/gpseg*/backups/YYYYMMDD/TIMESTAMP
		dirsToDelete = append(dirsToDelete, filepath.Join(backupDir, "gpseg*", "backups", dateDir, ts))
	}

	// 1. Delete coordinator files locally (gpseg-1)
	localDeleted := 0
	for _, ts := range timestamps {
		if len(ts) < 8 {
			continue
		}
		dateDir := ts[:8]
		coordDir := filepath.Join(backupDir, "gpseg-1", "backups", dateDir, ts)
		if _, err := os.Stat(coordDir); err == nil {
			os.RemoveAll(coordDir)
			localDeleted++
			// Try to remove empty parent date directory
			os.Remove(filepath.Join(backupDir, "gpseg-1", "backups", dateDir))
		}
	}
	if localDeleted > 0 {
		fmt.Printf("  Coordinator: removed %d backup directories\n", localDeleted)
	}

	// 2. Get segment hosts from database via psql
	segHosts := getSegmentHosts()
	if len(segHosts) == 0 {
		fmt.Println("  WARNING: Could not determine segment hosts. Segment backup files not deleted.")
		fmt.Println("  To clean manually, run on each segment host:")
		for _, pattern := range dirsToDelete {
			fmt.Printf("    rm -rf %s\n", pattern)
		}
		return
	}

	// 3. Delete on each segment host via ssh
	// Build a single rm command for all timestamps
	var rmParts []string
	for _, ts := range timestamps {
		if len(ts) < 8 {
			continue
		}
		dateDir := ts[:8]
		// Use glob to match all gpseg* directories on each host
		rmParts = append(rmParts,
			filepath.Join(backupDir, "gpseg*", "backups", dateDir, ts))
	}
	rmCmd := "rm -rf " + strings.Join(rmParts, " ")

	uniqueHosts := uniqueStrings(segHosts)
	segDeleted := 0
	for _, host := range uniqueHosts {
		cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=no",
			"-o", "BatchMode=yes", host, "cd / && "+rmCmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("  WARNING: Failed to clean files on %s: %v %s\n", host, err, string(output))
		} else {
			segDeleted++
		}
	}
	if segDeleted > 0 {
		fmt.Printf("  Segments: cleaned backup files on %d host(s): %s\n",
			segDeleted, strings.Join(uniqueHosts, ", "))
	}

	// Also try to clean empty date directories on segment hosts
	var cleanParts []string
	for _, ts := range timestamps {
		if len(ts) < 8 {
			continue
		}
		dateDir := ts[:8]
		cleanParts = append(cleanParts,
			filepath.Join(backupDir, "gpseg*", "backups", dateDir))
	}
	if len(cleanParts) > 0 {
		// rmdir only removes empty dirs, safe to run
		cleanCmd := "cd / && rmdir " + strings.Join(uniqueStrings(cleanParts), " ") + " 2>/dev/null; true"
		for _, host := range uniqueHosts {
			exec.Command("ssh", "-o", "StrictHostKeyChecking=no",
				"-o", "BatchMode=yes", host, cleanCmd).Run()
		}
	}

	fmt.Println("  File cleanup complete.")
}

// getSegmentHosts queries gp_segment_configuration to get all segment hostnames.
// Uses psql via environment (PGDATABASE, PGHOST, etc.) or defaults.
func getSegmentHosts() []string {
	// Get ALL segment hosts (primary + mirror + standby) to ensure
	// backup files are cleaned from every node that may hold data.
	query := "SELECT DISTINCT hostname FROM gp_segment_configuration WHERE content >= 0"

	// Try using psql with various database names
	for _, db := range []string{os.Getenv("PGDATABASE"), "postgres", "template1"} {
		if db == "" {
			continue
		}
		cmd := exec.Command("psql", "-d", db, "-t", "-A", "-c", query)
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		var hosts []string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				hosts = append(hosts, line)
			}
		}
		if len(hosts) > 0 {
			return hosts
		}
	}
	return nil
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	sort.Strings(result)
	return result
}

func findHistoryDB(backupDir string) string {
	candidates := []string{
		backupDir + "/gpbackup_history.db",
		backupDir + "/gpseg-1/gpbackup_history.db",
	}
	for _, envVar := range []string{"MASTER_DATA_DIRECTORY", "COORDINATOR_DATA_DIRECTORY"} {
		if dir := os.Getenv(envVar); dir != "" {
			candidates = append(candidates, dir+"/gpbackup_history.db")
		}
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func fmtTS(ts string) string {
	if len(ts) != 14 {
		return ts
	}
	t, err := time.ParseInLocation("20060102150405", ts, time.Local)
	if err != nil {
		return ts
	}
	return t.Format("2006-01-02 15:04:05")
}
