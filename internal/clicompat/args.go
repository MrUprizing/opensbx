// Package clicompat normalizes only explicitly retained Go-style long flags.
// Cobra/pflag still own command dispatch, parsing and validation.
package clicompat

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Args preserves values and everything from the first literal -- onward. It
// recognizes flag arity from the actual Cobra tree only to avoid rewriting data.
func Args(root *cobra.Command, args []string) ([]string, error) {
	result := append([]string(nil), args...)
	current := root
	for i := 0; i < len(result); i++ {
		arg := result[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			for _, child := range current.Commands() {
				if child.Name() == arg || child.HasAlias(arg) {
					current = child
					break
				}
			}
			continue
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		current.InitDefaultHelpFlag()
		lookup := func(name string) *pflag.Flag {
			if f := current.Flags().Lookup(name); f != nil {
				return f
			}
			if f := current.PersistentFlags().Lookup(name); f != nil {
				return f
			}
			return current.InheritedFlags().Lookup(name)
		}
		var f *pflag.Flag
		if strings.HasPrefix(arg, "--") {
			f = lookup(name)
		} else if retained(name) && lookup(name) != nil {
			result[i] = "-" + arg
			f = lookup(name)
		} else if len(arg) >= 2 {
			f = current.Flags().ShorthandLookup(arg[1:2])
			if f == nil {
				f = current.InheritedFlags().ShorthandLookup(arg[1:2])
			}
			assigned = assigned || len(arg) > 2
		}
		if f != nil && f.NoOptDefVal == "" && !assigned && i+1 < len(result) {
			if result[i+1] == "--" {
				// pflag otherwise consumes the separator as a value and can mistake
				// intended guest --help for CLI help. This is only a separator guard,
				// not value parsing; explicit --option=-- remains valid.
				return nil, fmt.Errorf("flag --%s requires a value before --; use --%s=-- for a literal separator value", f.Name, f.Name)
			}
			i++
		}
	}
	return result, nil
}

func retained(name string) bool {
	switch name {
	case "help", "runtime", "addr", "data-dir", "log-file", "legacy-db", "platform", "reference", "output", "all-platforms", "force":
		return true
	}
	return false
}
