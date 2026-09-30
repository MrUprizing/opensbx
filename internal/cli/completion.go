package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"opensbx/internal/client"
	"opensbx/models"

	"github.com/spf13/cobra"
)

func newCompletionCommand(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{Use: "completion", Short: "Generate shell completion (never installs it)", GroupID: "other", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		cmd.AddCommand(&cobra.Command{Use: shell, Short: "Print native Cobra " + shell + " completion", Args: cobra.NoArgs,
			Example: "  opensbx completion " + shell,
			RunE: func(cmd *cobra.Command, _ []string) error {
				switch shell {
				case "bash":
					return root.GenBashCompletionV2(cmd.OutOrStdout(), true)
				case "zsh":
					return root.GenZshCompletion(cmd.OutOrStdout())
				default:
					return root.GenFishCompletion(cmd.OutOrStdout(), true)
				}
			},
		})
	}
	return cmd
}

// Pinned Cobra performs a synthetic -- parse before a real completion parse;
// pinned pflag retains stale ArgsLenAtDash in that second parse. Capture only the
// actual completed separator boundary here, never dispatch or parse user flags.
func completionAfterSeparator(args []string) bool {
	if len(args) < 2 {
		return false
	}
	request := false
	for _, arg := range args[:len(args)-1] {
		if arg == cobra.ShellCompRequestCmd || arg == cobra.ShellCompNoDescRequestCmd {
			request = true
		}
		if request && arg == "--" {
			return true
		}
	}
	return false
}

func fixedCompletions(values ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		var result []string
		for _, value := range values {
			if strings.HasPrefix(value, prefix) {
				result = append(result, value)
			}
		}
		return result, cobra.ShellCompDirectiveNoFileComp
	}
}

func resourceCompletions(spec commandSpec, afterSeparator bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		directive := cobra.ShellCompDirectiveNoFileComp
		if afterSeparator || len(args) != 0 || spec.min == 0 || spec.name == "create" {
			return nil, directive
		}
		addr, _ := cmd.Flags().GetString("addr")
		c, err := client.New(addr, os.Getenv("API_KEY"))
		if err != nil {
			return nil, directive
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 300*time.Millisecond)
		defer cancel()
		var response struct {
			Sandboxes []models.SandboxSummary `json:"sandboxes"`
		}
		if c.Request(ctx, "GET", client.Path("sandboxes"), nil, &response) != nil {
			return nil, directive
		}
		var choices []string
		for _, item := range response.Sandboxes {
			for _, value := range []string{item.ID, item.Name} {
				if safeSuggestion(value) && strings.HasPrefix(value, prefix) {
					choices = append(choices, value)
				}
			}
		}
		return choices, directive
	}
}

func safeSuggestion(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.'
	}) == -1
}

// Complete is a compatibility entrypoint to Cobra's native completion protocol,
// including its final directive record. It does not generate or eval shell code.
func Complete(args []string, out io.Writer) error {
	if len(args) == 0 {
		args = []string{""}
	}
	return Run(context.Background(), append([]string{cobra.ShellCompNoDescRequestCmd}, args...), strings.NewReader(""), out, io.Discard)
}
