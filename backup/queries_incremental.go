package backup

import (
	"fmt"
	"strings"

	"github.com/greenplum-db/gp-common-go-libs/dbconn"
	"github.com/greenplum-db/gp-common-go-libs/gplog"
	"github.com/greenplum-db/gpbackup/toc"
)

func GetAOIncrementalMetadata(connectionPool *dbconn.DBConn) map[string]toc.AOEntry {
	gplog.Verbose("Querying table row mod counts")
	var modCounts = getAllModCounts(connectionPool)
	gplog.Verbose("Querying last DDL modification timestamp for tables")
	var lastDDLTimestamps = getLastDDLTimestamps(connectionPool)
	aoTableEntries := make(map[string]toc.AOEntry)
	for aoTableFQN := range modCounts {
		aoTableEntries[aoTableFQN] = toc.AOEntry{
			Modcount:         modCounts[aoTableFQN],
			LastDDLTimestamp: lastDDLTimestamps[aoTableFQN],
		}
	}

	return aoTableEntries
}

// GetAOIncrementalMetadataWithFileHash collects AO table metadata using file timestamp hash
// instead of modcount + DDL timestamp. This provides a more reliable detection mechanism
// that catches changes missed by modcount (e.g., VACUUM FULL on GP7+).
// getFileHashesForTables collects file timestamp MD5 hashes for a list of tables
// using the provided connection. The connection must be reused across all tables
// to ensure consistent gp_segment_id mapping.
func getFileHashesForTables(hashConn *dbconn.DBConn, tableFQNs []string) map[string]string {
	result := make(map[string]string)
	for _, fqn := range tableFQNs {
		hash := getTableFileHash(hashConn, fqn)
		if hash != "" {
			result[fqn] = hash
		}
	}
	return result
}

// ensureFileStatFunction creates a plpgsql function on the source database
// that uses pg_stat_file() to get data file modification time and size.
// This is pure SQL — no plpython, no shell, no psql subprocess.
// Uses a separate database connection to avoid interfering with the backup transaction.
func ensureFileStatFunction(connectionPool *dbconn.DBConn) bool {
	gplog.Verbose("Setting up file hash detection function (plpgsql + pg_stat_file)")

	setupConn := dbconn.NewDBConnFromEnvironment(connectionPool.DBName)
	setupConn.MustConnect(1)
	defer setupConn.Close()

	// Check if function exists
	checkSQL := `SELECT 1 FROM pg_proc
		WHERE proname = 'gpbackup_file_info'
		AND pronamespace = (SELECT oid FROM pg_namespace WHERE nspname = 'gp_toolkit');`
	var checkResult []struct{ Val int }
	err := setupConn.Select(&checkResult, checkSQL)

	if err != nil || len(checkResult) == 0 {
		gplog.Verbose("Creating gp_toolkit.gpbackup_file_info function")

		createSQL := `
CREATE OR REPLACE FUNCTION gp_toolkit.gpbackup_file_info(p_schema text, p_table text)
RETURNS text AS $BODY$
DECLARE
    v_tsp  oid;
    v_rfn  oid;
    v_dboid oid;
    v_dbtsp oid;
    v_path text;
    v_mod  text;
    v_size text;
BEGIN
    -- Get table's tablespace and relfilenode (local catalog on each segment)
    SELECT c.reltablespace, c.relfilenode INTO v_tsp, v_rfn
    FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid
    WHERE n.nspname = p_schema AND c.relname = p_table;

    IF v_rfn IS NULL THEN
        RETURN '';
    END IF;

    -- Get database oid and default tablespace
    SELECT oid, dattablespace INTO v_dboid, v_dbtsp
    FROM pg_database WHERE datname = current_database();

    IF v_tsp = 0 THEN
        v_tsp := v_dbtsp;
    END IF;

    -- Construct the main data file path
    IF v_tsp = 1663 THEN
        v_path := 'base/' || v_dboid || '/' || v_rfn;
    ELSE
        v_path := 'pg_tblspc/' || v_tsp || '/' || v_dboid || '/' || v_rfn;
    END IF;

    -- Get modification time and size via pg_stat_file.
    -- A CHECKPOINT must be issued before calling this function to ensure
    -- dirty pages are flushed and mtime/size reflect actual data changes.
    BEGIN
        SELECT (pg_stat_file(v_path)).modification::text,
               (pg_stat_file(v_path)).size::text
        INTO v_mod, v_size;
    EXCEPTION WHEN OTHERS THEN
        v_mod := '';
        v_size := '0';
    END;

    RETURN COALESCE(v_mod, '') || '|' || COALESCE(v_size, '0');
END;
$BODY$ LANGUAGE plpgsql;`

		_, createErr := setupConn.Exec(createSQL, 0)
		if createErr != nil {
			gplog.Warn("Could not create gpbackup_file_info function: %v", createErr)
			return false
		}
		gplog.Verbose("gpbackup_file_info function created successfully")
	}

	return true
}

// getHeapTableFQNs returns fully qualified names of all heap tables in the backup set
func getHeapTableFQNs(connectionPool *dbconn.DBConn) []string {
	var query string
	if connectionPool.Version.Before("7") {
		query = fmt.Sprintf(`
			SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS tablefqn
			FROM pg_class c
			JOIN pg_namespace n ON c.relnamespace = n.oid
			WHERE c.relstorage = 'h'
			AND c.relkind = 'r'
			AND c.oid NOT IN (SELECT inhrelid FROM pg_inherits)
			AND %s`, relationAndSchemaFilterClause())
	} else {
		query = fmt.Sprintf(`
			SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS tablefqn
			FROM pg_class c
			JOIN pg_namespace n ON c.relnamespace = n.oid
			JOIN pg_am a ON c.relam = a.oid
			WHERE a.amname = 'heap'
			AND c.relkind = 'r'
			AND c.oid NOT IN (SELECT inhrelid FROM pg_inherits)
			AND %s`, relationAndSchemaFilterClause())
	}

	var results []struct{ TableFQN string }
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	fqns := make([]string, len(results))
	for i, r := range results {
		fqns[i] = r.TableFQN
	}
	return fqns
}

// getAOTableFQNs returns fully qualified names of all AO/AOCS tables in the backup set
func getAOTableFQNs(connectionPool *dbconn.DBConn) []string {
	var query string
	if connectionPool.Version.Before("7") {
		query = fmt.Sprintf(`
			SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS tablefqn
			FROM pg_class c
			JOIN pg_namespace n ON c.relnamespace = n.oid
			WHERE c.relstorage IN ('ao', 'co')
			AND %s`, relationAndSchemaFilterClause())
	} else {
		query = fmt.Sprintf(`
			SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS tablefqn
			FROM pg_class c
			JOIN pg_namespace n ON c.relnamespace = n.oid
			JOIN pg_am a ON c.relam = a.oid
			WHERE a.amname IN ('ao_row', 'ao_column')
			AND %s`, relationAndSchemaFilterClause())
	}

	var results []struct{ TableFQN string }
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	fqns := make([]string, len(results))
	for i, r := range results {
		fqns[i] = r.TableFQN
	}
	return fqns
}

// getTableFileHash computes an MD5 hash of data file info across all segments.
// Uses gpbackup_file_info (plpgsql + pg_stat_file) — pure SQL, no shell.
// gp_dist_random('gp_id') ensures the function runs on each segment locally.
func getTableFileHash(hashConn *dbconn.DBConn, tableFQN string) string {
	parts := splitFQN(tableFQN)
	if len(parts) != 2 {
		return ""
	}
	schema, table := parts[0], parts[1]

	query := fmt.Sprintf(`
		SELECT COALESCE(md5(string_agg(
			gp_segment_id::text || ',' || info, chr(10) ORDER BY gp_segment_id
		)), '') AS filehash
		FROM (
			SELECT gp_segment_id,
				gp_toolkit.gpbackup_file_info('%s', '%s') AS info
			FROM gp_dist_random('gp_id')
		) x
		WHERE info <> ''`,
		schema, table)

	var results []struct{ FileHash string }
	err := hashConn.Select(&results, query)
	if err != nil {
		gplog.Warn("Could not get file hash for %s: %v", tableFQN, err)
		return ""
	}
	if len(results) == 0 || results[0].FileHash == "" {
		return ""
	}
	return results[0].FileHash
}

// splitFQN splits "schema.table" into [schema, table], removing quotes
func splitFQN(fqn string) []string {
	// Handle quoted identifiers: "schema"."table" or schema.table
	fqn = strings.ReplaceAll(fqn, "\"", "")
	parts := strings.SplitN(fqn, ".", 2)
	return parts
}

// GetAOContentHashes computes a per-table content hash from the pg_aoseg metadata.
// Each AO/AOCS table has its own aoseg table (pg_aoseg.pg_aoseg_<oid> or pg_aocsseg_<oid>).
// The hash is computed from (segno, eof, tupcount, modcount) of ALL rows in the aoseg table.
// This provides per-partition granularity: in GP5, parent modcount changes when any child
// is modified, but each child's aoseg table only changes when THAT child is modified.
// GetAOContentHashes computes a per-table content hash from the pg_aoseg metadata.
// Uses a separate connection to avoid transaction abort propagation.
func GetAOContentHashes(connectionPool *dbconn.DBConn) map[string]string {
	segTableFQNs := getAOSegTableFQNs(connectionPool)

	// Use a separate connection so that a failed query on one table
	// doesn't abort the transaction for subsequent tables.
	hashConn := dbconn.NewDBConnFromEnvironment(connectionPool.DBName)
	hashConn.MustConnect(1)
	defer hashConn.Close()

	result := make(map[string]string)
	for aoTableFQN, aosegTableFQN := range segTableFQNs {
		hash := getAOSegContentHash(hashConn, aosegTableFQN)
		if hash != "" {
			result[aoTableFQN] = hash
		}
	}
	return result
}

// getAOSegContentHash queries the aoseg metadata table and returns an MD5 hash.
// Handles both AO row tables (pg_aoseg.pg_aoseg_*) and AOCS column tables
// (pg_aoseg.pg_aocsseg_*) which have different column schemas.
func getAOSegContentHash(hashConn *dbconn.DBConn, aosegTableFQN string) string {
	// Detect table type from name: pg_aocsseg_* = column store, pg_aoseg_* = row store.
	// AOCS has: segno, column_num, physical_segno, tupcount, eof, eof_uncompressed, modcount, state
	// AO has:   segno, eof, tupcount, varblockcount, eofuncompressed, modcount, state
	isColumnStore := strings.Contains(aosegTableFQN, "pg_aocsseg")

	var cols string
	// Hash only data-content columns (segno, eof, tupcount), deliberately
	// EXCLUDING modcount. In GP5, modcount propagates across sibling partitions
	// even when only one is modified, but eof and tupcount only change on the
	// partition that actually received data.
	if isColumnStore {
		if hashConn.Version.Before("6") {
			cols = "segno::text || ',' || tupcount::text"
		} else {
			cols = "segno::text || ',' || column_num::text || ',' || physical_segno::text || ',' || tupcount::text || ',' || eof_uncompressed::text"
		}
	} else {
		cols = "segno::text || ',' || eof::text || ',' || tupcount::text"
	}

	var query string
	if hashConn.Version.Before("7") {
		query = fmt.Sprintf(`SELECT COALESCE(md5(string_agg(%s,
			chr(10) ORDER BY segno)), '') AS contenthash FROM %s`, cols, aosegTableFQN)
	} else {
		query = fmt.Sprintf(`SELECT COALESCE(md5(string_agg(
			gp_segment_id::text || ',' || %s,
			chr(10) ORDER BY gp_segment_id, segno)), '') AS contenthash FROM gp_dist_random('%s')`,
			cols, aosegTableFQN)
	}

	var results []struct{ ContentHash string }
	err := hashConn.Select(&results, query)
	if err != nil {
		gplog.Warn("Could not get aoseg content hash for %s: %v", aosegTableFQN, err)
		return ""
	}
	if len(results) == 0 {
		return ""
	}
	return results[0].ContentHash
}

func getAllModCounts(connectionPool *dbconn.DBConn) map[string]int64 {
	var segTableFQNs = getAOSegTableFQNs(connectionPool)
	modCounts := make(map[string]int64)
	for aoTableFQN, segTableFQN := range segTableFQNs {
		modCounts[aoTableFQN] = getModCount(connectionPool, segTableFQN)
	}
	return modCounts
}

func getAOSegTableFQNs(connectionPool *dbconn.DBConn) map[string]string {

	before7Query := fmt.Sprintf(`
		SELECT seg.aotablefqn,
			'pg_aoseg.' || quote_ident(aoseg_c.relname) AS aosegtablefqn
		FROM pg_class aoseg_c
			JOIN (SELECT pg_ao.relid AS aooid,
					pg_ao.segrelid,
					aotables.aotablefqn
				FROM pg_appendonly pg_ao
					JOIN (SELECT c.oid,
							quote_ident(n.nspname)|| '.' || quote_ident(c.relname) AS aotablefqn
						FROM pg_class c
							JOIN pg_namespace n ON c.relnamespace = n.oid
						WHERE relstorage IN ( 'ao', 'co' )
							AND %s
					) aotables ON pg_ao.relid = aotables.oid
			) seg ON aoseg_c.oid = seg.segrelid`, relationAndSchemaFilterClause())

	atLeast7Query := fmt.Sprintf(`
		SELECT seg.aotablefqn,
			'pg_aoseg.' || quote_ident(aoseg_c.relname) AS aosegtablefqn
		FROM pg_class aoseg_c
			JOIN (SELECT pg_ao.relid AS aooid,
					pg_ao.segrelid,
					aotables.aotablefqn
				FROM pg_appendonly pg_ao
					JOIN (SELECT c.oid,
							quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS aotablefqn
						FROM pg_class c
							JOIN pg_namespace n ON c.relnamespace = n.oid
							JOIN pg_am a ON c.relam = a.oid
						WHERE a.amname in ('ao_row', 'ao_column')
							AND %s
					) aotables ON pg_ao.relid = aotables.oid
			) seg ON aoseg_c.oid = seg.segrelid`, relationAndSchemaFilterClause())

	query := ""
	if connectionPool.Version.Before("7") {
		query = before7Query
	} else {
		query = atLeast7Query
	}

	results := make([]struct {
		AOTableFQN    string
		AOSegTableFQN string
	}, 0)
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	resultMap := make(map[string]string)
	for _, result := range results {
		resultMap[result.AOTableFQN] = result.AOSegTableFQN
	}
	return resultMap
}

func getModCount(connectionPool *dbconn.DBConn, aosegtablefqn string) int64 {

	before7Query := fmt.Sprintf(`SELECT COALESCE(pg_catalog.sum(modcount), 0) AS modcount FROM %s`,
		aosegtablefqn)

	// In GPDB 7+, the coordinator no longer stores AO segment data so we must
	// query the modcount from the segments. Unfortunately, this does give a
	// false positive if a VACUUM FULL compaction happens on the AO table.
	atLeast7Query := fmt.Sprintf(`SELECT COALESCE(pg_catalog.sum(modcount), 0) AS modcount FROM gp_dist_random('%s')`,
		aosegtablefqn)

	query := ""
	if connectionPool.Version.Before("7") {
		query = before7Query
	} else {
		query = atLeast7Query
	}

	var results []struct {
		Modcount int64
	}
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)

	return results[0].Modcount
}

func getLastDDLTimestamps(connectionPool *dbconn.DBConn) map[string]string {
	before7Query := fmt.Sprintf(`
		SELECT quote_ident(aoschema) || '.' || quote_ident(aorelname) as aotablefqn,
			lastddltimestamp
		FROM ( SELECT c.oid AS aooid,
					n.nspname AS aoschema,
					c.relname AS aorelname
				FROM pg_class c
				JOIN pg_namespace n ON c.relnamespace = n.oid
				WHERE c.relstorage IN ('ao', 'co')
				AND %s
			) aotables
		JOIN ( SELECT lo.objid,
					MAX(lo.statime) AS lastddltimestamp
				FROM pg_stat_last_operation lo
				WHERE lo.staactionname IN ('CREATE', 'ALTER', 'TRUNCATE')
				GROUP BY lo.objid
			) lastop
		ON aotables.aooid = lastop.objid`, relationAndSchemaFilterClause())

	atLeast7Query := fmt.Sprintf(`
		SELECT quote_ident(aoschema) || '.' || quote_ident(aorelname) as aotablefqn,
			lastddltimestamp
		FROM ( SELECT c.oid AS aooid,
					n.nspname AS aoschema,
					c.relname AS aorelname
				FROM pg_class c
					JOIN pg_namespace n ON c.relnamespace = n.oid
					JOIN pg_am a ON c.relam = a.oid
				WHERE a.amname in ('ao_row', 'ao_column')
					AND %s
			) aotables
		JOIN ( SELECT lo.objid,
					MAX(lo.statime) AS lastddltimestamp
				FROM pg_stat_last_operation lo
				WHERE lo.staactionname IN ('CREATE', 'ALTER', 'TRUNCATE')
				GROUP BY lo.objid
			) lastop
		ON aotables.aooid = lastop.objid`, relationAndSchemaFilterClause())

	query := ""
	if connectionPool.Version.Before("7") {
		query = before7Query
	} else {
		query = atLeast7Query
	}

	var results []struct {
		AOTableFQN       string
		LastDDLTimestamp string
	}
	err := connectionPool.Select(&results, query)
	gplog.FatalOnError(err)
	resultMap := make(map[string]string)
	for _, result := range results {
		resultMap[result.AOTableFQN] = result.LastDDLTimestamp
	}
	return resultMap
}
