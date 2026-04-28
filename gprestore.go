// +build gprestore

package main

import (
	"os"

	"github.com/greenplum-db/gpbackup/options"
	. "github.com/greenplum-db/gpbackup/restore"
	"github.com/spf13/cobra"
)

func main() {
	var rootCmd = &cobra.Command{
		Use:     "gprestore",
		Short:   "gprestore is the parallel restore utility for Greenplum",
		Args:    cobra.NoArgs,
		Version: GetVersion(),
		Run: func(cmd *cobra.Command, args []string) {
			// Handle management commands (--list-backups, --delete-backup)
			if HandleManageCommands() {
				return
			}
			defer DoTeardown()
			DoValidation(cmd)
			DoSetup()
			DoRestore()
		}}
	rootCmd.SetArgs(options.HandleSingleDashes(os.Args[1:]))
	DoInit(rootCmd)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(2)
	}
}
