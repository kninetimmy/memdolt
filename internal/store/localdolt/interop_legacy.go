package localdolt

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// These fields are the tagged memhub v0.2.0 src/export/v1.rs contract. The
// only later supplement admitted is v0.2.2's five nullable note fields.
type memhubExport struct {
	Version    int            `json:"memhub_export_version"`
	ExportedAt *string        `json:"exported_at"`
	ExportedBy *string        `json:"exported_by"`
	Schema     *string        `json:"source_schema_version"`
	Project    *memhubProject `json:"project"`
	Facts      []memhubRow    `json:"facts"`
	Decisions  []memhubRow    `json:"decisions"`
	Tasks      []memhubRow    `json:"tasks"`
	Commands   []memhubRow    `json:"commands"`
	Pending    []memhubRow    `json:"pending_writes"`
	Log        []memhubRow    `json:"writes_log"`
	Notes      []memhubRow    `json:"session_notes"`
	State      []memhubRow    `json:"project_state"`
	Arch       []memhubRow    `json:"project_arch"`
}

type memhubProject struct {
	Root    *string `json:"root_path_at_export"`
	Created *string `json:"created_at"`
}

type memhubRow = map[string]json.RawMessage

// A question mark allows an absent field; a tilde allows SQL/JSON NULL.
// Those are separate: older exports may omit only fields serde defaulted.
var memhubFields = map[string]string{
	"facts":          "id key value confidence source ~verified_at created_at ?~superseded_by ?~kind",
	"decisions":      "id title rationale status decided_at ~superseded_by ?source ?~summary",
	"tasks":          "id title status ~notes created_at updated_at",
	"commands":       "id kind cmdline ~last_exit_code ~last_run_at success_count fail_count",
	"session_notes":  "id actor actor_raw text created_at ?~session_id ?~agent_id ?~provider_id ?~model_id ?~variant",
	"project_state":  "id body actor actor_raw created_at",
	"project_arch":   "id body actor actor_raw created_at",
	"pending_writes": "id kind payload_json rationale status actor actor_raw created_at provenance_json ?~reviewed_at",
	"writes_log":     "id actor table_name ~row_id action ~reason at",
}

type InteropIdentity struct {
	Table    string `json:"table"`
	SourceID string `json:"source_id"`
	Key      string `json:"key"`
	Value    string `json:"value"`
}

type legacyHistory struct {
	Schema          string          `json:"source_schema_version"`
	ExportedAt      string          `json:"exported_at"`
	ExportedBy      string          `json:"exported_by"`
	Counts          map[string]int  `json:"source_counts"`
	HistoricalQueue map[string]int  `json:"historical_pending_statuses"`
	PendingMetadata []legacyPending `json:"recreated_pending_metadata"`
}

type legacyPending struct {
	LegacyID   string `json:"legacy_id"`
	ProposalID string `json:"proposal_id"`
	ActorRaw   string `json:"actor_raw"`
	Provenance string `json:"provenance_json"`
}

func decodeMemhubExport(data []byte) (InteropBundle, []InteropIdentity, legacyHistory, error) {
	var source memhubExport
	bundle := InteropBundle{Tables: map[string][]InteropRow{}, Proposals: []InteropProposal{}}
	var identities []InteropIdentity
	history := legacyHistory{Counts: map[string]int{}, HistoricalQueue: map[string]int{}, PendingMetadata: []legacyPending{}}
	if err := decodeInteropJSON(data, &source); err != nil {
		return bundle, nil, history, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return bundle, nil, history, err
	}
	for _, name := range []string{"session_notes", "project_state", "project_arch"} {
		if raw, present := fields[name]; present && strings.TrimSpace(string(raw)) == "null" {
			return bundle, nil, history, fmt.Errorf("memhub %s must be an array when present; only absence defaults to empty", name)
		}
	}
	if source.Version != 1 || source.ExportedAt == nil || source.ExportedBy == nil || source.Schema == nil || source.Project == nil || source.Project.Root == nil || source.Project.Created == nil {
		return bundle, nil, history, errors.New("require the complete memhub export v1 header (tagged v0.2.0 plus optional v0.2.2 note metadata)")
	}
	if _, err := memhubTimestamp(*source.ExportedAt); err != nil {
		return bundle, nil, history, errors.New("invalid memhub exported_at timestamp")
	}
	if !supportedMemhubSchema(*source.Schema) {
		return bundle, nil, history, errors.New("unsupported memhub source_schema_version; require numeric 1-24 or an exact tagged v0.2.0/v0.2.2 migration identifier")
	}
	if _, err := memhubTimestamp(*source.Project.Created); err != nil {
		return bundle, nil, history, errors.New("invalid memhub project.created_at timestamp")
	}
	history.Schema, history.ExportedAt, history.ExportedBy = *source.Schema, *source.ExportedAt, *source.ExportedBy
	groups := []struct {
		table    string
		rows     []memhubRow
		optional bool
	}{
		{"facts", source.Facts, false}, {"decisions", source.Decisions, false},
		{"tasks", source.Tasks, false}, {"commands", source.Commands, false},
		{"session_notes", source.Notes, true}, {"project_state", source.State, true},
		{"project_arch", source.Arch, true}, {"pending_writes", source.Pending, false},
		{"writes_log", source.Log, false},
	}
	// Complete every table's mapping before resolving any supersession or
	// proposal. Commands deliberately map to their real kind primary key.
	mapping := map[string]map[string]string{}
	for _, group := range groups {
		if group.rows == nil && !group.optional {
			return bundle, nil, history, fmt.Errorf("memhub v1 requires the %s array", group.table)
		}
		mapping[group.table] = map[string]string{}
		history.Counts[group.table] = len(group.rows)
		for _, row := range group.rows {
			if err := validateMemhubRow(group.table, row); err != nil {
				return bundle, nil, history, err
			}
			id := string(row["id"])
			if _, exists := mapping[group.table][id]; exists {
				return bundle, nil, history, fmt.Errorf("duplicate memhub %s numeric ID", group.table)
			}
			mapped := newID()
			if group.table == "commands" {
				mapped = memhubString(row, "kind")
			}
			if group.table == "writes_log" || group.table == "pending_writes" && memhubString(row, "status") != "pending" {
				mapped = "" // historical audit references are not durable row identities
			}
			mapping[group.table][id] = mapped
		}
	}
	for _, table := range interopTables {
		bundle.Tables[table] = []InteropRow{}
	}
	commandKinds := map[string]bool{}
	for _, group := range groups[:7] {
		for _, legacy := range group.rows {
			row := emptyInteropRow(group.table)
			id := string(legacy["id"])
			key := interopColumns(group.table)[0]
			row[key] = interopString(mapping[group.table][id])
			for _, column := range interopColumns(group.table)[1:] {
				raw, present := legacy[column]
				if !present || string(raw) == "null" {
					continue
				}
				if column == "superseded_by" {
					mapped, ok := mapping[group.table][string(raw)]
					if !ok {
						return bundle, nil, history, fmt.Errorf("dangling memhub %s superseded_by", group.table)
					}
					row[column] = interopString(mapped)
				} else if group.table == "commands" && slices.Contains([]string{"last_exit_code", "success_count", "fail_count"}, column) {
					row[column] = interopString(string(raw))
				} else {
					value := memhubString(legacy, column)
					if strings.HasSuffix(column, "_at") {
						var err error
						value, err = memhubTimestamp(value)
						if err != nil {
							return bundle, nil, history, fmt.Errorf("memhub %s.%s: %w", group.table, column, err)
						}
					}
					row[column] = interopString(value)
				}
			}
			if group.table == "decisions" && legacy["source"] == nil {
				row["source"] = interopString("user") // v1.rs's explicit serde default
			}
			if group.table == "commands" {
				kind := interopValue(row, "kind")
				if commandKinds[kind] {
					return bundle, nil, history, errors.New("memhub has multiple commands of one kind; memdolt has one kind primary key; reconcile commands explicitly in a retained source copy and export again")
				}
				commandKinds[kind] = true
			}
			bundle.Tables[group.table] = append(bundle.Tables[group.table], row)
			identities = append(identities, InteropIdentity{group.table, id, key, mapping[group.table][id]})
		}
	}
	for _, row := range source.Pending {
		status := memhubString(row, "status")
		if !slices.Contains([]string{"pending", "accepted", "rejected", "expired"}, status) {
			return bundle, nil, history, errors.New("unsupported memhub pending_writes.status")
		}
		var payload memhubRow
		if err := decodeInteropJSON([]byte(memhubString(row, "payload_json")), &payload); err != nil || payload == nil {
			return bundle, nil, history, errors.New("invalid memhub pending payload_json object")
		}
		var provenance any
		if err := decodeInteropJSON([]byte(memhubString(row, "provenance_json")), &provenance); err != nil {
			return bundle, nil, history, errors.New("invalid memhub pending provenance_json")
		}
		kind := memhubString(row, "kind")
		if !slices.Contains([]string{"fact", "decision", "supersede"}, kind) {
			return bundle, nil, history, errors.New("unsupported memhub pending write kind")
		}
		if err := validateMemhubPayload(kind, payload); err != nil {
			return bundle, nil, history, err
		}
		if status != "pending" {
			history.HistoricalQueue[status]++
			continue // annotated history only; an audit row is not an action to replay
		}
		if row["reviewed_at"] != nil && string(row["reviewed_at"]) != "null" {
			return bundle, nil, history, errors.New("ambiguous memhub pending write already has reviewed_at")
		}
		if kind == "supersede" {
			return bundle, nil, history, errors.New("legacy supersede links old/new existing fact or decision identifiers; memdolt proposals require a fresh fact replacement; review this pending write in the retained source before exporting again")
		}
		if target := memhubString(payload, "target"); target != "" && target != "repo" {
			return bundle, nil, history, errors.New("global-target memhub pending import is unsupported; resolve it in source human review")
		}
		id := string(row["id"])
		proposalID, rowID := mapping["pending_writes"][id], newID()
		stamp, err := memhubTimestamp(memhubString(row, "created_at"))
		if err != nil {
			return bundle, nil, history, err
		}
		metadata := InteropRow{
			"id": interopString(proposalID), "kind": interopString(kind),
			"rationale": interopString(memhubString(row, "rationale")), "actor": interopString(memhubString(row, "actor")),
			"created_at": interopString(stamp), "target": interopString("repo"),
		}
		table := "facts"
		if kind == "decision" {
			table = "decisions"
		}
		proposed := emptyInteropRow(table)
		proposed["id"], proposed["source"] = interopString(rowID), metadata["actor"]
		if kind == "fact" {
			proposed["key"], proposed["value"] = interopString(memhubString(payload, "key")), interopString(memhubString(payload, "value"))
			proposed["created_at"] = interopString(stamp)
			if raw := payload["kind"]; raw != nil && string(raw) != "null" {
				proposed["kind"] = interopString(memhubString(payload, "kind"))
			}
		} else {
			proposed["title"], proposed["rationale"] = interopString(memhubString(payload, "title")), metadata["rationale"]
			proposed["status"], proposed["decided_at"] = interopString("active"), interopString(stamp)
		}
		bundle.Proposals = append(bundle.Proposals, InteropProposal{ID: proposalID, Metadata: metadata, Changes: []InteropChange{{Table: table, To: proposed}}})
		identities = append(identities, InteropIdentity{"pending_writes", id, "id", proposalID}, InteropIdentity{table, "pending_writes:" + id, "id", rowID})
		history.PendingMetadata = append(history.PendingMetadata, legacyPending{id, proposalID, memhubString(row, "actor_raw"), memhubString(row, "provenance_json")})
	}
	return bundle, identities, history, validateInteropMemory(bundle)
}

func supportedMemhubSchema(schema string) bool {
	// Keep the numeric compatibility form. Tagged exporters instead copy the
	// exact migration ID from projects.schema_version; never trust a prefix.
	if version, err := strconv.Atoi(schema); err == nil && version >= 1 && version <= 24 {
		return true
	}
	// Tagged src/db/migrations.rs lists; provenance: testdata/README.md.
	switch schema {
	case "0001_initial",
		"0002_git_search",
		"0003_pending_writes",
		"0004_pending_write_provenance",
		"0005_pending_write_reviewed_at",
		"0006_session_notes",
		"0007_project_narrative",
		"0008_decisions_source",
		"0009_retrieval_indexes",
		"0010_embeddings_delete_triggers",
		"0011_decision_summary",
		"0012_metrics_tables",
		"0013_session_turn_metrics",
		"0014_documents",
		"0015_known_projects",
		"0016_global_accept_markers",
		"0017_session_baseline",
		"0018_supersede",
		"0019_metrics_maintenance_debounce",
		"0020_recall_metrics_surface",
		"0021_fact_kind",
		"0022_source_type_note",
		"0023_session_transcripts",
		"0024_session_note_provenance":
		return true
	default:
		return false
	}
}

func validateMemhubRow(table string, row memhubRow) error {
	allowed := map[string]bool{}
	for _, field := range strings.Fields(memhubFields[table]) {
		name := strings.TrimLeft(field, "?~")
		allowed[name] = true
		raw, present := row[name]
		if !present {
			if strings.Contains(field, "?") {
				continue
			}
			return fmt.Errorf("memhub %s missing %s", table, name)
		}
		if string(raw) == "null" {
			if strings.Contains(field, "~") {
				continue
			}
			return fmt.Errorf("memhub %s.%s cannot be null", table, name)
		}
		if name == "confidence" {
			var number float64
			if err := json.Unmarshal(raw, &number); err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
				return errors.New("memhub confidence must be a finite JSON number; it is intentionally omitted by the Dolt schema")
			}
		} else if slices.Contains([]string{"id", "superseded_by", "row_id", "last_exit_code", "success_count", "fail_count"}, name) {
			var number int64
			if err := json.Unmarshal(raw, &number); err != nil || (name == "id" || name == "superseded_by" || name == "row_id") && number < 1 {
				return fmt.Errorf("invalid memhub %s.%s integer", table, name)
			}
			// Normalize insignificant JSON whitespace for the identity map.
			row[name] = json.RawMessage(strconv.FormatInt(number, 10))
		} else {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("memhub %s.%s must be a string", table, name)
			}
			if strings.HasSuffix(name, "_at") || name == "at" {
				if _, err := memhubTimestamp(value); err != nil {
					return fmt.Errorf("memhub %s.%s: %w", table, name, err)
				}
			}
		}
	}
	for name := range row {
		if !allowed[name] {
			return fmt.Errorf("unsupported field in memhub %s", table)
		}
	}
	return nil
}

func memhubString(row memhubRow, key string) string {
	var value string
	_ = json.Unmarshal(row[key], &value) // caller validates the complete row first
	return value
}

func memhubTimestamp(value string) (string, error) {
	// time.Parse accepts and truncates fractional digits beyond nanoseconds.
	// Inspect them before parsing so a tiny nonzero fraction cannot disappear.
	if dot := strings.IndexAny(value, ".,"); dot >= 0 {
		for _, digit := range value[dot+1:] {
			if digit < '0' || digit > '9' {
				break
			}
			if digit != '0' {
				return "", errors.New("timestamp cannot be represented losslessly in UTC DATETIME seconds")
			}
		}
	}
	for _, format := range []string{time.DateTime, time.RFC3339Nano} {
		stamp, err := time.Parse(format, value)
		if err == nil && stamp.Nanosecond() == 0 && stamp.UTC().Year() >= 1000 && stamp.UTC().Year() <= 9999 {
			return datetime(stamp), nil
		}
	}
	return "", errors.New("timestamp cannot be represented losslessly in UTC DATETIME seconds")
}

func validateMemhubPayload(kind string, payload memhubRow) error {
	fields := map[string]string{
		"fact": "key value ?~kind ?target", "decision": "title ?target",
		"supersede": "target_kind old new",
	}
	allowed := map[string]bool{}
	for _, field := range strings.Fields(fields[kind]) {
		name := strings.TrimLeft(field, "?~")
		allowed[name] = true
		raw, present := payload[name]
		if !present && strings.Contains(field, "?") || string(raw) == "null" && strings.Contains(field, "~") {
			continue
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil || value == nil || !strings.Contains(field, "?") && strings.TrimSpace(*value) == "" {
			return errors.New("malformed memhub pending payload field")
		}
	}
	for name := range payload {
		if !allowed[name] {
			return errors.New("unsupported memhub pending payload field")
		}
	}
	if kind == "supersede" && (!slices.Contains([]string{"fact", "decision"}, memhubString(payload, "target_kind")) || memhubString(payload, "old") == memhubString(payload, "new")) {
		return errors.New("malformed legacy supersede target or identifiers")
	}
	if _, present := payload["target"]; present {
		if !slices.Contains([]string{"repo", "global"}, memhubString(payload, "target")) {
			return errors.New("unsupported legacy proposal target")
		}
	}
	return nil
}
