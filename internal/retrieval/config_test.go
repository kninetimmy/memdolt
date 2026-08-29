package retrieval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigPreservesDefaultsAroundPartialRetrievalTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[retrieval]\nmode = 'hybrid'\n[retrieval.scoring]\nvector_weight = 0.75\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeHybrid || cfg.Scoring.VectorWeight != 0.75 {
		t.Fatalf("partial config not applied: %+v", cfg)
	}
	if cfg.DefaultMaxResults != DefaultMaxResults || cfg.RerankCandidatePool != DefaultRerankPool || cfg.Scoring.StalePenalty != DefaultStalePenalty {
		t.Fatalf("partial config erased defaults: %+v", cfg)
	}
}
