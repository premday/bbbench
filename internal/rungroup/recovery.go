package rungroup

import (
	"encoding/json"
	"errors"
	"maps"
	"os"

	"bbbench/internal/settings"
)

// Recovery records what a run group changed so a crash can be undone: the
// original values of drives, and the layouts built (before each build).
type Recovery struct {
	Setting string          `json:"setting"`
	Drives  []RecoveryDrive `json:"drives"`
	Arrays  []RecoveryArray `json:"arrays,omitempty"`
}

// RecoveryArray is a layout bbbench built: what tears it down. Backend and
// Extra are the backend's, stored as they are.
type RecoveryArray struct {
	Backend string            `json:"backend"`
	Name    string            `json:"name"`
	UUID    string            `json:"uuid,omitempty"`
	Members []RecoveryMember  `json:"members"`
	Extra   map[string]string `json:"extra,omitempty"`
}

// RecoveryMember is a disk of a recorded layout, found again by serial and
// model (Name is the kernel name when it was recorded).
type RecoveryMember struct {
	Serial string        `json:"serial"`
	Model  string        `json:"model"`
	Name   string        `json:"name"`
	Role   settings.Role `json:"role"`
}

type RecoveryDrive struct {
	Serial   string           `json:"serial"`
	Model    string           `json:"model"`
	Name     string           `json:"name"`
	Original settings.Reading `json:"original"`
}

func (r *Recovery) Save(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// LoadRecovery returns nil, nil when the file does not exist.
func LoadRecovery(path string) (*Recovery, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &Recovery{}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Recovery) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Match pairs entries with devices by serial and model, never by name,
// since device names change across reboots. found maps the current device
// name to its entry; missing lists entries with no matching device.
func (r *Recovery) Match(devices []DriveRef) (found map[string]RecoveryDrive, missing []RecoveryDrive) {
	found = map[string]RecoveryDrive{}
	for _, e := range r.Drives {
		matched := false
		for _, d := range devices {
			if d.Serial == e.Serial && d.Model == e.Model {
				found[d.Name] = e
				matched = true
				break
			}
		}
		if !matched {
			missing = append(missing, e)
		}
	}
	return found, missing
}

// RecoveryOf is the recovery file of setting holding undos: each drive undo
// becomes a Drives entry, each layout undo an Arrays entry.
func RecoveryOf(setting string, undos []settings.Undo) *Recovery {
	r := &Recovery{Setting: setting}
	for _, u := range undos {
		if a := u.Array; a != nil {
			ra := RecoveryArray{Backend: a.Backend, Name: a.Name, UUID: a.UUID, Members: make([]RecoveryMember, 0, len(a.Members))}
			if a.Extra != nil {
				ra.Extra = maps.Clone(a.Extra)
			}
			for _, d := range a.Members {
				ra.Members = append(ra.Members, RecoveryMember{Serial: d.Serial, Model: d.Model, Name: d.Name, Role: d.Role})
			}
			r.Arrays = append(r.Arrays, ra)
			continue
		}
		if u.Drive == nil || u.Original == nil {
			continue
		}
		r.Drives = append(r.Drives, RecoveryDrive{Serial: u.Drive.Serial, Model: u.Drive.Model, Name: u.Drive.Name, Original: *u.Original})
	}
	return r
}

// Undos are the entries as undo records, matched to the current targets by
// serial and model (Match), in the order of the file: the drives, then the
// layouts. A found drive carries its current target; a missing one keeps its
// recorded name, serial and model as an unresolved target. Each member of a
// layout is resolved the same way; an unresolved member keeps its recorded
// name with an empty Path. A layout is missing when none of its members is
// present.
func (r *Recovery) Undos(targets []settings.Target) (found, missing []settings.Undo) {
	refs := make([]DriveRef, 0, len(targets))
	byName := make(map[string]settings.Target, len(targets))
	for _, t := range targets {
		refs = append(refs, DriveRef{Name: t.Name, Serial: t.Serial, Model: t.Model})
		byName[t.Name] = t
	}
	matched, _ := r.Match(refs)
	current := map[string]string{} // serial+model -> current name
	for name, e := range matched {
		current[e.Serial+"\x00"+e.Model] = name
	}
	for _, e := range r.Drives {
		orig := e.Original
		if name, ok := current[e.Serial+"\x00"+e.Model]; ok {
			t := byName[name]
			found = append(found, settings.Undo{Drive: &t, Original: &orig})
			continue
		}
		t := settings.Target{Name: e.Name, Serial: e.Serial, Model: e.Model}
		missing = append(missing, settings.Undo{Drive: &t, Original: &orig})
	}
	for _, e := range r.Arrays {
		a := &settings.ArrayUndo{Backend: e.Backend, Name: e.Name, UUID: e.UUID, Members: make([]settings.Disk, 0, len(e.Members))}
		if e.Extra != nil {
			a.Extra = maps.Clone(e.Extra)
		}
		resolved := 0
		for _, m := range e.Members {
			t := settings.Target{Name: m.Name, Serial: m.Serial, Model: m.Model}
			for _, c := range targets {
				if m.Serial != "" && c.Serial == m.Serial && c.Model == m.Model {
					t = c
					resolved++
					break
				}
			}
			a.Members = append(a.Members, settings.Disk{Target: t, Role: m.Role})
		}
		if resolved == 0 && len(e.Members) > 0 {
			missing = append(missing, settings.Undo{Array: a})
			continue
		}
		found = append(found, settings.Undo{Array: a})
	}
	return found, missing
}
