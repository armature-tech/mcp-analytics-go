package armatureanalytics

import (
	"runtime/debug"
	"strings"
	"sync"
)

const sdkLanguage = "go"

const sdkModulePath = "github.com/armature-tech/mcp-analytics-go"

var (
	sdkVersionOnce  sync.Once
	sdkVersionValue string
)

// SDKVersion reports this module's version as recorded in the consuming
// binary's Go build info — the real go.mod dependency version, never a
// hardcoded constant. Binaries built from a checkout of the module itself
// (tests, canaries, replace directives) report Go's "(devel)" placeholder,
// which is honest: they are not running a released version.
func SDKVersion() string {
	sdkVersionOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		sdkVersionValue = detectSDKVersion(info, ok)
	})
	return sdkVersionValue
}

func detectSDKVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path != sdkModulePath {
			continue
		}
		version := dep.Version
		if dep.Replace != nil {
			version = dep.Replace.Version
		}
		if version != "" {
			return version
		}
		return "(devel)"
	}
	if info.Main.Path == sdkModulePath || strings.HasPrefix(info.Main.Path, sdkModulePath+"/") {
		if info.Main.Version != "" {
			return info.Main.Version
		}
		return "(devel)"
	}
	return "unknown"
}

// batchSDKIdentity is stamped onto every batch at the delivery boundary so
// Armature ingest can attribute traffic to a language and version.
func batchSDKIdentity() *BatchSDK {
	return &BatchSDK{Language: sdkLanguage, Version: SDKVersion()}
}

func sdkUserAgent() string {
	return "armature-mcp-analytics-go/" + SDKVersion()
}
