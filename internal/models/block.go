package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Block is a session the admin has taken off the booking page: someone who wants
// to pay before it is sold, or a team that plays the same time every week. It
// holds the pitch exactly as a paid booking does, so nobody can book around it.
type Block struct {
	ID primitive.ObjectID `bson:"_id,omitempty"`
	// Date is the operating day the session belongs to (YYYY-MM-DD), and Time its
	// canonical label ("19:00 – 21:00"), both as a booking stores them.
	Date string `bson:"date"`
	Time string `bson:"time"`
	// PitchType says how much of the pitch is held: "8Aside" is the whole pitch,
	// "5Aside" is one half (the same sizes a booking takes).
	PitchType string `bson:"pitchType"`
	// Reason is the admin's note on who it is for. It is never shown to customers.
	Reason string `bson:"reason"`
	// SeriesID ties together the blocks made by one request, so a team's weekly
	// slots can be lifted in one go.
	SeriesID  string    `bson:"seriesId"`
	CreatedAt time.Time `bson:"createdAt"`
}

// StatusBlocked is what the public calendar lists a block as.
const StatusBlocked = "Blocked"
