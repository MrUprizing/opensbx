package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"opensbx/internal/applecontainer"
	"opensbx/internal/database"
	"opensbx/models"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type appleContractRunner struct{ t *testing.T }

func (r *appleContractRunner) Run(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	switch {
	case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
		_, _ = io.WriteString(out, `[{"Configuration":{"ID":"opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Labels":{"io.opensbx.managed":"opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"Resources":{"CPUs":1,"MemoryInBytes":1073741824},"PublishedPorts":[]},"Status":{"State":"running"}}]`)
	case len(args) == 8 && args[0] == "exec" && args[1] == "--interactive" && args[2] == "opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" && args[3] == "/bin/sh" && args[4] == "-c" && args[5] == `case "$1" in /*) ;; *) set -- "./$1";; esac; exec cat "$1"` && args[6] == "opensbx-file" && args[7] == "/tmp/contract.txt":
		_, _ = io.WriteString(out, "apple fixture content")
	default:
		r.t.Fatalf("unexpected Apple adapter argv: %#v", args)
	}
	return nil
}
func (r *appleContractRunner) Start(args []string, _ io.Reader, _ io.Writer, _ io.Writer) (applecontainer.Process, error) {
	r.t.Fatalf("unexpected Apple async command: %#v", args)
	return nil, io.ErrUnexpectedEOF
}

func TestInjectedAppleBackendKeepsRESTAndMCPFileReadContracts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = "opensbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	db := database.New(t.TempDir() + "/apple-api.db")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	repo := database.NewRepository(db)
	if err := repo.Save(database.Sandbox{ID: id, Name: "fixture", Image: "node:24"}); err != nil {
		t.Fatal(err)
	}
	backend := applecontainer.New(repo, &appleContractRunner{t: t}, nil)
	router := gin.New()
	handler := New(backend, "localhost", ":3000")
	handler.RegisterRoutes(router.Group("/v1"))
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+id+"/files?path=%2Ftmp%2Fcontract.txt", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("REST file read status=%d body=%s", response.Code, response.Body.String())
	}
	var rest models.FileReadResponse
	if err := json.Unmarshal(response.Body.Bytes(), &rest); err != nil || rest.Path != "/tmp/contract.txt" || rest.Content != "apple fixture content" {
		t.Fatalf("REST file response=%+v decode=%v", rest, err)
	}

	session := newMCPTestSession(t, backend)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "file_read", Arguments: map[string]any{"sandbox_id": id, "path": "/tmp/contract.txt"}})
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("MCP file_read result=%+v err=%v", result, err)
	}
	var mcpResponse models.FileReadResponse
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &mcpResponse); err != nil || mcpResponse.Path != rest.Path || mcpResponse.Content != rest.Content {
		t.Fatalf("MCP file response=%+v decode=%v", mcpResponse, err)
	}
}
