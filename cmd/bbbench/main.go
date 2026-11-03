package main

import (
	"fmt"
	"os"

	"bbbench/internal/buildinfo"
)

func main() {
	initLoggerFromEnv()

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "generate":
		exitWithError(runGenerate(os.Args[2:]))
	case "validate-templates":
		exitWithError(runValidateTemplates(os.Args[2:]))
	case "doctor":
		exitWithError(runDoctor(os.Args[2:]))
	case "orchestrator":
		exitWithError(runOrchestrator(os.Args[2:]))
	case "serve":
		exitWithError(runServe(os.Args[2:]))
	case "completion":
		exitWithError(runCompletion(os.Args[2:]))
	case "version", "-v", "--version":
		fmt.Println("bbbench", buildinfo.String())
	case "help", "-h", "--help":
		printHelp()
	default:
		logger.Error("unknown command", "cmd", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}
