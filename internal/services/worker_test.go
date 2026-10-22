package services

import (
	"reflect"
	"testing"
	"time"

	"turf-booking-backend/internal/camp"

	"go.mongodb.org/mongo-driver/bson"
)

var workerNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// A prompt is answerable for about a minute, but a payment that is late must
// still be able to settle its registration: only long-abandoned ones expire.
func TestCampExpireFilter(t *testing.T) {
	f := campExpireFilter(workerNow)
	if f["status"] != camp.StatusPending {
		t.Errorf("only Pending registrations expire, got %v", f["status"])
	}
	want := bson.M{"$lt": workerNow.Add(-30 * time.Minute)}
	if !reflect.DeepEqual(f["createdAt"], want) {
		t.Errorf("createdAt = %v, want %v", f["createdAt"], want)
	}
	set := campExpireUpdate(workerNow)["$set"].(bson.M)
	if set["status"] != camp.StatusExpired || set["updatedAt"] != workerNow {
		t.Errorf("expire update = %v", set)
	}
}

// The purge must never reach a Paid or Pending registration, and keeps the dead
// ones for a week from when they went dead, not from when they were created.
func TestCampPurgeFilter(t *testing.T) {
	f := campPurgeFilter(workerNow)
	if !reflect.DeepEqual(f["status"], bson.M{"$in": []string{camp.StatusExpired, camp.StatusFailed}}) {
		t.Errorf("purge status = %v; paid and pending registrations must never be deleted", f["status"])
	}
	if !reflect.DeepEqual(f["updatedAt"], bson.M{"$lt": workerNow.Add(-7 * 24 * time.Hour)}) {
		t.Errorf("purge cut-off = %v", f["updatedAt"])
	}
}
