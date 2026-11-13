package mcpgw

import (
	"context"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// List-changed notifications (docs/mcp.md#change-notifications). A
// client on MCP 2026-07-28 opens a subscriptions/listen stream and is
// told when the tools, prompts or resources of its virtual server
// change. Older clients cannot be told by a stateless server; they see
// changes when they list again.
//
// A virtual server is built anew whenever a catalog changes, and a
// stream stays with the server it was opened on. So the gateway keeps
// the servers it replaced for as long as someone listens to them, and
// makes them send the notification.

const (
	listenMethod = "subscriptions/listen"
	// listenMax ends a stream so that its client opens a new one, which
	// checks its credentials again.
	listenMax = 30 * time.Minute
	// maxListeners bounds the open streams on this replica.
	maxListeners = 2000
	// changedName is the item added to and removed from a replaced
	// server to make it notify.
	changedName = "gateway_list_changed"
)

// The kinds of list a virtual server has.
const (
	listTools = iota
	listPrompts
	listResources
)

// listHashes identify the tools, prompts and resources a virtual server
// lists.
type listHashes [3][32]byte

// listen serves one subscriptions/listen stream.
func (g *Gateway) listen(ctx context.Context, vs string, next mcp.MethodHandler, req mcp.Request) (mcp.Result, error) {
	if g.listeners.Add(1) > maxListeners {
		g.listeners.Add(-1)
		g.opts.Metrics.ObserveMCPRequest(vs, listenMethod, telemetry.MCPError, 0)
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "too many clients are listening for changes; list again instead"}
	}
	defer g.listeners.Add(-1)
	g.opts.Metrics.ObserveMCPRequest(vs, listenMethod, telemetry.MCPOk, 0)
	ctx, cancel := context.WithTimeout(ctx, listenMax)
	defer cancel()
	return next(ctx, listenMethod, req)
}

// notifyListeners tells the clients listening to each virtual server
// which of its lists changed. It is called with g.mu held, after the
// servers in prev were replaced.
func (g *Gateway) notifyListeners(prev map[string]*mcp.Server, lists map[string]listHashes) {
	if g.retired == nil {
		g.retired = map[string][]*mcp.Server{}
	}
	for name, old := range prev {
		if old != nil {
			g.retired[name] = append(g.retired[name], old)
		}
	}
	for name, servers := range g.retired {
		was, now := g.lists[name], lists[name]
		kept := servers[:0]
		for _, s := range servers {
			listened := false
			for range s.Sessions() {
				listened = true
				break
			}
			if !listened {
				continue
			}
			kept = append(kept, s)
			if was[listTools] != now[listTools] {
				s.AddTool(&mcp.Tool{Name: changedName, InputSchema: &jsonschema.Schema{Type: "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return blockedResult("List the tools again."), nil
				})
				s.RemoveTools(changedName)
			}
			if was[listPrompts] != now[listPrompts] {
				s.AddPrompt(&mcp.Prompt{Name: changedName}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "list the prompts again"}
				})
				s.RemovePrompts(changedName)
			}
			if was[listResources] != now[listResources] {
				s.AddResource(&mcp.Resource{Name: changedName, URI: "gateway:list-changed"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "list the resources again"}
				})
				s.RemoveResources("gateway:list-changed")
			}
		}
		if len(kept) == 0 {
			delete(g.retired, name)
		} else {
			g.retired[name] = kept
		}
	}
	g.lists = lists
}
