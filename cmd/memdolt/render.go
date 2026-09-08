package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func newRenderCommand() *cobra.Command {
	var flags storeFlags
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render committed main to PROJECT.md and PROJECT_LEDGER.md",
		Long: "Render one committed main snapshot through the direct or authenticated owner.\n" +
			"Read [render] output_dir from .memdolt/config.toml; default .memdolt/rendered.\n" +
			"Relative paths stay within the repository; explicit absolute local paths are\n" +
			"allowed. Symlinks/reparse points, protected metadata/index locations and\n" +
			"unmarked same-name user files are refused. Custom outputs need your own ignore rule.\n" +
			"Prepare both complete files and backups in .memdolt/backups/rendered first,\n" +
			"then replace each output separately. The pair is not transactionally atomic.\n" +
			"Report partial completion and retained backups on failure; inspect before retry.\n" +
			"A crash may leave .memdolt-render.lock in the output directory: stop all\n" +
			"renders and inspect outputs/backups before removing that residue.\n" +
			"A live MCP owner flushes its pending notes before capturing committed main.\n" +
			"Flush errors prevent publication; confirmed groups are never replayed.\n" +
			"noteCommits maps each note id flushed by this call to its confirmed hash,\n" +
			"including when later configuration, snapshot, file or reporting work fails.\n" +
			"Inspect note list/history on confirmed or unknown errors as well as outputs.\n" +
			"Without a session queue, render reads committed main without a Dolt commit.\n" +
			"Proposals remain excluded. No initialization, migration, transcript archive\n" +
			"or token accounting is performed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
				return err
			}
			st, err := openCommandStore(cmd.Context(), flags.dir, cliActor)
			if err != nil {
				return err
			}
			return runRender(cmd, st)
		},
	}
	return flags.bind(cmd)
}

func runRender(cmd *cobra.Command, st commandStore) error {
	result, err := st.Render(cmd.Context())
	err = errors.Join(err, result.NoteCommitError(st.Close()))
	if err != nil {
		result.Error = err.Error()
	}
	lines := []string{"render: " + result.Status, "source main: " + result.SourceCommit,
		fmt.Sprintf("source schema: v%d", result.SchemaVersion), "output directory: " + result.OutputDir}
	for _, path := range result.WrittenFiles {
		lines = append(lines, "written: "+path)
	}
	for _, path := range result.BackupFiles {
		lines = append(lines, "recoverable backup: "+path)
	}
	if len(result.NoteCommits) != 0 {
		lines = append(lines, fmt.Sprintf("confirmed note commits (id:hash): %v", result.NoteCommits))
	}
	if result.Error != "" {
		lines = append(lines, "error: "+result.Error)
	}
	if outputErr := emit(cmd, result, lines); outputErr != nil {
		err = errors.Join(err, result.NoteCommitError(fmt.Errorf("render output reporting failed after status %s, written %v, backups %v; inspect before retrying: %w",
			result.Status, result.WrittenFiles, result.BackupFiles, outputErr)))
	}
	return err
}
