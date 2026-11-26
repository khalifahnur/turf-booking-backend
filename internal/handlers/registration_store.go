package handlers

import (
	"context"
	"errors"
	"time"

	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/registration"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// RegistrationStore is everything the tournament and academy handlers need from
// storage. It is an interface so the rules can be tested against an in-memory
// fake; the MongoDB implementation below is the real one.
type RegistrationStore interface {
	Insert(ctx context.Context, reg models.Registration) error
	// TournamentSlots counts, for each category, the tournament registrations that
	// hold a place: paid ones, and unpaid ones created after pendingAfter whose
	// prompt may still be answered. The registration `excluding` is left out, so a
	// payment can be checked against everyone else.
	TournamentSlots(ctx context.Context, pendingAfter time.Time, excluding primitive.ObjectID) (map[string]registration.Slots, error)
	// TeamHolds returns the wanted categories this team (by name key) already holds.
	TeamHolds(ctx context.Context, teamKey string, wanted []string, pendingAfter time.Time) ([]string, error)
	// PlayerHolds reports whether this player, registered from this number, is
	// already paid for or has a payment in progress.
	PlayerHolds(ctx context.Context, playerKey, phone string, pendingAfter time.Time) (bool, error)
	// MarkPaid moves a registration to Paid. It returns nil, nil when nothing was
	// settled: the reference is unknown, it was already paid (which makes a
	// repeated webhook harmless), or paidCents is positive but less than the fee.
	// A paidCents of 0 means the amount is unknown and is accepted, since refusing
	// a payment Paystack confirmed is the worse error.
	MarkPaid(ctx context.Context, reference string, paidCents int64, now time.Time) (*models.Registration, error)
	// MarkFailed fails a registration that is still Pending and reports whether it
	// did; a paid one is never touched.
	MarkFailed(ctx context.Context, reference string, now time.Time) (bool, error)
	// Status returns the registration's status, or errUnknownReference.
	Status(ctx context.Context, reference string) (string, error)
	// FlagOverflow marks a registration as paid into categories that were full.
	FlagOverflow(ctx context.Context, id primitive.ObjectID, categories []string) error
	// ListForAdmin returns the paid and pending registrations of a kind, newest first.
	ListForAdmin(ctx context.Context, kind string) ([]models.Registration, error)
}

// ---- MongoDB queries --------------------------------------------------------
// Plain functions that only build filters, so tests can check them against the
// BSON field names without a database.

// holdingClause matches a registration that holds its place: paid, or unpaid but
// created recently enough that its prompt may still be answered.
func holdingClause(pendingAfter time.Time) bson.A {
	return bson.A{
		bson.M{"status": registration.StatusPaid},
		bson.M{"status": registration.StatusPending, "createdAt": bson.M{"$gte": pendingAfter}},
	}
}

func tournamentSlotsFilter(pendingAfter time.Time, excluding primitive.ObjectID) bson.M {
	return bson.M{
		"kind": registration.KindTournament,
		"_id":  bson.M{"$ne": excluding},
		"$or":  holdingClause(pendingAfter),
	}
}

func teamHoldsFilter(teamKey string, wanted []string, pendingAfter time.Time) bson.M {
	return bson.M{
		"kind":       registration.KindTournament,
		"teamKey":    teamKey,
		"categories": bson.M{"$in": wanted},
		"$or":        holdingClause(pendingAfter),
	}
}

func playerHoldsFilter(playerKey, phone string, pendingAfter time.Time) bson.M {
	return bson.M{
		"kind":        registration.KindAcademy,
		"playerKey":   playerKey,
		"phoneNumber": phone,
		"$or":         holdingClause(pendingAfter),
	}
}

func registrationPaidFilter(reference string, paidCents int64) bson.M {
	f := bson.M{"payment.reference": reference, "status": bson.M{"$ne": registration.StatusPaid}}
	if paidCents > 0 {
		// Decided inside the same atomic update as the status change, so a payment
		// that is short of the fee can never be settled by a race.
		f["$expr"] = bson.M{"$gte": bson.A{paidCents, bson.M{"$multiply": bson.A{"$amountKes", 100}}}}
	}
	return f
}

func registrationPaidUpdate(paidCents int64, now time.Time) bson.M {
	return bson.M{"$set": bson.M{
		"status":                  registration.StatusPaid,
		"payment.paidAmountCents": paidCents,
		"payment.paidAt":          now,
		"updatedAt":               now,
	}}
}

func registrationFailedFilter(reference string) bson.M {
	return bson.M{"payment.reference": reference, "status": registration.StatusPending}
}

func registrationFailedUpdate(now time.Time) bson.M {
	return bson.M{"$set": bson.M{"status": registration.StatusFailed, "updatedAt": now}}
}

func registrationReferenceFilter(reference string) bson.M {
	return bson.M{"payment.reference": reference}
}

func registrationIDFilter(id primitive.ObjectID) bson.M { return bson.M{"_id": id} }

func registrationOverflowUpdate(categories []string) bson.M {
	return bson.M{"$set": bson.M{"overflow": categories}}
}

func registrationAdminFilter(kind string) bson.M {
	return bson.M{"kind": kind, "status": bson.M{"$in": []string{registration.StatusPaid, registration.StatusPending}}}
}

type mongoRegistrationStore struct{ c *mongo.Collection }

// NewMongoRegistrationStore keeps registrations in the given collection.
func NewMongoRegistrationStore(c *mongo.Collection) RegistrationStore {
	return &mongoRegistrationStore{c: c}
}

func (s *mongoRegistrationStore) Insert(ctx context.Context, reg models.Registration) error {
	_, err := s.c.InsertOne(ctx, reg)
	return err
}

func (s *mongoRegistrationStore) TournamentSlots(ctx context.Context, pendingAfter time.Time, excluding primitive.ObjectID) (map[string]registration.Slots, error) {
	cursor, err := s.c.Find(ctx, tournamentSlotsFilter(pendingAfter, excluding),
		options.Find().SetProjection(bson.M{"categories": 1, "status": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var found []models.Registration
	if err := cursor.All(ctx, &found); err != nil {
		return nil, err
	}
	return countSlots(found), nil
}

// countSlots adds up, for each category, the paid and the held registrations.
func countSlots(regs []models.Registration) map[string]registration.Slots {
	out := map[string]registration.Slots{}
	for _, r := range regs {
		for _, id := range r.Categories {
			s := out[id]
			if r.Status == registration.StatusPaid {
				s.Paid++
			} else {
				s.Held++
			}
			out[id] = s
		}
	}
	return out
}

func (s *mongoRegistrationStore) TeamHolds(ctx context.Context, teamKey string, wanted []string, pendingAfter time.Time) ([]string, error) {
	cursor, err := s.c.Find(ctx, teamHoldsFilter(teamKey, wanted, pendingAfter),
		options.Find().SetProjection(bson.M{"categories": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var found []models.Registration
	if err := cursor.All(ctx, &found); err != nil {
		return nil, err
	}
	return held(wanted, found), nil
}

// held returns the wanted categories that any of the registrations has, in the
// order they were wanted.
func held(wanted []string, regs []models.Registration) []string {
	has := map[string]struct{}{}
	for _, r := range regs {
		for _, id := range r.Categories {
			has[id] = struct{}{}
		}
	}
	var out []string
	for _, id := range wanted {
		if _, ok := has[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func (s *mongoRegistrationStore) PlayerHolds(ctx context.Context, playerKey, phone string, pendingAfter time.Time) (bool, error) {
	n, err := s.c.CountDocuments(ctx, playerHoldsFilter(playerKey, phone, pendingAfter), options.Count().SetLimit(1))
	return n > 0, err
}

func (s *mongoRegistrationStore) MarkPaid(ctx context.Context, reference string, paidCents int64, now time.Time) (*models.Registration, error) {
	var reg models.Registration
	err := s.c.FindOneAndUpdate(ctx,
		registrationPaidFilter(reference, paidCents),
		registrationPaidUpdate(paidCents, now),
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

func (s *mongoRegistrationStore) MarkFailed(ctx context.Context, reference string, now time.Time) (bool, error) {
	res, err := s.c.UpdateOne(ctx, registrationFailedFilter(reference), registrationFailedUpdate(now))
	if err != nil {
		return false, err
	}
	return res.ModifiedCount > 0, nil
}

func (s *mongoRegistrationStore) Status(ctx context.Context, reference string) (string, error) {
	var reg models.Registration
	err := s.c.FindOne(ctx, registrationReferenceFilter(reference),
		options.FindOne().SetProjection(bson.M{"status": 1})).Decode(&reg)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errUnknownReference
	}
	if err != nil {
		return "", err
	}
	return reg.Status, nil
}

func (s *mongoRegistrationStore) FlagOverflow(ctx context.Context, id primitive.ObjectID, categories []string) error {
	_, err := s.c.UpdateOne(ctx, registrationIDFilter(id), registrationOverflowUpdate(categories))
	return err
}

func (s *mongoRegistrationStore) ListForAdmin(ctx context.Context, kind string) ([]models.Registration, error) {
	cursor, err := s.c.Find(ctx, registrationAdminFilter(kind),
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var regs []models.Registration
	if err := cursor.All(ctx, &regs); err != nil {
		return nil, err
	}
	return regs, nil
}
