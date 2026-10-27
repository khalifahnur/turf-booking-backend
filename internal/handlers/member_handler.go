package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"
	"turf-booking-backend/internal/validate"
)

// ---- the price a customer will be charged ------------------------------------

// quoteMember tells a member what the discount does for the booking being
// priced. It is only present for a number on the member list, on a session the
// discount covers.
type quoteMember struct {
	Percent int `json:"percent"`
	// Applied is false once the member has used the month's discounted sessions.
	Applied bool `json:"applied"`
	// Month is the month the session falls in ("2026-11"); Used of Limit
	// discounted sessions are already held in it, not counting this booking.
	Month string `json:"month"`
	Used  int    `json:"used"`
	Limit int    `json:"limit"`
}

type quoteResponse struct {
	// Amount is what the customer will be charged; ListPrice is the standard price.
	Amount    int          `json:"amount"`
	ListPrice int          `json:"listPrice"`
	Member    *quoteMember `json:"member,omitempty"`
}

// Quote says what a booking would cost, so the booking page can show a member
// their price before they pay. It decides nothing: the price is worked out
// again, from the same rules, when the booking is made.
func (h *BookingHandler) Quote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PhoneNumber string `json:"phoneNumber"`
		Date        string `json:"date"`
		Time        string `json:"timeRange"`
		PitchType   string `json:"pitchType"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	phone, err := validate.KenyanMobile(req.PhoneNumber)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	session, err := schedule.ParseSession(req.Date, req.Time, h.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok := schedule.Units(req.PitchType); !ok {
		http.Error(w, "Invalid pitch type provided", http.StatusBadRequest)
		return
	}
	list, ok := schedule.PriceKES(req.PitchType, session.Minutes())
	if !ok {
		http.Error(w, "Invalid session length", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	price, err := h.priceBooking(ctx, phone, req.PitchType, session, list)
	if err != nil {
		log.Printf("[DB_ERROR] quote: %v", err)
		http.Error(w, "Database read error", http.StatusInternalServerError)
		return
	}

	out := quoteResponse{Amount: price.Charge, ListPrice: price.List}
	if price.Member != nil {
		out.Member = &quoteMember{
			Percent: schedule.MemberDiscountPercent,
			Applied: price.Applied(),
			Month:   price.Member.Month,
			Used:    price.Member.Used,
			Limit:   schedule.MemberSessionsPerMonth,
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "Failed to price the booking", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// ---- the member list (admin) ---------------------------------------------------

// memberView is a member as the admin dashboard sees them.
type memberView struct {
	ID          string    `json:"id"`
	FullName    string    `json:"fullName"`
	PhoneNumber string    `json:"phoneNumber"`
	CreatedAt   time.Time `json:"createdAt"`
	// Used is how many member-priced sessions they hold in the current month.
	Used int `json:"used"`
}

// memberList is the member list with the rule it applies, so the dashboard
// never has to repeat it.
type memberList struct {
	Month   string       `json:"month"`
	Percent int          `json:"percent"`
	Limit   int          `json:"limit"`
	Members []memberView `json:"members"`
}

func (h *BookingHandler) memberListFor(ctx context.Context, month string) (memberList, error) {
	members, err := h.members.List(ctx)
	if err != nil {
		return memberList{}, err
	}
	used, err := h.store.MemberSessionsByPhone(ctx, month)
	if err != nil {
		return memberList{}, err
	}
	out := memberList{
		Month:   month,
		Percent: schedule.MemberDiscountPercent,
		Limit:   schedule.MemberSessionsPerMonth,
		Members: make([]memberView, 0, len(members)),
	}
	for _, m := range members {
		out.Members = append(out.Members, viewOf(m, used[m.PhoneNumber]))
	}
	sort.SliceStable(out.Members, func(i, j int) bool {
		return strings.ToLower(out.Members[i].FullName) < strings.ToLower(out.Members[j].FullName)
	})
	return out, nil
}

func viewOf(m models.Member, used int) memberView {
	return memberView{
		ID:          m.ID.Hex(),
		FullName:    m.FullName,
		PhoneNumber: m.PhoneNumber,
		CreatedAt:   m.CreatedAt,
		Used:        used,
	}
}

// currentMonth is the month the venue is in, by its operating day.
func (h *BookingHandler) currentMonth() string {
	return schedule.MonthOf(schedule.OperatingDate(h.now()))
}

// ListMembers returns the member list with each member's discounted sessions
// this month. It must be wrapped in AdminOnly: it exposes phone numbers.
func (h *BookingHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	if h.members == nil {
		http.Error(w, "Members are not enabled", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	list, err := h.memberListFor(ctx, h.currentMonth())
	if err != nil {
		log.Printf("[DB_ERROR] list members: %v", err)
		http.Error(w, "Failed to load members", http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(list)
	if err != nil {
		http.Error(w, "Failed to load members", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// AddMember puts a number on the member list.
func (h *BookingHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	if h.members == nil {
		http.Error(w, "Members are not enabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		FullName    string `json:"fullName"`
		PhoneNumber string `json:"phoneNumber"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, err := validate.Name(req.FullName)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	phone, err := validate.KenyanMobile(req.PhoneNumber)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	err = h.members.Add(ctx, models.Member{PhoneNumber: phone, FullName: name, CreatedAt: h.now()})
	if errors.Is(err, errMemberExists) {
		middleware.UserError(w, "That number is already on the member list.", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("[DB_ERROR] add member: %v", err)
		http.Error(w, "Failed to add the member", http.StatusInternalServerError)
		return
	}

	added, err := h.members.Find(ctx, phone)
	if err != nil || added == nil {
		log.Printf("[DB_ERROR] read back member: %v", err)
		http.Error(w, "The member was added but could not be read back", http.StatusInternalServerError)
		return
	}
	// A number that was a member before and was taken off the list keeps the
	// sessions it already booked at the member price this month.
	used, err := h.store.MemberSessions(ctx, phone, h.currentMonth())
	if err != nil {
		log.Printf("[DB_ERROR] member sessions: %v", err)
		used = 0
	}
	log.Printf("[MEMBER] added a member (number ending %s)", phone[len(phone)-3:])

	body, err := json.Marshal(viewOf(*added, used))
	if err != nil {
		http.Error(w, "Failed to add the member", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

// RemoveMember takes a number off the member list. Bookings it already made at
// the member price stay as they are.
func (h *BookingHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	if h.members == nil {
		http.Error(w, "Members are not enabled", http.StatusServiceUnavailable)
		return
	}
	phone, err := validate.KenyanMobile(r.PathValue("phone"))
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	removed, err := h.members.Remove(ctx, phone)
	if err != nil {
		log.Printf("[DB_ERROR] remove member: %v", err)
		http.Error(w, "Failed to remove the member", http.StatusInternalServerError)
		return
	}
	if !removed {
		middleware.UserError(w, "That number is not on the member list.", http.StatusNotFound)
		return
	}
	log.Printf("[MEMBER] removed a member (number ending %s)", phone[len(phone)-3:])
	w.WriteHeader(http.StatusNoContent)
}
