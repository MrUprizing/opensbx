package cli

import (
	"errors"
	"io"
	"os"

	"opensbx/internal/config"

	"github.com/spf13/cobra"
)

type serverOptions struct{ runtime, dataDir, logFile, legacyDB string }

func bindServerOptions(cmd *cobra.Command) *serverOptions {
	o := &serverOptions{}
	fs := cmd.Flags()
	fs.StringVar(&o.runtime, "runtime", "", "Runtime: docker or container (Apple)")
	fs.StringVar(&o.dataDir, "data-dir", config.DefaultDataDir(), "Local image catalog and server state")
	logFile := os.Getenv("LOG_FILE")
	if logFile == "" {
		logFile = "opensbx.log"
	}
	fs.StringVar(&o.logFile, "log-file", logFile, "Server log file")
	fs.StringVar(&o.legacyDB, "legacy-db", "", "Explicit runtime-matching legacy execution database")
	_ = cmd.RegisterFlagCompletionFunc("runtime", fixedCompletions("docker", "container"))
	_ = cmd.MarkFlagDirname("data-dir")
	_ = cmd.MarkFlagFilename("log-file")
	_ = cmd.MarkFlagFilename("legacy-db")
	return o
}

func (o *serverOptions) arguments(cmd *cobra.Command) []string {
	addr, _ := cmd.Flags().GetString("addr")
	return []string{"--addr", addr, "--runtime", o.runtime, "--data-dir", o.dataDir, "--log-file", o.logFile, "--legacy-db", o.legacyDB}
}

func newDaemonCommand(name string, hook func([]string, io.Writer) error, streams streams) *cobra.Command {
	short := "Start the API/MCP daemon in the background"
	long := "Start the server in the background. No sandbox resource is selected; use sandbox start TARGET for resources."
	if name == "stop" {
		short = "Send a graceful shutdown to the daemon"
		long = "Send a graceful shutdown to the server for this data directory. Never interpreted as sandbox stop by argument count; use sandbox stop TARGET for resources."
	}
	cmd := &cobra.Command{Use: name, Short: short, Long: long, GroupID: "daemon", Args: cobra.NoArgs, Example: "  opensbx " + name + " --data-dir /path/to/state"}
	o := bindServerOptions(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if hook == nil {
			return errors.New("daemon operations are available only through the opensbx binary")
		}
		return hook(o.arguments(cmd), streams.out)
	}
	return cmd
}
