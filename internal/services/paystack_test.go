package services

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"turf-booking-backend/internal/config"
)

// A failure after which the prompt may still have been sent must say so, because
// the camp tells the parent not to retry blindly; a definite refusal must not.
func TestInitiatePaymentClassifiesFailures(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		uncertain bool
		wantErr   bool
	}{
		{"accepted", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"status":true,"message":"ok","data":{"reference":"r","status":"pay_offline","display_text":"Enter PIN"}}`))
		}, false, false},
		{"rejected request (400)", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"status":false,"message":"Invalid phone"}`, http.StatusBadRequest)
		}, false, true},
		{"bad key (401)", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"status":false}`, http.StatusUnauthorized)
		}, false, true},
		{"status false with 200", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"status":false,"message":"declined"}`))
		}, false, true},
		{"server error (500)", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, true, true},
		{"bad gateway (502)", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}, true, true},
		{"unreadable success body", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		}, true, true},
		{"timeout", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.Write([]byte(`{"status":true}`))
		}, true, true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(c.handler)
		prevURL, prevTimeout := paystackChargeURL, paystackTimeout
		paystackChargeURL, paystackTimeout = srv.URL, 100*time.Millisecond
		_, err := InitiatePayment(&config.Config{PaystackSK: "sk"}, "254712345678", "a@b.co", 250000, "camp-x")
		paystackChargeURL, paystackTimeout = prevURL, prevTimeout
		srv.Close()

		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error = %v", c.name, err, c.wantErr)
			continue
		}
		if got := errors.Is(err, ErrPaymentUncertain); got != c.uncertain {
			t.Errorf("%s: uncertain = %v, want %v (%v)", c.name, got, c.uncertain, err)
		}
	}

	// A connection that cannot even be opened may or may not have reached Paystack.
	prevURL := paystackChargeURL
	paystackChargeURL = "http://127.0.0.1:1/charge"
	_, err := InitiatePayment(&config.Config{PaystackSK: "sk"}, "254712345678", "a@b.co", 250000, "camp-x")
	paystackChargeURL = prevURL
	if !errors.Is(err, ErrPaymentUncertain) {
		t.Errorf("a network error must be reported as uncertain, got %v", err)
	}
}
