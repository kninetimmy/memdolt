package localdolt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/kninetimmy/memdolt/internal/store"
)

// InteropVersion names memdolt's own format, not memhub_export_version.
const InteropVersion = 1

// InteropRow is a complete, fixed-schema SQL row image. SQL NULL is JSON null;
// every non-NULL cell is a string, including lossless decimal INTs and UTC
// DATETIME values. Generated live_key is reconstructed by Dolt, never supplied.
type InteropRow = map[string]*string

type InteropBundle struct {
	Version       int                     `json:"memdolt_export_version"`
	SchemaVersion int                     `json:"source_schema_version"`
	ExportedAt    string                  `json:"exported_at"`
	MainCommit    string                  `json:"main_commit"`
	Tables        map[string][]InteropRow `json:"tables"`
	Proposals     []InteropProposal       `json:"pending_proposals"`
}

type InteropProposal struct {
	ID       string          `json:"id"`
	Head     string          `json:"head"`
	Parent   string          `json:"parent"`
	Metadata InteropRow      `json:"metadata"`
	Changes  []InteropChange `json:"changes"`
}

type InteropChange struct {
	Table string     `json:"table"`
	From  InteropRow `json:"from"`
	To    InteropRow `json:"to"`
}

// The schema/type contract is transferTables; interop selects only durable
// memory, leaving documents, project identity, config and all local data out.
var interopTables = []string{"facts", "decisions", "tasks", "commands", "session_notes", "project_state", "project_arch", "proposals"}

func interopColumns(table string) []string {
	for _, entry := range transferTables {
		if entry.name == table && slices.Contains(interopTables, table) {
			var columns []string
			for _, field := range strings.Fields(entry.columns) {
				name, _, _ := strings.Cut(field, ":")
				if name != "live_key" {
					columns = append(columns, name)
				}
			}
			return columns
		}
	}
	return nil
}

func emptyInteropRow(table string) InteropRow {
	row := InteropRow{}
	for _, column := range interopColumns(table) {
		row[column] = nil
	}
	return row
}

func interopValue(row InteropRow, column string) string {
	if value := row[column]; value != nil {
		return *value
	}
	return ""
}

func interopString(value string) *string { return &value }

func validInteropID(value string) bool {
	id, err := ulid.ParseStrict(value)
	return err == nil && id.String() == value
}

func sameInteropRow(a, b InteropRow) bool {
	return (a == nil) == (b == nil) && maps.EqualFunc(a, b, equalStringPointers)
}

// Reject duplicates before decoding: encoding/json otherwise silently takes
// the last object member. Unknown/case-aliased fields, malformed Unicode and
// trailing JSON also fail before mapping identities or touching the destination.
func decodeInteropJSON(data []byte, dest any) error {
	if err := ValidateJSONUnicode(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkInteropJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("bundle must contain exactly one JSON value")
	}
	if err := checkInteropMembers(data, dest); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return fmt.Errorf("invalid bundle shape: %w", err)
	}
	return nil
}

func checkInteropJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid bundle JSON: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid JSON object member")
			}
			seen[name] = true
		}
		if err := checkInteropJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// encoding/json matches struct tags case-insensitively. Check exact member
// names at the format's struct boundaries so aliases cannot override fields.
// Row maps have their own exact-column validation; opaque provenance remains
// a case-sensitive JSON value and is not interpreted as a format struct.
func checkInteropMembers(data []byte, dest any) error {
	switch dest.(type) {
	case *memhubExport:
		root, err := interopObject(data, "memhub_export_version exported_at exported_by source_schema_version project facts decisions tasks commands pending_writes writes_log session_notes project_state project_arch")
		if err != nil {
			return err
		}
		_, err = interopObject(root["project"], "root_path_at_export created_at")
		return err
	case *InteropBundle:
		root, err := interopObject(data, "memdolt_export_version source_schema_version exported_at main_commit tables pending_proposals")
		if err != nil {
			return err
		}
		var proposals []json.RawMessage
		if err := json.Unmarshal(root["pending_proposals"], &proposals); err != nil {
			return err
		}
		for _, raw := range proposals {
			proposal, err := interopObject(raw, "id head parent metadata changes")
			if err != nil {
				return err
			}
			var changes []json.RawMessage
			if err := json.Unmarshal(proposal["changes"], &changes); err != nil {
				return err
			}
			for _, change := range changes {
				if _, err := interopObject(change, "table from to"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func interopObject(data []byte, names string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("expected a complete interop format object")
	}
	allowed := strings.Fields(names)
	for name := range object {
		if !slices.Contains(allowed, name) {
			return nil, errors.New("unknown or case-aliased interop format member")
		}
	}
	return object, nil
}

func validateInteropHeader(b InteropBundle) error {
	if b.Version != InteropVersion || b.SchemaVersion != store.LatestSchemaVersion() {
		return fmt.Errorf("unsupported memdolt interop/schema version; require memdolt_export_version %d and source_schema_version %d; legacy memhub v1 needs --from-memhub", InteropVersion, store.LatestSchemaVersion())
	}
	if !transferHash.MatchString(b.MainCommit) {
		return errors.New("main_commit must be an immutable Dolt hash")
	}
	if _, err := time.Parse(time.RFC3339Nano, b.ExportedAt); err != nil {
		return errors.New("exported_at must be an RFC3339 timestamp")
	}
	for _, proposal := range b.Proposals {
		if !transferHash.MatchString(proposal.Head) || !transferHash.MatchString(proposal.Parent) || proposal.Head == proposal.Parent {
			return errors.New("pending proposal requires distinct immutable head and parent hashes")
		}
	}
	return nil
}

func validateInteropMemory(b InteropBundle) error {
	if len(b.Tables) != len(interopTables) || b.Proposals == nil {
		return errors.New("bundle requires every supported memory table and a pending_proposals array")
	}
	indexed := map[string]map[string]InteropRow{}
	for _, table := range interopTables {
		rows, ok := b.Tables[table]
		if !ok || rows == nil {
			return fmt.Errorf("bundle requires the %s array", table)
		}
		indexed[table] = map[string]InteropRow{}
		key := interopColumns(table)[0]
		for _, row := range rows {
			if err := validateInteropRow(table, row); err != nil {
				return err
			}
			id := interopValue(row, key)
			if _, exists := indexed[table][id]; exists {
				return fmt.Errorf("duplicate identity in %s", table)
			}
			indexed[table][id] = row
		}
	}
	if err := validateInteropReferences(indexed); err != nil {
		return err
	}
	seen := map[string]bool{}
	inserted := map[string]bool{}
	for i, proposal := range b.Proposals {
		if seen[proposal.ID] || indexed["proposals"][proposal.ID] != nil {
			return fmt.Errorf("duplicate/committed pending proposal identity at position %d", i+1)
		}
		seen[proposal.ID] = true
		if err := validateInteropProposal(proposal, indexed); err != nil {
			return fmt.Errorf("pending proposal %d: %w", i+1, err)
		}
		for _, change := range proposal.Changes {
			if change.From == nil {
				id := change.Table + "/" + interopValue(change.To, "id")
				if inserted[id] {
					return errors.New("duplicate newly inserted identity across pending proposals")
				}
				inserted[id] = true
			}
		}
	}
	return nil
}

func validateInteropRow(table string, row InteropRow) error {
	columns := interopColumns(table)
	if len(columns) == 0 || len(row) != len(columns) {
		return fmt.Errorf("unsupported or incomplete %s row image", table)
	}
	var spec string
	for _, entry := range transferTables {
		if entry.name == table {
			spec = entry.columns
		}
	}
	for _, field := range strings.Fields(spec) {
		name, kind, _ := strings.Cut(field, ":")
		if name == "live_key" {
			continue
		}
		value, present := row[name]
		if !present || value == nil && strings.HasSuffix(kind, "!") {
			return fmt.Errorf("missing required %s.%s", table, name)
		}
		if value == nil {
			continue
		}
		if !utf8.ValidString(*value) {
			return fmt.Errorf("%s.%s must be valid UTF-8; refuse lossy JSON conversion", table, name)
		}
		kind = strings.TrimSuffix(kind, "!")
		switch {
		case kind == "char(26)":
			if !validInteropID(*value) {
				return fmt.Errorf("%s.%s must be a canonical ULID", table, name)
			}
		case strings.HasPrefix(kind, "varchar("):
			limit, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(kind, "varchar("), ")"))
			if err != nil || utf8.RuneCountInString(*value) > limit {
				return fmt.Errorf("%s.%s exceeds its durable column width", table, name)
			}
		case strings.HasPrefix(kind, "enum("):
			allowed := strings.Split(strings.TrimSuffix(strings.TrimPrefix(kind, "enum("), ")"), ",")
			if !slices.Contains(allowed, "'"+*value+"'") {
				return fmt.Errorf("unsupported %s.%s enum value", table, name)
			}
		case kind == "datetime":
			stamp, err := time.Parse(time.DateTime, *value)
			if err != nil || stamp.Year() < 1000 || stamp.Format(time.DateTime) != *value {
				return fmt.Errorf("%s.%s must be an exact UTC YYYY-MM-DD HH:MM:SS timestamp (years 1000-9999)", table, name)
			}
		case kind == "int":
			n, err := strconv.ParseInt(*value, 10, 32)
			if err != nil || strconv.FormatInt(n, 10) != *value || (name == "success_count" || name == "fail_count") && n < 0 {
				return fmt.Errorf("%s.%s must be a lossless decimal INT; counters cannot be negative", table, name)
			}
		case kind == "text":
			if len(*value) > 65535 {
				return fmt.Errorf("%s.%s exceeds the TEXT byte limit", table, name)
			}
		default:
			return fmt.Errorf("unsupported interop column type in %s.%s", table, name)
		}
	}
	return nil
}

func validateInteropReferences(indexed map[string]map[string]InteropRow) error {
	for _, table := range []string{"facts", "decisions"} {
		rows := indexed[table]
		live := map[string]bool{}
		for id, row := range rows {
			if table == "facts" && row["superseded_by"] == nil && row["key"] != nil {
				key := interopValue(row, "key")
				if live[key] {
					return errors.New("ambiguous facts: more than one live row has the same key")
				}
				live[key] = true
			}
			seen := map[string]bool{id: true}
			// ponytail: walk each memory-sized chain; share visitation state if
			// unusually long chains make this quadratic validation measurable.
			for next := row["superseded_by"]; next != nil; next = row["superseded_by"] {
				if seen[*next] {
					return fmt.Errorf("cyclic %s supersession", table)
				}
				seen[*next] = true
				row = rows[*next]
				if row == nil {
					return fmt.Errorf("dangling %s superseded_by reference", table)
				}
			}
		}
	}
	return nil
}

func validateInteropProposal(p InteropProposal, main map[string]map[string]InteropRow) error {
	if interopValue(p.Metadata, "target") != string(TargetRepo) {
		return errors.New("global-target import is unsupported; resolve it in the source's human review before exporting again")
	}
	return validateProposalPayload(p, main)
}

// validateProposalPayload is the fixed-schema shape shared by interop and
// terminal global review. Each caller separately enforces its allowed target.
func validateProposalPayload(p InteropProposal, main map[string]map[string]InteropRow) error {
	if !validInteropID(p.ID) || interopValue(p.Metadata, "id") != p.ID {
		return errors.New("proposal ID and metadata must name the same ULID")
	}
	if err := validateInteropRow("proposals", p.Metadata); err != nil {
		return err
	}
	if strings.TrimSpace(interopValue(p.Metadata, "rationale")) == "" || p.Metadata["created_at"] == nil {
		return errors.New("proposal requires its original rationale and creation time")
	}
	if err := (store.Actor{Name: interopValue(p.Metadata, "actor"), Email: "import-source@memdolt.invalid"}).Validate(); err != nil {
		return errors.New("proposal actor cannot be represented by ordinary staging")
	}
	kind := ProposalKind(interopValue(p.Metadata, "kind"))
	if len(p.Changes) == 0 || len(p.Changes) > 2 {
		return errors.New("unsupported proposal payload: require an ordinary fact, decision, overwrite or fact supersede")
	}
	view := maps.Clone(main)
	view["facts"], view["decisions"] = maps.Clone(main["facts"]), maps.Clone(main["decisions"])
	added, modified := 0, 0
	var fresh, old InteropRow
	seen := map[string]bool{}
	for _, change := range p.Changes {
		table := change.Table
		if table != "facts" && table != "decisions" || change.To == nil {
			return errors.New("unsupported proposal table or deletion")
		}
		if (kind == KindDecision) != (table == "decisions") {
			return errors.New("proposal kind does not match its payload table")
		}
		if err := validateInteropRow(table, change.To); err != nil {
			return err
		}
		if table == "facts" {
			if err := (Fact{Key: interopValue(change.To, "key"), Value: interopValue(change.To, "value")}).validate(); err != nil {
				return errors.New("proposal payload must contain an ordinary nonempty single-line fact key and nonempty value")
			}
		} else if err := (Decision{Title: interopValue(change.To, "title"), Rationale: interopValue(change.To, "rationale")}).validate(); err != nil {
			return errors.New("proposal payload must contain an ordinary nonempty single-line decision title and rationale")
		}
		id := interopValue(change.To, "id")
		if seen[id] {
			return errors.New("duplicate proposal row identity")
		}
		seen[id] = true
		if change.From == nil {
			if main[table][id] != nil {
				return errors.New("proposal insert collides with committed identity")
			}
			added++
			fresh = change.To
			if change.To["superseded_by"] != nil || interopValue(change.To, "source") != interopValue(p.Metadata, "actor") {
				return errors.New("new proposal row must be live and sourced to the original proposal actor")
			}
		} else {
			if err := validateInteropRow(table, change.From); err != nil {
				return err
			}
			if interopValue(change.From, "id") != id || !sameInteropRow(change.From, main[table][id]) {
				return errors.New("proposal before-image differs from committed main; reconcile the source proposal before exporting")
			}
			if sameInteropRow(change.From, change.To) {
				return errors.New("proposal modification must actually change the row")
			}
			if table != "facts" || change.From["superseded_by"] != nil {
				return errors.New("only an ordinary live-fact overwrite or supersede is supported")
			}
			modified++
			old = change.To
			for column, value := range change.From {
				allowed := kind == KindSupersede && column == "superseded_by" || kind == KindFact && slices.Contains([]string{"value", "source", "kind", "evidence", "verified_at"}, column)
				if !allowed && !equalStringPointers(value, change.To[column]) {
					return errors.New("unsupported field change in proposal")
				}
			}
			if kind == KindFact && (change.To["verified_at"] != nil || interopValue(change.To, "source") != interopValue(p.Metadata, "actor")) {
				return errors.New("overwrite must clear verification and retain proposal actor provenance")
			}
		}
		view[table][id] = change.To
	}
	switch kind {
	case KindFact:
		if added+modified != 1 {
			return errors.New("fact proposal must change exactly one row")
		}
	case KindDecision:
		if added != 1 || modified != 0 || interopValue(fresh, "status") != "active" {
			return errors.New("decision proposal must insert one active decision")
		}
	case KindSupersede:
		if added != 1 || modified != 1 || interopValue(old, "superseded_by") != interopValue(fresh, "id") {
			return errors.New("supersede proposal must link one live fact to one fresh replacement")
		}
	default:
		return errors.New("unsupported proposal kind")
	}
	return validateInteropReferences(view)
}
