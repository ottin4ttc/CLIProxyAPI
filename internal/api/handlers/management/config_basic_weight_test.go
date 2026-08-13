package management

import "testing"

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyHealthWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"health-weighted-round-robin", "hwrr", "HealthWeightedRoundRobin"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "health-weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want health-weighted-round-robin, true", input, got, ok)
		}
	}
}
