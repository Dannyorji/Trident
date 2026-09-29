package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
)

func main() {
	saltFlag := flag.String("salt", "", "API_KEY_SALT deployment secret (defaults to API_KEY_SALT env var)")
	flag.Parse()

	salt := *saltFlag
	if salt == "" {
		salt = os.Getenv("API_KEY_SALT")
	}

	if salt == "" {
		fmt.Fprintln(os.Stderr, "Warning: API_KEY_SALT is empty. Using default development salt.")
		salt = "default-salt"
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating random key: %v\n", err)
		os.Exit(1)
	}

	rawKey := hex.EncodeToString(keyBytes)

	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(rawKey))
	keyHash := hex.EncodeToString(mac.Sum(nil))

	fmt.Println("=== Trident API Key Generator ===")
	fmt.Printf("Raw API Key (client X-API-Key): %s\n", rawKey)
	fmt.Printf("HMAC-SHA256 Hash (server config): %s\n", keyHash)
	fmt.Printf("Salt used:                       %s\n\n", salt)
	fmt.Println("Configuration instructions:")
	fmt.Printf("  API_KEY_SALT=%s\n", salt)
	fmt.Printf("  API_KEY_HASHES=%s\n", keyHash)
	fmt.Println("  (or API_KEY=" + keyHash + ")")
}
