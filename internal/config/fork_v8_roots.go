package config

// forkConfigRoots are fork-only top-level sections. They keep the same spelling
// in the legacy and v8 layouts, so v8 migration and validation must retain them,
// but their presence alone must not mark a legacy document as v8.
var forkConfigRoots = []string{
	"codex-buckets",
	"api-key-limits",
	"model-access",
	"codex-bucket-model-routes",
	"credential-max-inflight",
	"save-health-ring",
}
