package diff

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// DiffCodexModelFallbackChanges compares codex.model-fallback chains and
// reports one entry per model whose chain was added, removed, reordered, or
// otherwise changed. Model and tier names are not secrets, so entries name
// them directly rather than summarizing counts (unlike the OAuth alias /
// excluded-models diffs, which redact by design).
func DiffCodexModelFallbackChanges(oldChains, newChains []config.CodexModelFallback) []string {
	oldByModel := indexCodexModelFallback(oldChains)
	newByModel := indexCodexModelFallback(newChains)

	keys := make(map[string]struct{}, len(oldByModel)+len(newByModel))
	for k := range oldByModel {
		keys[k] = struct{}{}
	}
	for k := range newByModel {
		keys[k] = struct{}{}
	}

	changes := make([]string, 0, len(keys))
	for key := range keys {
		oldTo, okOld := oldByModel[key]
		newTo, okNew := newByModel[key]
		switch {
		case okOld && !okNew:
			changes = append(changes, fmt.Sprintf("codex.model-fallback[%s]: %s -> <none>", key, strings.Join(oldTo, ",")))
		case !okOld && okNew:
			changes = append(changes, fmt.Sprintf("codex.model-fallback[%s]: <none> -> %s", key, strings.Join(newTo, ",")))
		case okOld && okNew && !reflect.DeepEqual(oldTo, newTo):
			if codexModelFallbackSameTiersReordered(oldTo, newTo) {
				changes = append(changes, fmt.Sprintf("codex.model-fallback[%s]: reordered (%s -> %s)", key, strings.Join(oldTo, ","), strings.Join(newTo, ",")))
			} else {
				changes = append(changes, fmt.Sprintf("codex.model-fallback[%s]: %s -> %s", key, strings.Join(oldTo, ","), strings.Join(newTo, ",")))
			}
		}
	}
	sort.Strings(changes)
	return changes
}

// indexCodexModelFallback normalizes a fallback chain list into a map keyed
// by the lowercased, trimmed "from" model, matching the lookup semantics of
// Config.CodexFallbackChain. Tier names are trimmed but keep their configured
// case, since that case is significant to the actual fallback request.
func indexCodexModelFallback(entries []config.CodexModelFallback) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for _, entry := range entries {
		key := strings.ToLower(strings.TrimSpace(entry.From))
		if key == "" {
			continue
		}
		tiers := make([]string, 0, len(entry.To))
		for _, tier := range entry.To {
			if trimmed := strings.TrimSpace(tier); trimmed != "" {
				tiers = append(tiers, trimmed)
			}
		}
		out[key] = tiers
	}
	return out
}

// codexModelFallbackSameTiersReordered reports whether oldTo and newTo
// contain the same tiers in a different order.
func codexModelFallbackSameTiersReordered(oldTo, newTo []string) bool {
	if len(oldTo) != len(newTo) {
		return false
	}
	oldSorted := append([]string(nil), oldTo...)
	newSorted := append([]string(nil), newTo...)
	sort.Strings(oldSorted)
	sort.Strings(newSorted)
	return reflect.DeepEqual(oldSorted, newSorted)
}
