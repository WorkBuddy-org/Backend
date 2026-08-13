package invite

import (
	"crypto/rand"
	"fmt"
)

// Invite codes are a short random string rather than a UUID: they're meant
// to be typed, spoken, or dropped into a chat message as a referral code,
// not used as a bearer secret guarding access (registration is open to
// anyone via OAuth regardless of invite — see product decision below).
const (
	codeLength = 10
	// Ambiguous characters (0/O, 1/l/I) are excluded to keep codes easy to
	// read and retype correctly.
	codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz"
)

// Product decision (documented, not a gap to fix): invites currently only
// track who invited whom. GET /invites/{code} reports validity but never
// marks a code as used, and OAuth registration does not require or consume
// an invite code. Wiring registration to require+consume an invite is a
// possible future shift, not part of this one.

func generateCode() (string, error) {
	b := make([]byte, codeLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate invite code: %w", err)
	}

	for i, v := range b {
		b[i] = codeAlphabet[int(v)%len(codeAlphabet)]
	}

	return string(b), nil
}
