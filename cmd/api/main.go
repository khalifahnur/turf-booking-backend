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
			origin := r.Header.Get("Origin")

			allowedOrigins := []string{
				"https://karena.stsebastiansportsacademy.com",
				//"http://localhost:3000",
			}

			isAllowed := false
			for _, o := range allowedOrigins {
				if origin == o {
					isAllowed = true
					break
				}
			}
			w.Header().Add("Vary", "Origin")

			if isAllowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
			}

			if r.Method == http.MethodOptions {
				if isAllowed {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusForbidden)
				}
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

	handler := corsMiddleware(mux)
	log.Printf("Server starting on port %s...\n", port)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}
