package converter

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/yuin/goldmark"
)

// chrome holds a long-lived Chrome process shared across Lambda invocations.
// Lambda reuses containers on warm invocations, so this avoids the ~15s cold
// startup cost on every PDF request.
var chrome struct {
	mu       sync.Mutex
	allocCtx context.Context
	cancel   context.CancelFunc
}

func getOrStartChrome() (context.Context, error) {
	chrome.mu.Lock()
	defer chrome.mu.Unlock()

	// Reuse if Chrome is still running
	if chrome.allocCtx != nil {
		select {
		case <-chrome.allocCtx.Done():
			// Chrome exited; fall through to restart
			slog.Info("chrome exited, restarting")
		default:
			return chrome.allocCtx, nil
		}
	}

	chromiumPath := os.Getenv("CHROMIUM_PATH")
	if chromiumPath == "" {
		chromiumPath = "/opt/chromium"
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromiumPath),
		chromedp.NoSandbox,
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("single-process", true),
		chromedp.Flag("no-zygote", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-setuid-sandbox", true),
	)

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	chrome.allocCtx = ctx
	chrome.cancel = cancel
	slog.Info("chrome started", "path", chromiumPath)
	return ctx, nil
}

// ToPrintable converts content to a printer-ready format.
// Returns converted data, output MIME type, and any error.
// Pass-through for PDF, images, and plain text.
// Converts Markdown → HTML → PDF and HTML → PDF via headless Chrome.
func ToPrintable(data []byte, mimeType string) ([]byte, string, error) {
	if idx := strings.Index(mimeType, ";"); idx > 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}

	switch mimeType {
	case "text/markdown":
		html, err := markdownToHTML(data)
		if err != nil {
			return nil, "", fmt.Errorf("markdown to HTML: %w", err)
		}
		pdf, err := htmlToPDF(html)
		if err != nil {
			return nil, "", fmt.Errorf("HTML to PDF: %w", err)
		}
		return pdf, "application/pdf", nil

	case "text/html":
		pdf, err := htmlToPDF(data)
		if err != nil {
			return nil, "", fmt.Errorf("HTML to PDF: %w", err)
		}
		return pdf, "application/pdf", nil

	default:
		// PDF, images, plain text — pass through unchanged
		return data, mimeType, nil
	}
}

func markdownToHTML(md []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := goldmark.Convert(md, &buf); err != nil {
		return nil, err
	}
	html := fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><style>
body{font-family:sans-serif;max-width:800px;margin:40px auto;padding:0 20px;line-height:1.6}
pre{background:#f4f4f4;padding:12px;border-radius:4px;overflow-x:auto}
code{background:#f4f4f4;padding:2px 4px;border-radius:2px}
table{border-collapse:collapse;width:100%%}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#f2f2f2}
img{max-width:100%%%%}
</style></head><body>%s</body></html>`, buf.String())
	return []byte(html), nil
}

func htmlToPDF(html []byte) ([]byte, error) {
	allocCtx, err := getOrStartChrome()
	if err != nil {
		return nil, fmt.Errorf("start chrome: %w", err)
	}

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	ctx, cancel = context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	// Use a data URL to load HTML without needing a web server
	dataURL := "data:text/html;base64," + base64.StdEncoding.EncodeToString(html)

	var pdfBuf []byte
	if err := chromedp.Run(ctx,
		chromedp.Navigate(dataURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			pdfBuf, _, err = page.PrintToPDF().
				WithPrintBackground(true).
				WithMarginTop(0.5).
				WithMarginBottom(0.5).
				WithMarginLeft(0.5).
				WithMarginRight(0.5).
				Do(ctx)
			return err
		}),
	); err != nil {
		return nil, err
	}

	return pdfBuf, nil
}
