// Package web embeds the dashboard page served as a CPA plugin resource.
package web

import (
	"bytes"
	_ "embed"
)

//go:embed index.html
var page []byte

// limits.js holds the Limits math as a plain script so bun can test it. The
// page stays a single static file, so it is inlined at this placeholder.
//
//go:embed limits.js
var limitsScript []byte

const limitsPlaceholder = "/* limits.js */"

var index = bytes.Replace(page, []byte(limitsPlaceholder), limitsScript, 1)

// Index returns the dashboard page. It is static: data comes from the
// authenticated management routes, never from the resource route.
func Index() []byte { return index }
