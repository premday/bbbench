package diskinfo

import (
	"encoding/json"
	"path/filepath"
)

// smartctlJSON is the part of `smartctl -j` that is read.
type smartctlJSON struct {
	Smartctl struct {
		Messages []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	NVMe *struct {
		PercentUsed *int `json:"percentage_used"`
		MediaErrors *int `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
	ATA *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

func ptr(n int) *int { return &n }

// smart reads the health of a disk with smartctl, or for NVMe without
// smartmontools from `nvme smart-log`.
func (c *collector) smart(name string, isNVMe bool) Smart {
	dev := filepath.Join(c.opts.Dev, name)
	out, ok := c.run("smartctl", "-j", "-H", "-A", dev)
	if ok {
		// smartctl reports problems through its exit status but still prints
		// valid JSON, so only the output counts.
		if s, ok := parseSmartctl(out); ok {
			return s
		}
	}
	if isNVMe {
		if m := nvmeNameRe.FindStringSubmatch(name); m != nil {
			if out, ok := c.run("nvme", "smart-log", filepath.Join(c.opts.Dev, m[1]), "-o", "json"); ok {
				if s, ok := parseNVMeSmartLog(out); ok {
					return s
				}
			}
		}
	}
	if !ok {
		return Smart{Detail: "smartctl is not installed or returned nothing (smartmontools)"}
	}
	return Smart{Detail: "smartctl returned no health status"}
}

// parseSmartctl reads `smartctl -j -H -A` output. ok is false when it holds
// no health information at all.
func parseSmartctl(out []byte) (Smart, bool) {
	var j smartctlJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return Smart{}, false
	}
	s := Smart{Source: "smartctl"}
	if j.SmartStatus != nil {
		s.Status = "FAILED"
		if j.SmartStatus.Passed {
			s.Status = "PASSED"
		}
	}
	if j.Temperature != nil {
		s.TempC = ptr(j.Temperature.Current)
	}
	if j.PowerOnTime != nil {
		s.PowerOnHours = ptr(j.PowerOnTime.Hours)
	}
	if j.NVMe != nil {
		s.PercentUsed, s.MediaErrors = j.NVMe.PercentUsed, j.NVMe.MediaErrors
	}
	if j.ATA != nil {
		for _, a := range j.ATA.Table {
			switch a.ID {
			case 5:
				s.Reallocated = ptr(a.Raw.Value)
			case 197:
				s.Pending = ptr(a.Raw.Value)
			}
		}
	}
	if s.Status == "" {
		if len(j.Smartctl.Messages) > 0 {
			s.Detail = j.Smartctl.Messages[0].String
		}
		return s, s.Detail != "" || s.TempC != nil
	}
	return s, true
}

// parseNVMeSmartLog reads `nvme smart-log -o json`. The NVMe critical warning
// byte is the controller's own health verdict: 0 means none.
func parseNVMeSmartLog(out []byte) (Smart, bool) {
	var j struct {
		CriticalWarning *int `json:"critical_warning"`
		Temperature     *int `json:"temperature"` // Kelvin
		PercentUsed     *int `json:"percent_used"`
		MediaErrors     *int `json:"media_errors"`
		PowerOnHours    *int `json:"power_on_hours"`
	}
	if err := json.Unmarshal(out, &j); err != nil || j.CriticalWarning == nil {
		return Smart{}, false
	}
	s := Smart{Source: "nvme smart-log", Status: "PASSED",
		PercentUsed: j.PercentUsed, MediaErrors: j.MediaErrors, PowerOnHours: j.PowerOnHours,
		Detail: "from the NVMe critical warning flags (smartctl not used)"}
	if *j.CriticalWarning != 0 {
		s.Status = "FAILED"
		s.Detail = "NVMe critical warning flags are set"
	}
	if j.Temperature != nil && *j.Temperature > 200 {
		s.TempC = ptr(*j.Temperature - 273)
	}
	return s, true
}
