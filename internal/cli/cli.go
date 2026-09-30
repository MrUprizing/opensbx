// Package cli translates local command-line operations into loopback HTTP requests.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"opensbx/internal/client"
	"opensbx/internal/terminaltext"
	"opensbx/models"
)

type commandSpec struct {
	group, name, alias, usage, description string
	min, max                               int
}

var commands = []commandSpec{
	{"sandbox", "create", "create", "IMAGE", "Create from an explicitly prepared local image.", 1, 1},
	{"sandbox", "list", "ls", "", "List owned sandboxes.", 0, 0},
	{"sandbox", "inspect", "inspect", "TARGET", "Inspect by exact name, full ID or unique ID prefix.", 1, 1},
	{"command", "exec", "exec", "TARGET [options] -- PROGRAM ARGS...", "Run in the foreground, or return immediately with --detach.", 1, 1},
	{"command", "logs", "logs", "TARGET COMMAND_ID [--follow]", "Read retained command logs; snapshot by default.", 2, 2},
	{"sandbox", "remove", "rm", "TARGET", "Remove an owned sandbox regardless of state.", 1, 1},
	{"sandbox", "start", "", "TARGET", "Start a sandbox, not the daemon.", 1, 1},
	{"sandbox", "stop", "", "TARGET", "Stop a sandbox, not the daemon.", 1, 1},
	{"sandbox", "restart", "", "TARGET", "Restart a sandbox.", 1, 1},
	{"sandbox", "pause", "", "TARGET", "Pause sandbox processes (runtime support required).", 1, 1},
	{"sandbox", "resume", "", "TARGET", "Resume sandbox processes.", 1, 1},
	{"sandbox", "renew", "", "TARGET --ttl DURATION", "Renew the server-owned expiration deadline.", 1, 1},
	{"sandbox", "stats", "", "TARGET", "Read a resource usage snapshot.", 1, 1},
	{"sandbox", "network", "", "TARGET", "Read proxy and loopback port mappings.", 1, 1},
	{"command", "list", "", "TARGET", "List command history within a sandbox.", 1, 1},
	{"command", "inspect", "", "TARGET COMMAND_ID", "Inspect an exact command ID.", 2, 2},
	{"command", "wait", "", "TARGET COMMAND_ID", "Wait for a confirmed exit code; interruption only detaches.", 2, 2},
	{"command", "kill", "", "TARGET COMMAND_ID [--signal TERM]", "Signal only the selected, verified guest process.", 2, 2},
	{"file", "read", "", "TARGET PATH", "Read UTF-8 text without adding a newline.", 2, 2},
	{"file", "write", "", "TARGET PATH [--input LOCAL_PATH]", "Write exact UTF-8 text from redirected stdin or a local file.", 2, 2},
	{"file", "list", "", "TARGET [PATH]", "List a guest directory (default /).", 1, 2},
	{"file", "remove", "", "TARGET PATH", "Remove a guest file or directory using backend semantics.", 2, 2},
	{"", "health", "", "", "Check the configured local server and runtime.", 0, 0},
}

// ExitError carries a guest or interrupt exit status without printing a fake runtime error.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// runOperation handles already-parsed/validated resource arguments. Both root
// shortcuts and grouped Cobra leaves call this same operation.
func runOperation(ctx context.Context, spec commandSpec, pos, program []string, o *options, in io.Reader, out, stderr io.Writer) error {
	if o.quiet && !(spec.name == "create" || spec.name == "exec" || spec.group == "sandbox" && spec.name == "list") {
		return errors.New("--quiet is supported only for create, sandbox list and detached exec")
	}
	if o.json && o.quiet {
		return errors.New("--json and --quiet are incompatible")
	}
	if o.follow && o.json {
		return errors.New("--json and --follow are incompatible; use a JSON snapshot or follow raw text")
	}
	if spec.name == "exec" && (len(program) == 0 || program[0] == "") {
		return errors.New("exec requires -- PROGRAM ARGS... (everything after -- is passed unchanged)")
	}
	if o.quiet && spec.name == "exec" && !o.detach {
		return errors.New("exec --quiet requires --detach")
	}
	// Validate local inputs before making any management request.
	ttl, err := durationSeconds(o.ttl, spec.name == "renew")
	if err != nil {
		return err
	}
	var resources *models.ResourceLimits
	if o.memory != "" || o.cpus != "" {
		resources = &models.ResourceLimits{}
		if o.memory != "" {
			resources.Memory, err = memoryMiB(o.memory)
			if err != nil {
				return err
			}
		}
		if o.cpus != "" {
			resources.CPUs, err = cpuLimit(o.cpus)
			if err != nil {
				return err
			}
		}
	}
	env, err := environment(o.env)
	if err != nil {
		return err
	}
	for _, port := range o.ports {
		number, proto, hasProtocol := strings.Cut(port, "/")
		n, err := strconv.ParseUint(number, 10, 16)
		if err != nil || n == 0 || hasProtocol && proto != "tcp" && proto != "udp" {
			return errors.New("--port must be a guest port from 1 to 65535, optionally /tcp or /udp")
		}
	}
	var sig int
	if spec.name == "kill" {
		sig, err = signalNumber(o.signal)
		if err != nil {
			return err
		}
	}
	var text string
	if spec.group == "file" && spec.name == "write" {
		text, err = readText(in, o.input)
		if err != nil {
			return err
		}
	}
	c, err := client.New(o.addr, os.Getenv("API_KEY"))
	if err != nil {
		return err
	}
	defer c.Close()
	if spec.group == "" {
		var value any
		if err := c.Request(ctx, "GET", client.Path("health"), nil, &value); err != nil {
			return err
		}
		return output(out, value, o.json)
	}
	if spec.group == "sandbox" && spec.name == "create" {
		var value models.CreateSandboxResponse
		err := c.Request(ctx, "POST", client.Path("sandboxes"), models.CreateSandboxRequest{Image: pos[0], Ports: o.ports, Timeout: ttl, Resources: resources, Env: o.env}, &value)
		if err != nil {
			return err
		}
		if value.ID == "" {
			return errors.New("create response is missing sandbox ID")
		}
		if o.quiet {
			_, err = fmt.Fprintln(out, terminaltext.Escape(value.ID, false))
			return err
		}
		return output(out, value, o.json)
	}
	if spec.group == "sandbox" && spec.name == "list" {
		var value struct {
			Sandboxes []models.SandboxSummary `json:"sandboxes"`
		}
		if err := c.Request(ctx, "GET", client.Path("sandboxes"), nil, &value); err != nil {
			return err
		}
		if o.json {
			return output(out, value, true)
		}
		for _, item := range value.Sandboxes {
			var err error
			if o.quiet {
				_, err = fmt.Fprintln(out, terminaltext.Escape(item.ID, false))
			} else {
				_, err = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", terminaltext.Escape(item.ID, false), terminaltext.Escape(item.Name, false), terminaltext.Escape(item.Image, false), terminaltext.Escape(item.Status, false), terminaltext.Escape(item.URL, false))
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	id, err := c.Resolve(ctx, pos[0])
	if err != nil {
		return err
	}
	base := client.Path("sandboxes", id)
	if spec.group == "command" {
		return command(ctx, c, spec.name, id, pos, program, env, sig, o, out, stderr)
	}
	var method, path string
	var body any
	if spec.group == "file" {
		guestPath := "/"
		if len(pos) > 1 {
			guestPath = pos[1]
		}
		path = base + "/files"
		if spec.name == "list" {
			path += "/list"
		}
		path += "?" + url.Values{"path": {guestPath}}.Encode()
		switch spec.name {
		case "read", "list":
			method = "GET"
		case "write":
			method = "PUT"
			body = models.FileWriteRequest{Content: text}
		case "remove":
			method = "DELETE"
		}
	} else {
		path = base
		method = "POST"
		switch spec.name {
		case "inspect":
			method = "GET"
		case "remove":
			method = "DELETE"
		case "stats", "network":
			method = "GET"
			path += "/" + spec.name
		case "renew":
			path += "/renew-expiration"
			body = models.RenewExpirationRequest{Timeout: ttl}
		default:
			path += "/" + spec.name
		}
	}
	var result any
	if err := c.Request(ctx, method, path, body, &result); err != nil {
		return err
	}
	if spec.group == "file" && !o.json && (spec.name == "read" || spec.name == "list") {
		key := "content"
		if spec.name == "list" {
			key = "output"
		}
		value, ok := result.(map[string]any)
		if !ok {
			return errors.New("invalid file response")
		}
		data, ok := value[key].(string)
		if !ok {
			return errors.New("file response is missing text")
		}
		if spec.name == "list" {
			data = terminaltext.Escape(data, true)
		}
		_, err := io.WriteString(out, data)
		return err
	}
	if method == http.MethodDelete {
		if spec.group == "file" {
			result = map[string]string{"path": pos[1], "status": "removed"}
		} else {
			result = map[string]string{"id": id, "status": "removed"}
		}
	}
	return output(out, result, o.json)
}

func readText(in io.Reader, input string) (string, error) {
	if input != "" {
		file, err := os.Open(input)
		if err != nil {
			return "", fmt.Errorf("--input: %w", err)
		}
		defer file.Close()
		in = file
	} else if file, ok := in.(*os.File); ok && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd())) {
		return "", errors.New("file write requires redirected UTF-8 stdin or --input PATH; example: printf 'hello' | opensbx file write TARGET /work/file")
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", errors.New("file write accepts UTF-8 text only, not arbitrary binary data")
	}
	return string(data), nil
}

func output(out io.Writer, value any, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var object any
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	return human(out, object, "")
}

func human(out io.Writer, value any, indent string) error {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, err := fmt.Fprintf(out, "%s%s: ", indent, terminaltext.Escape(key, false)); err != nil {
				return err
			}
			switch v[key].(type) {
			case map[string]any, []any:
				if _, err := fmt.Fprintln(out); err != nil {
					return err
				}
				if err := human(out, v[key], indent+"  "); err != nil {
					return err
				}
			default:
				if _, err := fmt.Fprintln(out, terminaltext.Escape(fmt.Sprint(v[key]), false)); err != nil {
					return err
				}
			}
		}
		return nil
	case []any:
		for _, item := range v {
			if err := human(out, item, indent); err != nil {
				return err
			}
		}
		return nil
	default:
		_, err := fmt.Fprintf(out, "%s%s\n", indent, terminaltext.Escape(fmt.Sprint(value), false))
		return err
	}
}
