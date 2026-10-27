package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // Africa/Nairobi must resolve even in a minimal container

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/db"
	"turf-booking-backend/internal/handlers"
	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/schedule"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/ws"
)

func main() {
	cfg := config.LoadConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Refusing to start: %v", err)
	}
	if cfg.AdminAPIKey == "" {
		log.Println("ADMIN_API_KEY is not set: the admin endpoints (bookings and camp registrations) are disabled")
	}

	configureHolidays(cfg)

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
	db.EnsureIndexes(collection)

	hub := ws.NewPaymentHub()

	services.StartCleanupWorker(collection)

	bookingHandler := handlers.NewBookingHandler(collection, cfg, hub)

	campCollection := mongoClient.Database("arena").Collection("camp_registrations")
	db.EnsureCampIndexes(campCollection)
	services.StartCampCleanupWorker(campCollection)
	campHandler := handlers.NewCampHandler(handlers.NewMongoCampStore(campCollection), cfg, hub)
	// The Paystack webhook and the payment WebSocket are shared with the camp.
	bookingHandler.AttachCamp(campHandler)

	// One bucket per client IP for each class of endpoint. The numbers leave
	// plenty of room for a busy shared network (the calendar polls every 3s) but
	// stop a single source from hammering the database or the payment gateway.
	hops := cfg.TrustedProxyHops
	readLimit := middleware.NewLimiter(300, 60)
	bookLimit := middleware.NewLimiter(6, 5)
	wsLimit := middleware.NewLimiter(30, 10)
	adminLimit := middleware.NewLimiter(30, 10)
	campLimit := middleware.NewLimiter(20, 10)

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/schedule", readLimit.ByIP(hops, http.HandlerFunc(handlers.NewScheduleHandler().Get)))
	mux.Handle("GET /api/v1/bookings", readLimit.ByIP(hops, http.HandlerFunc(bookingHandler.GetBookings)))
	mux.Handle("GET /api/v1/admin/bookings", adminLimit.ByIP(hops, bookingHandler.AdminOnly(bookingHandler.GetBookingsAdmin)))
	mux.Handle("POST /api/v1/initiate/paystack/push-stk", bookLimit.ByIP(hops, http.HandlerFunc(bookingHandler.InitiateBooking)))
	mux.Handle("POST /api/v1/camp/register", campLimit.ByIP(hops, http.HandlerFunc(campHandler.Register)))
	mux.Handle("GET /api/v1/admin/camp/registrations", adminLimit.ByIP(hops, campHandler.AdminOnly(campHandler.ListAdmin)))
	// Authenticated by Paystack's HMAC signature, so it is not IP-limited:
	// legitimate retries must never be throttled.
	mux.HandleFunc("POST /api/v1/webhook/paystack", bookingHandler.PaystackWebhook)
	mux.Handle("GET /ws/v1/payment-status", wsLimit.ByIP(hops, http.HandlerFunc(bookingHandler.WSStatusHandler)))

	handler := middleware.Recover(middleware.SecurityHeaders(middleware.CORS(cfg.AllowedOrigins, mux)))

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-stop
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Forced shutdown: %v", err)
		}
		mongoClient.Disconnect(ctx)
	}()

	log.Printf("Server starting on port %s...\n", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// ListenAndServe returns as soon as Shutdown begins; wait for requests in
	// flight (e.g. a payment webhook) to finish before the process exits.
	<-drained
}

// configureHolidays adds the holidays set with EXTRA_HOLIDAYS to the built-in
// Kenyan calendar, and says in the log what the calendar will do, so a mistake
// in the setting shows up when the server starts rather than on a holiday.
func configureHolidays(cfg *config.Config) {
	extra, problems := schedule.ParseHolidays(cfg.ExtraHolidays)
	for _, p := range problems {
		log.Printf("[CONFIG] EXTRA_HOLIDAYS entry ignored, %s", p)
	}
	schedule.SetExtraHolidays(extra)

	now := time.Now()
	upcoming := schedule.HolidaysBetween(now, now.AddDate(0, 0, 120))
	if len(upcoming) == 0 {
		log.Println("[SCHEDULE] no public holidays in the next 120 days")
	}
	for _, h := range upcoming {
		log.Printf("[SCHEDULE] public holiday (weekend hours): %s %s", h.Date, h.Name)
	}
	// Eid-ul-Fitr and Eid-ul-Adha are announced a day or two before, so they are
	// the one part of the calendar that needs a person's attention each year.
	if last := schedule.LastDeclared(); last == "" || last < now.AddDate(0, 0, 90).Format("2006-01-02") {
		log.Printf("[SCHEDULE] Eid-ul-Fitr and Eid-ul-Adha are only known up to %q: when the Interior Cabinet Secretary gazettes the next ones, add them to EXTRA_HOLIDAYS (e.g. EXTRA_HOLIDAYS=2027-03-09=Eid-ul-Fitr)", last)
	}
}
