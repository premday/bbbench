// Package rungroup holds the pure data and logic of run groups: the label
// stored with each measurement, the run plan, the group and recovery files
// and the failure policy.
package rungroup

import "bbbench/internal/settings"

// Label records the setting a drive had during a run.
type Label struct {
	Setting string            `json:"setting"`
	Value   string            `json:"value"`
	Before  *settings.Reading `json:"before"`
	After   *settings.Reading `json:"after"`
	Valid   bool              `json:"valid"`
	Note    string            `json:"note"`
}
