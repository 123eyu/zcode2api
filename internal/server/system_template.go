package server

import _ "embed"

// Default insertion between the fixed custom-template opening and Harness.
// Embed it so packaged deployments do not need the source tree at runtime.
//
//go:embed system-prompt.md
var defaultSubagentInsert string
