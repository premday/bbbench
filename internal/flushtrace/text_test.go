package flushtrace

import "testing"

func TestFlushSourceLine(t *testing.T) {
	for _, c := range []struct {
		s    Summary
		want string
	}{
		{Summary{Source: "nvme", Exact: true, Lost: 2}, "ftrace: nvme_setup_cmd -> nvme_complete_rq (Flush) · pairing: exact (ctrl,qid,cmdid) · lost events: 2"},
		{Summary{Source: "block"}, "ftrace: block_rq_issue -> block_rq_complete (Flush) · pairing: first-in first-out · lost events: 0"},
	} {
		if got := SourceLine(&c.s); got != c.want {
			t.Errorf("SourceLine(%s) = %q, want %q", c.s.Source, got, c.want)
		}
	}
}
