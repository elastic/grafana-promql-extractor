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
// metrics-generic.prometheus-default through remote write when the export
// references any, then runs queries against a five-minute query_range window
// that ends at the same timestamp as the remote-write samples. Readiness and
// checks use the plain /_prometheus endpoint, so an export with only literal
// queries does not need a data stream to exist. One sample per series is
// enough for fields to appear in the mapping; exact label values matter less
// than the names being present.
//
// # Grafana variables
//
// Before querying, Grafana template variables are stripped so Elasticsearch sees
// valid PromQL: a range selector that contains a variable becomes [1m], and
// $var / ${var} / ${var:format} / [[var]] placeholders become the variable name.
// $1 in label_replace is left alone; it is a PromQL capture, not a Grafana
// variable. A variable in function position stands for a function the export
// does not name, so it gets one that accepts its argument: rate when the
// argument is a range vector (${metric:value}(x[5m]) for a rate / increase
// dropdown), avg when it is an instant vector (${agg}(x) for an aggregation
// dropdown). The Java PromqlCoverageAnalyzer only handled $var / ${var}.
//
// # Docker
//
// --es-version or --es-image starts a single-node Elasticsearch container with
// testcontainers, populates the Prometheus data stream, waits until the PromQL
// query_range endpoint accepts requests, runs the analysis, and stops the
// container when the command exits. The two flags are mutually exclusive:
// --es-version takes a full version such as 9.5.0 and resolves to
// docker.elastic.co/elasticsearch/elasticsearch:<version>, while --es-image
// takes a full image reference. PromQL requires Elasticsearch 9.4 or later.
package analyze
