package handlers

import (
	"context"
	"errors"

	"turf-booking-backend/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// errMemberExists means the number is already on the member list.
var errMemberExists = errors.New("already a member")

// maxMembers bounds the list the admin page and the booking flow work with.
const maxMembers = 5000

// MemberStore is the list of members. It is an interface so the discount rules
// can be tested against an in-memory fake; the MongoDB implementation below is
// the real one.
type MemberStore interface {
	// Find returns the member with this (normalised) number, or nil, nil when
	// there is none.
	Find(ctx context.Context, phone string) (*models.Member, error)
	List(ctx context.Context) ([]models.Member, error)
	// Add puts a member on the list, or returns errMemberExists.
	Add(ctx context.Context, m models.Member) error
	// Remove takes a number off the list and reports whether it was on it.
	Remove(ctx context.Context, phone string) (bool, error)
}

func memberFilter(phone string) bson.M { return bson.M{"phoneNumber": phone} }

// memberInsertUpdate is applied as an upsert on the number, so it only ever
// writes a member that was not there.
func memberInsertUpdate(m models.Member) bson.M {
	return bson.M{"$setOnInsert": bson.M{
		"phoneNumber": m.PhoneNumber,
		"fullName":    m.FullName,
		"createdAt":   m.CreatedAt,
	}}
}

type mongoMemberStore struct{ c *mongo.Collection }

// NewMongoMemberStore keeps the member list in the given collection.
func NewMongoMemberStore(c *mongo.Collection) MemberStore { return &mongoMemberStore{c: c} }

func (s *mongoMemberStore) Find(ctx context.Context, phone string) (*models.Member, error) {
	var m models.Member
	err := s.c.FindOne(ctx, memberFilter(phone)).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *mongoMemberStore) List(ctx context.Context) ([]models.Member, error) {
	cursor, err := s.c.Find(ctx, bson.M{}, options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(maxMembers))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var members []models.Member
	if err := cursor.All(ctx, &members); err != nil {
		return nil, err
	}
	return members, nil
}

// Add inserts only if the number is not there yet, in one step, so two admins
// adding the same number cannot both succeed even before the unique index exists.
func (s *mongoMemberStore) Add(ctx context.Context, m models.Member) error {
	res, err := s.c.UpdateOne(ctx, memberFilter(m.PhoneNumber), memberInsertUpdate(m),
		options.Update().SetUpsert(true))
	if mongo.IsDuplicateKeyError(err) {
		return errMemberExists
	}
	if err != nil {
		return err
	}
	if res.UpsertedCount == 0 {
		return errMemberExists
	}
	return nil
}

func (s *mongoMemberStore) Remove(ctx context.Context, phone string) (bool, error) {
	res, err := s.c.DeleteOne(ctx, memberFilter(phone))
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}
