## Summary

- Describe the change and why it is needed.

## Verification

- [ ] I ran relevant tests locally (`go test ./...` or targeted package tests).
- [ ] I updated docs/openapi for any API or behavior changes.
- [ ] If I changed config vars in `internal/config/config.go`, I also updated:
  - [ ] `docs/configuration.md`
  - [ ] `docs/runbook.md` (operational subset, if relevant)
  - [ ] `.env.example` (if baseline local/dev defaults changed)
  - [ ] `docs/consistency-manifest.json` (only if policy/coverage rules changed)
- [ ] `python3 scripts/check_doc_config_consistency.py` passes.

## Risk / Rollout Notes

- Note any migration, backward compatibility, or operational risks.
