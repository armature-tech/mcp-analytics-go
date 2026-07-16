package armatureanalytics

import "sync"

var processSession struct {
	sync.Once
	id string
}

// ProcessScopedSessionID returns one stable ID for the lifetime of a stdio
// server process. Since MCP stdio serves one connection per process, this is
// the conversation boundary and prevents separate CLI processes from merging.
func ProcessScopedSessionID() string {
	processSession.Do(func() {
		processSession.id = "stdio-" + randomUUID()
	})
	return processSession.id
}
