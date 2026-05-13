package uiassets

import "embed"

// Content embeds the built admin UI for binaries that serve the dashboard.
//
//go:embed ui
//go:embed ui/**
var Content embed.FS
