package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Member is a customer who gets the member price (see schedule.MemberEligible)
// on the sessions it covers. The M-Pesa number is what identifies them: it is
// the number a booking is paid from.
type Member struct {
	ID primitive.ObjectID `bson:"_id,omitempty"`
	// PhoneNumber is normalised (254712345678) and unique.
	PhoneNumber string    `bson:"phoneNumber"`
	FullName    string    `bson:"fullName"`
	CreatedAt   time.Time `bson:"createdAt"`
}
