package services

import (
	"context"
	"log"
	"time"

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
