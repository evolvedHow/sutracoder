// Package prompt provides the embedded default system prompt, overridable at
// runtime. Tuning this prompt is the primary agent-quality lever.
package prompt

import _ "embed"

// DefaultSystem is the built-in system prompt baked in via embed.FS. It can be
// overridden with --system-prompt or by loading a file at runtime.
//
//go:embed system.md
var DefaultSystem string
