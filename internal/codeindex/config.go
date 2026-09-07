package codeindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/kninetimmy/memdolt/internal/denylist"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/retrieval"
)

// Config keeps code fusion independent of memory recall. Only mode and the
// rerank pool size retain the tagged shared [retrieval] settings.
type Config struct {
	FTSWeight       float64 `toml:"fts_weight"`
	VectorWeight    float64 `toml:"vector_weight"`
	TestPathPenalty float64 `toml:"test_path_penalty"`
}

type configuration struct {
	Code      Config `toml:"code_index"`
	Retrieval struct {
		Mode       retrieval.Mode `toml:"mode"`
		RerankPool int            `toml:"rerank_candidate_pool"`
	} `toml:"retrieval"`
	Deny struct {
		Patterns []string `toml:"patterns"`
	} `toml:"deny_list"`
	denied   *denylist.List
	ruleHash string
}

func readConfig(metadata *os.Root) (cfg configuration, err error) {
	cfg.Code = Config{FTSWeight: 0.5, VectorWeight: 0.5, TestPathPenalty: 0.90}
	cfg.Retrieval.Mode = retrieval.ModeFTS
	cfg.Retrieval.RerankPool = 20
	if metadata != nil {
		file, _, openErr := openRegular(metadata, layout.ConfigFileName)
		if openErr != nil && !os.IsNotExist(openErr) {
			return cfg, fmt.Errorf("open code-index configuration: %w", openErr)
		}
		if file != nil {
			defer func() { err = errors.Join(err, file.Close()) }()
			info, statErr := file.Stat()
			if statErr != nil {
				return cfg, statErr
			}
			if err := layout.CheckOwnerSource(metadata, info); err != nil {
				return cfg, err
			}
			md, decodeErr := toml.NewDecoder(file).Decode(&cfg)
			if decodeErr != nil {
				return cfg, fmt.Errorf("parse code-index configuration: %w", decodeErr)
			}
			if md.IsDefined("deny_list") && !md.IsDefined("deny_list", "patterns") {
				return cfg, errors.New("code index: [deny_list] requires patterns (use patterns = [] for no regex rules)")
			}
			for _, key := range md.Undecoded() {
				if len(key) > 0 && key[0] == "code_index" {
					return cfg, errors.New("unknown [code_index] key; use fts_weight, vector_weight or test_path_penalty")
				}
			}
		}
	}
	if _, err := retrieval.ParseMode(string(cfg.Retrieval.Mode)); err != nil {
		return cfg, err
	}
	if cfg.Retrieval.RerankPool < 1 {
		return cfg, errors.New("retrieval.rerank_candidate_pool must be positive")
	}
	for name, value := range map[string]float64{"fts_weight": cfg.Code.FTSWeight, "vector_weight": cfg.Code.VectorWeight, "test_path_penalty": cfg.Code.TestPathPenalty} {
		if math.IsNaN(value) || value < 0 || value > 1 {
			return cfg, fmt.Errorf("code_index.%s must be in [0,1]", name)
		}
	}
	cfg.denied, err = denylist.Compile(cfg.Deny.Patterns)
	if err != nil {
		return cfg, err
	}
	raw, err := json.Marshal(cfg.Deny.Patterns)
	if err != nil {
		return cfg, err
	}
	cfg.ruleHash = hashBytes(raw)
	return cfg, nil
}

// Memhub's default path exclusions remain unconditional here. Its custom
// globs are deliberately not parsed as memdolt's existing regex setting.
func defaultDenied(file string) bool {
	for suffix := file; suffix != ""; {
		base := path.Base(suffix)
		if base == ".env" || strings.HasPrefix(base, ".env.") ||
			strings.HasPrefix(suffix, "secrets/") || strings.HasPrefix(suffix, ".gnupg/") ||
			suffix == ".aws/credentials" || strings.HasPrefix(suffix, ".gcloud/credentials") {
			return true
		}
		for _, ext := range []string{".pem", ".key", ".p12", ".pfx"} {
			if strings.HasSuffix(suffix, ext) {
				return true
			}
		}
		for _, key := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
			if base == key || strings.HasPrefix(base, key+".") {
				return true
			}
		}
		_, tail, ok := strings.Cut(suffix, "/")
		if !ok {
			break
		}
		suffix = tail
	}
	return false
}
