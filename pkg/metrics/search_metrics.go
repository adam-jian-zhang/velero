/*
Copyright The Velero Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	searchIndexedBackups         = "search_indexed_backups"
	searchIndexedResources       = "search_indexed_resources"
	searchIndexAttemptsTotal     = "search_index_attempts_total"
	searchIndexDurationSeconds   = "search_index_duration_seconds"
	searchDeleteAttemptsTotal    = "search_delete_attempts_total"
	searchRequestTotal           = "search_request_total"
	searchRequestDurationSeconds = "search_request_duration_seconds"
	searchQueryDurationSeconds   = "search_query_duration_seconds"
	searchReady                  = "search_ready"

	searchResultLabel  = "result"
	searchPhaseLabel   = "phase"
	searchBackendLabel = "backend"
)

// RegisterSearchMetrics adds search-related Prometheus collectors.
func (m *ServerMetrics) RegisterSearchMetrics() {
	if m == nil || m.metrics == nil {
		return
	}
	m.metrics[searchIndexedBackups] = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      searchIndexedBackups,
			Help:      "Number of backups currently indexed for search",
		},
	)
	m.metrics[searchIndexedResources] = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      searchIndexedResources,
			Help:      "Total resource rows in the search index",
		},
	)
	m.metrics[searchIndexAttemptsTotal] = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      searchIndexAttemptsTotal,
			Help:      "Total IndexBackup attempts by outcome",
		},
		[]string{searchResultLabel},
	)
	m.metrics[searchIndexDurationSeconds] = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      searchIndexDurationSeconds,
			Help:      "IndexBackup latency in seconds",
			Buckets:   prometheus.DefBuckets,
		},
	)
	m.metrics[searchDeleteAttemptsTotal] = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      searchDeleteAttemptsTotal,
			Help:      "Total DeleteBackup attempts by outcome",
		},
		[]string{searchResultLabel},
	)
	m.metrics[searchRequestTotal] = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      searchRequestTotal,
			Help:      "SearchRequest reconciles by terminal phase",
		},
		[]string{searchPhaseLabel},
	)
	m.metrics[searchRequestDurationSeconds] = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      searchRequestDurationSeconds,
			Help:      "SearchRequest processing latency in seconds",
			Buckets:   prometheus.DefBuckets,
		},
	)
	m.metrics[searchQueryDurationSeconds] = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      searchQueryDurationSeconds,
			Help:      "Underlying store query latency in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{searchBackendLabel},
	)
	m.metrics[searchReady] = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      searchReady,
			Help:      "1 when the search index is ready to serve queries, 0 otherwise",
		},
	)

	// Register newly added collectors. Prefer Register over MustRegister so
	// repeated enablement / tests do not panic on AlreadyRegistered.
	for _, name := range []string{
		searchIndexedBackups, searchIndexedResources, searchIndexAttemptsTotal,
		searchIndexDurationSeconds, searchDeleteAttemptsTotal, searchRequestTotal,
		searchRequestDurationSeconds, searchQueryDurationSeconds, searchReady,
	} {
		if c, ok := m.metrics[name]; ok {
			if err := prometheus.Register(c); err != nil {
				if _, already := err.(prometheus.AlreadyRegisteredError); !already {
					panic(err)
				}
			}
		}
	}
}

func resultLabel(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}

func (m *ServerMetrics) ObserveSearchIndex(ok bool, seconds float64) {
	if m == nil || m.metrics == nil {
		return
	}
	if c, okm := m.metrics[searchIndexAttemptsTotal].(*prometheus.CounterVec); okm {
		c.WithLabelValues(resultLabel(ok)).Inc()
	}
	if h, okm := m.metrics[searchIndexDurationSeconds].(prometheus.Histogram); okm {
		h.Observe(seconds)
	}
}

func (m *ServerMetrics) ObserveSearchDelete(ok bool) {
	if m == nil || m.metrics == nil {
		return
	}
	if c, okm := m.metrics[searchDeleteAttemptsTotal].(*prometheus.CounterVec); okm {
		c.WithLabelValues(resultLabel(ok)).Inc()
	}
}

func (m *ServerMetrics) ObserveSearchRequest(ok bool, seconds float64) {
	if m == nil || m.metrics == nil {
		return
	}
	phase := "Processed"
	if !ok {
		phase = "Failed"
	}
	if c, okm := m.metrics[searchRequestTotal].(*prometheus.CounterVec); okm {
		c.WithLabelValues(phase).Inc()
	}
	if h, okm := m.metrics[searchRequestDurationSeconds].(prometheus.Histogram); okm {
		h.Observe(seconds)
	}
}

func (m *ServerMetrics) ObserveSearchQuery(backend string, seconds float64) {
	if m == nil || m.metrics == nil {
		return
	}
	if h, okm := m.metrics[searchQueryDurationSeconds].(*prometheus.HistogramVec); okm {
		h.WithLabelValues(backend).Observe(seconds)
	}
}

func (m *ServerMetrics) SetSearchReady(ready bool) {
	if m == nil || m.metrics == nil {
		return
	}
	if g, ok := m.metrics[searchReady].(prometheus.Gauge); ok {
		if ready {
			g.Set(1)
		} else {
			g.Set(0)
		}
	}
}

func (m *ServerMetrics) SetSearchIndexedBackups(n float64) {
	if m == nil || m.metrics == nil {
		return
	}
	if g, ok := m.metrics[searchIndexedBackups].(prometheus.Gauge); ok {
		g.Set(n)
	}
}

func (m *ServerMetrics) SetSearchIndexedResources(n float64) {
	if m == nil || m.metrics == nil {
		return
	}
	if g, ok := m.metrics[searchIndexedResources].(prometheus.Gauge); ok {
		g.Set(n)
	}
}
