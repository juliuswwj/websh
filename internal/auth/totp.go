package auth

import (
	"strings"

	"github.com/pquerna/otp/totp"
)

// VerifyTOTP checks a 6-digit code against a base32 secret using the standard
// Google Authenticator parameters (SHA1, 30s period, 6 digits). A skew of 1
// period (±30s) is tolerated.
func VerifyTOTP(secret, code string) bool {
	secret = strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	code = strings.TrimSpace(code)
	return totp.Validate(code, secret)
}
