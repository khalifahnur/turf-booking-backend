package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type CampChild struct {
	FullName string `bson:"fullName" json:"fullName"`
	// Key is the lower-cased name, used to recognise the same child registered
	// twice without being tripped up by capital letters.
	Key string `bson:"key" json:"-"`
}

type CampParent struct {
	FullName    string `bson:"fullName" json:"fullName"`
	Email       string `bson:"email" json:"email"`
	PhoneNumber string `bson:"phoneNumber" json:"phoneNumber"`
}

// CampConsent records what the parent agreed to and when, so there is evidence
// of the waiver they accepted.
type CampConsent struct {
	TermsVersion string    `bson:"termsVersion" json:"termsVersion"`
	AcceptedAt   time.Time `bson:"acceptedAt" json:"acceptedAt"`
}

type CampPayment struct {
	Reference string `bson:"reference" json:"-"`
	// PaidAmountCents is what Paystack says it collected, kept to reconcile
	// against the price the server set.
	PaidAmountCents int64      `bson:"paidAmountCents,omitempty" json:"-"`
	PaidAt          *time.Time `bson:"paidAt,omitempty" json:"paidAt,omitempty"`
}

// CampRegistration is one child's place at the holiday camp for one or more
// weeks. The amounts are what the server charged, never what the browser sent.
type CampRegistration struct {
	ID         primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Child      CampChild          `bson:"child" json:"child"`
	Parent     CampParent         `bson:"parent" json:"parent"`
	Program    string             `bson:"program" json:"program"`
	Lunch      bool               `bson:"lunch" json:"lunch"`
	Weeks      []string           `bson:"weeks" json:"weeks"` // Monday of each week, YYYY-MM-DD
	PerWeekKES int                `bson:"perWeekKes" json:"perWeekKes"`
	AmountKES  int                `bson:"amountKes" json:"amountKes"`
	Consent    CampConsent        `bson:"consent" json:"consent"`
	Payment    CampPayment        `bson:"payment" json:"payment"`
	Status     string             `bson:"status" json:"status"`
	CreatedAt  time.Time          `bson:"createdAt" json:"createdAt"`
	UpdatedAt  time.Time          `bson:"updatedAt" json:"updatedAt"`
}
