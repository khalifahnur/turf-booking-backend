package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Team struct {
	FullName    string `bson:"fullName" json:"fullName"`
	TeamName    string `bson:"teamName" json:"teamName"`
	PhoneNumber string `bson:"phoneNumber" json:"phoneNumber"`
}

type BookingDetails struct {
	Date          string    `bson:"date" json:"date"`
	Time          string    `bson:"time" json:"time"`
	PitchType     string    `bson:"pitchType" json:"pitchType"`
	CreatedAt     time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt     time.Time `bson:"updatedAt" json:"updatedAt"`
	BookingStatus string    `bson:"bookingStatus" json:"bookingStatus"`
}

type Transaction struct {
	PaymentStatus string    `bson:"paymentStatus" json:"paymentStatus"`
	Reference     string    `bson:"reference" json:"reference"`
	Timestamp     time.Time `bson:"timestamp" json:"timestamp"`
}

type Booking struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	Team           Team               `bson:"team" json:"team"`
	BookingDetails BookingDetails     `bson:"bookingDetails" json:"bookingDetails"`
	Transaction    Transaction        `bson:"transaction" json:"transaction"`
}
