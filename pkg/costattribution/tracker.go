// SPDX-License-Identifier: AGPL-3.0-only

package costattribution

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/labels"
	"go.uber.org/atomic"
)

type Observation struct {
	lastUpdate      *atomic.Int64
	activeSerie     *atomic.Int64
	receivedSample  *atomic.Int64
	discardSamplemu sync.RWMutex
	discardedSample map[string]*atomic.Int64
}

const (
	TrackerLabel       = "tracker"
	TenantLabel        = "tenant"
	defaultTrackerName = "cost-attribution"
)

type Tracker struct {
	userID                         string
	caLabels                       []string
	maxCardinality                 int
	activeSeriesPerUserAttribution *prometheus.GaugeVec
	receivedSamplesAttribution     *prometheus.CounterVec
	discardedSampleAttribution     *prometheus.CounterVec

	overflowLabels []string
	// obseveredMtx protects the observed map
	obseveredMtx sync.RWMutex
	observed     map[string]*Observation

	hashBuffer       []byte
	isOverflow       bool
	cooldownUntil    *atomic.Int64
	cooldownDuration int64
	logger           log.Logger
}

func newTracker(userID string, trackedLabels []string, limit int, cooldown time.Duration, logger log.Logger) (*Tracker, error) {
	// keep tracked labels sorted for consistent metric labels
	sort.Slice(trackedLabels, func(i, j int) bool {
		return trackedLabels[i] < trackedLabels[j]
	})
	m := &Tracker{
		userID:         userID,
		caLabels:       trackedLabels,
		maxCardinality: limit,
		obseveredMtx:   sync.RWMutex{},
		observed:       map[string]*Observation{},
		//lint:ignore faillint the metrics are registered in the mimir package
		discardedSampleAttribution: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "cortex_discarded_attributed_samples_total",
			Help:        "The total number of samples that were discarded per attribution.",
			ConstLabels: prometheus.Labels{TrackerLabel: defaultTrackerName},
		}, append(trackedLabels, TenantLabel, "reason")),
		//lint:ignore faillint the metrics are registered in the mimir package
		receivedSamplesAttribution: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "cortex_received_attributed_samples_total",
			Help:        "The total number of samples that were received per attribution.",
			ConstLabels: prometheus.Labels{TrackerLabel: defaultTrackerName},
		}, append(trackedLabels, TenantLabel)),
		//lint:ignore faillint the metrics are registered in the mimir package
		activeSeriesPerUserAttribution: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "cortex_ingester_attributed_active_series",
			Help:        "The total number of active series per user and attribution.",
			ConstLabels: prometheus.Labels{TrackerLabel: defaultTrackerName},
		}, append(trackedLabels, TenantLabel)),
		hashBuffer:       make([]byte, 0, 1024),
		cooldownDuration: int64(cooldown.Seconds()),
		logger:           logger,
	}

	// set overflow label values to export when the tracker is in overflow state
	m.overflowLabels = make([]string, len(trackedLabels)+2)
	for i := 0; i < len(trackedLabels); i++ {
		m.overflowLabels[i] = overflowValue
	}
	m.overflowLabels[len(trackedLabels)] = userID
	m.overflowLabels[len(trackedLabels)+1] = overflowValue
	return m, nil
}

func (t *Tracker) CALabels() []string {
	if t == nil {
		return nil
	}
	return t.caLabels
}

func (t *Tracker) MaxCardinality() int {
	if t == nil {
		return 0
	}
	return t.maxCardinality
}

func (t *Tracker) CooldownDuration() int64 {
	if t == nil {
		return 0
	}
	return t.cooldownDuration
}

// sep is used to separate the labels in the key, it is not a valid label caracter
const sep = rune(0x80)

func (t *Tracker) cleanupTrackerAttribution(key string) {
	if t == nil {
		return
	}

	t.obseveredMtx.Lock()
	delete(t.observed, key)
	t.obseveredMtx.Unlock()

	vals := strings.Split(key, string(sep))
	vals = append(vals, t.userID)
	t.activeSeriesPerUserAttribution.DeleteLabelValues(vals...)
	t.receivedSamplesAttribution.DeleteLabelValues(vals...)

	// except for discarded sample metrics, there is reason label that is not part of the key, we need to delete all partial matches
	filter := prometheus.Labels{}
	for i := 0; i < len(t.caLabels); i++ {
		filter[t.caLabels[i]] = vals[i]
	}
	filter[TenantLabel] = t.userID
	t.discardedSampleAttribution.DeletePartialMatch(filter)
}

func (t *Tracker) cleanupTracker() {
	if t == nil {
		return
	}
	filter := prometheus.Labels{TenantLabel: t.userID}
	t.activeSeriesPerUserAttribution.DeletePartialMatch(filter)
	t.receivedSamplesAttribution.DeletePartialMatch(filter)
	t.discardedSampleAttribution.DeletePartialMatch(filter)
}

func (t *Tracker) IncrementActiveSeries(lbs labels.Labels, now time.Time) {
	if t == nil {
		return
	}
	t.updateCounters(lbs, now.Unix(), 1, 0, 0, nil)
}

func (t *Tracker) DecrementActiveSeries(lbs labels.Labels, now time.Time) {
	if t == nil {
		return
	}
	t.updateCounters(lbs, now.Unix(), -1, 0, 0, nil)
}

func (t *Tracker) IncrementDiscardedSamples(lbs labels.Labels, value float64, reason string, now time.Time) {
	if t == nil {
		return
	}
	t.updateCounters(lbs, now.Unix(), 0, 0, int64(value), &reason)
}

func (t *Tracker) IncrementReceivedSamples(lbs labels.Labels, value float64, now time.Time) {
	if t == nil {
		return
	}
	t.updateCounters(lbs, now.Unix(), 0, int64(value), 0, nil)
}

func (t *Tracker) Collect(out chan<- prometheus.Metric) {
	if t == nil {
		return
	}

	t.activeSeriesPerUserAttribution.Collect(out)
	t.receivedSamplesAttribution.Collect(out)
	t.discardedSampleAttribution.Collect(out)
}

// Describe implements prometheus.Collector.
func (t *Tracker) Describe(chan<- *prometheus.Desc) {
	// this is an unchecked collector
	if t == nil {
		return
	}
}

func (t *Tracker) updateCounters(lbls labels.Labels, ts int64, activeSeriesIncrement, receviedSampleIncrement, discardedSampleIncrement int64, reason *string) {
	if t == nil {
		return
	}
	labelValues := make([]string, len(t.caLabels)+1)
	for i, label := range t.caLabels {
		labelValues[i] = lbls.Get(label)
		if labelValues[i] == "" {
			labelValues[i] = missingValue
		}
	}
	labelValues[len(labelValues)-1] = t.userID

	var sb strings.Builder
	for i, value := range labelValues[:len(labelValues)-1] {
		if i > 0 {
			sb.WriteRune(sep)
		}
		sb.WriteString(value)
	}

	t.updateOverflow(sb.String(), ts, activeSeriesIncrement, receviedSampleIncrement, discardedSampleIncrement, reason)
}

func (t *Tracker) updateOverflow(stream string, ts int64, activeSeriesIncrement, receviedSampleIncrement, discardedSampleIncrement int64, reason *string) {
	if t == nil {
		return
	}

	t.obseveredMtx.Lock()
	// we store up to 2 * maxCardinality observations, if we have seen the stream before, we update the last update time
	if o, known := t.observed[stream]; known && o.lastUpdate != nil {
		if o.lastUpdate.Load() < ts {
			o.lastUpdate.Store(ts)
		}
		if activeSeriesIncrement != 0 {
			o.activeSerie.Add(activeSeriesIncrement)
		}
		if receviedSampleIncrement > 0 {
			o.receivedSample.Add(receviedSampleIncrement)
		}
		if discardedSampleIncrement > 0 && reason != nil {
			o.discardSamplemu.Lock()
			o.discardedSample[*reason] = atomic.NewInt64(discardedSampleIncrement)
			o.discardSamplemu.Unlock()
		}
	} else if len(t.observed) < t.maxCardinality*2 {
		t.observed[stream] = &Observation{
			lastUpdate:      atomic.NewInt64(ts),
			activeSerie:     atomic.NewInt64(activeSeriesIncrement),
			receivedSample:  atomic.NewInt64(receviedSampleIncrement),
			discardedSample: map[string]*atomic.Int64{},
			discardSamplemu: sync.RWMutex{},
		}
		if discardedSampleIncrement > 0 && reason != nil {
			t.observed[stream].discardSamplemu.Lock()
			t.observed[stream].discardedSample[*reason] = atomic.NewInt64(discardedSampleIncrement)
			t.observed[stream].discardSamplemu.Unlock()
		}
	}
	t.obseveredMtx.Unlock()

	// If the maximum cardinality is hit all streams become `__overflow__`, the function would return true.
	// the origin labels ovserved time is not updated, but the overflow hash is updated.
	if !t.isOverflow && len(t.observed) > t.maxCardinality {
		t.isOverflow = true
		t.cleanupTracker()
		t.cooldownUntil = atomic.NewInt64(ts + t.cooldownDuration)
	}
}

func (t *Tracker) GetInactiveObservations(deadline int64) []string {
	if t == nil {
		return nil
	}

	// otherwise, we need to check all observations and clean up the ones that are inactive
	var invalidKeys []string
	t.obseveredMtx.RLock()
	defer t.obseveredMtx.RUnlock()
	for labkey, ob := range t.observed {
		if ob != nil && ob.lastUpdate != nil && ob.lastUpdate.Load() <= deadline {
			invalidKeys = append(invalidKeys, labkey)
		}
	}

	return invalidKeys
}

func (t *Tracker) UpdateMaxCardinality(limit int) {
	if t == nil {
		return
	}
	t.maxCardinality = limit
}

func (t *Tracker) UpdateCooldownDuration(cooldownDuration int64) {
	if t == nil {
		return
	}
	t.cooldownDuration = cooldownDuration
}

func (t *Tracker) updateMetrics() {
	if t == nil {
		return
	}

	if t.isOverflow {
		// if we are in overflow state, we only report the overflow metric
		t.activeSeriesPerUserAttribution.WithLabelValues(t.overflowLabels[:len(t.overflowLabels)-1]...).Set(float64(1))
		t.receivedSamplesAttribution.WithLabelValues(t.overflowLabels[:len(t.overflowLabels)-1]...).Add(float64(1))
		t.discardedSampleAttribution.WithLabelValues(t.overflowLabels...).Add(float64(1))
	} else {
		t.obseveredMtx.Lock()
		for key, c := range t.observed {
			if c != nil {
				keys := strings.Split(key, string(sep))
				keys = append(keys, t.userID)
				if c.activeSerie.Load() != 0 {
					t.activeSeriesPerUserAttribution.WithLabelValues(keys...).Add(float64(c.activeSerie.Swap(0)))
				}
				if c.receivedSample.Load() > 0 {
					t.receivedSamplesAttribution.WithLabelValues(keys...).Add(float64(c.receivedSample.Swap(0)))
				}
				c.discardSamplemu.Lock()
				for reason, cnt := range c.discardedSample {
					if cnt.Load() > 0 {
						t.discardedSampleAttribution.WithLabelValues(append(keys, reason)...).Add(float64(cnt.Swap(0)))
					}
				}
				c.discardSamplemu.Unlock()
			}
		}
		t.obseveredMtx.Unlock()
	}
}
