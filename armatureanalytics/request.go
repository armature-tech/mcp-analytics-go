package armatureanalytics

import (
	"net/http"
	"regexp"
	"strings"
)

const WorkflowRunIDHeader = "X-Armature-Workflow-Run-Id"

var workflowRunIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// WorkflowRunIDFromHeaders returns a valid Armature workflow-run UUID or an
// empty string. Stamped events are excluded from user Session Analytics.
func WorkflowRunIDFromHeaders(headers http.Header) string {
	value := strings.TrimSpace(headers.Get(WorkflowRunIDHeader))
	if !workflowRunIDRE.MatchString(value) {
		return ""
	}
	return value
}
