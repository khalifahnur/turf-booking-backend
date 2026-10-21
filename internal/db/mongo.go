package db

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func Connect(uri string) *mongo.Client {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clientOptions := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}

	err = client.Ping(ctx, nil)
	if err != nil {
		log.Fatalf("Failed to ping MongoDB: %v", err)
	}

	log.Println("Successfully connected to MongoDB")
	return client
}

// EnsureIndexes makes the webhook's lookup by payment reference O(1), stops two
// bookings from ever sharing a reference, and speeds up the per-date capacity
// check. A failure is logged, not fatal: the service works without them, just
// slower, and an old duplicate row must not stop a deploy.
func EnsureIndexes(collection *mongo.Collection) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := collection.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "transaction.reference", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("uniq_transaction_reference"),
		},
		{
			Keys: bson.D{
				{Key: "bookingDetails.date", Value: 1},
				{Key: "bookingDetails.bookingStatus", Value: 1},
			},
			Options: options.Index().SetName("date_status"),
		},
	})
	if err != nil {
		log.Printf("[DB] could not ensure indexes: %v", err)
	}
}

// EnsureCampIndexes keeps payment references unique and the duplicate-
// registration lookup and admin list fast. As with the booking indexes, a
// failure is logged rather than fatal.
func EnsureCampIndexes(collection *mongo.Collection) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := collection.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "payment.reference", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("uniq_payment_reference"),
		},
		{
			Keys: bson.D{
				{Key: "child.key", Value: 1},
				{Key: "parent.phoneNumber", Value: 1},
			},
			Options: options.Index().SetName("child_phone"),
		},
		{
			Keys: bson.D{
				{Key: "status", Value: 1},
				{Key: "createdAt", Value: -1},
			},
			Options: options.Index().SetName("status_created"),
		},
	})
	if err != nil {
		log.Printf("[DB] could not ensure camp indexes: %v", err)
	}
}
