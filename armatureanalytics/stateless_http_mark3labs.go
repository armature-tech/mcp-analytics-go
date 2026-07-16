package armatureanalytics

import (
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/server"
)

type statelessSessionIDManager struct {
	sessionID string
}

// Mark3labsSessionIDManager returns a no-store session manager for the current
// request. Pass it to server.WithSessionIdManager on a per-request server.
func (s StatelessHTTPSession) Mark3labsSessionIDManager() server.SessionIdManager {
	return &statelessSessionIDManager{sessionID: s.SessionID}
}

func (m *statelessSessionIDManager) Generate() string { return m.sessionID }

func (m *statelessSessionIDManager) Validate(sessionID string) (bool, error) {
	if strings.TrimSpace(sessionID) == "" || sessionID != m.sessionID {
		return false, fmt.Errorf("invalid session id")
	}
	// The boolean is isTerminated, not isValid. false means this matching
	// stateless session is active; returning true makes mark3labs reject it as
	// terminated with HTTP 404.
	return false, nil
}

func (*statelessSessionIDManager) Terminate(string) (bool, error) { return false, nil }
