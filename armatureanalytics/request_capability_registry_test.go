package armatureanalytics

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/mark3labs/mcp-go/server"
)

// A server marked as listing request_capability but never shut down through
// the SDK must not be retained by the registry once it becomes unreachable.
func TestRequestCapabilityRegistryDropsUnreachableServers(t *testing.T) {
	s := server.NewMCPServer("standalone", "0.0.1")
	MarkRequestCapabilityRegistered(s)
	if !RequestCapabilityRegistered(s) {
		t.Fatal("marked server not reported as listing request_capability")
	}
	key := weak.Make(s)
	s = nil
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if _, ok := requestCapabilityServers.Load(key); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("registry still holds an unreachable server")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
