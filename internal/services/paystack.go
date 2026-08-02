package services

import (
	"bytes"
	"encoding/json"
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

	req, err := http.NewRequest("POST", "https://api.paystack.co/charge", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+cfg.PaystackSK)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("api request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("paystack error (Status %d): %s", resp.StatusCode, string(body))
	}

	var result PaystackChargeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("error unmarshaling response: %w", err)
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
