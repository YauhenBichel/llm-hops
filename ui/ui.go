// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package ui embeds the page: one HTML file, one script, one stylesheet, no build step.
package ui

import "embed"

// Files holds index.html, app.js and app.css.
//
//go:embed index.html app.js app.css
var Files embed.FS
