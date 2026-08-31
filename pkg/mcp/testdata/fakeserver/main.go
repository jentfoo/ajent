// Command fakeserver is a scripted MCP server for pkg/mcp tests, built with
// mcp-go's own server package so the client is validated against an independent
// implementation. It serves over stdio by default or HTTP when given -http.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func main() {
	var httpAddr string
	var slow bool              // block every tool call until the client cancels
	var tools int              // number of generated echo tools to expose
	var notifyListChanged bool // emit a notifications/tools/list_changed on each trigger_listchanged call
	flag.StringVar(&httpAddr, "http", "", "serve over Streamable HTTP on this address")
	flag.BoolVar(&slow, "slow", false, "block each tool call until cancelled")
	flag.IntVar(&tools, "tools", 3, "number of generated echo tools to expose")
	flag.BoolVar(&notifyListChanged, "notify-list-changed", false, "emit list_changed via trigger_listchanged")
	flag.Parse()

	srv := mcpserver.NewMCPServer("fakeserver", "1.0")

	for i := range tools {
		name := fmt.Sprintf("tool_%02d", i)
		h := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if slow { // block until the client cancels; used for the timeout test
				<-ctx.Done()
				return &mcp.CallToolResult{}, nil
			}
			return okText(name + ": ok"), nil
		}
		srv.AddTool(mcp.NewToolWithRawSchema(name, "generated tool "+name,
			json.RawMessage(`{"type":"object","properties":{}}`),
		), h)
	}

	if notifyListChanged { // trigger tool emits list_changed, like real servers do
		srv.AddTool(mcp.NewToolWithRawSchema("trigger_listchanged", "emits notifications/tools/list_changed",
			json.RawMessage(`{"type":"object","properties":{}}`)),
			func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := srv.SendNotificationToClient(ctx, "notifications/tools/list_changed", nil); err != nil && !errors.Is(err, mcpserver.ErrNotificationChannelBlocked) {
					return &mcp.CallToolResult{}, fmt.Errorf("send list_changed: %w", err)
				}
				return okText("sent"), nil
			})
	}


	// a resource and prompt so the client can discover capabilities beyond tools.
	srv.AddResource(mcp.NewResource("fake://doc", "the doc",
		mcp.WithResourceDescription("a sample read-only document")),
		func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: "fake://doc", Text: "hello"}}, nil
		})
	srv.AddPrompt(mcp.NewPrompt("summarize",
		mcp.WithPromptDescription("summarize a document"),
		mcp.WithArgument("path", mcp.RequiredArgument())),
		func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Description: "summary", Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.NewTextContent("summarize")},
			}}, nil
		})

	if httpAddr != "" {
		h := mcpserver.NewStreamableHTTPServer(srv)
		fmt.Fprintln(os.Stderr, "listening on", httpAddr)
		if err := http.ListenAndServe(httpAddr, h); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := mcpserver.ServeStdio(srv); err != nil {
		fmt.Fprintln(os.Stderr, "stdio server:", err)
	}
}

func okText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(text)}}
}
