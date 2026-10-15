// Package logring is the bounded in-memory line buffer the console panel reads. It imports
// nothing else in this program.
package logring

import (
	"sync"
	"time"
)

// ringHolds is how many lines the ring keeps before the oldest one is dropped. Not a setting:
// the ring is a debugging aid, not a record, and it survives only for the life of the process.
const ringHolds = 2000

// Line is one entry: the sequence number, when it was written, which package wrote it, and the
// sentence itself. `Sequence` is the page's cursor into the ring, per `after=<sequence>`.
type Line struct {
	Sequence int64
	At       string // UTC ISO to the second with a trailing Z
	Source   string // the part of the program that wrote the line, such as "server" or "providers"
	Text     string
}

// Ring is a bounded, ordered buffer of Line, safe for concurrent readers and writers.
type Ring struct {
	mu      sync.Mutex
	lines   []Line
	nextSeq int64
}

// New returns an empty ring.
func New() *Ring {
	return &Ring{nextSeq: 1}
}

// Add appends one line from the named source, stamping it with the next sequence number and
// the current UTC time, and drops the oldest line once the ring is full.
func (r *Ring) Add(source, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := Line{
		Sequence: r.nextSeq,
		At:       nowUTC(),
		Source:   source,
		Text:     text,
	}
	r.nextSeq++
	r.lines = append(r.lines, line)
	if len(r.lines) > ringHolds {
		r.lines = r.lines[len(r.lines)-ringHolds:]
	}
}

// After returns every line with a sequence number greater than after, oldest first, plus the
// newest sequence number currently in the ring (0 if the ring is empty) and how many lines the
// ring holds at most.
func (r *Ring) After(after int64) (lines []Line, newestSequence int64, ringHoldsAt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if l.Sequence > after {
			lines = append(lines, l)
		}
	}
	if len(r.lines) > 0 {
		newestSequence = r.lines[len(r.lines)-1].Sequence
	}
	return lines, newestSequence, ringHolds
}

// lineTimeLayout is the shape of a line's time. It is the shape the store keeps timestamps in
// (store.TimeLayout); this package imports nothing else in this program, so it names its own.
const lineTimeLayout = "2006-01-02T15:04:05Z"

// nowUTC formats the current time as UTC ISO to the second with a trailing Z, the shape the
// page's app.js expects for a log line's `at` field.
func nowUTC() string {
	return time.Now().UTC().Format(lineTimeLayout)
}
