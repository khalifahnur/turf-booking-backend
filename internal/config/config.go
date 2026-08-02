package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	MongoURI   string
	Port       string
	PaystackSK string
	ATUsername string
	ATAPIKey   string
}

func LoadConfig() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on system environment variables")
	}

	return &Config{
		MongoURI:   os.Getenv("MONGO_URI"),
		Port:       os.Getenv("PORT"),
		PaystackSK: os.Getenv("PAYSTACK_SK"),
		ATUsername: os.Getenv("AT_USERNAME"),
		ATAPIKey:   os.Getenv("AT_API_KEY"),
	}
}
