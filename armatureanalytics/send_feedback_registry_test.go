package armatureanalytics

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/mark3labs/mcp-go/server"
)

// A server marked as listing send_feedback but never shut down through
// the SDK must not be retained by the registry once it becomes unreachable.
func TestSendFeedbackRegistryDropsUnreachableServers(t *testing.T) {
	s := server.NewMCPServer("standalone", "0.0.1")
	MarkSendFeedbackRegistered(s)
	if !SendFeedbackRegistered(s) {
		t.Fatal("marked server not reported as listing send_feedback")
	}
	key := weak.Make(s)
	s = nil
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if _, ok := sendFeedbackServers.Load(key); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("registry still holds an unreachable server")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
