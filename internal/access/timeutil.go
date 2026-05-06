package access

import "time"

// CivilDateInLocation returns the calendar year, month, and day for t in loc.
func CivilDateInLocation(t time.Time, loc *time.Location) (y, m, d int) {
	if loc == nil {
		loc = time.Local
	}
	c := t.In(loc)
	return c.Year(), int(c.Month()), c.Day()
}
