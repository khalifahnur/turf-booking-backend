package schedule

import "time"

// The types below are what GET /api/v1/schedule sends, so the booking page
// draws exactly the days and sessions the server will accept: it never works
// out opening hours or public holidays for itself.

// SnapshotSession is one bookable session. Label is the exact text the booking
// request takes as its time range. From and To are minutes after the operating
// day's midnight and pass 1440 for sessions that run into the next morning.
type SnapshotSession struct {
	Minutes int    `json:"minutes"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Label   string `json:"label"`
	From    int    `json:"from"`
	To      int    `json:"to"`
}

// SnapshotWindow is a stretch of the day with sessions in it.
type SnapshotWindow struct {
	Open  string `json:"open"`
	Close string `json:"close"`
	From  int    `json:"from"`
	To    int    `json:"to"`
}

// SnapshotKind is what one kind of day (weekday, weekend, holiday) offers.
type SnapshotKind struct {
	Windows  []SnapshotWindow  `json:"windows"`
	Sessions []SnapshotSession `json:"sessions"`
}

// SnapshotDay is one date in the booking calendar.
type SnapshotDay struct {
	Date    string  `json:"date"`
	Kind    DayKind `json:"kind"`
	Holiday string  `json:"holiday,omitempty"`
}

// Snapshot is the booking calendar: the opening hours of each kind of day and
// the kind of each day that can be booked.
type Snapshot struct {
	Timezone            string                   `json:"timezone"`
	Today               string                   `json:"today"`
	MinRemainingMinutes int                      `json:"minRemainingMinutes"`
	Kinds               map[DayKind]SnapshotKind `json:"kinds"`
	Days                []SnapshotDay            `json:"days"`
}

// SessionLengths are the session lengths that can be booked, in minutes.
var SessionLengths = []int{60, 120}

// NewSnapshot builds the booking calendar as of now: BookableDays days
// starting with the current operating day.
func NewSnapshot(now time.Time) Snapshot {
	first := operatingMidnight(now.In(location))
	s := Snapshot{
		Timezone:            location.String(),
		Today:               first.Format("2006-01-02"),
		MinRemainingMinutes: int(MinRemaining / time.Minute),
		Kinds:               make(map[DayKind]SnapshotKind, len(openingHours)),
		Days:                make([]SnapshotDay, 0, BookableDays),
	}

	for _, kind := range Kinds() {
		k := SnapshotKind{}
		for _, w := range openingHours[kind] {
			k.Windows = append(k.Windows, SnapshotWindow{
				Open: FormatClock(w.Open), Close: FormatClock(w.Close), From: w.Open, To: w.Close,
			})
		}
		// Listed by length, each length in time order.
		for _, minutes := range SessionLengths {
			for _, slot := range Slots(kind, minutes) {
				k.Sessions = append(k.Sessions, SnapshotSession{
					Minutes: minutes,
					Start:   FormatClock(slot.Start),
					End:     FormatClock(slot.End),
					Label:   slot.Label(),
					From:    slot.Start,
					To:      slot.End,
				})
			}
		}
		s.Kinds[kind] = k
	}

	for i := 0; i < BookableDays; i++ {
		day := first.AddDate(0, 0, i)
		kind, holiday := kindOfDay(day)
		s.Days = append(s.Days, SnapshotDay{Date: day.Format("2006-01-02"), Kind: kind, Holiday: holiday})
	}
	return s
}
