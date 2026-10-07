package openai

import (
	"context"
	"errors"
	"ttc/internal/filelock"
)

// Current reloads and validates local credentials without HTTP or token refresh.
func (a *Authenticator) Current(ctx context.Context) (Credentials, error) {
	if err := a.lockAuth(ctx); err != nil {
		return Credentials{}, err
	}
	defer a.unlockAuth()
	if err := a.load(); err != nil {
		return Credentials{}, err
	}
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	return *a.credentials, nil
}

// WithAccount coordinates publication with all credential writers. It reloads
// credentials under the same cross-process lock used by refresh/import/login,
// rejects an account change, and calls commit only while both locks are held.
// commit must not call this authenticator or acquire its credential file lock.
func (a *Authenticator) WithAccount(ctx context.Context, expected string, commit func() error) error {
	if err := a.lockAuth(ctx); err != nil {
		return err
	}
	defer a.unlockAuth()
	if expected == "" || commit == nil {
		return errors.New("account guard requires an account and commit function")
	}
	lock, err := filelock.Acquire(ctx, a.CredentialPath+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := a.load(); err != nil {
		return err
	}
	if a.credentials.Tokens.AccountID != expected {
		return errors.New("subscription account changed during model catalog request")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return commit()
}
