package main

import (
	"log"
	"net/http"

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/db"
	"turf-booking-backend/internal/handlers"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/ws"
)

func main() {
	cfg := config.LoadConfig()

	port := cfg.Port
	if port == "" {
		port = "8080"
	}

	mongoURI := cfg.MongoURI
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}

	mongoClient := db.Connect(mongoURI)
	collection := mongoClient.Database("arena").Collection("bookings")

	hub := ws.NewPaymentHub()

	services.StartCleanupWorker(collection)

	bookingHandler := &handlers.BookingHandler{
		Collection: collection,
		Config:     cfg,
		Hub:        hub,
	}

	mux := http.NewServeMux()

	corsMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}

	mux.HandleFunc("GET /api/v1/bookings", bookingHandler.GetBookings)
	mux.HandleFunc("GET /api/v1/admin/bookings", bookingHandler.GetBookingsAdmin)
	mux.HandleFunc("POST /api/v1/initiate/paystack/push-stk", bookingHandler.InitiateBooking)
	mux.HandleFunc("POST /api/v1/webhook/paystack", bookingHandler.PaystackWebhook)
	mux.HandleFunc("/ws/v1/payment-status", bookingHandler.WSStatusHandler)

	log.Printf("Server starting on port %s...\n", port)
	log.Fatal(http.ListenAndServe(":"+port, corsMiddleware(mux)))
}
