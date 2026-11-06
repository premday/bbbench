package diskinfo

import (
	"fmt"
	"strings"
	"time"
)

// dump renders everything the collector read as plain text: the summary
// first, then the sysfs values, then each command with its full output.
func (c *collector) dump(info Info) string {
	var b strings.Builder
	title := c.name
	if c.opts.Label != "" {
		title = c.opts.Label
	}
	fmt.Fprintf(&b, "bbbench disk configuration: %s\nCollected: %s\n", title, time.Now().Format(time.RFC3339))
	b.WriteString("Read-only: bbbench never changes the drive's configuration.\n\n")
	b.WriteString("== Summary ==\n")
	b.WriteString(Format(info))
	if len(c.sysfs) > 0 {
		b.WriteString("\n== sysfs ==\n")
		for _, s := range c.sysfs {
			fmt.Fprintf(&b, "%s = %s\n", s.label, s.body)
		}
	}
	for _, s := range c.cmds {
		fmt.Fprintf(&b, "\n== %s ==\n%s\n", s.label, s.body)
	}
	return b.String()
}

// Format renders the summary as text, one "Label: value" line per item that
// is known.
func Format(info Info) string {
	var b strings.Builder
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%-14s %s\n", k+":", v)
		}
	}
	line("Interface", info.Interface)
	link := info.LinkSpeed
	if info.LinkMax != "" {
		link += " (max " + info.LinkMax + ")"
	}
	if info.LinkBelowMax {
		link += " BELOW MAX"
	}
	line("Link", link)
	p := info.Power
	line("Power state", p.NVMeState)
	line("APST", p.APST)
	line("Runtime PM", p.RuntimePM)
	line("ASPM", p.ASPM)
	line("Link policy", p.LinkPolicy)
	line("Kernel cache", info.WriteCache.Kernel)
	line("Drive cache", info.WriteCache.Drive)
	s := info.Smart
	if s.Status != "" {
		line("SMART", s.Status+" "+bracket(s.Source))
	}
	line("SMART note", s.Detail)
	num := func(k string, n *int, unit string) {
		if n != nil {
			line(k, fmt.Sprintf("%d%s", *n, unit))
		}
	}
	num("Temperature", s.TempC, " C")
	num("Wear used", s.PercentUsed, "%")
	num("Power-on", s.PowerOnHours, " h")
	num("Media errors", s.MediaErrors, "")
	num("Reallocated", s.Reallocated, "")
	num("Pending", s.Pending, "")
	if b.Len() == 0 {
		return "(nothing could be read)\n"
	}
	return b.String()
}

func bracket(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}
