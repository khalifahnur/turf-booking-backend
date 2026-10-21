package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"turf-booking-backend/internal/config"
)

type PaystackChargeRequest struct {
	Email       string `json:"email"`
	Amount      int    `json:"amount"`
	Currency    string `json:"currency"`
	Reference   string `json:"reference"`
	MobileMoney struct {
		Phone    string `json:"phone"`
		Provider string `json:"provider"`
	} `json:"mobile_money"`
}

type PaystackChargeResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Reference   string `json:"reference"`
		Status      string `json:"status"`
		DisplayText string `json:"display_text"`
	} `json:"data"`
}

// ErrPaymentUncertain wraps a failure after which Paystack may nonetheless have
// sent the M-Pesa prompt (a timeout, a dropped connection, a 5xx, a garbled
// answer). Callers must not tell the customer they were not charged, and must
// not let them retry straight away. A failure that is not wrapped this way is
// definite: Paystack refused the request, or it never left this server.
var ErrPaymentUncertain = errors.New("the M-Pesa prompt may have been sent")

// Overridable so tests can point the charge at a local server.
var (
	paystackChargeURL = "https://api.paystack.co/charge"
	paystackTimeout   = 10 * time.Second
)

func formatPhoneNumber(phone string) string {
	phone = strings.ReplaceAll(phone, " ", "")
	phone = strings.ReplaceAll(phone, "-", "")

	if strings.HasPrefix(phone, "0") {
		return "+254" + phone[1:]
	}

	if strings.HasPrefix(phone, "254") {
		return "+" + phone
	}
	return phone
}

func InitiatePayment(cfg *config.Config, phone string, email string, amount int, reference string) (string, error) {

	payload := PaystackChargeRequest{
		Email:     email,
		Amount:    amount,
		Currency:  "KES",
		Reference: reference,
	}

	payload.MobileMoney.Phone = formatPhoneNumber(phone)
	payload.MobileMoney.Provider = "mpesa"

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", paystackChargeURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+cfg.PaystackSK)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: paystackTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: api request failed: %v", ErrPaymentUncertain, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: error reading response body: %v", ErrPaymentUncertain, err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		// Only an outright refusal (a 4xx, other than a request timeout) proves that
		// no prompt was sent; anything else (5xx, 408, an unexpected 1xx/2xx/3xx) leaves it open.
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode < 400 {
			return "", fmt.Errorf("%w: paystack error (Status %d): %s", ErrPaymentUncertain, resp.StatusCode, string(body))
		}
		return "", fmt.Errorf("paystack error (Status %d): %s", resp.StatusCode, string(body))
	}

	var result PaystackChargeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("%w: error unmarshaling response: %v", ErrPaymentUncertain, err)
	}

	if !result.Status {
		return "", fmt.Errorf("paystack returned failure status: %s", result.Message)
	}

	instructionText := result.Data.DisplayText
	if instructionText == "" {
		instructionText = "Please check your phone and enter your M-PESA PIN."
	}

	return instructionText, nil
}
