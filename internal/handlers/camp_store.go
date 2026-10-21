package handlers

import (
	"context"
	"errors"
	"sort"
	"time"

	"turf-booking-backend/internal/camp"
	"turf-booking-backend/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// errUnknownReference means no booking or registration has this payment
// reference.
var errUnknownReference = errors.New("unknown payment reference")

// CampStore is everything the camp handlers need from storage. It is an
// interface so the business rules can be tested against an in-memory fake; the
// MongoDB implementation below is the real one.
type CampStore interface {
	// Conflicts returns the requested weeks this child (identified by name key
	// and parent phone) already holds: weeks that are paid for, or whose payment
	// prompt was sent after pendingAfter and may still be answered.
	Conflicts(ctx context.Context, childKey, phone string, weeks []string, pendingAfter time.Time) ([]string, error)
	Insert(ctx context.Context, reg models.CampRegistration) error
	// MarkPaid moves a registration to Paid. It returns nil, nil when the
	// reference is unknown or was already paid, which makes a repeated webhook
	// harmless.
	MarkPaid(ctx context.Context, reference string, paidCents int64, now time.Time) (*models.CampRegistration, error)
	// MarkFailed fails a registration that is still Pending; a paid one is never
	// touched.
	MarkFailed(ctx context.Context, reference string, now time.Time) error
	// Status returns the registration's status, or errUnknownReference.
	Status(ctx context.Context, reference string) (string, error)
	// ListForAdmin returns paid and pending registrations, newest first.
	ListForAdmin(ctx context.Context) ([]models.CampRegistration, error)
}

type mongoCampStore struct{ c *mongo.Collection }

// NewMongoCampStore stores camp registrations in the given collection.
func NewMongoCampStore(c *mongo.Collection) CampStore { return &mongoCampStore{c: c} }

func (s *mongoCampStore) Conflicts(ctx context.Context, childKey, phone string, weeks []string, pendingAfter time.Time) ([]string, error) {
	cursor, err := s.c.Find(ctx, bson.M{
		"child.key":          childKey,
		"parent.phoneNumber": phone,
		"weeks":              bson.M{"$in": weeks},
		"$or": bson.A{
			bson.M{"status": camp.StatusPaid},
			bson.M{"status": camp.StatusPending, "createdAt": bson.M{"$gte": pendingAfter}},
		},
	}, options.Find().SetProjection(bson.M{"weeks": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var found []models.CampRegistration
	if err := cursor.All(ctx, &found); err != nil {
		return nil, err
	}
	return overlap(weeks, found), nil
}

// overlap returns the requested weeks that appear in any of the registrations.
func overlap(requested []string, regs []models.CampRegistration) []string {
	taken := make(map[string]struct{})
	for _, r := range regs {
		for _, w := range r.Weeks {
			taken[w] = struct{}{}
		}
	}
	var out []string
	for _, w := range requested {
		if _, ok := taken[w]; ok {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

func (s *mongoCampStore) Insert(ctx context.Context, reg models.CampRegistration) error {
	_, err := s.c.InsertOne(ctx, reg)
	return err
}

func (s *mongoCampStore) MarkPaid(ctx context.Context, reference string, paidCents int64, now time.Time) (*models.CampRegistration, error) {
	var reg models.CampRegistration
	err := s.c.FindOneAndUpdate(ctx,
		bson.M{"payment.reference": reference, "status": bson.M{"$ne": camp.StatusPaid}},
		bson.M{"$set": bson.M{
			"status":                  camp.StatusPaid,
			"payment.paidAmountCents": paidCents,
			"payment.paidAt":          now,
			"updatedAt":               now,
		}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&reg)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &reg, nil
}

func (s *mongoCampStore) MarkFailed(ctx context.Context, reference string, now time.Time) error {
	_, err := s.c.UpdateOne(ctx,
		bson.M{"payment.reference": reference, "status": camp.StatusPending},
		bson.M{"$set": bson.M{"status": camp.StatusFailed, "updatedAt": now}},
	)
	return err
}

func (s *mongoCampStore) Status(ctx context.Context, reference string) (string, error) {
	var reg models.CampRegistration
	err := s.c.FindOne(ctx, bson.M{"payment.reference": reference},
		options.FindOne().SetProjection(bson.M{"status": 1})).Decode(&reg)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errUnknownReference
	}
	if err != nil {
		return "", err
	}
	return reg.Status, nil
}

func (s *mongoCampStore) ListForAdmin(ctx context.Context) ([]models.CampRegistration, error) {
	cursor, err := s.c.Find(ctx,
		bson.M{"status": bson.M{"$in": []string{camp.StatusPaid, camp.StatusPending}}},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var regs []models.CampRegistration
	if err := cursor.All(ctx, &regs); err != nil {
		return nil, err
	}
	return regs, nil
}
