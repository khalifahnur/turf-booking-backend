package handlers

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"turf-booking-backend/internal/camp"
	"turf-booking-backend/internal/models"

	"go.mongodb.org/mongo-driver/bson"
)

// The Mongo queries are strings, so a typo in one silently matches nothing and
// no unit test against the in-memory fake would notice. These check every field
// name a query or update mentions against the bson tags on the model.

func bsonPaths(t reflect.Type, prefix string, into map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("bson"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		path := prefix + tag
		into[path] = true
		ft := f.Type
		if ft.Kind() == reflect.Struct && ft.PkgPath() != "time" && ft.PkgPath() != "go.mongodb.org/mongo-driver/bson/primitive" {
			bsonPaths(ft, path+".", into)
		}
	}
}

func modelPaths() map[string]bool {
	paths := map[string]bool{}
	bsonPaths(reflect.TypeOf(models.CampRegistration{}), "", paths)
	return paths
}

// fieldRefs lists every document field a query mentions: its keys that are not
// operators, plus "$field" references inside expressions.
func fieldRefs(v any, out *[]string) {
	switch x := v.(type) {
	case bson.M:
		for k, val := range x {
			if !strings.HasPrefix(k, "$") {
				*out = append(*out, k)
			}
			fieldRefs(val, out)
		}
	case bson.A:
		for _, val := range x {
			fieldRefs(val, out)
		}
	case string:
		if strings.HasPrefix(x, "$") && !strings.HasPrefix(x, "$$") {
			*out = append(*out, strings.TrimPrefix(x, "$"))
		}
	}
}

func TestCampQueriesUseRealFieldNames(t *testing.T) {
	paths := modelPaths()
	now := time.Now()
	queries := map[string]any{
		"conflictFilter":         conflictFilter("k", "254712345678", []string{"2026-11-02"}, now),
		"markPaidFilter":         markPaidFilter("camp-x", 250_000),
		"markPaidFilter(any)":    markPaidFilter("camp-x", 0),
		"markPaidUpdate":         markPaidUpdate(250_000, now),
		"markFailedFilter":       markFailedFilter("camp-x"),
		"markFailedUpdate":       markFailedUpdate(now),
		"paidSharingWeeksFilter": paidSharingWeeksFilter("k", "254712345678", []string{"2026-11-02"}),
		"duplicateUpdate":        duplicateUpdate(),
		"adminListFilter":        adminListFilter(),
	}
	for name, q := range queries {
		var refs []string
		fieldRefs(q, &refs)
		if len(refs) == 0 {
			t.Errorf("%s mentions no fields; the walker is broken", name)
		}
		for _, ref := range refs {
			if !paths[ref] {
				t.Errorf("%s uses field %q, which is not a bson field of CampRegistration", name, ref)
			}
		}
	}
}

func TestCampStoreQueriesCarryTheBusinessRules(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	// Settling must be conditional on the amount, inside the same atomic update.
	if _, ok := markPaidFilter("camp-x", 0)["$expr"]; ok {
		t.Error("an unknown amount (0) must not be compared against the price")
	}
	expr, ok := markPaidFilter("camp-x", 250_000)["$expr"].(bson.M)
	if !ok {
		t.Fatal("a known amount must be compared against the price in the filter")
	}
	gte, _ := expr["$gte"].(bson.A)
	if len(gte) != 2 || gte[0] != int64(250_000) {
		t.Fatalf("$expr = %v, want paid cents $gte price", expr)
	}
	if mul, _ := gte[1].(bson.M)["$multiply"].(bson.A); len(mul) != 2 || mul[0] != "$amountKes" || mul[1] != 100 {
		t.Errorf("price side of the comparison = %v, want $amountKes * 100", gte[1])
	}
	if st := markPaidFilter("camp-x", 1)["status"]; !reflect.DeepEqual(st, bson.M{"$ne": camp.StatusPaid}) {
		t.Errorf("markPaid must skip registrations already paid, got %v", st)
	}

	// Only a Pending registration can be failed: never a Paid one.
	if markFailedFilter("camp-x")["status"] != camp.StatusPending {
		t.Error("MarkFailed must only match Pending registrations")
	}

	// Only Paid, and still-answerable Pending, registrations block a retry.
	c := conflictFilter("k", "p", []string{"w"}, now)
	or, _ := c["$or"].(bson.A)
	if len(or) != 2 {
		t.Fatalf("conflict $or = %v", c["$or"])
	}
	if or[0].(bson.M)["status"] != camp.StatusPaid {
		t.Error("a paid registration must block a repeat")
	}
	pending := or[1].(bson.M)
	if pending["status"] != camp.StatusPending || !reflect.DeepEqual(pending["createdAt"], bson.M{"$gte": now}) {
		t.Errorf("pending branch = %v, want Pending created at or after the cut-off", pending)
	}

	// The admin sees paid and pending only.
	if got := adminListFilter()["status"]; !reflect.DeepEqual(got, bson.M{"$in": []string{camp.StatusPaid, camp.StatusPending}}) {
		t.Errorf("admin filter = %v", got)
	}
	if paidSharingWeeksFilter("k", "p", []string{"w"})["status"] != camp.StatusPaid {
		t.Error("duplicate payments are only counted among paid registrations")
	}
}

func TestOverlapReturnsSortedRequestedWeeksOnly(t *testing.T) {
	regs := []models.CampRegistration{
		{Weeks: []string{"2026-11-16", "2026-11-02"}},
		{Weeks: []string{"2026-11-23"}},
	}
	got := overlap([]string{"2026-11-02", "2026-11-09", "2026-11-16"}, regs)
	if strings.Join(got, ",") != "2026-11-02,2026-11-16" {
		t.Errorf("overlap = %v", got)
	}
	if got := overlap([]string{"2026-11-09"}, regs); len(got) != 0 {
		t.Errorf("no overlap should be empty, got %v", got)
	}
}

// Names alone would let a typo'd value through: an update that wrote the wrong
// status, or a filter that forgot one of its keys, would still "use real fields".
func TestCampUpdatesWriteTheRightValues(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	set := func(u bson.M) bson.M { return u["$set"].(bson.M) }

	paid := set(markPaidUpdate(250_000, now))
	if paid["status"] != camp.StatusPaid || paid["payment.paidAmountCents"] != int64(250_000) || paid["payment.paidAt"] != now || paid["updatedAt"] != now {
		t.Errorf("markPaidUpdate = %v", paid)
	}
	failed := set(markFailedUpdate(now))
	if failed["status"] != camp.StatusFailed || failed["updatedAt"] != now || len(failed) != 2 {
		t.Errorf("markFailedUpdate = %v", failed)
	}
	if dup := set(duplicateUpdate()); dup["duplicate"] != true || len(dup) != 1 {
		t.Errorf("duplicateUpdate = %v", dup)
	}
}

func TestCampFiltersKeepEveryKey(t *testing.T) {
	weeks := []string{"2026-11-02"}
	keys := func(f bson.M) []string {
		var out []string
		for k := range f {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	// The key set is the rule: dropping child.key, the phone or the weeks widens
	// the match to other families' children.
	for name, f := range map[string]bson.M{
		"conflictFilter":         conflictFilter("k", "p", weeks, time.Now()),
		"paidSharingWeeksFilter": paidSharingWeeksFilter("k", "p", weeks),
	} {
		if got := strings.Join(keys(f), ","); !strings.Contains(got, "child.key") || !strings.Contains(got, "parent.phoneNumber") || !strings.Contains(got, "weeks") {
			t.Errorf("%s keys = %s", name, got)
		}
		if f["child.key"] != "k" || f["parent.phoneNumber"] != "p" || !reflect.DeepEqual(f["weeks"], bson.M{"$in": weeks}) {
			t.Errorf("%s does not match on the child, the phone and the weeks: %v", name, f)
		}
	}
	if got := strings.Join(keys(markPaidFilter("r", 1)), ","); got != "$expr,payment.reference,status" {
		t.Errorf("markPaidFilter keys = %s", got)
	}
	if f := markPaidFilter("camp-r", 1); f["payment.reference"] != "camp-r" {
		t.Errorf("markPaid must match on the reference, got %v", f)
	}
	if f := markFailedFilter("camp-r"); f["payment.reference"] != "camp-r" || len(f) != 2 {
		t.Errorf("markFailedFilter = %v", f)
	}
}
