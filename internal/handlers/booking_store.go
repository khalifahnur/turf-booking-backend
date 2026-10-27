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
	// MemberSessions counts the member-priced sessions a number holds in a month
	// ("2026-11"): paid ones and ones still waiting for their payment.
	MemberSessions(ctx context.Context, phone, month string) (int, error)
	// MemberSessionsByPhone is MemberSessions for every number at once.
	MemberSessionsByPhone(ctx context.Context, month string) (map[string]int, error)
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

// memberSessionsFilter matches the member-priced sessions that hold a place in a
// month, optionally for one number. A session counts towards the month of its
// date, whenever it was booked.
func memberSessionsFilter(phone, from, to string) bson.M {
	f := bson.M{
		"transaction.discount":         models.DiscountMember,
		"bookingDetails.date":          bson.M{"$gte": from, "$lt": to},
		"bookingDetails.bookingStatus": bson.M{"$in": occupyingStatuses},
	}
	if phone != "" {
		f["team.phoneNumber"] = phone
	}
	return f
}

// memberSessionsByPhonePipeline counts the same sessions for every number.
func memberSessionsByPhonePipeline(from, to string) mongo.Pipeline {
	return mongo.Pipeline{
		{{Key: "$match", Value: memberSessionsFilter("", from, to)}},
		{{Key: "$group", Value: bson.M{"_id": "$team.phoneNumber", "n": bson.M{"$sum": 1}}}},
	}
}

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

func (s *mongoBookingStore) MemberSessions(ctx context.Context, phone, month string) (int, error) {
	from, to, err := schedule.MonthBounds(month)
	if err != nil {
		return 0, err
	}
	n, err := s.c.CountDocuments(ctx, memberSessionsFilter(phone, from, to))
	return int(n), err
}

func (s *mongoBookingStore) MemberSessionsByPhone(ctx context.Context, month string) (map[string]int, error) {
	from, to, err := schedule.MonthBounds(month)
	if err != nil {
		return nil, err
	}
	cursor, err := s.c.Aggregate(ctx, memberSessionsByPhonePipeline(from, to))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var rows []struct {
		Phone string `bson:"_id"`
		N     int    `bson:"n"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Phone] = r.N
	}
	return out, nil
}

// logMarkFailed is what the booking flow does when it cannot mark a booking
// failed: the cleanup worker removes unpaid bookings anyway, so it only logs.
func logMarkFailed(err error) {
	if err != nil {
		log.Printf("[DB_ERROR] mark booking failed: %v", err)
	}
}
