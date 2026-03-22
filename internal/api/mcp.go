package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"open-sandbox/models"
)

// NewMCPHandler returns a streamable HTTP MCP handler mounted under /v1/mcp.
func NewMCPHandler(d DockerClient, baseDomain, proxyAddr string, disableLocalhostProtection bool) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "open-sandbox",
		Version: "1.0.0",
	}, &mcp.ServerOptions{Instructions: mcpServerInstructions()})

	addMCPTools(server, d, baseDomain, proxyAddr)
	addMCPContext(server)
	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: disableLocalhostProtection})
}

func addMCPTools(server *mcp.Server, d DockerClient, baseDomain, proxyAddr string) {
	type noArgs struct{}

	type sandboxIDArgs struct {
		ID string `json:"id" jsonschema:"sandbox id"`
	}

	type sandboxCreateArgs struct {
		Image     string                 `json:"image" jsonschema:"docker image (required), e.g. node:24"`
		Ports     []string               `json:"ports,omitempty" jsonschema:"container ports, e.g. [3000,8080/tcp]"`
		Timeout   int                    `json:"timeout,omitempty" jsonschema:"auto stop timeout in seconds (0 uses default)"`
		Resources *models.ResourceLimits `json:"resources,omitempty" jsonschema:"resource limits"`
		Env       []string               `json:"env,omitempty" jsonschema:"environment vars as KEY=VALUE"`
	}

	type sandboxRenewArgs struct {
		ID      string `json:"id" jsonschema:"sandbox id"`
		Timeout int    `json:"timeout" jsonschema:"new timeout in seconds (>0)"`
	}

	type commandExecArgs struct {
		SandboxID string            `json:"sandbox_id" jsonschema:"sandbox id"`
		Command   string            `json:"command" jsonschema:"command name, e.g. npm"`
		Args      []string          `json:"args,omitempty" jsonschema:"command arguments"`
		Cwd       string            `json:"cwd,omitempty" jsonschema:"working directory"`
		Env       map[string]string `json:"env,omitempty" jsonschema:"env vars as object, e.g. {\"NODE_ENV\":\"development\"}"`
		Wait      bool              `json:"wait,omitempty" jsonschema:"wait until command finishes"`
	}

	type commandGetArgs struct {
		SandboxID string `json:"sandbox_id" jsonschema:"sandbox id"`
		CommandID string `json:"command_id" jsonschema:"command id"`
		Wait      bool   `json:"wait,omitempty" jsonschema:"wait until command finishes"`
	}

	type commandIDArgs struct {
		SandboxID string `json:"sandbox_id" jsonschema:"sandbox id"`
		CommandID string `json:"command_id" jsonschema:"command id"`
	}

	type commandKillArgs struct {
		SandboxID string `json:"sandbox_id" jsonschema:"sandbox id"`
		CommandID string `json:"command_id" jsonschema:"command id"`
		Signal    int    `json:"signal" jsonschema:"posix signal number"`
	}

	type fileReadArgs struct {
		SandboxID string `json:"sandbox_id" jsonschema:"sandbox id"`
		Path      string `json:"path" jsonschema:"file path inside sandbox"`
	}

	type fileWriteArgs struct {
		SandboxID string `json:"sandbox_id" jsonschema:"sandbox id"`
		Path      string `json:"path" jsonschema:"file path inside sandbox"`
		Content   string `json:"content" jsonschema:"file content"`
	}

	type fileListArgs struct {
		SandboxID string `json:"sandbox_id" jsonschema:"sandbox id"`
		Path      string `json:"path" jsonschema:"directory path (default /)"`
	}

	type imageIDArgs struct {
		ID string `json:"id" jsonschema:"image id or name:tag"`
	}

	type imagePullArgs struct {
		Image string `json:"image" jsonschema:"image name with tag"`
	}

	type imageDeleteArgs struct {
		ID    string `json:"id" jsonschema:"image id or name:tag"`
		Force bool   `json:"force,omitempty" jsonschema:"force deletion"`
	}

	mcp.AddTool(server, &mcp.Tool{Name: "system_health", Description: "Check Docker daemon health"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			if err := d.Ping(ctx); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]any{"status": "healthy"})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_list", Description: "List all sandboxes"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			items, err := d.List(ctx)
			if err != nil {
				return nil, nil, err
			}
			for i := range items {
				items[i].URL = buildSandboxURL(items[i].Name, baseDomain, proxyAddr)
			}
			return mcpJSON(map[string]any{"sandboxes": items})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_create", Description: "Create a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxCreateArgs) (*mcp.CallToolResult, any, error) {
			if args.Image == "" {
				return nil, nil, fmt.Errorf("image is required")
			}
			if args.Timeout < 0 {
				return nil, nil, fmt.Errorf("timeout must be >= 0")
			}
			if args.Resources != nil {
				if args.Resources.Memory < 0 || args.Resources.Memory > 8192 {
					return nil, nil, fmt.Errorf("resources.memory must be between 0 and 8192")
				}
				if args.Resources.CPUs < 0 || args.Resources.CPUs > 4.0 {
					return nil, nil, fmt.Errorf("resources.cpus must be between 0 and 4.0")
				}
			}

			resp, err := d.Create(ctx, models.CreateSandboxRequest{
				Image:     args.Image,
				Ports:     args.Ports,
				Timeout:   args.Timeout,
				Resources: args.Resources,
				Env:       args.Env,
			})
			if err != nil {
				return nil, nil, err
			}
			resp.URL = buildSandboxURL(resp.Name, baseDomain, proxyAddr)
			return mcpJSON(resp)
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_get", Description: "Get sandbox details"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			resp, err := d.Inspect(ctx, args.ID)
			if err != nil {
				return nil, nil, err
			}
			resp.URL = buildSandboxURL(resp.Name, baseDomain, proxyAddr)
			return mcpJSON(resp)
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_delete", Description: "Delete a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			if err := d.Remove(ctx, args.ID); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]string{"status": "deleted"})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_start", Description: "Start a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			resp, err := d.Start(ctx, args.ID)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(resp)
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_stop", Description: "Stop a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			if err := d.Stop(ctx, args.ID); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]string{"status": "stopped"})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_restart", Description: "Restart a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			resp, err := d.Restart(ctx, args.ID)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(resp)
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_pause", Description: "Pause a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			if err := d.Pause(ctx, args.ID); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]string{"status": "paused"})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_resume", Description: "Resume a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			if err := d.Resume(ctx, args.ID); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]string{"status": "resumed"})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_renew_expiration", Description: "Renew sandbox expiration"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxRenewArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			if args.Timeout <= 0 {
				return nil, nil, fmt.Errorf("timeout must be > 0")
			}
			if err := d.RenewExpiration(ctx, args.ID, args.Timeout); err != nil {
				return nil, nil, err
			}
			return mcpJSON(models.RenewExpirationResponse{Status: "renewed", Timeout: args.Timeout})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sandbox_stats", Description: "Get sandbox resource stats"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			stats, err := d.Stats(ctx, args.ID)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(stats)
		})

	mcp.AddTool(server, &mcp.Tool{Name: "command_exec", Description: "Execute a command in a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args commandExecArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" {
				return nil, nil, fmt.Errorf("sandbox_id is required")
			}
			if args.Command == "" {
				return nil, nil, fmt.Errorf("command is required")
			}
			cmd, err := d.ExecCommand(ctx, args.SandboxID, models.ExecCommandRequest{
				Command: args.Command,
				Args:    args.Args,
				Cwd:     args.Cwd,
				Env:     args.Env,
			})
			if err != nil {
				return nil, nil, err
			}
			if args.Wait {
				cmd, err = d.WaitCommand(ctx, args.SandboxID, cmd.ID)
				if err != nil {
					return nil, nil, err
				}
			}
			return mcpJSON(models.CommandResponse{Command: cmd})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "command_list", Description: "List commands for a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args sandboxIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			items, err := d.ListCommands(ctx, args.ID)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(models.CommandListResponse{Commands: items})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "command_get", Description: "Get command status"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args commandGetArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" || args.CommandID == "" {
				return nil, nil, fmt.Errorf("sandbox_id and command_id are required")
			}
			var (
				cmd models.CommandDetail
				err error
			)
			if args.Wait {
				cmd, err = d.WaitCommand(ctx, args.SandboxID, args.CommandID)
			} else {
				cmd, err = d.GetCommand(ctx, args.SandboxID, args.CommandID)
			}
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(models.CommandResponse{Command: cmd})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "command_kill", Description: "Kill a running command"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args commandKillArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" || args.CommandID == "" {
				return nil, nil, fmt.Errorf("sandbox_id and command_id are required")
			}
			if args.Signal == 0 {
				return nil, nil, fmt.Errorf("signal is required")
			}
			cmd, err := d.KillCommand(ctx, args.SandboxID, args.CommandID, args.Signal)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(models.CommandResponse{Command: cmd})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "command_logs", Description: "Get command logs snapshot"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args commandIDArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" || args.CommandID == "" {
				return nil, nil, fmt.Errorf("sandbox_id and command_id are required")
			}
			logs, err := d.GetCommandLogs(ctx, args.SandboxID, args.CommandID)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(logs)
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_read", Description: "Read a file in a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" || args.Path == "" {
				return nil, nil, fmt.Errorf("sandbox_id and path are required")
			}
			content, err := d.ReadFile(ctx, args.SandboxID, args.Path)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(models.FileReadResponse{Path: args.Path, Content: content})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_write", Description: "Write a file in a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args fileWriteArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" || args.Path == "" {
				return nil, nil, fmt.Errorf("sandbox_id and path are required")
			}
			if err := d.WriteFile(ctx, args.SandboxID, args.Path, args.Content); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]string{"path": args.Path, "status": "written"})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_delete", Description: "Delete a file or directory in a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args fileReadArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" || args.Path == "" {
				return nil, nil, fmt.Errorf("sandbox_id and path are required")
			}
			if err := d.DeleteFile(ctx, args.SandboxID, args.Path); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]string{"status": "deleted"})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_list", Description: "List directory content in a sandbox"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args fileListArgs) (*mcp.CallToolResult, any, error) {
			if args.SandboxID == "" {
				return nil, nil, fmt.Errorf("sandbox_id is required")
			}
			path := args.Path
			if path == "" {
				path = "/"
			}
			output, err := d.ListDir(ctx, args.SandboxID, path)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(models.FileListResponse{Path: path, Output: output})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "image_list", Description: "List local Docker images"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			images, err := d.ListImages(ctx)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]any{"images": images})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "image_get", Description: "Inspect a local image"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args imageIDArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			image, err := d.InspectImage(ctx, args.ID)
			if err != nil {
				return nil, nil, err
			}
			return mcpJSON(image)
		})

	mcp.AddTool(server, &mcp.Tool{Name: "image_pull", Description: "Pull an image from registry"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args imagePullArgs) (*mcp.CallToolResult, any, error) {
			if args.Image == "" {
				return nil, nil, fmt.Errorf("image is required")
			}
			if err := d.PullImage(ctx, args.Image); err != nil {
				return nil, nil, err
			}
			return mcpJSON(models.ImagePullResponse{Status: "pulled", Image: args.Image})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "image_delete", Description: "Delete a local image"},
		func(ctx context.Context, _ *mcp.CallToolRequest, args imageDeleteArgs) (*mcp.CallToolResult, any, error) {
			if args.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			if err := d.RemoveImage(ctx, args.ID, args.Force); err != nil {
				return nil, nil, err
			}
			return mcpJSON(map[string]string{"status": "deleted"})
		})
}

func mcpJSON(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, nil, nil
}

func addMCPContext(server *mcp.Server) {
	server.AddResource(&mcp.Resource{
		Name:        "opensandbox-how-it-works",
		Title:       "OpenSandbox MCP: How It Works",
		Description: "Operational model and best practices for using OpenSandbox tools",
		URI:         "opensandbox://docs/how-it-works",
		MIMEType:    "text/markdown",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if req.Params.URI != "opensandbox://docs/how-it-works" {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "text/markdown",
			Text:     mcpHowItWorksDoc(),
		}}}, nil
	})

	server.AddResource(&mcp.Resource{
		Name:        "opensandbox-quickstart",
		Title:       "OpenSandbox MCP: Quickstart",
		Description: "Fast workflow and command templates for common tasks",
		URI:         "opensandbox://docs/quickstart",
		MIMEType:    "text/markdown",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if req.Params.URI != "opensandbox://docs/quickstart" {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "text/markdown",
			Text:     mcpQuickstartDoc(),
		}}}, nil
	})

	server.AddPrompt(&mcp.Prompt{
		Name:        "sandbox_workflow",
		Title:       "Sandbox Workflow",
		Description: "Guidance prompt for creating a sandbox and running tasks safely",
		Arguments: []*mcp.PromptArgument{
			{Name: "goal", Description: "What the user wants to achieve", Required: true},
			{Name: "sandbox_id", Description: "Existing sandbox id, if already created"},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		goal := req.Params.Arguments["goal"]
		sandboxID := req.Params.Arguments["sandbox_id"]
		if sandboxID == "" {
			sandboxID = "<create_one_first>"
		}
		text := fmt.Sprintf(`You are using OpenSandbox over MCP.

Goal: %s
Sandbox ID: %s

Workflow:
1) If sandbox_id is missing, call sandbox_create with image/ports/timeout.
2) Verify with sandbox_get or sandbox_list.
3) Run commands only inside the sandbox with command_exec.
4) For long processes (e.g., dev servers), use command_exec with wait=false and then command_logs / command_get.
5) For file edits, use file_read then file_write in the detected project directory.
6) Confirm result by reading logs and, if exposed, the sandbox URL.

Important:
- command_exec.env is an object map, e.g. {"NODE_ENV":"development"}.
- Most failures after creation are wrong cwd or missing script.
- Detect the correct working directory before running app-specific commands.
`, goal, sandboxID)
		return &mcp.GetPromptResult{
			Description: "OpenSandbox best-practice workflow",
			Messages: []*mcp.PromptMessage{{
				Role:    mcp.Role("user"),
				Content: &mcp.TextContent{Text: text},
			}},
		}, nil
	})
}

func mcpServerInstructions() string {
	return `OpenSandbox MCP exposes Docker-backed sandboxes.

Recommended flow:
1) Create sandbox with sandbox_create.
2) Execute work inside it using command_exec.
3) Use command_get/command_logs for status/output.
4) Read/write files with file_read/file_write.

Important details:
- command_exec.env must be an object map (not an array).
- Always use the project directory as cwd (discover it first with file_list).
- Create does not run your app automatically; run the app command after creation.`
}

func mcpHowItWorksDoc() string {
	return `# OpenSandbox MCP: How It Works

OpenSandbox exposes tools to manage isolated Docker sandboxes.

## Core mental model
- ` + "`sandbox_create`" + ` provisions a container.
- ` + "`command_exec`" + ` runs processes inside that container.
- ` + "`file_read`" + ` / ` + "`file_write`" + ` edits files in the container filesystem.
- ` + "`sandbox_delete`" + ` destroys the environment.

## Typical workflow
1. Create sandbox with the right image.
2. Identify the correct project directory (for example by listing ` + "`/`" + `, then likely app folders).
3. Run command (` + "`bun run dev`" + `, ` + "`npm run dev`" + `, tests, etc.) with ` + "`command_exec`" + `.
4. Inspect output with ` + "`command_logs`" + `.
5. Modify files and rerun checks.

## Frequent mistakes
- Running commands in ` + "`/`" + ` instead of project directory.
- Passing ` + "`env`" + ` as array instead of object map.
- Assuming create auto-starts your app.
`
}

func mcpQuickstartDoc() string {
	return `# OpenSandbox MCP Quickstart

## Start project app
1. ` + "`sandbox_create`" + ` with your image.
2. Discover project directory with ` + "`file_list`" + ` on ` + "`/`" + ` and candidate folders.
3. ` + "`command_exec`" + ` with:
   - ` + "`cwd`" + `: ` + "`<project_dir>`" + `
   - ` + "`command`" + `: ` + "`bun`" + `
   - ` + "`args`" + `: [` + "`run`" + `, ` + "`dev`" + `]
   - ` + "`wait`" + `: false
4. ` + "`command_logs`" + ` to verify ready status.

## Edit a file safely
1. Locate file with ` + "`file_list`" + `.
2. Read current content with ` + "`file_read`" + `.
3. Write updated content with ` + "`file_write`" + `.
4. Re-run command or check logs for validation.
`
}
