package config

import (
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

type Root struct {
	Fio     FioConfig     `yaml:"fio"`
	Fioplot any           `yaml:"fioplot"` // not used by generator
	BBBench BBBenchConfig `yaml:"bbbench"`
}

type FioConfig struct {
	Generated struct {
		Path string `yaml:"path"`
	} `yaml:"generated"`
}

type BBBenchConfig struct {
	WCE         bool                   `yaml:"wce"`
	WorkloadsDB map[string]WorkloadDef `yaml:"workloadsdb"`
	Workloads   map[string][]string    `yaml:"workloads"`
}

type WorkloadDef struct {
	Blocksize          string `yaml:"blocksize"`
	Fulldisk           bool   `yaml:"fulldisk"`
	Duration           int    `yaml:"duration"`
	WriteLog           bool   `yaml:"write_log"`
	RandomDistribution string `yaml:"random_distribution"`
	// Per-job overrides of the [global] ioengine/iodepth/direct. Unset
	// (zero value, nil for Direct) means "use the template's global".
	Ioengine string `yaml:"ioengine"`
	Iodepth  int    `yaml:"iodepth"`
	Direct   *bool  `yaml:"direct"`
	// Fsync issues an fsync after every N writes (fio's fsync=N). 0 = off.
	Fsync   int            `yaml:"fsync"`
	Process map[string]any `yaml:"process"`
}

func Load(path string) (*Root, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadBytes(b)
}

func LoadFS(fsys fs.FS, path string) (*Root, error) {
	b, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}
	return LoadBytes(b)
}

func LoadBytes(b []byte) (*Root, error) {
	var r Root
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.BBBench.WorkloadsDB == nil || r.BBBench.Workloads == nil {
		return nil, fmt.Errorf("config missing bbbench.workloadsdb or bbbench.workloads")
	}
	return &r, nil
}
