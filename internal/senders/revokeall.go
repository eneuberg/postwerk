package senders

import (
	"context"
	"errors"
)

// RevokeAll revokes the keys with these ids, the ones of a sender that are
// not revoked yet (expired ones included), so the sender can no longer log
// in (D76: a sender is never deleted; this is how it is switched off). Each
// key is revoked on its own, with its own sender.revoked event; one revoked
// meanwhile by someone else is skipped. It returns the ids it revoked, also
// when it stops at an error.
func RevokeAll(ctx context.Context, st Store, ids []string, actor string) ([]string, error) {
	var done []string
	for _, id := range ids {
		err := st.Revoke(ctx, id, actor)
		switch {
		case errors.Is(err, ErrRevoked):
			continue
		case err != nil:
			return done, err
		}
		done = append(done, id)
	}
	return done, nil
}
