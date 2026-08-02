package services

// import (
// 	"fmt"
// 	"io"
// 	"net/http"
// 	"net/url"
// 	"strings"
// 	"time"

// 	"turf-booking-backend/internal/config"
// )

// func SendConfirmationSMS(cfg *config.Config, phone, name, date, timeSlot string) {
// 	message := fmt.Sprintf("Hello %s, your pitch booking for %s at %s is Confirmed. Payment received successfully. See you on the pitch!", name, date, timeSlot)

// 	data := url.Values{}
// 	data.Set("username", cfg.ATUsername)
// 	data.Set("to", phone)
// 	data.Set("message", message)

// 	// CRITICAL: Always include your Sender ID so it doesn't default to a generic number
// 	if cfg.ATSenderID != "" {
// 		data.Set("from", cfg.ATSenderID)
// 	}

// 	req, _ := http.NewRequest("POST", "https://api.africastalking.com/version1/messaging", strings.NewReader(data.Encode()))
// 	req.Header.Add("Accept", "application/json")
// 	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
// 	req.Header.Add("apiKey", cfg.ATAPIKey)

// 	// Add a timeout so a slow AT server doesn't freeze your app
// 	client := &http.Client{Timeout: 10 * time.Second}
// 	res, err := client.Do(req)
// 	if err != nil {
// 		fmt.Printf("Error sending SMS: %v\n", err)
// 		return
// 	}
// 	defer res.Body.Close()

// 	body, _ := io.ReadAll(res.Body)
// 	fmt.Printf("Africa's Talking Response: %s\n", string(body))
// }
