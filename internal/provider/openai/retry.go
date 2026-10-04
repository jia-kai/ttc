package openai

import (
	"context"
	"crypto/x509"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ttc/internal/provider"
)

const maxRetryDelay = 30 * time.Second

// Certificate configuration errors cannot recover by retrying the same request.
func invalidCertificate(err error) bool {
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate x509.CertificateInvalidError
	return errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &certificate)
}

// retryDelay uses server delays exactly, capped at 30 seconds. Ordinary backoff
// starts at one second, doubles to the cap, and uses 25% jitter. attempt is zero
// for the first failed request. Clamp before shifting/multiplying to avoid overflow.
func retryDelay(attempt int, header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header != "" {
		if seconds, err := strconv.ParseUint(header, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
			if seconds >= 30 {
				return maxRetryDelay
			}
			return time.Duration(seconds) * time.Second
		}
		if date, err := http.ParseTime(header); err == nil {
			delay := date.Sub(now)
			return min(max(delay, 0), maxRetryDelay)
		}
	}
	base := maxRetryDelay
	if attempt < 5 {
		base = time.Second * time.Duration(1<<attempt)
	}
	return min(time.Duration(float64(base)*(0.75+rand.Float64()*0.5)), maxRetryDelay)
}

func retryWait(ctx context.Context, emit func(provider.StreamEvent) error, attempt, limit int, reason, header string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := retryDelay(attempt, header, time.Now())
	if err := emit(provider.StreamEvent{Kind: "retry", Retry: &provider.Retry{
		Attempt: attempt + 2, MaxAttempts: limit, DelayMilliseconds: delay.Milliseconds(), Reason: reason,
	}}); err != nil {
		return err
	}
	return wait(ctx, delay)
}
