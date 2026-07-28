package official

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBaggageMemberParsing(t *testing.T) {
	tests := []struct {
		name    string
		baggage string
		want    string
	}{
		{name: "single member", baggage: "gen_ai.conversation.id=conv-1", want: "conv-1"},
		{name: "among members", baggage: "app=demo,gen_ai.conversation.id=conv-2,env=prod", want: "conv-2"},
		{name: "with properties", baggage: "gen_ai.conversation.id=conv-3;metadata=opaque", want: "conv-3"},
		{name: "percent decoded", baggage: "gen_ai.conversation.id=conv%2Fwith%20space", want: "conv/with space"},
		{name: "spaces around members", baggage: " app=demo , gen_ai.conversation.id = conv-4 ", want: "conv-4"},
		{name: "missing member", baggage: "app=demo,env=prod", want: ""},
		{name: "prefix does not match", baggage: "gen_ai.conversation.idx=nope", want: ""},
		{name: "no value", baggage: "gen_ai.conversation.id", want: ""},
		{name: "empty", baggage: "", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := baggageMember(test.baggage, baggageConversationKey); got != test.want {
				t.Fatalf("baggageMember(%q) = %q, want %q", test.baggage, got, test.want)
			}
		})
	}
}

func TestConversationIDFromMeta(t *testing.T) {
	if got := conversationIDFromMeta(nil); got != "" {
		t.Fatalf("nil meta = %q", got)
	}
	if got := conversationIDFromMeta(map[string]any{"baggage": 42}); got != "" {
		t.Fatalf("non-string baggage = %q", got)
	}
	meta := map[string]any{"baggage": "gen_ai.conversation.id=conv-9"}
	if got := conversationIDFromMeta(meta); got != "conv-9" {
		t.Fatalf("conversation id = %q", got)
	}
}

func TestIsModernRequest(t *testing.T) {
	if isModernRequest(nil) {
		t.Fatal("nil meta reported modern")
	}
	if isModernRequest(map[string]any{mcp.MetaKeyProtocolVersion: "2025-11-25"}) {
		t.Fatal("legacy version reported modern")
	}
	if !isModernRequest(map[string]any{mcp.MetaKeyProtocolVersion: "2026-07-28"}) {
		t.Fatal("2026-07-28 not reported modern")
	}
	if !isModernRequest(map[string]any{mcp.MetaKeyProtocolVersion: "2027-01-01"}) {
		t.Fatal("future version not reported modern")
	}
}

func TestPerRequestClientInfoFromRawMeta(t *testing.T) {
	// Wire-shaped values (generic maps), as produced by JSON transit.
	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{
			Meta: mcp.Meta{
				mcp.MetaKeyProtocolVersion:    "2026-07-28",
				mcp.MetaKeyClientInfo:         map[string]any{"name": "meta-client", "version": "7.7"},
				mcp.MetaKeyClientCapabilities: map[string]any{"roots": map[string]any{}},
			},
		},
	}
	info := perRequestClientInfo(req)
	if info == nil || info.Name != "meta-client" || info.Version != "7.7" || info.ProtocolVersion != "2026-07-28" {
		t.Fatalf("client info = %#v", info)
	}
	if _, ok := info.Capabilities["roots"]; !ok {
		t.Fatalf("capabilities lost: %#v", info.Capabilities)
	}
}

func TestPerRequestClientInfoAbsentIdentity(t *testing.T) {
	if info := perRequestClientInfo(nil); info != nil {
		t.Fatalf("nil request info = %#v", info)
	}
	empty := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{}}
	if info := perRequestClientInfo(empty); info != nil {
		t.Fatalf("identity-less request info = %#v", info)
	}
	var typedNil *mcp.CallToolParamsRaw
	nilParams := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: typedNil}
	if meta := requestMeta(nilParams); meta != nil {
		t.Fatalf("typed-nil params meta = %#v", meta)
	}
}
