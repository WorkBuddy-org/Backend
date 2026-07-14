package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTokenManagerIssueAndParse(t *testing.T) {
	manager := NewTokenManager("test-secret", time.Minute)

	userID := uuid.New()
	token, err := manager.IssueAccessToken(userID, "dev@example.com")
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}

	claims, err := manager.ParseAccessToken(token)
	if err != nil {
		t.Fatalf("parse access token: %v", err)
	}

	if claims.Subject != userID.String() {
		t.Fatalf("expected subject %s, got %s", userID, claims.Subject)
	}
	if claims.Email != "dev@example.com" {
		t.Fatalf("expected email dev@example.com, got %s", claims.Email)
	}
}

func TestHashTokenIsDeterministic(t *testing.T) {
	raw := "sample-refresh-token"
	first := hashToken(raw)
	second := hashToken(raw)

	if first != second {
		t.Fatalf("expected deterministic hash, got %s and %s", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("expected sha256 hex length 64, got %d", len(first))
	}
}

func TestGenerateRefreshTokenUnique(t *testing.T) {
	raw1, hash1, err := generateRefreshToken()
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}

	raw2, hash2, err := generateRefreshToken()
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}

	if raw1 == raw2 || hash1 == hash2 {
		t.Fatal("expected unique refresh tokens")
	}
}
