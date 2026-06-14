package auth

import (
	"errors"
	"fmt"

	"github.com/msteinert/pam/v2"
)

// PAMAuthenticate verifies a username/password against the given PAM service
// (e.g. "websh"). It runs both the auth and account-management phases so that
// expired/disabled accounts are rejected.
//
// PAM (pam_unix) reads /etc/shadow, which requires the process to run as root.
// PAM can also block (faildelay, network modules) — call this from a goroutine
// off the request's hot path. Never log the password.
func PAMAuthenticate(service, username, password string) error {
	if service == "" {
		service = "websh"
	}
	t, err := pam.StartFunc(service, username, func(s pam.Style, msg string) (string, error) {
		switch s {
		case pam.PromptEchoOff: // password
			return password, nil
		case pam.PromptEchoOn: // login name
			return username, nil
		case pam.ErrorMsg, pam.TextInfo:
			return "", nil
		default:
			return "", fmt.Errorf("unexpected PAM conversation style %v", s)
		}
	})
	if err != nil {
		return fmt.Errorf("pam start: %w", err)
	}
	defer func() { _ = t.End() }()

	if err := t.Authenticate(0); err != nil {
		return errors.New("authentication failed")
	}
	if err := t.AcctMgmt(0); err != nil {
		return errors.New("account not permitted")
	}
	return nil
}
