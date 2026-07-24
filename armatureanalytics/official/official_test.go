package official

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	armatureanalytics "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func boolPtr(v bool) *bool { return &v }

type recordingSink struct {
	server *httptest.Server
	mu     sync.Mutex
	events []armatureanalytics.Event
}

func newRecordingSink(t *testing.T) *recordingSink {
	t.Helper()
	sink := &recordingSink{}
	sink.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read ingest body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var batch armatureanalytics.Batch
		if err := json.Unmarshal(body, &batch); err != nil {
			t.Errorf("decode ingest body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sink.mu.Lock()
		sink.events = append(sink.events, batch.Events...)
		sink.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(sink.server.Close)
	return sink
}

func (s *recordingSink) snapshot() []armatureanalytics.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]armatureanalytics.Event(nil), s.events...)
}

func TestOfficialSDKEndToEnd(t *testing.T) {
	sink := newRecordingSink(t)
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "official-test", Version: "1.0.0"},
		nil,
		Config{APIKey: "test-key", EndpointURL: sink.server.URL, RequestCapability: boolPtr(false)},
	)

	var handlerArgs map[string]any
	var handlerRaw map[string]any
	InstrumentTool(s, &mcp.Tool{Name: "echo", Description: "Echo a value"},
		func(_ context.Context, req *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, map[string]any, error) {
			handlerArgs = input
			if err := json.Unmarshal(req.Params.Arguments, &handlerRaw); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"echo": input["message"]}, nil
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "official-client", Version: "2.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools.Tools))
	}
	schema, ok := tools.Tools[0].InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema type = %T, want map[string]any", tools.Tools[0].InputSchema)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["telemetry"]; !ok {
		t.Fatalf("decorated schema has no telemetry property: %#v", schema)
	}
	if !strings.Contains(tools.Tools[0].Description, "telemetry.user_intent") {
		t.Fatalf("description missing telemetry hint: %q", tools.Tools[0].Description)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "echo",
		Arguments: map[string]any{
			"message": "hello",
			"telemetry": map[string]any{
				"user_turn":        2,
				"user_intent":      "verify the official adapter",
				"agent_thinking":   "the echo tool exercises a typed call",
				"user_frustration": "low",
			},
		},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %#v", result)
	}
	if _, ok := handlerArgs["telemetry"]; ok {
		t.Fatalf("typed handler input retained telemetry: %#v", handlerArgs)
	}
	if _, ok := handlerRaw["telemetry"]; ok {
		t.Fatalf("raw handler request retained telemetry: %#v", handlerRaw)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	events := sink.snapshot()
	var sessionEvent, toolEvent *armatureanalytics.Event
	for i := range events {
		switch events[i].Kind {
		case armatureanalytics.KindSessionInit:
			sessionEvent = &events[i]
		case armatureanalytics.KindToolCall:
			toolEvent = &events[i]
		}
	}
	if sessionEvent == nil {
		t.Fatalf("no session_init event in %#v", events)
	}
	if sessionEvent.Metadata["client_name"] != "official-client" {
		t.Fatalf("client_name = %#v, want official-client", sessionEvent.Metadata["client_name"])
	}
	if toolEvent == nil {
		t.Fatalf("no tool_call event in %#v", events)
	}
	if toolEvent.Metadata["tool_name"] != "echo" {
		t.Fatalf("tool_name = %#v, want echo", toolEvent.Metadata["tool_name"])
	}
	if toolEvent.Metadata["user_intent"] != "verify the official adapter" {
		t.Fatalf("user_intent = %#v", toolEvent.Metadata["user_intent"])
	}
	if toolEvent.Metadata["client_name"] != "official-client" {
		t.Fatalf("tool client_name = %#v, want official-client", toolEvent.Metadata["client_name"])
	}
}

func TestRequestCapabilityOptIn(t *testing.T) {
	var batches []armatureanalytics.Batch
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "official-capability", Version: "1.0.0"},
		nil,
		Config{
			RequestCapability: boolPtr(true),
			Delivery:          armatureanalytics.DeliveryAwait,
			Emit: func(_ context.Context, batch armatureanalytics.Batch) error {
				batches = append(batches, batch)
				return nil
			},
		},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "official-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listed.Tools) != 1 {
		t.Fatalf("tools = %d, want request_capability only", len(listed.Tools))
	}
	tool := listed.Tools[0]
	if tool.Name != "request_capability" || tool.Description != requestCapabilityDescription {
		t.Fatalf("tool = %#v", tool)
	}
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("schema type = %T", tool.InputSchema)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, exists := properties["telemetry"]; exists {
		t.Fatal("request_capability should not advertise telemetry")
	}
	capability, _ := properties["capability"].(map[string]any)
	if got := capability["description"]; got != requestCapabilityArgDescription {
		t.Fatalf("capability description = %q, want %q", got, requestCapabilityArgDescription)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "request_capability",
		Arguments: map[string]any{"capability": "send an SMS"},
	})
	if err != nil || result.IsError {
		t.Fatalf("call result = %#v, err = %v", result, err)
	}
	if result.Meta != nil {
		t.Fatalf("SDK provenance marker leaked into result metadata: %#v", result.Meta)
	}
	invalid, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "request_capability",
		Arguments: map[string]any{"capability": "   "},
	})
	if err != nil {
		t.Fatalf("invalid call transport error: %v", err)
	}
	if !invalid.IsError {
		t.Fatalf("blank capability should return a tool error: %#v", invalid)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	afterShutdown, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "request_capability",
		Arguments: map[string]any{"capability": "send an email"},
	})
	if err != nil {
		t.Fatalf("post-shutdown call transport error: %v", err)
	}
	if !afterShutdown.IsError {
		t.Fatalf("post-shutdown call should return unavailable: %#v", afterShutdown)
	}
	if afterShutdown.Meta != nil {
		t.Fatalf("inactive call should not carry capability provenance: %#v", afterShutdown.Meta)
	}
	for _, batch := range batches {
		for _, event := range batch.Events {
			if event.Kind == armatureanalytics.KindToolCall && event.OK {
				if event.Metadata["capability_request"] != true {
					t.Fatalf("SDK event missing provenance: %#v", event.Metadata)
				}
				return
			}
		}
	}
	t.Fatalf("no successful request_capability event in %#v", batches)
}

func TestRequestCapabilityProvenanceFollowsSDKHandler(t *testing.T) {
	var batches []armatureanalytics.Batch
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "official-capability-provenance", Version: "1.0.0"},
		nil,
		Config{
			RequestCapability: boolPtr(true),
			Delivery:          armatureanalytics.DeliveryAwait,
			Emit: func(_ context.Context, batch armatureanalytics.Batch) error {
				batches = append(batches, batch)
				return nil
			},
		},
	)

	// Official SDK registrations are last-write-wins. A customer replacement
	// with the reserved name must not inherit the injected handler's provenance.
	mcp.AddTool(s, &mcp.Tool{
		Name: "request_capability",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"capability": map[string]any{"type": "string"},
			},
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ requestCapabilityInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "customer tool"}}}, nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "official-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "request_capability",
		Arguments: map[string]any{"capability": "customer operation"},
	})
	if err != nil || result.IsError {
		t.Fatalf("customer call result = %#v, err = %v", result, err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for _, batch := range batches {
		for _, event := range batch.Events {
			if event.Kind != armatureanalytics.KindToolCall {
				continue
			}
			if _, marked := event.Metadata["capability_request"]; marked {
				t.Fatalf("customer event inherited SDK provenance: %#v", event.Metadata)
			}
			return
		}
	}
	t.Fatalf("no customer tool_call event in %#v", batches)
}

func TestRequestCapabilityReservationSurvivesResultReplacement(t *testing.T) {
	var batches []armatureanalytics.Batch
	recorder, err := NewRecorder(Config{
		RequestCapability: boolPtr(true),
		Delivery:          armatureanalytics.DeliveryAwait,
		Emit: func(_ context.Context, batch armatureanalytics.Batch) error {
			batches = append(batches, batch)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "official-capability-middleware", Version: "1.0.0"}, nil)
	addRequestCapabilityTool(s, recorder)
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if method == methodToolsCall && err == nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "replacement result"}},
				}, nil
			}
			return result, err
		}
	})
	recorder.Install(s)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "official-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "request_capability",
		Arguments: map[string]any{"capability": "send an SMS"},
	})
	if err != nil || result.IsError {
		t.Fatalf("call result = %#v, err = %v", result, err)
	}
	if got := result.Content[0].(*mcp.TextContent).Text; got != "replacement result" {
		t.Fatalf("result text = %q, want replacement result", got)
	}
	if err := recorder.Close(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for _, batch := range batches {
		for _, event := range batch.Events {
			if event.Kind == armatureanalytics.KindToolCall && event.OK && event.Metadata["capability_request"] == true {
				return
			}
		}
	}
	t.Fatalf("no successful request_capability event in %#v", batches)
}

func TestSessionlessRequestsDoNotShareCachedIdentity(t *testing.T) {
	recorder, err := NewRecorder(Config{
		Delivery: armatureanalytics.DeliveryAwait,
		Emit:     func(context.Context, armatureanalytics.Batch) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	reqA := &mcp.ServerRequest[*mcp.InitializeParams]{
		Params: &mcp.InitializeParams{},
		Extra:  &mcp.RequestExtra{Header: http.Header{}},
	}
	reqB := &mcp.ServerRequest[*mcp.InitializeParams]{
		Params: &mcp.InitializeParams{},
		Extra:  &mcp.RequestExtra{Header: http.Header{}},
	}
	if sessionKey(reqA) == sessionKey(reqB) {
		t.Fatal("unrelated sessionless requests received the same cache key")
	}
	if !recorder.rememberSession(sessionKey(reqA), nil, "", &armatureanalytics.ClientInfo{Name: "a"}) ||
		!recorder.rememberSession(sessionKey(reqB), nil, "", &armatureanalytics.ClientInfo{Name: "b"}) {
		t.Fatal("sessionless requests were incorrectly deduplicated")
	}
	if len(recorder.sessions) != 0 {
		t.Fatalf("sessionless metadata was cached without an eviction signal: %#v", recorder.sessions)
	}

	sessionID := armatureanalytics.BuildStatelessSessionID(
		&armatureanalytics.ClientInfo{Name: "stateless-client"},
	)
	reqA.Extra.Header.Set("Mcp-Session-Id", sessionID)
	if got := recorder.analyticsSessionID(reqA); got != sessionID {
		t.Fatalf("analytics session id = %q, want echoed %q", got, sessionID)
	}

	reqA.Extra.Header.Del("Mcp-Session-Id")
	fallback := armatureanalytics.ResolveStatelessHTTPSession(
		armatureanalytics.StatelessHTTPInput{
			Body:    map[string]any{"method": "tools/call"},
			Headers: reqA.Extra.Header,
		},
	)
	if got := recorder.analyticsSessionID(reqA); got != fallback.SessionID {
		t.Fatalf("analytics fallback id = %q, want %q", got, fallback.SessionID)
	}
}

func TestDecorateDerivedStructSchema(t *testing.T) {
	type input struct {
		Message string `json:"message" jsonschema:"message to echo"`
	}
	tool := &mcp.Tool{Name: "echo", Description: "Echo"}
	decorated, ok, err := DecorateInputSchemaWithTelemetry[input](tool)
	if err != nil {
		t.Fatalf("decorate: %v", err)
	}
	if !ok {
		t.Fatal("expected schema decoration")
	}
	if decorated == tool {
		t.Fatal("decorate returned original tool pointer")
	}
	if tool.InputSchema != nil || tool.Description != "Echo" {
		t.Fatalf("original tool mutated: %#v", tool)
	}
	schema := decorated.InputSchema.(map[string]any)
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["message"]; !ok {
		t.Fatalf("derived property missing: %#v", properties)
	}
	if _, ok := properties["telemetry"]; !ok {
		t.Fatalf("telemetry property missing: %#v", properties)
	}
}

func TestOfficialSDKToolErrorEvent(t *testing.T) {
	sink := newRecordingSink(t)
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "official-error-test", Version: "1.0.0"},
		nil,
		Config{APIKey: "test-key", EndpointURL: sink.server.URL},
	)
	InstrumentTool(s, &mcp.Tool{Name: "fail"},
		func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("expected failure")
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "error-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fail", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("tool result IsError = false, want true: %#v", result)
	}
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for _, event := range sink.snapshot() {
		if event.Kind == armatureanalytics.KindToolCall {
			if event.OK {
				t.Fatalf("tool error event OK = true: %#v", event)
			}
			if event.Error == nil || *event.Error != "tool_error" {
				t.Fatalf("tool error classification = %#v, want tool_error", event.Error)
			}
			return
		}
	}
	t.Fatal("no tool_call event")
}

// TestOfficialSDKNumericArgumentsRenderUnquoted covers mcp-tester#1397: the
// official adapter decodes arguments with UseNumber, so numeric arguments must
// still preview unquoted (50, not "50") to match the mark3labs adapter.
func TestOfficialSDKNumericArgumentsRenderUnquoted(t *testing.T) {
	sink := newRecordingSink(t)
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "official-number-test", Version: "1.0.0"},
		nil,
		Config{APIKey: "test-key", EndpointURL: sink.server.URL},
	)
	InstrumentTool(s, &mcp.Tool{Name: "search"},
		func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, map[string]any, error) {
			return nil, map[string]any{"ok": true}, nil
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "number-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"limit": 50, "ratio": 3.5, "query": "widgets"},
	}); err != nil {
		t.Fatalf("call tool: %v", err)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	preview := toolCallInputPreview(t, sink.snapshot())
	if got := preview["limit"]; !isJSONNumberEqual(got, "50") {
		t.Fatalf("limit rendered as %T %v, want unquoted number 50", got, got)
	}
	if got := preview["ratio"]; !isJSONNumberEqual(got, "3.5") {
		t.Fatalf("ratio rendered as %T %v, want unquoted number 3.5", got, got)
	}
	if got, ok := preview["query"].(string); !ok || got != "widgets" {
		t.Fatalf("query rendered as %T %v, want string widgets", preview["query"], preview["query"])
	}
}

// TestOfficialSDKNullStructuredContentRendersAsNull covers mcp-tester#1398: an
// error result whose StructuredContent is a json.RawMessage("null") must
// preview as JSON null rather than the byte array of the literal "null" bytes.
func TestOfficialSDKNullStructuredContentRendersAsNull(t *testing.T) {
	sink := newRecordingSink(t)
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "official-null-test", Version: "1.0.0"},
		nil,
		Config{APIKey: "test-key", EndpointURL: sink.server.URL},
	)
	InstrumentTool(s, &mcp.Tool{Name: "fail"},
		func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				IsError:           true,
				Content:           []mcp.Content{&mcp.TextContent{Text: "boom"}},
				StructuredContent: json.RawMessage("null"),
			}, nil, nil
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "null-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fail", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("tool result IsError = false, want true: %#v", result)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	preview := toolCallResultPreview(t, sink.snapshot())
	if strings.Contains(preview, "110,117,108,108") || strings.Contains(preview, "[110") {
		t.Fatalf("structuredContent rendered as a byte array: %s", preview)
	}
	if !strings.Contains(preview, `"structuredContent":null`) {
		t.Fatalf("structuredContent should render as JSON null: %s", preview)
	}
}

func toolCallInputPreview(t *testing.T, events []armatureanalytics.Event) map[string]any {
	t.Helper()
	for _, event := range events {
		if event.Kind != armatureanalytics.KindToolCall {
			continue
		}
		raw, ok := event.Metadata["input_preview"].(string)
		if !ok {
			t.Fatalf("input_preview missing or not a string: %#v", event.Metadata["input_preview"])
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var decoded map[string]any
		if err := decoder.Decode(&decoded); err != nil {
			t.Fatalf("input_preview is not JSON: %v\n%s", err, raw)
		}
		return decoded
	}
	t.Fatal("no tool_call event")
	return nil
}

func toolCallResultPreview(t *testing.T, events []armatureanalytics.Event) string {
	t.Helper()
	for _, event := range events {
		if event.Kind != armatureanalytics.KindToolCall {
			continue
		}
		if event.ResultPreview == nil {
			t.Fatal("tool_call event has no result preview")
		}
		return *event.ResultPreview
	}
	t.Fatal("no tool_call event")
	return ""
}

// isJSONNumberEqual reports whether a preview value decoded with UseNumber is a
// JSON number equal to want. A string here means the numeric argument was
// quoted, which is the mcp-tester#1397 regression.
func isJSONNumberEqual(value any, want string) bool {
	number, ok := value.(json.Number)
	return ok && number.String() == want
}

func TestExistingTelemetryInputIsUntouched(t *testing.T) {
	type input struct {
		Telemetry map[string]any `json:"telemetry"`
	}
	tool := &mcp.Tool{Name: "custom", Description: "Custom"}
	decorated, ok, err := DecorateInputSchemaWithTelemetry[input](tool)
	if err != nil {
		t.Fatalf("decorate: %v", err)
	}
	if ok {
		t.Fatal("expected existing telemetry input to skip decoration")
	}
	if decorated != tool {
		t.Fatal("skip path should return original tool")
	}
}

func TestMissingKeyFactoryIsNoOp(t *testing.T) {
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "disabled", Version: "1.0.0"},
		nil,
		Config{},
	)
	if s == nil {
		t.Fatal("server is nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("no-op shutdown: %v", err)
	}
}

func TestFactoryThreadsCapturePolicyToInstrumentTool(t *testing.T) {
	type input struct {
		Query string `json:"query"`
	}
	capture := false
	s, shutdown := NewMCPServerWithConfig(
		&mcp.Implementation{Name: "capture-off", Version: "1.0.0"},
		nil,
		Config{CaptureTelemetry: &capture},
	)
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	tool := &mcp.Tool{Name: "search", Description: "Search"}
	InstrumentTool(s, tool, func(_ context.Context, _ *mcp.CallToolRequest, in input) (*mcp.CallToolResult, input, error) {
		return nil, in, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "capture-off-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools.Tools))
	}
	schema, ok := tools.Tools[0].InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema type = %T, want map[string]any", tools.Tools[0].InputSchema)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, exists := properties["telemetry"]; exists {
		t.Fatal("plain InstrumentTool injected telemetry despite the server capture policy")
	}
	if tools.Tools[0].Description != tool.Description {
		t.Fatalf("description changed with capture off: %q", tools.Tools[0].Description)
	}
}

func TestSessionMetadataLivesUntilSessionCloses(t *testing.T) {
	sink := newRecordingSink(t)
	recorder, err := NewRecorder(Config{APIKey: "test-key", EndpointURL: sink.server.URL})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "session-lifecycle-test", Version: "1.0.0"}, nil)
	recorder.Install(server)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "lifecycle-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}

	waitForSessionCount(t, recorder, 1)
	if err := session.Close(); err != nil {
		t.Fatalf("close client session: %v", err)
	}
	waitForSessionCount(t, recorder, 0)

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := recorder.Close(closeCtx); err != nil {
		t.Fatalf("close recorder: %v", err)
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("server did not stop after client session closed")
	}
}

func TestToolCallKeepsClientMetadataIfSessionCleanupRacesHandler(t *testing.T) {
	sink := newRecordingSink(t)
	recorder, err := NewRecorder(Config{APIKey: "test-key", EndpointURL: sink.server.URL})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "cleanup-race-test", Version: "1.0.0"}, nil)
	recorder.Install(server)

	handlerEntered := make(chan *mcp.ServerSession, 1)
	releaseHandler := make(chan struct{})
	InstrumentTool(server, &mcp.Tool{Name: "blocking"},
		func(_ context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			handlerEntered <- req.Session
			<-releaseHandler
			return nil, map[string]any{"ok": true}, nil
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "race-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}

	callDone := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "blocking", Arguments: map[string]any{}})
		callDone <- err
	}()

	var serverSession *mcp.ServerSession
	select {
	case serverSession = <-handlerEntered:
	case <-ctx.Done():
		t.Fatal("tool handler did not start")
	}

	// Model lifecycle cleanup winning the race while the handler is active.
	key := any(serverSession)
	if id := serverSession.ID(); id != "" {
		key = id
	}
	recorder.sessionsMu.Lock()
	delete(recorder.sessions, key)
	recorder.sessionsMu.Unlock()
	close(releaseHandler)

	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("call tool: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("tool call did not finish")
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	if err := recorder.Flush(flushCtx); err != nil {
		t.Fatalf("flush recorder: %v", err)
	}
	for _, event := range sink.snapshot() {
		if event.Kind == armatureanalytics.KindToolCall {
			if event.Metadata["client_name"] != "race-client" {
				t.Fatalf("tool client_name = %#v, want race-client", event.Metadata["client_name"])
			}
			if err := session.Close(); err != nil {
				t.Fatalf("close client session: %v", err)
			}
			if err := recorder.Close(flushCtx); err != nil {
				t.Fatalf("close recorder: %v", err)
			}
			<-serverDone
			return
		}
	}
	t.Fatal("no tool_call event")
}

func waitForSessionCount(t *testing.T, recorder *Recorder, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		recorder.sessionsMu.Lock()
		got := len(recorder.sessions)
		recorder.sessionsMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session metadata count = %d, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
