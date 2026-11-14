package flushtrace

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlushSourceLine(t *testing.T) {
	for _, c := range []struct {
		s    Summary
		want string
	}{
		{Summary{Source: "nvme", Exact: true, Lost: 2}, "ftrace: nvme_setup_cmd -> nvme_complete_rq (Flush) · pairing: exact (ctrl,qid,cmdid) · lost events: 2"},
		{Summary{Source: "block"}, "ftrace: block_rq_issue -> block_rq_complete (Flush) · pairing: first-in first-out · lost events: 0"},
		{Summary{Source: "bio"}, "ftrace: block_bio_queue -> block_bio_complete (Flush bios of the node, its members' flushes included) · pairing: first-in first-out · lost events: 0"},
	} {
		if got := SourceLine(&c.s); got != c.want {
			t.Errorf("SourceLine(%s) = %q, want %q", c.s.Source, got, c.want)
		}
	}
}

func TestSummarizeEvents(t *testing.T) {
	for src, want := range map[string]string{"nvme": "nvme_setup_cmd -> nvme_complete_rq", "block": "block_rq_issue -> block_rq_complete",
		"bio": "block_bio_queue -> block_bio_complete"} {
		s := Summarize(src, src == "nvme", 0, nil)
		if len(s.Events) != 1 || s.Events[0] != want {
			t.Errorf("%s: Events = %v", src, s.Events)
		}
	}
	if s := Summarize("", false, 0, nil); len(s.Events) != 0 {
		t.Errorf("unknown source: Events = %v", s.Events)
	}
}

func TestSummaryJSONCarriesEvents(t *testing.T) {
	b, err := json.Marshal(Summarize("block", false, 0, nil))
	if err != nil || !strings.Contains(string(b), `"events":["block_rq_issue -\u003e block_rq_complete"]`) {
		t.Errorf("json = %s, err %v", b, err)
	}
}
