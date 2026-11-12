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
	b.WriteString("This dump is a read-only collection: gathering it does not change the drive's configuration.\n\n")
	b.WriteString("== Summary ==\n")
	b.WriteString(Format(info))
	if c.virt == "" {
		c.smbiosDump(&b)
	}
	if len(c.sysfs) > 0 {
		b.WriteString("\n== sysfs ==\n")
		for _, s := range c.sysfs {
			fmt.Fprintf(&b, "%s = %s\n", s.label, s.body)
		}
	}
	b.WriteString(c.queueText)
	b.WriteString(c.moduleText)
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
	if sl := info.Slot; sl != nil {
		line("Slot", strings.TrimSpace(sl.Name+" "+bracket(strings.Join(nonEmpty(sl.Source, sl.Detail), ", "))))
	}
	if ct := info.Controller; ct != nil {
		line("Controller", strings.TrimSpace(ct.Kind+" "+bracket(strings.Join(nonEmpty(ct.Name, ct.Driver, ct.PCI), ", "))))
		line("Ctrl IDs", ct.IDs)
		line("Ctrl firmware", ct.Firmware)
		line("Ctrl BIOS", ct.BIOS)
		line("SAS address", ct.SASAddress)
		line("Ctrl mode", ct.Mode)
	}
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
	formatQueue(info.Queue, line)
	if b.Len() == 0 {
		return "(nothing could be read)\n"
	}
	return b.String()
}

func nonEmpty(s ...string) []string {
	var out []string
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// smbiosDump lists what the DMI table says about slots and onboard devices
// (the whole table, so a missing match can be debugged), or why it is unknown.
func (c *collector) smbiosDump(b *strings.Builder) {
	b.WriteString("\n== SMBIOS slots and onboard devices ==\n")
	t := c.smbiosTable()
	if t == nil {
		fmt.Fprintf(b, "(not readable: %v; the table is root-only)\n", c.smbiosErr)
		return
	}
	for _, s := range t.Slots {
		addr := "no PCI address"
		if s.HasAddr {
			addr = s.Addr.String()
		}
		fmt.Fprintf(b, "slot %q: %s, %s, ID %d, %s\n", s.Designation, s.Type, s.Usage, s.ID, addr)
	}
	for _, o := range t.Onboard {
		fmt.Fprintf(b, "onboard %q: %s, %s, enabled=%t\n", o.Designation, o.Type, o.Addr, o.Enabled)
	}
	if len(t.Slots)+len(t.Onboard) == 0 {
		b.WriteString("(no slot or onboard device entries)\n")
	}
}

func bracket(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}
