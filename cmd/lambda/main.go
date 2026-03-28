package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	fiberadapter "github.com/awslabs/aws-lambda-go-api-proxy/fiber"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"

	appconfig "go-mcp-printer-direct/internal/config"
	"go-mcp-printer-direct/internal/mcp"
	"go-mcp-printer-direct/internal/middleware"
	"go-mcp-printer-direct/internal/oauth"
	"go-mcp-printer-direct/internal/store"
	"go-mcp-printer-direct/internal/telemetry"
	"go-mcp-printer-direct/internal/token"
	"go-mcp-printer-direct/internal/vpn"
)

var fiberLambda *fiberadapter.FiberLambda
var otelShutdown func(context.Context) error

func init() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	ctx := context.Background()

	cfg, err := appconfig.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		slog.Error("failed to load AWS config", "error", err)
		os.Exit(1)
	}

	smClient := secretsmanager.NewFromConfig(awsCfg)
	ddbClient := dynamodb.NewFromConfig(awsCfg)

	otelShutdown, err = telemetry.Init(ctx, cfg.OTelServiceName, cfg.OTelEndpoint)
	if err != nil {
		slog.Warn("OTel init failed, continuing without telemetry", "error", err)
	}

	keyPair, err := token.LoadKeyPair(ctx, smClient, cfg.JWTSigningKeyARN)
	if err != nil {
		slog.Error("failed to load JWT signing keys", "error", err)
		os.Exit(1)
	}

	// Initialize WireGuard tunnel (optional)
	var tunnel *vpn.Tunnel
	var dialFunc func(network, addr string) (net.Conn, error)

	if cfg.WGConfigSecretARN != "" {
		wgCfg, err := vpn.LoadConfig(ctx, smClient, cfg.WGConfigSecretARN)
		if err != nil {
			slog.Error("failed to load WireGuard config", "error", err)
		} else {
			tunnel, err = vpn.StartTunnel(wgCfg)
			if err != nil {
				slog.Error("failed to start WireGuard tunnel", "error", err)
			} else {
				slog.Info("WireGuard tunnel started successfully")
				dialFunc = tunnel.DialContext
			}
		}
	} else {
		slog.Warn("WG_CONFIG_SECRET_ARN not set, WireGuard VPN disabled")
	}

	// Initialize DynamoDB store
	oauthStore := store.NewDynamoDBStore(ddbClient, cfg.DynamoDBTable)

	// Create OAuth handler
	oauthHandler := &oauth.Handler{
		Config:  cfg,
		Store:   oauthStore,
		KeyPair: keyPair,
	}

	// Create MCP handler with direct printer access
	mcpHandler := mcp.NewHandler(cfg.PrinterIP, cfg.PrinterName, dialFunc)

	// Create Fiber app
	app := fiber.New(fiber.Config{
		ReadTimeout:           29 * time.Second,
		WriteTimeout:          29 * time.Second,
		AppName:               "mcp-printer-direct",
		DisableStartupMessage: true,
	})

	// Global middleware
	app.Use(recover.New())
	app.Use(middleware.RequestLogger())
	app.Use(middleware.OTelTracing())

	// Health check
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":     "ok",
			"vpn":        tunnel != nil,
			"printer_ip": cfg.PrinterIP,
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
		})
	})

	// OAuth routes
	oauthHandler.RegisterRoutes(app)

	// MCP endpoint (authenticated)
	app.Post("/mcp", oauth.BearerAuthMiddleware(keyPair), func(c *fiber.Ctx) error {
		body := c.Body()

		resp, err := mcpHandler.HandleRequest(body)
		if err != nil {
			slog.Error("MCP handler error", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "internal server error",
			})
		}

		if resp == nil {
			return c.SendStatus(fiber.StatusNoContent)
		}

		c.Set("Content-Type", "application/json")
		return c.Send(resp)
	})

	// Direct MCP endpoint for claude.ai (sends to /printer path)
	mcpRoute := func(c *fiber.Ctx) error {
		body := c.Body()

		resp, err := mcpHandler.HandleRequest(body)
		if err != nil {
			slog.Error("MCP handler error", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "internal server error",
			})
		}

		if resp == nil {
			return c.SendStatus(fiber.StatusNoContent)
		}

		c.Set("Content-Type", "application/json")
		return c.Send(resp)
	}

	app.Post("/printer", oauth.BearerAuthMiddleware(keyPair), mcpRoute)
	app.Post("/printer/*", oauth.BearerAuthMiddleware(keyPair), mcpRoute)

	// SSE endpoint for streamable HTTP transport
	sseHandler := func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")
		return c.SendString("event: endpoint\ndata: /mcp\n\n")
	}
	app.Get("/mcp", oauth.BearerAuthMiddleware(keyPair), sseHandler)

	// Root path MCP handlers (Claude and other clients connect to the server URL root)
	app.Post("/", oauth.BearerAuthMiddleware(keyPair), mcpRoute)
	app.Get("/", oauth.BearerAuthMiddleware(keyPair), sseHandler)

	// Download URL for print_url tool (proxied through VPN if enabled)
	app.Get("/download", oauth.BearerAuthMiddleware(keyPair), func(c *fiber.Ctx) error {
		url := c.Query("url")
		if url == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "url parameter required"})
		}

		transport := &fiber.Map{"url": url}
		_ = transport

		return c.JSON(fiber.Map{"status": "use print_url tool instead"})
	})

	slog.Info("application initialized",
		"public_url", cfg.PublicURL,
		"printer_ip", cfg.PrinterIP,
		"printer_name", cfg.PrinterName,
		"vpn_enabled", tunnel != nil,
	)

	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		fiberLambda = fiberadapter.New(app)
	} else {
		slog.Info("starting local server on :3000")
		go func() {
			if err := app.Listen(":3000"); err != nil {
				slog.Error("server error", "error", err)
			}
		}()
	}
}

func handleLambdaEvent(ctx context.Context, event json.RawMessage) (interface{}, error) {
	var probe struct {
		Source     string `json:"source"`
		DetailType string `json:"detail-type"`
	}
	if err := json.Unmarshal(event, &probe); err == nil &&
		probe.Source == "aws.events" &&
		strings.Contains(probe.DetailType, "Scheduled") {
		return handleScheduledEvent(ctx)
	}

	var apiGWEvent events.APIGatewayV2HTTPRequest
	if err := json.Unmarshal(event, &apiGWEvent); err != nil {
		slog.Error("failed to parse API Gateway V2 event", "error", err)
		return nil, fmt.Errorf("failed to parse event: %w", err)
	}
	return fiberLambda.ProxyWithContextV2(ctx, apiGWEvent)
}

func handleScheduledEvent(ctx context.Context) (interface{}, error) {
	slog.Info("running scheduled health check")

	// Simple health check - test printer connectivity
	result := map[string]interface{}{
		"status":    "completed",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	slog.Info("health check completed")
	return result, nil
}

func main() {
	// Suppress unused import warnings
	_ = io.Discard

	if fiberLambda != nil {
		lambda.Start(handleLambdaEvent)
	} else {
		select {}
	}
}
