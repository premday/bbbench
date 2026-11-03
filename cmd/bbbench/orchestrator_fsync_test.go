package main

import "testing"

func TestFsyncLatency(t *testing.T) {
	// Shape of fio --output-format=json for a job with fsync=1.
	out, err := parseFioJsonOutput([]byte(`{"fio version":"fio-3.36","jobs":[
	  {"jobname":"a","write":{"iops":3917.0,"total_ios":100},
	   "sync":{"total_ios":100,"lat_ns":{"min":1,"max":9,"mean":236100.0,"stddev":1.0,"N":100}}},
	  {"jobname":"b","write":{"iops":1.0,"total_ios":300},
	   "sync":{"total_ios":300,"lat_ns":{"mean":238100.0}}}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	us, n := fsyncLatency(out)
	if n != 400 {
		t.Errorf("count = %d, want 400", n)
	}
	// weighted mean: (236100*100 + 238100*300) / 400 = 237600 ns
	if us < 237.59 || us > 237.61 {
		t.Errorf("mean = %.3f us, want 237.6", us)
	}
}

func TestFsyncLatency_NoFsyncs(t *testing.T) {
	out, err := parseFioJsonOutput([]byte(`{"jobs":[{"jobname":"a","write":{"iops":1}}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if us, n := fsyncLatency(out); us != 0 || n != 0 {
		t.Errorf("want (0,0) for a job without fsync, got (%v,%v)", us, n)
	}
}
