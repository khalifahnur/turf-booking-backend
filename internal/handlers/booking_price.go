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
	// the member discount covers, whether or not this booking gets it.
	Member *memberStanding
}

// memberStanding is where a member is with the discount in the month the
// session falls in.
type memberStanding struct {
	Month string
	// Paid is how many 8-a-side 2 hour sessions the number has paid for in that
	// month, not counting this booking.
	Paid int
	// DiscountHeld is true when the month's discount has already been given.
	DiscountHeld bool
}

// Applied reports whether this booking gets the discount: the month's discount
// has not been given yet and the number has paid for the three sessions before
// it. A member who has paid for more than three without having had the discount
// (their sessions were all booked before the earlier ones were paid) gets it on
// their next one.
func (p bookingPrice) Applied() bool {
	return p.Member != nil && !p.Member.DiscountHeld && p.Member.Paid >= schedule.MemberDiscountSession-1
}

// priceBooking works out what a booking costs. Everyone pays the standard
// price, except a member on a session the discount covers once they have paid
// for three of them in the month. The caller holds the member's lock
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
	standing, err := h.store.MemberMonth(ctx, phone, month)
	if err != nil {
		return p, err
	}
	p.Member = &memberStanding{Month: month, Paid: standing.Paid, DiscountHeld: standing.DiscountHeld}
	if p.Applied() {
		if discounted, ok := schedule.MemberPriceKES(pitchType, session.Minutes()); ok {
			p.Charge = discounted
		}
	}
	return p, nil
}
