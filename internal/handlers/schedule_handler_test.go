package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"turf-booking-backend/internal/schedule"
)

func TestScheduleEndpoint(t *testing.T) {
	// Friday 2026-12-25 12:00 in Nairobi: Christmas Day.
	fixed := time.Date(2026, 12, 25, 12, 0, 0, 0, schedule.Location())
	h := &ScheduleHandler{now: func() time.Time { return fixed }}

	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/schedule", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q: the answer changes at midnight and must be revalidated", cc)
	}

	var got struct {
		Timezone string `json:"timezone"`
		Today    string `json:"today"`
		Kinds    map[string]struct {
			Windows []struct {
				Open  string `json:"open"`
				Close string `json:"close"`
			} `json:"windows"`
			Sessions []struct {
				Minutes int    `json:"minutes"`
				Label   string `json:"label"`
				From    int    `json:"from"`
				To      int    `json:"to"`
			} `json:"sessions"`
		} `json:"kinds"`
		Days []struct {
			Date    string `json:"date"`
			Kind    string `json:"kind"`
			Holiday string `json:"holiday"`
		} `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Timezone != "Africa/Nairobi" || got.Today != "2026-12-25" || len(got.Days) != 30 {
		t.Errorf("timezone %q, today %q, %d days", got.Timezone, got.Today, len(got.Days))
	}
	if d := got.Days[0]; d.Date != "2026-12-25" || d.Kind != "holiday" || d.Holiday != "Christmas Day" {
		t.Errorf("first day = %+v", d)
	}
	if d := got.Days[1]; d.Date != "2026-12-26" || d.Kind != "holiday" || d.Holiday != "Boxing Day" {
		t.Errorf("second day = %+v", d)
	}
	// Monday 28 December is an ordinary weekday.
	if d := got.Days[3]; d.Date != "2026-12-28" || d.Kind != "weekday" || d.Holiday != "" {
		t.Errorf("fourth day = %+v", d)
	}

	weekday := got.Kinds["weekday"]
	if len(weekday.Windows) != 2 || weekday.Windows[0].Open != "07:00" || weekday.Windows[0].Close != "09:00" ||
		weekday.Windows[1].Open != "15:00" || weekday.Windows[1].Close != "01:00" {
		t.Errorf("weekday windows = %+v", weekday.Windows)
	}
	if len(got.Kinds["weekend"].Windows) != 1 || got.Kinds["weekend"].Windows[0].Open != "07:00" || got.Kinds["weekend"].Windows[0].Close != "01:00" {
		t.Errorf("weekend windows = %+v", got.Kinds["weekend"].Windows)
	}
	if len(got.Kinds["holiday"].Sessions) != len(got.Kinds["weekend"].Sessions) || len(got.Kinds["holiday"].Sessions) != 27 {
		t.Errorf("a holiday offers %d sessions, a weekend %d (want 27: 18 hourly and 9 two-hour)",
			len(got.Kinds["holiday"].Sessions), len(got.Kinds["weekend"].Sessions))
	}
	last := weekday.Sessions[len(weekday.Sessions)-1]
	if len(weekday.Sessions) != 18 || last.Label != "23:00 – 01:00" || last.Minutes != 120 || last.From != 23*60 || last.To != 25*60 {
		t.Errorf("weekday sessions: %d, last = %+v", len(weekday.Sessions), last)
	}
}

func TestScheduleEndpointIsPublicAndCostsNoDatabaseAccess(t *testing.T) {
	// No collection, no config: the handler needs neither, so anyone may call it.
	rec := httptest.NewRecorder()
	NewScheduleHandler().Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/schedule", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("status %d, %d bytes", rec.Code, rec.Body.Len())
	}
}
