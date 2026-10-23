package mysql

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/jmoiron/sqlx"
)

func (ds *Datastore) SetHostGoogleDeviceID(ctx context.Context, hostID uint, googleDeviceID string) (bool, error) {
	if googleDeviceID == "" {
		res, err := ds.writer(ctx).ExecContext(ctx, `DELETE FROM host_google_device_ids WHERE host_id = ?`, hostID)
		if err != nil {
			return false, ctxerr.Wrap(ctx, err, "clearing host google device id")
		}
		deleted, _ := res.RowsAffected()
		return deleted > 0, nil
	}

	const selectStmt = `SELECT google_device_id FROM host_google_device_ids WHERE host_id = ? FOR UPDATE`
	const insertStmt = `INSERT INTO host_google_device_ids (host_id, google_device_id) VALUES (?, ?)`

	conflict := func(stored string) error {
		if stored == googleDeviceID {
			return nil
		}
		return fleet.NewInvalidArgumentError("google_device_id", "This host already has a different Google device ID.").
			WithStatus(http.StatusConflict)
	}

	var changed bool
	err := ds.withRetryTxx(ctx, func(tx sqlx.ExtContext) error {
		changed = false
		var stored string
		err := sqlx.GetContext(ctx, tx, &stored, selectStmt, hostID)
		switch {
		case err == nil:
			return conflict(stored)
		case !errors.Is(err, sql.ErrNoRows):
			return ctxerr.Wrap(ctx, err, "reading host google device id")
		}

		_, err = tx.ExecContext(ctx, insertStmt, hostID, googleDeviceID)
		switch {
		case err == nil:
			changed = true
			return nil
		case IsDuplicate(err):
			// a concurrent call bound the host first; its value decides whether ours conflicts
			if err := sqlx.GetContext(ctx, tx, &stored, selectStmt, hostID); err != nil {
				return ctxerr.Wrap(ctx, err, "reading host google device id after conflict")
			}
			return conflict(stored)
		default:
			return ctxerr.Wrap(ctx, err, "inserting host google device id")
		}
	})
	return changed, err
}

func (ds *Datastore) GetHostGoogleDeviceID(ctx context.Context, hostID uint) (string, error) {
	var id string
	err := sqlx.GetContext(ctx, ds.reader(ctx), &id, `SELECT google_device_id FROM host_google_device_ids WHERE host_id = ?`, hostID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", ctxerr.Wrap(ctx, err, "getting host google device id")
	}
	return id, nil
}

// clearHostGoogleDeviceIDDB removes the host's Google device ID binding. Called when a host enrolls
// again on an existing hosts row (wipe and re-enroll), since Google issues a new device ID to the
// fresh install and the stored one would make every later registration fail with a conflict.
func clearHostGoogleDeviceIDDB(ctx context.Context, tx sqlx.ExecerContext, hostID uint) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM host_google_device_ids WHERE host_id = ?`, hostID); err != nil {
		return ctxerr.Wrap(ctx, err, "clearing host google device id on re-enrollment")
	}
	return nil
}
