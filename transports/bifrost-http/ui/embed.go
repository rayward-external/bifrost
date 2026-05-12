package uiassets

import "embed"

// Content embeds the built admin UI for binaries that serve the dashboard.
//
//go:embed all:*
var Content embed.FS
