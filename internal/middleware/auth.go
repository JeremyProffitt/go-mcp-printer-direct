package middleware

import (
	"github.com/gofiber/fiber/v2"

	"go-mcp-printer-direct/internal/oauth"
	"go-mcp-printer-direct/internal/token"
)

func BearerAuth(kp *token.KeyPair) fiber.Handler {
	return oauth.BearerAuthMiddleware(kp)
}
