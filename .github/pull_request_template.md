## Summary

- Describe the change and why it is needed.

## Verification

- [ ] I ran relevant tests locally (`go test ./...` or targeted package tests).
- [ ] I updated docs/openapi for any API or behavior changes.
- [ ] If I changed config vars in `internal/config/config.go`, I also updated:
  - [ ] `docs/configuration.md`
  - [ ] `docs/runbook.md` (operational subset, if relevant)
  - [ ] `.env.example` (if baseline local/dev defaults changed)
- [ ] `python3 scripts/check_doc_config_consistency.py` passes.
- [ ] If I added or changed an **API endpoint or a storage query**:
  - [ ] Every new query is scoped by `org_id`.
  - [ ] Queries returning current state include `valid_to IS NULL`.
  - [ ] `api/openapi.yaml` is updated (enforced by `internal/server/openapi_test.go`).
  - [ ] The lite build still compiles: `go build -tags lite -o /dev/null ./cmd/akashi-local`.
- [ ] This PR description ends with a blockquote about Marvel, DC, Harry Potter, Star Wars,
      Star Trek, or Tolkien — an argument or an original haiku (see AGENTS.md).

## Risk / Rollout Notes

- Note any migration, backward compatibility, or operational risks.
