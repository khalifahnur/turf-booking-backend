package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Team struct {
	FullName string `bson:"fullName" json:"fullName"`
	//TeamName    string `bson:"teamName" json:"teamName"`
	Email       string `bson:"email" json:"email"`
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

// DiscountMember marks a booking that was charged the member price.
const DiscountMember = "member"

type Transaction struct {
	PaymentStatus string `bson:"paymentStatus" json:"paymentStatus"`
	Reference     string `bson:"reference" json:"reference"`
	// Amount is the price in whole KES that the server charged for the booking.
	Amount int `bson:"amount,omitempty" json:"amount,omitempty"`
	// ListAmount is the standard price, recorded only when a discount took
	// something off it. Discount names that discount (DiscountMember).
	ListAmount int       `bson:"listAmount,omitempty" json:"listAmount,omitempty"`
	Discount   string    `bson:"discount,omitempty" json:"discount,omitempty"`
	Timestamp  time.Time `bson:"timestamp" json:"timestamp"`
}

type Booking struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	Team           Team               `bson:"team" json:"team"`
	BookingDetails BookingDetails     `bson:"bookingDetails" json:"bookingDetails"`
	Transaction    Transaction        `bson:"transaction" json:"transaction"`
}
