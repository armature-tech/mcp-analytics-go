package armatureanalytics

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type telemetryContractVectors struct {
	Sanitization    []telemetryContractVector `json:"sanitization"`
	SecretRedaction []telemetryContractVector `json:"secret_redaction"`
}

type telemetryContractVector struct {
	Name       string   `json:"name"`
	Value      any      `json:"value"`
	ValueParts []string `json:"value_parts"`
	Expect     any      `json:"expect"`
}

func (v telemetryContractVector) resolvedValue() any {
	if len(v.ValueParts) > 0 {
		return strings.Join(v.ValueParts, "")
	}
	return v.Value
}

func loadTelemetryContractVectors(t *testing.T) telemetryContractVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/telemetry_contract_vectors.json")
	if err != nil {
		t.Fatalf("read telemetry vectors: %v", err)
	}
	var vectors telemetryContractVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("decode telemetry vectors: %v", err)
	}
	return vectors
}

func TestSanitizeValueContractVectors(t *testing.T) {
	for _, vector := range loadTelemetryContractVectors(t).Sanitization {
		t.Run(vector.Name, func(t *testing.T) {
			if got := SanitizeValue(vector.Value); !reflect.DeepEqual(got, vector.Expect) {
				t.Fatalf("SanitizeValue() = %#v, want %#v", got, vector.Expect)
			}
		})
	}
}

func TestSecretRedactionContractVectors(t *testing.T) {
	for _, vector := range loadTelemetryContractVectors(t).SecretRedaction {
		t.Run(vector.Name, func(t *testing.T) {
			got := RedactSecretsInValue(SanitizeValue(vector.resolvedValue()))
			if !reflect.DeepEqual(got, vector.Expect) {
				t.Fatalf("composed sanitization/redaction = %#v, want %#v", got, vector.Expect)
			}
		})
	}
}

func TestSanitizeValueBudgetPreservesSerializedHorizon(t *testing.T) {
	large := map[string]any{"payload": strings.Repeat("not base64 prose! ", 200_000)}
	boundedJSON, err := json.Marshal(SanitizeValue(large))
	if err != nil {
		t.Fatalf("marshal bounded value: %v", err)
	}
	unboundedJSON, err := json.Marshal(large)
	if err != nil {
		t.Fatalf("marshal original value: %v", err)
	}
	if len(boundedJSON) <= MaxSourceBytes || len(unboundedJSON) <= MaxSourceBytes {
		t.Fatal("test payload did not cross serialization horizon")
	}
	if string(boundedJSON[:MaxSourceBytes]) != string(unboundedJSON[:MaxSourceBytes]) {
		t.Fatal("bounded traversal changed content inside the 32 KB serialization horizon")
	}

	start := time.Unix(0, 0)
	boundedEvent := BuildToolCallEvent(ToolCallInput{ToolName: "large", Args: large, StartedAt: start, FinishedAt: start})
	if !boundedEvent.ScriptSourceTruncated || len(*boundedEvent.ScriptSource) != MaxSourceBytes {
		t.Fatalf("bounded source truncation = (%d, %v)", len(*boundedEvent.ScriptSource), boundedEvent.ScriptSourceTruncated)
	}
	if got := len(boundedEvent.Metadata["input_preview"].(string)); got != MaxPreviewBytes {
		t.Fatalf("bounded input preview length = %d, want %d", got, MaxPreviewBytes)
	}
}

func TestSanitizeValueCycleSafe(t *testing.T) {
	value := map[string]any{}
	value["self"] = value
	got := SanitizeValue(value).(map[string]any)
	if got["self"] != "[circular]" {
		t.Fatalf("cycle = %#v", got["self"])
	}
}
