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
