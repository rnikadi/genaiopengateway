package mcpgw

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// TestMain runs the test binary as a stdio MCP server when asked, so
// hosted servers are tested against a real child process.
func TestMain(m *testing.M) {
	if os.Getenv("GOG_TEST_STDIO_SERVER") == "1" {
		s := mcp.NewServer(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
		mcp.AddTool(s, &mcp.Tool{Name: "env", Description: "The server's environment"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			os.Stderr.WriteString("listing the environment\n")
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(os.Environ(), "\n")}}}, nil, nil
		})
		if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStdioServer(t *testing.T) {
	t.Setenv("GOG_ADMIN_TOKEN", "must-not-reach-the-child")
	cfg := config.MCPServer{
		Command: []string{os.Args[0]},
		Env:     map[string]config.Secret{"GOG_TEST_STDIO_SERVER": "1", "PROBE": "hello"},
		Dir:     t.TempDir(),
		Timeout: config.Duration(10 * time.Second),
	}
	u := newUpstream("local", cfg, http.DefaultTransport, nil, 1)
	defer u.close()
	ctx := context.Background()
	cat, err := u.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Tools) != 1 || cat.Tools[0].Name != "env" {
		t.Fatalf("tools %v", cat.Tools)
	}
	var out string
	err = u.do(ctx, "", func(ctx context.Context, cs *mcp.ClientSession) error {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "env"})
		if err == nil {
			out = res.Content[0].(*mcp.TextContent).Text
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "PROBE=hello") || !strings.Contains(out, "PATH=") || strings.Contains(out, "GOG_ADMIN_TOKEN") {
		t.Fatalf("the server's environment:\n%s", out)
	}

	// A server that exits is started again on the next call.
	u.mu.Lock()
	cs := u.session
	u.mu.Unlock()
	_ = cs.Close()
	if _, err := u.list(ctx); err != nil {
		t.Fatalf("after the process ended: %v", err)
	}
}
