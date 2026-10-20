// Package seoul is the single calendar-day clock for GODA.
// Todomate dates and Google Calendar instants are both interpreted as
// Asia/Seoul calendar days so reads and writes do not drift by a day.
package seoul

import (
	"time"
	_ "time/tzdata"
)

// Location is Asia/Seoul, falling back to a fixed +09:00 offset if the
// zone database is unavailable.
var Location = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return time.FixedZone("Asia/Seoul", 9*60*60)
	}
	return loc
}()

// Midnight returns the Asia/Seoul calendar date of t at 00:00.
func Midnight(t time.Time) time.Time {
	y, m, d := t.In(Location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, Location)
}

// DateString is the Asia/Seoul calendar day in YYYY-MM-DD.
func DateString(t time.Time) string {
	return Midnight(t).Format("2006-01-02")
}
