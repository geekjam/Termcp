package mcp

import (
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// registerTermTools declares the single terminal entry, which is the whole
// default surface. Keep it that way: the tool listing is context the caller pays
// for on every turn, and the capability count must not move it (issue #117 33b).
func registerTermTools(mcpServer *mcpserver.MCPServer, s *Server) {
	mcpServer.AddTool(mcpgo.NewTool(termToolName,
		// Four default booleans on every tool carry no termcp information and cost
		// ~80 bytes; the annotation block is empty on purpose.
		mcpgo.WithToolAnnotation(mcpgo.ToolAnnotation{}),
		mcpgo.WithDescription(termDescription),
		mcpgo.WithString("cmd", mcpgo.Description("One terminal line, e.g. `run a1f3:1 ls -la`. Empty = guide: usage summary plus what is available to act on now.")),
		// The default is repeated in the prose, not only in the schema's
		// `default` field: clients differ on whether they render that field, and
		// a hidden default is a guess the caller should not have to make.
		mcpgo.WithNumber("timeout", mcpgo.Description(fmt.Sprintf("Time axis: max seconds to wait. A pipe returns early when its process exits; a pty waits the window out. 0 = do not wait. Default: %d.", defaultTimeoutSeconds)), mcpgo.DefaultNumber(defaultTimeoutSeconds)),
		mcpgo.WithNumber("bytes_limit", mcpgo.Description(fmt.Sprintf("Space axis: max raw bytes returned. 0 = handle only, consume nothing. Default: %d.", defaultBytesLimit)), mcpgo.DefaultNumber(defaultBytesLimit)),
		mcpgo.WithNumber("lines_limit", mcpgo.Description(fmt.Sprintf("Space axis: max complete lines returned. 0 = no line limit. Default: %d.", defaultLinesLimit)), mcpgo.DefaultNumber(defaultLinesLimit)),
		mcpgo.WithNumber("total_bytes_limit", mcpgo.Description(fmt.Sprintf("Whole reply: absolute cap across all slots. Default: %d.", defaultTotalBytesLimit)), mcpgo.DefaultNumber(defaultTotalBytesLimit)),
		mcpgo.WithString("on_time_overflow", mcpgo.Description("Time window overflow: truncate = return what arrived, fold = return no bytes. Default: truncate."), mcpgo.DefaultString("truncate"), mcpgo.Enum("truncate", "fold")),
		mcpgo.WithString("on_bytes_overflow", mcpgo.Description("Byte window overflow: truncate = return the head, fold = return no bytes. Default: truncate."), mcpgo.DefaultString("truncate"), mcpgo.Enum("truncate", "fold")),
		mcpgo.WithString("on_lines_overflow", mcpgo.Description("Line window overflow: truncate = return the first N lines, fold = return no bytes. Default: truncate."), mcpgo.DefaultString("truncate"), mcpgo.Enum("truncate", "fold")),
		mcpgo.WithString("pipe_to", mcpgo.Description("Send this command's stdout into another handle's stdin; those bytes stay out of the reply body. Default: empty (stdout stays in the reply).")),
		mcpgo.WithBoolean("learn_mode", mcpgo.Description("Attach the matched directive's one-line usage to its slot (once per directive). Default: false."), mcpgo.DefaultBool(false)),
	), withLogging(termToolName, s.handleTerm, s))
}
