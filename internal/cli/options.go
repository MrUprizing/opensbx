package cli

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type options struct {
	addr, ttl, memory, cpus, cwd, input, signal string
	json, quiet, detach, follow                 bool
	ports, env                                  []string
}

func bindOptions(cmd *cobra.Command, spec commandSpec) *options {
	o := &options{}
	fs := cmd.Flags()
	if spec.name == "create" || spec.name == "renew" {
		fs.StringVar(&o.ttl, "ttl", "", "TTL duration in whole seconds, e.g. 15m")
	}
	if spec.name == "create" {
		fs.StringArrayVarP(&o.ports, "port", "p", nil, "Guest port, repeatable (e.g. 3000 or 3000/tcp)")
		fs.StringVar(&o.memory, "memory", "", "Memory with explicit units: MiB/GiB or MB/GB; whole MiB required")
		fs.StringVar(&o.cpus, "cpus", "", "Fractional CPU limit (0 through 4; 0 uses backend default)")
	}
	if spec.name == "create" || spec.name == "exec" {
		fs.StringArrayVarP(&o.env, "env", "e", nil, "Environment KEY=VALUE, repeatable (commas are literal)")
	}
	if spec.name == "exec" {
		fs.BoolVarP(&o.detach, "detach", "d", false, "Return command ID immediately")
		fs.StringVar(&o.cwd, "cwd", "", "Guest working directory")
	}
	if spec.name == "logs" {
		fs.BoolVarP(&o.follow, "follow", "f", false, "Follow retained logs until stream closes")
	}
	if spec.group == "file" && spec.name == "write" {
		fs.StringVar(&o.input, "input", "", "Read UTF-8 text from local file instead of redirected stdin")
		_ = cmd.MarkFlagFilename("input")
	}
	if spec.name == "kill" {
		fs.StringVar(&o.signal, "signal", "TERM", "POSIX signal number or name (e.g. TERM, INT, KILL)")
		_ = cmd.RegisterFlagCompletionFunc("signal", fixedCompletions("TERM", "INT", "KILL", "HUP", "QUIT", "USR1", "USR2", "STOP", "CONT"))
	}
	return o
}

func durationSeconds(value string, required bool) (int, error) {
	if value == "" && !required {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 || d%time.Second != 0 || required && d == 0 {
		return 0, errors.New("--ttl must be a nonnegative whole-second duration (renew requires > 0), e.g. 15m")
	}
	seconds := int64(d / time.Second)
	if uint64(seconds) > uint64(^uint(0)>>1) {
		return 0, errors.New("--ttl exceeds the supported integer range")
	}
	return int(seconds), nil
}

func memoryMiB(value string) (int64, error) {
	units := []struct {
		suffix string
		bytes  int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1000000000}, {"MB", 1000000}, {"KB", 1000}, {"B", 1}}
	for _, unit := range units {
		if !strings.HasSuffix(value, unit.suffix) {
			continue
		}
		number, ok := new(big.Rat).SetString(strings.TrimSuffix(value, unit.suffix))
		if !ok || number.Sign() < 0 {
			break
		}
		number.Mul(number, big.NewRat(unit.bytes, 1<<20))
		if !number.IsInt() || !number.Num().IsInt64() || number.Num().Int64() > 8192 {
			break
		}
		return number.Num().Int64(), nil
	}
	return 0, errors.New("--memory requires explicit units and an exact whole MiB from 0 to 8192 (e.g. 512MiB or 1GiB; MB/GB are decimal)")
}

func cpuLimit(value string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 4 {
		return 0, errors.New("--cpus must be a finite number from 0 to 4")
	}
	return n, nil
}

func environment(values []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsRune(value, 0) {
			return nil, errors.New("--env must be KEY=VALUE with a nonempty key and no NUL")
		}
		result[key] = val
	}
	return result, nil
}

func signalNumber(value string) (int, error) {
	name := strings.TrimPrefix(strings.ToUpper(value), "SIG")
	names := map[string]int{"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "BUS": 7, "FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15, "CHLD": 17, "CONT": 18, "STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22, "URG": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28, "IO": 29, "SYS": 31}
	if n, ok := names[name]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 64 {
		return 0, errors.New("--signal must be a familiar POSIX name or number from 1 to 64")
	}
	return n, nil
}
