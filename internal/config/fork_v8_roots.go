package config

// forkConfigRoots are fork-only top-level sections. They keep the same spelling
// in the legacy and v8 layouts, so v8 migration and validation must retain them,
// but their presence alone must not mark a legacy document as v8.
var forkConfigRoots = []string{
	"buckets",
	"api-key-limits",
	"model-access",
	"bucket-model-routes",
	"credential-max-inflight",
	"save-health-ring",
	"conversation-store",
}

// forkExternalConfigRoots are fork roots read straight from the config file
// instead of being decoded into Config, so strict v8 validation skips them.
var forkExternalConfigRoots = []string{"conversation-store"}
