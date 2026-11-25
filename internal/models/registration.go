package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// RegistrationPayment is the M-Pesa payment behind a registration.
type RegistrationPayment struct {
	Reference string `bson:"reference" json:"-"`
	// PaidAmountCents is what Paystack says it collected, kept to reconcile
	// against the fee the server set.
	PaidAmountCents int64      `bson:"paidAmountCents,omitempty" json:"-"`
	PaidAt          *time.Time `bson:"paidAt,omitempty" json:"paidAt,omitempty"`
}

// Registration is one paid-for place: a team's entry in the tournament, or a
// player's place at the academy (Kind says which). The fields of the other kind
// are left out of the document. The amount is what the server charged, never what
// the browser sent.
type Registration struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"`
	Kind        string             `bson:"kind"`
	PhoneNumber string             `bson:"phoneNumber"`

	// Tournament. TeamName is the academy or club entering; TeamKey is its
	// lower-cased name, used to recognise the same team registering twice.
	TeamName   string   `bson:"teamName,omitempty"`
	TeamKey    string   `bson:"teamKey,omitempty"`
	Players    int      `bson:"players,omitempty"`
	Categories []string `bson:"categories,omitempty"`
	// RulesVersion and RulesAcceptedAt are the evidence that the team accepted the
	// tournament rules.
	RulesVersion    string    `bson:"rulesVersion,omitempty"`
	RulesAcceptedAt time.Time `bson:"rulesAcceptedAt,omitempty"`
	// Overflow lists the categories this registration paid into after they were
	// already full (a payment that landed after its place had been given up). The
	// admin must refund it.
	Overflow []string `bson:"overflow,omitempty"`

	// Academy.
	PlayerName   string `bson:"playerName,omitempty"`
	PlayerKey    string `bson:"playerKey,omitempty"`
	DateOfBirth  string `bson:"dateOfBirth,omitempty"` // YYYY-MM-DD
	GuardianName string `bson:"guardianName,omitempty"`

	AmountKES int                 `bson:"amountKes"`
	Payment   RegistrationPayment `bson:"payment"`
	Status    string              `bson:"status"`
	CreatedAt time.Time           `bson:"createdAt"`
	UpdatedAt time.Time           `bson:"updatedAt"`
}
