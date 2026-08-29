// Package retrieval implements PRD §8 recall over committed Dolt text and
// the local derived embedding side-store.
package retrieval

import (
	"errors"
	"fmt"
	"io/fs"
	"math"

	"github.com/BurntSushi/toml"
)

const (
	DefaultMaxResults         = 6
	DefaultFactStaleAfterDays = 90
	DefaultRerankPool         = 20
	DefaultFTSWeight          = 0.5
	DefaultVectorWeight       = 0.5
	DefaultStalePenalty       = 0.3
	DefaultSupersededPenalty  = 0.4
	DefaultMinRerankScore     = float32(2.0)
	DefaultDocMinRerankScore  = float32(0.0)
)

// Mode selects explicit Dolt FULLTEXT or vector-only hybrid candidate
// assembly.
type Mode string

const (
	ModeFTS    Mode = "fts"
	ModeHybrid Mode = "hybrid"
)

func (m *Mode) UnmarshalText(raw []byte) error {
	mode, err := ParseMode(string(raw))
	if err != nil {
		return err
	}
	*m = mode
	return nil
}

func ParseMode(raw string) (Mode, error) {
	switch Mode(raw) {
	case ModeFTS:
		return ModeFTS, nil
	case ModeHybrid:
		return ModeHybrid, nil
	default:
		return "", fmt.Errorf("retrieval mode %q must be fts or hybrid", raw)
	}
}

// ScoringConfig is the PRD §8.1 fusion, penalty, age-decay, and floor
// configuration.
type ScoringConfig struct {
	FTSWeight         float64 `toml:"fts_weight"`
	VectorWeight      float64 `toml:"vector_weight"`
	StalePenalty      float64 `toml:"stale_penalty"`
	SupersededPenalty float64 `toml:"superseded_penalty"`
	AgeHalfLifeDays   int64   `toml:"age_half_life_days"`
	MinRerankScore    float32 `toml:"min_rerank_score"`
	DocMinRerankScore float32 `toml:"doc_min_rerank_score"`
}

// Config is the `[retrieval]` table. Missing files and keys use the memhub
// parity defaults recorded in PRD §8.
type Config struct {
	Mode                  Mode          `toml:"mode"`
	DefaultMaxResults     int           `toml:"default_max_results"`
	AcceptedOnlyByDefault bool          `toml:"accepted_only_by_default"`
	IncludeStaleByDefault bool          `toml:"include_stale_by_default"`
	FactStaleAfterDays    int64         `toml:"fact_stale_after_days"`
	UseReranker           bool          `toml:"use_reranker"`
	RerankCandidatePool   int           `toml:"rerank_candidate_pool"`
	IncludeDocsInDefault  bool          `toml:"include_docs_in_default"`
	Scoring               ScoringConfig `toml:"scoring"`
}

func DefaultConfig() Config {
	return Config{
		Mode:                  ModeFTS,
		DefaultMaxResults:     DefaultMaxResults,
		IncludeStaleByDefault: true,
		FactStaleAfterDays:    DefaultFactStaleAfterDays,
		UseReranker:           true,
		RerankCandidatePool:   DefaultRerankPool,
		Scoring: ScoringConfig{
			FTSWeight:         DefaultFTSWeight,
			VectorWeight:      DefaultVectorWeight,
			StalePenalty:      DefaultStalePenalty,
			SupersededPenalty: DefaultSupersededPenalty,
			MinRerankScore:    DefaultMinRerankScore,
			DocMinRerankScore: DefaultDocMinRerankScore,
		},
	}
}

// LoadConfig reads only `[retrieval]`; deny-list enforcement keeps its
// independent fail-closed loader because reads and writes have different
// failure boundaries.
func LoadConfig(path string) (Config, error) {
	file := struct {
		Retrieval Config `toml:"retrieval"`
	}{Retrieval: DefaultConfig()}
	if _, err := toml.DecodeFile(path, &file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return file.Retrieval, nil
		}
		return Config{}, fmt.Errorf("retrieval: read %s: %w", path, err)
	}
	if err := file.Retrieval.Validate(); err != nil {
		return Config{}, fmt.Errorf("retrieval: %s: %w", path, err)
	}
	return file.Retrieval, nil
}

func (c Config) Validate() error {
	if _, err := ParseMode(string(c.Mode)); err != nil {
		return err
	}
	if c.DefaultMaxResults < 1 {
		return errors.New("retrieval.default_max_results must be greater than zero")
	}
	if c.RerankCandidatePool < 1 {
		return errors.New("retrieval.rerank_candidate_pool must be greater than zero")
	}
	if c.FactStaleAfterDays < 1 {
		return errors.New("retrieval.fact_stale_after_days must be at least 1")
	}
	if c.Scoring.AgeHalfLifeDays < 0 {
		return errors.New("retrieval.scoring.age_half_life_days must be nonnegative (0 disables decay)")
	}
	for name, value := range map[string]float64{
		"fts_weight": c.Scoring.FTSWeight, "vector_weight": c.Scoring.VectorWeight,
		"stale_penalty": c.Scoring.StalePenalty, "superseded_penalty": c.Scoring.SupersededPenalty,
	} {
		if value < 0 || value > 1 || math.IsNaN(value) {
			return fmt.Errorf("retrieval.scoring.%s must be in [0,1]", name)
		}
	}
	if !finite32(c.Scoring.MinRerankScore) || !finite32(c.Scoring.DocMinRerankScore) {
		return errors.New("retrieval rerank score floors must be finite")
	}
	return nil
}

func finite32(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
