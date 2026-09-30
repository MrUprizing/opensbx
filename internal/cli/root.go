package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"opensbx/internal/clicompat"
	"opensbx/internal/client"
	"opensbx/internal/config"
	"opensbx/internal/images"
	"opensbx/internal/terminaltext"

	"github.com/spf13/cobra"
)

// ServerHooks keep daemon/runtime construction in the binary, not in the CLI.
// Arguments passed to hooks are serialized parsed server options, never guest argv.
type ServerHooks struct {
	Foreground func([]string, io.Writer) error
	Start      func([]string, io.Writer) error
	Stop       func([]string, io.Writer) error
}

type streams struct {
	in          io.Reader
	out, stderr io.Writer
}

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) error {
	return Execute(ctx, args, in, out, stderr, ServerHooks{})
}

// Execute builds fresh Cobra nodes and flag bindings for every invocation.
func Execute(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, hooks ServerHooks) error {
	io := streams{in, out, stderr}
	output := &clicompat.Output{Writer: out}
	io.out = output
	root := newRoot(io, hooks, completionAfterSeparator(args))
	normalized, err := clicompat.Args(root, args)
	if err != nil {
		return err
	}
	root.SetArgs(normalized)
	if err := root.ExecuteContext(ctx); err != nil {
		return err
	}
	return output.Err
}

func newRoot(io streams, hooks ServerHooks, afterSeparator bool) *cobra.Command {
	root := &cobra.Command{
		Use: "opensbx", Short: "Disposable local sandboxes",
		Long:         "OpenSBX runs disposable local sandboxes. Management uses loopback HTTP; no automatic server startup, image pull or remote mode. API_KEY supplies the optional Bearer token. No arguments runs the foreground server.",
		SilenceUsage: true, SilenceErrors: true, Args: cobra.NoArgs,
		Example: "  opensbx image pull node:22\n  opensbx start\n  opensbx create node:22 --port 3000 --ttl 15m\n  opensbx exec SANDBOX -- node --version",
	}
	root.SetIn(io.in)
	root.SetOut(io.out)
	root.SetErr(terminaltext.LayoutWriter(io.stderr))
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = client.DefaultAddr
	}
	root.PersistentFlags().String("addr", addr, "Loopback API host:port (ADDR)")
	root.PersistentFlags().Bool("json", false, "Structured management output")
	root.PersistentFlags().BoolP("quiet", "q", false, "IDs only: create, sandbox list or detached exec")
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddGroup(&cobra.Group{ID: "common", Title: "Common commands:"}, &cobra.Group{ID: "resources", Title: "Resource groups:"}, &cobra.Group{ID: "daemon", Title: "Daemon commands:"}, &cobra.Group{ID: "other", Title: "Other commands:"})
	root.SetHelpCommandGroupID("other")
	// Use the native grouped/default template, with a compact first usage line.
	root.SetUsageTemplate(strings.Replace(root.UsageTemplate(), "Usage:{{if .Runnable}}\n  ", "Usage:{{if .Runnable}} ", 1))
	nativeHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		out := cmd.OutOrStdout()
		cmd.SetOut(terminaltext.LayoutWriter(out))
		defer cmd.SetOut(out)
		nativeHelp(cmd, args)
	})
	server := bindServerOptions(root)
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := rejectManagementOutput(cmd); err != nil {
			return err
		}
		if hooks.Foreground == nil {
			return cmd.Help()
		}
		return hooks.Foreground(server.arguments(cmd), io.out)
	}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Annotations["management"] != "true" {
			return rejectManagementOutput(cmd)
		}
		return nil
	}
	groups := map[string]*cobra.Command{}
	for _, name := range []string{"sandbox", "command", "file"} {
		group := &cobra.Command{Use: name, Short: "Manage " + name + " resources", GroupID: "resources", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }, Long: "Manage " + name + " resources. TARGET is an exact name, full ID or unique ID prefix; ambiguity is an error."}
		groups[name] = group
		root.AddCommand(group)
	}
	for _, spec := range commands {
		leaf := newLeaf(spec, spec.name, io, afterSeparator)
		if spec.group == "" {
			leaf.GroupID = "other"
			root.AddCommand(leaf)
		} else {
			leaf.Example = leafExample(spec, spec.group+" "+spec.name)
			groups[spec.group].AddCommand(leaf)
		}
		if spec.alias != "" {
			alias := newLeaf(spec, spec.alias, io, afterSeparator)
			alias.GroupID = "common"
			root.AddCommand(alias)
		}
	}
	image := images.NewCommand(config.DefaultDataDir())
	image.GroupID = "resources"
	root.AddCommand(image)
	root.AddCommand(newDaemonCommand("start", hooks.Start, io), newDaemonCommand("stop", hooks.Stop, io), newCompletionCommand(root))
	return root
}

func newLeaf(spec commandSpec, name string, io streams, afterSeparator bool) *cobra.Command {
	cmd := &cobra.Command{
		Use: strings.TrimSpace(name + " " + spec.usage), Short: spec.description,
		Annotations: map[string]string{"management": "true"},
		Example:     leafExample(spec, name),
		Args: func(cmd *cobra.Command, args []string) error {
			if spec.name == "exec" {
				if cmd.ArgsLenAtDash() != 1 || len(args) < 2 || args[1] == "" {
					return errors.New("exec requires TARGET [options] -- PROGRAM ARGS...; flags need values before the separator")
				}
				return nil
			}
			return cobra.RangeArgs(spec.min, spec.max)(cmd, args)
		},
	}
	o := bindOptions(cmd, spec)
	if spec.name == "create" {
		cmd.Long = spec.description + " Defaults: 1 CPU, 1 GiB memory, 15-minute TTL. Limits: 4 CPUs and 8 GiB. Image pulls are explicit; no runtime/server is started by create."
	}
	if spec.name == "exec" {
		cmd.DisableFlagsInUseLine = true
		cmd.Long = spec.description + " Foreground streams stdout/stderr and returns the confirmed guest exit code. --json captures at most 8 MiB total; --quiet requires --detach. Everything after literal -- is guest argv, not CLI flags."
	}
	cmd.ValidArgsFunction = resourceCompletions(spec, afterSeparator)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		o.addr, _ = cmd.Flags().GetString("addr")
		o.json, _ = cmd.Flags().GetBool("json")
		o.quiet, _ = cmd.Flags().GetBool("quiet")
		var program []string
		if spec.name == "exec" {
			dash := cmd.ArgsLenAtDash()
			program = args[dash:]
			args = args[:dash]
		}
		return runOperation(cmd.Context(), spec, args, program, o, io.in, io.out, io.stderr)
	}
	return cmd
}

func leafExample(spec commandSpec, name string) string {
	path := name
	switch spec.name {
	case "create":
		return "  opensbx " + path + " node:22 --port 3000 --ttl 15m --memory 1GiB --cpus 2"
	case "exec":
		return "  opensbx " + path + " TARGET -- node --version\n  opensbx " + path + " TARGET --detach --quiet -- node server.js"
	case "logs":
		return "  opensbx " + path + " TARGET COMMAND_ID --follow"
	case "renew":
		return "  opensbx " + path + " TARGET --ttl 30m"
	case "kill":
		return "  opensbx " + path + " TARGET COMMAND_ID --signal TERM"
	}
	if spec.group == "file" {
		if spec.name == "write" {
			return "  printf 'hello' | opensbx file write TARGET /tmp/message.txt\n  opensbx file write TARGET /tmp/script.js --input ./script.js"
		}
		return "  opensbx " + path + " TARGET /tmp"
	}
	return "  opensbx " + strings.TrimSpace(path+" "+spec.usage)
}

func rejectManagementOutput(cmd *cobra.Command) error {
	for _, name := range []string{"json", "quiet"} {
		if value, _ := cmd.Flags().GetBool(name); value {
			return fmt.Errorf("--%s is a management option, not supported for %s", name, cmd.CommandPath())
		}
	}
	return nil
}

// These convenience entrypoints delegate to Cobra, with no second registry/parser.
func RootHelp(out io.Writer) error {
	return Run(context.Background(), []string{"--help"}, strings.NewReader(""), out, io.Discard)
}
func Help(args []string, out io.Writer) error {
	return Run(context.Background(), append([]string{"help"}, args...), strings.NewReader(""), out, io.Discard)
}
