package armatureanalytics

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// StatelessSessionHeader is the internal request header
// (*Recorder).WrapStatelessHTTPHandler uses to hand the resolved legacy
// session identity to the instrumented MCP server behind it. go-sdk >= v1.7.0
// stateless transports ignore Mcp-Session-Id entirely (and a stateful
// transport would treat an injected one as a session lookup), so the repair
// travels on a header only the analytics adapters read. Treat the value as
// observability, never authentication.
const StatelessSessionHeader = "X-Armature-Stateless-Session-Id"

type statelessSessionContextKey struct{}

// StatelessHTTPSessionFromRequest returns the session WrapStatelessHTTPHandler
// resolved for this request. ok is false on unwrapped requests and on wrapped
// requests that carried no session signal (2026-07-28 traffic or a lost echo).
// Handlers that construct a per-request MCP server should prefer this over
// calling ResolveStatelessHTTPSession again: a second resolve of an initialize
// body mints a DIFFERENT id than the one the middleware attached.
func StatelessHTTPSessionFromRequest(r *http.Request) (StatelessHTTPSession, bool) {
	session, ok := r.Context().Value(statelessSessionContextKey{}).(StatelessHTTPSession)
	return session, ok
}

// WrapStatelessHTTPHandler wraps a stateless MCP HTTP handler to repair
// legacy-client session attribution. It is the Go equivalent of the
// TypeScript adapter's wrapMcpHandler.
//
// go-sdk >= v1.7.0's StreamableHTTPHandler{Stateless: true} serves
// pre-2026-07-28 clients but mints no Mcp-Session-Id (ServerOptions.
// GetSessionID is never consulted), so a legacy client gets no session signal:
// its tool calls land in a server-side fallback bucket with no client
// identity, split from the initialize's session_init. On a legacy initialize
// POST this middleware mints the identity-bearing session id
// (mcp_<name>_v_<version>_<uuid>, honoring X-Armature-Session-Seed as the
// uuid seed — the ResolveStatelessHTTPSession scheme), attaches it as the
// response's Mcp-Session-Id, and records the session_init. Conforming legacy
// clients echo the header on every later request, where it is mirrored back
// and the adapters' session-id ladder picks it up. 2026-07-28 requests carry
// no initialize and no session header and pass through untouched.
//
// The resolved identity is also handed to the wrapped handler itself — via
// the request context (StatelessHTTPSessionFromRequest) and the
// StatelessSessionHeader request header — so the analytics middleware inside
// the per-request server attributes the initialize POST's own events to the
// same session instead of an empty one.
//
// The receiver may be nil: minting, echo, and propagation still run, only the
// middleware's own session_init recording is skipped (an instrumented inner
// server still records it from the propagated identity). Wrap only stateless
// handlers; pre-v1.7.0 stateful transports mint their own session ids and
// need no repair.
func (r *Recorder) WrapStatelessHTTPHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The repair header is middleware-owned: never trust an inbound value.
		req.Header.Del(StatelessSessionHeader)
		session, body := resolveWrappedStatelessRequest(req)
		if session.SessionID == "" {
			// Modern-era traffic or a lost echo: leave the request untouched
			// and let ingest bucket sessionless events server-side.
			next.ServeHTTP(w, req)
			return
		}
		req = req.WithContext(context.WithValue(req.Context(), statelessSessionContextKey{}, session))
		req.Header.Set(StatelessSessionHeader, session.SessionID)
		startedAt := time.Now()
		writer := &sessionHeaderResponseWriter{ResponseWriter: w, sessionID: session.SessionID}
		next.ServeHTTP(writer, req)
		// A handler that returned without writing commits an implicit 200 —
		// the headers are still open, so the attach can happen here.
		if !writer.wroteHeader && writer.Header().Get(sessionIDHeader) == "" {
			writer.Header().Set(sessionIDHeader, session.SessionID)
		}
		if r != nil && session.IsInitialize && writer.succeeded() {
			r.RecordSessionInit(req.Context(), SessionInitInput{
				SessionID:     session.SessionID,
				ActorSeed:     r.ResolveActorSeed(req.Context(), req.Header),
				StartedAt:     startedAt,
				ClientInfo:    sessionInitClientInfo(body),
				WorkflowRunID: WorkflowRunIDFromHeaders(req.Header),
			})
		}
	})
}

const sessionIDHeader = "Mcp-Session-Id"

// resolveWrappedStatelessRequest reads and restores the request body and
// resolves the stateless session for it. Non-POST requests, unreadable
// bodies, and non-JSON bodies all resolve to the zero session, which the
// middleware treats as pass-through.
func resolveWrappedStatelessRequest(req *http.Request) (StatelessHTTPSession, any) {
	if req.Method != http.MethodPost || req.Body == nil {
		return StatelessHTTPSession{}, nil
	}
	raw, err := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return StatelessHTTPSession{}, nil
	}
	body := decodeStatelessBody(raw)
	return ResolveStatelessHTTPSession(StatelessHTTPInput{Body: body, Headers: req.Header}), body
}

// sessionInitClientInfo captures the full initialize-time client identity for
// the middleware's session_init — the same fields the official adapter's
// initialize hook records — rather than only the name/version recoverable
// from the minted session id.
func sessionInitClientInfo(body any) *ClientInfo {
	initialize := findInitializeMessage(body)
	if initialize == nil {
		return nil
	}
	info := clientInfoFromInitialize(initialize)
	params, _ := initialize["params"].(map[string]any)
	protocolVersion, _ := params["protocolVersion"].(string)
	capabilities, _ := params["capabilities"].(map[string]any)
	if info == nil {
		if protocolVersion == "" && capabilities == nil {
			return nil
		}
		info = &ClientInfo{}
	}
	info.ProtocolVersion = protocolVersion
	info.Capabilities = capabilities
	return info
}

// sessionHeaderResponseWriter attaches the session id header the moment a
// 2xx status is committed, before the response is flushed to the client.
type sessionHeaderResponseWriter struct {
	http.ResponseWriter
	sessionID   string
	wroteHeader bool
	status      int
}

func (w *sessionHeaderResponseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = status
		if status >= 200 && status < 300 && w.Header().Get(sessionIDHeader) == "" {
			w.Header().Set(sessionIDHeader, w.sessionID)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *sessionHeaderResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// Flush keeps SSE streaming working through the wrapper. Unwrap serves
// http.NewResponseController; the explicit Flush serves direct assertions.
func (w *sessionHeaderResponseWriter) Flush() {
	flusher, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	flusher.Flush()
}

func (w *sessionHeaderResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *sessionHeaderResponseWriter) succeeded() bool {
	return !w.wroteHeader || (w.status >= 200 && w.status < 300)
}
