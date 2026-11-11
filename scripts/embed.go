// Package scripts holds the agent install and uninstall scripts the hub serves.
// The hub fills in __HUB_URL__ and __AGENT_SHA256__ when it serves them.
package scripts

import "embed"

// FS contains every file in this directory.
//
//go:embed *
var FS embed.FS
