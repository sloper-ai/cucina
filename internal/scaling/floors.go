// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling

import (
	"fmt"
	"strings"
	"time"
)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseFloorWindow converts the WorkerPool floorSchedule fields (weekday names
// "Mon".."Sun", "HH:MM" UTC) into a FloorWindow.
func ParseFloorWindow(name string, days []string, start, end string, minRunning int) (FloorWindow, error) {
	w := FloorWindow{Name: name, MinRunning: minRunning}
	if minRunning < 1 {
		return w, fmt.Errorf("floor window %q: minRunning %d < 1", name, minRunning)
	}
	if len(days) == 0 {
		return w, fmt.Errorf("floor window %q: no days", name)
	}
	for _, d := range days {
		key := strings.ToLower(strings.TrimSpace(d))
		if len(key) > 3 {
			key = key[:3] // "Monday" → "mon"
		}
		wd, ok := weekdays[key]
		if !ok {
			return w, fmt.Errorf("floor window %q: unknown weekday %q", name, d)
		}
		w.Days = append(w.Days, wd)
	}
	var err error
	if w.Start, err = parseHHMM(start); err != nil {
		return w, fmt.Errorf("floor window %q: start: %w", name, err)
	}
	if w.End, err = parseHHMM(end); err != nil {
		return w, fmt.Errorf("floor window %q: end: %w", name, err)
	}
	return w, nil
}

func parseHHMM(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Active reports whether the window covers now (UTC).
func (w FloorWindow) Active(now time.Time) bool {
	now = now.UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	off := now.Sub(midnight)
	startsOn := func(d time.Weekday) bool {
		for _, x := range w.Days {
			if x == d {
				return true
			}
		}
		return false
	}
	if w.End > w.Start {
		return startsOn(now.Weekday()) && off >= w.Start && off < w.End
	}
	// Crosses midnight (or End == Start: a full 24 h window).
	if startsOn(now.Weekday()) && off >= w.Start {
		return true
	}
	yesterday := (now.Weekday() + 6) % 7
	return startsOn(yesterday) && off < w.End
}

// FloorAt is minRunning' = max(spec.MinRunning, every active window).
func FloorAt(minRunning int, windows []FloorWindow, now time.Time) int {
	f := max(minRunning, 0)
	for _, w := range windows {
		if w.Active(now) {
			f = max(f, w.MinRunning)
		}
	}
	return f
}
