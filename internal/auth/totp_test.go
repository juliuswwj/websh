package auth

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestVerifyTOTP(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyTOTP(secret, code) {
		t.Fatal("valid code rejected")
	}
	if !VerifyTOTP("jbswy3dpehpk3pxp", code) {
		t.Fatal("lowercase secret should still verify")
	}
	if VerifyTOTP(secret, "000000") {
		t.Fatal("bogus code accepted (flaky if it happens to match)")
	}
}
