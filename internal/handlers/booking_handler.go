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
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

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

	// phoneLimiter stops the API being used to spam someone else's handset with
	// M-Pesa prompts.
	phoneLimiter *middleware.Limiter
	upgrader     websocket.Upgrader
	cache        bookingsCache
}

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

// validReference accepts the current 32-hex references and the 24-hex
// ObjectID references issued before they were randomised.
func validReference(s string) bool {
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
	return func(w http.ResponseWriter, r *http.Request) {
		key := h.Config.AdminAPIKey
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
			"bookingDetails.date": bson.M{"$gte": schedule.Today(time.Now())},
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
		bookedSlots = append(bookedSlots, map[string]string{
			"date":        b.BookingDetails.Date,
			"time":        b.BookingDetails.Time,
			"status":      b.BookingDetails.BookingStatus,
			"pitchType":   b.BookingDetails.PitchType,
			"phoneNumber": b.Team.PhoneNumber,
			"userName":    b.Team.FullName,
			"teamName":    b.Team.FullName,
		})
	}

	body, err := json.Marshal(bookedSlots)
	if err != nil {
		http.Error(w, "Failed to load bookings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// occupancy loads what is already on the pitch for a date.
func (h *BookingHandler) occupancy(ctx context.Context, date string) ([]schedule.Occupancy, error) {
	cursor, err := h.Collection.Find(ctx, bson.M{
		"bookingDetails.date":          date,
		"bookingDetails.bookingStatus": bson.M{"$in": occupyingStatuses},
	}, options.Find().SetProjection(bson.M{"bookingDetails.time": 1, "bookingDetails.pitchType": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var existing []models.Booking
	if err := cursor.All(ctx, &existing); err != nil {
		return nil, err
	}
	return toOccupancy(existing, date), nil
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
			start, end = 0, 24*60
		}
		out = append(out, schedule.Occupancy{Start: start, End: end, Units: units})
	}
	return out
}

func (h *BookingHandler) InitiateBooking(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	var req struct {
		UserName    string `json:"userName"`
		Email       string `json:"email"`
		PhoneNumber string `json:"phoneNumber"`
		Date        string `json:"date"`
		Time        string `json:"timeRange"`
		PitchType   string `json:"pitchType"`
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBookingBody))
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
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
	session, err := schedule.ParseSession(req.Date, req.Time, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	units, ok := schedule.Units(req.PitchType)
	if !ok {
		http.Error(w, "Invalid pitch type provided", http.StatusBadRequest)
		return
	}
	priceKES, ok := schedule.PriceKES(req.PitchType, session.Minutes())
	if !ok {
		http.Error(w, "Invalid session length", http.StatusBadRequest)
		return
	}
	amountInCents := priceKES * 100

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

	// Lock the whole date, not just this time range: a 2 hour booking and the
	// 1 hour bookings inside it have different labels but compete for the same
	// pitch, so concurrent requests for a day must be checked one at a time.
	h.Locker.Lock(session.Date)
	unlockMutex := sync.OnceFunc(func() { h.Locker.Unlock(session.Date) })
	defer unlockMutex()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	existing, err := h.occupancy(ctx, session.Date)
	if err != nil {
		log.Printf("[DB_ERROR] occupancy: %v", err)
		http.Error(w, "Database read error", http.StatusInternalServerError)
		return
	}
	if !schedule.Fits(existing, session.Start, session.End, units) {
		http.Error(w, "This slot is currently locked or already fully booked", http.StatusConflict)
		return
	}

	now := time.Now()
	newBooking := models.Booking{
		ID: primitive.NewObjectID(),
		Team: models.Team{
			FullName:    userName,
			PhoneNumber: phone,
			Email:       email,
		},
		BookingDetails: models.BookingDetails{
			Date:          session.Date,
			Time:          session.Label(),
			PitchType:     req.PitchType,
			CreatedAt:     now,
			UpdatedAt:     now,
			BookingStatus: statusPending,
		},
		Transaction: models.Transaction{
			PaymentStatus: "Pending",
			Reference:     reference,
			Amount:        priceKES,
			Timestamp:     now,
		},
	}

	if _, err := h.Collection.InsertOne(ctx, newBooking); err != nil {
		log.Printf("[DB_ERROR] insert booking: %v", err)
		http.Error(w, "Failed to initialize booking", http.StatusInternalServerError)
		return
	}
	h.cache.invalidate()

	unlockMutex()

	if _, err := services.InitiatePayment(h.Config, phone, email, amountInCents, reference); err != nil {
		filter := bson.M{"transaction.reference": reference}
		update := bson.M{
			"$set": bson.M{
				"transaction.paymentStatus":    "Failed",
				"bookingDetails.bookingStatus": "Failed",
			},
		}
		failCtx, failCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer failCancel()
		if _, uerr := h.Collection.UpdateOne(failCtx, filter, update); uerr != nil {
			log.Printf("[DB_ERROR] mark booking failed: %v", uerr)
		}
		h.cache.invalidate()

		log.Printf("[PAYMENT_ERROR]: %v", err)
		http.Error(w, "Payment gateway unavailable", http.StatusBadGateway)
		return
	}

	body, _ := json.Marshal(map[string]string{
		"message":   "Awaiting Payment",
		"reference": reference,
	})
	writeJSON(w, http.StatusCreated, body)
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
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	reference := event.Data.Reference
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
			err := services.SendConfirmationSMS(
				h.Config,
				updatedBooking.Team.PhoneNumber,
				updatedBooking.Team.FullName,
				updatedBooking.BookingDetails.Date,
				updatedBooking.BookingDetails.Time,
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

	var b models.Booking
	err := h.Collection.FindOne(ctx, bson.M{"transaction.reference": reference},
		options.FindOne().SetProjection(bson.M{"transaction.paymentStatus": 1})).Decode(&b)
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
		if errors.Is(err, mongo.ErrNoDocuments) {
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
