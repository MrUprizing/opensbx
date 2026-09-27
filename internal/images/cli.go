package images

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"

	v1 "github.com/google/go-containerregistry/pkg/v1"
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

// CLI needs neither a runtime selector nor a runtime connection. Flags precede
// positional arguments, following Go's flag package convention.
func CLI(ctx context.Context, args []string, dataDir string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || isHelp(args[0]) {
		return writeImageUsage(out)
	}
	fs := flag.NewFlagSet("image "+args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { _ = writeImageUsage(out) }
	dir := fs.String("data-dir", dataDir, "Local data directory")
	platform := fs.String("platform", "linux/"+runtime.GOARCH, "Explicit target platform")
	ref := fs.String("reference", "", "Reference for a single imported archive root")
	output := fs.String("output", "", "Export archive destination (must not exist)")
	full := fs.Bool("all-platforms", false, "Export complete root index; fails if any blob is missing")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	p, err := v1.ParsePlatform(*platform)
	if err != nil {
		return err
	}
	if p.OS == "" || p.Architecture == "" {
		return errors.New("platform must specify OS and architecture")
	}
	if args[0] != "list" && fs.NArg() != 1 {
		return errors.New("exactly one reference or archive is required")
	}
	if args[0] == "list" && fs.NArg() != 0 {
		return errors.New("list takes no arguments")
	}
	s, err := Open(*dir)
	if err != nil {
		return err
	}
	arg := fs.Arg(0)
	switch args[0] {
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
		return s.Import(ctx, arg, *ref, *p)
	case "export":
		if *output == "" {
			return errors.New("export requires --output")
		}
		return s.Export(ctx, arg, *output, *p, *full)
	case "remove":
		return s.Remove(ctx, arg, false)
	default:
		return fmt.Errorf("unknown image command %q; use opensbx image help", args[0])
	}
}

func isHelp(arg string) bool {
	switch arg {
	case "-h", "-help", "--h", "--help":
		return true
	default:
		return false
	}
}

func writeImageUsage(out io.Writer) error {
	_, err := fmt.Fprintln(out, "Usage: opensbx image <list|inspect|pull|import|export|remove> [options] [reference/archive]\nOptions: --data-dir PATH --platform linux/arch[/variant]\nImport: --reference NAME archive.tar\nExport: --output PATH [--all-platforms] reference\nPull explicitly downloads the selected platform. Import/export use OCI archives; no runtime is contacted.")
	return err
}
