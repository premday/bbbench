package rungroup

import (
	"encoding/json"
	"errors"
	"os"

	"bbbench/internal/settings"
)

// Recovery records the original values so a crashed run can be undone.
type Recovery struct {
	Setting string          `json:"setting"`
	Drives  []RecoveryDrive `json:"drives"`
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
// becomes a Drives entry.
func RecoveryOf(setting string, undos []settings.Undo) *Recovery {
	r := &Recovery{Setting: setting}
	for _, u := range undos {
		if u.Drive == nil || u.Original == nil {
			continue
		}
		r.Drives = append(r.Drives, RecoveryDrive{Serial: u.Drive.Serial, Model: u.Drive.Model, Name: u.Drive.Name, Original: *u.Original})
	}
	return r
}

// Undos are the entries as undo records, matched to the current targets by
// serial and model (Match), in the order of the file. A found entry carries
// its current target; a missing one keeps its recorded name, serial and
// model as an unresolved target.
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
	return found, missing
}
