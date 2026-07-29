# AGENTS.md

A Go CLI that extracts PromQL queries from Grafana dashboards. `make build` builds it,
`make help` lists every target.

## Before committing

```bash
make lint   # gofmt and go vet, including the tagged builds
make test   # unit tests, a few seconds
```

## Test tiers

| Command | Needs | When |
| --- | --- | --- |
| `make test`, `make test-race` | nothing | every change |
| `make test-integration` | Docker | changes to the Grafana client, search paging or datasource resolution |
| `make test-scale` | a minute | changes to the pipeline or the writer |
| `make test-corpus` | grafana.com, once | changes to extraction or anonymization |
| `make test-throughput` | Docker and a warm corpus cache | only when timings are asked for |

`make test-all` runs every tier that needs no third-party service.

**Integration tests** (`integration` build tag) start Grafana with testcontainers and skip
themselves when Docker is not running. Point them at another release with
`make test-integration GRAFANA_IMAGE=grafana/grafana:11.6.6`.

**Corpus tests** (`corpus` build tag) check the extractor and `--anonymize` against the 1000
most downloaded dashboards on grafana.com. The dashboards are downloaded once into
`.cache/grafana-com`, one request every 500 ms, which takes about ten minutes; every later
run reads the cache and touches no network. Do not run this tier in CI, and do not
`make clean-corpus` without a reason, since that pays the download again. `make test-corpus
CORPUS=200` is the quick version. The full report and the extracted queries are left in the
cache directory for inspection.

**Throughput tests** need both tags and both prerequisites: they upload the cached corpus
into a dockerized Grafana and time the extraction, so `make test-corpus` has to have filled
the cache first.

## Fixtures

The unit and integration tiers share the dashboards in
`internal/testsupport/testdata/dashboards`. Each one has an `.expected` file listing the
exact output lines it must produce. After verifying a change in output is intended,
regenerate them:

```bash
go test ./internal/extract/ -run TestExtractFixtures -update
```
