// Package analyze checks extracted PromQL queries against an Elasticsearch
// PromQL HTTP endpoint.
//
// # What it checks
//
// Each line of an export file is scrubbed of Grafana template variables, then
// sent to /_prometheus/api/v1/query_range. A query counts as supported when
// the endpoint returns Prometheus status success, even if the result is empty.
// Unsupported constructs return status error with a message that is grouped for
// the report. Queries are sent with GET; form-encoded POST needs HTTP TLS at
// the Elasticsearch node, which the Docker analyze path does not use.
//
// # Streaming
//
// A run scans the export twice without loading every query into memory. The first
// pass collects referenced metrics for remote-write seeding; the second checks
// each query and feeds a running report that only retains aggregated counts and
// error groups.
//
// # Populating the index
//
// analyze starts Docker, seeds referenced metrics and labels into
// metrics-generic.prometheus-default through remote write, then runs queries
// against a five-minute query_range window that ends at the same timestamp as
// the remote-write samples. One sample per series is enough for fields to appear in the mapping; exact label values
// matter less than the names being present.
//
// # Grafana variables
//
// Before querying, [$var] range durations become [1m], and $var / ${var} label
// placeholders become bare identifiers, matching the Java PromqlCoverageAnalyzer.
//
// # Docker
//
// --es-version or --es-image starts a single-node Elasticsearch container with
// testcontainers, populates the Prometheus data stream, waits until the PromQL
// query_range endpoint accepts requests, runs the analysis, and stops the
// container when the command exits. The two flags are mutually exclusive:
// --es-version resolves to docker.elastic.co/elasticsearch/elasticsearch:<version>,
// while --es-image takes a full image reference. PromQL requires Elasticsearch
// 9.4 or later.
package analyze
