package main

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	HeaderAPIKey    = "Dujiao-Next-Api-Key"
	HeaderTimestamp = "Dujiao-Next-Timestamp"
	HeaderSignature = "Dujiao-Next-Signature"

	MaxTimestampSkew = 60
	CallbackPath     = "/api/v1/upstream/callback"
)

// Sign builds hex_lower(hmac_sha256(secret, "{METHOD}\n{PATH}\n{TS}\n{MD5(BODY)}")).
func Sign(secret, method, path string, timestamp int64, body []byte) string {
	sum := md5.Sum(body)
	signString := fmt.Sprintf("%s\n%s\n%d\n%s", method, path, timestamp, hex.EncodeToString(sum[:]))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signString))
	return hex.EncodeToString(mac.Sum(nil))
}

func Verify(secret, method, path, signature string, timestamp int64, body []byte) bool {
	expected := Sign(secret, method, path, timestamp, body)
	return hmac.Equal([]byte(expected), []byte(signature))
}

func TimestampValid(timestamp int64) bool {
	return math.Abs(float64(time.Now().Unix()-timestamp)) <= MaxTimestampSkew
}

func ParseTimestamp(raw string) (int64, error) {
	return strconv.ParseInt(raw, 10, 64)
}
