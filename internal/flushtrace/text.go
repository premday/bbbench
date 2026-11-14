package flushtrace

import "strconv"

// SourceLine says which tracepoints a Flush was timed from, how the events
// were paired and how many were lost. The tracepoints follow Source ("nvme",
// "block" or "bio"), so nothing else needs to know their names.
func SourceLine(s *Summary) string {
	lost := " · lost events: " + strconv.Itoa(s.Lost)
	switch s.Source {
	case "nvme":
		return "ftrace: nvme_setup_cmd -> nvme_complete_rq (Flush) · pairing: exact (ctrl,qid,cmdid)" + lost
	case "bio":
		return "ftrace: block_bio_queue -> block_bio_complete (Flush bios of the node, its members' flushes included) · pairing: first-in first-out" + lost
	}
	return "ftrace: block_rq_issue -> block_rq_complete (Flush) · pairing: first-in first-out" + lost
}
