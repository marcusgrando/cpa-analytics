// Package web embeds the dashboard page served as a CPA plugin resource.
package web

import (
	"bytes"
	_ "embed"
)

//go:embed index.html
var page []byte

// The Limits math, the period comparison, the provider colors, the tooltip
// placement, the sparklines, the line end labels and the latency scatter
// scales live in plain scripts so bun can test them. The page stays a single
// static file, so each is inlined at its placeholder.
var (
	//go:embed limits.js
	limitsScript []byte
	//go:embed compare.js
	compareScript []byte
	//go:embed colors.js
	colorsScript []byte
	//go:embed tooltip.js
	tooltipScript []byte
	//go:embed sparkline.js
	sparklineScript []byte
	//go:embed endlabels.js
	endLabelsScript []byte
	//go:embed scatter.js
	scatterScript []byte
	//go:embed drift.js
	driftScript []byte
	//go:embed plans.js
	plansScript []byte
)

var index = inline(page, map[string][]byte{
	"/* limits.js */":    limitsScript,
	"/* compare.js */":   compareScript,
	"/* colors.js */":    colorsScript,
	"/* tooltip.js */":   tooltipScript,
	"/* sparkline.js */": sparklineScript,
	"/* endlabels.js */": endLabelsScript,
	"/* scatter.js */":   scatterScript,
	"/* drift.js */":     driftScript,
	"/* plans.js */":     plansScript,
})

func inline(page []byte, scripts map[string][]byte) []byte {
	for placeholder, script := range scripts {
		page = bytes.Replace(page, []byte(placeholder), script, 1)
	}
	return page
}

// Index returns the dashboard page. It is static: data comes from the
// authenticated management routes, never from the resource route.
func Index() []byte { return index }
