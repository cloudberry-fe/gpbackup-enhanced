package backup

import (
	"fmt"
	"strings"
	"time"

	"github.com/greenplum-db/gp-common-go-libs/dbconn"
	"github.com/greenplum-db/gp-common-go-libs/gplog"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/greenplum-db/gpbackup/toc"
	"github.com/greenplum-db/gpbackup/utils"
)

// GenerateExtMetadata generates ext_metadata.yaml and ext_config.yaml
// alongside the backup files, enabling cross-cluster backup querying.
func GenerateExtMetadata() {
	if !MustGetFlagBool(options.GEN_EXT_METADATA) {
		return
	}

	gplog.Info("Generating external table metadata files (--gen-ext-metadata)")

	metaPath := globalFPInfo.GetDirForContent(-1) + "/gpbackup_" + globalFPInfo.Timestamp + "_ext_metadata.yaml"
	configPath := globalFPInfo.GetDirForContent(-1) + "/gpbackup_" + globalFPInfo.Timestamp + "_ext_config.yaml"

	// Use a separate connection for metadata queries
	metaConn := dbconn.NewDBConnFromEnvironment(connectionPool.DBName)
	metaConn.MustConnect(1)
	defer metaConn.Close()

	// 1. Collect segment info
	segInfo := getSegmentInfoForMetadata(metaConn)

	// 2. Collect table column definitions from TOC data entries
	tableInfo := getTableColumnsForMetadata(metaConn, globalTOC)

	// 3. Write metadata YAML
	var meta strings.Builder
	meta.WriteString(fmt.Sprintf("# gpbackup external table metadata\n"))
	meta.WriteString(fmt.Sprintf("# Generated: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	meta.WriteString(fmt.Sprintf("# Timestamp: %s\n", globalFPInfo.Timestamp))
	meta.WriteString(fmt.Sprintf("# Database: %s\n\n", connectionPool.DBName))
	meta.WriteString("segments:\n")
	for _, seg := range segInfo {
		meta.WriteString(fmt.Sprintf("  - content: %d\n", seg.Content))
		meta.WriteString(fmt.Sprintf("    hostname: \"%s\"\n", seg.Hostname))
		meta.WriteString(fmt.Sprintf("    port: %d\n", seg.Port))
		meta.WriteString(fmt.Sprintf("    datadir: \"%s\"\n", seg.DataDir))
	}
	meta.WriteString("\ntables:\n")
	for _, t := range tableInfo {
		meta.WriteString(fmt.Sprintf("  - schema: \"%s\"\n", t.Schema))
		meta.WriteString(fmt.Sprintf("    name: \"%s\"\n", t.Name))
		meta.WriteString(fmt.Sprintf("    oid: %d\n", t.Oid))
		meta.WriteString("    columns:\n")
		for _, col := range t.Columns {
			meta.WriteString(fmt.Sprintf("      - name: \"%s\"\n", col.Name))
			meta.WriteString(fmt.Sprintf("        type: \"%s\"\n", col.Type))
		}
	}

	_ = utils.WriteToFileAndMakeReadOnly(metaPath, []byte(meta.String()))

	// 4. Write editable config YAML
	var cfg strings.Builder
	cfg.WriteString("# gpbackup external table config\n")
	cfg.WriteString("# Edit this file to remap hosts/paths for cross-cluster backup querying.\n\n")
	cfg.WriteString(fmt.Sprintf("timestamp: \"%s\"\n", globalFPInfo.Timestamp))
	cfg.WriteString(fmt.Sprintf("database: \"%s\"\n", connectionPool.DBName))
	cfg.WriteString(fmt.Sprintf("backup_dir: \"%s\"\n", MustGetFlagString(options.BACKUP_DIR)))
	cfg.WriteString("gpfdist_port: 0\n\n")
	cfg.WriteString("host_map:\n")

	uniqueHosts := make(map[string]bool)
	for _, seg := range segInfo {
		if !uniqueHosts[seg.Hostname] {
			uniqueHosts[seg.Hostname] = true
			cfg.WriteString(fmt.Sprintf("  - original: \"%s\"\n", seg.Hostname))
			cfg.WriteString(fmt.Sprintf("    target: \"%s\"\n", seg.Hostname))
		}
	}

	_ = utils.WriteToFileAndMakeReadOnly(configPath, []byte(cfg.String()))

	gplog.Info("External table metadata: %s (%d tables)", metaPath, len(tableInfo))
	gplog.Info("External table config:   %s", configPath)
}

type segmentMeta struct {
	Content  int    `db:"content"`
	Hostname string `db:"hostname"`
	Port     int    `db:"port"`
	DataDir  string `db:"datadir"`
}

type columnMeta struct {
	Name string `db:"name"`
	Type string `db:"type"`
}

type tableMeta struct {
	Schema  string
	Name    string
	Oid     uint32
	Columns []columnMeta
}

func getSegmentInfoForMetadata(conn *dbconn.DBConn) []segmentMeta {
	var query string
	if conn.Version.Before("6") {
		query = `SELECT c.content, c.hostname, c.port, f.fselocation AS datadir
			FROM gp_segment_configuration c
			JOIN pg_filespace_entry f ON c.dbid = f.fsedbid
			WHERE c.role = 'p' AND c.content >= 0
			AND f.fsefsoid = (SELECT oid FROM pg_filespace WHERE fsname = 'pg_system')
			ORDER BY c.content`
	} else {
		query = `SELECT content, hostname, port, datadir
			FROM gp_segment_configuration
			WHERE role = 'p' AND content >= 0 ORDER BY content`
	}

	var results []segmentMeta
	err := conn.Select(&results, query)
	if err != nil {
		gplog.Warn("Could not query segment info for ext metadata: %v", err)
		return nil
	}
	return results
}

func getTableColumnsForMetadata(conn *dbconn.DBConn, tocObj *toc.TOC) []tableMeta {
	var tables []tableMeta
	for _, entry := range tocObj.DataEntries {
		colQuery := fmt.Sprintf(`SELECT attname AS name, format_type(atttypid, atttypmod) AS type
			FROM pg_attribute a
			JOIN pg_class c ON a.attrelid = c.oid
			JOIN pg_namespace n ON c.relnamespace = n.oid
			WHERE n.nspname = '%s' AND c.relname = '%s'
			AND a.attnum > 0 AND NOT a.attisdropped
			ORDER BY a.attnum`, entry.Schema, entry.Name)

		var cols []columnMeta
		err := conn.Select(&cols, colQuery)
		if err != nil || len(cols) == 0 {
			continue
		}
		tables = append(tables, tableMeta{
			Schema:  entry.Schema,
			Name:    entry.Name,
			Oid:     entry.Oid,
			Columns: cols,
		})
	}
	return tables
}
