// +build gpbackup

package main

import (
	"os"
	"strings"

	. "github.com/greenplum-db/gpbackup/backup"
	"github.com/greenplum-db/gpbackup/options"
	"github.com/spf13/cobra"
)

func main() {
	var rootCmd = &cobra.Command{
		Use:     "gpbackup",
		Short:   "gpbackup is the parallel backup utility for Greenplum",
		Args:    cobra.NoArgs,
		Version: GetVersion(),
		Run: func(cmd *cobra.Command, args []string) {
			// Handle management commands (--list-backups, --delete-backup)
			// These don't require a database connection.
			if HandleManageCommands() {
				return
			}
			defer DoTeardown()
			DoFlagValidation(cmd)
			DoSetup()
			DoBackup()
		}}
	args := options.HandleSingleDashes(os.Args[1:])
	rootCmd.SetArgs(args)
	DoInit(rootCmd)
	// For management commands (--list-backups, --delete-backup), --dbname is not required.
	// Set a dummy value so cobra's required flag check passes.
	for _, arg := range args {
		if arg == "--list-backups" || strings.HasPrefix(arg, "--delete-backup") {
			_ = rootCmd.Flags().Set(options.DBNAME, "_manage_")
			break
		}
	}
	// Note: --heap-file-hash and --ao-file-hash are independent flags.
	// Either can be used alone or together.
	if err := rootCmd.Execute(); err != nil {
		os.Exit(2)
	}
}
