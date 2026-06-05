package armatureanalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DefaultEndpointURL is the production Armature ingest endpoint. Override via
// Config.EndpointURL or the ANALYTICS_INGEST_URL env var.
const DefaultEndpointURL = "https://app.armature.tech/api/mcp-analytics/ingest"

// DefaultTimeout caps each ingest POST. The TS SDK ships with 500ms; this Go
// SDK uses a slightly more generous default because per-call POSTs run on
// background goroutines off the request path.
const DefaultTimeout = 5 * time.Second

// userAgent identifies this SDK in the User-Agent header so the Armature
// backend can attribute traffic by language.
const userAgent = "mcp-analytics-go/0.1"

// ErrMissingAPIKey is returned by Send when no API key is configured.
var ErrMissingAPIKey = errors.New("armatureanalytics: APIKey is required")

// Client posts batches of analytics events to the Armature ingest endpoint.
// One client is safe for concurrent use.
type Client struct {
	httpClient *http.Client
	endpoint   string
	apiKey     string
}

// NewClient constructs an ingest client. APIKey is required; EndpointURL and
// Timeout fall back to package defaults.
func NewClient(apiKey, endpoint string, timeout time.Duration) (*Client, error) {
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	if endpoint == "" {
		endpoint = DefaultEndpointURL
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		httpClient: &http.Client{Timeout: timeout},
		endpoint:   endpoint,
		apiKey:     apiKey,
	}, nil
}

// Send POSTs a single batch to the Armature ingest endpoint. Returns nil on
// 2xx, an error otherwise.
func (c *Client) Send(ctx context.Context, batch Batch) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshal batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("armature ingest returned %d", resp.StatusCode)
}

// SendEvent is a convenience wrapper around Send that wraps a single event
// in a Batch with the canonical schema_version.
func (c *Client) SendEvent(ctx context.Context, ev Event) error {
	return c.Send(ctx, Batch{
		SchemaVersion: SchemaVersion,
		Events:        []Event{ev},
	})
}
