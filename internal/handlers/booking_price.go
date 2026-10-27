package handlers

import (
	"context"

	"turf-booking-backend/internal/schedule"
)

// bookingPrice is what a booking costs and why.
type bookingPrice struct {
	// List is the standard price and Charge is what is actually charged.
	List, Charge int
	// Member is set when the number is on the member list and the session is one
	// the member discount covers, whether or not it still has discounted
	// sessions left this month.
	Member *memberStanding
}

// memberStanding is where a member is with their discounted sessions in the
// month the session falls in.
type memberStanding struct {
	Month string
	// Used is how many member-priced sessions the member already holds in that
	// month, not counting this booking.
	Used int
}

// Applied reports whether the member price is the one being charged.
func (p bookingPrice) Applied() bool {
	return p.Member != nil && p.Member.Used < schedule.MemberSessionsPerMonth
}

// priceBooking works out what a booking costs. Everyone pays the standard
// price, except a member on a session the discount covers, until they have used
// their discounted sessions for the month. The caller holds the member's lock
// (memberLockKey) when it goes on to record a booking at this price.
func (h *BookingHandler) priceBooking(ctx context.Context, phone, pitchType string, session schedule.Session, list int) (bookingPrice, error) {
	p := bookingPrice{List: list, Charge: list}
	if h.members == nil || !schedule.MemberEligible(pitchType, session.Minutes()) {
		return p, nil
	}
	member, err := h.members.Find(ctx, phone)
	if err != nil || member == nil {
		return p, err
	}
	month := schedule.MonthOf(session.Date)
	used, err := h.store.MemberSessions(ctx, phone, month)
	if err != nil {
		return p, err
	}
	p.Member = &memberStanding{Month: month, Used: used}
	if p.Applied() {
		if discounted, ok := schedule.MemberPriceKES(pitchType, session.Minutes()); ok {
			p.Charge = discounted
		}
	}
	return p, nil
}
