package db

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestCampIndexes(t *testing.T) {
	byName := map[string]bson.D{}
	unique := map[string]bool{}
	for _, m := range campIndexModels() {
		name := *m.Options.Name
		byName[name], _ = m.Keys.(bson.D)
		unique[name] = m.Options.Unique != nil && *m.Options.Unique
	}

	// A duplicate reference would let one payment settle two registrations.
	ref := byName["uniq_payment_reference"]
	if len(ref) != 1 || ref[0].Key != "payment.reference" || !unique["uniq_payment_reference"] {
		t.Errorf("payment.reference must be uniquely indexed, got %v (unique=%v)", ref, unique["uniq_payment_reference"])
	}

	// The duplicate-registration check queries by child key and parent phone.
	lookup := byName["child_phone"]
	if len(lookup) != 2 || lookup[0].Key != "child.key" || lookup[1].Key != "parent.phoneNumber" {
		t.Errorf("child_phone index = %v", lookup)
	}

	// The admin list and the cleanup worker filter on status and createdAt.
	list := byName["status_created"]
	if len(list) != 2 || list[0].Key != "status" || list[1].Key != "createdAt" {
		t.Errorf("status_created index = %v", list)
	}
}
