package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"turf-booking-backend/internal/middleware"
)

const defaultOrigin = "https://karena.stsebastiansportsacademy.com"

type Config struct {
	MongoURI   string
	Port       string
	PaystackSK string
	ATUsername string
	ATAPIKey   string
	ATSenderID string

	// AllowedOrigins are the browser origins permitted to call the API and open
	// the payment-status WebSocket. Set ALLOWED_ORIGINS (comma separated) to add
	// e.g. http://localhost:3000 for local development.
	AllowedOrigins []string

	// AdminAPIKey guards /api/v1/admin/*. It is held only by the Next.js server,
	// never by a browser. When empty the admin endpoint is switched off.
	AdminAPIKey string

	// TrustedProxyHops is how many reverse proxies sit in front of this server
	// (see middleware.ClientIP). TRUSTED_PROXY_HOPS=auto (the default) infers it.
	TrustedProxyHops int
}

func LoadConfig() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on system environment variables")
	}

	return &Config{
		MongoURI:         os.Getenv("MONGO_URI"),
		Port:             os.Getenv("PORT"),
		PaystackSK:       os.Getenv("PAYSTACK_SK"),
		ATUsername:       os.Getenv("AT_USERNAME"),
		ATAPIKey:         os.Getenv("AT_API_KEY"),
		ATSenderID:       os.Getenv("AT_SENDER_ID"),
		AllowedOrigins:   parseOrigins(os.Getenv("ALLOWED_ORIGINS")),
		AdminAPIKey:      os.Getenv("ADMIN_API_KEY"),
		TrustedProxyHops: parseHops(os.Getenv("TRUSTED_PROXY_HOPS")),
	}
}

// Validate refuses to start with a configuration that would be unsafe to run.
func (c *Config) Validate() error {
	var problems []string

	// The webhook signature is an HMAC keyed with this secret. With an empty
	// key anyone can compute a "valid" signature and mark bookings as paid.
	if strings.TrimSpace(c.PaystackSK) == "" {
		problems = append(problems, "PAYSTACK_SK is required: webhook signatures cannot be verified without it")
	}
	if c.AdminAPIKey != "" && len(c.AdminAPIKey) < 32 {
		problems = append(problems, "ADMIN_API_KEY must be at least 32 characters (try: openssl rand -hex 32)")
	}
	for _, o := range c.AllowedOrigins {
		if !strings.HasPrefix(o, "https://") && !strings.HasPrefix(o, "http://localhost") && !strings.HasPrefix(o, "http://127.0.0.1") {
			problems = append(problems, fmt.Sprintf("ALLOWED_ORIGINS entry %q must be https (http is only allowed for localhost)", o))
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func parseOrigins(raw string) []string {
	origins := []string{defaultOrigin}
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o != "" && o != defaultOrigin {
			origins = append(origins, o)
		}
	}
	return origins
}

func parseHops(raw string) int {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "auto" {
		return middleware.AutoProxyHops
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("Ignoring invalid TRUSTED_PROXY_HOPS %q, using auto", raw)
		return middleware.AutoProxyHops
	}
	return n
}
