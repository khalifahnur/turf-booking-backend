package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/ws"

	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type BookingHandler struct {
	Collection *mongo.Collection
	Config     *config.Config
	Hub        *ws.PaymentHub
}

func (h *BookingHandler) GetBookings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{"$or": bson.A{
		bson.M{"paymentDetails.paymentStatus": "Completed"},
		bson.M{"bookingDetails.bookingStatus": bson.M{"$in": []string{"Booked", "Pending"}}},
	}}
	cursor, err := h.Collection.Find(ctx, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer cursor.Close(ctx)

	var bookings []models.Booking
	cursor.All(ctx, &bookings)

	var bookedSlots []map[string]string
	for _, b := range bookings {
		bookedSlots = append(bookedSlots, map[string]string{
			"date":      b.BookingDetails.Date,
			"time":      b.BookingDetails.Time,
			"status":    b.BookingDetails.BookingStatus,
			"pitchType": b.BookingDetails.PitchType,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(bookedSlots)
}

func (h *BookingHandler) GetBookingsAdmin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{"$or": bson.A{
		bson.M{"paymentDetails.paymentStatus": "Completed"},
		bson.M{"bookingDetails.bookingStatus": bson.M{"$in": []string{"Booked", "Pending"}}},
	}}

	findOptions := options.Find().SetSort(bson.D{{"bookingDetails.createdAt", -1}})

	cursor, err := h.Collection.Find(ctx, filter, findOptions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer cursor.Close(ctx)

	var bookings []models.Booking

	if err := cursor.All(ctx, &bookings); err != nil {
		http.Error(w, "Failed to decode bookings", http.StatusInternalServerError)
		return
	}

	var bookedSlots []map[string]string
	for _, b := range bookings {
		bookedSlots = append(bookedSlots, map[string]string{
			"date":        b.BookingDetails.Date,
			"time":        b.BookingDetails.Time,
			"status":      b.BookingDetails.BookingStatus,
			"pitchType":   b.BookingDetails.PitchType,
			"phoneNumber": b.Team.PhoneNumber,
			"userName":    b.Team.TeamName,
			"teamName":    b.Team.FullName,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(bookedSlots)
}

func ValidateDateTime(dateStr, timeRangeStr string) error {
	normalizedTimeStr := strings.ReplaceAll(timeRangeStr, "–", "-")
	timestr := strings.Split(normalizedTimeStr, "-")

	if len(timestr) != 2 {
		return errors.New("invalid format")
	}

	startTimeStr := strings.TrimSpace(timestr[0])
	endTimeStr := strings.TrimSpace(timestr[1])

	startDateTime := fmt.Sprintf("%s %s", dateStr, startTimeStr)
	endDateTime := fmt.Sprintf("%s %s", dateStr, endTimeStr)

	layout := "2006-01-02 15:04"

	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		loc = time.UTC
	}
	startTime, err := time.ParseInLocation(layout, startDateTime, loc)
	if err != nil {
		return fmt.Errorf("failed to parse start time: %w", err)
	}

	endTime, err := time.ParseInLocation(layout, endDateTime, loc)
	if err != nil {
		return fmt.Errorf("failed to parse end time: %w", err)
	}

	if !endTime.After(startTime) {
		return errors.New("booking end time must be after start time")
	}

	now := time.Now().In(loc)

	timeRemaining := endTime.Sub(now)

	if timeRemaining < 30*time.Minute {
		return errors.New("booking closed: less than 30 minutes remaining in this slot")
	}

	return nil
}

func (h *BookingHandler) InitiateBooking(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserName    string `json:"userName"`
		TeamName    string `json:"teamName"`
		PhoneNumber string `json:"phoneNumber"`
		Date        string `json:"date"`
		Time        string `json:"timeRange"`
		PitchType   string `json:"pitchType"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	if err := ValidateDateTime(req.Date, req.Time); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var amountInCents int
	switch req.PitchType {
	case "8Aside":
		amountInCents = 12000 * 100
	case "5Aside":
		amountInCents = 6500 * 100
	default:
		http.Error(w, "Invalid pitch type provided", http.StatusBadRequest)
		return
	}

	existingBookingFilter := bson.M{
		"bookingDetails.date":          req.Date,
		"bookingDetails.time":          req.Time,
		"bookingDetails.pitchType":     req.PitchType,
		"bookingDetails.bookingStatus": bson.M{"$in": []string{"Pending", "Confirmed"}},
	}

	var existingBooking models.Booking
	err := h.Collection.FindOne(r.Context(), existingBookingFilter).Decode(&existingBooking)

	if err == nil {
		http.Error(w, "This slot is currently locked or already booked", http.StatusConflict)
		return
	} else if err != mongo.ErrNoDocuments {
		log.Printf("Database error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	bookingID := primitive.NewObjectID()
	reference := bookingID.Hex()

	newBooking := models.Booking{
		ID: bookingID,
		Team: models.Team{
			FullName:    req.UserName,
			TeamName:    req.TeamName,
			PhoneNumber: req.PhoneNumber,
		},
		BookingDetails: models.BookingDetails{
			Date:          req.Date,
			Time:          req.Time,
			PitchType:     req.PitchType,
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
			BookingStatus: "Pending",
		},
		Transaction: models.Transaction{
			PaymentStatus: "Pending",
			Reference:     reference,
			Timestamp:     time.Now(),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := h.Collection.InsertOne(ctx, newBooking); err != nil {
		http.Error(w, "Failed to initialize booking", http.StatusInternalServerError)
		return
	}

	email := "khalifahnur1095@gmail.com"

	_, err = services.InitiatePayment(h.Config, req.PhoneNumber, email, amountInCents, reference)

	if err != nil {
		filter := bson.M{"transaction.reference": reference}
		update := bson.M{
			"$set": bson.M{
				"transaction.paymentStatus":    "Failed",
				"bookingDetails.bookingStatus": "Failed",
			},
		}

		h.Collection.UpdateOne(ctx, filter, update)

		log.Printf("[PAYMENT_ERROR]: %v", err)
		http.Error(w, "Payment gateway unavailable", http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"message":   "Awaiting Payment",
		"reference": reference,
	})
}

func (h *BookingHandler) PaystackWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Unable to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	paystackSignature := r.Header.Get("x-paystack-signature")
	mac := hmac.New(sha512.New, []byte(h.Config.PaystackSK))
	mac.Write(body)
	expectedSignature := hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(paystackSignature), []byte(expectedSignature)) != 1 {
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
				"bookingDetails.bookingStatus": "Booked",
			},
		}

		var updatedBooking struct {
			Customer struct {
				Name  string `bson:"name"`
				Phone string `bson:"phone"`
			} `bson:"customer"`
			BookingDetails struct {
				Date     string `bson:"date"`
				TimeSlot string `bson:"timeSlot"`
			} `bson:"bookingDetails"`
		}

		opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
		err := h.Collection.FindOneAndUpdate(r.Context(), filter, update, opts).Decode(&updatedBooking)

		if err != nil {
			if err == mongo.ErrNoDocuments {
				log.Printf("Webhook ignored: Reference %s already processed or not found\n", reference)
				w.WriteHeader(http.StatusOK)
				return
			}
			log.Printf("Failed to update DB for reference %s: %v\n", reference, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		h.Hub.NotifyClient(reference, "Completed")

		// go services.SendConfirmationSMS(
		// 	h.Config,
		// 	updatedBooking.Customer.Phone,
		// 	updatedBooking.Customer.Name,
		// 	updatedBooking.BookingDetails.Date,
		// 	updatedBooking.BookingDetails.TimeSlot,
		// )
	}

	h.Hub.NotifyClient(reference, "Failed")
	w.WriteHeader(http.StatusOK)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func (h *BookingHandler) WSStatusHandler(w http.ResponseWriter, r *http.Request) {
	reference := r.URL.Query().Get("reference")
	if reference == "" {
		http.Error(w, "Reference is required", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	h.Hub.AddClient(reference, conn)
	defer h.Hub.RemoveClient(reference)

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}
