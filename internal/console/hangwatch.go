package console

import (
	"strings"
	"time"
)

// The early give-up on vLLM's hang: a machine still loading whose log has shown nothing but
// vLLM's shared-memory hang message for hangGiveUpAfter is given up and destroyed then, not at
// the hour. [OBS] hung two-card machines printed that one line a minute and nothing else
// (docs/providers.md#multi-card-machines). A load that prints anything else is never cut short
// by this.
const (
	hangMessage = "No available shared memory broadcast block"
	// hangGiveUpAfter is how long every new log line must have been the hang message.
	hangGiveUpAfter = 10 * time.Minute
	// hangMinLines is how many hang lines must have arrived in that time, so a log that was merely
	// not read for ten minutes is never taken for a hang.
	hangMinLines = 5
	// hangReadInterval is how often the log is read once the hang message has been seen.
	hangReadInterval = time.Minute
	// hangLogTail is how many lines each read asks for.
	hangLogTail = 200
)

// hangWatch follows one loading machine's log reads. It holds only what the reads showed.
type hangWatch struct {
	prev      []string  // the previous read, to tell which lines of the next one are new
	quietFrom time.Time // when the last non-hang line was seen (or the first read, before any hang line)
	hangLines int       // hang lines seen since quietFrom
	lastRead  time.Time
}

// active is true once the hang message has been seen since the last other line.
func (h *hangWatch) active() bool { return h.hangLines > 0 }

// observe takes one read of the machine's log at now. Every line that is new since the previous
// read counts: a hang line adds to hangLines, any other non-empty line resets the ten minutes.
func (h *hangWatch) observe(lines []string, now time.Time) {
	h.lastRead = now
	fresh := lines
	if len(h.prev) > 0 {
		last := h.prev[len(h.prev)-1]
		for i := len(lines) - 1; i >= 0; i-- {
			if lines[i] == last {
				fresh = lines[i+1:]
				break
			}
		}
	}
	if h.quietFrom.IsZero() {
		h.quietFrom = now
	}
	for _, l := range fresh {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.Contains(l, hangMessage) {
			h.hangLines++
			continue
		}
		h.quietFrom = now
		h.hangLines = 0
	}
	h.prev = lines
}

// dueForRead says whether the minute read is due: only once the hang message has been seen.
func (h *hangWatch) dueForRead(now time.Time) bool {
	return h.active() && now.Sub(h.lastRead) >= hangReadInterval
}

// shouldGiveUp is true when only hang lines have arrived for hangGiveUpAfter, at least
// hangMinLines of them.
func (h *hangWatch) shouldGiveUp(now time.Time) bool {
	return h.active() && h.hangLines >= hangMinLines && now.Sub(h.quietFrom) >= hangGiveUpAfter
}
