package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"

	"opensbx/internal/clicompat"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"
)

type cliImageSummary struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	Size int64    `json:"size"`
}
type cliImageDetail struct {
	cliImageSummary
	Created      string `json:"created"`
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type imageOptions struct {
	dir, platform, reference, output string
	all, force                       bool
}

// NewCommand owns the offline image subtree. No store is opened during command
// construction, help or completion generation.
func NewCommand(dataDir string) *cobra.Command {
	o := &imageOptions{}
	cmd := &cobra.Command{
		Use: "image", Short: "Manage the standalone local OCI image catalog",
		Long:         "Manage the local OCI image catalog offline; no runtime is contacted. Pulls are explicit. Existing list/inspect output is JSON.",
		SilenceUsage: true, SilenceErrors: true,
		Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	fs := cmd.PersistentFlags()
	fs.StringVar(&o.dir, "data-dir", dataDir, "Local data directory")
	fs.StringVar(&o.platform, "platform", "linux/"+runtime.GOARCH, "Target OS/architecture[/variant]")
	fs.StringVar(&o.reference, "reference", "", "Reference for a single imported archive root")
	fs.StringVar(&o.output, "output", "", "Export destination (must not exist)")
	fs.BoolVar(&o.all, "all-platforms", false, "Export complete root index; fails if a blob is missing")
	fs.BoolVar(&o.force, "force", false, "Remove: ignore missing reference, retain native images/pinned blobs")
	_ = cmd.MarkPersistentFlagFilename("output")
	_ = cmd.MarkPersistentFlagDirname("data-dir")
	for _, action := range []string{"pull", "list", "inspect", "remove", "import", "export"} {
		use := action + " REFERENCE"
		args := cobra.ExactArgs(1)
		if action == "list" {
			use = action
			args = cobra.NoArgs
		}
		if action == "import" {
			use = action + " ARCHIVE"
		}
		leaf := &cobra.Command{
			Use: use, Short: "OCI catalog " + action, Args: args,
			Example: "  opensbx image " + use,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runImage(cmd.Context(), action, args, o, cmd.OutOrStdout())
			},
		}
		switch action {
		case "list":
			leaf.Example = "  opensbx image list"
		case "import":
			leaf.Example = "  opensbx image import node.tar --reference node:22"
		case "export":
			leaf.Example = "  opensbx image export node:22 --output node.tar"
		default:
			leaf.Example = "  opensbx image " + action + " node:22"
		}
		if action != "import" {
			leaf.ValidArgsFunction = cobra.NoFileCompletions
		}
		cmd.AddCommand(leaf)
	}
	cmd.AddCommand(&cobra.Command{Use: "help [COMMAND]", Short: "Show image command help", Hidden: true, Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		target, remaining, err := cmd.Find(args)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return fmt.Errorf("unknown image command %q", remaining[0])
		}
		return target.Help()
	}})
	return cmd
}

// CLI preserves the standalone Go entrypoint while delegating tree/parsing/help
// entirely to Cobra. The binary mounts the same image subtree in its main tree.
func CLI(ctx context.Context, args []string, dataDir string, out io.Writer) error {
	root := &cobra.Command{Use: "opensbx", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(NewCommand(dataDir))
	output := &clicompat.Output{Writer: out}
	root.SetOut(output)
	root.SetErr(out)
	root.SetUsageTemplate("Usage: {{.UseLine}}\n{{if .HasAvailableSubCommands}}\nCommands:\n{{range .Commands}}{{if .IsAvailableCommand}}  {{.Name}}\t{{.Short}}\n{{end}}{{end}}{{end}}{{if .HasAvailableInheritedFlags}}\nOptions:\n{{.InheritedFlags.FlagUsages}}{{end}}{{if .HasAvailableLocalFlags}}{{.LocalFlags.FlagUsages}}{{end}}")
	call := append([]string{"image"}, args...)
	normalized, err := clicompat.Args(root, call)
	if err != nil {
		return err
	}
	root.SetArgs(normalized)
	if err := root.ExecuteContext(ctx); err != nil {
		return err
	}
	return output.Err
}

func runImage(ctx context.Context, action string, args []string, o *imageOptions, out io.Writer) error {
	p, err := v1.ParsePlatform(o.platform)
	if err != nil {
		return err
	}
	if p.OS == "" || p.Architecture == "" {
		return errors.New("platform must specify OS and architecture")
	}
	if action == "export" && o.output == "" {
		return errors.New("export requires --output")
	}
	s, err := Open(o.dir)
	if err != nil {
		return err
	}
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	switch action {
	case "list":
		items, err := s.List(ctx)
		if err != nil {
			return err
		}
		result := make([]cliImageSummary, 0, len(items))
		for _, item := range items {
			result = append(result, cliImageSummary{string(item.ID), item.Tags, item.Size})
		}
		return json.NewEncoder(out).Encode(result)
	case "inspect":
		item, err := s.Inspect(ctx, arg, *p)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(cliImageDetail{cliImageSummary: cliImageSummary{string(item.ID), item.Tags, item.Size}, Created: item.Created, Architecture: item.Architecture, OS: item.OS})
	case "pull":
		return s.Pull(ctx, arg, *p)
	case "import":
		return s.Import(ctx, arg, o.reference, *p)
	case "export":
		return s.Export(ctx, arg, o.output, *p, o.all)
	case "remove":
		return s.Remove(ctx, arg, o.force)
	}
	return errors.New("unsupported image action")
}
