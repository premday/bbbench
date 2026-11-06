package settings

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	hdparmRe  = regexp.MustCompile(`write-caching\s*=\s*(\d)`)
	nvmeValRe = regexp.MustCompile(`(?i)value:\s*(0x[0-9a-f]+|\d+)`)
	nvmeCtlRe = regexp.MustCompile(`^(nvme\d+)n\d+`)
)

const (
	enabled  = "enabled"
	disabled = "disabled"
)

// writeCache is the drive's own volatile write cache. It never writes the
// kernel's queue/write_cache, only reads it for information.
type writeCache struct{ env Env }

// NewWriteCache returns the disk write cache policy setting.
func NewWriteCache(env Env) Setting { return &writeCache{env: env.fill()} }

func (w *writeCache) Name() string { return "disk write cache policy" }
func (w *writeCache) Kind() Kind   { return Applied }
func (w *writeCache) Scope() Scope { return ScopeDrive }
func (w *writeCache) Input() Input { return InputValues }
func (w *writeCache) Values() []Value {
	return []Value{{ID: enabled, Label: "Write cache enabled"}, {ID: disabled, Label: "Write cache disabled"}}
}

// class maps an interface name to the tool family: nvme, sata, sas or "".
func class(iface string) string {
	switch {
	case strings.HasPrefix(iface, "NVMe"):
		return "nvme"
	case strings.HasPrefix(iface, "SATA"):
		return "sata"
	case strings.HasPrefix(iface, "SAS"), strings.HasPrefix(iface, "SCSI"):
		return "sas"
	}
	return ""
}

func (w *writeCache) Applicable(t Target) (bool, string) {
	need := map[string][2]string{
		"nvme": {"nvme", "nvme is not installed (nvme-cli)"},
		"sata": {"hdparm", "hdparm is not installed"},
		"sas":  {"sdparm", "sdparm is not installed"},
	}
	n, ok := need[class(t.Interface)]
	if !ok {
		return false, "no controllable volatile write cache on this interface"
	}
	if _, err := w.env.LookPath(n[0]); err != nil {
		return false, n[1]
	}
	return true, ""
}

func (w *writeCache) Read(ctx context.Context, t Target) (Reading, error) {
	r := Reading{At: time.Now()}
	if b, err := os.ReadFile(filepath.Join(w.env.Sys, "block", t.Name, "queue", "write_cache")); err == nil {
		r.Kernel = strings.TrimSpace(string(b))
	}
	switch class(t.Interface) {
	case "nvme":
		ctl, err := nvmeController(t)
		if err != nil {
			return r, err
		}
		out, err := w.env.Run(ctx, "nvme", "get-feature", ctl, "-f", "6")
		// A non-zero exit with output is accepted (hdparm can exit non-zero
		// while still printing); the parse below decides. No output is an error.
		if err != nil && len(out) == 0 {
			return r, fmt.Errorf("nvme get-feature %s: %w", ctl, err)
		}
		m := nvmeValRe.FindStringSubmatch(string(out))
		if m == nil {
			return r, fmt.Errorf("nvme get-feature %s: no value in %q", ctl, strings.TrimSpace(string(out)))
		}
		var v uint64
		if _, err := fmt.Sscan(m[1], &v); err != nil {
			return r, fmt.Errorf("nvme get-feature %s: %w", ctl, err)
		}
		r.Value = onOff(v&1 == 1)
	case "sata":
		out, err := w.env.Run(ctx, "hdparm", "-W", t.Path)
		// A non-zero exit with output is accepted (hdparm can exit non-zero
		// while still printing); the parse below decides. No output is an error.
		if err != nil && len(out) == 0 {
			return r, fmt.Errorf("hdparm -W %s: %w", t.Path, err)
		}
		m := hdparmRe.FindStringSubmatch(string(out))
		if m == nil {
			return r, fmt.Errorf("hdparm -W %s: no write-caching in %q", t.Path, strings.TrimSpace(string(out)))
		}
		r.Value = onOff(m[1] == "1")
	case "sas":
		on, err := w.env.GetWCE(t.Path)
		if err != nil {
			return r, fmt.Errorf("read WCE of %s: %w", t.Path, err)
		}
		r.Value = onOff(on)
	default:
		return r, fmt.Errorf("no controllable write cache on %q", t.Interface)
	}
	return r, nil
}

func onOff(on bool) string {
	if on {
		return enabled
	}
	return disabled
}

// nvmeController derives the controller node from the namespace name.
func nvmeController(t Target) (string, error) {
	m := nvmeCtlRe.FindStringSubmatch(t.Name)
	if m == nil {
		return "", fmt.Errorf("%q is not an NVMe namespace name", t.Name)
	}
	return "/dev/" + m[1], nil
}

func (w *writeCache) Apply(ctx context.Context, t Target, v Value) error {
	if v.ID != enabled && v.ID != disabled {
		return fmt.Errorf("unknown write cache value %q", v.ID)
	}
	on := v.ID == enabled
	bit := map[bool]string{true: "1", false: "0"}[on]
	var name string
	var args []string
	switch class(t.Interface) {
	case "nvme":
		ctl, err := nvmeController(t)
		if err != nil {
			return err
		}
		name, args = "nvme", []string{"set-feature", ctl, "-f", "6", "-V", bit}
	case "sata":
		name, args = "hdparm", []string{"-W" + bit, t.Path}
	case "sas":
		// Volatile only: never --save. UNVERIFIED: this syntax must be
		// checked against sdparm(8) on a real SAS host before release.
		name, args = "sdparm", []string{"--set=WCE=" + bit, t.Path}
	default:
		return fmt.Errorf("no controllable write cache on %q", t.Interface)
	}
	if out, err := w.env.Run(ctx, name, args...); err != nil {
		return fmt.Errorf("%s %s: %w %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (w *writeCache) Restore(ctx context.Context, t Target, original Reading) error {
	if original.Value == "" {
		return fmt.Errorf("no original write cache value was recorded for %s; not restoring", t.Name)
	}
	cur, err := w.Read(ctx, t)
	if err == nil && cur.Value == original.Value {
		return nil
	}
	return w.Apply(ctx, t, Value{ID: original.Value})
}
