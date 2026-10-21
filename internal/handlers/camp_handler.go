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
	"unicode"
	"unicode/utf8"

	"turf-booking-backend/internal/camp"
	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/utils"
	"turf-booking-backend/internal/validate"
	"turf-booking-backend/internal/ws"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	maxCampBody = 8 << 10

	// An M-Pesa prompt stays answerable for about a minute. While one may still
	// be live, a second registration for the same child is refused so a
	// double-click cannot send two prompts; after that a parent can retry.
	campPendingWindow = 2 * time.Minute
)

// CampHandler registers children for the holiday camp and takes payment for it.
type CampHandler struct {
	Store  CampStore
	Config *config.Config
	Hub    *ws.PaymentHub
	Locker *utils.SlotLocker

	// phoneLimiter stops the form being used to spam someone's phone with
	// M-Pesa prompts.
	phoneLimiter *middleware.Limiter

	// Replaceable so tests can run the whole flow without Paystack or SMS.
	pay     func(cfg *config.Config, phone, email string, amountCents int, reference string) (string, error)
	sendSMS func(cfg *config.Config, reg models.CampRegistration) error
	notify  func(reference, status string)
	now     func() time.Time
}

func NewCampHandler(store CampStore, cfg *config.Config, hub *ws.PaymentHub) *CampHandler {
	return &CampHandler{
		Store:        store,
		Config:       cfg,
		Hub:          hub,
		Locker:       utils.NewSlotLocker(),
		phoneLimiter: middleware.NewLimiter(0.3, 3), // 3 attempts, then one every ~3 minutes
		pay:          services.InitiatePayment,
		sendSMS: func(cfg *config.Config, reg models.CampRegistration) error {
			what, when := campSMSDetails(reg)
			return services.SendCampConfirmationSMS(cfg, reg.Parent.PhoneNumber, reg.Child.FullName,
				what, when, reg.AmountKES)
		},
		notify: hub.NotifyClient,
		now:    time.Now,
	}
}

type campRegisterRequest struct {
	ChildName     string   `json:"childName"`
	ParentName    string   `json:"parentName"`
	Email         string   `json:"email"`
	PhoneNumber   string   `json:"phoneNumber"`
	Program       string   `json:"program"`
	Lunch         bool     `json:"lunch"`
	Weeks         []string `json:"weeks"`
	AcceptedTerms bool     `json:"acceptedTerms"`
	TermsVersion  string   `json:"termsVersion"`
}

// campSMSDetails says what a registration bought and for when, for the
// confirmation text: "Full day 8:30am - 4:00pm + lunch" and "2 Nov - 13 Nov".
func campSMSDetails(reg models.CampRegistration) (what, when string) {
	p, ok := camp.LookupProgram(reg.Program)
	if !ok {
		p = camp.Program{Label: reg.Program}
	}
	return camp.ProgramSummary(p, reg.Lunch), camp.WeeksLabel(reg.Weeks)
}

func newCampReference() (string, error) {
	token, err := newReference()
	if err != nil {
		return "", err
	}
	return camp.ReferencePrefix + token, nil
}

// Register validates a registration, prices it, records it as Pending and
// sends the parent an M-Pesa prompt.
func (c *CampHandler) Register(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		middleware.UserError(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	var req campRegisterRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCampBody))
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			middleware.UserError(w, "Request too large", http.StatusRequestEntityTooLarge)
			return
		}
		middleware.UserError(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if dec.More() {
		middleware.UserError(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	childName, err := validate.Name(req.ChildName)
	if err != nil {
		middleware.UserError(w, "Child's "+err.Error(), http.StatusBadRequest)
		return
	}
	parentName, err := validate.Name(req.ParentName)
	if err != nil {
		middleware.UserError(w, "Parent's "+err.Error(), http.StatusBadRequest)
		return
	}
	email, err := validate.Email(req.Email)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}
	phone, err := validate.KenyanMobile(req.PhoneNumber)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}

	// The waiver is a condition of registering, so it is checked here and not
	// only in the form.
	if !req.AcceptedTerms {
		middleware.UserError(w, "Please accept the terms and conditions to continue", http.StatusBadRequest)
		return
	}
	if req.TermsVersion != camp.TermsVersion {
		middleware.UserError(w, "The terms and conditions have been updated. Please refresh the page and accept them again", http.StatusConflict)
		return
	}

	now := c.now()
	quote, err := camp.NewQuote(req.Program, req.Lunch, req.Weeks, now)
	if err != nil {
		middleware.UserError(w, capitalise(err.Error()), http.StatusBadRequest)
		return
	}

	reference, err := newCampReference()
	if err != nil {
		log.Printf("[RAND_ERROR] %v", err)
		middleware.UserError(w, "We could not start your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}

	childKey := strings.ToLower(childName)

	// Check-then-insert must be one step per child, or two quick submissions
	// could both pass the duplicate check.
	lockKey := "camp|" + phone + "|" + childKey
	c.Locker.Lock(lockKey)
	unlock := sync.OnceFunc(func() { c.Locker.Unlock(lockKey) })
	defer unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	taken, err := c.Store.Conflicts(ctx, childKey, phone, quote.Weeks, now.Add(-campPendingWindow))
	if err != nil {
		log.Printf("[DB_ERROR] camp conflicts: %v", err)
		middleware.UserError(w, "We could not check your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}
	if len(taken) > 0 {
		middleware.UserError(w, fmt.Sprintf("%s is already registered, or has a payment in progress, for: %s. Please check the weeks you chose or wait a moment.",
			childName, camp.WeekRunsLabel(taken)), http.StatusConflict)
		return
	}

	// Counted only once the registration would really go ahead, so a parent who
	// is turned away above (duplicate, not yet paid) does not use up an attempt.
	if allowed, wait := c.phoneLimiter.Allow(phone); !allowed {
		middleware.TooManyRequestsMessage(w, wait,
			"Too many payment attempts for this phone number. Please wait a few minutes and try again.")
		return
	}

	reg := models.CampRegistration{
		ID:         primitive.NewObjectID(),
		Child:      models.CampChild{FullName: childName, Key: childKey},
		Parent:     models.CampParent{FullName: parentName, Email: email, PhoneNumber: phone},
		Program:    quote.Program.ID,
		Lunch:      quote.Lunch,
		Weeks:      quote.Weeks,
		PerWeekKES: quote.PerWeekKES,
		AmountKES:  quote.TotalKES,
		Consent:    models.CampConsent{TermsVersion: camp.TermsVersion, AcceptedAt: now},
		Payment:    models.CampPayment{Reference: reference},
		Status:     camp.StatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := c.Store.Insert(ctx, reg); err != nil {
		log.Printf("[DB_ERROR] camp insert: %v", err)
		c.phoneLimiter.Refund(phone) // nothing was sent, so this attempt should not count
		middleware.UserError(w, "We could not save your registration. You have not been charged. Please try again.", http.StatusInternalServerError)
		return
	}
	unlock()

	if _, err := c.pay(c.Config, phone, email, quote.TotalKES*100, reference); err != nil {
		log.Printf("[PAYMENT_ERROR] camp: %v", err)
		if errors.Is(err, services.ErrPaymentUncertain) {
			// Paystack may have sent the prompt even though we did not hear back.
			// The registration stays Pending, so the two-minute window still stops
			// an immediate retry from sending a second prompt, and a payment that
			// does arrive still settles it.
			middleware.UserError(w, "We could not confirm that the M-Pesa prompt was sent. Please check your phone: if a prompt arrived, enter your PIN and we will confirm by SMS. If nothing arrives, wait about 2 minutes and try again.", http.StatusBadGateway)
			return
		}
		failCtx, failCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer failCancel()
		if _, ferr := c.Store.MarkFailed(failCtx, reference, c.now()); ferr != nil {
			log.Printf("[DB_ERROR] camp mark failed: %v", ferr)
		}
		c.phoneLimiter.Refund(phone) // refused before any prompt went out
		middleware.UserError(w, "We could not send the M-Pesa prompt. You have not been charged. Please try again.", http.StatusBadGateway)
		return
	}

	body, _ := json.Marshal(map[string]any{
		"message":   "Awaiting Payment",
		"reference": reference,
		"total":     quote.TotalKES,
	})
	writeJSON(w, http.StatusCreated, body)
}

// capitalise upper-cases the first letter of a validation message so every
// error shown to a parent reads as a sentence.
func capitalise(msg string) string {
	if msg == "" {
		return msg
	}
	r, size := utf8.DecodeRuneInString(msg)
	return string(unicode.ToUpper(r)) + msg[size:]
}

// paystackEvent is the part of a verified Paystack webhook that camp payments
// need.
type paystackEvent struct {
	Event       string
	Reference   string
	Status      string
	Currency    string
	AmountCents int64
}

// failed reports whether Paystack is saying the charge did not go through.
func (e paystackEvent) failed() bool {
	if e.Event == "charge.success" {
		return false
	}
	switch strings.ToLower(e.Status) {
	case "failed", "abandoned", "reversed":
		return true
	}
	return strings.HasSuffix(e.Event, ".failed")
}

// HandlePaystackEvent finishes a camp payment from the shared Paystack
// webhook, which has already verified the signature. It writes the response.
func (c *CampHandler) HandlePaystackEvent(w http.ResponseWriter, r *http.Request, ev paystackEvent) {
	// Paystack may hang up before the write returns; the outcome must still be
	// recorded, so this does not follow the request's cancellation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()

	switch {
	case ev.Event == "charge.success":
		c.settle(ctx, w, ev)
	case ev.failed():
		c.fail(ctx, w, ev)
	default:
		// Not an outcome of a charge (a transfer, a refund...): nothing to do, and
		// not a reason for the parent to be told the payment failed.
		w.WriteHeader(http.StatusOK)
	}
}

func (c *CampHandler) settle(ctx context.Context, w http.ResponseWriter, ev paystackEvent) {
	ref := ev.Reference

	if ev.Status != "" && !strings.EqualFold(ev.Status, "success") {
		log.Printf("Camp webhook ignored: reference %s charge.success carries status %q\n", ref, ev.Status)
		w.WriteHeader(http.StatusOK)
		return
	}
	if ev.Currency != "" && !strings.EqualFold(ev.Currency, "KES") {
		// The amount is only comparable to our price in shillings.
		log.Printf("[RECONCILE] camp reference %s was paid in %s, not KES; not settled\n", ref, ev.Currency)
		w.WriteHeader(http.StatusOK)
		return
	}

	if ev.AmountCents <= 0 {
		// Settled below without checking the price, because refusing a payment
		// Paystack confirmed is the worse mistake; say so, so it can be checked.
		log.Printf("[RECONCILE] camp reference %s: charge.success carried no amount; settling without comparing it to the price\n", ref)
	}

	reg, err := c.Store.MarkPaid(ctx, ref, ev.AmountCents, c.now())
	if err != nil {
		log.Printf("Failed to update camp registration for reference %s: %v\n", ref, err)
		w.WriteHeader(http.StatusInternalServerError) // makes Paystack retry
		return
	}
	if reg == nil {
		c.explainUnsettled(ctx, ref, ev.AmountCents)
		w.WriteHeader(http.StatusOK)
		return
	}

	if ev.AmountCents != int64(reg.AmountKES)*100 && ev.AmountCents > 0 {
		log.Printf("[RECONCILE] camp reference %s: Paystack collected %d cents, expected %d\n", ref, ev.AmountCents, reg.AmountKES*100)
	}

	// A late payment on an old prompt can land after the parent retried and paid
	// again. Both are real money, so flag them for the owner to refund one.
	if n, err := c.Store.FlagDuplicates(ctx, reg.Child.Key, reg.Parent.PhoneNumber, reg.Weeks); err != nil {
		log.Printf("[DB_ERROR] camp duplicate check for %s: %v\n", ref, err)
	} else if n > 1 {
		log.Printf("[DUPLICATE_PAYMENT] camp reference %s: %q now has %d paid registrations sharing a week; one needs refunding\n", ref, reg.Child.FullName, n)
	}

	go func(reg models.CampRegistration) {
		if err := c.sendSMS(c.Config, reg); err != nil {
			log.Printf("[SMS_ERROR] camp confirmation for ref %s: %v\n", ref, err)
		}
	}(*reg)

	c.notify(ref, "Completed")
	w.WriteHeader(http.StatusOK)
}

// explainUnsettled logs why a successful charge did not settle a registration,
// because every one of these needs a person to look: either it is a repeat of
// a webhook already handled, or money was taken that the registration does not
// reflect.
func (c *CampHandler) explainUnsettled(ctx context.Context, ref string, paidCents int64) {
	status, err := c.Store.Status(ctx, ref)
	switch {
	case errors.Is(err, errUnknownReference):
		log.Printf("[RECONCILE] camp webhook for unknown reference %s (%d cents)\n", ref, paidCents)
	case err != nil:
		log.Printf("[DB_ERROR] camp status for %s: %v\n", ref, err)
	case status == camp.StatusPaid:
		log.Printf("Camp webhook ignored: reference %s already processed\n", ref)
	default:
		log.Printf("[UNDERPAID] camp reference %s (%s): Paystack collected %d cents, less than the price; registration left unpaid\n", ref, status, paidCents)
	}
}

func (c *CampHandler) fail(ctx context.Context, w http.ResponseWriter, ev paystackEvent) {
	moved, err := c.Store.MarkFailed(ctx, ev.Reference, c.now())
	if err != nil {
		log.Printf("Failed to update camp registration for reference %s: %v\n", ev.Reference, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Only tell the parent it failed when it did. A paid registration must never
	// be shown as failed by a stray event, and an already-failed one is told again
	// in case the first message was missed.
	if moved || c.statusIs(ctx, ev.Reference, camp.StatusFailed, camp.StatusExpired) {
		c.notify(ev.Reference, "Failed")
	}
	w.WriteHeader(http.StatusOK)
}

func (c *CampHandler) statusIs(ctx context.Context, ref string, statuses ...string) bool {
	got, err := c.Store.Status(ctx, ref)
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

// PaymentStatus reports a camp payment as the payment WebSocket understands it.
func (c *CampHandler) PaymentStatus(ctx context.Context, reference string) (string, error) {
	status, err := c.Store.Status(ctx, reference)
	if err != nil {
		return "", err
	}
	switch status {
	case camp.StatusPaid:
		return "Completed", nil
	case camp.StatusFailed, camp.StatusExpired:
		return "Failed", nil
	default:
		return "Pending", nil
	}
}

// campRegistrationView is what the admin dashboard receives.
type campRegistrationView struct {
	ID           string     `json:"id"`
	ChildName    string     `json:"childName"`
	ParentName   string     `json:"parentName"`
	Email        string     `json:"email"`
	PhoneNumber  string     `json:"phoneNumber"`
	Program      string     `json:"program"`
	ProgramLabel string     `json:"programLabel"`
	Lunch        bool       `json:"lunch"`
	Weeks        []string   `json:"weeks"`
	AmountKES    int        `json:"amount"`
	Status       string     `json:"status"`
	PaidAt       *time.Time `json:"paidAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	TermsVersion string     `json:"termsVersion"`
	Duplicate    bool       `json:"duplicate"`
}

// ListAdmin returns the paid and pending registrations. It must be wrapped in
// AdminOnly: it exposes children's and parents' details.
func (c *CampHandler) ListAdmin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	regs, err := c.Store.ListForAdmin(ctx)
	if err != nil {
		log.Printf("[DB_ERROR] camp list: %v", err)
		http.Error(w, "Failed to load registrations", http.StatusInternalServerError)
		return
	}

	views := make([]campRegistrationView, 0, len(regs))
	for _, reg := range regs {
		label := reg.Program
		if p, ok := camp.LookupProgram(reg.Program); ok {
			label = p.Label
		}
		weeks := reg.Weeks
		if weeks == nil {
			weeks = []string{}
		}
		views = append(views, campRegistrationView{
			ID:           reg.ID.Hex(),
			ChildName:    reg.Child.FullName,
			ParentName:   reg.Parent.FullName,
			Email:        reg.Parent.Email,
			PhoneNumber:  reg.Parent.PhoneNumber,
			Program:      reg.Program,
			ProgramLabel: label,
			Lunch:        reg.Lunch,
			Weeks:        weeks,
			AmountKES:    reg.AmountKES,
			Status:       reg.Status,
			PaidAt:       reg.Payment.PaidAt,
			CreatedAt:    reg.CreatedAt,
			TermsVersion: reg.Consent.TermsVersion,
			Duplicate:    reg.Duplicate,
		})
	}

	body, err := json.Marshal(views)
	if err != nil {
		http.Error(w, "Failed to load registrations", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// AdminOnly guards a camp handler with the shared admin API key.
func (c *CampHandler) AdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return adminOnly(c.Config, next)
}
