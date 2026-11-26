package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/registration"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/utils"
	"turf-booking-backend/internal/validate"
	"turf-booking-backend/internal/ws"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	maxRegistrationBody = 4 << 10

	// tournamentLockKey makes "are there places left?" and "take one" a single
	// step for the whole tournament: two teams registering for the last place at
	// the same moment cannot both be given it.
	tournamentLockKey = "tournament"

	// academyPendingWindow is how long a second registration of the same player
	// from the same number is refused while a first payment prompt may still be
	// live, so a double-click cannot send two prompts.
	academyPendingWindow = 2 * time.Minute
)

// RegistrationHandler registers teams for the tournament and players at the
// academy, and takes payment for both.
type RegistrationHandler struct {
	Store  RegistrationStore
	Config *config.Config
	Hub    *ws.PaymentHub
	Locker *utils.SlotLocker

	// phoneLimiter stops the forms being used to spam someone's phone with M-Pesa
	// prompts.
	phoneLimiter *middleware.Limiter

	// Replaceable so tests can run the whole flow without Paystack or SMS.
	pay     func(cfg *config.Config, phone, email string, amountCents int, reference string) (string, error)
	sendSMS func(cfg *config.Config, reg models.Registration) error
	notify  func(reference, status string)
	now     func() time.Time
}

func NewRegistrationHandler(store RegistrationStore, cfg *config.Config, hub *ws.PaymentHub) *RegistrationHandler {
	return &RegistrationHandler{
		Store:        store,
		Config:       cfg,
		Hub:          hub,
		Locker:       utils.NewSlotLocker(),
		phoneLimiter: middleware.NewLimiter(0.3, 3), // 3 attempts, then one every ~3 minutes
		pay:          services.InitiatePayment,
		sendSMS:      sendRegistrationSMS,
		notify:       hub.NotifyClient,
		now:          time.Now,
	}
}

func sendRegistrationSMS(cfg *config.Config, reg models.Registration) error {
	if reg.Kind == registration.KindTournament {
		return services.SendTournamentConfirmationSMS(cfg, reg.PhoneNumber, reg.TeamName,
			registration.Labels(reg.Categories), reg.AmountKES)
	}
	return services.SendAcademyConfirmationSMS(cfg, reg.PhoneNumber, reg.PlayerName, reg.AmountKES)
}

// readUserJSON reads the one JSON object a registration request carries. When it
// cannot, it says why on w, in words for the person filling in the form.
func readUserJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		middleware.UserError(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegistrationBody))
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			middleware.UserError(w, "Request too large", http.StatusRequestEntityTooLarge)
			return false
		}
		middleware.UserError(w, "Invalid JSON payload", http.StatusBadRequest)
		return false
	}
	if dec.More() {
		middleware.UserError(w, "Invalid JSON payload", http.StatusBadRequest)
		return false
	}
	return true
}

func newRegistrationReference(prefix string) (string, error) {
	token, err := newReference()
	if err != nil {
		return "", err
	}
	return prefix + token, nil
}

// ---- the tournament ---------------------------------------------------------

type tournamentRequest struct {
	AcademyName   string   `json:"academyName"`
	Players       int      `json:"players"`
	Categories    []string `json:"categories"`
	PhoneNumber   string   `json:"phoneNumber"`
	AcceptedRules bool     `json:"acceptedRules"`
	RulesVersion  string   `json:"rulesVersion"`
}

// slotView is how many places of a category are taken, for the page and the admin.
// Capacity is every team the category takes and Reserved how many of those the
// academy's own teams hold; Left is what a new team can still get.
type slotView struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Group    string `json:"group"`
	Capacity int    `json:"capacity"`
	Reserved int    `json:"reserved"`
	Paid     int    `json:"paid"`
	Held     int    `json:"held"`
	Left     int    `json:"left"`
}

func slotViews(slots map[string]registration.Slots) []slotView {
	cats := registration.Categories()
	out := make([]slotView, 0, len(cats))
	for _, c := range cats {
		s := slots[c.ID]
		out = append(out, slotView{
			ID: c.ID, Label: c.Label, Group: c.Group, Capacity: c.Capacity, Reserved: c.Reserved,
			Paid: s.Paid, Held: s.Held, Left: registration.Left(c.ID, s),
		})
	}
	return out
}

// TournamentInfo is the public picture of the tournament: the fee, the limits the
// form applies and how many places each category has left. It names nobody.
func (h *RegistrationHandler) TournamentInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	slots, err := h.Store.TournamentSlots(ctx, h.now().Add(-registration.HoldWindow), primitive.NilObjectID)
	if err != nil {
		log.Printf("[DB_ERROR] tournament slots: %v", err)
		http.Error(w, "Failed to load the tournament", http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(map[string]any{
		"feePerCategory": registration.TournamentFeePerCategoryKES,
		"minPlayers":     registration.MinPlayers,
		"maxPlayers":     registration.MaxPlayers,
		"rulesVersion":   registration.RulesVersion,
		"holdMinutes":    int(registration.HoldWindow / time.Minute),
		"categories":     slotViews(slots),
	})
	if err != nil {
		http.Error(w, "Failed to load the tournament", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// fullMessage explains which categories have no place left and what can still be done.
func fullMessage(full, wanted []string, slots map[string]registration.Slots) string {
	var msg string
	if len(full) == 1 {
		msg = fmt.Sprintf("%s is full: all its places are taken.", full[0])
	} else {
		msg = fmt.Sprintf("%s are full: all their places are taken.", strings.Join(full, " and "))
	}
	var open []string
	stillPaying := false
	for _, id := range wanted {
		s := slots[id]
		if registration.Full(id, s) {
			stillPaying = stillPaying || s.Held > 0
			continue
		}
		open = append(open, id)
	}
	if len(open) > 0 {
		msg += fmt.Sprintf(" You can still register for %s on its own.", registration.Labels(open))
	}
	if stillPaying {
		msg += " A team that is still paying may release its place in a few minutes."
	}
	return msg
}

// RegisterTournament validates a team's registration, gives it a place in each
// category it asked for if there are any left (first come, first place), records
// it as Pending and sends an M-Pesa prompt for the fee.
func (h *RegistrationHandler) RegisterTournament(w http.ResponseWriter, r *http.Request) {
	var req tournamentRequest
	if !readUserJSON(w, r, &req) {
		return
	}

	teamName, err := validate.Name(req.AcademyName)
	if err != nil {
		middleware.UserError(w, "Academy "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := registration.ValidatePlayers(req.Players); err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	entry, err := registration.NewEntry(req.Categories)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	phone, err := validate.KenyanMobile(req.PhoneNumber)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	// Agreeing to the rules is a condition of entering, so it is checked here and
	// not only in the form.
	if !req.AcceptedRules {
		middleware.UserError(w, "Please accept the tournament rules to continue", http.StatusBadRequest)
		return
	}
	if req.RulesVersion != registration.RulesVersion {
		middleware.UserError(w, "The tournament rules have been updated. Please refresh the page and accept them again", http.StatusConflict)
		return
	}

	reference, err := newRegistrationReference(registration.TournamentReferencePrefix)
	if err != nil {
		log.Printf("[RAND_ERROR] %v", err)
		middleware.UserError(w, "We could not start your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}

	now := h.now()
	teamKey := strings.ToLower(teamName)

	h.Locker.Lock(tournamentLockKey)
	unlock := sync.OnceFunc(func() { h.Locker.Unlock(tournamentLockKey) })
	defer unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	holdingAfter := now.Add(-registration.HoldWindow)

	already, err := h.Store.TeamHolds(ctx, teamKey, entry.Categories, holdingAfter)
	if err != nil {
		log.Printf("[DB_ERROR] tournament team check: %v", err)
		middleware.UserError(w, "We could not check your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}
	if len(already) > 0 {
		middleware.UserError(w, fmt.Sprintf("%s is already registered, or has a payment in progress, for %s.",
			teamName, registration.Labels(already)), http.StatusConflict)
		return
	}

	slots, err := h.Store.TournamentSlots(ctx, holdingAfter, primitive.NilObjectID)
	if err != nil {
		log.Printf("[DB_ERROR] tournament slots: %v", err)
		middleware.UserError(w, "We could not check the places left. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}
	if full := registration.Unavailable(entry.Categories, slots); len(full) > 0 {
		middleware.UserError(w, fullMessage(full, entry.Categories, slots), http.StatusConflict)
		return
	}

	// Counted only once the registration would really go ahead, so a team that is
	// turned away above does not use up an attempt.
	if allowed, wait := h.phoneLimiter.Allow(phone); !allowed {
		middleware.TooManyRequestsMessage(w, wait,
			"Too many payment attempts for this phone number. Please wait a few minutes and try again.")
		return
	}

	reg := models.Registration{
		ID:              primitive.NewObjectID(),
		Kind:            registration.KindTournament,
		PhoneNumber:     phone,
		TeamName:        teamName,
		TeamKey:         teamKey,
		Players:         req.Players,
		Categories:      entry.Categories,
		RulesVersion:    registration.RulesVersion,
		RulesAcceptedAt: now,
		AmountKES:       entry.FeeKES,
		Payment:         models.RegistrationPayment{Reference: reference},
		Status:          registration.StatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	h.start(ctx, w, r, reg, unlock, registration.HoldWindow, map[string]any{"categories": entry.Categories})
}

// ---- the academy ------------------------------------------------------------

type academyRequest struct {
	PlayerName   string `json:"playerName"`
	DateOfBirth  string `json:"dateOfBirth"`
	GuardianName string `json:"guardianName"`
	PhoneNumber  string `json:"phoneNumber"`
}

// AcademyInfo is the public fee for registering at the academy, so the page never
// has to state a price of its own.
func (h *RegistrationHandler) AcademyInfo(w http.ResponseWriter, r *http.Request) {
	body, _ := json.Marshal(map[string]any{"fee": registration.AcademyFeeKES})
	writeJSON(w, http.StatusOK, body)
}

// RegisterAcademy validates a player's registration at the academy, records it as
// Pending and sends their parent an M-Pesa prompt for the fee.
func (h *RegistrationHandler) RegisterAcademy(w http.ResponseWriter, r *http.Request) {
	var req academyRequest
	if !readUserJSON(w, r, &req) {
		return
	}

	playerName, err := validate.Name(req.PlayerName)
	if err != nil {
		middleware.UserError(w, "Player's "+err.Error(), http.StatusBadRequest)
		return
	}
	guardianName, err := validate.Name(req.GuardianName)
	if err != nil {
		middleware.UserError(w, "Parent's "+err.Error(), http.StatusBadRequest)
		return
	}
	now := h.now()
	birth, err := registration.ParseBirthDate(req.DateOfBirth, now)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	phone, err := validate.KenyanMobile(req.PhoneNumber)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}

	reference, err := newRegistrationReference(registration.AcademyReferencePrefix)
	if err != nil {
		log.Printf("[RAND_ERROR] %v", err)
		middleware.UserError(w, "We could not start your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}

	playerKey := strings.ToLower(playerName)

	// Check-then-insert must be one step per player, or two quick submissions
	// could both pass the duplicate check.
	lockKey := "academy|" + phone + "|" + playerKey
	h.Locker.Lock(lockKey)
	unlock := sync.OnceFunc(func() { h.Locker.Unlock(lockKey) })
	defer unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// A player who has paid is registered for good; one whose prompt may still be
	// answered must not be sent a second.
	exists, err := h.Store.PlayerHolds(ctx, playerKey, phone, now.Add(-academyPendingWindow))
	if err != nil {
		log.Printf("[DB_ERROR] academy player check: %v", err)
		middleware.UserError(w, "We could not check your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}
	if exists {
		middleware.UserError(w, fmt.Sprintf("%s is already registered, or has a payment in progress. Please wait a moment, or call us if you need help.", playerName), http.StatusConflict)
		return
	}
	if allowed, wait := h.phoneLimiter.Allow(phone); !allowed {
		middleware.TooManyRequestsMessage(w, wait,
			"Too many payment attempts for this phone number. Please wait a few minutes and try again.")
		return
	}

	reg := models.Registration{
		ID:           primitive.NewObjectID(),
		Kind:         registration.KindAcademy,
		PhoneNumber:  phone,
		PlayerName:   playerName,
		PlayerKey:    playerKey,
		DateOfBirth:  birth,
		GuardianName: guardianName,
		AmountKES:    registration.AcademyFeeKES,
		Payment:      models.RegistrationPayment{Reference: reference},
		Status:       registration.StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	h.start(ctx, w, r, reg, unlock, academyPendingWindow, nil)
}

// ---- paying -----------------------------------------------------------------

// start records a validated registration as Pending, lets go of the lock that
// made the checks before it atomic, and sends the M-Pesa prompt for its fee.
func (h *RegistrationHandler) start(ctx context.Context, w http.ResponseWriter, r *http.Request, reg models.Registration, unlock func(), wait time.Duration, extra map[string]any) {
	if err := h.Store.Insert(ctx, reg); err != nil {
		log.Printf("[DB_ERROR] registration insert: %v", err)
		h.phoneLimiter.Refund(reg.PhoneNumber) // nothing was sent, so this attempt should not count
		middleware.UserError(w, "We could not save your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}
	// The place is recorded, so the check is settled; the payment call below can
	// take a while and must not hold everyone else up.
	unlock()

	ref := reg.Payment.Reference
	if _, err := h.pay(h.Config, reg.PhoneNumber, registration.PayerEmail(reg.PhoneNumber), reg.AmountKES*100, ref); err != nil {
		log.Printf("[PAYMENT_ERROR] registration: %v", err)
		if errors.Is(err, services.ErrPaymentUncertain) {
			// Paystack may have sent the prompt even though we did not hear back. The
			// registration stays Pending, so its place is still held, an immediate
			// retry is refused, and a payment that does arrive still settles it.
			middleware.UserError(w, fmt.Sprintf("We could not confirm that the M-Pesa prompt was sent. Please check your phone: if a prompt arrived, enter your PIN and we will confirm by SMS. If nothing arrives, wait about %d minutes and try again.", int(wait/time.Minute)), http.StatusBadGateway)
			return
		}
		failCtx, failCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer failCancel()
		if _, ferr := h.Store.MarkFailed(failCtx, ref, h.now()); ferr != nil {
			log.Printf("[DB_ERROR] registration mark failed: %v", ferr)
		}
		h.phoneLimiter.Refund(reg.PhoneNumber) // refused before any prompt went out
		middleware.UserError(w, "We could not send the M-Pesa prompt. You have not been charged. Please try again.", http.StatusBadGateway)
		return
	}

	out := map[string]any{"message": "Awaiting Payment", "reference": ref, "total": reg.AmountKES}
	for k, v := range extra {
		out[k] = v
	}
	body, _ := json.Marshal(out)
	writeJSON(w, http.StatusCreated, body)
}

// HandlePaystackEvent finishes a registration payment from the shared Paystack
// webhook, which has already verified the signature. It writes the response.
func (h *RegistrationHandler) HandlePaystackEvent(w http.ResponseWriter, r *http.Request, ev paystackEvent) {
	// Paystack may hang up before the write returns; the outcome must still be
	// recorded, so this does not follow the request's cancellation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()

	switch {
	case ev.Event == "charge.success":
		h.settle(ctx, w, ev)
	case ev.failed():
		h.fail(ctx, w, ev)
	default:
		// Not an outcome of a charge (a transfer, a refund...): nothing to do.
		w.WriteHeader(http.StatusOK)
	}
}

func (h *RegistrationHandler) settle(ctx context.Context, w http.ResponseWriter, ev paystackEvent) {
	ref := ev.Reference

	if ev.Status != "" && !strings.EqualFold(ev.Status, "success") {
		log.Printf("Registration webhook ignored: reference %s charge.success carries status %q\n", ref, ev.Status)
		w.WriteHeader(http.StatusOK)
		return
	}
	if ev.Currency != "" && !strings.EqualFold(ev.Currency, "KES") {
		// The amount is only comparable to our fee in shillings.
		log.Printf("[RECONCILE] registration reference %s was paid in %s, not KES; not settled\n", ref, ev.Currency)
		w.WriteHeader(http.StatusOK)
		return
	}
	if ev.AmountCents <= 0 {
		// Settled below without checking the fee, because refusing a payment
		// Paystack confirmed is the worse mistake; say so, so it can be checked.
		log.Printf("[RECONCILE] registration reference %s: charge.success carried no amount; settling without comparing it to the fee\n", ref)
	}

	// Settling and checking the places left is one step, in the same order as the
	// registrations that give the places out.
	h.Locker.Lock(tournamentLockKey)
	reg, err := h.Store.MarkPaid(ctx, ref, ev.AmountCents, h.now())
	var over []string
	if err == nil && reg != nil && reg.Kind == registration.KindTournament {
		over, err = h.overCapacity(ctx, *reg)
	}
	h.Locker.Unlock(tournamentLockKey)

	if err != nil {
		log.Printf("Failed to update registration for reference %s: %v\n", ref, err)
		w.WriteHeader(http.StatusInternalServerError) // makes Paystack retry
		return
	}
	if reg == nil {
		h.explainUnsettled(ctx, ref, ev.AmountCents)
		w.WriteHeader(http.StatusOK)
		return
	}
	if ev.AmountCents != int64(reg.AmountKES)*100 && ev.AmountCents > 0 {
		log.Printf("[RECONCILE] registration reference %s: Paystack collected %d cents, expected %d\n", ref, ev.AmountCents, reg.AmountKES*100)
	}
	if len(over) > 0 {
		log.Printf("[OVERFLOW] registration reference %s paid after %s had filled up; it needs refunding\n", ref, registration.Labels(over))
	}

	go func(reg models.Registration) {
		if err := h.sendSMS(h.Config, reg); err != nil {
			log.Printf("[SMS_ERROR] registration confirmation for ref %s: %v\n", ref, err)
		}
	}(*reg)

	h.notify(ref, "Completed")
	w.WriteHeader(http.StatusOK)
}

// overCapacity checks a tournament payment that has just settled against the
// teams that paid before it. A hold normally guarantees the place, but a payment
// can land after its hold lapsed and the place went to someone else; the money is
// taken either way, so the registration is flagged for the admin to refund.
func (h *RegistrationHandler) overCapacity(ctx context.Context, reg models.Registration) ([]string, error) {
	slots, err := h.Store.TournamentSlots(ctx, h.now(), reg.ID) // pendingAfter is now: only paid ones count
	if err != nil {
		return nil, err
	}
	paid := make(map[string]int, len(slots))
	for id, s := range slots {
		paid[id] = s.Paid
	}
	over := registration.OverCapacity(reg.Categories, paid)
	if len(over) == 0 {
		return nil, nil
	}
	return over, h.Store.FlagOverflow(ctx, reg.ID, over)
}

// explainUnsettled logs why a successful charge did not settle a registration,
// because every one of these needs a person to look: either it is a repeat of a
// webhook already handled, or money was taken that the registration does not
// reflect.
func (h *RegistrationHandler) explainUnsettled(ctx context.Context, ref string, paidCents int64) {
	status, err := h.Store.Status(ctx, ref)
	switch {
	case errors.Is(err, errUnknownReference):
		log.Printf("[RECONCILE] registration webhook for unknown reference %s (%d cents)\n", ref, paidCents)
	case err != nil:
		log.Printf("[DB_ERROR] registration status for %s: %v\n", ref, err)
	case status == registration.StatusPaid:
		log.Printf("Registration webhook ignored: reference %s already processed\n", ref)
	default:
		log.Printf("[UNDERPAID] registration reference %s (%s): Paystack collected %d cents, less than the fee; registration left unpaid\n", ref, status, paidCents)
	}
}

func (h *RegistrationHandler) fail(ctx context.Context, w http.ResponseWriter, ev paystackEvent) {
	moved, err := h.Store.MarkFailed(ctx, ev.Reference, h.now())
	if err != nil {
		log.Printf("Failed to update registration for reference %s: %v\n", ev.Reference, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Only tell the payer it failed when it did. A paid registration must never be
	// shown as failed by a stray event, and an already-failed one is told again in
	// case the first message was missed.
	if moved || h.statusIs(ctx, ev.Reference, registration.StatusFailed, registration.StatusExpired) {
		h.notify(ev.Reference, "Failed")
	}
	w.WriteHeader(http.StatusOK)
}

func (h *RegistrationHandler) statusIs(ctx context.Context, ref string, statuses ...string) bool {
	got, err := h.Store.Status(ctx, ref)
	if err != nil {
		return false
	}
	for _, s := range statuses {
		if got == s {
			return true
		}
	}
	return false
}

// PaymentStatus reports a registration payment as the payment WebSocket understands it.
func (h *RegistrationHandler) PaymentStatus(ctx context.Context, reference string) (string, error) {
	status, err := h.Store.Status(ctx, reference)
	if err != nil {
		return "", err
	}
	switch status {
	case registration.StatusPaid:
		return "Completed", nil
	case registration.StatusFailed, registration.StatusExpired:
		return "Failed", nil
	default:
		return "Pending", nil
	}
}

// ---- the admin dashboard ------------------------------------------------------

type tournamentRegistrationView struct {
	ID          string     `json:"id"`
	AcademyName string     `json:"academyName"`
	Players     int        `json:"players"`
	Categories  []string   `json:"categories"`
	PhoneNumber string     `json:"phoneNumber"`
	AmountKES   int        `json:"amount"`
	Status      string     `json:"status"`
	PaidAt      *time.Time `json:"paidAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	// Overflow lists the categories this paid into after they were full; it needs refunding.
	Overflow     []string `json:"overflow"`
	RulesVersion string   `json:"rulesVersion"`
}

type academyRegistrationView struct {
	ID           string     `json:"id"`
	PlayerName   string     `json:"playerName"`
	DateOfBirth  string     `json:"dateOfBirth"`
	GuardianName string     `json:"guardianName"`
	PhoneNumber  string     `json:"phoneNumber"`
	AmountKES    int        `json:"amount"`
	Status       string     `json:"status"`
	PaidAt       *time.Time `json:"paidAt"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ListTournamentAdmin returns the paid and pending tournament registrations with
// how many places each category has taken. It must be wrapped in AdminOnly: it
// exposes phone numbers.
func (h *RegistrationHandler) ListTournamentAdmin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	regs, err := h.Store.ListForAdmin(ctx, registration.KindTournament)
	if err != nil {
		log.Printf("[DB_ERROR] tournament list: %v", err)
		http.Error(w, "Failed to load registrations", http.StatusInternalServerError)
		return
	}
	slots, err := h.Store.TournamentSlots(ctx, h.now().Add(-registration.HoldWindow), primitive.NilObjectID)
	if err != nil {
		log.Printf("[DB_ERROR] tournament slots: %v", err)
		http.Error(w, "Failed to load registrations", http.StatusInternalServerError)
		return
	}

	views := make([]tournamentRegistrationView, 0, len(regs))
	for _, reg := range regs {
		views = append(views, tournamentRegistrationView{
			ID:           reg.ID.Hex(),
			AcademyName:  reg.TeamName,
			Players:      reg.Players,
			Categories:   nonNil(reg.Categories),
			PhoneNumber:  reg.PhoneNumber,
			AmountKES:    reg.AmountKES,
			Status:       reg.Status,
			PaidAt:       reg.Payment.PaidAt,
			CreatedAt:    reg.CreatedAt,
			Overflow:     nonNil(reg.Overflow),
			RulesVersion: reg.RulesVersion,
		})
	}
	body, err := json.Marshal(map[string]any{
		"feePerCategory": registration.TournamentFeePerCategoryKES,
		"holdMinutes":    int(registration.HoldWindow / time.Minute),
		"categories":     slotViews(slots),
		"registrations":  views,
	})
	if err != nil {
		http.Error(w, "Failed to load registrations", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// ListAcademyAdmin returns the paid and pending academy registrations. It must be
// wrapped in AdminOnly: it exposes children's and parents' details.
func (h *RegistrationHandler) ListAcademyAdmin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	regs, err := h.Store.ListForAdmin(ctx, registration.KindAcademy)
	if err != nil {
		log.Printf("[DB_ERROR] academy list: %v", err)
		http.Error(w, "Failed to load registrations", http.StatusInternalServerError)
		return
	}
	views := make([]academyRegistrationView, 0, len(regs))
	for _, reg := range regs {
		views = append(views, academyRegistrationView{
			ID:           reg.ID.Hex(),
			PlayerName:   reg.PlayerName,
			DateOfBirth:  reg.DateOfBirth,
			GuardianName: reg.GuardianName,
			PhoneNumber:  reg.PhoneNumber,
			AmountKES:    reg.AmountKES,
			Status:       reg.Status,
			PaidAt:       reg.Payment.PaidAt,
			CreatedAt:    reg.CreatedAt,
		})
	}
	body, err := json.Marshal(map[string]any{
		"fee":           registration.AcademyFeeKES,
		"registrations": views,
	})
	if err != nil {
		http.Error(w, "Failed to load registrations", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// AdminOnly guards a registration handler with the shared admin API key.
func (h *RegistrationHandler) AdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return adminOnly(h.Config, next)
}
