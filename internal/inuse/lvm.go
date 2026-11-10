package inuse

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// listPVs asks lvm for its physical volumes. Without lvm there is nothing to
// ask (the udev signature still covers PVs), so it returns nil, nil.
func listPVs() ([]PV, error) {
	path, err := exec.LookPath("lvm")
	if err != nil {
		return nil, nil
	}
	out, err := exec.Command(path, "pvs", "--reportformat", "json", "-o", "pv_name,vg_name").Output()
	if err != nil {
		return nil, fmt.Errorf("lvm pvs: %w", err)
	}
	return parsePVs(out)
}

func parsePVs(data []byte) ([]PV, error) {
	var doc struct {
		Report []struct {
			PV []struct {
				Name string `json:"pv_name"`
				VG   string `json:"vg_name"`
			} `json:"pv"`
		} `json:"report"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse lvm pvs output: %w", err)
	}
	var pvs []PV
	if len(doc.Report) > 0 {
		for _, p := range doc.Report[0].PV {
			pvs = append(pvs, PV{Device: p.Name, VG: p.VG})
		}
	}
	return pvs, nil
}
