package flushtrace

import "strconv"

// SourceLine says which tracepoints a Flush was timed from, how the events
// were paired and how many were lost. The tracepoints follow Source ("nvme"
// or "block"), so nothing else needs to know their names.
func SourceLine(s *Summary) string {
	lost := " · lost events: " + strconv.Itoa(s.Lost)
	if s.Source == "nvme" {
		return "ftrace: nvme_setup_cmd -> nvme_complete_rq (Flush) · pairing: exact (ctrl,qid,cmdid)" + lost
	}
	return "ftrace: block_rq_issue -> block_rq_complete (Flush) · pairing: first-in first-out" + lost
}
