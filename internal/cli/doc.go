// Package cli implements the command line interface.
//
// The root command groups two subcommands: extract pulls PromQL from Grafana,
// and analyze checks an export file against Elasticsearch PromQL.
//
// # The extract pipeline
//
// A run enumerates dashboards, fetches them through a worker pool and funnels
// the extracted queries through a single writer, so that only --concurrency
// dashboard documents are ever in memory:
//
//	dashboard listing          /api/datasources
//	  page by page               plugin types
//	        |                          |
//	        +------------+-------------+
//	                     v
//	          worker pool (--concurrency)
//	          fetch, extract, dedupe
//	                     |
//	                     v
//	          single writer: gzip, rotate
//	                     |
//	                     v
//	     promql-queries-00001.txt.gz, ...
//
// Requests that fail with a 429 or a 5xx are retried with exponential backoff,
// honoring Retry-After. A dashboard that cannot be fetched is counted and
// skipped rather than aborting the run, unless --fail-fast says otherwise.
// Interrupting flushes what has been written, prints the summary and reports
// where to resume.
//
// # Memory and throughput
//
// Extracting 400,000 dashboards costs roughly what extracting 50 does, because
// the documents stream through the pool instead of being collected. Fetching
// them one by one peaks at a handful of megabytes whatever the size of the
// instance. Reading them in pages costs two things more: whole documents are in
// flight rather than streamed straight into the parser, which took the peak
// from 4.7 to 9.5 MiB over the thousand most downloaded community dashboards,
// and the uids the run remembers to check the listing with add about 12 MiB per
// 50,000 dashboards. That puts a bulk run around 20 MiB at 50,000 dashboards
// and 60 MiB at 400,000. Neither grows with the page size.
//
// Grafana, not the extractor, sets the pace: against a local instance expect
// around 600 dashboards per second at the default concurrency, or around 800
// where whole dashboards can be read a page at a time. TestScale here and the
// throughput test in ./integration reproduce both measurements; AGENTS.md says
// what each needs.
//
// # Reading dashboards in pages
//
// Grafana 12 serves whole dashboards a page at a time, which turns 50,000
// requests into around a thousand, so --bulk auto uses it wherever the instance
// offers it. Client.ListDashboards in internal/grafana records why it asks for
// the v0alpha1 version of that API rather than a later one.
//
// Such a listing can come back short without looking wrong. Grafana assembles a
// page by reading a batch of dashboards and then asking its authorization
// service, in one call for the whole batch, which of them the caller may see. If
// that call fails rather than answers, every dashboard in the batch looks
// invisible and is left out of the page, and the reply is still a 200. In
// practice this showed up on SQLite under write load, which is the default for a
// container but not for anything holding 50,000 dashboards; the same Grafana on
// Postgres served fifteen runs without dropping a dashboard, and /api/search
// never lost one on either.
//
// So a run does not take a listing at its word. Once the pages are exhausted,
// pipeline.fetchMissing enumerates /api/search and fetches whatever the pages
// never delivered, which makes the worst case a slow run rather than a file with
// holes. Two options cannot be checked this way, and both say so when they run:
// --max-dashboards stops a run before it knows what it should have seen, and
// --continue-token resumes a listing whose earlier pages this run never saw.
// --folder-uid, --tag and --start-page cannot be expressed as a listing at all
// and keep to one request per dashboard.
package cli
