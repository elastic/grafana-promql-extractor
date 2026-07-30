// Package extract parses Grafana dashboard JSON and pulls out the PromQL
// expressions of panel targets and annotation queries backed by a
// Prometheus-family datasource. Grafana runs both against the datasource, so
// both are extracted.
//
// # Datasource resolution
//
// Whether a target is PromQL depends on its datasource, and dashboards
// reference datasources in several ways. [Extractor.Extract] resolves every
// reference to a concrete plugin type, in this order:
//
//  1. The target's own datasource, otherwise the enclosing panel's, otherwise
//     the row's, otherwise the instance's default datasource.
//  2. "-- Mixed --" defers to the per-target references. "-- Dashboard --",
//     "-- Grafana --" and __expr__ never carry PromQL and are skipped, as are
//     logs panels.
//  3. A reference to a dashboard variable ($datasource, ${DS_PROMETHEUS}) is
//     resolved through the dashboard's own templating list, or through
//     __inputs for exported dashboards.
//  4. A literal uid or name is looked up in the instance's datasource list,
//     which the caller supplies as a [DatasourceLookup].
//  5. Only when neither of those yields a type does the type recorded in the
//     reference itself ({"type": "prometheus", "uid": "..."}) decide. A uid is
//     what Grafana resolves at query time, so the live datasource list
//     outranks a type that went stale when the panel was last pointed
//     somewhere else.
//
// A reference that resolves to nothing, a bare uid pointing at a deleted
// datasource for example, is kept or dropped according to the
// IncludeUnresolved field, and counted in [Stats] either way.
//
// # What is left out
//
// Queries inside library panels live outside the dashboard document and cannot
// be read from it; they are counted instead. Dashboard variable queries such as
// label_values(up, job) are Grafana functions rather than PromQL, so they are
// left out as well.
package extract
