package main

import (
	"flag"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"sync"
	"time"

	"encoding/json"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "elasticsearch"
)

type VecInfo struct {
	help   string
	labels []string
}

var (
	gaugeMetrics = map[string]string{
		"indices_fielddata_memory_size_bytes":    "Field data cache memory usage in bytes",
		"indices_filter_cache_memory_size_bytes": "Field data cache memory usage in bytes",
		"indices_docs":                           "Count of documents on this node",
		"indices_docs_deleted":                   "Count of deleted documents on this node",
		"indices_store_size_bytes":               "Current size of stored index data in bytes",
		"indices_segments_memory_bytes":          "Current memory size of segments in bytes",
		"jvm_mem_heap_committed_bytes":           "JVM heap memory currently committed",
		"jvm_mem_heap_used_bytes":                "JVM heap memory currently used",
		"jvm_mem_heap_max_bytes":                 "JVM heap memory max",
		"jvm_mem_non_heap_committed_bytes":       "JVM non-heap memory currently committed",
		"jvm_mem_non_heap_used_bytes":            "JVM non-heap memory currently used",
	}
	counterMetrics = map[string]string{
		"indices_fielddata_evictions":           "Evictions from field data",
		"indices_filter_cache_evictions":        "Evictions from field data",
		"indices_flush_total":                   "Total flushes",
		"indices_flush_time_ms_total":           "Cumulative flush time in milliseconds",
		"transport_rx_packets_total":            "Count of packets received",
		"transport_rx_size_bytes_total":         "Total number of bytes received",
		"transport_tx_packets_total":            "Count of packets sent",
		"transport_tx_size_bytes_total":         "Total number of bytes sent",
		"indices_store_throttle_time_ms_total":  "Throttle time for index store in milliseconds",
		"indices_indexing_index_total":          "Total index calls",
		"indices_indexing_index_time_ms_total":  "Cumulative index time in milliseconds",
		"indices_merges_total":                  "Total merges",
		"indices_merges_total_docs_total":       "Cumulative docs merged",
		"indices_merges_total_size_bytes_total": "Total merge size in bytes",
		"indices_merges_total_time_ms_total":    "Total time spent merging in milliseconds",
	}
	counterVecMetrics = map[string]*VecInfo{
		"jvm_gc_collections": &VecInfo{
			help:   "Count of JVM GC runs",
			labels: []string{"collector"},
		},
		"jvm_gc_collections_time_ms": &VecInfo{
			help:   "GC run time in milliseconds",
			labels: []string{"collector"},
		},
	}

	gaugeVecMetrics = map[string]*VecInfo{
		"breakers_estimated_size_bytes": &VecInfo{
			help:   "Estimated size in bytes of breaker",
			labels: []string{"breaker"},
		},
		"breakers_limit_size_bytes": &VecInfo{
			help:   "Limit size in bytes for breaker",
			labels: []string{"breaker"},
		},
	}
)

// Exporter collects Elasticsearch stats from the given server and exports
// them using the prometheus metrics package.
type Exporter struct {
	URI   string
	mutex sync.RWMutex

	up prometheus.Gauge

	gauges   map[string]*prometheus.GaugeVec
	counters map[string]*prometheus.CounterVec

	client *http.Client
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri string, timeout time.Duration) *Exporter {
	counters := make(map[string]*prometheus.CounterVec, len(counterMetrics)+len(counterVecMetrics))
	gauges := make(map[string]*prometheus.GaugeVec, len(gaugeMetrics)+len(gaugeVecMetrics))

	for name, info := range counterVecMetrics {
		counters[name] = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      name,
			Help:      info.help,
		}, append([]string{"cluster"}, info.labels...))
	}

	for name, info := range gaugeVecMetrics {
		gauges[name] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      name,
			Help:      info.help,
		}, append([]string{"cluster"}, info.labels...))
	}

	for name, help := range counterMetrics {
		counters[name] = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      name,
			Help:      help,
		}, []string{"cluster"})
	}

	for name, help := range gaugeMetrics {
		gauges[name] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      name,
			Help:      help,
		}, []string{"cluster"})
	}

	// Init our exporter.
	return &Exporter{
		URI: uri,

		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the Elasticsearch instance query successful?",
		}),

		counters: counters,
		gauges:   gauges,

		client: &http.Client{
			Transport: &http.Transport{
				Dial: func(netw, addr string) (net.Conn, error) {
					c, err := net.DialTimeout(netw, addr, timeout)
					if err != nil {
						return nil, err
					}
					if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
						return nil, err
					}
					return c, nil
				},
			},
		},
	}
}

// Describe describes all the metrics ever exported by the elasticsearch
// exporter. It implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.up.Desc()

	for _, vec := range e.counters {
		vec.Describe(ch)
	}

	for _, vec := range e.gauges {
		vec.Describe(ch)
	}
}

// Collect fetches the stats from configured elasticsearch location and
// delivers them as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	// Reset metrics.
	for _, vec := range e.gauges {
		vec.Reset()
	}

	for _, vec := range e.counters {
		vec.Reset()
	}

	resp, err := e.client.Get(e.URI)
	if err != nil {
		e.up.Set(0)
		log.Println("Error while querying Elasticsearch:", err)
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("Failed to read ES response body:", err)
		e.up.Set(0)
		return
	}

	e.up.Set(1)

	var allStats NodeStatsResponse
	err = json.Unmarshal(body, &allStats)
	if err != nil {
		log.Println("Failed to unmarshal JSON into struct:", err)
		return
	}

	// We only expose metrics for the local node, not the whole cluster.
	if l := len(allStats.Nodes); l != 1 {
		log.Println("Unexpected number of nodes returned:", l)
	}

	for _, stats := range allStats.Nodes {
		// GC Stats
		for collector, gcstats := range stats.JVM.GC.Collectors {
			e.counters["jvm_gc_collections"].WithLabelValues(allStats.ClusterName, collector).Set(float64(gcstats.CollectionCount))
			e.counters["jvm_gc_collections_time_ms"].WithLabelValues(allStats.ClusterName, collector).Set(float64(gcstats.CollectionTime))
		}

		// Breaker stats
		for breaker, bstats := range stats.Breakers {
			e.gauges["breakers_estimated_size_bytes"].WithLabelValues(allStats.ClusterName, breaker).Set(float64(bstats.EstimatedSize))
			e.gauges["breakers_limit_size_bytes"].WithLabelValues(allStats.ClusterName, breaker).Set(float64(bstats.LimitSize))
		}

		// JVM Memory Stats
		e.gauges["jvm_mem_heap_committed_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.JVM.Mem.HeapCommitted))
		e.gauges["jvm_mem_heap_used_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.JVM.Mem.HeapUsed))
		e.gauges["jvm_mem_heap_max_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.JVM.Mem.HeapMax))
		e.gauges["jvm_mem_non_heap_committed_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.JVM.Mem.NonHeapCommitted))
		e.gauges["jvm_mem_non_heap_used_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.JVM.Mem.NonHeapUsed))

		// Indices Stats
		e.gauges["indices_fielddata_memory_size_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.FieldData.MemorySize))
		e.gauges["indices_filter_cache_memory_size_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.FilterCache.MemorySize))
		e.counters["indices_filter_cache_evictions"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.FilterCache.Evictions))
		e.counters["indices_fielddata_evictions"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.FieldData.Evictions))

		e.gauges["indices_docs"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Docs.Count))
		e.gauges["indices_docs_deleted"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Docs.Deleted))

		e.gauges["indices_segments_memory_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Segments.Memory))

		e.gauges["indices_store_size_bytes"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Store.Size))
		e.counters["indices_store_throttle_time_ms_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Store.ThrottleTime))

		e.counters["indices_flush_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Flush.Total))
		e.counters["indices_flush_time_ms_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Flush.Time))

		e.counters["indices_indexing_index_time_ms_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Indexing.IndexTime))
		e.counters["indices_indexing_index_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Indexing.IndexTotal))

		e.counters["indices_merges_total_time_ms_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Merges.TotalTime))
		e.counters["indices_merges_total_size_bytes_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Merges.TotalSize))
		e.counters["indices_merges_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Indices.Merges.Total))

		// Transport Stats
		e.counters["transport_rx_packets_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Transport.RxCount))
		e.counters["transport_rx_size_bytes_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Transport.RxSize))
		e.counters["transport_tx_packets_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Transport.TxCount))
		e.counters["transport_tx_size_bytes_total"].WithLabelValues(allStats.ClusterName).Set(float64(stats.Transport.TxSize))
	}

	// Report metrics.
	ch <- e.up

	for _, vec := range e.counters {
		vec.Collect(ch)
	}

	for _, vec := range e.gauges {
		vec.Collect(ch)
	}
}

func main() {
	var (
		listenAddress = flag.String("web.listen-address", ":9108", "Address to listen on for web interface and telemetry.")
		metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
		esURI         = flag.String("es.uri", "http://localhost:9200", "HTTP API address of a Elasticsearch node.")
		esTimeout     = flag.Duration("es.timeout", 5*time.Second, "Timeout for trying to get stats from Elasticsearch.")
	)
	flag.Parse()

	*esURI = *esURI + "/_nodes/_local/stats"

	exporter := NewExporter(*esURI, *esTimeout)
	prometheus.MustRegister(exporter)

	log.Println("Starting Server:", *listenAddress)
	http.Handle(*metricsPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Elasticsearch Exporter</title></head>
             <body>
             <h1>Elasticsearch Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}