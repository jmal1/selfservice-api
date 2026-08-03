package runner

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Millis is a duration that crosses the wire as whole milliseconds.
//
// The obvious spelling — a plain time.Duration with a `json:"duration_ms"` tag —
// is a trap, and it shipped. encoding/json has no special case for
// time.Duration: it is an int64, so it marshals as its raw *nanosecond* count.
// A 30ms action therefore serialised as 30000000 under a field named "_ms".
//
// Nothing rejected it, because every consumer just believed the field name. The
// UI renders it with formatDuration(), which divides by 1000 and prints
// "30000.0s" — an 8.3-hour duration for a check that took 30 milliseconds. Every
// action on a student's results page showed a nonsense number, and the failure
// was silent in both directions: the runner produced it without complaint and
// the UI displayed it without complaint.
//
// This type exists so the unit lives in the Go type system rather than in a
// struct tag that only a reviewer could enforce. Conversion is explicit at every
// boundary, so the compiler catches a missed site.
type Millis time.Duration

// FromDuration converts a time.Duration for transport. Sub-millisecond values
// truncate toward zero: these durations are a human-facing display value, not an
// SLO measurement.
func FromDuration(d time.Duration) Millis { return Millis(d) }

// Duration converts back to a time.Duration for arithmetic and formatting.
func (m Millis) Duration() time.Duration { return time.Duration(m) }

// MarshalJSON emits whole milliseconds, matching the `_ms` field names.
func (m Millis) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(time.Duration(m).Milliseconds(), 10)), nil
}

// UnmarshalJSON reads whole milliseconds. This must stay the exact inverse of
// MarshalJSON: the runner POSTs these payloads to the engine, which parses them
// back, so an asymmetric pair would silently rescale every recorded duration by
// a factor of a million.
func (m *Millis) UnmarshalJSON(b []byte) error {
	ms, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("duration must be an integer number of milliseconds: %w", err)
	}
	*m = Millis(time.Duration(ms) * time.Millisecond)
	return nil
}

// String renders the underlying duration (e.g. "1.5s") so log lines and %v
// formatting stay readable instead of printing a bare integer.
func (m Millis) String() string { return time.Duration(m).String() }

var (
	_ json.Marshaler   = Millis(0)
	_ json.Unmarshaler = (*Millis)(nil)
)
