// Package analyze checks extracted PromQL queries against an Elasticsearch
// PromQL HTTP endpoint.
//
// # What it checks
//
// Each line of an export file is scrubbed of Grafana template variables, then
// sent to /_prometheus/api/v1/query_range. A query counts as supported when
// the endpoint returns Prometheus status success, even if the result is empty.
// Unsupported constructs return status error with a message that is grouped for
// the report.
//
// # Streaming
//
// A run scans the export twice without loading every query into memory. The first
// pass collects referenced metrics for remote-write seeding; the second checks
// each query and feeds a running report that only retains aggregated counts and
// error groups.
//
// # Grafana variables
//
// Before querying, [$var] range durations become [1m], and $var / ${var} label
// placeholders become bare identifiers, matching the Java PromqlCoverageAnalyzer.
package analyze
