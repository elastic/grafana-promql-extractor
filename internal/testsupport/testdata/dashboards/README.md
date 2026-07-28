# Dashboard fixtures

Every `<name>.json` here is paired with a `<name>.expected` file holding the exact
`uid;query` lines the extractor must produce for it under the default settings and the
datasource set defined in `testsupport.Datasources()`.

Both test tiers use these files: the unit tests in `internal/extract` parse them directly,
and the integration tests upload them to a real Grafana and assert on the output file. A
fixture added here is therefore covered by both.

Regenerate the expectations after an intentional change:

```bash
go test ./internal/extract/ -run TestExtractFixtures -update
```

The `fx-*` fixtures are hand written, each covering one datasource reference shape or
extraction rule. The `real-*` fixtures are unmodified community dashboards from
grafana.com, kept as regression tests against real-world schema versions:

| Fixture | Source | Covers |
| --- | --- | --- |
| `real-dcgm-exporter.json` | [NVIDIA DCGM Exporter Dashboard](https://grafana.com/grafana/dashboards/12239), schema version 22 | `${DS_PROMETHEUS}` string references resolved through `__inputs` |
| `real-legacy-rows.json` | [Memcached Pods monitoring](https://grafana.com/grafana/dashboards/3063), schema version 14 | the pre-Grafana-5 `rows[]` layout |

Both had their numeric `id` removed so they can be uploaded to any instance, and
`real-legacy-rows.json` was given a stable `uid` because the original has none.

For coverage beyond a curated set, `make test-corpus` runs the extractor over the top
dashboards on grafana.com; see the README.
