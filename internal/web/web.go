// Package web embeds the dashboard page served as a CPA plugin resource.
package web

import _ "embed"

//go:embed index.html
var index []byte

// Index returns the dashboard page. It is static: data comes from the
// authenticated management routes, never from the resource route.
func Index() []byte { return index }
