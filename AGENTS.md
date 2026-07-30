# AGENTS.md

A Go CLI that extracts PromQL queries from Grafana dashboards. `make build` builds it,
`make help` lists every target.

## Before committing

```bash
make lint   # gofmt and go vet, including the tagged builds
make test   # unit tests, a few seconds
```

## Where documentation goes

The README covers what a user can observe: the flags worth knowing, the output format, what
ends up in the file and what does not, rough expectations at scale. How it works goes in a
package doc comment, in a `doc.go` when it runs long, where `go doc` finds it and where it
gets updated along with the code. Do not restate `--help` in a table, and link a mechanism
rather than explain it twice.

## Test tiers

| Command | Needs | When |
| --- | --- | --- |
| `make test`, `make test-race` | nothing | every change |
| `make test-integration` | Docker | changes to the Grafana client, search paging or datasource resolution |
| `make test-versions` | Docker and a warm corpus cache | changes to how dashboards are read or stored |
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

**Cross-version tests** (`make test-versions`, both tags) store real community dashboards in
Grafana 11.6.6, 12.4.0 and 13.0.1 in turn and check that all three yield the same queries,
read whichever way each release allows. This is the only test that covers what a release
does to a dashboard between saving it and serving it back, so run it after touching the
Grafana client, the bulk listing or datasource resolution. It takes about half a minute and
needs the corpus cache.

**Throughput tests** need both tags and both prerequisites: they upload the cached corpus
into a dockerized Grafana and time the extraction, so `make test-corpus` has to have filled
the cache first. They also compare the two ways of reading dashboards over a thousand real
ones, and record peak heap, which synthetic dashboards say nothing about because real ones
are two orders of magnitude larger.

## The bulk listing

`--bulk` reads dashboards in pages from the Kubernetes-style API of Grafana 12, and is on by
default where the instance serves it. `go doc ./internal/cli` explains the mechanism; two
things about it are easy to break and hard to notice:

- It asks for `v0alpha1` on purpose. That version returns the document as stored; the later
  ones migrate it and drop `__inputs`, after which exported dashboards lose their datasource
  types and their Loki queries come out looking like PromQL.
- Grafana drops a whole batch of dashboards from a page when the authorization call for that
  batch fails rather than answers, and still replies 200. On SQLite under write load this is
  reliable enough to reproduce: `pkg/storage/unified/resource/server.go` filters the batch,
  and the container log shows `could not get basic roles: database is locked (SQLITE_BUSY)`
  against a `check_count` equal to the page size. `fetchMissing` in `internal/cli/pipeline.go`
  is what keeps that from reaching the output: it enumerates `/api/search`, which does not
  have the defect, and fetches whatever the pages skipped. Do not remove it, and do not
  assume a listing that returned a token and a 200 returned the dashboards.

## Fixtures

The unit and integration tiers share the dashboards in
`internal/testsupport/testdata/dashboards`. Each one has an `.expected` file listing the
exact output lines it must produce. After verifying a change in output is intended,
regenerate them:

```bash
go test ./internal/extract/ -run TestExtractFixtures -update
```
