package mysql

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/test"
	"github.com/stretchr/testify/require"
)

func TestHostGoogleDeviceIDs(t *testing.T) {
	ds := CreateMySQLDS(t)

	cases := []struct {
		name string
		fn   func(t *testing.T, ds *Datastore)
	}{
		{"SetAndGet", testHostGoogleDeviceIDSetAndGet},
		{"ClearedOnAppleReenrollment", testHostGoogleDeviceIDClearedOnAppleReenrollment},
		{"ClearedOnAndroidReenrollment", testHostGoogleDeviceIDClearedOnAndroidReenrollment},
		{"ConcurrentDifferentValues", testHostGoogleDeviceIDConcurrentDifferentValues},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer TruncateTables(t, ds)
			c.fn(t, ds)
		})
	}
}

func testHostGoogleDeviceIDSetAndGet(t *testing.T, ds *Datastore) {
	ctx := t.Context()
	host := test.NewHost(t, ds, "h1", "", "h1key", "h1uuid", time.Now())
	other := test.NewHost(t, ds, "h2", "", "h2key", "h2uuid", time.Now())

	id, err := ds.GetHostGoogleDeviceID(ctx, host.ID)
	require.NoError(t, err)
	require.Empty(t, id)

	// clearing a host that never had an ID is a no-op
	changed, err := ds.SetHostGoogleDeviceID(ctx, host.ID, "")
	require.NoError(t, err)
	require.False(t, changed)

	changed, err = ds.SetHostGoogleDeviceID(ctx, host.ID, "device-a")
	require.NoError(t, err)
	require.True(t, changed)
	id, err = ds.GetHostGoogleDeviceID(ctx, host.ID)
	require.NoError(t, err)
	require.Equal(t, "device-a", id)

	// same value again is idempotent
	changed, err = ds.SetHostGoogleDeviceID(ctx, host.ID, "device-a")
	require.NoError(t, err)
	require.False(t, changed)

	// a different value is refused and the stored one is kept
	_, err = ds.SetHostGoogleDeviceID(ctx, host.ID, "device-b")
	require.Error(t, err)
	var statusErr interface{ Status() int }
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusConflict, statusErr.Status()) //nolint:nilaway // cannot be nil due to require.ErrorAs above
	var invalidErr interface{ Invalid() []map[string]string }
	require.ErrorAs(t, err, &invalidErr)
	require.Equal(t, []map[string]string{{
		"name":   "google_device_id",
		"reason": "This host already has a different Google device ID.",
	}}, invalidErr.Invalid()) //nolint:nilaway // cannot be nil due to require.ErrorAs above
	id, err = ds.GetHostGoogleDeviceID(ctx, host.ID)
	require.NoError(t, err)
	require.Equal(t, "device-a", id)

	// other hosts are independent
	id, err = ds.GetHostGoogleDeviceID(ctx, other.ID)
	require.NoError(t, err)
	require.Empty(t, id)
	_, err = ds.SetHostGoogleDeviceID(ctx, other.ID, "device-b")
	require.NoError(t, err)

	// clearing allows a new value
	changed, err = ds.SetHostGoogleDeviceID(ctx, host.ID, "")
	require.NoError(t, err)
	require.True(t, changed)
	id, err = ds.GetHostGoogleDeviceID(ctx, host.ID)
	require.NoError(t, err)
	require.Empty(t, id)
	_, err = ds.SetHostGoogleDeviceID(ctx, host.ID, "device-c")
	require.NoError(t, err)
	id, err = ds.GetHostGoogleDeviceID(ctx, host.ID)
	require.NoError(t, err)
	require.Equal(t, "device-c", id)

	id, err = ds.GetHostGoogleDeviceID(ctx, other.ID)
	require.NoError(t, err)
	require.Equal(t, "device-b", id)
}

func testHostGoogleDeviceIDClearedOnAppleReenrollment(t *testing.T, ds *Datastore) {
	ctx := t.Context()
	host := test.NewHost(t, ds, "iphone", "", "iphonekey", "iphone-uuid", time.Now())
	_, err := ds.writer(ctx).ExecContext(ctx, `UPDATE hosts SET platform = 'ios', hardware_serial = 'iphone-serial' WHERE id = ?`, host.ID)
	require.NoError(t, err)
	_, err = ds.SetHostGoogleDeviceID(ctx, host.ID, "device-a")
	require.NoError(t, err)

	// a wiped device enrolls again with the same serial and UDID, so the host row is reused
	err = ds.MDMAppleUpsertHost(ctx, &fleet.Host{
		UUID:           "iphone-uuid",
		HardwareSerial: "iphone-serial",
		Platform:       "ios",
	}, fleet.PersonalEnrollmentTypeNone)
	require.NoError(t, err)

	hosts := listHostsCheckCount(t, ds, fleet.TeamFilter{User: test.UserAdmin}, fleet.HostListOptions{}, 1)
	require.Equal(t, host.ID, hosts[0].ID)
	id, err := ds.GetHostGoogleDeviceID(ctx, host.ID)
	require.NoError(t, err)
	require.Empty(t, id)
}

func testHostGoogleDeviceIDClearedOnAndroidReenrollment(t *testing.T, ds *Datastore) {
	ctx := t.Context()
	android, err := ds.NewAndroidHost(ctx, createAndroidHost("es-id-reenroll"), false)
	require.NoError(t, err)
	_, err = ds.SetHostGoogleDeviceID(ctx, android.Host.ID, "device-a")
	require.NoError(t, err)

	// a regular detail update keeps the binding
	require.NoError(t, ds.UpdateAndroidHost(ctx, android, false, false))
	id, err := ds.GetHostGoogleDeviceID(ctx, android.Host.ID)
	require.NoError(t, err)
	require.Equal(t, "device-a", id)

	// a factory-reset device enrolls again with the same enterprise-specific ID
	require.NoError(t, ds.UpdateAndroidHost(ctx, android, true, false))
	id, err = ds.GetHostGoogleDeviceID(ctx, android.Host.ID)
	require.NoError(t, err)
	require.Empty(t, id)

	// a host enrolled by the Fleet Android agent is reused when it enrolls in MDM
	agentHost := test.NewHost(t, ds, "agent", "", "agentkey", "es-id-agent", time.Now())
	_, err = ds.writer(ctx).ExecContext(ctx, `UPDATE hosts SET platform = 'android' WHERE id = ?`, agentHost.ID)
	require.NoError(t, err)
	_, err = ds.SetHostGoogleDeviceID(ctx, agentHost.ID, "device-b")
	require.NoError(t, err)
	reused, err := ds.NewAndroidHost(ctx, createAndroidHost("es-id-agent"), false)
	require.NoError(t, err)
	require.Equal(t, agentHost.ID, reused.Host.ID)
	id, err = ds.GetHostGoogleDeviceID(ctx, agentHost.ID)
	require.NoError(t, err)
	require.Empty(t, id)
}

func testHostGoogleDeviceIDConcurrentDifferentValues(t *testing.T, ds *Datastore) {
	ctx := t.Context()
	host := test.NewHost(t, ds, "h1", "", "h1key", "h1uuid", time.Now())

	values := []string{"device-a", "device-b"}
	errs := make([]error, len(values))
	var wg sync.WaitGroup
	for i, v := range values {
		wg.Go(func() {
			_, errs[i] = ds.SetHostGoogleDeviceID(ctx, host.ID, v)
		})
	}
	wg.Wait()

	// exactly one bind wins and the other is refused with a conflict
	var winners []string
	for i, err := range errs {
		if err == nil {
			winners = append(winners, values[i])
			continue
		}
		var statusErr interface{ Status() int }
		require.ErrorAs(t, err, &statusErr)
		require.Equal(t, http.StatusConflict, statusErr.Status()) //nolint:nilaway // cannot be nil due to require.ErrorAs above
	}
	require.Len(t, winners, 1)
	id, err := ds.GetHostGoogleDeviceID(ctx, host.ID)
	require.NoError(t, err)
	require.Equal(t, winners[0], id)
}
