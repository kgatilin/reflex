module github.com/kgatilin/reflex

go 1.23.0

require (
	github.com/google/uuid v1.6.0
	github.com/kgatilin/archmotif v0.0.0-00010101000000-000000000000
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	gonum.org/v1/gonum v0.15.1 // indirect
)

// archmotif is consumed locally for the analyzer's graph adapter. The
// `pkg/archmotifimport` API exposes archmotif's typed graph; the analyzer
// uses it to construct a graph from a reflex event trace. Local replace
// is a PoC-grade hack — Phase 4+ will resolve to a proper tagged release.
replace github.com/kgatilin/archmotif => /home/dev/dev/sandbox/archmotif
