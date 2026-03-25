package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"go-mcp-printer-direct/internal/printer"
)

// Handler processes MCP JSON-RPC requests using direct printer communication.
type Handler struct {
	ippClient  *printer.IPPClient
	snmpClient *printer.SNMPClient
	printerIP  string
	printerName string
	dialFunc   func(network, addr string) (net.Conn, error)
	tools      []Tool
	resources  []Resource
	prompts    []Prompt
}

// NewHandler creates a new MCP handler.
func NewHandler(printerIP, printerName string, dialFunc func(network, addr string) (net.Conn, error)) *Handler {
	h := &Handler{
		ippClient:   printer.NewIPPClient(printerIP, dialFunc),
		snmpClient:  printer.NewSNMPClient(printerIP, dialFunc),
		printerIP:   printerIP,
		printerName: printerName,
		dialFunc:    dialFunc,
	}

	h.registerTools()
	h.registerResources()
	h.registerPrompts()

	return h
}

// HandleRequest processes a raw HTTP request body as an MCP JSON-RPC request.
func (h *Handler) HandleRequest(body []byte) ([]byte, error) {
	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return h.errorResponse(nil, ParseError, "Parse error")
	}

	if req.JSONRPC != "2.0" {
		return h.errorResponse(req.ID, InvalidRequest, "Invalid JSON-RPC version")
	}

	// Notifications (no ID) don't require a response
	if req.ID == nil || string(req.ID) == "null" {
		return nil, nil
	}

	switch req.Method {
	case "initialize":
		return h.handleInitialize(req.ID)
	case "tools/list":
		return h.handleToolsList(req.ID)
	case "tools/call":
		return h.handleToolsCall(req.ID, req.Params)
	case "resources/list":
		return h.handleResourcesList(req.ID)
	case "resources/read":
		return h.handleResourcesRead(req.ID, req.Params)
	case "prompts/list":
		return h.handlePromptsList(req.ID)
	case "prompts/get":
		return h.handlePromptsGet(req.ID, req.Params)
	case "ping":
		return h.successResponse(req.ID, map[string]string{})
	default:
		return h.errorResponse(req.ID, MethodNotFound, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (h *Handler) handleInitialize(id json.RawMessage) ([]byte, error) {
	result := InitializeResult{
		ProtocolVersion: ProtocolVersion,
		Capabilities: ServerCapabilities{
			Tools:     &ToolsCapability{},
			Resources: &ResourcesCapability{},
			Prompts:   &PromptsCapability{},
		},
		ServerInfo: ServerInfo{
			Name:    "go-mcp-printer-direct",
			Version: "1.0.0",
		},
	}
	return h.successResponse(id, result)
}

func (h *Handler) handleToolsList(id json.RawMessage) ([]byte, error) {
	return h.successResponse(id, map[string]interface{}{
		"tools": h.tools,
	})
}

func (h *Handler) handleToolsCall(id json.RawMessage, params json.RawMessage) ([]byte, error) {
	var call ToolCallParams
	if err := json.Unmarshal(params, &call); err != nil {
		return h.errorResponse(id, InvalidParams, "Invalid tool call parameters")
	}

	slog.Info("tool call", "name", call.Name, "arguments", call.Arguments)

	result := h.dispatchTool(call.Name, call.Arguments)
	return h.successResponse(id, result)
}

func (h *Handler) handleResourcesList(id json.RawMessage) ([]byte, error) {
	return h.successResponse(id, map[string]interface{}{
		"resources": h.resources,
	})
}

func (h *Handler) handleResourcesRead(id json.RawMessage, params json.RawMessage) ([]byte, error) {
	var readParams ResourceReadParams
	if err := json.Unmarshal(params, &readParams); err != nil {
		return h.errorResponse(id, InvalidParams, "Invalid resource read parameters")
	}

	result := h.readResource(readParams.URI)
	return h.successResponse(id, result)
}

func (h *Handler) handlePromptsList(id json.RawMessage) ([]byte, error) {
	return h.successResponse(id, map[string]interface{}{
		"prompts": h.prompts,
	})
}

func (h *Handler) handlePromptsGet(id json.RawMessage, params json.RawMessage) ([]byte, error) {
	var getParams PromptGetParams
	if err := json.Unmarshal(params, &getParams); err != nil {
		return h.errorResponse(id, InvalidParams, "Invalid prompt get parameters")
	}

	result := h.getPrompt(getParams.Name, getParams.Arguments)
	return h.successResponse(id, result)
}

// --- Tool dispatch ---

func (h *Handler) dispatchTool(name string, args map[string]interface{}) *ToolResult {
	switch name {
	case "get_printer_info":
		return h.toolGetPrinterInfo()
	case "get_ink_levels":
		return h.toolGetInkLevels()
	case "print_text":
		return h.toolPrintText(args)
	case "print_url":
		return h.toolPrintURL(args)
	case "get_print_queue":
		return h.toolGetPrintQueue()
	case "get_job_status":
		return h.toolGetJobStatus(args)
	case "cancel_job":
		return h.toolCancelJob(args)
	case "test_connectivity":
		return h.toolTestConnectivity()
	default:
		return ErrorResult(fmt.Sprintf("Unknown tool: %s", name))
	}
}

func (h *Handler) toolGetPrinterInfo() *ToolResult {
	info, err := h.ippClient.GetPrinterInfo()
	if err != nil {
		return ErrorResult(fmt.Sprintf("Failed to get printer info: %v", err))
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	return TextResult(string(data))
}

func (h *Handler) toolGetInkLevels() *ToolResult {
	status, err := h.snmpClient.GetSupplyLevels()
	if err != nil {
		return ErrorResult(fmt.Sprintf("Failed to get supply levels: %v", err))
	}
	data, _ := json.MarshalIndent(status, "", "  ")
	return TextResult(string(data))
}

func (h *Handler) toolPrintText(args map[string]interface{}) *ToolResult {
	text, ok := args["text"].(string)
	if !ok || text == "" {
		return ErrorResult("'text' argument is required")
	}

	jobName, _ := args["job_name"].(string)
	copies := 1
	if c, ok := args["copies"].(float64); ok {
		copies = int(c)
	}

	result, err := h.ippClient.PrintText(text, jobName, copies)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Print failed: %v", err))
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return TextResult(string(data))
}

func (h *Handler) toolPrintURL(args map[string]interface{}) *ToolResult {
	url, ok := args["url"].(string)
	if !ok || url == "" {
		return ErrorResult("'url' argument is required")
	}

	// Validate URL starts with http:// or https://
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return ErrorResult("URL must start with http:// or https://")
	}

	// Download the content
	transport := &http.Transport{}
	if h.dialFunc != nil {
		transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return h.dialFunc(network, addr)
		}
	}
	dlClient := &http.Client{Transport: transport, Timeout: 15 * time.Second}

	resp, err := dlClient.Get(url)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Failed to download URL: %v", err))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Failed to read URL content: %v", err))
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// Simplify content type (remove charset etc.)
	if idx := strings.Index(contentType, ";"); idx > 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	jobName, _ := args["job_name"].(string)
	if jobName == "" {
		jobName = "URL: " + url
	}
	copies := 1
	if c, ok := args["copies"].(float64); ok {
		copies = int(c)
	}

	result, err := h.ippClient.PrintDocument(data, contentType, jobName, copies)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Print failed: %v", err))
	}
	resultData, _ := json.MarshalIndent(result, "", "  ")
	return TextResult(string(resultData))
}

func (h *Handler) toolGetPrintQueue() *ToolResult {
	jobs, err := h.ippClient.GetJobs()
	if err != nil {
		return ErrorResult(fmt.Sprintf("Failed to get print queue: %v", err))
	}
	if len(jobs) == 0 {
		return TextResult("Print queue is empty")
	}
	data, _ := json.MarshalIndent(jobs, "", "  ")
	return TextResult(string(data))
}

func (h *Handler) toolGetJobStatus(args map[string]interface{}) *ToolResult {
	jobIDFloat, ok := args["job_id"].(float64)
	if !ok {
		return ErrorResult("'job_id' argument is required (integer)")
	}
	jobID := int(jobIDFloat)

	job, err := h.ippClient.GetJobStatus(jobID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Failed to get job status: %v", err))
	}
	data, _ := json.MarshalIndent(job, "", "  ")
	return TextResult(string(data))
}

func (h *Handler) toolCancelJob(args map[string]interface{}) *ToolResult {
	jobIDFloat, ok := args["job_id"].(float64)
	if !ok {
		return ErrorResult("'job_id' argument is required (integer)")
	}
	jobID := int(jobIDFloat)

	if err := h.ippClient.CancelJob(jobID); err != nil {
		return ErrorResult(fmt.Sprintf("Failed to cancel job: %v", err))
	}
	return TextResult(fmt.Sprintf("Job %d cancelled successfully", jobID))
}

func (h *Handler) toolTestConnectivity() *ToolResult {
	result := printer.TestConnectivity(h.printerIP, h.dialFunc)
	data, _ := json.MarshalIndent(result, "", "  ")
	return TextResult(string(data))
}

// --- Resource dispatch ---

func (h *Handler) readResource(uri string) *ResourceReadResult {
	switch uri {
	case "printer://info":
		info, err := h.ippClient.GetPrinterInfo()
		if err != nil {
			return &ResourceReadResult{
				Contents: []ResourceContent{{URI: uri, MimeType: "text/plain", Text: fmt.Sprintf("Error: %v", err)}},
			}
		}
		data, _ := json.MarshalIndent(info, "", "  ")
		return &ResourceReadResult{
			Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}},
		}

	case "printer://supplies":
		status, err := h.snmpClient.GetSupplyLevels()
		if err != nil {
			return &ResourceReadResult{
				Contents: []ResourceContent{{URI: uri, MimeType: "text/plain", Text: fmt.Sprintf("Error: %v", err)}},
			}
		}
		data, _ := json.MarshalIndent(status, "", "  ")
		return &ResourceReadResult{
			Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}},
		}

	case "printer://help":
		return &ResourceReadResult{
			Contents: []ResourceContent{{URI: uri, MimeType: "text/markdown", Text: h.helpText()}},
		}

	default:
		return &ResourceReadResult{
			Contents: []ResourceContent{{URI: uri, MimeType: "text/plain", Text: "Resource not found: " + uri}},
		}
	}
}

func (h *Handler) helpText() string {
	return fmt.Sprintf(`# MCP Printer Direct - Help

## Printer
- **Name:** %s
- **IP:** %s

## Available Tools

### get_printer_info
Get printer model, status, capabilities, and supported paper sizes.

### get_ink_levels
Check ink/toner supply levels via SNMP.

### print_text
Print plain text content. Arguments:
- **text** (required): The text to print
- **job_name** (optional): Name for the print job
- **copies** (optional): Number of copies (default: 1)

### print_url
Download and print content from a URL. Supports PDF, images, and text. Arguments:
- **url** (required): URL to download and print (http:// or https://)
- **job_name** (optional): Name for the print job
- **copies** (optional): Number of copies (default: 1)

### get_print_queue
View current print jobs in the queue.

### get_job_status
Check the status of a specific print job. Arguments:
- **job_id** (required): The job ID to check

### cancel_job
Cancel a print job. Arguments:
- **job_id** (required): The job ID to cancel

### test_connectivity
Test network connectivity to the printer (IPP, JetDirect, HTTP, SNMP ports).

## Supported Protocols
- **IPP** (port 631): Print jobs, queue management, printer status
- **SNMP** (port 161): Ink/toner supply levels
- **JetDirect** (port 9100): Raw printing
- **HTTP** (port 80): Printer web interface
`, h.printerName, h.printerIP)
}

// --- Prompt dispatch ---

func (h *Handler) getPrompt(name string, args map[string]string) *PromptGetResult {
	switch name {
	case "diagnose-printer":
		return &PromptGetResult{
			Description: "Diagnose printer issues",
			Messages: []PromptMessage{
				{Role: "user", Content: ContentBlock{Type: "text", Text: fmt.Sprintf(
					`Please diagnose the printer at %s (%s). Follow these steps:
1. Run test_connectivity to check all ports
2. Run get_printer_info to check printer state and any error reasons
3. Run get_ink_levels to check supply levels
4. Run get_print_queue to check for stuck jobs
5. Summarize findings and suggest fixes for any issues found`, h.printerIP, h.printerName)}},
			},
		}

	case "supply-check":
		return &PromptGetResult{
			Description: "Check printer supply levels",
			Messages: []PromptMessage{
				{Role: "user", Content: ContentBlock{Type: "text", Text: fmt.Sprintf(
					`Check the supply levels for the printer at %s (%s):
1. Run get_ink_levels to get current supply levels
2. Flag any supplies below 20%% as low
3. Flag any supplies below 10%% as critical
4. Provide a summary of all supply levels`, h.printerIP, h.printerName)}},
			},
		}

	case "print-document":
		path := args["path"]
		if path == "" {
			path = "[specify file URL]"
		}
		return &PromptGetResult{
			Description: "Print a document with smart defaults",
			Messages: []PromptMessage{
				{Role: "user", Content: ContentBlock{Type: "text", Text: fmt.Sprintf(
					`Print the document at: %s
1. First check printer status with get_printer_info
2. If the URL is a web page, use print_url
3. If it's text content, use print_text
4. Verify the job was accepted by checking get_print_queue
5. Report the job ID and status`, path)}},
			},
		}

	default:
		return &PromptGetResult{
			Messages: []PromptMessage{
				{Role: "user", Content: ContentBlock{Type: "text", Text: "Unknown prompt: " + name}},
			},
		}
	}
}

// --- Tool/resource/prompt registration ---

func (h *Handler) registerTools() {
	h.tools = []Tool{
		{
			Name:        "get_printer_info",
			Description: fmt.Sprintf("Get detailed information about the printer at %s including model, status, capabilities, and supported paper sizes", h.printerIP),
			InputSchema: InputSchema{Type: "object"},
			Annotations: &ToolAnnotations{ReadOnlyHint: BoolPtr(true)},
		},
		{
			Name:        "get_ink_levels",
			Description: "Get ink/toner supply levels for the printer via SNMP",
			InputSchema: InputSchema{Type: "object"},
			Annotations: &ToolAnnotations{ReadOnlyHint: BoolPtr(true)},
		},
		{
			Name:        "print_text",
			Description: "Print plain text content to the printer via IPP",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"text":     {Type: "string", Description: "The text content to print"},
					"job_name": {Type: "string", Description: "Name for the print job (optional)"},
					"copies":   {Type: "integer", Description: "Number of copies to print", Minimum: intPtr(1), Maximum: intPtr(99), Default: 1},
				},
				Required: []string{"text"},
			},
			Annotations: &ToolAnnotations{DestructiveHint: BoolPtr(true)},
		},
		{
			Name:        "print_url",
			Description: "Download content from a URL and print it. Supports PDF, images, text, and HTML",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"url":      {Type: "string", Description: "URL to download and print (must start with http:// or https://)"},
					"job_name": {Type: "string", Description: "Name for the print job (optional)"},
					"copies":   {Type: "integer", Description: "Number of copies to print", Minimum: intPtr(1), Maximum: intPtr(99), Default: 1},
				},
				Required: []string{"url"},
			},
			Annotations: &ToolAnnotations{DestructiveHint: BoolPtr(true)},
		},
		{
			Name:        "get_print_queue",
			Description: "Get the current print queue showing all active and pending jobs",
			InputSchema: InputSchema{Type: "object"},
			Annotations: &ToolAnnotations{ReadOnlyHint: BoolPtr(true)},
		},
		{
			Name:        "get_job_status",
			Description: "Get the status of a specific print job by ID",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"job_id": {Type: "integer", Description: "The print job ID to check"},
				},
				Required: []string{"job_id"},
			},
			Annotations: &ToolAnnotations{ReadOnlyHint: BoolPtr(true)},
		},
		{
			Name:        "cancel_job",
			Description: "Cancel a specific print job by ID",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"job_id": {Type: "integer", Description: "The print job ID to cancel"},
				},
				Required: []string{"job_id"},
			},
			Annotations: &ToolAnnotations{DestructiveHint: BoolPtr(true)},
		},
		{
			Name:        "test_connectivity",
			Description: fmt.Sprintf("Test network connectivity to the printer at %s on IPP (631), JetDirect (9100), HTTP (80), and SNMP (161) ports", h.printerIP),
			InputSchema: InputSchema{Type: "object"},
			Annotations: &ToolAnnotations{ReadOnlyHint: BoolPtr(true)},
		},
	}
}

func (h *Handler) registerResources() {
	h.resources = []Resource{
		{
			URI:         "printer://info",
			Name:        "Printer Information",
			Description: "Current printer status, model, and capabilities",
			MimeType:    "application/json",
		},
		{
			URI:         "printer://supplies",
			Name:        "Supply Levels",
			Description: "Current ink/toner supply levels",
			MimeType:    "application/json",
		},
		{
			URI:         "printer://help",
			Name:        "Printing Guide",
			Description: "Help guide for using the printer MCP tools",
			MimeType:    "text/markdown",
		},
	}
}

func (h *Handler) registerPrompts() {
	h.prompts = []Prompt{
		{
			Name:        "diagnose-printer",
			Description: "Run a full diagnostic on the printer: connectivity, status, ink levels, and queue",
		},
		{
			Name:        "supply-check",
			Description: "Check ink/toner supply levels and flag low or critical supplies",
		},
		{
			Name:        "print-document",
			Description: "Print a document with smart defaults based on content type",
			Arguments: []PromptArgument{
				{Name: "path", Description: "URL or path of the document to print", Required: true},
			},
		},
	}
}

// --- Response helpers ---

func (h *Handler) successResponse(id json.RawMessage, result interface{}) ([]byte, error) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	return json.Marshal(resp)
}

func (h *Handler) errorResponse(id json.RawMessage, code int, message string) ([]byte, error) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
	return json.Marshal(resp)
}

func intPtr(i int) *int {
	return &i
}
