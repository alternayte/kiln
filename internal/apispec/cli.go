package apispec

import (
	"fmt"
	"strings"
)

// The exit codes of the kiln client. The docs and the skill read this table,
// so an agent that branches on a code reads what the binary does.
const (
	ExitOK        = 0
	ExitFailure   = 1
	ExitUsage     = 2
	ExitNotFound  = 3
	ExitConflict  = 4
	ExitExhausted = 5
	ExitInvalid   = 6
	ExitTimeout   = 14
	// ExitExecFailed is what kiln exec returns when Kiln failed, because
	// every other code there belongs to the command.
	ExitExecFailed = 125
	// ExitExecTimedOut is what kiln exec returns when the command reached
	// its timeout_seconds.
	ExitExecTimedOut = 124
)

// ExitCodes describes each code, in order.
var ExitCodes = []struct {
	Code    int
	Meaning string
}{
	{ExitOK, "Success."},
	{ExitFailure, "An API or network failure, or a template that did not build."},
	{ExitUsage, "A usage or configuration error, such as a missing KILN_URL."},
	{ExitNotFound, "The API answered `not_found`."},
	{ExitConflict, "The API answered `conflict`."},
	{ExitExhausted, "The API answered `exhausted`: a cap of this tenant is reached."},
	{ExitInvalid, "The API answered `invalid`."},
	{ExitTimeout, "`--timeout` ended the wait. The build continues."},
	{ExitExecTimedOut, "`kiln exec`: the command reached `timeout_seconds`."},
	{ExitExecFailed, "`kiln exec`: Kiln itself failed. Any other code from `kiln exec` is the exit code of the command."},
}

// ExitCodeTable is the table as Markdown.
func ExitCodeTable() string {
	var b strings.Builder
	b.WriteString("| Code | Meaning |\n|---|---|\n")
	for _, c := range ExitCodes {
		fmt.Fprintf(&b, "| %d | %s |\n", c.Code, c.Meaning)
	}
	return b.String()
}
