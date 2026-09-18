package main

// miro-code entry point. All application logic lives in internal/app
// (single-package migration, 2026-09-18) so the repository root stays clean.
// ldflags inject into the app package:
//   -X github.com/kiyor/soul-cli/internal/app.defaultAppName=<name>

import "github.com/kiyor/soul-cli/internal/app"

func main() { app.Main() }
