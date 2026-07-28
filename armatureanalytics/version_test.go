package armatureanalytics

import (
	"runtime/debug"
	"testing"
)

func TestDetectSDKVersionFromDependency(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "example.com/customer/server", Version: "(devel)"},
		Deps: []*debug.Module{
			{Path: "example.com/other", Version: "v9.9.9"},
			{Path: sdkModulePath, Version: "v0.3.1"},
		},
	}
	if got := detectSDKVersion(info, true); got != "v0.3.1" {
		t.Errorf("detectSDKVersion = %q, want v0.3.1", got)
	}
}

func TestDetectSDKVersionPrefersReplaceDirective(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "example.com/customer/server"},
		Deps: []*debug.Module{
			{
				Path:    sdkModulePath,
				Version: "v0.3.1",
				Replace: &debug.Module{Path: "../mcp-analytics-go", Version: "v0.4.0-rc.1"},
			},
		},
	}
	if got := detectSDKVersion(info, true); got != "v0.4.0-rc.1" {
		t.Errorf("detectSDKVersion = %q, want v0.4.0-rc.1", got)
	}
}

func TestDetectSDKVersionForModuleOwnBuilds(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: sdkModulePath, Version: "(devel)"},
	}
	if got := detectSDKVersion(info, true); got != "(devel)" {
		t.Errorf("detectSDKVersion = %q, want (devel)", got)
	}
}

func TestDetectSDKVersionWithoutBuildInfo(t *testing.T) {
	if got := detectSDKVersion(nil, false); got != "unknown" {
		t.Errorf("detectSDKVersion = %q, want unknown", got)
	}
}

func TestSDKVersionIsNeverEmptyOrHardcoded(t *testing.T) {
	version := SDKVersion()
	if version == "" {
		t.Fatal("SDKVersion() must never be empty")
	}
	// The old User-Agent shipped a hardcoded "0.1" that never tracked releases;
	// the reported version must come from build info instead.
	if version == "0.1" {
		t.Fatalf("SDKVersion() = %q — hardcoded placeholder resurfaced", version)
	}
}
