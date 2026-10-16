package agent

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v5"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
)

// Run reconnects sessions with bounded, jittered exponential backoff until ctx
// expires or serve returns. options runs before every dial so credentials and
// applied cursors can be refreshed. serve must return when Events closes or Done
// fires. A nil serve result stops Run successfully; application errors stop it.
// Subscription restoration and mutation retries remain explicit in serve.
func Run(ctx context.Context, options func(context.Context) (Options, error), serve func(*Client) error) error {
	if options == nil || serve == nil {
		return errors.New("agent: missing session callbacks")
	}
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = time.Second
	policy.MaxInterval = 20 * time.Second // With 0.5 jitter, waits never exceed 30s.
	policy.Multiplier = 2
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		if err := ctx.Err(); err != nil {
			return struct{}{}, backoff.Permanent(err)
		}
		o, err := options(ctx)
		if err != nil {
			return struct{}{}, backoff.Permanent(err)
		}
		if _, err = o.normalized(); err != nil {
			return struct{}{}, backoff.Permanent(err)
		}
		c, err := Dial(ctx, o)
		if err != nil {
			return struct{}{}, retryError(err)
		}
		applicationErr := serve(c)
		sessionErr := c.Err()
		_ = c.Close()
		if ctx.Err() != nil {
			return struct{}{}, backoff.Permanent(ctx.Err())
		}
		if applicationErr == nil {
			return struct{}{}, nil
		}
		if sessionErr == nil || !errors.Is(applicationErr, sessionErr) {
			return struct{}{}, backoff.Permanent(applicationErr)
		}
		return struct{}{}, retryError(sessionErr)
	}, backoff.WithBackOff(policy), backoff.WithMaxElapsedTime(0))
	return err
}

func retryError(err error) error {
	if errors.Is(err, ErrProtocol) || errors.Is(err, ErrBackpressure) {
		return backoff.Permanent(err)
	}
	if e, ok := errors.AsType[*agentv1.ProtocolError](err); ok {
		return backoff.Permanent(e)
	}
	if e, ok := errors.AsType[*HandshakeError](err); ok {
		if e.StatusCode != 408 && e.StatusCode != 429 && e.StatusCode < 500 {
			return backoff.Permanent(e)
		}
		if e.RetryAfter > 0 {
			return errors.Join(err, &backoff.RetryAfterError{Duration: e.RetryAfter})
		}
	}
	if e, ok := errors.AsType[*GoAwayError](err); ok && e.RetryAfter > 0 {
		return errors.Join(err, &backoff.RetryAfterError{Duration: e.RetryAfter})
	}
	if e, ok := errors.AsType[*RemoteError](err); ok {
		if !e.Detail.Retryable {
			return backoff.Permanent(err)
		}
		if e.Detail.RetryAfterMs > 0 {
			return errors.Join(err, &backoff.RetryAfterError{Duration: time.Duration(e.Detail.RetryAfterMs) * time.Millisecond})
		}
	}
	return err
}
