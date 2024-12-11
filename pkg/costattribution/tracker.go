// SPDX-License-Identifier: AGPL-3.0-only

package costattribution

import (
	"bytes"
	"sort"
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
	labelValues     []string
}

const (
	TrackerLabel       = "tracker"
	TenantLabel        = "tenant"
	defaultTrackerName = "cost-attribution"
)

type Tracker struct {
	userID                         string
	caLabels                       []string
	caLabelMap                     map[string]int
	maxCardinality                 int
	activeSeriesPerUserAttribution *prometheus.GaugeVec
	receivedSamplesAttribution     *prometheus.CounterVec
	discardedSampleAttribution     *prometheus.CounterVec

	overflowLabels []string
	// obseveredMtx protects the observed map
	obseveredMtx sync.RWMutex
	observed     map[uint64]*Observation

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
	caLabelMap := make(map[string]int, len(trackedLabels))
	for i, label := range trackedLabels {
		caLabelMap[label] = i
	}
	m := &Tracker{
		userID:         userID,
		caLabels:       trackedLabels,
		caLabelMap:     caLabelMap,
		maxCardinality: limit,
		obseveredMtx:   sync.RWMutex{},
		observed:       map[uint64]*Observation{},
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

var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// sep is used to separate the labels in the key, it is not a valid label caracter
const sep = rune(0x80)

func (t *Tracker) cleanupTrackerAttribution(key uint64) {
	if t == nil {
		return
	}

	t.obseveredMtx.RLock()
	lvs := t.observed[key].labelValues
	t.obseveredMtx.RUnlock()

	// vals := strings.Split(key, string(sep))
	// vals = append(vals, t.userID)
	t.activeSeriesPerUserAttribution.DeleteLabelValues(lvs...)
	t.receivedSamplesAttribution.DeleteLabelValues(lvs...)

	// except for discarded sample metrics, there is reason label that is not part of the key, we need to delete all partial matches
	filter := prometheus.Labels{}
	for i := 0; i < len(t.caLabels); i++ {
		filter[t.caLabels[i]] = lvs[i]
	}
	filter[TenantLabel] = t.userID
	t.discardedSampleAttribution.DeletePartialMatch(filter)

	t.obseveredMtx.Lock()
	delete(t.observed, key)
	t.obseveredMtx.Unlock()
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

	labelValues := make([]string, len(t.caLabels)+2)
	lbls.Range(func(l labels.Label) {
		if idx, ok := t.caLabelMap[l.Name]; ok {
			labelValues[idx] = l.Value
		}
	})

	labelValues[len(labelValues)-2] = t.userID

	for i := 0; i < len(labelValues)-2; i++ {
		if labelValues[i] == "" {
			labelValues[i] = missingValue
		}
	}

	if reason != nil {
		labelValues[len(labelValues)-1] = *reason
	}

	var stream uint64
	stream, t.hashBuffer = lbls.HashForLabels(t.hashBuffer, t.caLabels...)

	t.obseveredMtx.Lock()
	defer t.obseveredMtx.Unlock()

	t.updateOverflow(stream, ts, activeSeriesIncrement, receviedSampleIncrement, discardedSampleIncrement, labelValues)
}

func (t *Tracker) updateOverflow(stream uint64, ts int64, activeSeriesIncrement, receviedSampleIncrement, discardedSampleIncrement int64, lvs []string) {
	if t == nil {
		return
	}

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
		if discardedSampleIncrement > 0 {
			o.discardSamplemu.Lock()
			o.discardedSample[lvs[len(lvs)-1]] = atomic.NewInt64(discardedSampleIncrement)
			o.discardSamplemu.Unlock()
		}
	} else if len(t.observed) < t.maxCardinality*2 {
		t.observed[stream] = &Observation{
			lastUpdate:      atomic.NewInt64(ts),
			activeSerie:     atomic.NewInt64(activeSeriesIncrement),
			receivedSample:  atomic.NewInt64(receviedSampleIncrement),
			discardedSample: map[string]*atomic.Int64{},
			discardSamplemu: sync.RWMutex{},
			labelValues:     lvs[:len(lvs)-1],
		}
		if discardedSampleIncrement > 0 {
			t.observed[stream].discardSamplemu.Lock()
			t.observed[stream].discardedSample[lvs[len(lvs)-1]] = atomic.NewInt64(discardedSampleIncrement)
			t.observed[stream].discardSamplemu.Unlock()
		}
	}

	// If the maximum cardinality is hit all streams become `__overflow__`, the function would return true.
	// the origin labels ovserved time is not updated, but the overflow hash is updated.
	if !t.isOverflow && len(t.observed) > t.maxCardinality {
		t.isOverflow = true
		t.cleanupTracker()
		t.cooldownUntil = atomic.NewInt64(ts + t.cooldownDuration)
	}
}

func (t *Tracker) GetInactiveObservations(deadline int64) []uint64 {
	if t == nil {
		return nil
	}

	// otherwise, we need to check all observations and clean up the ones that are inactive
	var invalidKeys []uint64
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
		for _, c := range t.observed {
			if c != nil {
				// keys := strings.Split(key, string(sep))
				// keys = append(keys, t.userID)
				if c.activeSerie.Load() != 0 {
					t.activeSeriesPerUserAttribution.WithLabelValues(c.labelValues...).Add(float64(c.activeSerie.Swap(0)))
				}
				if c.receivedSample.Load() > 0 {
					t.receivedSamplesAttribution.WithLabelValues(c.labelValues...).Add(float64(c.receivedSample.Swap(0)))
				}
				c.discardSamplemu.Lock()
				for reason, cnt := range c.discardedSample {
					if cnt.Load() > 0 {
						t.discardedSampleAttribution.WithLabelValues(append(c.labelValues, reason)...).Add(float64(cnt.Swap(0)))
					}
				}
				c.discardSamplemu.Unlock()
			}
		}
		t.obseveredMtx.Unlock()
	}
}
