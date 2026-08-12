package services

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"turf-booking-backend/internal/config"
)

func FormatPhoneE164(phone string) string {
	cleaned := strings.TrimSpace(phone)
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	cleaned = strings.ReplaceAll(cleaned, "-", "")

	if strings.HasPrefix(cleaned, "+") {
		return cleaned
	}

	if strings.HasPrefix(cleaned, "254") {
		return "+" + cleaned
	}

	if strings.HasPrefix(cleaned, "0") {
		return "+254" + cleaned[1:]
	}

	if len(cleaned) == 9 && (strings.HasPrefix(cleaned, "7") || strings.HasPrefix(cleaned, "1")) {
		return "+254" + cleaned
	}

	return cleaned
}

func SendConfirmationSMS(cfg *config.Config, phone, name, date, timeSlot string) error {
	formattedPhone := FormatPhoneE164(phone)

	if formattedPhone == "" {
		return fmt.Errorf("cannot send SMS: phone number is empty")
	}

	directionsLink := "https://maps.app.goo.gl/uBNLHePXgTiTciC5A"
	message := fmt.Sprintf("Confirmed! %s, your pitch is booked @ %s Date: %s. Directions: %s", name, timeSlot, date, directionsLink)

	data := url.Values{}
	data.Set("username", cfg.ATUsername)
	data.Set("to", formattedPhone)
	data.Set("message", message)

	if cfg.ATUsername != "sandbox" && cfg.ATSenderID != "" {
		data.Set("from", cfg.ATSenderID)
	}

	endpoint := "https://api.africastalking.com/version1/messaging"
	if cfg.ATUsername == "sandbox" {
		endpoint = "https://api.sandbox.africastalking.com/version1/messaging"
	}

	req, err := http.NewRequest("POST", endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("apiKey", cfg.ATAPIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute SMS request: %w", err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return fmt.Errorf("africa's talking rejected request (status %d): %s", res.StatusCode, string(body))
	}

	log.Printf("[AT RESPONSE %d]: %s", res.StatusCode, string(body))

	return nil
}
