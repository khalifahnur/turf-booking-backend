package handlers

import (
	"context"
	"log"

	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// bookingStore is what taking a booking needs from storage. It is an interface
// so the booking and pricing rules can be tested against an in-memory fake; the
// MongoDB implementation below is the real one.
type bookingStore interface {
	// Occupancy is what is already on the pitch on a date.
	Occupancy(ctx context.Context, date string) ([]schedule.Occupancy, error)
	Insert(ctx context.Context, b models.Booking) error
	// MarkFailed fails the booking with this payment reference.
	MarkFailed(ctx context.Context, reference string) error
	// MemberMonth is where a number stands with the member discount in a month
	// ("2026-11").
	MemberMonth(ctx context.Context, phone, month string) (MemberMonth, error)
	// MemberMonths is MemberMonth for every number that has booked in the month.
	MemberMonths(ctx context.Context, month string) (map[string]MemberMonth, error)
}

// MemberMonth is where a number stands with the member discount in a month.
type MemberMonth struct {
	// Paid is how many 8-a-side 2 hour sessions the number has paid for in the
	// month, whoever it was booked by: these are the sessions that count towards
	// the discount. Sessions still waiting for their payment do not, so a member
	// cannot reach the discount by starting bookings they never pay for.
	Paid int
	// DiscountHeld is true once the month's discount has been given: a
	// member-priced session is paid for, or is still waiting for its payment.
	DiscountHeld bool
}

// ---- MongoDB queries --------------------------------------------------------
// Plain functions that only build filters, so tests can check them against the
// BSON field names without a database.

func occupancyFilter(date string) bson.M {
	return bson.M{
		"bookingDetails.date":          date,
		"bookingDetails.bookingStatus": bson.M{"$in": occupyingStatuses},
	}
}

// memberMonthFilter matches the 8-a-side bookings that hold a place in a month,
// optionally for one number. A session counts towards the month its date is in,
// whenever it was booked. Which of them are 2 hour sessions is worked out from
// the stored time (summariseMemberMonths), because a time is stored as text.
func memberMonthFilter(phone, from, to string) bson.M {
	f := bson.M{
		"bookingDetails.pitchType":     schedule.Pitch8Aside,
		"bookingDetails.date":          bson.M{"$gte": from, "$lt": to},
		"bookingDetails.bookingStatus": bson.M{"$in": occupyingStatuses},
	}
	if phone != "" {
		f["team.phoneNumber"] = phone
	}
	return f
}

// memberMonthFields are the only fields summariseMemberMonths reads.
var memberMonthFields = bson.M{
	"team.phoneNumber":             1,
	"bookingDetails.pitchType":     1,
	"bookingDetails.time":          1,
	"bookingDetails.bookingStatus": 1,
	"transaction.discount":         1,
}

// summariseMemberMonths turns the bookings memberMonthFilter matched into where
// each number stands. Anything it cannot read (a time that is not a range) is
// not counted, which leaves the member at the standard price.
func summariseMemberMonths(bookings []models.Booking) map[string]MemberMonth {
	out := map[string]MemberMonth{}
	for _, b := range bookings {
		start, end, ok := schedule.ParseStoredRange(b.BookingDetails.Time)
		if !ok || !schedule.MemberEligible(b.BookingDetails.PitchType, end-start) {
			continue
		}
		m := out[b.Team.PhoneNumber]
		if isPaid(b.BookingDetails.BookingStatus) {
			m.Paid++
		}
		if b.Transaction.Discount == models.DiscountMember {
			m.DiscountHeld = true
		}
		out[b.Team.PhoneNumber] = m
	}
	return out
}

// isPaid is true for a booking whose payment has landed ("Confirmed" only
// exists in older rows).
func isPaid(status string) bool { return status == statusBooked || status == statusConfirmed }

func failBookingFilter(reference string) bson.M {
	return bson.M{"transaction.reference": reference}
}

func failBookingUpdate() bson.M {
	return bson.M{"$set": bson.M{
		"transaction.paymentStatus":    "Failed",
		"bookingDetails.bookingStatus": "Failed",
	}}
}

type mongoBookingStore struct{ c *mongo.Collection }

func (s *mongoBookingStore) Occupancy(ctx context.Context, date string) ([]schedule.Occupancy, error) {
	cursor, err := s.c.Find(ctx, occupancyFilter(date),
		options.Find().SetProjection(bson.M{"bookingDetails.time": 1, "bookingDetails.pitchType": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var existing []models.Booking
	if err := cursor.All(ctx, &existing); err != nil {
		return nil, err
	}
	return toOccupancy(existing, date), nil
}

func (s *mongoBookingStore) Insert(ctx context.Context, b models.Booking) error {
	_, err := s.c.InsertOne(ctx, b)
	return err
}

func (s *mongoBookingStore) MarkFailed(ctx context.Context, reference string) error {
	_, err := s.c.UpdateOne(ctx, failBookingFilter(reference), failBookingUpdate())
	return err
}

func (s *mongoBookingStore) memberMonths(ctx context.Context, phone, month string) (map[string]MemberMonth, error) {
	from, to, err := schedule.MonthBounds(month)
	if err != nil {
		return nil, err
	}
	cursor, err := s.c.Find(ctx, memberMonthFilter(phone, from, to), options.Find().SetProjection(memberMonthFields))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var found []models.Booking
	if err := cursor.All(ctx, &found); err != nil {
		return nil, err
	}
	return summariseMemberMonths(found), nil
}

func (s *mongoBookingStore) MemberMonth(ctx context.Context, phone, month string) (MemberMonth, error) {
	months, err := s.memberMonths(ctx, phone, month)
	return months[phone], err
}

func (s *mongoBookingStore) MemberMonths(ctx context.Context, month string) (map[string]MemberMonth, error) {
	return s.memberMonths(ctx, "", month)
}

// logMarkFailed is what the booking flow does when it cannot mark a booking
// failed: the cleanup worker removes unpaid bookings anyway, so it only logs.
func logMarkFailed(err error) {
	if err != nil {
		log.Printf("[DB_ERROR] mark booking failed: %v", err)
	}
}
