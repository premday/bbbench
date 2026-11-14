package main

import (
	"fmt"

	"bbbench/internal/buildinfo"
)

func printHelp() {
	fmt.Printf(`bbbench %s - fio job generator

Usage:
  bbbench <command> [flags]

Commands:
  generate             generate fio job files
  orchestrator         run benchmarks on multiple drives (single run, or a
                       setting test: the plan once per value of a drive
                       setting, restored afterwards; or a layout test:
                       the plan once per storage layout, which ERASES
                       its disks and is torn down after each run)
  serve                start web server GUI (-addr, default 0.0.0.0:12345)
  validate-templates   validate template syntax
  doctor               check host readiness
  completion           generate shell completions
  version              print the version
  help                 show this help

Environment:
  BBBENCH_LOG_LEVEL    debug|info|warn|error
  BBBENCH_DIST_DIR     overrides dist search path
`, buildinfo.Version)
}
