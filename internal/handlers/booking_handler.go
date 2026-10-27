package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"turf-booking-backend/internal/camp"
	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/utils"
	"turf-booking-backend/internal/validate"
	"turf-booking-backend/internal/ws"

	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	maxBookingBody = 4 << 10 // a booking request is a few hundred bytes
	maxWebhookBody = 1 << 20

	// Statuses that hold a place on the pitch. "Booked" is what the webhook
	// writes once a payment lands; "Confirmed" only exists in older rows.
	statusPending   = "Pending"
	statusBooked    = "Booked"
	statusConfirmed = "Confirmed"

	wsPongWait  = 60 * time.Second
	wsPingEvery = 25 * time.Second
	wsMaxLife   = 5 * time.Minute
)

var occupyingStatuses = []string{statusPending, statusBooked, statusConfirmed}

type BookingHandler struct {
	Collection *mongo.Collection
	Config     *config.Config
	Hub        *ws.PaymentHub
	Locker     *utils.SlotLocker

	// store is what taking a booking reads and writes; members is the member
	// list, and leaving it nil switches the member discount off.
	store   bookingStore
	members MemberStore

	// phoneLimiter stops the API being used to spam someone else's handset with
	// M-Pesa prompts.
	phoneLimiter *middleware.Limiter
	upgrader     websocket.Upgrader
	cache        bookingsCache

	// camp shares this handler's Paystack webhook and payment WebSocket.
	camp *CampHandler

	// Replaceable so tests can run the whole booking flow without Paystack or a
	// real clock.
	pay func(cfg *config.Config, phone, email string, amountCents int, reference string) (string, error)
	now func() time.Time
}

// AttachCamp lets the webhook and the payment WebSocket serve camp payments
// as well as pitch bookings.
func (h *BookingHandler) AttachCamp(c *CampHandler) { h.camp = c }

// AttachMembers turns the member discount on, for the numbers on this list.
func (h *BookingHandler) AttachMembers(m MemberStore) { h.members = m }

func NewBookingHandler(collection *mongo.Collection, cfg *config.Config, hub *ws.PaymentHub) *BookingHandler {
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = struct{}{}
	}
	return &BookingHandler{
		Collection:   collection,
		Config:       cfg,
		Hub:          hub,
		Locker:       utils.NewSlotLocker(),
		store:        &mongoBookingStore{c: collection},
		pay:          services.InitiatePayment,
		now:          time.Now,
		phoneLimiter: middleware.NewLimiter(0.3, 3), // 3 attempts, then one every ~3 minutes
		upgrader: websocket.Upgrader{
			ReadBufferSize:   1024,
			WriteBufferSize:  1024,
			HandshakeTimeout: 10 * time.Second,
			// Browsers always send Origin on a WebSocket handshake, so a page on
			// another site cannot open this socket on a visitor's behalf.
			// Non-browser clients send none and gain nothing from it.
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				_, ok := allowed[origin]
				return ok
			},
		},
	}
}

// bookingsCache lets the 3-second polling from every open browser tab share a
// single database query. It is invalidated on every write, so a booking shows
// up immediately for everyone rather than after the TTL.
type bookingsCache struct {
	mu      sync.Mutex
	body    []byte
	expires time.Time
}

const bookingsCacheTTL = 2 * time.Second

func (c *bookingsCache) load(fill func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && time.Now().Before(c.expires) {
		return c.body, nil
	}
	body, err := fill()
	if err != nil {
		return nil, err
	}
	c.body, c.expires = body, time.Now().Add(bookingsCacheTTL)
	return body, nil
}

func (c *bookingsCache) invalidate() {
	c.mu.Lock()
	c.body = nil
	c.mu.Unlock()
}

func newReference() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validReference accepts the current 32-hex references, the 24-hex ObjectID
// references issued before they were randomised, and camp references
// ("camp-" plus 32 hex).
func validReference(s string) bool {
	s = strings.TrimPrefix(s, camp.ReferencePrefix)
	if len(s) != 32 && len(s) != 24 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// AdminOnly gates a handler behind the shared admin API key. The key is held
// only by the Next.js server, which checks the admin's session before calling.
func (h *BookingHandler) AdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return adminOnly(h.Config, next)
}

// adminOnly is the bearer-key check shared by every admin endpoint.
func adminOnly(cfg *config.Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := cfg.AdminAPIKey
		if key == "" {
			http.Error(w, "Admin API is not enabled", http.StatusServiceUnavailable)
			return
		}
		token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || !secretEqual(token, key) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// secretEqual compares via fixed-length digests so neither the length nor the
// content of the real secret leaks through timing.
func secretEqual(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (h *BookingHandler) GetBookings(w http.ResponseWriter, r *http.Request) {
	body, err := h.cache.load(func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		filter := bson.M{
			// The operating day is still the previous date until the venue closes
			// in the small hours, so its late sessions stay on the calendar.
			"bookingDetails.date": bson.M{"$gte": schedule.OperatingDate(h.now())},
			"$or": bson.A{
				bson.M{"paymentDetails.paymentStatus": "Completed"},
				bson.M{"bookingDetails.bookingStatus": bson.M{"$in": []string{statusBooked, statusPending}}},
			},
		}
		findOptions := options.Find().SetSort(bson.D{{Key: "bookingDetails.date", Value: 1}, {Key: "bookingDetails.time", Value: 1}})

		cursor, err := h.Collection.Find(ctx, filter, findOptions)
		if err != nil {
			return nil, err
		}
		defer cursor.Close(ctx)

		var bookings []models.Booking
		if err := cursor.All(ctx, &bookings); err != nil {
			return nil, err
		}

		// Public: no names, phone numbers or emails, only what the calendar needs.
		bookedSlots := make([]map[string]string, 0, len(bookings))
		for _, b := range bookings {
			bookedSlots = append(bookedSlots, map[string]string{
				"date":      b.BookingDetails.Date,
				"time":      b.BookingDetails.Time,
				"status":    b.BookingDetails.BookingStatus,
				"pitchType": b.BookingDetails.PitchType,
			})
		}
		return json.Marshal(bookedSlots)
	})
	if err != nil {
		log.Printf("[DB_ERROR] GetBookings: %v", err)
		http.Error(w, "Failed to load bookings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// adminBookingView is a booking as the admin dashboard lists it.
func adminBookingView(b models.Booking) map[string]string {
	view := map[string]string{
		"date":        b.BookingDetails.Date,
		"time":        b.BookingDetails.Time,
		"status":      b.BookingDetails.BookingStatus,
		"pitchType":   b.BookingDetails.PitchType,
		"phoneNumber": b.Team.PhoneNumber,
		"userName":    b.Team.FullName,
		"teamName":    b.Team.FullName,
	}
	// What was charged, and the discount that made it less than the list price.
	// Bookings from before prices were recorded have neither.
	if b.Transaction.Amount > 0 {
		view["amount"] = strconv.Itoa(b.Transaction.Amount)
	}
	if b.Transaction.Discount != "" {
		view["discount"] = b.Transaction.Discount
		view["listAmount"] = strconv.Itoa(b.Transaction.ListAmount)
	}
	return view
}

func (h *BookingHandler) GetBookingsAdmin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	filter := bson.M{"$or": bson.A{
		bson.M{"paymentDetails.paymentStatus": "Completed"},
		bson.M{"bookingDetails.bookingStatus": bson.M{"$in": []string{statusBooked, statusPending}}},
	}}
	findOptions := options.Find().SetSort(bson.D{{Key: "bookingDetails.createdAt", Value: -1}})

	cursor, err := h.Collection.Find(ctx, filter, findOptions)
	if err != nil {
		log.Printf("[DB_ERROR] GetBookingsAdmin: %v", err)
		http.Error(w, "Failed to load bookings", http.StatusInternalServerError)
		return
	}
	defer cursor.Close(ctx)

	var bookings []models.Booking
	if err := cursor.All(ctx, &bookings); err != nil {
		log.Printf("[DB_ERROR] GetBookingsAdmin decode: %v", err)
		http.Error(w, "Failed to load bookings", http.StatusInternalServerError)
		return
	}

	bookedSlots := make([]map[string]string, 0, len(bookings))
	for _, b := range bookings {
		bookedSlots = append(bookedSlots, adminBookingView(b))
	}

	body, err := json.Marshal(bookedSlots)
	if err != nil {
		http.Error(w, "Failed to load bookings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// toOccupancy converts stored bookings into pitch usage. Anything it cannot
// understand is treated as taking the most it possibly could, so bad data
// blocks a slot instead of letting it be double-booked.
func toOccupancy(bookings []models.Booking, date string) []schedule.Occupancy {
	out := make([]schedule.Occupancy, 0, len(bookings))
	for _, b := range bookings {
		units, ok := schedule.Units(b.BookingDetails.PitchType)
		if !ok {
			units = schedule.PitchCapacity
		}
		start, end, ok := schedule.ParseStoredRange(b.BookingDetails.Time)
		if !ok {
			log.Printf("[DATA] unreadable stored time %q on %s; blocking the date", b.BookingDetails.Time, date)
			start, end = 0, schedule.DayEnd
		}
		out = append(out, schedule.Occupancy{Start: start, End: end, Units: units})
	}
	return out
}

// readJSON reads the one JSON object a booking request carries. When it cannot,
// it says why on w and returns false.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBookingBody))
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return false
	}
	if dec.More() {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return false
	}
	return true
}

// memberLockKey is the lock that makes counting a member's discounted sessions
// and booking one more a single step for that number.
func memberLockKey(phone string) string { return "member:" + phone }

func (h *BookingHandler) InitiateBooking(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserName    string `json:"userName"`
		Email       string `json:"email"`
		PhoneNumber string `json:"phoneNumber"`
		Date        string `json:"date"`
		Time        string `json:"timeRange"`
		PitchType   string `json:"pitchType"`
		// ExpectedAmount is the price the customer was shown. If it is sent and
		// the price is no longer that (a member's discounted sessions for the
		// month ran out in another tab, say) the booking is refused, so nobody is
		// charged an amount they have not seen.
		ExpectedAmount *int `json:"expectedAmount"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	userName, err := validate.Name(req.UserName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	email, err := validate.Email(req.Email)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	phone, err := validate.KenyanMobile(req.PhoneNumber)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The session, its length and therefore its price all come from the server.
	session, err := schedule.ParseSession(req.Date, req.Time, h.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	units, ok := schedule.Units(req.PitchType)
	if !ok {
		http.Error(w, "Invalid pitch type provided", http.StatusBadRequest)
		return
	}
	listKES, ok := schedule.PriceKES(req.PitchType, session.Minutes())
	if !ok {
		http.Error(w, "Invalid session length", http.StatusBadRequest)
		return
	}

	if allowed, wait := h.phoneLimiter.Allow(phone); !allowed {
		middleware.TooManyRequests(w, wait)
		return
	}

	reference, err := newReference()
	if err != nil {
		log.Printf("[RAND_ERROR] %v", err)
		http.Error(w, "Failed to initialize booking", http.StatusInternalServerError)
		return
	}

	// A member's discounted sessions are counted and then one more is booked, so
	// that has to happen one request at a time for that number: otherwise two
	// requests sent together would both see "3 of 4 used". This lock is always
	// taken before the date lock below, never after, so the two cannot deadlock.
	b := bookingRequest{
		userName: userName, email: email, phone: phone, pitchType: req.PitchType,
		session: session, units: units, listKES: listKES, reference: reference,
		expected: req.ExpectedAmount, releaseMember: func() {},
	}
	if h.members != nil && schedule.MemberEligible(req.PitchType, session.Minutes()) {
		key := memberLockKey(phone)
		h.Locker.Lock(key)
		b.releaseMember = sync.OnceFunc(func() { h.Locker.Unlock(key) })
		defer b.releaseMember()
	}
	h.finishBooking(w, r, b)
}

// bookingRequest is a booking that has passed validation.
type bookingRequest struct {
	userName, email, phone string
	pitchType              string
	session                schedule.Session
	units                  int
	listKES                int
	reference              string
	// expected is the price the customer was shown, when they sent one.
	expected *int
	// releaseMember lets go of the member's lock, if this booking holds one.
	releaseMember func()
}

// finishBooking prices the booking, checks the pitch is free, records it as
// Pending and sends the M-Pesa prompt. A member's lock, if one is held, is
// released as soon as the booking is recorded.
func (h *BookingHandler) finishBooking(w http.ResponseWriter, r *http.Request, b bookingRequest) {
	priceCtx, cancelPrice := context.WithTimeout(r.Context(), 5*time.Second)
	price, err := h.priceBooking(priceCtx, b.phone, b.pitchType, b.session, b.listKES)
	cancelPrice()
	if err != nil {
		log.Printf("[DB_ERROR] pricing: %v", err)
		http.Error(w, "Database read error", http.StatusInternalServerError)
		return
	}
	if b.expected != nil && *b.expected != price.Charge {
		// Nothing was sent to the customer's phone, so this does not count as an attempt.
		h.phoneLimiter.Refund(b.phone)
		middleware.UserError(w, fmt.Sprintf(
			"The price for this session is now %s, not %s. Please check the total and confirm again.",
			schedule.FormatKES(price.Charge), schedule.FormatKES(*b.expected)), http.StatusConflict)
		return
	}

	// Lock the whole date, not just this time range: a 2 hour booking and the
	// 1 hour bookings inside it have different labels but compete for the same
	// pitch, so concurrent requests for a day must be checked one at a time.
	h.Locker.Lock(b.session.Date)
	unlockMutex := sync.OnceFunc(func() { h.Locker.Unlock(b.session.Date) })
	defer unlockMutex()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	existing, err := h.store.Occupancy(ctx, b.session.Date)
	if err != nil {
		log.Printf("[DB_ERROR] occupancy: %v", err)
		http.Error(w, "Database read error", http.StatusInternalServerError)
		return
	}
	if !schedule.Fits(existing, b.session.Start, b.session.End, b.units) {
		http.Error(w, "This slot is currently locked or already fully booked", http.StatusConflict)
		return
	}

	now := h.now()
	newBooking := models.Booking{
		ID: primitive.NewObjectID(),
		Team: models.Team{
			FullName:    b.userName,
			PhoneNumber: b.phone,
			Email:       b.email,
		},
		BookingDetails: models.BookingDetails{
			Date:          b.session.Date,
			Time:          b.session.Label(),
			PitchType:     b.pitchType,
			CreatedAt:     now,
			UpdatedAt:     now,
			BookingStatus: statusPending,
		},
		Transaction: models.Transaction{
			PaymentStatus: "Pending",
			Reference:     b.reference,
			Amount:        price.Charge,
			Timestamp:     now,
		},
	}
	if price.Applied() {
		newBooking.Transaction.Discount = models.DiscountMember
		newBooking.Transaction.ListAmount = price.List
	}

	if err := h.store.Insert(ctx, newBooking); err != nil {
		log.Printf("[DB_ERROR] insert booking: %v", err)
		http.Error(w, "Failed to initialize booking", http.StatusInternalServerError)
		return
	}
	h.cache.invalidate()

	// The booking is recorded, so the pitch and the member's count are both
	// settled; the payment call below can take a while and must not hold them.
	unlockMutex()
	b.releaseMember()

	if _, err := h.pay(h.Config, b.phone, b.email, price.Charge*100, b.reference); err != nil {
		failCtx, failCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer failCancel()
		logMarkFailed(h.store.MarkFailed(failCtx, b.reference))
		h.cache.invalidate()

		log.Printf("[PAYMENT_ERROR]: %v", err)
		http.Error(w, "Payment gateway unavailable", http.StatusBadGateway)
		return
	}
	if price.Applied() {
		log.Printf("[MEMBER] member price %d (list %d) for booking %s", price.Charge, price.List, b.reference)
	}

	body, _ := json.Marshal(initiateResponse{
		Message:   "Awaiting Payment",
		Reference: b.reference,
		Amount:    price.Charge,
		Discount:  newBooking.Transaction.Discount,
	})
	writeJSON(w, http.StatusCreated, body)
}

// initiateResponse is what a started booking answers with. Amount is what the
// customer's phone is being asked for.
type initiateResponse struct {
	Message   string `json:"message"`
	Reference string `json:"reference"`
	Amount    int    `json:"amount"`
	Discount  string `json:"discount,omitempty"`
}

func (h *BookingHandler) PaystackWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "Unable to read body", http.StatusBadRequest)
		return
	}

	// Config.Validate guarantees the key is non-empty; with an empty key anyone
	// could compute a valid signature.
	signature, err := hex.DecodeString(r.Header.Get("x-paystack-signature"))
	if err != nil {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}
	mac := hmac.New(sha512.New, []byte(h.Config.PaystackSK))
	mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	var event struct {
		Event string `json:"event"`
		Data  struct {
			Reference string `json:"reference"`
			Status    string `json:"status"`
			Amount    int64  `json:"amount"`
			Currency  string `json:"currency"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	reference := event.Data.Reference

	// Camp registrations share this webhook and are told apart by their prefix.
	if camp.IsReference(reference) {
		if h.camp == nil {
			log.Printf("Webhook for camp reference %s ignored: camp handler not attached\n", reference)
			w.WriteHeader(http.StatusOK)
			return
		}
		h.camp.HandlePaystackEvent(w, r, paystackEvent{
			Event:       event.Event,
			Reference:   reference,
			Status:      event.Data.Status,
			Currency:    event.Data.Currency,
			AmountCents: event.Data.Amount,
		})
		return
	}

	if event.Event == "charge.success" {

		filter := bson.M{
			"transaction.reference":     reference,
			"transaction.paymentStatus": bson.M{"$ne": "Completed"},
		}

		update := bson.M{
			"$set": bson.M{
				"transaction.paymentStatus":    "Completed",
				"bookingDetails.bookingStatus": statusBooked,
			},
		}

		var updatedBooking models.Booking

		// Paystack may hang up before the write returns; the booking must still
		// be recorded, so the write does not follow the request's cancellation.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()

		opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
		err := h.Collection.FindOneAndUpdate(ctx, filter, update, opts).Decode(&updatedBooking)

		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				log.Printf("Webhook ignored: Reference %s already processed or not found\n", reference)
				w.WriteHeader(http.StatusOK)
				return
			}
			log.Printf("Failed to update DB for reference %s: %v\n", reference, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h.cache.invalidate()

		go func() {
			when, slot := schedule.SMSWhen(updatedBooking.BookingDetails.Date, updatedBooking.BookingDetails.Time)
			err := services.SendConfirmationSMS(
				h.Config,
				updatedBooking.Team.PhoneNumber,
				updatedBooking.Team.FullName,
				when,
				slot,
			)
			if err != nil {
				log.Printf("[SMS_ERROR] Failed to send SMS for ref %s: %v\n", reference, err)
			}
		}()

		h.Hub.NotifyClient(reference, "Completed")
		w.WriteHeader(http.StatusOK)
		return

	}

	h.Hub.NotifyClient(reference, "Failed")
	w.WriteHeader(http.StatusOK)
}

func (h *BookingHandler) paymentStatus(reference string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if camp.IsReference(reference) {
		if h.camp == nil {
			return "", errUnknownReference
		}
		return h.camp.PaymentStatus(ctx, reference)
	}

	var b models.Booking
	err := h.Collection.FindOne(ctx, bson.M{"transaction.reference": reference},
		options.FindOne().SetProjection(bson.M{"transaction.paymentStatus": 1})).Decode(&b)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errUnknownReference
	}
	if err != nil {
		return "", err
	}
	return b.Transaction.PaymentStatus, nil
}

func (h *BookingHandler) WSStatusHandler(w http.ResponseWriter, r *http.Request) {
	reference := r.URL.Query().Get("reference")
	if !validReference(reference) {
		http.Error(w, "A valid reference is required", http.StatusBadRequest)
		return
	}

	// Only a real, known payment may hold a socket open.
	if _, err := h.paymentStatus(reference); err != nil {
		if errors.Is(err, errUnknownReference) {
			http.Error(w, "Unknown reference", http.StatusNotFound)
		} else {
			log.Printf("[DB_ERROR] ws lookup: %v", err)
			http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		}
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	conn.SetReadLimit(512) // clients never need to send us anything meaningful

	handle, ok := h.Hub.AddClient(reference, conn)
	if !ok {
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "busy"),
			time.Now().Add(time.Second))
		return
	}
	defer handle.Remove()

	// The payment can settle between the client getting its reference and this
	// socket opening, in which case the webhook had nobody to tell. Checking
	// after registering closes that window.
	if status, err := h.paymentStatus(reference); err == nil && (status == "Completed" || status == "Failed") {
		handle.Send(reference, status)
		return
	}

	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(wsPingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if handle.Ping() != nil {
					conn.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()
	defer time.AfterFunc(wsMaxLife, func() { conn.Close() }).Stop()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}
