package services

import (
	"context"
	"log"
	"time"

	"turf-booking-backend/internal/camp"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func StartCleanupWorker(collection *mongo.Collection) {
	ticker := time.NewTicker(2 * time.Minute)

	go func() {
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

			fiveMinsAgo := time.Now().Add(-5 * time.Minute)
			filter := bson.M{
				"bookingDetails.bookingStatus": bson.M{"$in": []string{"Pending", "Failed"}},
				"bookingDetails.createdAt":     bson.M{"$lt": fiveMinsAgo},
			}

			res, err := collection.DeleteMany(ctx, filter)
			if err != nil {
				log.Printf("Cleanup worker error: %v", err)
			} else if res.DeletedCount > 0 {
				log.Printf("Cleaned up %d abandoned pending booking(s)", res.DeletedCount)
			}
			cancel()
		}
	}()
}

// StartCampCleanupWorker tidies camp registrations whose payment never
// finished. Unlike pitch bookings they are not deleted straight away: an
// unanswered prompt is marked Expired and kept for a week, so a payment that
// arrives late (a parent who enters their PIN after the prompt "timed out")
// can still complete the registration instead of taking their money for
// nothing. Only after that are the dead records, and the personal details on
// them, removed.
const (
	// campExpireAfter is far longer than a prompt stays answerable, so only
	// payments that were truly abandoned are expired.
	campExpireAfter = 30 * time.Minute
	// campPurgeAfter is how long an expired or failed registration is kept, for a
	// late payment to settle it.
	campPurgeAfter = 7 * 24 * time.Hour
)

func campExpireFilter(now time.Time) bson.M {
	return bson.M{"status": camp.StatusPending, "createdAt": bson.M{"$lt": now.Add(-campExpireAfter)}}
}

func campExpireUpdate(now time.Time) bson.M {
	return bson.M{"$set": bson.M{"status": camp.StatusExpired, "updatedAt": now}}
}

func campPurgeFilter(now time.Time) bson.M {
	return bson.M{
		"status":    bson.M{"$in": []string{camp.StatusExpired, camp.StatusFailed}},
		"updatedAt": bson.M{"$lt": now.Add(-campPurgeAfter)},
	}
}

func StartCampCleanupWorker(collection *mongo.Collection) {
	ticker := time.NewTicker(5 * time.Minute)

	go func() {
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			now := time.Now()

			expired, err := collection.UpdateMany(ctx, campExpireFilter(now), campExpireUpdate(now))
			if err != nil {
				log.Printf("Camp cleanup (expire) error: %v", err)
			} else if expired.ModifiedCount > 0 {
				log.Printf("Expired %d unpaid camp registration(s)", expired.ModifiedCount)
			}

			purged, err := collection.DeleteMany(ctx, campPurgeFilter(now))
			if err != nil {
				log.Printf("Camp cleanup (purge) error: %v", err)
			} else if purged.DeletedCount > 0 {
				log.Printf("Removed %d abandoned camp registration(s)", purged.DeletedCount)
			}
			cancel()
		}
	}()
}
