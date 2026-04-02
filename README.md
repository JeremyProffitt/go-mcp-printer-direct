# go-mcp-printer-direct

A serverless MCP (Model Context Protocol) server and Alexa Custom Skill that provides direct network printer control via voice and AI assistants. Runs on AWS Lambda with OAuth 2.1 authentication and a WireGuard VPN tunnel to reach printers on a private home network.

## What It Does

This single Lambda function serves two interfaces to the same printer:

- **MCP Server** for Claude (claude.ai, Claude Code, and other MCP clients) -- query printer status, check ink levels, print documents, manage print jobs
- **Alexa Custom Skill** for voice control -- "Alexa, ask my printer to check ink levels"

Both interfaces share the same printer communication layer (IPP/SNMP), VPN tunnel, and OAuth authentication.

## Architecture

```
                          +------------------+
                          |  AWS Lambda      |
                          |  (Go, arm64)     |
  Claude / MCP Client --> |                  |
    (API Gateway HTTP)    |  Event Router    |
                          |   |              |
  Alexa Echo Device ----> |   +-> MCP Handler (JSON-RPC 2.0)
    (Direct Invoke)       |   +-> Alexa Handler (ASK JSON)
                          |   +-> Keepalive (CloudWatch Schedule)
  CloudWatch Schedule --> |   |              |
                          |  Printer Clients |
                          |   +-> IPP (631)  |-----> WireGuard -----> Printer
                          |   +-> SNMP (161) |       VPN Tunnel       (Home Network)
                          +------------------+
                                 |
                          +------+------+
                          |  DynamoDB   |  (OAuth state: clients,
                          |  (single    |   auth codes, refresh tokens)
                          |   table)    |
                          +-------------+
```

### Key Technologies

| Component | Technology |
|-----------|-----------|
| Runtime | Go on AWS Lambda (arm64, provided.al2023) |
| Web Framework | GoFiber v2 + aws-lambda-go-api-proxy |
| Printer Protocol | IPP (Internet Printing Protocol) over HTTP |
| Supply Monitoring | SNMP v2c (RFC 3805) |
| Authentication | OAuth 2.1 with PKCE, Ed25519 JWTs |
| Storage | DynamoDB single-table design with TTL |
| VPN | WireGuard userspace (pure Go netstack, no kernel module) |
| Observability | OpenTelemetry + CloudWatch Logs |
| Deployment | AWS SAM + GitHub Actions CI/CD |

### Event Routing

The Lambda handles three event types through a discriminated-union pattern:

1. **CloudWatch Scheduled Events** -- printer keepalive every 55 minutes (IPP + SNMP queries to prevent printer sleep)
2. **Alexa Skill Requests** -- direct Lambda invocations with ASK JSON format (`version`, `session`, `request` fields)
3. **API Gateway HTTP Requests** -- MCP JSON-RPC, OAuth endpoints, health checks

## Available Printer Operations

These operations are available through both MCP tools and Alexa voice commands:

| Operation | MCP Tool | Alexa Voice Command |
|-----------|----------|-------------------|
| Printer status | `get_printer_info` | "check printer status" |
| Ink/toner levels | `get_ink_levels` | "check ink levels" |
| Print text | `print_text` | "print [text]" |
| Print from URL | `print_url` | -- |
| Print queue | `get_print_queue` | "check the print queue" |
| Job status | `get_job_status` | "check job [number]" |
| Cancel job | `cancel_job` | "cancel job [number]" |
| Test connectivity | `test_connectivity` | "test connectivity" |

## Project Structure

```
go-mcp-printer-direct/
  cmd/lambda/              Lambda entry point & event routing
  internal/
    alexa/                 Alexa Custom Skill handler
      types.go             ASK request/response structs
      handler.go           Request parsing, validation, dispatch
      intents.go           Intent handlers (one per voice command)
      speech.go            Speech formatting & response builders
    mcp/                   MCP JSON-RPC protocol handler
      handler.go           Tool dispatch, resources, prompts
    printer/               Printer communication
      ipp.go               IPP binary protocol client
      snmp.go              SNMP supply level queries
      types.go             Shared data types
      connectivity.go      Port connectivity testing
    oauth/                 OAuth 2.1 server
      handler.go           Route registration
      metadata.go          .well-known endpoints
      register.go          Dynamic client registration
      authorize.go         Authorization code flow + login page
      token.go             Token exchange + refresh
      middleware.go        Bearer token validation middleware
    token/                 Ed25519 JWT signing/validation
    store/                 DynamoDB persistence
    vpn/                   WireGuard userspace tunnel
    config/                Environment-based configuration
    middleware/            HTTP middleware (logging, tracing)
    telemetry/             OpenTelemetry setup
  alexa/                   Alexa skill configuration
    skill.json             Skill manifest
    interactionModels/
      custom/
        en-US.json         Interaction model (intents, utterances)
  template.yaml            AWS SAM template
  .github/workflows/
    deploy.yml             CI/CD pipeline
```

## Setup Guide

### Prerequisites

- Go 1.25+
- AWS CLI configured with credentials
- AWS SAM CLI
- An HP Color LaserJet (or compatible IPP printer) on your network
- (Optional) WireGuard VPN to reach a private network from Lambda

### Build

```bash
make build
# or manually:
cd cmd/lambda && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap .
```

### Test

```bash
go test -v ./...
```

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PUBLIC_URL` | No | `http://localhost:3000` | OAuth issuer URL (your domain) |
| `ADMIN_USER` | No | `admin` | Admin username for OAuth login |
| `ADMIN_PASSWORD` | Yes | -- | Bcrypt-hashed admin password |
| `DYNAMODB_TABLE` | No | `mcp-printer-direct-oauth` | DynamoDB table name |
| `JWT_SIGNING_KEY_ARN` | No | -- | Secrets Manager ARN for Ed25519 key pair |
| `WG_CONFIG_SECRET_ARN` | No | -- | Secrets Manager ARN for WireGuard config |
| `PRINTER_IP` | No | `192.168.1.244` | Printer IP address |
| `PRINTER_NAME` | No | `HP Color LaserJet MFP M283fdw` | Friendly printer name |
| `ALEXA_SKILL_ID` | No | -- | Alexa skill application ID (enables Alexa) |
| `OTEL_ENDPOINT` | No | `http://192.168.1.202:4318` | OpenTelemetry OTLP endpoint |
| `ACCESS_TOKEN_TTL` | No | `3600` | JWT access token TTL in seconds |
| `REFRESH_TOKEN_TTL` | No | `604800` | Refresh token TTL in seconds |

### Deploy

```bash
# Via SAM CLI
sam deploy --guided

# Or via GitHub Actions (push to main)
git push origin main
```

The GitHub Actions workflow builds the Go binary, runs tests, and deploys via SAM. Configure these GitHub repository secrets/variables:

| GitHub Secret/Variable | Type | Description |
|----------------------|------|-------------|
| `AWS_ACCESS_KEY_ID` | Secret | AWS credentials |
| `AWS_SECRET_ACCESS_KEY` | Secret | AWS credentials |
| `ADMIN_PASSWORD` | Secret | Bcrypt hash of admin password |
| `JWT_SIGNING_KEY_ARN` | Secret | Secrets Manager ARN |
| `WG_CONFIG_SECRET_ARN` | Secret | Secrets Manager ARN |
| `ADMIN_USER` | Variable | Admin username |
| `DOMAIN_NAME` | Variable | Custom domain (e.g., `mcp-printer.jeremy.ninja`) |
| `HOSTED_ZONE_ID` | Variable | Route53 hosted zone ID |
| `CERTIFICATE_ARN_US_EAST_2` | Variable | ACM certificate ARN |
| `CLOUDFORMATION_S3_BUCKET` | Variable | S3 bucket for SAM artifacts |
| `PRINTER_IP` | Variable | Printer IP address |
| `ALEXA_SKILL_ID` | Variable | Alexa skill ID (optional) |

---

## Setting Up for Claude.ai (MCP)

### Step 1: Deploy the Server

Deploy the Lambda using SAM or GitHub Actions as described above. You need:
- A custom domain with HTTPS (ACM certificate + Route53)
- A DynamoDB table (created by SAM)
- Ed25519 JWT signing keys in Secrets Manager
- (If printer is on a private network) WireGuard VPN config in Secrets Manager

### Step 2: Verify the Server

```bash
# Health check
curl https://mcp-printer.your-domain.com/health

# OAuth metadata
curl https://mcp-printer.your-domain.com/.well-known/oauth-authorization-server
```

### Step 3: Connect Claude.ai

1. Go to [claude.ai](https://claude.ai) Settings > Integrations
2. Click "Add Integration" or "Add MCP Server"
3. Enter your server URL: `https://mcp-printer.your-domain.com/mcp`
4. Claude will discover the OAuth metadata automatically from `/.well-known/oauth-authorization-server`
5. You'll be redirected to the login page -- enter your admin credentials
6. After authorization, Claude can use all 8 printer tools

### Step 4: Use with Claude

Once connected, you can ask Claude things like:

- "Check the printer status"
- "What are the current ink levels?"
- "Print 'Hello World' on the printer"
- "Show me the print queue"
- "Cancel print job 42"
- "Run a full printer diagnostic" (uses the `diagnose-printer` prompt)

### MCP Endpoints

| Endpoint | Method | Auth | Purpose |
|----------|--------|------|---------|
| `/mcp` | POST | Bearer JWT | Main MCP JSON-RPC endpoint |
| `/mcp` | GET | Bearer JWT | SSE transport endpoint discovery |
| `/` | POST | Bearer JWT | Root MCP endpoint (Claude default) |
| `/printer` | POST | Bearer JWT | Alternate MCP path |
| `/.well-known/oauth-authorization-server` | GET | None | OAuth metadata |
| `/.well-known/oauth-protected-resource` | GET | None | Resource metadata |
| `/register` | POST | None | Dynamic client registration |
| `/authorize` | GET/POST | None | Authorization (login page) |
| `/token` | POST | None | Token exchange |
| `/health` | GET | None | Health check |

### MCP Tools

The server exposes 8 tools via MCP:

**Read-only tools:**
- `get_printer_info` -- printer model, status, capabilities, paper sizes
- `get_ink_levels` -- toner/ink supply levels via SNMP
- `get_print_queue` -- list all active and pending print jobs
- `get_job_status` -- check a specific job by ID
- `test_connectivity` -- test IPP, SNMP, JetDirect, and HTTP ports

**Destructive tools (marked with `destructiveHint`):**
- `print_text` -- print plain text content
- `print_url` -- download and print from a URL (PDF, images, HTML)
- `cancel_job` -- cancel a print job by ID

### MCP Resources

- `printer://info` -- current printer status (JSON)
- `printer://supplies` -- current supply levels (JSON)
- `printer://help` -- help guide (Markdown)

### MCP Prompts

- `diagnose-printer` -- run a full diagnostic (connectivity, status, ink, queue)
- `supply-check` -- check supplies and flag low/critical levels
- `print-document` -- print a document with smart defaults

---

## Setting Up for Alexa

### Step 1: Create the Alexa Skill

1. Go to the [Alexa Developer Console](https://developer.amazon.com/alexa/console/ask)
2. Click "Create Skill"
3. Choose:
   - Skill name: **My Printer**
   - Primary locale: **English (US)**
   - Type: **Custom**
   - Hosting: **Provision your own**
4. Click "Create Skill", then choose **Start from Scratch**

### Step 2: Configure the Interaction Model

1. In the Alexa Developer Console, go to **Build** > **Interaction Model** > **JSON Editor**
2. Paste the contents of `alexa/interactionModels/custom/en-US.json`
3. Click **Save** then **Build Model**

This defines the invocation name ("my printer") and all intents:

| Voice Command | Intent |
|--------------|--------|
| "check printer status" | GetPrinterStatusIntent |
| "check ink levels" | GetInkLevelsIntent |
| "print [text]" | PrintTextIntent |
| "check the print queue" | GetPrintQueueIntent |
| "check job [number]" | GetJobStatusIntent |
| "cancel job [number]" | CancelJobIntent |
| "test connectivity" | TestConnectivityIntent |

### Step 3: Set the Endpoint

1. Go to **Build** > **Endpoint**
2. Select **AWS Lambda ARN**
3. Paste your Lambda function ARN (from the SAM deploy output `FunctionArn`)
4. Copy the **Skill ID** shown at the top (e.g., `amzn1.ask.skill.xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`)

### Step 4: Configure the Lambda

Add the Alexa Skill ID to your deployment:

**Option A: SAM parameter**
```bash
sam deploy --parameter-overrides AlexaSkillId=amzn1.ask.skill.your-skill-id ...
```

**Option B: GitHub Actions variable**
1. Go to your repo Settings > Secrets and Variables > Actions
2. Add a variable `ALEXA_SKILL_ID` with your skill ID value
3. Push to main to trigger a deploy

The SAM template automatically creates the Lambda invoke permission for the Alexa service principal when `AlexaSkillId` is set.

### Step 5: Configure Account Linking

Account linking connects your Alexa user to your printer server's OAuth identity.

1. In the Alexa Developer Console, go to **Build** > **Account Linking**
2. Toggle **Do you allow users to create an account or link to an existing account?** to **ON**
3. Configure:
   - **Authorization Grant Type**: Auth Code Grant
   - **Authorization URI**: `https://mcp-printer.your-domain.com/authorize`
   - **Access Token URI**: `https://mcp-printer.your-domain.com/token`
   - **Client ID**: (you'll register this -- see below)
   - **Client Secret**: (leave empty or use a placeholder)
   - **Client Authentication Scheme**: HTTP Body
   - **Scope**: `printer`
4. Note the **Alexa Redirect URLs** shown (e.g., `https://layla.amazon.com/api/skill/link/...` and `https://pitangui.amazon.com/api/skill/link/...`)

**Register the Alexa OAuth client:**

Make a POST request to register Alexa as an OAuth client:

```bash
curl -X POST https://mcp-printer.your-domain.com/register \
  -H "Content-Type: application/json" \
  -d '{
    "client_name": "Alexa Skill",
    "redirect_uris": [
      "https://layla.amazon.com/api/skill/link/YOUR_VENDOR_ID",
      "https://pitangui.amazon.com/api/skill/link/YOUR_VENDOR_ID",
      "https://alexa.amazon.co.jp/api/skill/link/YOUR_VENDOR_ID"
    ],
    "grant_types": ["authorization_code", "refresh_token"],
    "response_types": ["code"],
    "token_endpoint_auth_method": "none",
    "scope": "printer"
  }'
```

The response will include a `client_id` -- use this in the Account Linking configuration above.

### Step 6: Test the Skill

1. Go to the **Test** tab in the Alexa Developer Console
2. Enable testing in **Development** mode
3. Type or say: "ask my printer to check ink levels"

You can also test on a real Echo device linked to the same Amazon account.

### Step 7: Link Your Account

1. Open the **Alexa app** on your phone
2. Go to **More** > **Skills & Games** > **Your Skills** > **Dev Skills**
3. Find "My Printer" and tap **Enable**
4. You'll be redirected to the OAuth login page
5. Enter your admin credentials
6. After successful login, your account is linked

### Alexa Voice Commands

Once set up, use your Echo device (or the Alexa app) with these commands:

```
"Alexa, ask my printer to check printer status"
"Alexa, ask my printer to check ink levels"
"Alexa, ask my printer to print hello world"
"Alexa, ask my printer to check the print queue"
"Alexa, ask my printer to check job 42"
"Alexa, ask my printer to cancel job 42"
"Alexa, ask my printer to test connectivity"
"Alexa, ask my printer for help"
```

Destructive actions (printing, cancelling) will ask for confirmation:
> "I'll print 'hello world'. Should I go ahead?"
> "Yes"
> "Print job 42 has been submitted successfully."

### Alexa Response Cards

All responses include visual cards for devices with screens (Echo Show, Fire TV, Alexa app). Cards show structured data like:

- **Printer Status**: Model, IP, state, capabilities
- **Ink Levels**: Color-by-color percentage breakdown
- **Print Queue**: Job list with IDs and states
- **Connectivity**: Port-by-port reachable/unreachable status

---

## OAuth 2.1 Flow

The server implements a complete OAuth 2.1 authorization code flow with PKCE, used by both Claude and Alexa:

```
1. Client registers via POST /register (Dynamic Client Registration)
2. Client redirects user to GET /authorize?client_id=...&redirect_uri=...&code_challenge=...&state=...
3. Server shows login page
4. User submits credentials via POST /authorize
5. Server validates credentials, generates auth code, redirects to redirect_uri
6. Client exchanges code for tokens via POST /token (with code_verifier for PKCE)
7. Server issues Ed25519 JWT access token (1 hour) + refresh token (7 days)
8. Client uses access token in Authorization: Bearer header
9. On expiry, client uses refresh token to get a new access token
```

### DynamoDB Schema

Single-table design with TTL for automatic cleanup:

| PK | SK | TTL | Purpose |
|----|----|----|---------|
| `CLIENT#<id>` | `CLIENT` | No | OAuth client registration |
| `CODE#<code>` | `<code>` | 5 min | Authorization codes |
| `RT#<token>` | `<token>` | 7 days | Refresh tokens |

---

## WireGuard VPN

The Lambda includes a userspace WireGuard implementation (pure Go, no kernel module). This creates a VPN tunnel from the Lambda to your home network, allowing it to reach printers that aren't publicly accessible.

**How it works:**
1. WireGuard config is stored in AWS Secrets Manager
2. On Lambda cold start, the tunnel is established using the `golang.zx2c4.com/wireguard` netstack
3. All printer communication (IPP, SNMP) is routed through the tunnel via a custom `dialFunc`
4. A scheduled keepalive runs every 55 minutes to prevent the Lambda from going cold and to keep the printer awake

**Config format** (stored in Secrets Manager as JSON):
```json
{
  "private_key": "base64-encoded-private-key",
  "endpoint": "your-wireguard-server:51820",
  "public_key": "base64-encoded-server-public-key",
  "allowed_ips": "192.168.1.0/24",
  "address": "10.0.0.2/32",
  "dns": "192.168.1.1"
}
```

---

## Printer Communication

### IPP (Internet Printing Protocol)

The server implements IPP operations directly in Go (no external libraries), communicating with the printer on port 631:

- `Print-Job` (0x0002) -- submit print jobs
- `Cancel-Job` (0x0008) -- cancel print jobs
- `Get-Job-Attributes` (0x0009) -- query job status
- `Get-Jobs` (0x000A) -- list print queue
- `Get-Printer-Attributes` (0x000B) -- query printer info

### SNMP (Simple Network Management Protocol)

Supply levels are queried via SNMP v2c on port 161 using RFC 3805 Printer MIB OIDs:

| OID | Data |
|-----|------|
| `1.3.6.1.2.1.43.11.1.1.6.1.*` | Supply description |
| `1.3.6.1.2.1.43.11.1.1.8.1.*` | Max capacity |
| `1.3.6.1.2.1.43.11.1.1.9.1.*` | Current level |
| `1.3.6.1.2.1.43.12.1.1.4.1.*` | Colorant value (color name) |

---

## Troubleshooting

### Cold Starts

The Lambda has a ~2-3 second cold start due to WireGuard tunnel establishment. Alexa has an 8-second response timeout. The 55-minute keepalive schedule keeps the Lambda warm under normal use.

### Alexa "Please link your account"

This means the access token is missing or expired. Re-link your account in the Alexa app (Skills > My Printer > Settings > Link Account).

### Printer Unreachable

1. Check the VPN tunnel is configured (`WG_CONFIG_SECRET_ARN`)
2. Use the `test_connectivity` tool/intent to check all ports
3. Verify the printer IP is correct (`PRINTER_IP`)
4. Check if the printer is in sleep mode (the keepalive should prevent this)

### MCP Connection Issues in Claude

1. Verify the OAuth metadata endpoint returns valid JSON: `curl https://your-domain/.well-known/oauth-authorization-server`
2. Check that the domain has a valid SSL certificate
3. Try the health endpoint: `curl https://your-domain/health`
