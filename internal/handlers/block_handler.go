package handlers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"
	"turf-booking-backend/internal/validate"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// maxBlockDates is the most dates one request may block: a team playing four
// times a week for twelve weeks is 48.
const maxBlockDates = 100

// AttachBlocks turns blocking on: sessions on this list are off the booking page.
func (h *BookingHandler) AttachBlocks(b BlockStore) { h.blocks = b }

// blockView is a block as the admin dashboard sees it.
type blockView struct {
	ID        string    `json:"id"`
	Date      string    `json:"date"`
	Time      string    `json:"time"`
	PitchType string    `json:"pitchType"`
	Reason    string    `json:"reason"`
	SeriesID  string    `json:"seriesId"`
	CreatedAt time.Time `json:"createdAt"`
}

func blockViewOf(b models.Block) blockView {
	return blockView{
		ID: b.ID.Hex(), Date: b.Date, Time: b.Time, PitchType: b.PitchType,
		Reason: b.Reason, SeriesID: b.SeriesID, CreatedAt: b.CreatedAt,
	}
}

// sortBlocks puts blocks in the order the venue runs them: by day, then by when
// the session starts on that day's clock, so "00:00 – 01:00" comes after the
// evening it belongs to rather than before it.
func sortBlocks(blocks []models.Block) {
	start := func(b models.Block) int {
		from, _, ok := schedule.ParseStoredRange(b.Time)
		if !ok {
			return 0
		}
		return from
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].Date != blocks[j].Date {
			return blocks[i].Date < blocks[j].Date
		}
		return start(blocks[i]) < start(blocks[j])
	})
}

// ListBlocks returns the sessions that are blocked from today on. It must be
// wrapped in AdminOnly: it exposes the admin's notes.
func (h *BookingHandler) ListBlocks(w http.ResponseWriter, r *http.Request) {
	if h.blocks == nil {
		http.Error(w, "Blocking is not enabled", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	blocks, err := h.blocks.From(ctx, schedule.OperatingDate(h.now()))
	if err != nil {
		log.Printf("[DB_ERROR] list blocks: %v", err)
		http.Error(w, "Failed to load the blocked sessions", http.StatusInternalServerError)
		return
	}
	sortBlocks(blocks)
	views := make([]blockView, 0, len(blocks))
	for _, b := range blocks {
		views = append(views, blockViewOf(b))
	}
	body, err := json.Marshal(map[string]any{"blocks": views})
	if err != nil {
		http.Error(w, "Failed to load the blocked sessions", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// skippedBlock is a date that could not be blocked, and why, in words for the admin.
type skippedBlock struct {
	Date   string `json:"date"`
	Reason string `json:"reason"`
}

// CreateBlocks blocks one session on each of the given dates: one date for a
// customer who wants to pay first, many for a team that plays the same time every
// week. Each date is checked on its own, against the bookings and blocks already
// on the pitch, so a date that is taken is reported and skipped while the rest
// are blocked.
func (h *BookingHandler) CreateBlocks(w http.ResponseWriter, r *http.Request) {
	if h.blocks == nil {
		http.Error(w, "Blocking is not enabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Reason    string   `json:"reason"`
		PitchType string   `json:"pitchType"`
		Time      string   `json:"timeRange"`
		Dates     []string `json:"dates"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	reason, err := validate.Note(req.Reason)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	if _, ok := schedule.Units(req.PitchType); !ok {
		middleware.UserError(w, "Choose the whole pitch or one half.", http.StatusBadRequest)
		return
	}
	dates := uniqueSorted(req.Dates)
	if len(dates) == 0 {
		middleware.UserError(w, "Choose at least one date.", http.StatusBadRequest)
		return
	}
	if len(dates) > maxBlockDates {
		middleware.UserError(w, fmt.Sprintf("Block at most %d dates at a time.", maxBlockDates), http.StatusBadRequest)
		return
	}
	series, err := newReference()
	if err != nil {
		log.Printf("[RAND_ERROR] %v", err)
		http.Error(w, "Failed to block the sessions", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	now := h.now()
	created := make([]blockView, 0, len(dates))
	skipped := []skippedBlock{}
	for _, date := range dates {
		session, err := schedule.ParseBlock(date, req.Time, now)
		if err != nil {
			skipped = append(skipped, skippedBlock{Date: date, Reason: err.Error()})
			continue
		}
		block := models.Block{
			ID: primitive.NewObjectID(), Date: session.Date, Time: session.Label(),
			PitchType: req.PitchType, Reason: reason, SeriesID: series, CreatedAt: now,
		}
		taken, err := h.blockOne(ctx, session, block)
		if err != nil {
			log.Printf("[DB_ERROR] block session: %v", err)
			// What was blocked before the failure stays blocked, so say nothing
			// was done only when that is true.
			if len(created) > 0 {
				h.cache.invalidate()
			}
			http.Error(w, "Failed to block the sessions", http.StatusInternalServerError)
			return
		}
		if taken {
			skipped = append(skipped, skippedBlock{Date: date, Reason: "already booked or blocked at that time"})
			continue
		}
		created = append(created, blockViewOf(block))
	}
	if len(created) > 0 {
		h.cache.invalidate()
	}
	log.Printf("[BLOCK] blocked %d session(s), skipped %d", len(created), len(skipped))

	if len(created) == 0 {
		msg := "Nothing was blocked: " + skipped[0].Reason + "."
		if len(dates) > 1 {
			msg = fmt.Sprintf("None of the %d sessions could be blocked (the first: %s).", len(dates), skipped[0].Reason)
		}
		middleware.UserError(w, capitalise(msg), http.StatusConflict)
		return
	}
	body, err := json.Marshal(map[string]any{"created": created, "skipped": skipped})
	if err != nil {
		http.Error(w, "Failed to block the sessions", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

// blockOne records the block if the pitch has room for it, and reports taken
// when it does not. The whole date is locked, as it is for a booking, so a
// customer paying for the same session at this moment is either seen here or
// sees the block.
func (h *BookingHandler) blockOne(ctx context.Context, s schedule.Session, b models.Block) (taken bool, err error) {
	h.Locker.Lock(s.Date)
	defer h.Locker.Unlock(s.Date)

	existing, err := h.store.Occupancy(ctx, s.Date)
	if err != nil {
		return false, err
	}
	blocks, err := h.blocks.OnDate(ctx, s.Date)
	if err != nil {
		return false, err
	}
	existing = append(existing, blockOccupancy(blocks, s.Date)...)

	units, _ := schedule.Units(b.PitchType)
	if !schedule.Fits(existing, s.Start, s.End, units) {
		return true, nil
	}
	return false, h.blocks.Insert(ctx, b)
}

// uniqueSorted drops repeats and puts the dates in order.
func uniqueSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// RemoveBlock lifts one blocked session, so customers can book it again.
func (h *BookingHandler) RemoveBlock(w http.ResponseWriter, r *http.Request) {
	if h.blocks == nil {
		http.Error(w, "Blocking is not enabled", http.StatusServiceUnavailable)
		return
	}
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		middleware.UserError(w, "That is not a blocked session.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	removed, err := h.blocks.Remove(ctx, id)
	if err != nil {
		log.Printf("[DB_ERROR] remove block: %v", err)
		http.Error(w, "Failed to unblock the session", http.StatusInternalServerError)
		return
	}
	if !removed {
		middleware.UserError(w, "That session is not blocked any more.", http.StatusNotFound)
		return
	}
	h.cache.invalidate()
	log.Printf("[BLOCK] lifted 1 block")
	w.WriteHeader(http.StatusNoContent)
}

// RemoveBlockSeries lifts every session of a series from today on, for a team
// that has stopped playing. Sessions that have already been played stay as history.
func (h *BookingHandler) RemoveBlockSeries(w http.ResponseWriter, r *http.Request) {
	if h.blocks == nil {
		http.Error(w, "Blocking is not enabled", http.StatusServiceUnavailable)
		return
	}
	series := r.PathValue("series")
	if raw, err := hex.DecodeString(series); err != nil || len(raw) != 16 {
		middleware.UserError(w, "That is not a series of blocked sessions.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	n, err := h.blocks.RemoveSeries(ctx, series, schedule.OperatingDate(h.now()))
	if err != nil {
		log.Printf("[DB_ERROR] remove block series: %v", err)
		http.Error(w, "Failed to unblock the sessions", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		middleware.UserError(w, "Those sessions are not blocked any more.", http.StatusNotFound)
		return
	}
	h.cache.invalidate()
	log.Printf("[BLOCK] lifted %d block(s) of a series", n)
	body, _ := json.Marshal(map[string]int{"removed": n})
	writeJSON(w, http.StatusOK, body)
}

// blockedSlots lists the blocked sessions from today on the way the public
// calendar lists a booked one: the admin's note on who a block is for is not
// part of it.
func (h *BookingHandler) blockedSlots(ctx context.Context) ([]map[string]string, error) {
	if h.blocks == nil {
		return nil, nil
	}
	blocks, err := h.blocks.From(ctx, schedule.OperatingDate(h.now()))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, map[string]string{
			"date":      b.Date,
			"time":      b.Time,
			"status":    models.StatusBlocked,
			"pitchType": b.PitchType,
		})
	}
	return out, nil
}
