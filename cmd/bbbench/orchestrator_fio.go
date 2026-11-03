package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// FioJob represents a single job section in a fio file.
type FioJob struct {
	Name        string
	Section     string // Full section content including [name]
	Index       int
	IsStonewall bool
	WaitFor     string
}

// FioWorkload represents a parsed fio file with all its jobs.
type FioWorkload struct {
	GlobalSection string
	Jobs          []FioJob
	Phases        [][]FioJob // Jobs grouped by phase (separated by stonewall)
}

// parseFioFile reads and parses a fio file into a structured workload.
func parseFioFile(path string) (*FioWorkload, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open fio file: %w", err)
	}
	defer file.Close()

	workload := &FioWorkload{}
	scanner := bufio.NewScanner(file)

	var currentSection strings.Builder
	var currentJobName string
	var inGlobal bool
	jobIndex := 0

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Check for section header
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			// Save previous section
			if currentSection.Len() > 0 {
				if inGlobal {
					workload.GlobalSection = currentSection.String()
				} else if currentJobName != "" {
					job := parseFioJobSection(currentJobName, currentSection.String(), jobIndex)
					workload.Jobs = append(workload.Jobs, job)
					jobIndex++
				}
				currentSection.Reset()
			}

			// Start new section
			sectionName := strings.Trim(trimmed, "[]")
			currentJobName = sectionName
			inGlobal = (sectionName == "global")
			currentSection.WriteString(line + "\n")
		} else {
			currentSection.WriteString(line + "\n")
		}
	}

	// Save last section
	if currentSection.Len() > 0 {
		if inGlobal {
			workload.GlobalSection = currentSection.String()
		} else if currentJobName != "" {
			job := parseFioJobSection(currentJobName, currentSection.String(), jobIndex)
			workload.Jobs = append(workload.Jobs, job)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan fio file: %w", err)
	}

	// Group jobs into phases
	workload.Phases = groupJobsIntoPhases(workload.Jobs)

	return workload, nil
}

// parseFioJobSection parses a single job section to extract metadata.
func parseFioJobSection(name, section string, index int) FioJob {
	job := FioJob{
		Name:    name,
		Section: section,
		Index:   index,
	}

	lines := strings.Split(section, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if trimmed == "stonewall" || strings.HasPrefix(trimmed, "stonewall=") {
			job.IsStonewall = true
		}

		if strings.HasPrefix(trimmed, "wait_for=") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 {
				job.WaitFor = strings.TrimSpace(parts[1])
			}
		}
	}

	return job
}

// groupJobsIntoPhases groups jobs by phase boundaries (stonewall directives).
// Each phase contains jobs that should run together before the next stonewall.
// A job with stonewall starts a new phase (unless it's the first job).
func groupJobsIntoPhases(jobs []FioJob) [][]FioJob {
	if len(jobs) == 0 {
		return nil
	}

	var phases [][]FioJob
	var currentPhase []FioJob

	for _, job := range jobs {
		// If this job has stonewall and we already have jobs in current phase,
		// save current phase and start a new one
		if job.IsStonewall && len(currentPhase) > 0 {
			phases = append(phases, currentPhase)
			currentPhase = []FioJob{job}
		} else {
			// Add to current phase
			currentPhase = append(currentPhase, job)
		}
	}

	// Add any remaining jobs as the last phase
	if len(currentPhase) > 0 {
		phases = append(phases, currentPhase)
	}

	return phases
}

// buildPhaseFioFile generates a temporary fio file for a specific phase.
// It removes wait_for directives that reference jobs from other phases,
// since phases run sequentially in separate fio invocations.
func buildPhaseFioFile(workload *FioWorkload, phase []FioJob) string {
	var sb strings.Builder

	// Add global section
	sb.WriteString(workload.GlobalSection)
	sb.WriteString("\n")

	// Build map of job names in this phase
	jobsInPhase := make(map[string]bool)
	for _, job := range phase {
		jobsInPhase[job.Name] = true
	}

	// Add jobs for this phase, filtering wait_for directives
	for _, job := range phase {
		sb.WriteString(filterJobSection(job, jobsInPhase))
		sb.WriteString("\n")
	}

	return sb.String()
}

// filterJobSection removes wait_for directives that reference jobs not in this phase.
func filterJobSection(job FioJob, jobsInPhase map[string]bool) string {
	lines := strings.Split(job.Section, "\n")
	var filtered []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Strip per-job log file directives; the orchestrator manages log file
		// paths via command-line flags so these per-job overrides must be removed.
		if strings.HasPrefix(trimmed, "write_bw_log=") ||
			strings.HasPrefix(trimmed, "write_iops_log=") ||
			strings.HasPrefix(trimmed, "write_lat_log=") {
			continue
		}

		// Skip wait_for directives that reference jobs not in this phase
		if strings.HasPrefix(trimmed, "wait_for=") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 {
				waitForJob := strings.TrimSpace(parts[1])
				if !jobsInPhase[waitForJob] {
					// Skip this line - it references a job from another phase
					continue
				}
			}
		}

		filtered = append(filtered, line)
	}

	return strings.Join(filtered, "\n")
}

var fsyncOptRe = regexp.MustCompile(`(?m)^\s*fsync\s*=\s*[1-9]`)

// phaseHasFsync reports whether any job of the phase issues fsyncs
// (fio's fsync=N with N > 0), i.e. whether device Flushes are expected.
func phaseHasFsync(phase []FioJob) bool {
	for _, j := range phase {
		if fsyncOptRe.MatchString(j.Section) {
			return true
		}
	}
	return false
}
