package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"turf-booking-backend/internal/schedule"
)

// ScheduleHandler publishes the booking calendar: which days can be booked, the
// opening hours of each kind of day (weekday, weekend, public holiday) and the
// sessions they offer. The booking page draws exactly this, so it can never
// offer a session the server would refuse.
type ScheduleHandler struct {
	now func() time.Time
}

func NewScheduleHandler() *ScheduleHandler { return &ScheduleHandler{now: time.Now} }

// Get is public: it holds no personal data and costs no database access.
func (h *ScheduleHandler) Get(w http.ResponseWriter, r *http.Request) {
	body, err := json.Marshal(schedule.NewSnapshot(h.now()))
	if err != nil {
		log.Printf("[ERROR] schedule snapshot: %v", err)
		http.Error(w, "Failed to load the schedule", http.StatusInternalServerError)
		return
	}
	// The answer changes at midnight and whenever a holiday is added, so it is
	// never served from a shared cache without asking this server first.
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, body)
}
