// Package providers holds the provider files grelot ships, one YAML file per
// known host, with each host's install files in a folder of the same name.
package providers

import "embed"

// FS holds the shipped provider files at its root. It is the first layer the
// client loads, so a user's file of the same name replaces a shipped one.
//
//go:embed *.yaml
var FS embed.FS
