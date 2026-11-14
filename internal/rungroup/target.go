package rungroup

import "bbbench/internal/settings"

// Member is a disk of a composite target, as stored in group files, run
// results and the status page.
type Member struct {
	Name   string        `json:"name"`
	Serial string        `json:"serial"`
	Model  string        `json:"model"`
	Role   settings.Role `json:"role"`
}

// MembersOf describes the disks of a layout as members.
func MembersOf(disks []settings.Disk) []Member {
	out := make([]Member, 0, len(disks))
	for _, d := range disks {
		out = append(out, Member{Name: d.Name, Serial: d.Serial, Model: d.Model, Role: d.Role})
	}
	return out
}

// GroupVariant is one layout of a layout group: its value and disks.
type GroupVariant struct {
	Value string   `json:"value"`
	Disks []Member `json:"disks"`
}
