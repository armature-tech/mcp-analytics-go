package armatureanalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultEndpointURL is the production Armature ingest endpoint. Override via
// Config.EndpointURL or the ANALYTICS_INGEST_URL env var.
const DefaultEndpointURL = "https://app.armature.tech/api/mcp-analytics/ingest"

// DefaultTimeout caps each ingest POST consistently across all Armature SDKs.
const DefaultTimeout = 5 * time.Second

const DefaultIngestMaxAttempts = 2
const DefaultIngestRetryDelay = 100 * time.Millisecond

// userAgent identifies this SDK in the User-Agent header so the Armature
// backend can attribute traffic by language.
const userAgent = "mcp-analytics-go/0.1"

// ErrMissingAPIKey is returned by Send when no API key is configured.
var ErrMissingAPIKey = errors.New("armatureanalytics: APIKey is required")

// DeliveryError classifies a failed ingest delivery without including the API
// key or event payload. Callers can inspect it from Config.OnError.
type DeliveryError struct {
	Code      string
	Status    int
	Attempts  int
	Retryable bool
	Err       error
}

func (e *DeliveryError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("armature ingest failed with HTTP %d (%s) after %d attempt(s)", e.Status, e.Code, e.Attempts)
	}
	return fmt.Sprintf("armature ingest failed (%s) after %d attempt(s)", e.Code, e.Attempts)
}

func (e *DeliveryError) Unwrap() error { return e.Err }

// ingestRejection is one refused event in the ingest response body.
type ingestRejection struct {
	EventID *string `json:"event_id"`
	Reason  string  `json:"reason"`
}

// ingestResponse is the HTTP 200 body of POST /api/mcp-analytics/ingest.
// Accepted is a pointer so an absent field (a differently-shaped 200 from a
// proxy/gateway) is distinguishable from a genuine accepted:0 — matching the
// TS/Python emitters, which treat a missing count as unobservable rather than
// as "nothing accepted".
type ingestResponse struct {
	Accepted       *int              `json:"accepted"`
	Rejected       []ingestRejection `json:"rejected"`
	DuplicateCount int               `json:"duplicate_count"`
}

// IngestRejectionError reports that ingest answered HTTP 200 but refused events
// in its body — validation, quota, schema drift, or nothing accepted from a
// non-empty batch. Checking only the status code hides this (#1403); Send
// returns it so Config.OnError fires. Server-side dedup counts as accepted, so
// benign session_init re-delivery does not produce this error.
type IngestRejectionError struct {
	Rejected []ingestRejection
	Accepted int
}

func (e *IngestRejectionError) Error() string {
	seen := map[string]struct{}{}
	var reasons []string
	for _, r := range e.Rejected {
		if r.Reason == "" {
			continue
		}
		if _, ok := seen[r.Reason]; ok {
			continue
		}
		seen[r.Reason] = struct{}{}
		reasons = append(reasons, r.Reason)
	}
	sort.Strings(reasons)
	detail := ""
	if len(reasons) > 0 {
		detail = " (" + strings.Join(reasons, ", ") + ")"
	}
	return fmt.Sprintf("armature ingest rejected %d event(s)%s", len(e.Rejected), detail)
}

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

	for attempt := 1; attempt <= DefaultIngestMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("User-Agent", userAgent)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				code := "ingest_cancelled"
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					code = "ingest_timeout"
				}
				return &DeliveryError{Code: code, Attempts: attempt, Retryable: code == "ingest_timeout", Err: ctx.Err()}
			}
			if attempt < DefaultIngestMaxAttempts {
				if err := waitForRetry(ctx); err != nil {
					return &DeliveryError{Code: "ingest_timeout", Attempts: attempt, Retryable: true, Err: err}
				}
				continue
			}
			code := "ingest_connection_failed"
			if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
				code = "ingest_timeout"
			}
			return &DeliveryError{Code: code, Attempts: attempt, Retryable: true, Err: err}
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			bodyError := ingestBodyError(resp.Body, len(batch.Events))
			resp.Body.Close()
			return bodyError
		}
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4_096))
		resp.Body.Close()
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if retryable && attempt < DefaultIngestMaxAttempts {
			if err := waitForRetry(ctx); err != nil {
				return &DeliveryError{Code: "ingest_timeout", Status: resp.StatusCode, Attempts: attempt, Retryable: true, Err: err}
			}
			continue
		}
		return &DeliveryError{
			Code:      ingestResponseCode(detail, resp.StatusCode),
			Status:    resp.StatusCode,
			Attempts:  attempt,
			Retryable: retryable,
		}
	}
	return &DeliveryError{Code: "ingest_delivery_failed", Attempts: DefaultIngestMaxAttempts}
}

func waitForRetry(ctx context.Context) error {
	timer := time.NewTimer(DefaultIngestRetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func ingestResponseCode(body []byte, status int) string {
	fallback := fmt.Sprintf("ingest_http_%d", status)
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		ErrorCode string `json:"errorCode"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return fallback
	}
	code := payload.Error.Code
	if code == "" {
		code = payload.ErrorCode
	}
	if !validDiagnosticCode(code) {
		return fallback
	}
	return code
}

func validDiagnosticCode(code string) bool {
	if len(code) == 0 || len(code) > 100 {
		return false
	}
	for index, char := range code {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		if index > 0 && (char == '_' || char == ':' || char == '-') {
			continue
		}
		return false
	}
	return true
}

// ingestBodyError inspects a 200 response body and returns an
// *IngestRejectionError when ingest refused events (#1403). A non-JSON body
// just means rejections are unobservable, not a delivery failure.
func ingestBodyError(body io.Reader, eventCount int) error {
	raw, err := io.ReadAll(body)
	if err != nil || len(raw) == 0 {
		return nil
	}
	var parsed ingestResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	accepted := 0
	if parsed.Accepted != nil {
		accepted = *parsed.Accepted
	}
	if len(parsed.Rejected) > 0 {
		return &IngestRejectionError{Rejected: parsed.Rejected, Accepted: accepted}
	}
	// Only "nothing accepted" when the count is actually present and zero; an
	// absent count means an unexpected body shape, treated as delivered.
	if parsed.Accepted != nil && accepted == 0 && eventCount > 0 {
		return &IngestRejectionError{Rejected: nil, Accepted: 0}
	}
	return nil
}

// SendEvent is a convenience wrapper around Send that wraps a single event
// in a Batch with the canonical schema_version.
func (c *Client) SendEvent(ctx context.Context, ev Event) error {
	return c.Send(ctx, Batch{
		SchemaVersion: SchemaVersion,
		Events:        []Event{ev},
	})
}
