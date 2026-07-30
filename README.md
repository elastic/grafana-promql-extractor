# grafana-promql-extractor

Extract the PromQL queries from every dashboard of a Grafana instance into a flat file.

Output is one query per line, prefixed by the dashboard UID and a semicolon:

```
cdf6f5b7;sum(rate(http_requests_total[5m]))
a1b2c3d4;http_requests_total{job="api-server"}
```

Only queries backed by a Prometheus-family datasource are included. Loki, CloudWatch,
Elasticsearch and other query languages are filtered out by resolving each panel's and
target's datasource reference to a concrete plugin type. Panel targets and annotation
queries are both extracted, since Grafana runs both against the datasource.

## Install

Download a binary for your platform from the [releases page](https://github.com/felixbarny/grafana-promql-extractor/releases),
or build from source:

```bash
go install github.com/felixbarny/grafana-promql-extractor@latest
```

The binary is statically linked with no runtime dependencies.

## Usage

```bash
export GRAFANA_URL=https://grafana.example.com
export GRAFANA_TOKEN=glsa_xxxxxxxx

grafana-promql-extractor
```

That writes `promql-queries.txt.gz` and reports progress on stderr:

```
 24.9% | 12,431/50,000 dashboards | 84,203 queries | 61/s | 3m22s | ~10m left
```

More examples:

```bash
# A sample of 500 dashboards, uncompressed
grafana-promql-extractor --max-dashboards 500 --compress=false -o sample.txt

# Split a large instance into files of 10k dashboards each
grafana-promql-extractor --dashboards-per-file 10000

# Basic auth instead of a service account token, restricted to one folder
grafana-promql-extractor --user admin --password secret --folder-uid abc123
```

### Credentials

A service account token is the recommended option. A **Viewer** role is enough: the tool
only reads dashboards and datasource metadata.

| Flag | Environment variable |
| --- | --- |
| `--url` | `GRAFANA_URL` |
| `--token` | `GRAFANA_TOKEN` |
| `--user` | `GRAFANA_USER` |
| `--password` | `GRAFANA_PASSWORD` |
| `--org-id` | `GRAFANA_ORG_ID` |

Prefer the environment variables so credentials do not end up in your shell history.

### Options

Run `grafana-promql-extractor --help` for the full list. The ones that matter most:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--max-dashboards`, `-n` | `0` (all) | Stop after this many dashboards |
| `--dashboards-per-file` | `0` (one file) | Split the output every N dashboards |
| `--compress` | `true` | gzip the output and append `.gz` |
| `--output`, `-o` | `promql-queries.txt` | Output path |
| `--append` | `false` | Add to existing output files instead of replacing them |
| `--concurrency`, `-c` | `8` | Dashboards fetched in parallel |
| `--page-size` | `1000` | Dashboards per search request, max 5000 |
| `--datasource-types` | `prometheus,grafana-amazonprometheus-datasource,grafana-mimir-datasource` | Plugin types treated as PromQL sources |
| `--include-unresolved` | `true` | Keep queries whose datasource cannot be resolved |
| `--dedupe` | `true` | Drop repeated identical queries within a dashboard |
| `--anonymize` | `false` | Replace the identifiers of every query with pseudonyms |
| `--start-page` | `1` | Resume an interrupted run at a later search page |
| `--bulk` | `auto` | Read whole dashboards [a page at a time](#reading-dashboards-in-pages) where Grafana serves them: `auto`, `on`, `off` |
| `--progress` | `auto` | `auto`, `always` or `never` |

An interrupted run reports the page it stopped on. Resume it without losing what was
already written by combining `--start-page` with `--append`:

```bash
grafana-promql-extractor --start-page 7 --append
```

With `--dashboards-per-file`, the resumed run continues the numbering rather than reopening
the last file, so files a consumer already picked up are never modified. The file the
interrupted run was working on keeps whatever it holds and stays below the limit.

## Output format

Each line is `<dashboard-uid>;<query>`. Split on the **first** semicolon only: a PromQL
label value may legitimately contain one, while a dashboard UID never does.

```bash
zcat promql-queries.txt.gz | cut -d';' -f2-          # queries only
zcat promql-queries.txt.gz | cut -d';' -f1 | uniq -c # queries per dashboard
```

Multi-line expressions are collapsed onto a single line, so every query occupies exactly
one line. Dashboards are fetched concurrently, so line order is not stable across runs.
Dashboards without PromQL queries contribute no lines.

## Sharing the output

Queries name what an organization runs. `--anonymize` replaces every identifier with a
pseudonym, so the output can be handed to someone outside it:

```bash
grafana-promql-extractor --anonymize -o shareable.txt
```

```
dash_9e3a17b204;sum by (label_1f7c4a0e83) (rate(metric_5b8d02c9a1{label_44e1b7cd90=~"$var_7c0a91fe32"}[$__rate_interval]))
```

Metric names, label names, label values, dashboard variable names and dashboard UIDs are
replaced. What is the same in every Grafana instance is kept, so the queries stay worth
analyzing: functions, aggregations, operators, durations, numbers, regular expression
syntax, the reserved labels `le` and `quantile`, and Grafana's own `$__rate_interval` and
friends. An identifier maps to the same pseudonym everywhere in the output, so grouping and
counting still work.

The mapping is a salted digest, and the salt is random per run and never written down, so
nobody can turn a pseudonym back into a name, not even by guessing likely names. That also
means two runs produce unrelated pseudonyms. To compare runs, or to resume one with
`--append`, supply your own secret and keep it out of your shell history:

```bash
export GRAFANA_ANONYMIZE_SALT=$(openssl rand -hex 32)
grafana-promql-extractor --anonymize
```

Two caveats worth knowing before sharing. Anything the tool cannot recognize as PromQL is
pseudonymized rather than kept, so a query in a dialect such as MetricsQL loses the parts
of its syntax that Prometheus does not have. And stderr is not anonymized: `--verbose`
logs real dashboard UIDs for failures.

## How it works at scale

Dashboards are enumerated page by page and fetched by a worker pool that feeds a single
writer, so only `--concurrency` dashboard documents exist in memory at any moment.
Extracting 400,000 dashboards one by one uses the same memory as extracting 50: a live heap
of about 1.3 MiB, with a peak of roughly 6 MiB including garbage awaiting collection.
Reading them [in pages](#reading-dashboards-in-pages) costs two things more. A page carries
whole documents rather than uids, so a bounded number of them is in flight instead of being
streamed straight into the parser: over the thousand most downloaded community dashboards,
which average 61 KiB and reach 1.3 MiB, that took the peak from 4.7 to 9.5 MiB. It also
remembers one uid per dashboard until it can check what the pages left out, which adds about
12 MiB for 50,000 dashboards and 47 MiB for 400,000. Neither grows with the page size.
Run `make test-scale SCALE=50000` and `make test-throughput` to reproduce the measurements.

Grafana, not the extractor, sets the pace: against a local Grafana, expect a throughput of
around 600 dashboards per second at the default concurrency, and around 800 where whole
dashboards can be read a page at a time. A remote instance answers slower, and the progress
line reports the rate a run actually achieves.

### Reading dashboards in pages

Grafana 12 added an API that returns whole dashboards a page at a time. Where it is
available, 50,000 dashboards cost around a thousand requests instead of 50,000, which is
both faster and considerably gentler on the instance, so `--bulk auto` uses it by default.

There is a catch worth knowing about. Grafana assembles a page by reading a batch of
dashboards and then asking its authorization service, in one call for the whole batch, which
of them the caller may see. If that call fails rather than answers, every dashboard in the
batch looks invisible and is left out of the page, and the reply is still a 200. A listing
can come back missing hundreds of dashboards, or all of them, with nothing in it looking
wrong.

So a run does not take a listing at its word. Once the pages are exhausted, it asks
`/api/search` what the instance holds and fetches anything the pages never delivered:

```
  left out of the pages: 999, fetched one by one instead
```

The worst case is therefore a slow run rather than a file with holes: walking pages that
return nothing takes its own time, and the dashboards are then fetched one by one on top of
that. Output is identical either way.

In practice this only showed up on SQLite, which is the default for a container and for a
development instance, but not for anything holding 50,000 dashboards. Under a write load
heavy enough to make SQLite lose every listing, the same Grafana on Postgres served fifteen
runs without dropping a single dashboard, and `/api/search` never lost one on either.

Two limits remain. `--max-dashboards` stops a run before it knows what it should have seen,
and `--continue-token` resumes a listing whose earlier pages this run never saw, so neither
can be checked; both say so when they run. `--folder-uid`, `--tag` and `--start-page` cannot
be expressed as a listing at all and keep to one request per dashboard.

```mermaid
flowchart LR
  Search["/api/search\npage by page"] -->|uid channel| Pool
  Datasources["/api/datasources\nplugin types"] --> Pool
  Pool["worker pool\nfetch, extract, dedupe"] -->|result channel| Writer
  Writer["single writer\ngzip, rotate"] --> Files["promql-queries-00001.txt.gz"]
```

Requests that fail with a 429 or a 5xx are retried with exponential backoff, honoring
`Retry-After`. A dashboard that cannot be fetched is counted and skipped rather than
aborting the run; pass `--fail-fast` for the opposite. Interrupting with Ctrl-C flushes
what has been written and prints the summary.

## Datasource resolution

Whether a target is PromQL depends on its datasource, and dashboards reference datasources
in several ways. Resolution happens in this order:

1. The target's own `datasource`, otherwise the enclosing panel's, otherwise the row's,
   otherwise the instance's default datasource.
2. `-- Mixed --` defers to the per-target references. `-- Dashboard --`, `-- Grafana --`
   and `__expr__` never carry PromQL and are skipped, as are `logs` panels.
3. A reference to a dashboard variable (`$datasource`, `${DS_PROMETHEUS}`) is resolved
   through the dashboard's own `templating` list, or through `__inputs` for exported
   dashboards.
4. A literal uid or name is looked up in the instance's datasource list.
5. Only when neither of those yields a type does the `type` recorded in the reference
   itself (`{"type": "prometheus", "uid": "..."}`) decide. A uid is what Grafana resolves
   at query time, so the live datasource list outranks a type that went stale when the
   panel was last pointed somewhere else.

Datasource types come from `/api/datasources`. Tokens that may not read that endpoint fall
back to `/api/frontend/settings`, which exposes the same plugin types to any authenticated
user.

If a reference cannot be resolved at all, for example a bare uid pointing at a deleted
datasource, the expression is kept by default and counted separately in the summary. Use
`--include-unresolved=false` to drop those instead.

Queries inside library panels are not visible in the dashboard document and are therefore
not extracted; the summary reports how many were encountered. Dashboard variable queries
such as `label_values(up, job)` are Grafana functions rather than PromQL and are left out
as well.

## Development

```bash
make build             # build ./bin/grafana-promql-extractor
make test-race         # unit tests
make test-integration  # integration tests against a dockerized Grafana
make test-versions     # real dashboards through Grafana 11, 12 and 13
make test-scale        # 50k dashboard memory check
make test-corpus       # validate against the top 1000 grafana.com dashboards
make test-throughput   # time extracting them from a dockerized Grafana
make test-all          # every tier that needs no third-party service
```

The integration tests start a real Grafana with [testcontainers](https://golang.testcontainers.org/),
provision Prometheus, Loki and CloudWatch datasources, upload the dashboard fixtures from
`internal/testsupport/testdata/dashboards`, and assert on the produced files. They are
behind the `integration` build tag and skip themselves when Docker is unavailable. Point
them at another version with `make test-integration GRAFANA_IMAGE=grafana/grafana:11.6.6`.

Unit tests and integration tests share one fixture set and one set of expectations, so a
dashboard added to `internal/testsupport/testdata/dashboards` is covered by both tiers.
Each fixture has a `.expected` file listing the exact output lines it must produce;
regenerate them with `go test ./internal/extract/ -run TestExtractFixtures -update` after
verifying the change is intended.

### Validating against real dashboards

Fixtures only prove the extractor does what its author expected. `make test-corpus`
checks it against the most downloaded dashboards on grafana.com instead, serving them
from a fake Grafana and running the real command over them. Four independent signals
have to agree:

- every extracted query parses as PromQL once Grafana variables are interpolated,
- no extracted query contains a pipe, which PromQL has no operator for and every LogQL
  pipeline stage starts with,
- dashboards that grafana.com lists under Prometheus yield at least one query,
- every `expr` a schema-agnostic walk of the raw JSON finds in a panel target or an
  annotation either appears in the output or has an explicable reason not to.

The last one is what surfaced that annotation queries were being ignored.

The same corpus validates `--anonymize`, by extracting it twice and comparing. No word of a
dashboard may survive in the anonymized output, judged against a vocabulary of public
PromQL and Grafana words assembled from a parser rather than from the anonymizer's own
lists. And a query has to parse the same way before and after, which is what caught `\d`
losing its `d` to a pseudonym, `(?i)` losing its flag, and the unit of `[${__range_s}s]`
being mistaken for a metric name.

The test is behind the `corpus` build tag, so `go test ./...` never runs it and CI never
touches grafana.com. Dashboards are downloaded once, one request at a time with 500 ms in
between, into `.cache/grafana-com`, which `.gitignore` excludes; the first run takes about
ten minutes and later runs are instant. It also leaves the full report and the extracted
queries there for inspection.

```bash
make test-corpus CORPUS=200      # fewer dashboards
make clean-corpus                # drop the cache
CORPUS_REQUEST_INTERVAL=2s make test-corpus
```

`make test-throughput` reuses the same cache for a different question: it uploads the
dashboards into a dockerized Grafana and times the extraction at several concurrencies,
plain and anonymized, recording peak heap for each. It needs both build tags and both
prerequisites, Docker and a warm cache, and it fails only if the settings change what is
extracted or the rate collapses.

`make test-versions` answers a third: it stores community dashboards in Grafana 11.6.6,
12.4.0 and 13.0.1 in turn and checks that all three yield the same queries, read whichever
way each release allows. A fake serves back whatever it was given, so it cannot show what a
release does to a dashboard between saving it and serving it again; only real releases can,
and only they can show that reading dashboards a page at a time gets the same result as
fetching them one by one on the releases that offer both.

## License

Apache 2.0
