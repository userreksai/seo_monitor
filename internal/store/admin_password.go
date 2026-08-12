package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/crypto/bcrypt"

	"seo-monitor/internal/model"
)

const (
	minPasswordBytes = 12
	maxPasswordBytes = 72
)

var ErrInvalidPassword = errors.New("invalid password")

// SetUserPassword resets an account password and revokes all of its sessions.
// It is intended for trusted local administration tools, not HTTP handlers.
func (s *Store) SetUserPassword(ctx context.Context, username, newPassword string) error {
	username = normalizeUsername(username)
	if username == "" {
		return errors.New("username is required")
	}
	if err := validateNewPassword(newPassword); err != nil {
		return err
	}

	var user model.User
	if err := s.users.FindOne(ctx, bson.M{"username": username}).Decode(&user); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return ErrNotFound
		}
		return fmt.Errorf("find user: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	now := time.Now().UTC()
	if _, err := s.users.UpdateOne(ctx, bson.M{"_id": user.ID}, bson.M{"$set": bson.M{
		"password_hash":       string(hash),
		"password_changed_at": now,
		"updated_at":          now,
	}}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	// password_changed_at already invalidates old sessions during authentication;
	// this deletion is best-effort cleanup for the TTL collection.
	_, _ = s.sessions.DeleteMany(ctx, bson.M{"user_id": user.ID})
	return nil
}

func validateNewPassword(password string) error {
	if strings.ContainsAny(password, "\r\n") {
		return fmt.Errorf("%w: password cannot contain a line break", ErrInvalidPassword)
	}
	passwordBytes := len([]byte(password))
	if passwordBytes < minPasswordBytes {
		return fmt.Errorf("%w: password must contain at least %d bytes", ErrInvalidPassword, minPasswordBytes)
	}
	if passwordBytes > maxPasswordBytes {
		return fmt.Errorf("%w: password must contain at most %d bytes", ErrInvalidPassword, maxPasswordBytes)
	}
	return nil
}
