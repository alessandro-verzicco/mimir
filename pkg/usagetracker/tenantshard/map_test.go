// SPDX-License-Identifier: AGPL-3.0-only

package tenantshard

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/grafana/mimir/pkg/usagetracker/clock"
)

func TestMap(t *testing.T) {
	series := atomic.NewUint64(0)
	const events = 5
	const seriesPerEvent = 5
	limit := uint64(events * seriesPerEvent)

	// Start small, let rehashing happen.
	m := New(seriesPerEvent, 0, Shards)

	storedValues := map[uint64]clock.Minutes{}
	for i := 1; i <= events; i++ {
		refs := make([]uint64, seriesPerEvent)
		for j := range refs {
			refs[j] = uint64((i*100 + j) << valueBits)
			storedValues[refs[j]] = clock.Minutes(i)
			created, rejected := m.Put(refs[j], clock.Minutes(i), series, limit, false)
			require.True(t, created)
			require.False(t, rejected)
		}
	}

	require.Equal(t, events*seriesPerEvent, m.count())
	require.Equal(t, uint64(events*seriesPerEvent), series.Load())

	{
		// No more series will fit.
		created, rejected := m.Put(uint64(65535)<<valueBits, 1, series, limit, true)
		require.False(t, created)
		require.True(t, rejected)
	}

	{
		gotValues := map[uint64]clock.Minutes{}
		iterator := m.Iterator()
		iterator(
			func(c int) { require.Equal(t, len(storedValues), c) },
			func(key uint64, value clock.Minutes) { gotValues[key] = value },
		)
		require.Equal(t, storedValues, gotValues)
	}

	{
		// Cleanup first wave of series
		m.Cleanup(clock.Minutes(1), series)
		expectedSeries := (events - 1) * seriesPerEvent

		// It's unsafe to check m.count() after Cleanup event.
		require.Equal(t, expectedSeries, int(series.Load()))
	}
}

func TestMapValues(t *testing.T) {
	const count = 10e3
	stored := map[uint64]clock.Minutes{}
	m := New(100, 0, Shards)
	total := atomic.NewUint64(0)
	for i := 0; i < count; i++ {
		key := rand.Uint64() &^ valueMask // we can only store values of this shard.
		val := clock.Minutes(i) % valueMask
		stored[key] = val
		m.Put(key, val, total, 0, false)
	}
	require.Equal(t, len(stored), m.count())
	require.Equal(t, len(stored), int(total.Load()))

	got := map[uint64]clock.Minutes{}
	m.Iterator()(
		func(c int) { require.Equal(t, len(stored), c) },
		func(key uint64, value clock.Minutes) { got[key] = value },
	)
	require.Equal(t, stored, got)
}
