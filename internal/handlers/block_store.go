package handlers

import (
	"context"

	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// maxBlocks bounds the list the admin page and the public calendar work with.
const maxBlocks = 5000

// BlockStore is the list of sessions the admin has blocked. It is an interface so
// the blocking rules can be tested against an in-memory fake; the MongoDB
// implementation below is the real one.
type BlockStore interface {
	// OnDate is every block on an operating day.
	OnDate(ctx context.Context, date string) ([]models.Block, error)
	// From is every block on or after a date, in date and time order.
	From(ctx context.Context, date string) ([]models.Block, error)
	Insert(ctx context.Context, b models.Block) error
	// Remove lifts one block and reports whether it was there.
	Remove(ctx context.Context, id primitive.ObjectID) (bool, error)
	// RemoveSeries lifts the blocks of a series that fall on or after a date and
	// says how many there were. Earlier ones are history and stay.
	RemoveSeries(ctx context.Context, seriesID, fromDate string) (int, error)
}

// ---- MongoDB queries --------------------------------------------------------
// Plain functions that only build filters, so tests can check them against the
// BSON field names without a database.

func blockDateFilter(date string) bson.M { return bson.M{"date": date} }

func blockFromFilter(date string) bson.M { return bson.M{"date": bson.M{"$gte": date}} }

func blockIDFilter(id primitive.ObjectID) bson.M { return bson.M{"_id": id} }

func blockSeriesFilter(seriesID, fromDate string) bson.M {
	return bson.M{"seriesId": seriesID, "date": bson.M{"$gte": fromDate}}
}

type mongoBlockStore struct{ c *mongo.Collection }

// NewMongoBlockStore keeps the blocks in the given collection.
func NewMongoBlockStore(c *mongo.Collection) BlockStore { return &mongoBlockStore{c: c} }

func (s *mongoBlockStore) find(ctx context.Context, filter bson.M) ([]models.Block, error) {
	cursor, err := s.c.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "date", Value: 1}, {Key: "time", Value: 1}}).
		SetLimit(maxBlocks))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var blocks []models.Block
	if err := cursor.All(ctx, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func (s *mongoBlockStore) OnDate(ctx context.Context, date string) ([]models.Block, error) {
	return s.find(ctx, blockDateFilter(date))
}

func (s *mongoBlockStore) From(ctx context.Context, date string) ([]models.Block, error) {
	return s.find(ctx, blockFromFilter(date))
}

func (s *mongoBlockStore) Insert(ctx context.Context, b models.Block) error {
	_, err := s.c.InsertOne(ctx, b)
	return err
}

func (s *mongoBlockStore) Remove(ctx context.Context, id primitive.ObjectID) (bool, error) {
	res, err := s.c.DeleteOne(ctx, blockIDFilter(id))
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func (s *mongoBlockStore) RemoveSeries(ctx context.Context, seriesID, fromDate string) (int, error) {
	res, err := s.c.DeleteMany(ctx, blockSeriesFilter(seriesID, fromDate))
	if err != nil {
		return 0, err
	}
	return int(res.DeletedCount), nil
}

// blockOccupancy is what blocks take up on the pitch. A block that cannot be
// read (bad data) takes the whole day, as an unreadable booking does.
func blockOccupancy(blocks []models.Block, date string) []schedule.Occupancy {
	as := make([]models.Booking, 0, len(blocks))
	for _, b := range blocks {
		as = append(as, models.Booking{BookingDetails: models.BookingDetails{Time: b.Time, PitchType: b.PitchType}})
	}
	return toOccupancy(as, date)
}
