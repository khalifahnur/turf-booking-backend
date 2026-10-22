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
	// MarkPaid moves a registration to Paid. It returns nil, nil when nothing
	// was settled: the reference is unknown, it was already paid (which makes a
	// repeated webhook harmless), or paidCents is positive but less than the
	// registration's price. A paidCents of 0 means the amount is unknown and is
	// accepted, since refusing a payment Paystack confirmed is the worse error.
	MarkPaid(ctx context.Context, reference string, paidCents int64, now time.Time) (*models.CampRegistration, error)
	// MarkFailed fails a registration that is still Pending and reports whether
	// it did; a paid one is never touched.
	MarkFailed(ctx context.Context, reference string, now time.Time) (bool, error)
	// Status returns the registration's status, or errUnknownReference.
	Status(ctx context.Context, reference string) (string, error)
	// ListForAdmin returns paid and pending registrations, newest first.
	ListForAdmin(ctx context.Context) ([]models.CampRegistration, error)
	// FlagDuplicates counts the paid registrations this child (name key and
	// parent phone) holds that share any of the given weeks. When there are two
	// or more it marks them all as possible duplicate payments for the admin to
	// look at, and returns the count.
	FlagDuplicates(ctx context.Context, childKey, phone string, weeks []string) (int, error)
}

// ---- MongoDB queries --------------------------------------------------------
// Kept as plain functions that only build filters, so tests can check them
// against the BSON field names without a database.

func conflictFilter(childKey, phone string, weeks []string, pendingAfter time.Time) bson.M {
	return bson.M{
		"child.key":          childKey,
		"parent.phoneNumber": phone,
		"weeks":              bson.M{"$in": weeks},
		"$or": bson.A{
			bson.M{"status": camp.StatusPaid},
			bson.M{"status": camp.StatusPending, "createdAt": bson.M{"$gte": pendingAfter}},
		},
	}
}

func markPaidFilter(reference string, paidCents int64) bson.M {
	f := bson.M{"payment.reference": reference, "status": bson.M{"$ne": camp.StatusPaid}}
	if paidCents > 0 {
		// Decided inside the same atomic update as the status change, so a payment
		// that is short of the price can never be settled by a race.
		f["$expr"] = bson.M{"$gte": bson.A{paidCents, bson.M{"$multiply": bson.A{"$amountKes", 100}}}}
	}
	return f
}

func markPaidUpdate(paidCents int64, now time.Time) bson.M {
	return bson.M{"$set": bson.M{
		"status":                  camp.StatusPaid,
		"payment.paidAmountCents": paidCents,
		"payment.paidAt":          now,
		"updatedAt":               now,
	}}
}

func markFailedFilter(reference string) bson.M {
	return bson.M{"payment.reference": reference, "status": camp.StatusPending}
}

func markFailedUpdate(now time.Time) bson.M {
	return bson.M{"$set": bson.M{"status": camp.StatusFailed, "updatedAt": now}}
}

func duplicateUpdate() bson.M {
	return bson.M{"$set": bson.M{"duplicate": true}}
}

func paidSharingWeeksFilter(childKey, phone string, weeks []string) bson.M {
	return bson.M{
		"child.key":          childKey,
		"parent.phoneNumber": phone,
		"status":             camp.StatusPaid,
		"weeks":              bson.M{"$in": weeks},
	}
}

func adminListFilter() bson.M {
	return bson.M{"status": bson.M{"$in": []string{camp.StatusPaid, camp.StatusPending}}}
}

type mongoCampStore struct{ c *mongo.Collection }

// NewMongoCampStore stores camp registrations in the given collection.
func NewMongoCampStore(c *mongo.Collection) CampStore { return &mongoCampStore{c: c} }

func (s *mongoCampStore) Conflicts(ctx context.Context, childKey, phone string, weeks []string, pendingAfter time.Time) ([]string, error) {
	cursor, err := s.c.Find(ctx, conflictFilter(childKey, phone, weeks, pendingAfter),
		options.Find().SetProjection(bson.M{"weeks": 1}))
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
		markPaidFilter(reference, paidCents),
		markPaidUpdate(paidCents, now),
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

func (s *mongoCampStore) MarkFailed(ctx context.Context, reference string, now time.Time) (bool, error) {
	res, err := s.c.UpdateOne(ctx,
		markFailedFilter(reference),
		markFailedUpdate(now),
	)
	if err != nil {
		return false, err
	}
	return res.ModifiedCount > 0, nil
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
	cursor, err := s.c.Find(ctx, adminListFilter(),
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}))
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

func (s *mongoCampStore) FlagDuplicates(ctx context.Context, childKey, phone string, weeks []string) (int, error) {
	filter := paidSharingWeeksFilter(childKey, phone, weeks)
	n, err := s.c.CountDocuments(ctx, filter)
	if err != nil || n < 2 {
		return int(n), err
	}
	_, err = s.c.UpdateMany(ctx, filter, duplicateUpdate())
	return int(n), err
}
