package main

import (
	"fmt"
	"log"
	"os"
	"sort"

	"bbbench/internal/buildinfo"
	"bbbench/internal/config"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("check-config", buildinfo.String())
		return
	}

	configPath := "dist/default.yml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatal("Failed to load config: ", err)
	}

	fmt.Printf("Config loaded successfully from: %s\n\n", configPath)
	fmt.Printf("WorkloadsDB entries: %d\n", len(cfg.BBBench.WorkloadsDB))
	fmt.Printf("Workload types: %d\n\n", len(cfg.BBBench.Workloads))

	// Print workload types in deterministic order.
	types := make([]string, 0, len(cfg.BBBench.Workloads))
	for typ := range cfg.BBBench.Workloads {
		types = append(types, typ)
	}
	sort.Strings(types)

	for _, typ := range types {
		workloads := cfg.BBBench.Workloads[typ]
		fmt.Printf("%s: %d workloads\n", typ, len(workloads))
		for i, w := range workloads {
			fmt.Printf("  %d. %s", i+1, w)
			if wdef, ok := cfg.BBBench.WorkloadsDB[w]; ok {
				fmt.Printf(" (%d jobs)\n", countJobs(wdef))
			} else {
				fmt.Printf(" ⚠️  NOT FOUND IN DATABASE\n")
			}
		}
		for _, warn := range warnings(typ, workloads, cfg.BBBench.WorkloadsDB) {
			fmt.Printf("  ⚠️  WARNING: %s\n", warn)
		}
		fmt.Println()
	}
}

// countJobs returns the number of process types in a workload definition that
// have a positive integer count (i.e. active job types).
func countJobs(wdef config.WorkloadDef) int {
	n := 0
	for _, count := range wdef.Process {
		if c, ok := count.(int); ok && c > 0 {
			n++
		}
	}
	return n
}

// warnings returns any configuration problems for one workload type.
func warnings(typ string, workloads []string, db map[string]config.WorkloadDef) []string {
	var ws []string
	if len(workloads) == 0 {
		ws = append(ws, fmt.Sprintf("no workloads defined for %s", typ))
		return ws
	}
	for _, w := range workloads {
		if _, ok := db[w]; !ok {
			ws = append(ws, fmt.Sprintf("workload %q not found in workloads database", w))
		}
	}
	return ws
}
