// Package main implements the Hyperax Slack plugin — a standalone MCP server
// that provides bi-directional Slack communication over stdio.
//
// Protocol: JSON-RPC 2.0 on stdin/stdout. All logging goes to stderr.
// Configuration: environment variables or MCP initialize params.
//
// This is a scaffold — all tool implementations return "not yet implemented".
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// --- JSON-RPC 2.0 wire types ---

// JSONRPCRequest represents an incoming JSON-RPC 2.0 request or notification.
type JSONRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// JSONRPCResponse is a JSON-RPC 2.0 response sent back to the client.
type JSONRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *JSONRPCError    `json:"error,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ToolCallParams is the params object for tools/call.
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResult is the MCP tool result format.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent is a single content block in a tool result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolDefinition describes a tool for the tools/list response.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolHandlerFunc is the signature for a tool implementation.
type ToolHandlerFunc func(ctx context.Context, args json.RawMessage) (*ToolResult, error)

// --- Server ---

// Server is the MCP server that reads JSON-RPC from stdin and writes to stdout.
type Server struct {
	logger  *slog.Logger
	tools   map[string]ToolHandlerFunc
	schemas []ToolDefinition
	writeMu sync.Mutex
	encoder *json.Encoder
}

// NewServer creates an MCP server with placeholder Slack tools.
func NewServer(logger *slog.Logger) *Server {
	s := &Server{
		logger:  logger,
		tools:   make(map[string]ToolHandlerFunc),
		encoder: json.NewEncoder(os.Stdout),
	}
	s.registerTools()
	return s
}

// RegisterTool adds a tool to the server's registry.
func (s *Server) RegisterTool(name, description string, inputSchema json.RawMessage, handler ToolHandlerFunc) {
	s.tools[name] = handler
	s.schemas = append(s.schemas, ToolDefinition{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
	})
}

// registerTools registers all Slack tool placeholders.
func (s *Server) registerTools() {
	stub := func(_ context.Context, _ json.RawMessage) (*ToolResult, error) {
		return &ToolResult{
			Content: []ToolContent{{Type: "text", Text: "not yet implemented"}},
		}, nil
	}

	s.RegisterTool("slack_send_message", "Send a message to a Slack channel", json.RawMessage(`{
		"type": "object",
		"properties": {
			"channel": {"type": "string", "description": "Channel ID"},
			"text":    {"type": "string", "description": "Message text"}
		},
		"required": ["channel", "text"]
	}`), stub)

	s.RegisterTool("slack_read_history", "Read recent messages from a Slack channel", json.RawMessage(`{
		"type": "object",
		"properties": {
			"channel": {"type": "string", "description": "Channel ID"},
			"limit":   {"type": "integer", "description": "Number of messages", "default": 20}
		},
		"required": ["channel"]
	}`), stub)

	s.RegisterTool("slack_list_channels", "List available Slack channels", json.RawMessage(`{
		"type": "object",
		"properties": {}
	}`), stub)

	s.RegisterTool("slack_poll_channels", "Poll monitored channels for new messages", json.RawMessage(`{
		"type": "object",
		"properties": {}
	}`), stub)
}

// Run starts the MCP server loop, reading from stdin until EOF or context cancellation.
func (s *Server) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	s.logger.Info("MCP server listening on stdin")

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("context cancelled, shutting down")
			return nil
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("stdin read error: %w", err)
			}
			s.logger.Info("stdin closed, shutting down")
			return nil
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		s.handleMessage(ctx, line)
	}
}

func (s *Server) handleMessage(ctx context.Context, data []byte) {
	var req JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		s.logger.Error("invalid JSON-RPC message", "error", err)
		s.sendError(nil, -32700, "Parse error", nil)
		return
	}

	if req.JSONRPC != "2.0" {
		s.sendError(req.ID, -32600, "Invalid Request: jsonrpc must be 2.0", nil)
		return
	}

	s.logger.Debug("received request", "method", req.Method)

	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "initialized", "notifications/initialized":
		s.logger.Info("client initialized")
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(ctx, req)
	case "ping":
		s.sendResult(req.ID, map[string]string{})
	default:
		s.sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method), nil)
	}
}

func (s *Server) handleInitialize(req JSONRPCRequest) {
	s.logger.Info("initialize handshake")

	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "hax-plugin-slack",
			"version": version,
		},
	}

	s.sendResult(req.ID, result)
}

func (s *Server) handleToolsList(req JSONRPCRequest) {
	s.sendResult(req.ID, map[string]any{"tools": s.schemas})
}

func (s *Server) handleToolsCall(ctx context.Context, req JSONRPCRequest) {
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.sendError(req.ID, -32602, "Invalid params: "+err.Error(), nil)
		return
	}

	handler, ok := s.tools[params.Name]
	if !ok {
		s.sendError(req.ID, -32602, fmt.Sprintf("Unknown tool: %s", params.Name), nil)
		return
	}

	result, err := handler(ctx, params.Arguments)
	if err != nil {
		s.logger.Error("tool error", "tool", params.Name, "error", err)
		s.sendResult(req.ID, &ToolResult{
			Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Error: %s", err.Error())}},
			IsError: true,
		})
		return
	}

	s.sendResult(req.ID, result)
}

func (s *Server) sendResult(id *json.RawMessage, result any) {
	resp := JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.encoder.Encode(resp); err != nil {
		s.logger.Error("failed to write response", "error", err)
	}
}

func (s *Server) sendError(id *json.RawMessage, code int, message string, data any) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: message, Data: data},
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.encoder.Encode(resp); err != nil {
		s.logger.Error("failed to write error response", "error", err)
	}
}

func main() {
	logLevel := os.Getenv("SLACK_LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	logger := newLogger(logLevel)
	logger.Info("hax-plugin-slack starting",
		"version", version,
		"commit", commit,
		"date", date,
	)

	server := NewServer(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}

	logger.Info("hax-plugin-slack stopped")
}

// newLogger creates an slog.Logger that writes JSON to stderr.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})
	return slog.New(handler)
}
