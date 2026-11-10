package inuse

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type mount struct {
	MajMin string // "M:m"
	Point  string
	FSType string
	Source string
}

// decodeOctal turns the \040 style escapes of mountinfo and swaps into text.
func decodeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func readMountinfo(path string) ([]mount, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []mount
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, fl := range fields {
			if fl == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) < sep+3 {
			continue
		}
		out = append(out, mount{
			MajMin: fields[2],
			Point:  decodeOctal(fields[4]),
			FSType: fields[sep+1],
			Source: decodeOctal(fields[sep+2]),
		})
	}
	return out, sc.Err()
}
