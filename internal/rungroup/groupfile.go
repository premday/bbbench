package rungroup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"bbbench/internal/settings"
)

type GroupFile struct {
	ID        string                      `json:"id"`
	Created   time.Time                   `json:"created"`
	Host      string                      `json:"host"`
	Plan      []string                    `json:"plan"`
	Setting   string                      `json:"setting"`
	Strategy  Strategy                    `json:"strategy"`
	Values    []string                    `json:"values"`
	Originals map[string]settings.Reading `json:"originals"`
	Runs      []GroupRun                  `json:"runs"`
	Restore   GroupRestore                `json:"restore"`
}

type GroupRun struct {
	RunID  string       `json:"run_id"`
	Round  int          `json:"round"`
	Status RunStatus    `json:"status"`
	Drives []GroupDrive `json:"drives"`
}

type GroupDrive struct {
	Device string `json:"device"`
	Serial string `json:"serial"`
	Model  string `json:"model"`
	Value  string `json:"value"`
	Label  Label  `json:"label"`
}

// GroupRestore tracks restoring the originals; Status is pending, done or failed.
type GroupRestore struct {
	Status string   `json:"status"`
	Failed []string `json:"failed"`
}

// writeAtomic writes data to path through a temp file in the same
// directory and a rename, leaving no temp file behind.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Save writes group_<ID>_group.json into dir atomically.
func (g *GroupFile) Save(dir string) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "group_"+g.ID+"_group.json"), data)
}

func LoadGroup(path string) (*GroupFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	g := &GroupFile{}
	if err := json.Unmarshal(data, g); err != nil {
		return nil, err
	}
	return g, nil
}

// LoadGroups loads every group file in dir, skipping unreadable ones.
func LoadGroups(dir string) ([]*GroupFile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "group_*_group.json"))
	if err != nil {
		return nil, err
	}
	var out []*GroupFile
	for _, p := range paths {
		if g, err := LoadGroup(p); err == nil {
			out = append(out, g)
		}
	}
	return out, nil
}
