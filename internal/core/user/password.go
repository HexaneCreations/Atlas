package user

import (
	"github.com/hexane/atlas/internal/platform/errs"
	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is bcrypt's own recommended default. Atlas has no measured
// login-latency budget that would justify tuning this away from the
// library's own default, and the mistake in either direction — too low
// invites brute-forcing, too high turns login into a denial-of-service knob
// — is not one to make without a reason.
const bcryptCost = bcrypt.DefaultCost

// HashPassword returns the stored form of a plaintext password.
//
// bcrypt, not the plain SHA-256 [fleet.HashToken] uses for enrollment
// tokens: a token has 256 bits of real entropy that a fast hash does not
// weaken, but a human-chosen password does not, and an adaptive hash with a
// deliberate work factor is what makes an offline guessing attack against a
// stolen hash expensive rather than free.
func HashPassword(plaintext string) (string, error) {
	const op = "user.HashPassword"
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "could not hash password").WithOp(op)
	}
	return string(hash), nil
}

// VerifyPassword reports whether plaintext matches hash.
//
// It never returns why a mismatch occurred — bcrypt's own error already
// collapses "wrong password" and "corrupt hash" into one outcome, which is
// the right amount of detail for a caller that must not be able to
// distinguish them.
func VerifyPassword(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}
