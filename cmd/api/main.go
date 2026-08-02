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

			// 1. Log the exact origin to your Railway logs to see what Vercel is actually sending
			if origin != "" {
				log.Printf("[CORS DEBUG] Method: %s | Origin: '%s' | Path: %s", r.Method, origin, r.URL.Path)
			}

			// 2. The "Echo Strategy": Reflect the origin dynamically to bypass strict matching for now
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			} else {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}

			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")

			// 3. Expanded Headers: Next.js/Vercel sometimes attach extra headers implicitly.
			// We must allow them all.
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, Authorization, X-CSRF-Token")
			w.Header().Set("Vary", "Origin")

			if r.Method == http.MethodOptions {
				// 4. Use 204 (No Content) instead of 200. Browsers prefer 204 for OPTIONS preflights.
				w.WriteHeader(http.StatusNoContent)
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
