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

Download a binary for your platform from the [releases page](https://github.com/elastic/grafana-promql-extractor/releases),
or build from source:

```bash
go install github.com/elastic/grafana-promql-extractor@latest
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

A service account token is the recommended credential, and a **Viewer** role is enough:
the tool only reads dashboards and datasource metadata. Basic auth works too, through
`--user` and `--password`. Every credential flag has an environment variable equivalent —
`GRAFANA_URL`, `GRAFANA_TOKEN`, `GRAFANA_USER`, `GRAFANA_PASSWORD`, `GRAFANA_ORG_ID` — and
those are worth preferring so credentials do not end up in your shell history.

`--help` lists every flag with its default. The ones reached for most often are
`--max-dashboards` to take a sample, `--dashboards-per-file` to split a large instance,
`--compress=false` for plain text, `--anonymize` to make the output shareable, and
`--folder-uid` or `--tag` to narrow what is read.

```bash
# A sample of 500 dashboards, uncompressed
grafana-promql-extractor --max-dashboards 500 --compress=false -o sample.txt

# Split a large instance into files of 10k dashboards each
grafana-promql-extractor --dashboards-per-file 10000

# Basic auth instead of a service account token, restricted to one folder
grafana-promql-extractor --user admin --password secret --folder-uid abc123
```

### Resuming an interrupted run

An interrupted run reports the page it stopped on. Resume it without losing what was
already written by combining `--start-page` with `--append`:

```bash
grafana-promql-extractor --start-page 7 --append
```

With `--dashboards-per-file`, the resumed run continues the numbering rather than reopening
the last file, so files a consumer already picked up are never modified. The file the
interrupted run was working on keeps whatever it holds and stays below the limit.

## Scale

Large instances are the case this was built for. Dashboards are read in batches where
Grafana serves them that way, and streamed through a pool of workers into a single writer,
so memory does not grow with the instance: a run over 400,000 dashboards peaks at some tens
of megabytes, much the same as a run over 50.

Throughput is set by Grafana rather than by the extractor. Expect roughly 600 dashboards per
second against an instance on the same machine, and less over a network; the progress line
reports what a run is actually achieving. A run that stops early can be resumed, and
`--bulk off` falls back to one request per dashboard for an instance that answers a batched
read oddly.

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

Besides the other query languages, a few things are left out because Grafana keeps them
outside the dashboard document: queries in library panels, and dashboard variable queries
such as `label_values(up, job)`, which are Grafana functions rather than PromQL. Queries
whose datasource cannot be identified at all are kept, on the assumption that an `expr`
field is most likely PromQL; `--include-unresolved=false` drops them instead. The summary
printed at the end of a run counts everything it skipped, and why.

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

The mapping is a salted digest whose salt is random per run and never written down, so
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

## Development

```bash
make build     # build ./bin/grafana-promql-extractor
make lint      # gofmt and go vet
make test      # unit tests
make test-all  # every tier that needs no third-party service
```

`make help` lists every target. [AGENTS.md](AGENTS.md) covers what each test tier checks,
what it needs, and when it is worth running, along with the fixture set the unit and
integration tiers share.

How it works is documented next to the code it describes:

```bash
go doc ./internal/cli      # the pipeline, memory and throughput, the batched listing
go doc ./internal/extract  # how a target's datasource decides whether it is PromQL
go doc ./internal/anonymize
```

## License

Apache 2.0
