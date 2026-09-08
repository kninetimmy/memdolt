# Synthetic memhub interop fixture

`memhub-v1.json` contains invented memory only. Its header follows the tagged
memhub exporter, whose `read_project_meta` copies `projects.schema_version`
verbatim; it does not turn the migration identifier into a decimal string.
The `exported_by` value and SQL timestamp shape also follow that exporter.

Source contracts inspected for issue #159:

- [v0.2.0 migrations](https://github.com/kninetimmy/memhub/blob/v0.2.0/src/db/migrations.rs)
  end at `0023_session_transcripts` (commit
  `43fdbb1269dab0ad42c5514f77a758b4fbe626a8`).
- [v0.2.2 migrations](https://github.com/kninetimmy/memhub/blob/v0.2.2/src/db/migrations.rs)
  add only `0024_session_note_provenance` (commit
  `9f78083c445ab7b544f351601543683aeccc0cf6`).
- [v0.2.2 export command](https://github.com/kninetimmy/memhub/blob/v0.2.2/src/commands/export.rs)
  constructs the header; the
  [v0.2.0 v1 format](https://github.com/kninetimmy/memhub/blob/v0.2.0/src/export/v1.rs)
  defines the rows, with only v0.2.2's five nullable note fields supplemented.

Before #159 this fixture declared `"24"`, exercising the numeric compatibility
form while masking the tagged exporter's named header. After #159 it declares
`"0024_session_note_provenance"`; every other fixture value stays the same.
The fresh-process CLI test derives the v0.2.0 variant by selecting its header
and omitting the five later note fields. Older omitted serde-defaulted fields
in these synthetic rows intentionally continue to exercise compatibility.
This is contract-based regression data, not an export of a user's database or
evidence that a real migration, command reconciliation or adoption succeeded.
